package service

import "errors"

// service 層只回傳以下 sentinel error（以 %w 包上細節），由 handler 統一轉成 gRPC code；
// 對照表見 doc/errors.md。
var (
	// ErrClientNotAllowed：憑證有效，但 CN 不在 [clients]。
	ErrClientNotAllowed = errors.New("client not allowed")
	// ErrSenderNotAllowed：寄件者別名不在這個呼叫端可用的清單。
	ErrSenderNotAllowed = errors.New("sender not allowed")
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrRateLimited      = errors.New("rate limit exceeded")
	ErrQueueFull        = errors.New("queue full")
	// ErrSenderQuota：寄件帳號達到每日上限，暫停中。
	ErrSenderQuota = errors.New("sender daily quota exceeded")
	// ErrSenderAuth：寄件帳號登入失敗，暫停中。
	ErrSenderAuth    = errors.New("sender authentication failed")
	ErrSMTPTemporary = errors.New("smtp temporary failure")
	ErrSMTPPermanent = errors.New("smtp permanent failure")
)
