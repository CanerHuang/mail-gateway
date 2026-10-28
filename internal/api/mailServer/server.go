package mailServer

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"maild/internal/config"
	"maild/internal/service"
	api "maild/pkg/grpc/mail"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// Server 封裝 gRPC server 與其監聽器，負責啟動 serve 與停止時的資源釋放。
type Server struct {
	grpc            *grpc.Server
	listener        net.Listener
	shutdownTimeout time.Duration
	errCh           chan error
}

// NewServer 建立啟用 mTLS 的 gRPC server、綁定 cfg.ListenAddress，並立即開始 serve。
// 回傳的 error 表示是否成功啟動；serve 期間的執行期錯誤改由 Err() 取得。
func NewServer(mailService *service.Service, cfg config.ServerConfig, tlsManager *TLSManager) (*Server, error) {
	ka := cfg.Keepalive
	addr := strings.TrimSpace(cfg.ListenAddress)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind tcp %q failed: %w", addr, err)
	}
	log.Info().Str("network", "tcp").Str("address", addr).Msg("gRPC listener bound")
	if cfg.ConnectionLog {
		lis = &loggingListener{Listener: lis, network: "tcp"}
	}

	creds := credentials.NewTLS(tlsManager.ServerConfig())
	if cfg.ConnectionLog {
		creds = loggingCreds{TransportCredentials: creds}
	}

	opts := []grpc.ServerOption{
		grpc.Creds(creds),
		// 限制新連線完成 TLS 與 HTTP/2 handshake 的時間。預設值為 120s，而只建立 TCP
		// 卻不送 preface 的連線（L4 health check、port scan）會一路佔用 accept 迴圈的
		// goroutine 到逾時為止；GracefulStop 必須等這些 goroutine 全數結束才會往下走，
		// 因此這個值直接決定關機的最短保證時間。
		grpc.ConnectionTimeout(cfg.ConnectionTimeout.Std()),
		// 主動探活：閒置 ka.Time 後送 keepalive ping，ka.Timeout 內未收到回應即斷線。
		// MaxConnectionAge 讓連線定期重建，換掉的 CA／憑證才會在新的握手全面生效。
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:                  ka.Time.Std(),
			Timeout:               ka.Timeout.Std(),
			MaxConnectionAge:      ka.MaxConnectionAge.Std(),
			MaxConnectionAgeGrace: ka.MaxConnectionAgeGrace.Std(),
		}),
		// 限制 client 端 keepalive ping 頻率：最短間隔 ka.MinTime，且允許在沒有
		// active stream 時送 ping；過於頻繁的 ping 會被視為違規而中斷連線。
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             ka.MinTime.Std(),
			PermitWithoutStream: true,
		}),
		// 附件直接放在 request 裡，上限由 server.max_request_size_mib 決定（gRPC 預設 4 MiB）。
		grpc.MaxRecvMsgSize(cfg.MaxRequestBytes()),
		grpc.UnaryInterceptor(clientInterceptor(mailService)),
	}
	if cfg.ConnectionLog {
		// 只在啟用時掛上，避免關閉日誌時仍付出 stats.Handler 的 context 成本。
		opts = append(opts, grpc.StatsHandler(connStatsHandler{}))
	}

	g := grpc.NewServer(opts...)
	api.RegisterMailAPIServer(g, NewHandler(mailService))

	s := &Server{
		grpc:            g,
		listener:        lis,
		shutdownTimeout: cfg.ShutdownTimeout.Std(),
		errCh:           make(chan error, 1),
	}
	go func() {
		// Serve 在 GracefulStop 後會回傳 ErrServerStopped，視為正常結束。
		if err := s.grpc.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.errCh <- err
		}
	}()
	return s, nil
}

// Addr 回傳實際監聽的位址（listen_address 的 port 為 0 時由系統分配）。
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Err 回傳 serve 期間發生的非預期錯誤 channel。
func (s *Server) Err() <-chan error {
	return s.errCh
}

// Stop 優雅停止 server：GracefulStop 會等進行中的 RPC（含同步寄送）完成，但本身沒有
// 時間上限，只要有 client 收到 GOAWAY 後不關連線，關機就會無限期卡住。因此改為等待
// shutdownTimeout，逾時就退回 Stop() 強制中斷剩餘連線，確保關機時間有界。
func (s *Server) Stop() {
	gracefulDone := make(chan struct{})
	go func() {
		defer close(gracefulDone)
		s.grpc.GracefulStop()
	}()

	select {
	case <-gracefulDone:
	case <-time.After(s.shutdownTimeout):
		log.Warn().Str("timeout", humanDuration(s.shutdownTimeout)).
			Msg("graceful shutdown timed out, forcing gRPC server stop")
		// Stop 與進行中的 GracefulStop 併發呼叫是安全的，但在極端情況下自己
		// 也可能被阻塞（例如仍有連線卡在 handshake），故放到背景執行，讓關機
		// 流程繼續往下走。
		go s.grpc.Stop()
	}
}
