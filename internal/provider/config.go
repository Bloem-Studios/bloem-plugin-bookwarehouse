package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/verifiedcache"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultBookBytes int64 = 128 << 20
const defaultCacheBytes int64 = 512 << 20

// Config belongs to the secret "connection" manifest field.
type Config struct {
	Content           string `json:"content,omitempty"`
	BaseURL           string `json:"base_url"`
	APIKey            string `json:"api_key"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http"`
	SourceID          string `json:"source_id"`
	SourceName        string `json:"source_name"`
	MaxBookBytes      int64  `json:"max_book_bytes"`
	MaxCacheBytes     int64  `json:"max_cache_bytes"`
	// Request settings, used only by the request router. Both are optional
	// and left out of the storage token key, so adding them never changes
	// an existing source's entry tokens or cursors.
	RequestAPIKey   string `json:"request_api_key,omitempty"`
	PreferredFormat string `json:"preferred_format,omitempty"`
}
type state struct {
	config Config
	client *bookwarehouse.Client
	cache  *verifiedcache.Cache
	covers coverCache
	key    [32]byte
	ctx    context.Context
	cancel context.CancelFunc
	books  audioBookCache
}
type Provider struct {
	storagev1.UnimplementedStorageProviderServer
	mu         sync.RWMutex
	current    *state
	boundaries map[string]string
}

func New() *Provider { return &Provider{} }

// Configure performs no backend I/O. Incomplete/invalid settings remain
// unconfigured so a newly installed executable can complete the handshake.
func (p *Provider) Configure(ctx context.Context, entries []*pluginv1.ConfigEntry) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	var cfg Config
	found := false
	for _, e := range entries {
		if e == nil || e.Key != "connection" || found || e.Value == nil {
			return status.Error(codes.InvalidArgument, "invalid configuration envelope")
		}
		found = true
		raw, err := json.Marshal(e.Value.AsMap())
		if err != nil {
			return status.Error(codes.InvalidArgument, "invalid configuration value")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err != nil {
			return status.Error(codes.InvalidArgument, "invalid configuration value")
		}
	}
	if cfg.Content == "" {
		cfg.Content = "ebooks"
	}
	if cfg.Content != "ebooks" && cfg.Content != "audiobooks" {
		return status.Error(codes.InvalidArgument, "invalid content feed")
	}
	if cfg.PreferredFormat != "" && cfg.PreferredFormat != "epub" && cfg.PreferredFormat != "pdf" {
		return status.Error(codes.InvalidArgument, "invalid preferred format")
	}
	if cfg.SourceID == "" && cfg.Content == "audiobooks" {
		cfg.SourceID = "bookwarehouse-audiobooks"
	}
	if cfg.SourceID == "" {
		cfg.SourceID = "bookwarehouse"
	}
	if cfg.SourceName == "" && cfg.Content == "audiobooks" {
		cfg.SourceName = "Bookwarehouse audiobooks"
	}
	if cfg.SourceName == "" {
		cfg.SourceName = "Bookwarehouse"
	}
	if cfg.MaxBookBytes == 0 {
		cfg.MaxBookBytes = defaultBookBytes
	}
	if cfg.MaxCacheBytes == 0 {
		cfg.MaxCacheBytes = defaultCacheBytes
	}
	var next *state
	if cfg.BaseURL != "" && cfg.APIKey != "" && validID(cfg.SourceID) && len(cfg.SourceName) <= 256 &&
		strings.IndexFunc(cfg.SourceName, unicode.IsControl) < 0 && cfg.MaxBookBytes > 0 && cfg.MaxBookBytes <= 8<<30 &&
		cfg.MaxCacheBytes >= cfg.MaxBookBytes && cfg.MaxCacheBytes <= 32<<30 {
		client, err := bookwarehouse.New(cfg.BaseURL, cfg.APIKey, cfg.AllowInsecureHTTP)
		if err == nil {
			cache, cacheErr := verifiedcache.New(cfg.MaxBookBytes, cfg.MaxCacheBytes)
			if cacheErr != nil {
				client.Close()
				return status.Error(codes.Unavailable, "private cache unavailable")
			}
			keyConfig := cfg
			if keyConfig.Content == "ebooks" {
				keyConfig.Content = ""
			}
			keyConfig.RequestAPIKey, keyConfig.PreferredFormat = "", ""
			// Preserve the original ebook token namespace across the additive
			// audio release; only audio adds a content boundary to the key.
			raw, _ := json.Marshal(keyConfig)
			stateCtx, cancel := context.WithCancel(context.Background())
			next = &state{config: cfg, client: client, cache: cache, key: sha256.Sum256(raw), ctx: stateCtx, cancel: cancel}
		}
	}
	p.mu.Lock()
	if next != nil {
		if previous, ok := p.boundaries[cfg.SourceID]; ok && previous != cfg.Content {
			p.mu.Unlock()
			closeState(next)
			return status.Error(codes.FailedPrecondition, "content feed requires a distinct source identity")
		}
		if p.boundaries == nil {
			p.boundaries = map[string]string{}
		}
		p.boundaries[cfg.SourceID] = cfg.Content
	}
	old := p.current
	p.current = next
	p.mu.Unlock()
	closeState(old)
	return nil
}

// Connection returns the configured connection, for the request router, or
// false while the provider is unconfigured.
func (p *Provider) Connection() (Config, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current == nil {
		return Config{}, false
	}
	return p.current.config, true
}

func closeState(s *state) {
	if s != nil {
		s.cancel()
		_ = s.cache.Close()
		s.client.Close()
	}
}
func (p *Provider) Close() error {
	p.mu.Lock()
	old := p.current
	p.current = nil
	p.mu.Unlock()
	closeState(old)
	return nil
}
func (p *Provider) begin(ctx context.Context, source string) (*state, context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, status.FromContextError(err).Err()
	}
	p.mu.RLock()
	s := p.current
	p.mu.RUnlock()
	if s == nil {
		return nil, nil, nil, status.Error(codes.FailedPrecondition, "Bookwarehouse connection is not configured")
	}
	if source != "" && source != s.config.SourceID {
		return nil, nil, nil, status.Error(codes.NotFound, "source absent")
	}
	scoped, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	done := func() { stop(); cancel() }
	if s.ctx.Err() != nil {
		done()
		return nil, nil, nil, status.Error(codes.Canceled, "configuration replaced")
	}
	return s, scoped, done, nil
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validID(id string) bool { return idPattern.MatchString(id) }
