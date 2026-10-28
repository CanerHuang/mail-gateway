package message

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"
)

// MaxAddressLength 為單一地址的長度上限（RFC 5321 的 path 上限扣掉角括號）。
const MaxAddressLength = 254

// HasControl 回報字串是否含控制字元（含 CR、LF、Tab）。會寫進 header 或 log 的欄位
// 一律拒絕控制字元：CR／LF 能插入額外的 header 或偽造 log 行。
func HasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// CheckAddress 檢查單一郵件地址：只接受 ASCII、不含顯示名稱，且能由 net/mail 解析。
func CheckAddress(addr string) error {
	if addr == "" {
		return errors.New("empty address")
	}
	if len(addr) > MaxAddressLength {
		return fmt.Errorf("address longer than %d characters", MaxAddressLength)
	}
	for i := 0; i < len(addr); i++ {
		c := addr[i]
		if c >= 0x80 {
			return errors.New("address must be ASCII")
		}
		if c <= ' ' || c == 0x7f {
			return errors.New("address must not contain spaces or control characters")
		}
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil || parsed.Name != "" || parsed.Address != addr {
		return errors.New("invalid address")
	}
	return nil
}

// Domain 回傳地址 @ 後面的網域；呼叫前應已通過 CheckAddress。
func Domain(addr string) string {
	return addr[strings.LastIndexByte(addr, '@')+1:]
}
