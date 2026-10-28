package mailServer

import (
	"context"
	"errors"

	"maild/internal/service"
	api "maild/pkg/grpc/mail"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type clientNameKey struct{}

// clientInterceptor 以 client 憑證的 CN 識別呼叫端，並在每次 RPC 檢查呼叫端名單；
// 名單在 reload 後立即生效，已建立的連線也一樣。VersionGet 只需通過 mTLS 握手。
func clientInterceptor(svc *service.Service) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == api.MailAPI_VersionGet_FullMethodName {
			return handler(ctx, req)
		}
		name, err := peerCommonName(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		if !svc.AllowedClient(name) {
			log.Warn().Str("client", name).Str("peer", peerFromContext(ctx)).Str("method", info.FullMethod).Msg("client not allowed")
			return nil, status.Error(codes.PermissionDenied, service.ErrClientNotAllowed.Error())
		}
		return handler(context.WithValue(ctx, clientNameKey{}, name), req)
	}
}

// peerCommonName 從 TLS 驗證過的憑證鏈取出 client 憑證的 CN；不採用呼叫端自己宣稱的名稱。
func peerCommonName(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("missing peer information")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("connection is not using TLS")
	}
	chains := info.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", errors.New("missing verified client certificate")
	}
	return chains[0][0].Subject.CommonName, nil
}

func clientFromContext(ctx context.Context) string {
	name, _ := ctx.Value(clientNameKey{}).(string)
	return name
}

func peerFromContext(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		return peerAddr(p.Addr)
	}
	return "unknown"
}
