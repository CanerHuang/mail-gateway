package service

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// mailFields 為每筆郵件 log 共用的欄位；同步寄送從 request 產生，背景寄送從佇列檔第一行產生。
type mailFields struct {
	mailID      string
	client      string
	mode        string
	sender      string
	from        string
	to          []string
	cc          []string
	bcc         []string
	subject     string
	labels      map[string]string
	messageID   string
	attachments int
	size        int64
}

// logMail 為 log 事件加上郵件欄位；文字欄位超過 log.max_field_length 時截斷。
func (s *Service) logMail(e *zerolog.Event, f *mailFields) *zerolog.Event {
	limit := s.cfg.Log.FieldLimit()
	e = e.Str("mail_id", f.mailID).
		Str("client", f.client).
		Str("mode", f.mode).
		Str("sender", f.sender).
		Str("from", f.from)
	for _, list := range []struct {
		key   string
		addrs []string
	}{{"to", f.to}, {"cc", f.cc}, {"bcc", f.bcc}} {
		if len(list.addrs) > 0 {
			e = e.Str(list.key, truncate(strings.Join(list.addrs, ", "), limit))
		}
	}
	e = e.Int("rcpt_count", len(f.to)+len(f.cc)+len(f.bcc)).
		Str("subject", truncate(f.subject, limit))
	if len(f.labels) > 0 {
		e = e.Interface("labels", truncateLabels(f.labels, limit))
	}
	e = e.Str("message_id", f.messageID).Int("attachments", f.attachments)
	if f.size > 0 {
		e = e.Int64("size", f.size)
	}
	return e
}

// logRejected 記錄被拒的請求（驗證失敗、超過寄送量、佇列已滿等），此時還沒有 mail_id。
func (s *Service) logRejected(client, mode string, in *MailInput, err error) {
	limit := s.cfg.Log.FieldLimit()
	e := log.Warn().Str("client", client).Str("mode", mode)
	if in.Sender != "" {
		e = e.Str("sender", in.Sender)
	}
	e = e.Int("rcpt_count", len(in.To)+len(in.Cc)+len(in.Bcc)).
		Str("subject", truncate(in.Subject, limit))
	if len(in.Labels) > 0 {
		e = e.Interface("labels", truncateLabels(in.Labels, limit))
	}
	e.Str("error", truncate(err.Error(), limit)).Msg("mail rejected")
}

// truncate 依字數（不是 byte 數）截斷字串並標示省略的字數；limit 為 0 表示不限制。
func truncate(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	n := utf8.RuneCountInString(s)
	if n <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if cut == limit {
			return s[:i] + fmt.Sprintf("…(+%d chars)", n-limit)
		}
		cut++
	}
	return s
}

func truncateLabels(labels map[string]string, limit int) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = truncate(v, limit)
	}
	return out
}

// logTime 把時間印成 RFC 3339（UTC）。zerolog 的 TimeFieldFormat 設成 Unix 毫秒，ConsoleWriter
// 只會轉換 time 欄位，其他時間欄位要自己格式化才看得懂。
func logTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// humanDuration 把時間長度印成人看得懂的形式（zerolog 的 Dur 預設輸出浮點毫秒）。
func humanDuration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
