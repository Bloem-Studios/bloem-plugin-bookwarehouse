package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestExecutableAudioPlugin(t *testing.T) {
	binary := buildExecutable(t)
	manifest := executableManifest(t, binary)
	var schema map[string]any
	if len(manifest.GlobalConfigSchema) != 1 || json.Unmarshal([]byte(manifest.GlobalConfigSchema[0].JsonSchema), &schema) != nil {
		t.Fatal("missing connection schema")
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["content"]; !ok {
		t.Fatal("manifest cannot select audiobook feed")
	}

	var mu sync.Mutex
	body := []byte("hello")
	hash := strings.Repeat("a", 64)
	deny := false
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-API-Key") != fixtureAPIKey {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/warehouse/api/v1/audiobooks/book-a" {
			json.NewEncoder(w).Encode(map[string]any{"id": "book-a", "title": "Audio Book", "updated_at": "2026-10-10T12:00:00Z", "files": []map[string]any{{"id": 41, "audiobook_id": "book-a", "file_path": "private/track.m4b", "file_size": 5, "file_hash": hash, "disc_num": 1, "track_num": 1}}})
			return
		}
		if r.URL.Path == "/warehouse/api/v1/audiobooks/book-a/stream" && r.URL.Query().Get("file_id") == "41" {
			if deny {
				w.WriteHeader(403)
				return
			}
			ranges = append(ranges, r.Header.Get("Range"))
			var first, last int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); err != nil || last >= len(body) {
				w.WriteHeader(416)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))
			w.WriteHeader(206)
			w.Write(body[first : last+1])
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	requested := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(ranges)
	}
	configure := func(t *testing.T) (storagev1.StorageProviderClient, context.Context) {
		t.Helper()
		client := launchExecutable(t, binary)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		t.Cleanup(cancel)
		value, _ := structpb.NewStruct(map[string]any{"base_url": srv.URL + "/warehouse", "api_key": fixtureAPIKey, "allow_insecure_http": true, "content": "audiobooks"})
		if _, err := client.Runtime().Configure(ctx, &pluginv1.ConfigureRequest{Config: []*pluginv1.ConfigEntry{{Key: "connection", Value: value}}}); err != nil {
			t.Fatal(err)
		}
		return storagev1.NewStorageProviderClient(client.Conn()), ctx
	}
	read := func(ctx context.Context, storage storagev1.StorageProviderClient, e *storagev1.Entry, offset, length int64) ([]byte, error) {
		stream, err := storage.Read(ctx, &storagev1.ReadRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision, Offset: offset, Length: length})
		if err != nil {
			return nil, err
		}
		var data []byte
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				return data, nil
			}
			if err != nil {
				return data, err
			}
			data = append(data, chunk.Data...)
		}
	}
	var entry *storagev1.Entry
	t.Run("listing names evidence and reads request ranges", func(t *testing.T) {
		storage, ctx := configure(t)
		desc, err := storage.Describe(ctx, &storagev1.DescribeRequest{})
		if err != nil || desc.GetSources()[0].GetRootEntryId() != "audio-root" {
			t.Fatalf("Describe: %v %v", desc, err)
		}
		page, err := storage.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"})
		if err != nil || len(page.GetEntries()) != 1 || len(requested()) != 0 {
			t.Fatalf("List: %v %v, requested %v", page, err, requested())
		}
		entry = page.Entries[0]
		if !strings.HasPrefix(entry.Revision, "bwaudio-v1:") {
			t.Fatalf("revision %q", entry.Revision)
		}
		if data, err := read(ctx, storage, entry, 1, 3); err != nil || !bytes.Equal(data, []byte("ell")) || !slices.Equal(requested(), []string{"bytes=1-3"}) {
			t.Fatalf("Read: %q %v, requested %v", data, err, requested())
		}
		mu.Lock()
		deny = true
		mu.Unlock()
		if _, err := read(ctx, storage, entry, 0, 5); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("revoked read: %v", err)
		}
		mu.Lock()
		deny = false
		mu.Unlock()
	})
	t.Run("restart keeps the revision without opening the track", func(t *testing.T) {
		storage, ctx := configure(t)
		got, err := storage.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: entry.Id, ExpectedRevision: entry.Revision})
		if err != nil || got.GetEntry().GetRevision() != entry.Revision || len(requested()) != 1 {
			t.Fatalf("restart Stat: %v %v, requested %v", got, err, requested())
		}
		if data, err := read(ctx, storage, entry, 0, 5); err != nil || !bytes.Equal(data, []byte("hello")) {
			t.Fatalf("restart Read: %q %v", data, err)
		}
	})
	t.Run("restart refuses a revision Bookwarehouse no longer describes", func(t *testing.T) {
		mu.Lock()
		hash = strings.Repeat("b", 64)
		mu.Unlock()
		storage, ctx := configure(t)
		if _, err := storage.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: entry.Id, ExpectedRevision: entry.Revision}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("stale Stat: %v", err)
		}
		before := len(requested())
		if data, err := read(ctx, storage, entry, 0, 5); status.Code(err) != codes.FailedPrecondition || len(data) != 0 || len(requested()) != before {
			t.Fatalf("stale Read: %q %v", data, err)
		}
	})
}
