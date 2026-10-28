package mailServer

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/stats"
)

// 連線日誌分成三層，因為單靠 stats.Handler 看不到全貌：
//
//   - stats.Handler 的 ConnBegin 是在 grpc-go 的 serveStreams 才觸發，也就是
//     HTTP/2 handshake 成功之後。只建立 TCP 卻不送 preface 的連線（L4 health
//     check、port scan、誤連的 TLS client）在 stats.Handler 眼中完全不存在 ——
//     而那正是最需要被看見的一類。
//   - 因此再包一層 listener，讓每一條 accept 到的連線無論是否講 gRPC 都留下紀錄。
//   - listener 看不到憑證，stats.Handler 也拿不到 TLS 狀態，所以再包一層
//     credentials，在 TLS 握手完成時記下 client 憑證的 CN，握手失敗時記下原因
//     （例如憑證過期、不是這個 CA 簽的）。
//
// 三層合起來可以這樣讀：只有 accepted/closed 而沒有 TLS handshake completed，代表
// 對方沒有通過 mTLS；有 TLS 而沒有 established，代表對方沒有完成 HTTP/2 handshake；
// closed 那行的 read_bytes=0 更進一步表示對方連一個 byte 都沒送，是純粹的 TCP 探測。

// loggingListener 包裝 net.Listener，記錄每一條被 accept 的連線。
type loggingListener struct {
	net.Listener
	network string
}

func (l *loggingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	log.Info().
		Str("network", l.network).
		Str("peer", peerAddr(conn.RemoteAddr())).
		Msg("connection accepted")
	return &loggingConn{Conn: conn, network: l.network, acceptedAt: time.Now()}, nil
}

// loggingConn 統計連線收送的 byte 數，並在關閉時輸出存活時間。
type loggingConn struct {
	net.Conn
	network    string
	acceptedAt time.Time
	read       atomic.Int64
	wrote      atomic.Int64
	closed     atomic.Bool
}

func (c *loggingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.read.Add(int64(n))
	return n, err
}

func (c *loggingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.wrote.Add(int64(n))
	return n, err
}

// Close 可能被 grpc-go 重複呼叫（transport 與 server 各關一次），以 CAS 確保只記一次。
func (c *loggingConn) Close() error {
	err := c.Conn.Close()
	if c.closed.CompareAndSwap(false, true) {
		log.Info().
			Str("network", c.network).
			Str("peer", peerAddr(c.RemoteAddr())).
			Str("lifetime", humanDuration(time.Since(c.acceptedAt))).
			Int64("read_bytes", c.read.Load()).
			Int64("wrote_bytes", c.wrote.Load()).
			Msg("connection closed")
	}
	return err
}

// loggingCreds 包裝 TransportCredentials，在 TLS 握手完成或失敗時記錄 peer 與 client 憑證 CN。
type loggingCreds struct {
	credentials.TransportCredentials
}

func (c loggingCreds) ServerHandshake(rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ServerHandshake(rawConn)
	peer := peerAddr(rawConn.RemoteAddr())
	if err != nil {
		log.Info().Str("peer", peer).Err(err).Msg("TLS handshake failed")
		return conn, info, err
	}
	cn := "unknown"
	if tlsInfo, ok := info.(credentials.TLSInfo); ok {
		if chains := tlsInfo.State.VerifiedChains; len(chains) > 0 && len(chains[0]) > 0 {
			cn = chains[0][0].Subject.CommonName
		}
	}
	log.Info().Str("peer", peer).Str("cn", cn).Msg("TLS handshake completed")
	return conn, info, nil
}

func (c loggingCreds) Clone() credentials.TransportCredentials {
	return loggingCreds{TransportCredentials: c.TransportCredentials.Clone()}
}

// connStatsHandler 實作 grpc/stats.Handler，補上 handshake 成功之後的 gRPC 連線視角。
type connStatsHandler struct{}

type connStatsKey struct{}

func (connStatsHandler) TagConn(ctx context.Context, info *stats.ConnTagInfo) context.Context {
	return context.WithValue(ctx, connStatsKey{}, peerAddr(info.RemoteAddr))
}

// HandleConn 只記錄 ConnBegin。ConnEnd 的內容會被 loggingConn.Close 那行完全涵蓋
// （且多了 lifetime 與 byte 數），重複記一次只是噪音。
func (connStatsHandler) HandleConn(ctx context.Context, s stats.ConnStats) {
	if _, ok := s.(*stats.ConnBegin); !ok {
		return
	}
	peer, ok := ctx.Value(connStatsKey{}).(string)
	if !ok {
		return
	}
	log.Info().Str("peer", peer).Msg("gRPC connection established")
}

// TagRPC / HandleRPC 刻意留空：per-RPC 日誌的量級比連線層高一到兩個數量級，
// 需要時應另外開一個獨立開關，不要跟著連線日誌一起打開。
func (connStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (connStatsHandler) HandleRPC(context.Context, stats.RPCStats) {}

// humanDuration 把時間長度印成人看得懂的形式（zerolog 的 Dur 預設輸出浮點毫秒，
// 例如 1998.637653，在 console 上很難讀）。
func humanDuration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

// peerAddr 取出可讀的對端位址。UDS 的 RemoteAddr 通常為空字串或 "@"（匿名 socket），
// 此時沒有有意義的對端資訊可記。
func peerAddr(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	if s := addr.String(); s != "" && s != "@" {
		return s
	}
	return "unknown"
}
