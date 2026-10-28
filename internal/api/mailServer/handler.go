package mailServer

import (
	"context"
	"errors"
	"time"

	"maild/internal/message"
	"maild/internal/service"
	api "maild/pkg/grpc/mail"
	"maild/pkg/version"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Handler struct {
	api.UnimplementedMailAPIServer
	service *service.Service
}

func NewHandler(mailService *service.Service) *Handler {
	return &Handler{service: mailService}
}

func (h *Handler) SendMail(ctx context.Context, req *api.SendMailRequest) (*api.SendMailResponse, error) {
	result, err := h.service.SendMail(ctx, clientFromContext(ctx), toMailInput(req.GetMail()))
	if err != nil {
		return nil, toStatusError(err)
	}
	return &api.SendMailResponse{
		MailId:          result.MailID,
		HeaderMessageId: result.MessageID,
		SmtpResponse:    result.SMTPResponse,
		SentAt:          timestamppb.New(result.SentAt),
	}, nil
}

func (h *Handler) QueueMail(ctx context.Context, req *api.QueueMailRequest) (*api.QueueMailResponse, error) {
	// send_at 留空表示立即寄送，以零值的 time.Time 傳給 service。
	var sendAt time.Time
	if req.SendAt != nil {
		if err := req.SendAt.CheckValid(); err != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid argument: send_at: "+err.Error())
		}
		sendAt = req.SendAt.AsTime()
	}
	result, err := h.service.QueueMail(ctx, clientFromContext(ctx), toMailInput(req.GetMail()), sendAt)
	if err != nil {
		return nil, toStatusError(err)
	}
	return &api.QueueMailResponse{
		MailId:          result.MailID,
		HeaderMessageId: result.MessageID,
	}, nil
}

func (h *Handler) ListSenders(ctx context.Context, _ *emptypb.Empty) (*api.ListSendersResponse, error) {
	senders, err := h.service.ListSenders(clientFromContext(ctx))
	if err != nil {
		return nil, toStatusError(err)
	}
	resp := &api.ListSendersResponse{Senders: make([]*api.Sender, len(senders))}
	for i, s := range senders {
		resp.Senders[i] = &api.Sender{Alias: s.Alias, Address: s.Address, Name: s.Name, IsDefault: s.IsDefault}
	}
	return resp, nil
}

func (h *Handler) VersionGet(ctx context.Context, _ *emptypb.Empty) (*api.VersionInfo, error) {
	_ = ctx
	return &api.VersionInfo{
		Version: version.Version,
		Commit:  version.Commit,
		Date:    version.Date,
	}, nil
}

func toMailInput(m *api.Mail) service.MailInput {
	in := service.MailInput{
		Sender:     m.GetSender(),
		SenderName: m.GetSenderName(),
		To:         toAddresses(m.GetTo()),
		Cc:         toAddresses(m.GetCc()),
		Bcc:        toAddresses(m.GetBcc()),
		ReplyTo:    toAddresses(m.GetReplyTo()),
		Subject:    m.GetSubject(),
		TextBody:   m.GetTextBody(),
		HTMLBody:   m.GetHtmlBody(),
		Labels:     m.GetLabels(),
	}
	for _, a := range m.GetAttachments() {
		in.Attachments = append(in.Attachments, message.Attachment{
			Filename:    a.GetFilename(),
			ContentType: a.GetContentType(),
			Content:     a.GetContent(),
			ContentID:   a.GetContentId(),
		})
	}
	return in
}

func toAddresses(list []*api.Address) []message.Address {
	if len(list) == 0 {
		return nil
	}
	out := make([]message.Address, len(list))
	for i, a := range list {
		out[i] = message.Address{Address: a.GetAddress(), Name: a.GetName()}
	}
	return out
}

// toStatusError 把 service 的 sentinel error 轉成 gRPC status；對照表見 doc/errors.md。
func toStatusError(err error) error {
	switch {
	case err == nil:
		return nil

	// === PermissionDenied ===
	case errors.Is(err, service.ErrClientNotAllowed),
		errors.Is(err, service.ErrSenderNotAllowed):
		return status.Error(codes.PermissionDenied, err.Error())

	// === InvalidArgument ===
	case errors.Is(err, service.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())

	// === ResourceExhausted（訊息開頭固定，呼叫端據此分辨原因）===
	case errors.Is(err, service.ErrRateLimited),
		errors.Is(err, service.ErrQueueFull),
		errors.Is(err, service.ErrSenderQuota):
		return status.Error(codes.ResourceExhausted, err.Error())

	// === Unavailable（暫時失敗，可稍後重送或改用 QueueMail）===
	case errors.Is(err, service.ErrSenderAuth),
		errors.Is(err, service.ErrSMTPTemporary):
		return status.Error(codes.Unavailable, err.Error())

	// === FailedPrecondition（永久失敗，修正內容前不要原封不動重送）===
	case errors.Is(err, service.ErrSMTPPermanent):
		return status.Error(codes.FailedPrecondition, err.Error())

	// === 呼叫端 deadline 到期或取消 ===
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())

	// === Internal（fallback）===
	default:
		log.Error().Err(err).Msg("internal error")
		return status.Error(codes.Internal, "internal server error")
	}
}
