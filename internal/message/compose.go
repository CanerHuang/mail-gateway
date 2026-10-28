package message

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode/utf8"
)

// Address 為一個收件者或寄件者；Address 已通過 CheckAddress。
type Address struct {
	Address string
	Name    string
}

// Attachment 為一個附件；ContentType 已正規化（不可為空），ContentID 非空表示 inline。
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
	ContentID   string
}

// Message 為組成 MIME 郵件所需的內容。Bcc 不在這裡：bcc 只出現在 SMTP 信封，
// 不可寫進 header。
type Message struct {
	From        Address
	To          []Address
	Cc          []Address
	ReplyTo     []Address
	Subject     string
	TextBody    string
	HTMLBody    string
	Attachments []Attachment
	MessageID   string    // 不含 <>
	Date        time.Time // 零值表示不寫 Date header（佇列檔寄出時才補上）
}

// DateHeader 回傳寄出時補在郵件最前面的 Date header（含結尾 CRLF）。
func DateHeader(t time.Time) string {
	return "Date: " + t.Format(time.RFC1123Z) + "\r\n"
}

// WriteTo 將郵件以 MIME 格式串流寫入 w，行尾一律為 CRLF，回傳寫入的 byte 數。
//
// 結構依內容決定，沒有的層級直接省略：
//
//	multipart/mixed          一般附件
//	└ multipart/related      inline 附件（html 以 cid: 引用）
//	  └ multipart/alternative 純文字與 html
func (m *Message) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	bw := bufio.NewWriter(cw)
	if err := m.write(bw); err != nil {
		return cw.n, err
	}
	if err := bw.Flush(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

func (m *Message) write(w *bufio.Writer) error {
	if !m.Date.IsZero() {
		w.WriteString(DateHeader(m.Date))
	}
	writeHeader(w, "From", formatAddress(m.From))
	if len(m.To) > 0 {
		writeHeader(w, "To", formatAddressList(m.To))
	}
	if len(m.Cc) > 0 {
		writeHeader(w, "Cc", formatAddressList(m.Cc))
	}
	if len(m.ReplyTo) > 0 {
		writeHeader(w, "Reply-To", formatAddressList(m.ReplyTo))
	}
	writeHeader(w, "Subject", encodeText(m.Subject))
	writeHeader(w, "Message-ID", "<"+m.MessageID+">")
	writeHeader(w, "MIME-Version", "1.0")

	root, err := m.buildEntity()
	if err != nil {
		return err
	}
	for _, key := range []string{"Content-Type", "Content-Transfer-Encoding"} {
		if v := root.header.Get(key); v != "" {
			writeHeader(w, key, v)
		}
	}
	w.WriteString("\r\n")
	return root.writeBody(w)
}

// entity 為 MIME 樹的一個節點：leaf 有 body，multipart 有 parts 與 boundary。
type entity struct {
	header   textproto.MIMEHeader
	body     func(io.Writer) error
	parts    []*entity
	boundary string
}

func (m *Message) buildEntity() (*entity, error) {
	var alternatives []*entity
	if m.TextBody != "" {
		alternatives = append(alternatives, textEntity("text/plain", m.TextBody))
	}
	if m.HTMLBody != "" {
		alternatives = append(alternatives, textEntity("text/html", m.HTMLBody))
	}

	var inline, regular []*entity
	for i := range m.Attachments {
		a := &m.Attachments[i]
		e, err := attachmentEntity(a)
		if err != nil {
			return nil, err
		}
		if a.ContentID != "" {
			inline = append(inline, e)
		} else {
			regular = append(regular, e)
		}
	}

	root := multipartEntity("multipart/alternative", alternatives)
	if len(inline) > 0 {
		root = multipartEntity("multipart/related", append([]*entity{root}, inline...))
	}
	if len(regular) > 0 {
		root = multipartEntity("multipart/mixed", append([]*entity{root}, regular...))
	}
	return root, nil
}

// multipartEntity 只有一個子節點時直接回傳該節點，不多包一層。
func multipartEntity(mediaType string, parts []*entity) *entity {
	if len(parts) == 1 {
		return parts[0]
	}
	boundary := randomBoundary()
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType(mediaType, map[string]string{"boundary": boundary}))
	return &entity{header: header, parts: parts, boundary: boundary}
}

func textEntity(mediaType, body string) *entity {
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mediaType+"; charset=UTF-8")
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	return &entity{header: header, body: func(w io.Writer) error {
		qp := quotedprintable.NewWriter(w)
		if _, err := io.WriteString(qp, body); err != nil {
			return err
		}
		return qp.Close()
	}}
}

func attachmentEntity(a *Attachment) (*entity, error) {
	mediaType, params, err := mime.ParseMediaType(a.ContentType)
	if err != nil {
		return nil, fmt.Errorf("attachment %q content_type: %w", a.Filename, err)
	}
	params["name"] = a.Filename
	disposition := "attachment"
	if a.ContentID != "" {
		disposition = "inline"
	}

	header := textproto.MIMEHeader{}
	// FormatMediaType 會將非 ASCII 的參數值依 RFC 2231 編碼（filename*=utf-8''...）。
	header.Set("Content-Type", mime.FormatMediaType(mediaType, params))
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.Filename}))
	header.Set("Content-Transfer-Encoding", "base64")
	if a.ContentID != "" {
		header.Set("Content-ID", "<"+a.ContentID+">")
	}
	content := a.Content
	return &entity{header: header, body: func(w io.Writer) error {
		enc := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: w, width: 76})
		if _, err := enc.Write(content); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		_, err := io.WriteString(w, "\r\n")
		return err
	}}, nil
}

func (e *entity) writeBody(w io.Writer) error {
	if e.body != nil {
		return e.body(w)
	}
	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(e.boundary); err != nil {
		return err
	}
	for _, part := range e.parts {
		header := part.header
		if part.body == nil {
			// 子節點也是 multipart，Content-Type 已含它自己的 boundary。
			header = textproto.MIMEHeader{"Content-Type": {part.header.Get("Content-Type")}}
		}
		pw, err := mw.CreatePart(header)
		if err != nil {
			return err
		}
		if err := part.writeBody(pw); err != nil {
			return err
		}
	}
	return mw.Close()
}

func formatAddress(a Address) string {
	if a.Name == "" {
		return a.Address
	}
	if isPrintableASCII(a.Name) {
		// net/mail 會依需要為 ASCII 顯示名稱加上引號與跳脫。
		return (&mail.Address{Name: a.Name, Address: a.Address}).String()
	}
	return encodeWords(a.Name) + " <" + a.Address + ">"
}

func formatAddressList(list []Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = formatAddress(a)
	}
	return strings.Join(parts, ", ")
}

// encodeText 為 unstructured header（例如 Subject）產生可安全寫入的值：純 ASCII 且
// 能在 998 字元內折行時原樣保留，否則依 RFC 2047 轉成 encoded-word。
func encodeText(s string) string {
	if isPrintableASCII(s) && longestToken(s) <= 900 {
		return s
	}
	return encodeWords(s)
}

// encodeWords 將字串切成多個 RFC 2047 B encoded-word（每個不超過 75 字元），以空白
// 分隔，讓 writeHeader 能在 encoded-word 之間折行；切割時不會拆開多 byte 字元。
func encodeWords(s string) string {
	const maxChunk = 45 // base64 後 60 字元，加上 =?UTF-8?B? 與 ?= 共 72 字元
	var words []string
	for len(s) > 0 {
		n := 0
		for n < len(s) {
			_, size := utf8.DecodeRuneInString(s[n:])
			if n+size > maxChunk {
				break
			}
			n += size
		}
		words = append(words, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	return strings.Join(words, " ")
}

// writeHeader 寫出一行 header，超過 78 字元時在空白處折行（CRLF + 空白）。
func writeHeader(w *bufio.Writer, key, value string) {
	line := key + ": " + value
	for len(line) > 78 {
		cut := strings.LastIndexByte(line[:78], ' ')
		if cut <= len(key)+1 {
			cut = strings.IndexByte(line[78:], ' ')
			if cut < 0 {
				break
			}
			cut += 78
		}
		w.WriteString(line[:cut])
		w.WriteString("\r\n")
		line = line[cut:] // 保留空白作為折行後的開頭
	}
	w.WriteString(line)
	w.WriteString("\r\n")
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

func longestToken(s string) int {
	longest := 0
	for tok := range strings.SplitSeq(s, " ") {
		longest = max(longest, len(tok))
	}
	return longest
}

func randomBoundary() string {
	var buf [18]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err) // crypto/rand 在支援的平台上不會失敗
	}
	return "maild-" + hex.EncodeToString(buf[:])
}

// lineWrapper 每 width 個 byte 插入一次 CRLF，用於 base64 內容（RFC 2045 限制每行 76 字元）。
type lineWrapper struct {
	w     io.Writer
	width int
	col   int
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if l.col == l.width {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return written, err
			}
			l.col = 0
		}
		n := min(l.width-l.col, len(p))
		if _, err := l.w.Write(p[:n]); err != nil {
			return written, err
		}
		l.col += n
		written += n
		p = p[n:]
	}
	return written, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
