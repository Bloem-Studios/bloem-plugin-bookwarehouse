package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestNestedEbookFolders(t *testing.T) {
	b, backend := fixture(t, 3)
	b.books[0]["storage_path"] = "Archive/70s/A.epub"
	target, _ := url.Parse(backend.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	denied := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret" || denied {
			w.WriteHeader(403)
			return
		}
		folder := r.URL.Query().Get("folder")
		if r.URL.Path == "/api/v1/books/folders" {
			dirs := []map[string]string{}
			switch folder {
			case "":
				dirs = []map[string]string{{"name": "Archive", "path": "Archive"}}
			case "Archive":
				if r.URL.Query().Get("after") == "" {
					dirs = []map[string]string{{"name": "70s", "path": "Archive/70s"}}
				} else {
					dirs = []map[string]string{{"name": "80s", "path": "Archive/80s"}}
				}
			case "Archive/70s":
				dirs = []map[string]string{{"name": "Series", "path": "Archive/70s/Series"}}
			}
			next := ""
			if folder == "Archive" && r.URL.Query().Get("after") == "" {
				next = "next-page"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"path": folder, "exists": folder != "missing", "directories": dirs, "next_after": next})
			return
		}
		if r.URL.Path == "/api/v1/books" && folder != "" {
			if folder != "Archive/70s" {
				t.Errorf("unexpected scan filter %q", folder)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"books": b.books[:1], "pagination": map[string]any{"page": 1, "limit": 100, "total_items": 1, "total_pages": 1}})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p := configured(t, srv.URL)
	ctx := context.Background()
	stat, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "epub"})
	if err != nil || stat.GetEntry().GetKind() != storagev1.EntryKind_ENTRY_KIND_DIRECTORY {
		t.Fatalf("directory stat: %v %v", stat, err)
	}
	root, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", DirectoriesOnly: true})
	if err != nil || len(root.GetEntries()) != 1 {
		t.Fatalf("browse format: %v %v", root, err)
	}
	archive := root.Entries[0]
	if archive.Name != "Archive" {
		t.Fatal(archive)
	}
	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: archive.Id, DirectoriesOnly: true, MaxEntries: 1}
	first, err := p.List(ctx, req)
	if err != nil || len(first.GetEntries()) != 1 || first.NextCursor == "" {
		t.Fatalf("first: %v %v", first, err)
	}
	req.Cursor = first.NextCursor
	second, err := p.List(ctx, req)
	if err != nil || second.Entries[0].Name != "80s" {
		t.Fatalf("second: %v %v", second, err)
	}
	req.DirectoryId = "epub"
	if _, err := p.List(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cursor crossed folders: %v", err)
	}
	seventy := first.Entries[0]
	nested, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: seventy.Id, DirectoriesOnly: true})
	if err != nil || nested.Entries[0].Name != "Series" {
		t.Fatalf("deep browse: %v %v", nested, err)
	}
	listed, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: seventy.Id})
	if err != nil || len(listed.GetEntries()) != 1 || listed.Entries[0].Id != "epub/book-000" || !listed.Complete {
		t.Fatalf("scoped scan: %v %v", listed, err)
	}
	if listed.ChangeToken != "" {
		t.Fatal("nested scope cannot use global deletion feed")
	}
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("file pinning relaxed: %v", err)
	}
	missing := "folders/epub/" + base64.RawURLEncoding.EncodeToString([]byte("missing"))
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: missing}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing folder: %v", err)
	}
	for _, path := range []string{"../escape", "/absolute", "A//B", "A\\B"} {
		id := "folders/epub/" + base64.RawURLEncoding.EncodeToString([]byte(path))
		if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: id}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%q accepted: %v", path, err)
		}
	}
	denied = true
	if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: seventy.Id, DirectoriesOnly: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("denial: %v", err)
	}
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: seventy.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stat denial: %v", err)
	}
}

func TestFolderEntriesAndCursorsFitHostBounds(t *testing.T) {
	path := strings.Repeat("a", 250) + "/" + strings.Repeat("b", 250) + "/" + strings.Repeat("c", 250)
	e := folderEntry("epub", path)
	if len(e.Id) > 1024 {
		t.Fatalf("oversized entry identity: %d", len(e.Id))
	}
	_, srv := fixture(t, 0)
	p := configured(t, srv.URL)
	s := p.current
	raw, _ := json.Marshal(folderCursor{Version: 1, Directory: e.Id, After: strings.Repeat("x", 344)})
	if token := sign(s, raw); len(token) > 4096 {
		t.Fatalf("oversized continuation: %d", len(token))
	}
	if proto.Size(e) > 4096 {
		t.Fatal("unexpected entry size")
	}
	if _, _, err := splitFolder("folders/epub/" + base64.RawURLEncoding.EncodeToString([]byte(path+"/long"))); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized directory accepted: %v", err)
	}
}

func TestDirectoryOnlyCannotConsumeFileChangeToken(t *testing.T) {
	_, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	listed, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", DirectoriesOnly: true, ChangesSince: listed.ChangeToken})
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
}
