package config

import (
	"fmt"
	"strings"
	"time"
)

// Duration 是 config 專用的時間長度，TOML 中以字串表示（如 "30s", "1h30m"），
// 解析規則沿用 time.ParseDuration。底層為 time.Duration。
type Duration time.Duration

// Std 回傳對應的 time.Duration，供 service / store 等使用標準型別的呼叫端。
func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		return fmt.Errorf("empty duration")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}
