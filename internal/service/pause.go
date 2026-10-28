package service

import (
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// pauseReason 為寄件帳號暫停的原因；兩者的問題都出在帳號而不是郵件。
type pauseReason int

const (
	pauseQuota pauseReason = iota + 1 // 每日額度用完（Gmail 550 5.4.5）
	pauseAuth                         // 登入失敗（535）
)

func (r pauseReason) String() string {
	if r == pauseAuth {
		return "authentication failed"
	}
	return "daily quota exceeded"
}

// err 為暫停期間同步寄送要回的錯誤。
func (r pauseReason) err() error {
	if r == pauseAuth {
		return ErrSenderAuth
	}
	return ErrSenderQuota
}

// level 為暫停相關 log 的等級：登入失敗需要管理者處理，記成 error。
func (r pauseReason) level() zerolog.Level {
	if r == pauseAuth {
		return zerolog.ErrorLevel
	}
	return zerolog.WarnLevel
}

type pauseState struct {
	reason  pauseReason
	until   time.Time
	probing bool // 暫停已到期，正在試寄一封
}

// outcome 為一次寄送結果對寄件帳號狀態的意義。
type outcome int

const (
	outcomeUnknown outcome = iota // 沒有得到帳號狀態的資訊，例如連線失敗、取消、郵件被拒
	outcomeOK                     // 寄出成功：帳號正常
	outcomeQuota
	outcomeAuth
)

// pauses 記錄暫停中的寄件帳號（只存在記憶體，重啟後消失）。暫停期間同步與非同步都不寄；
// 暫停到期後先放行一封試寄，成功才恢復，仍失敗就再暫停一次。
type pauses struct {
	mu    sync.Mutex
	state map[string]*pauseState
}

// begin 檢查寄件帳號現在能不能寄。ok 為 false 時 st 為目前的暫停狀態；
// probe 為 true 表示這一封是暫停到期後的試寄，結果要透過 finish 回報。
func (p *pauses) begin(alias string, now time.Time) (ok, probe bool, st pauseState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, paused := p.state[alias]
	if !paused {
		return true, false, pauseState{}
	}
	if now.Before(cur.until) || cur.probing {
		return false, false, *cur
	}
	cur.probing = true
	return true, true, *cur
}

// finish 回報寄送結果並更新帳號狀態；暫停、延長、恢復都在這裡記 log。
func (p *pauses) finish(alias string, probe bool, o outcome, now time.Time, pause time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == nil {
		p.state = map[string]*pauseState{}
	}
	cur, paused := p.state[alias]

	switch o {
	case outcomeOK:
		if paused {
			delete(p.state, alias)
			log.Warn().Str("sender", alias).Str("reason", cur.reason.String()).Msg("sender resumed")
		}

	case outcomeQuota, outcomeAuth:
		reason := pauseQuota
		if o == outcomeAuth {
			reason = pauseAuth
		}
		until := now.Add(pause)
		msg := "sender paused"
		if paused {
			msg = "sender pause extended"
			cur.reason, cur.until, cur.probing = reason, until, false
		} else {
			p.state[alias] = &pauseState{reason: reason, until: until}
		}
		log.WithLevel(reason.level()).Str("sender", alias).Str("reason", reason.String()).
			Str("until", logTime(until)).Msg(msg)

	default:
		if paused && probe {
			// 試寄沒有得到帳號狀態的資訊，讓下一封接著試。
			cur.probing = false
		}
	}
}

// clearAuth 解除所有因登入失敗的暫停：reload 代表管理者可能已修正密碼。
func (p *pauses) clearAuth() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for alias, st := range p.state {
		if st.reason == pauseAuth {
			delete(p.state, alias)
			log.Info().Str("sender", alias).Msg("sender authentication pause cleared by reload")
		}
	}
}
