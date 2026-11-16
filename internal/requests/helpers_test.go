package requests

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	isbn         = "9780547928227"
	storageKey   = "synthetic-secret"
	curatorKey   = "synthetic-curator-secret"
	bookRequest  = "stable-host-request"
	audioRequest = "0b6c1a52-8a37-4c64-9f0e-2c1d5a7e9b10"
)

// installation is the plugin installation's own connection, as Runtime.Configure
// stores it for the storage provider.
func installation(content, base string, edit ...func(*Settings)) func() (Settings, bool) {
	s := Settings{Content: content, BaseURL: base, APIKey: storageKey, AllowHTTP: true}
	for _, e := range edit {
		e(&s)
	}
	return func() (Settings, bool) { return s, true }
}

func ebookRouter(base string, edit ...func(*Settings)) *Router {
	return New(installation("ebooks", base, edit...))
}

// conn is the host's per-call request connection. Its base URL and key are
// deliberately unrelated: the router uses the installation's.
func conn(t *testing.T, extra map[string]any) *pb.RouterConnection {
	t.Helper()
	s, err := structpb.NewStruct(extra)
	if err != nil {
		t.Fatal(err)
	}
	return &pb.RouterConnection{Id: "test", BaseUrl: "https://ignored.invalid", ApiKey: "ignored-host-key", Config: s}
}

func req(c *pb.RouterConnection) *pb.FulfillRequest {
	return &pb.FulfillRequest{CapabilityId: EbooksCapability, Request: &pb.RequestDescriptor{MediaType: "book", Title: "Synthetic Book", ExternalIds: map[string]string{"isbn": isbn, "bloem_request_id": bookRequest}}, Connections: []*pb.RouterConnection{c}}
}

func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func emptyBooks(w http.ResponseWriter) {
	jsonResponse(w, map[string]any{"books": []any{}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 0, "total_pages": 0}})
}

func emptyMonitoring(w http.ResponseWriter) {
	jsonResponse(w, map[string]any{"requests": []any{}, "page": 1, "limit": 100, "total": 0})
}

func base64JSON(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
