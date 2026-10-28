package message_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"maild/internal/message"
)

func compose(t *testing.T, m *message.Message) string {
	t.Helper()
	var buf bytes.Buffer
	n, err := m.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != int64(buf.Len()) {
		t.Fatalf("WriteTo returned %d, wrote %d", n, buf.Len())
	}
	return buf.String()
}

func baseMessage() *message.Message {
	return &message.Message{
		From:      message.Address{Address: "service@example.com", Name: "Example Service"},
		To:        []message.Address{{Address: "a@example.com"}},
		Subject:   "hello",
		TextBody:  "line1\nline2",
		MessageID: "01JBXK3Q9V8M2T4N6P0R5S7W9Y@example.com",
	}
}

// 每一行都必須以 CRLF 結尾，且不可超過 RFC 5322 的 998 字元上限。
func assertLines(t *testing.T, raw string) {
	t.Helper()
	if strings.Contains(strings.ReplaceAll(raw, "\r\n", ""), "\n") {
		t.Fatalf("found bare LF in message:\n%s", raw)
	}
	for i, line := range strings.Split(raw, "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d has %d characters", i, len(line))
		}
	}
}

func TestWriteTo_TextOnly(t *testing.T) {
	raw := compose(t, baseMessage())
	assertLines(t, raw)

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := msg.Header.Get("Date"); got != "" {
		t.Errorf("Date header should be omitted when Date is zero, got %q", got)
	}
	if got := msg.Header.Get("Message-ID"); got != "<01JBXK3Q9V8M2T4N6P0R5S7W9Y@example.com>" {
		t.Errorf("Message-ID = %q", got)
	}
	mediaType, _, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mediaType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", mediaType)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Name != "Example Service" || from.Address != "service@example.com" {
		t.Errorf("From = %v, %v", from, err)
	}
}

func TestWriteTo_DateHeader(t *testing.T) {
	m := baseMessage()
	m.Date = time.Date(2026, 10, 28, 3, 15, 2, 0, time.UTC)
	msg, err := mail.ReadMessage(strings.NewReader(compose(t, m)))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	got, err := msg.Header.Date()
	if err != nil || !got.Equal(m.Date) {
		t.Errorf("Date = %v, %v", got, err)
	}
}

func TestWriteTo_NonASCIIHeaders(t *testing.T) {
	m := baseMessage()
	m.Subject = strings.Repeat("訂單已出貨，", 40) // 很長的中文主旨，必須拆成多個 encoded-word 並折行
	m.From.Name = "客服中心"
	m.To = []message.Address{{Address: "a@example.com", Name: "王小明"}, {Address: "b@example.com"}}
	raw := compose(t, m)
	assertLines(t, raw)

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("decode subject: %v", err)
	}
	// 折行時 encoded-word 之間的空白會被解碼器移除。
	if subject != m.Subject {
		t.Errorf("subject round-trip mismatch:\n got %q\nwant %q", subject, m.Subject)
	}
	to, err := msg.Header.AddressList("To")
	if err != nil || len(to) != 2 || to[0].Name != "王小明" || to[1].Address != "b@example.com" {
		t.Errorf("To = %v, %v", to, err)
	}
	from, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil || from.Name != "客服中心" {
		t.Errorf("From = %v, %v", from, err)
	}
}

func TestWriteTo_FullStructure(t *testing.T) {
	m := baseMessage()
	m.HTMLBody = `<p>hi</p><img src="cid:logo@example">`
	m.Attachments = []message.Attachment{
		{Filename: "logo.png", ContentType: "image/png", Content: []byte{0x89, 'P', 'N', 'G'}, ContentID: "logo@example"},
		{Filename: "報價單.pdf", ContentType: "application/pdf", Content: bytes.Repeat([]byte("x"), 1000)},
	}
	raw := compose(t, m)
	assertLines(t, raw)

	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	mixed := readMultipart(t, msg.Header.Get("Content-Type"), msg.Body, "multipart/mixed")
	if len(mixed) != 2 {
		t.Fatalf("mixed has %d parts, want 2", len(mixed))
	}

	related := readMultipart(t, mixed[0].contentType, strings.NewReader(mixed[0].body), "multipart/related")
	if len(related) != 2 {
		t.Fatalf("related has %d parts, want 2", len(related))
	}
	alternative := readMultipart(t, related[0].contentType, strings.NewReader(related[0].body), "multipart/alternative")
	if len(alternative) != 2 {
		t.Fatalf("alternative has %d parts, want 2", len(alternative))
	}
	if alternative[0].body != "line1\r\nline2" {
		t.Errorf("text body = %q", alternative[0].body)
	}
	if alternative[1].body != m.HTMLBody {
		t.Errorf("html body = %q", alternative[1].body)
	}
	if got := related[1].header.Get("Content-Id"); got != "<logo@example>" {
		t.Errorf("inline Content-ID = %q", got)
	}

	_, params, err := mime.ParseMediaType(mixed[1].header.Get("Content-Disposition"))
	if err != nil || params["filename"] != "報價單.pdf" {
		t.Errorf("attachment filename = %q, %v", params["filename"], err)
	}
	if !strings.Contains(mixed[1].header.Get("Content-Disposition"), "filename*=") {
		t.Errorf("non-ASCII filename should use RFC 2231: %q", mixed[1].header.Get("Content-Disposition"))
	}
	if mixed[1].body != strings.Repeat("x", 1000) {
		t.Errorf("attachment content mismatch (len %d)", len(mixed[1].body))
	}
}

type part struct {
	header      textproto.MIMEHeader
	contentType string
	body        string
}

// readMultipart 解開一層 multipart，並依 Content-Transfer-Encoding 還原各部分的內容。
func readMultipart(t *testing.T, contentType string, body io.Reader, want string) []part {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != want {
		t.Fatalf("Content-Type = %q (%v), want %s", contentType, err, want)
	}
	mr := multipart.NewReader(body, params["boundary"])
	var parts []part
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("NextRawPart: %v", err)
		}
		data, err := io.ReadAll(decodeTransfer(t, p.Header.Get("Content-Transfer-Encoding"), p))
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		parts = append(parts, part{header: p.Header, contentType: p.Header.Get("Content-Type"), body: string(data)})
	}
}

func decodeTransfer(t *testing.T, encoding string, r io.Reader) io.Reader {
	t.Helper()
	switch strings.ToLower(encoding) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "":
		return r
	default:
		t.Fatalf("unexpected Content-Transfer-Encoding %q", encoding)
		return nil
	}
}
