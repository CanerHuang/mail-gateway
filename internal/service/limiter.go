package service

import (
	"sync"
	"time"
)

// rateLimiter 以滑動視窗計算每個呼叫端最近一段時間內收下的郵件數。計數只存在記憶體，
// gateway 重啟後歸零；gateway 只有一個執行個體，不需要共用狀態。
type rateLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time // 依時間遞增
}

// allow 在未超過上限時記一筆並回傳 true；limit 為 0 表示不限制。
func (l *rateLimiter) allow(client string, limit int, window time.Duration, now time.Time) bool {
	if limit == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.events == nil {
		l.events = map[string][]time.Time{}
	}

	cutoff := now.Add(-window)
	events := l.events[client]
	i := 0
	for i < len(events) && !events[i].After(cutoff) {
		i++
	}
	events = events[i:]
	if len(events) >= limit {
		l.events[client] = events
		return false
	}
	l.events[client] = append(events, now)
	return true
}
