package service

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"maild/internal/message"
	"maild/internal/smtpclient"
	"maild/internal/spool"

	"github.com/rs/zerolog/log"
)

// RunQueue 為第二階段（背景寄送）：每 queue.scan_interval 或收到新郵件時掃描 queue/，
// 把到期的檔案交給 queue.workers 個 worker 寄送。ctx 取消後不再取新的檔案，
// 等進行中的寄送完成才回傳。
func (s *Service) RunQueue(ctx context.Context) {
	jobs := make(chan spool.Name)
	var wg sync.WaitGroup
	for range s.cfg.Queue.Workers {
		wg.Go(func() {
			for name := range jobs {
				s.deliverQueued(name)
				s.inflight.Delete(name.MailID)
			}
		})
	}

	ticker := time.NewTicker(s.cfg.Queue.ScanInterval.Std())
	defer ticker.Stop()
	for {
		s.scan(ctx, jobs)
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case <-ticker.C:
		case <-s.trigger:
		}
	}
}

// triggerScan 讓背景寄送立刻掃描一次；已有待處理的觸發時直接略過。
func (s *Service) triggerScan() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

func (s *Service) scan(ctx context.Context, jobs chan<- spool.Name) {
	names, invalid, err := s.spool.List()
	if err != nil {
		log.Error().Err(err).Msg("scan queue failed")
		return
	}
	for _, filename := range invalid {
		if _, warned := s.invalid.LoadOrStore(filename, true); !warned {
			log.Warn().Str("file", filename).Msg("ignoring queue file with an unexpected name")
		}
	}

	now := s.now()
	slices.SortFunc(names, func(a, b spool.Name) int {
		if c := a.Due.Compare(b.Due); c != 0 {
			return c
		}
		return strings.Compare(a.MailID, b.MailID)
	})
	for _, name := range names {
		if name.Due.After(now) {
			return
		}
		if _, busy := s.inflight.LoadOrStore(name.MailID, true); busy {
			continue
		}
		select {
		case jobs <- name:
		case <-ctx.Done():
			s.inflight.Delete(name.MailID)
			return
		}
	}
}

// deliverQueued 寄送一個佇列檔，並依結果刪除或改名（重新排程）。
func (s *Service) deliverQueued(name spool.Name) {
	start := s.now()
	entry, err := s.spool.OpenEntry(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return // 已被前一輪處理掉（這一輪掃描的清單比較舊）
	case errors.Is(err, spool.ErrCorrupted):
		log.Error().Err(err).Str("mail_id", name.MailID).Str("file", name.String()).Msg("mail failed")
		s.removeQueued(name)
		return
	case err != nil:
		log.Error().Err(err).Str("mail_id", name.MailID).Msg("open queue file failed")
		return
	}
	defer entry.Close()

	meta := &entry.Meta
	fields := mailFields{
		mailID:      meta.MailID,
		client:      meta.Client,
		mode:        modeAsync,
		sender:      meta.Sender,
		from:        meta.From,
		to:          meta.To,
		cc:          meta.Cc,
		bcc:         meta.Bcc,
		subject:     meta.Subject,
		labels:      meta.Labels,
		messageID:   meta.MessageID,
		attachments: meta.Attachments,
		size:        entry.Size,
	}
	attempt := name.Failures + 1

	// 寄出前依當下的設定取寄件帳號；別名被刪除或地址改了，檔案裡的 From 已對不上，只能判定失敗。
	sender, ok := s.snapshot.Load().senders[meta.Sender]
	if !ok || sender.Address != meta.From {
		s.logMail(log.Error(), &fields).Int("attempt", attempt).Str("error", "sender configuration changed").Msg("mail failed")
		s.removeQueued(name)
		return
	}

	ok, probe, st := s.pauses.begin(meta.Sender, start)
	if !ok {
		// 帳號暫停中：延後到暫停結束，不扣重試次數。另一封正在試寄時保持原樣，下一輪再看。
		if !st.probing && st.until.Unix() > name.Due.Unix() {
			s.reschedule(name, spool.Name{Due: st.until, MailID: name.MailID, Failures: name.Failures})
		}
		return
	}

	sentAt := s.now()
	envelope := smtpclient.Envelope{From: meta.From, Recipients: uniqueRecipients(meta.To, meta.Cc, meta.Bcc)}
	resp, err := s.send(context.Background(), sender.SMTP, envelope, s.cfg.SMTP.Timeout.Std(), func(w io.Writer) error {
		if _, err := io.WriteString(w, message.DateHeader(sentAt)); err != nil {
			return err
		}
		_, err := io.Copy(w, entry.Body)
		return err
	})
	now := s.now()
	s.pauses.finish(meta.Sender, probe, outcomeOf(err), now, s.cfg.SMTP.SenderPause.Std())

	limit := s.cfg.Log.FieldLimit()
	duration := humanDuration(now.Sub(start))
	if err == nil {
		s.logMail(log.Info(), &fields).Int("attempt", attempt).Str("smtp_response", truncate(resp, limit)).Str("duration", duration).Msg("mail sent")
		s.removeQueued(name)
		return
	}

	var smtpErr *smtpclient.Error
	kind := smtpclient.KindTemporary
	if errors.As(err, &smtpErr) {
		kind = smtpErr.Kind
	}
	errText := truncate(err.Error(), limit)

	switch {
	case kind == smtpclient.KindQuota || kind == smtpclient.KindAuth:
		// 問題在帳號：延後到暫停結束，不扣重試次數。
		next := spool.Name{Due: now.Add(s.cfg.SMTP.SenderPause.Std()), MailID: name.MailID, Failures: name.Failures}
		s.logMail(log.Warn(), &fields).Int("attempt", attempt).Str("error", errText).Str("next_attempt_at", logTime(next.Due)).Str("duration", duration).Msg("mail retry scheduled")
		s.reschedule(name, next)

	case kind != smtpclient.KindPermanent && attempt < s.cfg.Queue.MaxAttempts:
		next := spool.Name{Due: now.Add(s.retryDelay(name.Failures)), MailID: name.MailID, Failures: attempt}
		e := s.logMail(log.Warn(), &fields).Int("attempt", attempt).Str("error", errText).Str("next_attempt_at", logTime(next.Due)).Str("duration", duration)
		if smtpErr != nil && smtpErr.Unknown {
			// 內容已送完才中斷，可能其實已寄出；重試就可能寄出兩封。
			e = e.Bool("result_unknown", true)
		}
		e.Msg("mail retry scheduled")
		s.reschedule(name, next)

	default:
		e := s.logMail(log.Error(), &fields).Int("attempt", attempt).Str("error", errText).Str("duration", duration)
		if kind != smtpclient.KindPermanent {
			e = e.Str("reason", "max attempts reached")
		}
		e.Msg("mail failed")
		s.removeQueued(name)
	}
}

// retryDelay 為第 failures+1 次失敗後的重試間隔：從 retry_initial_interval 開始每次加倍，
// 最多 retry_max_interval。
func (s *Service) retryDelay(failures int) time.Duration {
	d := s.cfg.Queue.RetryInitialInterval.Std()
	limit := s.cfg.Queue.RetryMaxInterval.Std()
	for i := 0; i < failures && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

func (s *Service) reschedule(old, next spool.Name) {
	if err := s.spool.Reschedule(old, next); err != nil {
		log.Error().Err(err).Str("mail_id", old.MailID).Msg("reschedule queue file failed")
	}
}

func (s *Service) removeQueued(name spool.Name) {
	if err := s.spool.Remove(name); err != nil {
		log.Error().Err(err).Str("mail_id", name.MailID).Msg("remove queue file failed")
		return
	}
	s.releaseQueueSlot()
}
