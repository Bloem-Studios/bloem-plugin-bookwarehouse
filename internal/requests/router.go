// Package requests serves request_router.v1 for one Bookwarehouse plugin
// installation: ebook requests through Bookwarehouse monitoring, or
// audiobook requests through Bookwarehouse's AudiobookBay search and torrent
// downloads, depending on the installation's content setting.
package requests

import (
	"context"
	"fmt"
	"sync"
	"time"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Capability IDs in the manifest. Each installation serves only the one
// matching its content setting.
const (
	EbooksCapability     = "ebooks"
	AudiobooksCapability = "audiobooks"
)

// Settings is the installation's own Bookwarehouse connection, the one its
// storage source uses. Requests never use the host connection's base_url or
// api_key.
type Settings struct {
	Content         string // "ebooks" (the default) or "audiobooks"
	BaseURL         string
	APIKey          string
	RequestAPIKey   string // optional Curator key for audiobook torrents
	AllowHTTP       bool
	PreferredFormat string // ebooks: "epub" (the default) or "pdf"
}

// audiobookKey is the key for audiobook requests: request_api_key when set,
// else api_key.
func (s Settings) audiobookKey() string {
	if s.RequestAPIKey != "" {
		return s.RequestAPIKey
	}
	return s.APIKey
}

type Router struct {
	pb.UnimplementedRequestRouterServer
	settings func() (Settings, bool)
	// Serializes submissions within the plugin process; host leases still
	// serialize across processes.
	mu sync.Mutex
}

// New serves requests with the installation connection settings returns.
// It reports false while the installation has no usable connection.
func New(settings func() (Settings, bool)) *Router { return &Router{settings: settings} }

const notConfigured = "Configure the Bookwarehouse connection (address and API key) on this plugin installation first."

func kindOf(capabilityID string) (string, error) {
	switch capabilityID {
	case EbooksCapability:
		return "ebooks", nil
	case AudiobooksCapability:
		return "audiobooks", nil
	}
	return "", status.Error(codes.InvalidArgument, "Unknown request capability.")
}

// begin resolves the installation settings for a capability, refusing the
// kind this installation does not serve.
func (r *Router) begin(capabilityID string) (Settings, error) {
	kind, err := kindOf(capabilityID)
	if err != nil {
		return Settings{}, err
	}
	s, ok := r.settings()
	if !ok || s.BaseURL == "" || s.APIKey == "" {
		return Settings{}, status.Error(codes.FailedPrecondition, notConfigured)
	}
	if s.Content == "" {
		s.Content = "ebooks"
	}
	if s.Content != kind {
		return Settings{}, status.Error(codes.FailedPrecondition, fmt.Sprintf(
			"This Bookwarehouse installation serves %s, so it does not take %s requests. Use the Bookwarehouse installation whose content is %s.", s.Content, kind, kind))
	}
	return s, nil
}

func (r *Router) Fulfill(ctx context.Context, in *pb.FulfillRequest) (*pb.FulfillResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	s, err := r.begin(in.CapabilityId)
	if err != nil {
		return nil, err
	}
	if in.CapabilityId == EbooksCapability {
		return r.fulfillEbook(ctx, s, in)
	}
	return r.fulfillAudiobook(ctx, s, in)
}

func (r *Router) CheckStatus(ctx context.Context, in *pb.CheckStatusRequest) (*pb.CheckStatusResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	s, err := r.begin(in.CapabilityId)
	if err != nil {
		return nil, err
	}
	if in.CapabilityId == EbooksCapability {
		return r.checkEbook(ctx, s, in)
	}
	return r.checkAudiobook(ctx, s, in)
}

func (r *Router) Validate(ctx context.Context, in *pb.ValidateRequest) (*pb.ValidateResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	if _, err := kindOf(in.CapabilityId); err != nil {
		return nil, err
	}
	if s, ok := r.settings(); !ok || s.BaseURL == "" || s.APIKey == "" {
		return &pb.ValidateResponse{FormError: notConfigured}, nil
	}
	s, err := r.begin(in.CapabilityId)
	if err != nil {
		return nil, err
	}
	if in.CapabilityId == EbooksCapability {
		return validateEbook(s, in), nil
	}
	return validateAudiobook(ctx, s, in), nil
}

func (r *Router) ListConfigOptions(ctx context.Context, in *pb.ListConfigOptionsRequest) (*pb.ListConfigOptionsResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	s, err := r.begin(in.CapabilityId)
	if err != nil {
		return nil, err
	}
	if in.CapabilityId == EbooksCapability {
		return ebookOptions(s, in.Connection), nil
	}
	return audiobookOptions(ctx, s)
}

func (r *Router) TestConnection(ctx context.Context, in *pb.TestConnectionRequest) (*pb.TestConnectionResponse, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Missing request.")
	}
	s, err := r.begin(in.CapabilityId)
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			if _, ok := r.settings(); !ok {
				return &pb.TestConnectionResponse{Message: notConfigured}, nil
			}
		}
		return nil, err
	}
	if in.CapabilityId == EbooksCapability {
		return testEbook(ctx, s), nil
	}
	return testAudiobook(ctx, s), nil
}

// bounded caps ctx at limit, leaving reserve before the caller's deadline.
func bounded(ctx context.Context, limit, reserve time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		limit = min(limit, time.Until(deadline)-reserve)
	}
	return context.WithTimeout(ctx, limit)
}
