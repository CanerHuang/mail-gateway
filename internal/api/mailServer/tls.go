package mailServer

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"maild/internal/config"

	"github.com/rs/zerolog/log"
)

// TLSManager 持有 mTLS 的 server 憑證與 client CA。SIGHUP 時 Reload 重新讀取檔案，
// 之後的每一次 TLS 握手都會用到最新的設定；已建立的連線不受影響。
type TLSManager struct {
	cfg     config.TLSConfig
	current atomic.Pointer[tls.Config]
	// warned 記錄每張憑證上次提醒到期的日期，同一張憑證每天最多提醒一次。
	warned sync.Map
	now    func() time.Time
}

// NewTLSManager 載入 server 憑證與 client CA；任一檔案無法讀取或格式錯誤都會回傳 error。
func NewTLSManager(cfg config.TLSConfig) (*TLSManager, error) {
	m := &TLSManager{cfg: cfg, now: time.Now}
	if err := m.Reload(); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload 重新讀取 server 憑證、私鑰與 client CA；失敗時沿用舊的設定。
func (m *TLSManager) Reload() error {
	cert, err := tls.LoadX509KeyPair(m.cfg.CertFile, m.cfg.KeyFile)
	if err != nil {
		return fmt.Errorf("load server certificate failed: %w", err)
	}
	caPEM, err := os.ReadFile(m.cfg.ClientCAFile)
	if err != nil {
		return fmt.Errorf("read client CA file failed: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("client CA file contains no PEM certificates")
	}
	m.checkExpiry("server", cert.Leaf)

	m.current.Store(&tls.Config{
		// 呼叫端都是 Go，一律只接受 TLS 1.3。
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		// GetConfigForClient 回傳的設定會取代原本的設定，ALPN 也要自己帶上 h2，
		// 否則 grpc-go 會因為沒有協商出 h2 而拒絕連線。
		NextProtos:       []string{"h2"},
		VerifyConnection: m.verifyConnection,
	})
	return nil
}

// ServerConfig 回傳交給 gRPC credentials 的設定；實際的憑證與 CA 在每次握手時才取最新的。
func (m *TLSManager) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return m.current.Load(), nil
		},
	}
}

// verifyConnection 在 client 憑證驗證通過後檢查剩餘有效期。
func (m *TLSManager) verifyConnection(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) > 0 {
		m.checkExpiry("client", cs.PeerCertificates[0])
	}
	return nil
}

// checkExpiry 在憑證剩餘有效期少於 tls.expiry_warning 時記 warning log。
func (m *TLSManager) checkExpiry(kind string, cert *x509.Certificate) {
	if cert == nil {
		return
	}
	now := m.now()
	remaining := cert.NotAfter.Sub(now)
	if remaining >= m.cfg.ExpiryWarning.Std() {
		return
	}
	key := kind + "/" + cert.Issuer.String() + "/" + cert.SerialNumber.String()
	today := now.UTC().Format(time.DateOnly)
	if last, ok := m.warned.Swap(key, today); ok && last == today {
		return
	}
	log.Warn().
		Str("certificate", kind).
		Str("cn", cert.Subject.CommonName).
		Str("not_after", cert.NotAfter.UTC().Format(time.RFC3339)).
		Str("remaining", humanDuration(remaining.Truncate(time.Hour))).
		Msg("certificate expires soon")
}
