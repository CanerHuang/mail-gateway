// Package service 為 mail gateway 的核心：驗證郵件、同步寄送、寫入與處理非同步佇列。
package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"maild/internal/config"
	"maild/internal/smtpclient"
	"maild/internal/spool"

	"github.com/oklog/ulid/v2"
	"github.com/rs/zerolog/log"
)

// sendFunc 為實際寄送的函式，測試時替換成假的 SMTP。
type sendFunc func(ctx context.Context, srv config.SenderSMTPConfig, env smtpclient.Envelope, timeout time.Duration, body func(io.Writer) error) (string, error)

// snapshot 為 SIGHUP 可以重新載入的設定，以 atomic pointer 整份替換。
type snapshot struct {
	senders map[string]config.SenderConfig
	clients map[string]config.ClientConfig
}

type Service struct {
	cfg      config.Config // 啟動期設定；其中的 Senders／Clients 以 snapshot 為準
	snapshot atomic.Pointer[snapshot]
	spool    *spool.Spool
	limiter  rateLimiter
	pauses   pauses
	syncSem  chan struct{}

	queueMu sync.Mutex
	queued  int // 佇列中的郵件數，用來檢查 queue.max_queued_mails

	idMu    sync.Mutex
	entropy *ulid.MonotonicEntropy

	trigger  chan struct{}
	inflight sync.Map // 正在寄送的 mail_id，掃描時跳過
	invalid  sync.Map // 已提醒過的無效檔名，避免每次掃描都記 log
	now      func() time.Time
	send     sendFunc
}

// SendResult 為同步寄送成功的結果。
type SendResult struct {
	MailID       string
	MessageID    string
	SMTPResponse string
	SentAt       time.Time
}

// QueueResult 為存入佇列的結果。
type QueueResult struct {
	MailID    string
	MessageID string
}

// SenderInfo 為 ListSenders 回傳的寄件者。
type SenderInfo struct {
	Alias     string
	Address   string
	Name      string
	IsDefault bool
}

// New 打開 spool 並建立 Service；佇列中的郵件數由 queue/ 的檔案數算出。
func New(cfg config.Config) (*Service, error) {
	sp, err := spool.Open(cfg.Queue.SpoolDir)
	if err != nil {
		return nil, err
	}
	names, _, err := sp.List()
	if err != nil {
		return nil, err
	}
	s := &Service{
		cfg:     cfg,
		spool:   sp,
		syncSem: make(chan struct{}, cfg.SMTP.MaxSyncConnections),
		queued:  len(names),
		entropy: ulid.Monotonic(rand.Reader, 0),
		trigger: make(chan struct{}, 1),
		now:     time.Now,
		send:    smtpclient.Send,
	}
	s.snapshot.Store(&snapshot{senders: cfg.Senders, clients: cfg.Clients})
	return s, nil
}

// Reload 替換 [senders] 與 [clients]，並解除登入失敗造成的寄件帳號暫停。
func (s *Service) Reload(cfg config.Config) {
	s.snapshot.Store(&snapshot{senders: cfg.Senders, clients: cfg.Clients})
	s.pauses.clearAuth()
}

// QueuedCount 回傳佇列中的郵件數。
func (s *Service) QueuedCount() int {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return s.queued
}

// AllowedClient 回報 CN 是否在呼叫端名單上；每次 RPC 都會檢查，reload 後立即生效。
func (s *Service) AllowedClient(name string) bool {
	_, ok := s.snapshot.Load().clients[name]
	return ok
}

// ListSenders 回傳呼叫端可用的寄件者，依別名排序。
func (s *Service) ListSenders(clientName string) ([]SenderInfo, error) {
	snap := s.snapshot.Load()
	client, ok := snap.clients[clientName]
	if !ok {
		return nil, ErrClientNotAllowed
	}
	aliases := slices.Sorted(slices.Values(client.Senders))
	out := make([]SenderInfo, len(aliases))
	for i, alias := range aliases {
		sender := snap.senders[alias]
		out[i] = SenderInfo{Alias: alias, Address: sender.Address, Name: sender.Name, IsDefault: alias == client.DefaultSender}
	}
	return out, nil
}

// SendMail 同步寄送：只寄一次，當下成功或失敗都直接回覆。同步連線都在使用中時
// 排隊等待，直到有連線空出來或 ctx 到期；ctx 到期時立刻中止寄送。
func (s *Service) SendMail(ctx context.Context, clientName string, in MailInput) (SendResult, error) {
	start := s.now()
	p, err := s.prepare(clientName, &in, modeSync, start)
	if err != nil {
		s.logRejected(clientName, modeSync, &in, err)
		return SendResult{}, err
	}
	in.Sender = p.fields.sender // 之後被拒時 log 記實際使用的寄件者

	// 先檢查寄件帳號暫停，再計寄送量：暫停期間被拒的請求不佔用寄送量。
	ok, probe, st := s.pauses.begin(p.fields.sender, start)
	if !ok {
		err := fmt.Errorf("%w: sender %q is paused until %s", st.reason.err(), p.fields.sender, logTime(st.until))
		s.logRejected(clientName, modeSync, &in, err)
		return SendResult{}, err
	}
	if err := s.checkRate(clientName, p.client, start); err != nil {
		s.pauses.finish(p.fields.sender, probe, outcomeUnknown, start, 0)
		s.logRejected(clientName, modeSync, &in, err)
		return SendResult{}, err
	}

	select {
	case s.syncSem <- struct{}{}:
	case <-ctx.Done():
		s.pauses.finish(p.fields.sender, probe, outcomeUnknown, s.now(), 0)
		s.logMail(log.Error(), &p.fields).Str("error", "waiting for a free SMTP connection: "+ctx.Err().Error()).Msg("mail failed")
		return SendResult{}, ctx.Err()
	}
	defer func() { <-s.syncSem }()

	sentAt := s.now()
	p.msg.Date = sentAt
	resp, err := s.send(ctx, p.sender.SMTP, p.envelope, s.cfg.SMTP.Timeout.Std(), func(w io.Writer) error {
		n, err := p.msg.WriteTo(w)
		p.fields.size = n
		return err
	})
	s.pauses.finish(p.fields.sender, probe, outcomeOf(err), s.now(), s.cfg.SMTP.SenderPause.Std())
	duration := humanDuration(s.now().Sub(start))

	limit := s.cfg.Log.FieldLimit()
	if err != nil {
		e := s.logMail(log.Error(), &p.fields).Int("attempt", 1).Str("error", truncate(err.Error(), limit)).Str("duration", duration)
		var smtpErr *smtpclient.Error
		if errors.As(err, &smtpErr) && smtpErr.Unknown {
			e = e.Bool("result_unknown", true)
		}
		e.Msg("mail failed")
		return SendResult{}, syncError(err)
	}
	s.logMail(log.Info(), &p.fields).Int("attempt", 1).Str("smtp_response", truncate(resp, limit)).Str("duration", duration).Msg("mail sent")
	return SendResult{MailID: p.fields.mailID, MessageID: p.msg.MessageID, SMTPResponse: resp, SentAt: sentAt}, nil
}

// QueueMail 驗證後把郵件寫進佇列並立即回應，寄送與重試由背景處理。
func (s *Service) QueueMail(ctx context.Context, clientName string, in MailInput, sendAt time.Time) (QueueResult, error) {
	_ = ctx
	now := s.now()
	p, err := s.prepare(clientName, &in, modeAsync, now)
	if err == nil {
		in.Sender = p.fields.sender // 之後被拒時 log 記實際使用的寄件者
	}
	if err == nil && !sendAt.IsZero() && sendAt.After(now.Add(s.cfg.Queue.MaxScheduleAhead.Std())) {
		err = invalid("send_at must be within %s from now", s.cfg.Queue.MaxScheduleAhead.Std())
	}
	if err == nil && !s.reserveQueueSlot() {
		err = ErrQueueFull
	} else if err == nil {
		if err = s.checkRate(clientName, p.client, now); err != nil {
			s.releaseQueueSlot()
		}
	}
	if err != nil {
		s.logRejected(clientName, modeAsync, &in, err)
		return QueueResult{}, err
	}

	due := now
	if sendAt.After(now) {
		due = sendAt
	}
	name := spool.Name{Due: due, MailID: p.fields.mailID}
	err = s.spool.Write(name, p.meta(now), func(w io.Writer) error {
		n, err := p.msg.WriteTo(w)
		p.fields.size = n
		return err
	})
	if err != nil {
		s.releaseQueueSlot()
		s.logMail(log.Error(), &p.fields).Str("error", err.Error()).Msg("mail rejected")
		return QueueResult{}, err
	}

	e := s.logMail(log.Info(), &p.fields)
	if due.After(now) {
		e = e.Str("send_at", logTime(due))
	}
	e.Msg("mail queued")
	s.triggerScan()
	return QueueResult{MailID: p.fields.mailID, MessageID: p.msg.MessageID}, nil
}

func (s *Service) checkRate(clientName string, client config.ClientConfig, now time.Time) error {
	if !s.limiter.allow(clientName, client.Limit(), client.RateLimitWindow.Std(), now) {
		return fmt.Errorf("%w: at most %d mails per %s", ErrRateLimited, client.Limit(), client.RateLimitWindow.Std())
	}
	return nil
}

func (s *Service) reserveQueueSlot() bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.queued >= s.cfg.Queue.MaxQueuedMails {
		return false
	}
	s.queued++
	return true
}

func (s *Service) releaseQueueSlot() {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.queued--
}

func (s *Service) newMailID(now time.Time) string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(now), s.entropy).String()
}

// outcomeOf 把寄送結果轉成對寄件帳號狀態的意義。
func outcomeOf(err error) outcome {
	if err == nil {
		return outcomeOK
	}
	var smtpErr *smtpclient.Error
	if errors.As(err, &smtpErr) {
		switch smtpErr.Kind {
		case smtpclient.KindQuota:
			return outcomeQuota
		case smtpclient.KindAuth:
			return outcomeAuth
		}
	}
	return outcomeUnknown
}

// syncError 把同步寄送的失敗轉成 service 的 sentinel error，訊息附上 SMTP 原始回應。
func syncError(err error) error {
	var smtpErr *smtpclient.Error
	if !errors.As(err, &smtpErr) {
		return err
	}
	switch smtpErr.Kind {
	case smtpclient.KindQuota:
		return fmt.Errorf("%w: %s", ErrSenderQuota, smtpErr.Response)
	case smtpclient.KindAuth:
		return fmt.Errorf("%w: %s", ErrSenderAuth, smtpErr.Response)
	case smtpclient.KindPermanent:
		detail := smtpErr.Response
		if len(smtpErr.Rejected) > 0 {
			detail += fmt.Sprintf(" (rejected: %v)", smtpErr.Rejected)
		}
		return fmt.Errorf("%w: %s", ErrSMTPPermanent, detail)
	case smtpclient.KindCanceled:
		return smtpErr.Err // context.DeadlineExceeded 或 context.Canceled
	default:
		detail := smtpErr.Response
		if detail == "" && smtpErr.Err != nil {
			detail = smtpErr.Err.Error()
		}
		return fmt.Errorf("%w: %s", ErrSMTPTemporary, detail)
	}
}
