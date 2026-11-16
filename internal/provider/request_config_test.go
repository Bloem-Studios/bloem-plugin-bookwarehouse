package provider

import (
	"context"
	"testing"

	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func configureMap(t *testing.T, p *Provider, values map[string]any) error {
	t.Helper()
	cfg, err := structpb.NewStruct(values)
	if err != nil {
		t.Fatal(err)
	}
	return p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}})
}

// Installations 30 and 36 upgrade with their stored configuration unchanged:
// the request fields are optional, and setting them must not move the storage
// token namespace (cursors and entry tokens are keyed by the connection).
func TestRequestFieldsAreOptionalAndKeepTheStorageNamespace(t *testing.T) {
	for _, content := range []string{"", "ebooks", "audiobooks"} {
		base := map[string]any{"base_url": "https://books.example", "api_key": "secret"}
		if content != "" {
			base["content"] = content
		}
		before := New()
		if err := configureMap(t, before, base); err != nil {
			t.Fatal(err)
		}
		with := map[string]any{"request_api_key": "curator-secret", "preferred_format": "pdf"}
		for k, v := range base {
			with[k] = v
		}
		after := New()
		if err := configureMap(t, after, with); err != nil {
			t.Fatal(err)
		}
		if before.current == nil || after.current == nil || before.current.key != after.current.key {
			t.Fatalf("content %q: request fields changed the storage token key", content)
		}
		got, ok := after.Connection()
		if !ok || got.RequestAPIKey != "curator-secret" || got.PreferredFormat != "pdf" || got.APIKey != "secret" {
			t.Fatalf("connection snapshot = %+v, %v", got, ok)
		}
		_ = before.Close()
		_ = after.Close()
	}
}

func TestRequestFieldValidation(t *testing.T) {
	p := New()
	defer func() { _ = p.Close() }()
	for _, bad := range []map[string]any{
		{"base_url": "https://books.example", "api_key": "secret", "preferred_format": "mobi"},
		{"base_url": "https://books.example", "api_key": "secret", "request_api_key": true},
	} {
		if err := configureMap(t, p, bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("config %v: %v", bad, err)
		}
	}
	if _, ok := p.Connection(); ok {
		t.Fatal("rejected configuration must not be served")
	}
}
