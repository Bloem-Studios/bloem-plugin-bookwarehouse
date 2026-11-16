package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	pluginv1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type backend struct {
	mu           sync.Mutex
	body         []byte
	books        []map[string]any
	downloads    int
	probes       int
	deny         bool
	corrupt      bool
	denyDownload bool
	totalByPage  map[int]int
	listPages    []int
	deletions    []map[string]any
	cover        []byte
	lookups      int
	coverReads   int
	listDelay    time.Duration
}

// slow holds a catalog request for delay, or until the client gives up.
func slow(r *http.Request, delay time.Duration) {
	if delay <= 0 {
		return
	}
	select {
	case <-time.After(delay):
	case <-r.Context().Done():
	}
}

// listed mirrors Bookwarehouse's list query: an inclusive whole-second
// updated_after filter, ordered by whole-second updated_at, then id.
func listed(books []map[string]any, updatedAfter string) []map[string]any {
	second := func(book map[string]any) int64 {
		ts, _ := time.Parse(time.RFC3339Nano, book["updated_at"].(string))
		return ts.Unix()
	}
	var since int64 = math.MinInt64
	if updatedAfter != "" {
		ts, _ := time.Parse(time.RFC3339, updatedAfter)
		since = ts.Unix()
	}
	out := []map[string]any{}
	for _, book := range books {
		if second(book) >= since {
			out = append(out, book)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := second(out[i]), second(out[j]); a != b {
			return a < b
		}
		return out[i]["id"].(string) < out[j]["id"].(string)
	})
	return out
}

func fixture(t *testing.T, count int) (*backend, *httptest.Server) {
	t.Helper()
	b := &backend{body: bytes.Repeat([]byte("ebook"), 40000)}
	h := sha256.Sum256(b.body)
	for i := 0; i < count; i++ {
		b.books = append(b.books, map[string]any{"id": fmt.Sprintf("book-%03d", i), "title": fmt.Sprintf("Title %d", i), "file_format": "epub", "file_size": len(b.body), "file_hash": hex.EncodeToString(h[:]), "updated_at": "2026-10-07T12:00:00Z"})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if r.Header.Get("X-API-Key") != "secret" || b.deny {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path == "/api/v1/books/deletions" {
			since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
			var page, limit int
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			fmt.Sscan(r.URL.Query().Get("limit"), &limit)
			if err != nil || page < 1 || limit < 1 {
				w.WriteHeader(400)
				return
			}
			matching := []map[string]any{}
			for _, d := range b.deletions {
				if at, _ := time.Parse(time.RFC3339, d["deleted_at"].(string)); !at.Before(since) {
					matching = append(matching, d)
				}
			}
			start := min((page-1)*limit, len(matching))
			end := min(start+limit, len(matching))
			_ = json.NewEncoder(w).Encode(map[string]any{"deletions": matching[start:end], "pagination": map[string]any{"page": page, "limit": limit, "total_items": len(matching)}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/cover/medium") {
			b.coverReads++
			if b.cover == nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(b.cover)
			return
		}
		if r.URL.Path == "/api/v1/books" {
			var page, limit int
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			fmt.Sscan(r.URL.Query().Get("limit"), &limit)
			if page < 1 || limit < 1 || r.URL.Query().Get("sort") != "updated_at" || r.URL.Query().Get("order") != "asc" {
				w.WriteHeader(400)
				return
			}
			b.listPages = append(b.listPages, page)
			slow(r, b.listDelay)
			records := []map[string]any{}
			if r.URL.Query().Get("format") != "pdf" {
				records = listed(b.books, r.URL.Query().Get("updated_after"))
			}
			start := (page - 1) * limit
			end := min(start+limit, len(records))
			if start > len(records) {
				start = len(records)
			}
			total := len(records)
			if override, ok := b.totalByPage[page]; ok {
				total = override
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"books": records[start:end], "pagination": map[string]any{"page": page, "limit": limit, "total_items": total, "total_pages": (total + limit - 1) / limit}})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/books/")
		download := strings.HasSuffix(id, "/download")
		if !download {
			b.lookups++
		}
		id = strings.TrimSuffix(id, "/download")
		for _, book := range b.books {
			if book["id"] == id {
				if download {
					if b.denyDownload {
						w.WriteHeader(403)
						return
					}
					if r.Header.Get("Range") == "bytes=0-0" {
						b.probes++
						w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(b.body)))
						w.WriteHeader(http.StatusPartialContent)
						_, _ = w.Write(b.body[:1])
						return
					}
					b.downloads++
					if b.corrupt {
						_, _ = w.Write(bytes.Repeat([]byte("x"), len(b.body)))
					} else {
						_, _ = w.Write(b.body)
					}
				} else {
					_ = json.NewEncoder(w).Encode(book)
				}
				return
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return b, srv
}
func configured(t *testing.T, url string) *Provider {
	t.Helper()
	p := New()
	t.Cleanup(func() { _ = p.Close() })
	cfg, _ := structpb.NewStruct(map[string]any{"base_url": url, "api_key": "secret", "allow_insecure_http": true})
	if err := p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}}); err != nil {
		t.Fatal(err)
	}
	return p
}
func revision(b *backend) string {
	h := sha256.Sum256(b.body)
	return "sha256:" + hex.EncodeToString(h[:])
}

type capture struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*storagev1.ReadChunk
}

func (s *capture) Context() context.Context          { return s.ctx }
func (s *capture) Send(c *storagev1.ReadChunk) error { s.chunks = append(s.chunks, c); return nil }

func TestUnconfiguredAndInvalidConfiguration(t *testing.T) {
	p := New()
	defer p.Close()
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Describe(context.Background(), &storagev1.DescribeRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unconfigured %v", err)
	}
	cfg, _ := structpb.NewStruct(map[string]any{"base_url": "http://example.test", "api_key": "secret"})
	if err := p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}}); err != nil {
		t.Fatal("incomplete/invalid settings must be stored", err)
	}
	if _, err := p.Describe(context.Background(), &storagev1.DescribeRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("insecure config: %v", err)
	}
}
func TestStorageDescriptionAndNamespace(t *testing.T) {
	_, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	ctx := context.Background()
	d, err := p.Describe(ctx, &storagev1.DescribeRequest{})
	if err != nil || d.GetRevision() != 1 || len(d.GetSources()) != 1 || !d.Sources[0].RevisionPinnedReads {
		t.Fatalf("%v %v", d, err)
	}
	first, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "root", MaxEntries: 1})
	if err != nil || first.Complete || len(first.Entries) != 1 || first.Entries[0].Id != "epub" || first.NextCursor == "" {
		t.Fatalf("%v %v", first, err)
	}
	last, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "root", MaxEntries: 1, Cursor: first.NextCursor})
	if err != nil || !last.Complete || len(last.Entries) != 1 || last.Entries[0].Id != "pdf" {
		t.Fatalf("%v %v", last, err)
	}
	empty, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "pdf"})
	if err != nil || !empty.Complete || len(empty.Entries) != 0 {
		t.Fatalf("%v %v", empty, err)
	}
}
func TestPinnedVerifiedRangeAndAuthorization(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	ctx := context.Background()
	req := &storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: revision(b), Offset: 11, Length: 170000}
	out := &capture{ctx: ctx}
	if err := p.Read(req, out); err != nil {
		t.Fatal(err)
	}
	var got []byte
	off := int64(11)
	for i, c := range out.chunks {
		if c.Offset != off || len(c.Data) > 128<<10 || c.Eof != (i == len(out.chunks)-1) {
			t.Fatalf("bad chunk %v", c)
		}
		got = append(got, c.Data...)
		off += int64(len(c.Data))
	}
	if !bytes.Equal(got, b.body[11:170011]) {
		t.Fatal("wrong verified bytes")
	}
	again := &capture{ctx: ctx}
	if err := p.Read(req, again); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	downloads, probes := b.downloads, b.probes
	b.deny = true
	b.mu.Unlock()
	// The cache hit re-checks download access with a one-byte range only.
	if downloads != 1 || probes != 1 {
		t.Fatalf("downloaded %d times, probed %d times", downloads, probes)
	}
	denied := &capture{ctx: ctx}
	if err := p.Read(req, denied); status.Code(err) != codes.PermissionDenied || len(denied.chunks) != 0 {
		t.Fatalf("cache bypassed authorization: %v", err)
	}
}
func TestCorruptDownloadEmitsNoChunks(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	b.mu.Lock()
	b.corrupt = true
	b.mu.Unlock()
	out := &capture{ctx: context.Background()}
	err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: revision(b), Length: 5}, out)
	if status.Code(err) != codes.FailedPrecondition || len(out.chunks) != 0 {
		t.Fatalf("corrupt bytes exposed: %d %v", len(out.chunks), err)
	}
}
func TestStatRevisionIdentityAndReadRanges(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	ctx := context.Background()
	_, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: "sha256:" + strings.Repeat("0", 64)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	_, err = p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "epub/../../secret", ExpectedRevision: revision(b)})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	for _, r := range []*storagev1.ReadRequest{
		{Offset: -1, Length: 1}, {Offset: 0, Length: -1}, {Offset: 0, Length: 8<<20 + 1}, {Offset: int64(len(b.body)), Length: 1}, {Offset: 1 << 62, Length: 1 << 62},
	} {
		r.SourceId = "bookwarehouse"
		r.EntryId = "epub/book-000"
		r.ExpectedRevision = revision(b)
		out := &capture{ctx: ctx}
		if err := p.Read(r, out); status.Code(err) != codes.InvalidArgument || len(out.chunks) != 0 {
			t.Fatalf("range admitted %v %v", r, err)
		}
	}
	zero := &capture{ctx: ctx}
	err = p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: revision(b), Offset: int64(len(b.body))}, zero)
	if err != nil || len(zero.chunks) != 1 || !zero.chunks[0].Eof || len(zero.chunks[0].Data) != 0 {
		t.Fatalf("empty %v %v", zero.chunks, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Describe(ctx, &storagev1.DescribeRequest{}); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
}
func TestPaginationRevalidatesBoundariesAndCursor(t *testing.T) {
	b, srv := fixture(t, 101)
	p := configured(t, srv.URL)
	ctx := context.Background()
	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 1}
	first, err := p.List(ctx, req)
	if err != nil || len(first.Entries) != 1 || first.Complete {
		t.Fatalf("%v %v", first, err)
	}
	req.Cursor = first.NextCursor
	second, err := p.List(ctx, req)
	if err != nil || second.Entries[0].Id != "epub/book-001" {
		t.Fatalf("%v %v", second, err)
	}
	req.Cursor = first.NextCursor + "x"
	if _, err = p.List(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("tampered cursor %v", err)
	}
	req.Cursor = first.NextCursor
	req.DirectoryId = "pdf"
	if _, err = p.List(ctx, req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cross-dir cursor %v", err)
	}
	req.DirectoryId = "epub"
	req.Cursor = ""
	req.MaxEntries = 100
	first, err = p.List(ctx, req)
	if err != nil || len(first.Entries) != 100 {
		t.Fatalf("%v %v", first, err)
	}
	req.Cursor = first.NextCursor
	last, err := p.List(ctx, req)
	if err != nil || !last.Complete || len(last.Entries) != 1 {
		t.Fatalf("%v %v", last, err)
	}
	b.mu.Lock()
	b.books[0]["title"] = "changed"
	b.mu.Unlock()
	// A change behind the cursor no longer aborts the listing.
	if replay, err := p.List(ctx, req); err != nil || !replay.Complete || len(replay.Entries) != 1 {
		t.Fatalf("changed preceding page rejected %v %v", replay, err)
	}
}
func TestConfigurationReplacementInvalidatesOldCursorAndCache(t *testing.T) {
	b, srv := fixture(t, 2)
	p := configured(t, srv.URL)
	ctx := context.Background()
	first, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := structpb.NewStruct(map[string]any{"base_url": srv.URL, "api_key": "different", "allow_insecure_http": true})
	if err = p.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}}); err != nil {
		t.Fatal(err)
	}
	_, err = p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", Cursor: first.NextCursor})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("foreign cursor admitted %v", err)
	}
	b.mu.Lock()
	b.deny = true
	b.mu.Unlock()
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Describe(ctx, &storagev1.DescribeRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
}

func TestCachedReadRechecksDownloadPermission(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	ctx := context.Background()
	req := &storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: revision(b), Length: 5}
	if err := p.Read(req, &capture{ctx: ctx}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.denyDownload = true
	b.mu.Unlock()
	out := &capture{ctx: ctx}
	if err := p.Read(req, out); status.Code(err) != codes.PermissionDenied || len(out.chunks) != 0 {
		t.Fatalf("download permission bypassed via cache: %v, chunks=%d", err, len(out.chunks))
	}
}

func TestReconfigureCancelsUnverifiedDownload(t *testing.T) {
	body := []byte("unpublished ebook bytes")
	hash := sha256.Sum256(body)
	started := make(chan struct{})
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			close(stopped)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "book-000", "file_format": "epub", "file_hash": hex.EncodeToString(hash[:]), "file_size": len(body), "updated_at": "2026-10-07T12:00:00Z"})
	}))
	t.Cleanup(srv.Close)
	p := configured(t, srv.URL)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out := &capture{ctx: ctx}
	result := make(chan error, 1)
	go func() {
		result <- p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: "sha256:" + hex.EncodeToString(hash[:]), Length: 5}, out)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("download did not start")
	}
	if err := p.Configure(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if status.Code(err) != codes.Canceled || len(out.chunks) != 0 {
			t.Fatalf("replaced read: %v chunks %d", err, len(out.chunks))
		}
	case <-ctx.Done():
		t.Fatal("old download did not stop")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("backend connection leaked")
	}
}

func TestSlowDownloadContinuesAfterTheReadDeadline(t *testing.T) {
	body := bytes.Repeat([]byte("slow book "), 20000)
	hash := sha256.Sum256(body)
	release := make(chan struct{})
	var mu sync.Mutex
	var fullRequests int
	completed := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/download") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "book-000", "file_format": "epub", "file_hash": hex.EncodeToString(hash[:]), "file_size": len(body), "updated_at": "2026-10-07T12:00:00Z"})
			return
		}
		mu.Lock()
		fullRequests++
		first := fullRequests == 1
		mu.Unlock()
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if !first {
			_, _ = w.Write(body)
			return
		}
		// The first transfer outlasts the caller's deadline.
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		<-release
		_, err := w.Write(body[len(body)/2:])
		w.(http.Flusher).Flush()
		completed <- err == nil && r.Context().Err() == nil
	}))
	t.Cleanup(srv.Close)
	p := configured(t, srv.URL)
	req := &storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "epub/book-000", ExpectedRevision: "sha256:" + hex.EncodeToString(hash[:]), Offset: 7, Length: 1000}
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	early := &capture{ctx: ctx}
	if err := p.Read(req, early); status.Code(err) != codes.Unavailable || len(early.chunks) != 0 {
		t.Fatalf("read before the download finished: %v, %d chunks", err, len(early.chunks))
	}
	if ctx.Err() != nil {
		t.Fatal("the read did not answer before its deadline")
	}
	close(release)
	select {
	case ok := <-completed:
		if !ok {
			t.Fatal("the download was cut off with its first caller")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the download did not finish")
	}
	later := &capture{ctx: t.Context()}
	if err := p.Read(req, later); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for _, c := range later.chunks {
		got = append(got, c.Data...)
	}
	if !bytes.Equal(got, body[7:1007]) {
		t.Fatal("wrong verified bytes")
	}
}
