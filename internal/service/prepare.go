package service

import (
	"fmt"
	"mime"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"maild/internal/config"
	"maild/internal/message"
	"maild/internal/smtpclient"
	"maild/internal/spool"
)

const (
	modeSync  = "sync"
	modeAsync = "async"
)

var contentIDPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// MailInput 為 SendMail 與 QueueMail 共用的郵件內容（對應 proto 的 Mail）。
type MailInput struct {
	Sender      string // 寄件者別名，留空使用呼叫端的預設寄件者
	SenderName  string // 顯示名稱，留空使用設定檔的 name
	To          []message.Address
	Cc          []message.Address
	Bcc         []message.Address
	ReplyTo     []message.Address
	Subject     string
	TextBody    string
	HTMLBody    string
	Attachments []message.Attachment // ContentType 可留空，依檔名推斷
	Labels      map[string]string
}

// prepared 為通過驗證、可以寄出的郵件。
type prepared struct {
	mode     string
	client   config.ClientConfig
	fields   mailFields
	sender   config.SenderConfig
	msg      *message.Message
	envelope smtpclient.Envelope
}

// prepare 依呼叫端設定解析寄件者、驗證郵件內容並組成 Message；不檢查寄送量與佇列容量。
func (s *Service) prepare(clientName string, in *MailInput, mode string, now time.Time) (*prepared, error) {
	snap := s.snapshot.Load()
	client, ok := snap.clients[clientName]
	if !ok {
		return nil, ErrClientNotAllowed
	}
	alias := in.Sender
	if alias == "" {
		alias = client.DefaultSender
	}
	if !slices.Contains(client.Senders, alias) {
		return nil, fmt.Errorf("%w: %q", ErrSenderNotAllowed, alias)
	}
	sender := snap.senders[alias]

	if err := validateMail(s.cfg.Mail, in); err != nil {
		return nil, err
	}

	mailID := s.newMailID(now)
	fromName := in.SenderName
	if fromName == "" {
		fromName = sender.Name
	}
	msg := &message.Message{
		From:        message.Address{Address: sender.Address, Name: fromName},
		To:          in.To,
		Cc:          in.Cc,
		ReplyTo:     in.ReplyTo,
		Subject:     in.Subject,
		TextBody:    in.TextBody,
		HTMLBody:    in.HTMLBody,
		Attachments: in.Attachments,
		MessageID:   mailID + "@" + message.Domain(sender.Address),
	}
	fields := mailFields{
		mailID:      mailID,
		client:      clientName,
		mode:        mode,
		sender:      alias,
		from:        sender.Address,
		to:          addresses(in.To),
		cc:          addresses(in.Cc),
		bcc:         addresses(in.Bcc),
		subject:     in.Subject,
		labels:      in.Labels,
		messageID:   msg.MessageID,
		attachments: len(in.Attachments),
	}
	return &prepared{
		mode:     mode,
		client:   client,
		fields:   fields,
		sender:   sender,
		msg:      msg,
		envelope: smtpclient.Envelope{From: sender.Address, Recipients: uniqueRecipients(fields.to, fields.cc, fields.bcc)},
	}, nil
}

// meta 產生佇列檔第一行的寄送資訊；主旨只供 log 使用，超過上限的部分截掉。
func (p *prepared) meta(now time.Time) *spool.Meta {
	f := &p.fields
	return &spool.Meta{
		MailID:      f.mailID,
		Client:      f.client,
		Sender:      f.sender,
		From:        f.from,
		To:          f.to,
		Cc:          f.cc,
		Bcc:         f.bcc,
		Subject:     truncate(f.subject, config.MetaSubjectLimit),
		Labels:      f.labels,
		MessageID:   f.messageID,
		Attachments: f.attachments,
		CreatedAt:   now,
	}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// validateMail 檢查郵件內容；附件的 ContentType 留空時會在這裡補上推斷結果。
func validateMail(limits config.MailConfig, in *MailInput) error {
	if n := len(in.To) + len(in.Cc) + len(in.Bcc); n == 0 {
		return invalid("at least one recipient (to, cc or bcc) is required")
	} else if n > limits.MaxRecipients {
		return invalid("too many recipients: %d (max %d)", n, limits.MaxRecipients)
	}
	for _, list := range []struct {
		field string
		addrs []message.Address
	}{{"to", in.To}, {"cc", in.Cc}, {"bcc", in.Bcc}, {"reply_to", in.ReplyTo}} {
		for i, a := range list.addrs {
			if err := message.CheckAddress(a.Address); err != nil {
				return invalid("%s[%d] %q: %v", list.field, i, a.Address, err)
			}
			if message.HasControl(a.Name) {
				return invalid("%s[%d].name must not contain control characters", list.field, i)
			}
		}
	}

	if message.HasControl(in.SenderName) {
		return invalid("sender_name must not contain control characters")
	}
	if message.HasControl(in.Subject) {
		return invalid("subject must not contain control characters")
	}
	if in.TextBody == "" && in.HTMLBody == "" {
		return invalid("text_body or html_body is required")
	}

	if err := validateAttachments(limits, in.Attachments); err != nil {
		return err
	}
	return validateLabels(limits, in.Labels)
}

func validateAttachments(limits config.MailConfig, attachments []message.Attachment) error {
	var total int64
	contentIDs := map[string]bool{}
	for i := range attachments {
		a := &attachments[i]
		total += int64(len(a.Content))
		if a.Filename == "" {
			return invalid("attachments[%d].filename is required", i)
		}
		if message.HasControl(a.Filename) {
			return invalid("attachments[%d].filename must not contain control characters", i)
		}
		if a.ContentType == "" {
			a.ContentType = mime.TypeByExtension(filepath.Ext(a.Filename))
			if a.ContentType == "" {
				a.ContentType = "application/octet-stream"
			}
		} else {
			mediaType, _, err := mime.ParseMediaType(a.ContentType)
			if err != nil || message.HasControl(a.ContentType) {
				return invalid("attachments[%d].content_type %q is invalid", i, a.ContentType)
			}
			if strings.HasPrefix(mediaType, "multipart/") {
				return invalid("attachments[%d].content_type must not be multipart", i)
			}
		}
		if a.ContentID != "" {
			if !contentIDPattern.MatchString(a.ContentID) {
				return invalid("attachments[%d].content_id may only contain letters, digits and . _ @ -", i)
			}
			if contentIDs[a.ContentID] {
				return invalid("attachments[%d].content_id %q is duplicated", i, a.ContentID)
			}
			contentIDs[a.ContentID] = true
		}
	}
	if total > limits.MaxAttachmentsBytes() {
		return invalid("attachments total %d bytes exceeds the %d MiB limit", total, limits.MaxAttachmentsSizeMiB)
	}
	return nil
}

func validateLabels(limits config.MailConfig, labels map[string]string) error {
	if len(labels) > limits.MaxLabels {
		return invalid("too many labels: %d (max %d)", len(labels), limits.MaxLabels)
	}
	for key, value := range labels {
		if !config.LabelKeyPattern.MatchString(key) {
			return invalid("label key %q may only contain letters, digits and _ . -", key)
		}
		if len(key) > limits.MaxLabelKeyLength {
			return invalid("label key %q is longer than %d characters", key, limits.MaxLabelKeyLength)
		}
		if utf8.RuneCountInString(value) > limits.MaxLabelValueLength {
			return invalid("label %q value is longer than %d characters", key, limits.MaxLabelValueLength)
		}
		if message.HasControl(value) {
			return invalid("label %q value must not contain control characters", key)
		}
	}
	return nil
}

func addresses(list []message.Address) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Address
	}
	return out
}

// uniqueRecipients 合併 to、cc、bcc 作為 SMTP 信封收件者；同一地址只送一次 RCPT TO。
func uniqueRecipients(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, addr := range list {
			key := strings.ToLower(addr)
			if !seen[key] {
				seen[key] = true
				out = append(out, addr)
			}
		}
	}
	return out
}
