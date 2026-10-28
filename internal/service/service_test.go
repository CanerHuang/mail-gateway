package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maild/internal/config"
	"maild/internal/message"
	"maild/internal/smtpclient"
	"maild/internal/spool"
)

// fakeClock 讓測試控制 Service 看到的時間。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeSMTP 取代 smtpclient.Send：記下每次寄送的內容，並依序回傳預先設定的錯誤。
type fakeSMTP struct {
	mu     sync.Mutex
	errs   []error // 依序回傳，用完之後一律成功
	sent   []sentMail
	server []config.SenderSMTPConfig
}

type sentMail struct {
	env  smtpclient.Envelope
	data string
}

func (f *fakeSMTP) Send(_ context.Context, srv config.SenderSMTPConfig, env smtpclient.Envelope, _ time.Duration, body func(io.Writer) error) (string, error) {
	var buf bytes.Buffer
	if err := body(&buf); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.server = append(f.server, srv)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", err
	}
	f.sent = append(f.sent, sentMail{env: env, data: buf.String()})
	return "250 2.0.0 OK queued", nil
}

func (f *fakeSMTP) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.server)
}

func loadTestConfig(t *testing.T, replacements ...string) config.Config {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "conf", "maild.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Replace(string(data), "spool_dir = 'data/spool'", "spool_dir = '"+t.TempDir()+"'", 1)
	for i := 0; i+1 < len(replacements); i += 2 {
		if !strings.Contains(content, replacements[i]) {
			t.Fatalf("config does not contain %q", replacements[i])
		}
		content = strings.Replace(content, replacements[i], replacements[i+1], 1)
	}
	path := filepath.Join(t.TempDir(), "maild.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}
	return cfg
}

func newTestService(t *testing.T, cfg config.Config) (*Service, *fakeSMTP, *fakeClock) {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	smtp := &fakeSMTP{}
	clock := &fakeClock{now: time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)}
	s.send = smtp.Send
	s.now = clock.Now
	return s, smtp, clock
}

func validInput() MailInput {
	return MailInput{
		To:       []message.Address{{Address: "a@example.com", Name: "王小明"}},
		Bcc:      []message.Address{{Address: "hidden@example.com"}},
		Subject:  "訂單已出貨",
		TextBody: "hello",
		Labels:   map[string]string{"order_id": "A1234"},
	}
}

func smtpErr(kind smtpclient.Kind, resp string) error {
	return &smtpclient.Error{Kind: kind, Response: resp}
}

func TestValidateMail(t *testing.T) {
	limits := config.MailConfig{MaxRecipients: 2, MaxAttachmentsSizeMiB: 1, MaxLabels: 2, MaxLabelKeyLength: 8, MaxLabelValueLength: 5}
	cases := []struct {
		name    string
		mutate  func(*MailInput)
		wantErr string
	}{
		{"valid", func(*MailInput) {}, ""},
		{"no recipients", func(in *MailInput) { in.To, in.Bcc = nil, nil }, "at least one recipient"},
		{"too many recipients", func(in *MailInput) { in.Cc = []message.Address{{Address: "c@example.com"}} }, "too many recipients"},
		{"non-ascii address", func(in *MailInput) { in.To[0].Address = "用戶@example.com" }, "to[0]"},
		{"address with name", func(in *MailInput) { in.To[0].Address = "A <a@example.com>" }, "to[0]"},
		{"bad reply_to", func(in *MailInput) { in.ReplyTo = []message.Address{{Address: "nope"}} }, "reply_to[0]"},
		{"header injection in subject", func(in *MailInput) { in.Subject = "hi\r\nBcc: evil@example.com" }, "subject"},
		{"header injection in name", func(in *MailInput) { in.To[0].Name = "x\nBcc: y" }, "to[0].name"},
		{"no body", func(in *MailInput) { in.TextBody = "" }, "text_body or html_body"},
		{"attachments too large", func(in *MailInput) {
			in.Attachments = []message.Attachment{{Filename: "a.bin", Content: make([]byte, 1<<20+1)}}
		}, "exceeds the 1 MiB limit"},
		{"attachment without filename", func(in *MailInput) { in.Attachments = []message.Attachment{{Content: []byte("x")}} }, "filename is required"},
		{"bad content type", func(in *MailInput) {
			in.Attachments = []message.Attachment{{Filename: "a", ContentType: "not a type", Content: []byte("x")}}
		}, "content_type"},
		{"bad content id", func(in *MailInput) {
			in.Attachments = []message.Attachment{{Filename: "a.png", Content: []byte("x"), ContentID: "<logo>"}}
		}, "content_id"},
		{"too many labels", func(in *MailInput) { in.Labels = map[string]string{"a": "1", "b": "2", "c": "3"} }, "too many labels"},
		{"bad label key", func(in *MailInput) { in.Labels = map[string]string{"訂單": "1"} }, "label key"},
		{"label value too long", func(in *MailInput) { in.Labels = map[string]string{"k": "訂單編號長度"} }, "longer than 5"},
		{"label value newline", func(in *MailInput) { in.Labels = map[string]string{"k": "a\nb"} }, "control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			err := validateMail(limits, &in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want InvalidArgument containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateMail_InfersContentType(t *testing.T) {
	in := validInput()
	in.Attachments = []message.Attachment{{Filename: "report.pdf", Content: []byte("x")}, {Filename: "data", Content: []byte("x")}}
	if err := validateMail(config.MailConfig{MaxRecipients: 5, MaxAttachmentsSizeMiB: 1, MaxLabels: 5, MaxLabelKeyLength: 64, MaxLabelValueLength: 64}, &in); err != nil {
		t.Fatal(err)
	}
	if in.Attachments[0].ContentType != "application/pdf" || in.Attachments[1].ContentType != "application/octet-stream" {
		t.Errorf("content types = %q, %q", in.Attachments[0].ContentType, in.Attachments[1].ContentType)
	}
}

func TestSendMail_Success(t *testing.T) {
	s, smtp, clock := newTestService(t, loadTestConfig(t))
	res, err := s.SendMail(context.Background(), "order-service", validInput())
	if err != nil {
		t.Fatalf("SendMail: %v", err)
	}
	if !strings.HasSuffix(res.MessageID, "@example.com") || !strings.HasPrefix(res.MessageID, res.MailID) {
		t.Errorf("MessageID = %q, MailID = %q", res.MessageID, res.MailID)
	}
	if !res.SentAt.Equal(clock.Now()) || res.SMTPResponse != "250 2.0.0 OK queued" {
		t.Errorf("result = %+v", res)
	}
	if len(smtp.sent) != 1 {
		t.Fatalf("sent %d mails", len(smtp.sent))
	}
	got := smtp.sent[0]
	if got.env.From != "service@example.com" || strings.Join(got.env.Recipients, ",") != "a@example.com,hidden@example.com" {
		t.Errorf("envelope = %+v", got.env)
	}
	msg, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" || strings.Contains(got.data, "hidden@example.com") {
		t.Error("bcc must not appear in the message")
	}
	if date, err := msg.Header.Date(); err != nil || !date.Equal(clock.Now()) {
		t.Errorf("Date = %v, %v", date, err)
	}
	if from, _ := mail.ParseAddress(msg.Header.Get("From")); from == nil || from.Name != "Example Service" {
		t.Errorf("From = %q", msg.Header.Get("From"))
	}
}

func TestSendMail_SenderSelection(t *testing.T) {
	s, smtp, _ := newTestService(t, loadTestConfig(t))
	in := validInput()
	in.Sender = "billing"
	in.SenderName = "帳務通知"
	if _, err := s.SendMail(context.Background(), "order-service", in); err != nil {
		t.Fatal(err)
	}
	if smtp.server[0].Host != "smtp.example.com" || smtp.sent[0].env.From != "billing@example.com" {
		t.Errorf("billing should use its own SMTP server: %+v / %+v", smtp.server[0], smtp.sent[0].env)
	}

	in.Sender = "billing"
	if _, err := s.SendMail(context.Background(), "member-service", in); !errors.Is(err, ErrSenderNotAllowed) {
		t.Errorf("member-service may not use billing, got %v", err)
	}
	if _, err := s.SendMail(context.Background(), "unknown", validInput()); !errors.Is(err, ErrClientNotAllowed) {
		t.Errorf("unknown client, got %v", err)
	}
}

func TestSendMail_RateLimit(t *testing.T) {
	s, _, clock := newTestService(t, loadTestConfig(t, "rate_limit = 100\nrate_limit_window = '1h'", "rate_limit = 2\nrate_limit_window = '1h'"))
	for i := range 2 {
		if _, err := s.SendMail(context.Background(), "member-service", validInput()); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if _, err := s.SendMail(context.Background(), "member-service", validInput()); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third send should be rate limited, got %v", err)
	}
	clock.Advance(time.Hour + time.Second)
	if _, err := s.SendMail(context.Background(), "member-service", validInput()); err != nil {
		t.Fatalf("window passed, send should succeed: %v", err)
	}
}

func TestSendMail_ErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{smtpErr(smtpclient.KindTemporary, "451 4.3.0 later"), ErrSMTPTemporary},
		{smtpErr(smtpclient.KindPermanent, "550 5.1.1 user unknown"), ErrSMTPPermanent},
		{smtpErr(smtpclient.KindQuota, "550 5.4.5 quota"), ErrSenderQuota},
		{smtpErr(smtpclient.KindAuth, "535 5.7.8 bad credentials"), ErrSenderAuth},
		{&smtpclient.Error{Kind: smtpclient.KindCanceled, Err: context.DeadlineExceeded}, context.DeadlineExceeded},
	}
	for _, tc := range cases {
		s, smtp, _ := newTestService(t, loadTestConfig(t))
		smtp.errs = []error{tc.err}
		_, err := s.SendMail(context.Background(), "order-service", validInput())
		if !errors.Is(err, tc.want) {
			t.Errorf("%v: got %v, want %v", tc.err, err, tc.want)
		}
		if r, ok := tc.err.(*smtpclient.Error); ok && r.Response != "" && !strings.Contains(err.Error(), r.Response) {
			t.Errorf("error %q should include the SMTP response", err)
		}
	}
}

func TestSendMail_SenderPause(t *testing.T) {
	cfg := loadTestConfig(t)
	s, smtp, clock := newTestService(t, cfg)
	smtp.errs = []error{smtpErr(smtpclient.KindQuota, "550 5.4.5 Daily user sending quota exceeded.")}

	if _, err := s.SendMail(context.Background(), "order-service", validInput()); !errors.Is(err, ErrSenderQuota) {
		t.Fatalf("first send should hit the quota, got %v", err)
	}
	// 暫停期間不連線到 SMTP server，直接回錯誤。
	if _, err := s.SendMail(context.Background(), "order-service", validInput()); !errors.Is(err, ErrSenderQuota) {
		t.Fatalf("paused sender should be rejected, got %v", err)
	}
	if smtp.calls() != 1 {
		t.Fatalf("paused sender should not contact SMTP, calls = %d", smtp.calls())
	}
	// 其他寄件帳號照常寄送。
	in := validInput()
	in.Sender = "billing"
	if _, err := s.SendMail(context.Background(), "order-service", in); err != nil {
		t.Fatalf("other sender should still work: %v", err)
	}

	clock.Advance(cfg.SMTP.SenderPause.Std())
	if _, err := s.SendMail(context.Background(), "order-service", validInput()); err != nil {
		t.Fatalf("probe after the pause should be allowed: %v", err)
	}
	if ok, _, _ := s.pauses.begin("service", clock.Now()); !ok {
		t.Error("successful probe should resume the sender")
	}
}

func TestReload_ClearsAuthPause(t *testing.T) {
	cfg := loadTestConfig(t)
	s, smtp, _ := newTestService(t, cfg)
	smtp.errs = []error{smtpErr(smtpclient.KindAuth, "535 5.7.8 Username and Password not accepted")}
	if _, err := s.SendMail(context.Background(), "order-service", validInput()); !errors.Is(err, ErrSenderAuth) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.SendMail(context.Background(), "order-service", validInput()); !errors.Is(err, ErrSenderAuth) {
		t.Fatalf("sender should be paused, got %v", err)
	}
	s.Reload(cfg)
	if _, err := s.SendMail(context.Background(), "order-service", validInput()); err != nil {
		t.Fatalf("reload should clear the authentication pause: %v", err)
	}
}

func TestReload_RemovedClient(t *testing.T) {
	s, _, _ := newTestService(t, loadTestConfig(t))
	if !s.AllowedClient("member-service") {
		t.Fatal("member-service should be allowed")
	}
	reloaded := loadTestConfig(t, "[clients.member-service]", "[clients.other-service]")
	s.Reload(reloaded)
	if s.AllowedClient("member-service") {
		t.Error("removed client should be rejected right after reload")
	}
}

func TestListSenders(t *testing.T) {
	s, _, _ := newTestService(t, loadTestConfig(t))
	senders, err := s.ListSenders("order-service")
	if err != nil {
		t.Fatal(err)
	}
	if len(senders) != 2 || senders[0].Alias != "billing" || senders[1].Alias != "service" || !senders[1].IsDefault || senders[0].IsDefault {
		t.Errorf("senders = %+v", senders)
	}
}

// queueOne 收下一封郵件並回傳它在佇列裡的檔名。
func queueOne(t *testing.T, s *Service, in MailInput, sendAt time.Time) spool.Name {
	t.Helper()
	res, err := s.QueueMail(context.Background(), "order-service", in, sendAt)
	if err != nil {
		t.Fatalf("QueueMail: %v", err)
	}
	names, _, err := s.spool.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n.MailID == res.MailID {
			return n
		}
	}
	t.Fatalf("queued mail %s not found", res.MailID)
	return spool.Name{}
}

func onlyName(t *testing.T, s *Service) (spool.Name, bool) {
	t.Helper()
	names, _, err := s.spool.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) > 1 {
		t.Fatalf("expected at most one queue file, got %v", names)
	}
	if len(names) == 0 {
		return spool.Name{}, false
	}
	return names[0], true
}

func TestQueue_DeliverSuccess(t *testing.T) {
	s, smtp, clock := newTestService(t, loadTestConfig(t))
	in := validInput()
	in.Attachments = []message.Attachment{{Filename: "報價單.pdf", Content: bytes.Repeat([]byte("x"), 100_000)}}
	name := queueOne(t, s, in, time.Time{})
	if s.QueuedCount() != 1 {
		t.Fatalf("queued = %d", s.QueuedCount())
	}

	clock.Advance(5 * time.Second)
	s.deliverQueued(name)
	if _, ok := onlyName(t, s); ok || s.QueuedCount() != 0 {
		t.Fatal("delivered mail should be removed from the queue")
	}
	if len(smtp.sent) != 1 {
		t.Fatalf("sent = %d", len(smtp.sent))
	}
	msg, err := mail.ReadMessage(strings.NewReader(smtp.sent[0].data))
	if err != nil {
		t.Fatal(err)
	}
	// Date 在寄出時才補上，是寄出的時間而不是收下的時間。
	if date, err := msg.Header.Date(); err != nil || !date.Equal(clock.Now()) {
		t.Errorf("Date = %v, %v; want %v", date, err, clock.Now())
	}
	if strings.Join(smtp.sent[0].env.Recipients, ",") != "a@example.com,hidden@example.com" {
		t.Errorf("recipients = %v", smtp.sent[0].env.Recipients)
	}
}

func TestQueue_TemporaryFailureRetries(t *testing.T) {
	cfg := loadTestConfig(t)
	s, smtp, clock := newTestService(t, cfg)
	name := queueOne(t, s, validInput(), time.Time{})

	for attempt := 1; attempt < cfg.Queue.MaxAttempts; attempt++ {
		smtp.errs = []error{smtpErr(smtpclient.KindTemporary, "451 4.3.0 later")}
		s.deliverQueued(name)
		next, ok := onlyName(t, s)
		if !ok || next.Failures != attempt {
			t.Fatalf("attempt %d: next = %+v, %v", attempt, next, ok)
		}
		wantDelay := min(cfg.Queue.RetryInitialInterval.Std()<<(attempt-1), cfg.Queue.RetryMaxInterval.Std())
		if got := next.Due.Sub(clock.Now()); got > wantDelay || got < wantDelay-time.Second {
			t.Errorf("attempt %d: retry delay = %s, want %s", attempt, got, wantDelay)
		}
		clock.Advance(wantDelay)
		name = next
	}

	smtp.errs = []error{smtpErr(smtpclient.KindTemporary, "451 4.3.0 later")}
	s.deliverQueued(name)
	if _, ok := onlyName(t, s); ok {
		t.Fatal("mail should be removed after max attempts")
	}
	if s.QueuedCount() != 0 {
		t.Errorf("queued = %d", s.QueuedCount())
	}
}

func TestQueue_PermanentFailureRemoves(t *testing.T) {
	s, smtp, _ := newTestService(t, loadTestConfig(t))
	name := queueOne(t, s, validInput(), time.Time{})
	smtp.errs = []error{smtpErr(smtpclient.KindPermanent, "550 5.1.1 user unknown")}
	s.deliverQueued(name)
	if _, ok := onlyName(t, s); ok {
		t.Fatal("permanently failed mail should be removed")
	}
}

func TestQueue_SenderPauseKeepsAttempts(t *testing.T) {
	cfg := loadTestConfig(t)
	s, smtp, clock := newTestService(t, cfg)
	first := queueOne(t, s, validInput(), time.Time{})
	second := queueOne(t, s, validInput(), time.Time{})

	smtp.errs = []error{smtpErr(smtpclient.KindQuota, "550 5.4.5 quota")}
	s.deliverQueued(first)
	// 同帳號的下一封不連線到 SMTP，直接延後到暫停結束。
	s.deliverQueued(second)
	if smtp.calls() != 1 {
		t.Fatalf("paused sender should not be contacted again, calls = %d", smtp.calls())
	}
	names, _, _ := s.spool.List()
	pauseEnd := clock.Now().Add(cfg.SMTP.SenderPause.Std())
	for _, n := range names {
		if n.Failures != 0 {
			t.Errorf("%s: pause must not count as a failed attempt", n)
		}
		if n.Due.Unix() != pauseEnd.Unix() {
			t.Errorf("%s: due = %v, want %v", n, n.Due, pauseEnd)
		}
	}
}

func TestQueue_SenderConfigChanged(t *testing.T) {
	cfg := loadTestConfig(t)
	s, smtp, _ := newTestService(t, cfg)
	name := queueOne(t, s, validInput(), time.Time{})
	s.Reload(loadTestConfig(t, "address = 'service@example.com'", "address = 'noreply@example.com'"))
	s.deliverQueued(name)
	if _, ok := onlyName(t, s); ok || smtp.calls() != 0 {
		t.Fatal("mail whose sender address changed should be removed without sending")
	}
}

func TestQueueMail_Limits(t *testing.T) {
	s, _, clock := newTestService(t, loadTestConfig(t, "max_queued_mails = 1000", "max_queued_mails = 1"))
	if _, err := s.QueueMail(context.Background(), "order-service", validInput(), clock.Now().Add(8*24*time.Hour)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("send_at beyond max_schedule_ahead should be rejected, got %v", err)
	}
	queueOne(t, s, validInput(), clock.Now().Add(time.Hour))
	if _, err := s.QueueMail(context.Background(), "order-service", validInput(), time.Time{}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("queue should be full, got %v", err)
	}
}

func TestRunQueue_ScheduledAndTriggered(t *testing.T) {
	s, smtp, clock := newTestService(t, loadTestConfig(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunQueue(ctx)
		close(done)
	}()

	later := queueOne(t, s, validInput(), clock.Now().Add(time.Hour))
	queueOne(t, s, validInput(), time.Time{}) // 立即寄送：收下時觸發掃描

	deadline := time.Now().Add(5 * time.Second)
	for smtp.calls() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if smtp.calls() != 1 {
		t.Fatalf("immediate mail should be delivered, calls = %d", smtp.calls())
	}
	if n, ok := onlyName(t, s); !ok || n.MailID != later.MailID {
		t.Fatalf("scheduled mail should stay queued, got %+v %v", n, ok)
	}
	cancel()
	<-done
}

func TestSendMail_PausedRejectionDoesNotCountRate(t *testing.T) {
	s, smtp, clock := newTestService(t, loadTestConfig(t, "rate_limit = 100\nrate_limit_window = '1h'", "rate_limit = 2\nrate_limit_window = '1h'"))
	smtp.errs = []error{smtpErr(smtpclient.KindQuota, "550 5.4.5 quota")}
	if _, err := s.SendMail(context.Background(), "member-service", validInput()); !errors.Is(err, ErrSenderQuota) {
		t.Fatalf("got %v", err)
	}
	for range 3 {
		if _, err := s.SendMail(context.Background(), "member-service", validInput()); !errors.Is(err, ErrSenderQuota) {
			t.Fatalf("paused sender should be rejected, got %v", err)
		}
	}
	clock.Advance(time.Hour) // 暫停結束，但寄送量視窗還在：只有第一封佔用了寄送量
	if _, err := s.SendMail(context.Background(), "member-service", validInput()); err != nil {
		t.Fatalf("rejections during the pause must not use up the rate limit: %v", err)
	}
}

func TestQueue_DeduplicatesRecipients(t *testing.T) {
	s, smtp, _ := newTestService(t, loadTestConfig(t))
	in := validInput()
	in.Cc = []message.Address{{Address: "A@example.com"}}
	s.deliverQueued(queueOne(t, s, in, time.Time{}))
	if got := strings.Join(smtp.sent[0].env.Recipients, ","); got != "a@example.com,hidden@example.com" {
		t.Errorf("recipients = %q", got)
	}
}
