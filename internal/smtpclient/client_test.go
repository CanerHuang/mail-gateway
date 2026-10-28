package smtpclient_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"maild/internal/config"
	"maild/internal/smtpclient"
	"maild/internal/smtptest"
)

func server(port int, username string) config.SenderSMTPConfig {
	// PlainAuth 只允許在 TLS 或 localhost 上送出密碼，測試用 127.0.0.1 不加密即可。
	return config.SenderSMTPConfig{Host: "127.0.0.1", Port: port, Security: config.SecurityNone, Username: username, Password: "secret"}
}

func body(content string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, content)
		return err
	}
}

var envelope = smtpclient.Envelope{From: "service@example.com", Recipients: []string{"a@example.com", "b@example.com"}}

func TestSend_Success(t *testing.T) {
	srv := smtptest.NewServer(t)
	// 行首的 . 必須經過 dot-stuffing，server 端還原後內容要完全相同。
	content := "Subject: hi\r\n\r\n.leading dot\r\nbody\r\n"
	resp, err := smtpclient.Send(context.Background(), server(srv.Port(), "service@example.com"), envelope, 5*time.Second, body(content))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp != "250 2.0.0 OK queued as 4A1B2C" {
		t.Errorf("response = %q", resp)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 || msgs[0].Data != content {
		t.Fatalf("server received %+v, want data %q", msgs, content)
	}
	if msgs[0].From != envelope.From || strings.Join(msgs[0].Recipients, ",") != "a@example.com,b@example.com" {
		t.Errorf("envelope = %+v", msgs[0])
	}
}

func TestSend_Classification(t *testing.T) {
	cases := []struct {
		name     string
		cmd      string
		reply    string
		want     smtpclient.Kind
		rejected []string
	}{
		{"temporary at MAIL", "MAIL", "451 4.3.0 try again later", smtpclient.KindTemporary, nil},
		{"permanent at DATA end", "END", "554 5.7.1 message rejected", smtpclient.KindPermanent, nil},
		{"auth failure", "AUTH", "535 5.7.8 Username and Password not accepted", smtpclient.KindAuth, nil},
		{"gmail quota", "END", "550 5.4.5 Daily user sending quota exceeded.", smtpclient.KindQuota, nil},
		{"partial recipient rejection", "RCPT:b@example.com", "550 5.1.1 user unknown", smtpclient.KindPermanent, []string{"b@example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := smtptest.NewServer(t)
			srv.Reply(tc.cmd, tc.reply)
			_, err := smtpclient.Send(context.Background(), server(srv.Port(), "u"), envelope, 5*time.Second, body("x\r\n"))
			var smtpErr *smtpclient.Error
			if !errors.As(err, &smtpErr) {
				t.Fatalf("want *smtpclient.Error, got %v", err)
			}
			if smtpErr.Kind != tc.want {
				t.Errorf("kind = %s, want %s (%v)", smtpErr.Kind, tc.want, err)
			}
			if smtpErr.Response != tc.reply {
				t.Errorf("response = %q, want %q", smtpErr.Response, tc.reply)
			}
			if !slices.Equal(smtpErr.Rejected, tc.rejected) {
				t.Errorf("rejected = %v, want %v", smtpErr.Rejected, tc.rejected)
			}
		})
	}
}

func TestSend_ConnectionRefused(t *testing.T) {
	srv := smtptest.NewServer(t)
	port := srv.Port()
	srv.Close()
	_, err := smtpclient.Send(context.Background(), server(port, ""), envelope, time.Second, body("x\r\n"))
	var smtpErr *smtpclient.Error
	if !errors.As(err, &smtpErr) || smtpErr.Kind != smtpclient.KindTemporary {
		t.Fatalf("want temporary failure, got %v", err)
	}
}

func TestSend_CallerDeadlineAfterData(t *testing.T) {
	srv := smtptest.NewServer(t)
	srv.HangAfterData()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := smtpclient.Send(ctx, server(srv.Port(), ""), envelope, 10*time.Second, body("x\r\n"))
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Send should abort at the caller deadline, took %s", elapsed)
	}
	var smtpErr *smtpclient.Error
	if !errors.As(err, &smtpErr) || smtpErr.Kind != smtpclient.KindCanceled || !smtpErr.Unknown {
		t.Fatalf("want canceled with unknown result, got %+v", err)
	}
}

func TestSend_SMTPTimeoutIsTemporary(t *testing.T) {
	srv := smtptest.NewServer(t)
	srv.HangAfterData()
	_, err := smtpclient.Send(context.Background(), server(srv.Port(), ""), envelope, 300*time.Millisecond, body("x\r\n"))
	var smtpErr *smtpclient.Error
	if !errors.As(err, &smtpErr) || smtpErr.Kind != smtpclient.KindTemporary || !smtpErr.Unknown {
		t.Fatalf("want temporary with unknown result, got %+v", err)
	}
}
