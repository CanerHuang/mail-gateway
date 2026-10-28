// Package smtpclient 以串流方式透過 SMTP 寄出一封郵件，並依約定的規則分類失敗原因。
package smtpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"maild/internal/config"
)

// Kind 為寄送失敗的分類，決定呼叫端（同步回應或佇列重試）接下來怎麼處理。
type Kind int

const (
	// KindTemporary：4xx、連線問題、逾時，稍後重試可能成功。
	KindTemporary Kind = iota + 1
	// KindPermanent：5xx 或部分收件者被拒，重試也不會成功。
	KindPermanent
	// KindQuota：寄件帳號達到每日上限（Gmail 回 550 5.4.5），要暫停整個帳號。
	KindQuota
	// KindAuth：寄件帳號登入失敗（535），要暫停整個帳號並通知管理者。
	KindAuth
	// KindCanceled：呼叫端取消或 deadline 到期。
	KindCanceled
)

func (k Kind) String() string {
	switch k {
	case KindTemporary:
		return "temporary"
	case KindPermanent:
		return "permanent"
	case KindQuota:
		return "quota"
	case KindAuth:
		return "auth"
	case KindCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Error 為寄送失敗的結果。
type Error struct {
	Kind Kind
	// Response 為 SMTP server 的原始回應（例如 "550 5.1.1 user unknown"），連線問題時為空。
	Response string
	// Rejected 為被拒的收件者，只在部分收件者被拒時有值。
	Rejected []string
	// Unknown 表示郵件內容已送完、正在等 server 回應時中斷，無法確定是否已寄出。
	Unknown bool
	Err     error
}

func (e *Error) Error() string {
	msg := e.Kind.String() + " failure"
	if e.Response != "" {
		msg += ": " + e.Response
	} else if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if len(e.Rejected) > 0 {
		msg += " (rejected: " + strings.Join(e.Rejected, ", ") + ")"
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Envelope 為 SMTP 信封：MAIL FROM 與 RCPT TO（含 bcc）。
type Envelope struct {
	From       string
	Recipients []string
}

// Send 連線到 srv 寄出一封郵件，body 負責把郵件內容串流寫進 DATA。成功時回傳
// server 收下郵件時的回應（例如 "250 2.0.0 OK queued as 4A1B2C"），失敗時回傳 *Error。
//
// timeout 是整次寄送（連線、交握到送完內容）的上限；ctx 是呼叫端的 deadline，
// 兩者取先到者。ctx 到期屬於 KindCanceled，timeout 到期屬於 KindTemporary。
func Send(ctx context.Context, srv config.SenderSMTPConfig, env Envelope, timeout time.Duration, body func(io.Writer) error) (string, error) {
	callerCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	s := &session{callerCtx: callerCtx, ctx: ctx}
	return s.send(srv, env, body)
}

type session struct {
	callerCtx context.Context
	ctx       context.Context
	// dataSent 在 DATA 的結尾（單獨一行 .）送出後設為 true，之後的中斷無法確定是否已寄出。
	dataSent bool
}

func (s *session) send(srv config.SenderSMTPConfig, env Envelope, body func(io.Writer) error) (string, error) {
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	tlsConfig := &tls.Config{ServerName: srv.Host, MinVersion: tls.VersionTLS12}

	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if srv.Security == config.SecurityTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(s.ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(s.ctx, "tcp", addr)
	}
	if err != nil {
		return "", s.classify(err)
	}
	// net/smtp 不吃 context：deadline 到期或呼叫端取消時直接關閉底層連線，
	// 讓進行中的讀寫立即失敗。
	if deadline, ok := s.ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(s.ctx, func() { conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, srv.Host)
	if err != nil {
		conn.Close()
		return "", s.classify(err)
	}
	defer c.Close()

	if srv.Security == config.SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return "", &Error{Kind: KindTemporary, Err: errors.New("server does not support STARTTLS")}
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return "", s.classify(err)
		}
	}
	if srv.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", srv.Username, srv.Password, srv.Host)); err != nil {
			return "", s.classify(err)
		}
	}
	if err := c.Mail(env.From); err != nil {
		return "", s.classify(err)
	}

	// 部分收件者被拒時整封判失敗，但先把所有收件者都試過，錯誤訊息才能列出全部被拒的地址。
	var rejected []string
	var rejectErr *textproto.Error
	for _, rcpt := range env.Recipients {
		err := c.Rcpt(rcpt)
		if err == nil {
			continue
		}
		classified := s.classify(err)
		var tpErr *textproto.Error
		if classified.Kind == KindPermanent && errors.As(err, &tpErr) {
			rejected = append(rejected, rcpt)
			rejectErr = tpErr
			continue
		}
		return "", classified
	}
	if len(rejected) > 0 {
		return "", &Error{Kind: KindPermanent, Response: formatResponse(rejectErr.Code, rejectErr.Msg), Rejected: rejected, Err: rejectErr}
	}

	// 不用 Client.Data：它的 Close 會丟掉 server 收下郵件時的回應，而那正是要回給呼叫端的內容。
	id, err := c.Text.Cmd("DATA")
	if err != nil {
		return "", s.classify(err)
	}
	c.Text.StartResponse(id)
	_, _, err = c.Text.ReadResponse(354)
	c.Text.EndResponse(id)
	if err != nil {
		return "", s.classify(err)
	}

	// DotWriter 會處理行首的 .（dot-stuffing），並在 Close 時送出結尾的單獨一行 .。
	w := c.Text.DotWriter()
	if err := body(w); err != nil {
		return "", s.classify(err)
	}
	if err := w.Close(); err != nil {
		return "", s.classify(err)
	}
	s.dataSent = true

	code, msg, err := c.Text.ReadResponse(250)
	if err != nil {
		return "", s.classify(err)
	}
	c.Quit() // 郵件已被收下，QUIT 失敗不影響結果
	return formatResponse(code, msg), nil
}

// classify 依約定的規則分類錯誤；Unknown 由目前是否已送完郵件內容決定。
func (s *session) classify(err error) *Error {
	if expired(s.callerCtx) {
		return &Error{Kind: KindCanceled, Unknown: s.dataSent, Err: callerErr(s.callerCtx)}
	}
	if expired(s.ctx) {
		// 呼叫端還在等，到期的是 smtp.timeout：視同連線逾時。
		return &Error{Kind: KindTemporary, Unknown: s.dataSent, Err: fmt.Errorf("smtp timeout: %w", err)}
	}

	var tpErr *textproto.Error
	if !errors.As(err, &tpErr) {
		// 連不上、TLS 握手失敗、連線中斷等。
		return &Error{Kind: KindTemporary, Unknown: s.dataSent, Err: err}
	}
	resp := formatResponse(tpErr.Code, tpErr.Msg)
	switch {
	case tpErr.Code == 535:
		return &Error{Kind: KindAuth, Response: resp, Err: err}
	case tpErr.Code == 550 && strings.HasPrefix(strings.TrimSpace(tpErr.Msg), "5.4.5"):
		return &Error{Kind: KindQuota, Response: resp, Err: err}
	case tpErr.Code >= 500:
		return &Error{Kind: KindPermanent, Response: resp, Err: err}
	default:
		// 4xx，以及不在預期中的回應碼。
		return &Error{Kind: KindTemporary, Response: resp, Unknown: s.dataSent, Err: err}
	}
}

// expired 回報 ctx 是否已取消或到期。連線的 read/write deadline 與 ctx 的計時器設在同一時間點，
// 但 deadline 可能比計時器早一點觸發，此時 ctx.Err() 仍是 nil，所以也要直接比對 deadline。
func expired(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

// callerErr 回傳呼叫端取消的原因；計時器還沒觸發時以 DeadlineExceeded 表示。
func callerErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

// formatResponse 把多行回應合成一行，方便寫進 log 與回給呼叫端。
func formatResponse(code int, msg string) string {
	return strconv.Itoa(code) + " " + strings.ReplaceAll(msg, "\n", " ")
}
