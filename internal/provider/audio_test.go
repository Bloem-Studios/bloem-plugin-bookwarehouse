package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

type audioBackend struct {
	mu                  sync.Mutex
	books               []map[string]any
	bodies              map[string][]byte
	streams, probes     int
	ranges              []string
	details             int
	deletions           []map[string]any
	denyStream, missing bool
	cover               []byte
	deny, denyCover     bool
	collections         map[string][]collectionRow
	listCalls, lookups  map[string]int
	omitCollection      string
	emptyCollections    bool
	listDelay           time.Duration
}

type collectionRow struct{ id, name string }

func (b *audioBackend) streamCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.streams }
func (b *audioBackend) probeCount() int  { b.mu.Lock(); defer b.mu.Unlock(); return b.probes }
func (b *audioBackend) rangeLog() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ranges)
}
func (b *audioBackend) calls(kind string) (lists, lookups int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listCalls[kind], b.lookups[kind]
}

// collectionRows serves one row per spelling in Bookwarehouse's order: the
// exact name bytes, ascending.
func (b *audioBackend) collectionRows(kind string) []collectionRow {
	var rows []collectionRow
	for _, row := range b.collections[kind] {
		if !b.emptyCollections && row.id != b.omitCollection {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(x, y collectionRow) int { return strings.Compare(x.name, y.name) })
	return rows
}

// spellingFixture puts one spelling of id before 150 other rows and another
// after them, on a different provider page.
func spellingFixture(id, first, last, filler string) []collectionRow {
	rows := []collectionRow{{id, first}, {id, last}}
	for i := range 150 {
		rows = append(rows, collectionRow{fmt.Sprintf("%s-%03d", strings.ToLower(filler), i), fmt.Sprintf("%s %03d", filler, i)})
	}
	return rows
}

func audioFixture(t *testing.T) (*audioBackend, *httptest.Server) {
	t.Helper()
	b := &audioBackend{bodies: map[string][]byte{"41": []byte("hello"), "42": []byte("world"), "43": []byte("single")}, listCalls: map[string]int{}, lookups: map[string]int{}}
	b.collections = map[string][]collectionRow{
		"authors": spellingFixture("m-r-forbes", "M. R. Forbes", "m.r. forbes", "Name"),
		"series":  spellingFixture("star-crossed", "Star Crossed", "star-crossed", "Series"),
	}
	file := func(id, disc, track int, name string) map[string]any {
		return map[string]any{"id": id, "audiobook_id": "book-a", "file_path": "private/" + name, "file_size": len(b.bodies[strconv.Itoa(id)]), "file_hash": strings.Repeat("a", 64), "disc_num": disc, "track_num": track, "duration_seconds": 2, "codec": "mp3"}
	}
	b.books = []map[string]any{
		{"id": "book-a", "title": "Book A", "author": "M.R. Forbes", "series_name": "Star-Crossed", "updated_at": "2026-10-10T12:00:00Z", "files": []map[string]any{file(42, 1, 2, "second.mp3"), file(41, 1, 1, "first.mp3")}, "chapters": []map[string]any{{"id": 1, "audiobook_id": "book-a", "start_seconds": 0, "end_seconds": 1, "is_accurate": true}}},
		{"id": "book-b", "title": "Book B", "author": "M. R. Forbes", "series_name": "Star Crossed", "updated_at": "2026-10-10T12:00:01Z", "files": []map[string]any{{"id": 43, "audiobook_id": "book-b", "file_path": "private/single.m4b", "file_size": 6, "disc_num": 1, "track_num": 1}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if r.Header.Get("X-API-Key") != "secret" || b.deny {
			w.WriteHeader(403)
			return
		}
		endpoint := strings.TrimPrefix(r.URL.Path, "/api/v1/audiobooks")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if kind, id, ok := strings.Cut(strings.TrimPrefix(endpoint, "/"), "/"); kind == "authors" || kind == "series" {
			rows := b.collectionRows(kind)
			if !ok {
				b.listCalls[kind]++
				start := min((page-1)*limit, len(rows))
				end := min(start+limit, len(rows))
				out := []map[string]any{}
				for _, row := range rows[start:end] {
					out = append(out, map[string]any{"id": row.id, "name": row.name})
				}
				json.NewEncoder(w).Encode(map[string]any{kind: out, "page": page, "limit": limit, "total": len(rows)})
				return
			}
			b.lookups[kind]++
			var names []string
			for _, row := range rows {
				if row.id == id {
					names = append(names, row.name)
				}
			}
			if len(names) == 0 {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "name": names[0], "names": names})
			return
		}
		if endpoint == "/deletions" {
			rows := b.deletions
			if rows == nil {
				rows = []map[string]any{}
			}
			json.NewEncoder(w).Encode(map[string]any{"deletions": rows, "pagination": map[string]any{"page": page, "limit": limit}})
			return
		}
		if endpoint == "" {
			slow(r, b.listDelay)
			rows := listed(b.books, r.URL.Query().Get("updated_after"))
			if r.URL.Query().Get("author_id") != "" && r.URL.Query().Get("author_id") != "m-r-forbes" {
				rows = nil
			}
			if r.URL.Query().Get("series_id") != "" && r.URL.Query().Get("series_id") != "star-crossed" {
				rows = nil
			}
			start := min((page-1)*limit, len(rows))
			end := min(start+limit, len(rows))
			json.NewEncoder(w).Encode(map[string]any{"audiobooks": rows[start:end], "page": page, "limit": limit, "total": len(rows)})
			return
		}
		if endpoint == "/book-a/cover" {
			if b.denyCover {
				w.WriteHeader(403)
				return
			}
			if b.cover == nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(b.cover)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(endpoint, "/"), "/stream")
		for _, book := range b.books {
			if book["id"] == id {
				if strings.HasSuffix(endpoint, "/stream") {
					if b.denyStream {
						w.WriteHeader(403)
						return
					}
					if b.missing {
						w.WriteHeader(404)
						return
					}
					body := b.bodies[r.URL.Query().Get("file_id")]
					if body == nil {
						w.WriteHeader(404)
						return
					}
					if r.Header.Get("Range") == "bytes=0-0" {
						b.probes++
						w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(body)))
						w.WriteHeader(206)
						w.Write(body[:1])
						return
					}
					var first, last int
					if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); err == nil {
						b.ranges = append(b.ranges, r.Header.Get("Range"))
						if first >= len(body) {
							w.WriteHeader(416)
							return
						}
						last = min(last, len(body)-1)
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(body)))
						w.Header().Set("Content-Length", strconv.Itoa(last-first+1))
						w.WriteHeader(206)
						w.Write(body[first : last+1])
						return
					}
					b.streams++
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					w.Write(body)
					return
				}
				b.details++
				json.NewEncoder(w).Encode(book)
				return
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return b, srv
}
func configuredAudio(t *testing.T, url string, extra map[string]any) *Provider {
	t.Helper()
	p := New()
	t.Cleanup(func() { p.Close() })
	m := map[string]any{"base_url": url, "api_key": "secret", "allow_insecure_http": true, "content": "audiobooks"}
	for k, v := range extra {
		m[k] = v
	}
	cfg, _ := structpb.NewStruct(m)
	if err := p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAudioBrowseNeverHydratesAndBindsCursorIntent(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	d, err := p.Describe(ctx, &storagev1.DescribeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sources) != 1 || d.Sources[0].Id != "bookwarehouse-audiobooks" || !d.Sources[0].DirectoryOnlyListing || len(d.Sources[0].MediaKinds) != 1 || d.Sources[0].MediaKinds[0] != "audiobooks" {
		t.Fatalf("source: %v", d)
	}
	if d.Sources[0].RootEntryId != "audio-root" {
		t.Fatalf("audio root: %v", d)
	}
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: "root", ExpectedRevision: directoryRevision}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("accepted ebook root: %v", err)
	}
	ebook := configured(t, srv.URL)
	if _, err := ebook.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "audio-root", ExpectedRevision: audioDirectoryRevision}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("accepted audio root: %v", err)
	}
	page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true, MaxEntries: 1})
	if err != nil || page.Complete || len(page.Entries) != 1 || page.Entries[0].Id != "audiobooks/book-a" {
		t.Fatalf("browse: %v %v", page, err)
	}
	if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", Cursor: page.NextCursor}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("intent reused: %v", err)
	}
	leaf, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a", DirectoriesOnly: true})
	if err != nil || !leaf.Complete || len(leaf.Entries) != 0 || leaf.ChangeToken != "" {
		t.Fatalf("leaf: %v %v", leaf, err)
	}
	if b.streamCount() != 0 || b.probeCount() != 0 {
		t.Fatal("browsing opened audio")
	}
}

// listDirectory follows a directory listing's continuations to the end.
func listDirectory(t *testing.T, p *Provider, directory, cursor string, max uint32) []*storagev1.Entry {
	t.Helper()
	var entries []*storagev1.Entry
	for range 10000 {
		page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: directory, DirectoriesOnly: true, MaxEntries: max, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, page.Entries...)
		if page.Complete {
			return entries
		}
		cursor = page.NextCursor
	}
	t.Fatal("listing did not complete")
	return nil
}

func TestAudioCollectionSpellingsListAsSeparateDirectories(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	entries := listDirectory(t, p, "authors", "", 7)
	seen := map[string]bool{}
	var forbes []*storagev1.Entry
	for _, e := range entries {
		if seen[e.Id] {
			t.Fatalf("duplicate directory ID %s", e.Id)
		}
		seen[e.Id] = true
		if strings.HasPrefix(e.Id, "authors/bS1yLWZvcmJlcw.") {
			forbes = append(forbes, e)
		}
	}
	if len(entries) != 152 || len(forbes) != 2 || forbes[0].Name != "M. R. Forbes" || forbes[1].Name != "m.r. forbes" {
		t.Fatalf("spellings: %d entries, M. R. Forbes as %v", len(entries), forbes)
	}
	for _, e := range forbes {
		books, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: e.Id, DirectoriesOnly: true})
		if err != nil || len(books.Entries) != 2 || books.Entries[1].Id != "audiobooks/book-b" {
			t.Fatalf("books of %s: %v %v", e.Name, books, err)
		}
	}
	if b.streamCount() != 0 {
		t.Fatal("collection browsing hydrated")
	}
}

func TestAudioCollectionsPageLazilyPastTheOldCap(t *testing.T) {
	b, srv := audioFixture(t)
	b.mu.Lock()
	b.collections["authors"] = nil
	for i := range 25000 {
		b.collections["authors"] = append(b.collections["authors"], collectionRow{fmt.Sprintf("author-%05d", i), fmt.Sprintf("Author %05d", i)})
	}
	b.mu.Unlock()
	p := configuredAudio(t, srv.URL, nil)
	first, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "authors", DirectoriesOnly: true, MaxEntries: 100})
	if err != nil || first.Complete || len(first.Entries) != 100 || first.Entries[99].Name != "Author 00099" {
		t.Fatalf("first page: %v %v", first, err)
	}
	if lists, _ := b.calls("authors"); lists != 1 {
		t.Fatalf("first page read %d provider pages", lists)
	}
	entries := listDirectory(t, p, "authors", "", 512)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Id] = true
	}
	if len(entries) != 25000 || len(seen) != 25000 {
		t.Fatalf("listed %d entries, %d unique", len(entries), len(seen))
	}
	// 250 provider pages, each read once plus at most one reread per reply.
	if lists, _ := b.calls("authors"); lists > 1+250+50 {
		t.Fatalf("full listing read %d provider pages", lists)
	}
}

func TestAudioCollectionContinuationResumesAfterTheLastSpelling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func([]collectionRow) []collectionRow
	}{
		{"rows inserted before the cursor", func(rows []collectionRow) []collectionRow {
			for i := range 250 {
				rows = append(rows, collectionRow{fmt.Sprintf("aaa-%03d", i), fmt.Sprintf("Aaa %03d", i)})
			}
			return rows
		}},
		{"rows removed before the cursor", func(rows []collectionRow) []collectionRow { return rows[250:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, srv := audioFixture(t)
			b.mu.Lock()
			b.collections["authors"] = nil
			for i := range 1000 {
				b.collections["authors"] = append(b.collections["authors"], collectionRow{fmt.Sprintf("author-%05d", i), fmt.Sprintf("Author %05d", i)})
			}
			b.mu.Unlock()
			p := configuredAudio(t, srv.URL, nil)
			first, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "authors", DirectoriesOnly: true, MaxEntries: 300})
			if err != nil || len(first.Entries) != 300 || first.Entries[299].Name != "Author 00299" {
				t.Fatalf("first page: %d entries, %v", len(first.GetEntries()), err)
			}
			b.mu.Lock()
			b.collections["authors"] = tc.change(b.collections["authors"])
			b.mu.Unlock()
			rest := listDirectory(t, p, "authors", first.NextCursor, 64)
			if len(rest) != 700 {
				t.Fatalf("continued with %d entries", len(rest))
			}
			for i, e := range rest {
				if want := fmt.Sprintf("Author %05d", 300+i); e.Name != want {
					t.Fatalf("entry %d: %s, want %s", i, e.Name, want)
				}
			}
		})
	}
}

func TestAudioCollectionDirectoryLooksUpOnlyItsCollection(t *testing.T) {
	for _, tc := range []struct{ kind, id, spelling string }{
		{"authors", "m-r-forbes", "m.r. forbes"},
		{"series", "star-crossed", "star-crossed"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			b, srv := audioFixture(t)
			p := configuredAudio(t, srv.URL, nil)
			ctx := context.Background()
			directory := collectionEntryID(tc.kind, tc.id, tc.spelling)
			got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: directory, ExpectedRevision: audioDirectoryRevision})
			if err != nil || got.GetEntry().GetName() != tc.spelling || got.GetEntry().GetId() != directory {
				t.Fatalf("Stat: %v %v", got, err)
			}
			books, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: directory, DirectoriesOnly: true})
			if err != nil || len(books.Entries) != 2 {
				t.Fatalf("List: %v %v", books, err)
			}
			if lists, lookups := b.calls(tc.kind); lists != 0 || lookups != 2 {
				t.Fatalf("read %d collection pages and %d lookups", lists, lookups)
			}
			// A spelling the collection no longer has is absent, though its books remain.
			gone := collectionEntryID(tc.kind, tc.id, "Renamed")
			if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: gone, ExpectedRevision: audioDirectoryRevision}); status.Code(err) != codes.NotFound || got != nil {
				t.Fatalf("removed spelling Stat: %v %v", got, err)
			}
			if got, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: gone, DirectoriesOnly: true}); status.Code(err) != codes.NotFound || got != nil {
				t.Fatalf("removed spelling List: %v %v", got, err)
			}
			key, tag, _ := strings.Cut(strings.TrimPrefix(directory, tc.kind+"/"), ".")
			for _, bad := range []string{tc.kind + "/" + key, tc.kind + "/" + key + ".", directory + "0", tc.kind + "/" + key + "." + strings.ToUpper(tag), tc.kind + "/" + key + "=." + tag} {
				if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: bad, ExpectedRevision: audioDirectoryRevision}); status.Code(err) != codes.InvalidArgument || got != nil {
					t.Fatalf("malformed %q: %v %v", bad, got, err)
				}
			}
		})
	}
}

// editFile changes book-a's track 41 in place and returns a restore function.
func editFile(b *audioBackend, change func(map[string]any)) (restore func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.books[0]["files"].([]map[string]any)[1]
	saved := maps.Clone(f)
	change(f)
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		clear(f)
		maps.Copy(f, saved)
	}
}

func TestAudioListingNamesTracksByEvidenceWithoutOpeningThem(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	req := &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"}
	page, err := p.List(ctx, req)
	if err != nil || !page.Complete || len(page.Entries) != 2 || page.Entries[0].Id != "audio/book-a/41" || page.Entries[1].Id != "audio/book-a/42" {
		t.Fatalf("tracks: %v %v", page, err)
	}
	first := page.Entries[0]
	if !validAudioRevision(first.Revision) || first.Revision == page.Entries[1].Revision || first.Size != 5 {
		t.Fatalf("revisions: %v", page.Entries)
	}
	if len(first.AudioFile.Chapters) != 0 {
		t.Fatal("guessed chapter association")
	}
	// A file missing from storage is found by reading it, not by listing.
	b.mu.Lock()
	b.missing = true
	b.mu.Unlock()
	if _, err := p.List(ctx, req); err != nil {
		t.Fatal(err)
	}
	if b.streamCount() != 0 || b.probeCount() != 0 || len(b.rangeLog()) != 0 {
		t.Fatalf("listing opened audio: streams=%d probes=%d ranges=%v", b.streamCount(), b.probeCount(), b.rangeLog())
	}
	// The revision names Bookwarehouse's description of the bytes: new bytes
	// under the same size and sampled hash keep it; a new hash or size does not.
	b.mu.Lock()
	b.bodies["41"] = []byte("hallo")
	b.mu.Unlock()
	if again, err := p.List(ctx, req); err != nil || again.Entries[0].Revision != first.Revision {
		t.Fatalf("unchanged evidence: %v %v", again, err)
	}
	for name, change := range map[string]func(map[string]any){
		"sampled hash": func(f map[string]any) { f["file_hash"] = strings.Repeat("b", 64) },
		"size":         func(f map[string]any) { f["file_size"] = 6 },
	} {
		restore := editFile(b, change)
		changed, err := p.List(ctx, req)
		restore()
		if err != nil || changed.Entries[0].Revision == first.Revision {
			t.Errorf("%s kept the revision: %v %v", name, changed, err)
		}
	}
}

func TestAudioReadStreamsTheRequestedRange(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"})
	if err != nil {
		t.Fatal(err)
	}
	e := page.Entries[0]
	read := func(offset, length int64) (string, []*storagev1.ReadChunk, error) {
		out := &capture{ctx: ctx}
		err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision, Offset: offset, Length: length}, out)
		var data []byte
		for _, c := range out.chunks {
			data = append(data, c.Data...)
		}
		return string(data), out.chunks, err
	}
	data, chunks, err := read(1, 3)
	if err != nil || data != "ell" || chunks[0].Offset != 1 || !chunks[len(chunks)-1].Eof {
		t.Fatalf("range read: %q %v %v", data, chunks, err)
	}
	if data, _, err := read(0, 5); err != nil || data != "hello" {
		t.Fatalf("whole read: %q %v", data, err)
	}
	if got := b.rangeLog(); !slices.Equal(got, []string{"bytes=1-3", "bytes=0-4"}) {
		t.Fatalf("requested %v", got)
	}
	if _, chunks, err := read(5, 0); err != nil || len(chunks) != 1 || !chunks[0].Eof || len(b.rangeLog()) != 2 {
		t.Fatalf("empty read: %v %v", chunks, err)
	}
	// Stat checks the same evidence without opening the track.
	if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision}); err != nil || got.GetEntry().GetRevision() != e.Revision || len(b.rangeLog()) != 2 {
		t.Fatalf("Stat: %v %v", got, err)
	}
	// Once the reused description expires, a revision Bookwarehouse no longer
	// describes is refused before any bytes are requested.
	restore := editFile(b, func(f map[string]any) { f["file_hash"] = strings.Repeat("b", 64) })
	expireAudioBooks(p)
	if _, _, err := read(0, 5); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale revision read: %v", err)
	}
	if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision}); status.Code(err) != codes.FailedPrecondition || got != nil {
		t.Fatalf("stale revision Stat: %v %v", got, err)
	}
	restore()
	expireAudioBooks(p)
	if len(b.rangeLog()) != 2 {
		t.Fatalf("stale revision requested bytes: %v", b.rangeLog())
	}
	// Bookwarehouse's answer to the range is the current authority and size.
	for name, tc := range map[string]struct {
		set  func()
		want codes.Code
	}{
		"revoked stream": {func() { b.denyStream = true }, codes.PermissionDenied},
		"missing file":   {func() { b.missing = true }, codes.NotFound},
		"resized bytes":  {func() { b.bodies["41"] = []byte("hello!") }, codes.FailedPrecondition},
	} {
		b.mu.Lock()
		tc.set()
		b.mu.Unlock()
		if _, chunks, err := read(0, 5); status.Code(err) != tc.want || len(chunks) != 0 {
			t.Errorf("%s: %d chunks, %v", name, len(chunks), err)
		}
		b.mu.Lock()
		b.denyStream, b.missing, b.bodies["41"] = false, false, []byte("hello")
		b.mu.Unlock()
	}
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: "sha256:" + strings.Repeat("0", 64)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("digest revision from an older release: %v", err)
	}
}

// expireAudioBooks ages every reused book description past its lifetime.
func expireAudioBooks(p *Provider) {
	books := &p.current.books
	books.mu.Lock()
	defer books.mu.Unlock()
	for id, cached := range books.books {
		cached.at = time.Now().Add(-audioBookTTL)
		books.books[id] = cached
	}
}

func TestAudioReadsReuseTheListedBookDescription(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"})
	if err != nil {
		t.Fatal(err)
	}
	e := page.Entries[0]
	read := func() error {
		return p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision, Offset: 1, Length: 2}, &capture{ctx: ctx})
	}
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := read(); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	details := b.details
	b.mu.Unlock()
	if details != 1 || len(b.rangeLog()) != 3 {
		t.Fatalf("listing, Stat and 3 reads fetched the book %d times and %d ranges", details, len(b.rangeLog()))
	}
	// A listing always reads the current description.
	if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"}); err != nil {
		t.Fatal(err)
	}
	expireAudioBooks(p)
	if err := read(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	details = b.details
	b.mu.Unlock()
	if details != 3 {
		t.Fatalf("relisting and an expired read fetched %d times in all, want 3", details)
	}
}

func TestAudioSourceBoundaryStaysFixed(t *testing.T) {
	_, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	if _, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := structpb.NewStruct(map[string]any{"base_url": srv.URL, "api_key": "secret", "allow_insecure_http": true, "source_id": "bookwarehouse-audiobooks", "content": "ebooks"})
	if err := p.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "connection", Value: cfg}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("source boundary silently changed: %v", err)
	}
}

func TestEbookLegacyConfigurationTokensStillContinue(t *testing.T) {
	_, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	// Exact pre-audio Config serialization, independent of current Config.
	old := fmt.Sprintf(`{"base_url":%q,"api_key":"secret","allow_insecure_http":true,"source_id":"bookwarehouse","source_name":"Bookwarehouse","max_book_bytes":134217728,"max_cache_bytes":536870912}`, srv.URL)
	legacy := &state{key: sha256.Sum256([]byte(old))}
	token := encodeCursor(legacy, cursor{Version: 3, Directory: "root", Page: 1, Offset: 1})
	page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "root", Cursor: token})
	if err != nil || !page.GetComplete() || len(page.GetEntries()) != 1 || page.Entries[0].Id != "pdf" {
		t.Fatalf("legacy cursor invalidated: %v %v", page, err)
	}
}

// The audiobook directory listing pages the backend like the ebook one, so a
// slow Bookwarehouse must not cost it the audiobooks it already fetched.
func TestAudioListingReturnsFetchedBooksBeforeTheDeadline(t *testing.T) {
	b, srv := audioFixture(t)
	b.books = nil
	want := make([]string, 600)
	for i := range want {
		id := fmt.Sprintf("book-%03d", i)
		b.books = append(b.books, map[string]any{"id": id, "title": "Title " + id, "updated_at": listingBase.Add(time.Duration(i) * time.Second).Format(time.RFC3339), "files": []map[string]any{}})
		want[i] = "audiobooks/" + id
	}
	b.listDelay = 200 * time.Millisecond
	p := configuredAudio(t, srv.URL, nil)
	req := &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true, MaxEntries: 512}
	got, short := listWithin(t, p, req, time.Second)
	sameIDs(t, got, want)
	if short == 0 {
		t.Fatal("no page stopped early for the deadline")
	}
}

func TestAudioTrackPagesFollowMaxEntries(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	req := &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a", MaxEntries: 1}
	first, err := p.List(ctx, req)
	if err != nil || first.GetComplete() || len(first.GetEntries()) != 1 {
		t.Fatalf("first track page: %v %v", first, err)
	}
	req.Cursor = first.NextCursor
	second, err := p.List(ctx, req)
	if err != nil || !second.GetComplete() || len(second.GetEntries()) != 1 || second.Entries[0].Id != "audio/book-a/42" {
		t.Fatalf("track page did not advance: %v %v", second, err)
	}
	// A book whose tracks change between pages restarts its listing.
	defer editFile(b, func(f map[string]any) { f["file_size"] = 6 })()
	if got, err := p.List(ctx, req); status.Code(err) != codes.FailedPrecondition || got != nil {
		t.Fatalf("continued across changed tracks: %v %v", got, err)
	}
	if b.streamCount() != 0 || len(b.rangeLog()) != 0 {
		t.Fatal("track listing opened audio")
	}
}

func TestAudioInvalidReadRangeDoesNotOpenTrack(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"})
	if err != nil {
		t.Fatal(err)
	}
	e := page.Entries[0]
	for _, r := range [][2]int64{{6, 0}, {2, 4}} {
		out := &capture{ctx: context.Background()}
		err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse-audiobooks", EntryId: e.Id, ExpectedRevision: e.Revision, Offset: r[0], Length: r[1]}, out)
		if status.Code(err) != codes.InvalidArgument || len(out.chunks) != 0 {
			t.Fatalf("range %v: %v", r, err)
		}
	}
	if b.streamCount() != 0 || len(b.rangeLog()) != 0 {
		t.Fatalf("invalid range opened audio: %v", b.rangeLog())
	}
}

func TestAudioCoverRetainsVirtualIdentityAndCacheIntegration(t *testing.T) {
	b, srv := audioFixture(t)
	b.cover = []byte("synthetic jpeg")
	b.books[0]["has_cover"] = true
	b.books[0]["cover_id"] = "cover-a"
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	meta := page.Entries[0].Audiobook
	if meta.CoverEntryId != "cover/book-a" || !strings.HasPrefix(meta.CoverRevision, "cover:v2:") {
		t.Fatalf("cover identity: %v", meta)
	}
	stat, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: meta.CoverEntryId, ExpectedRevision: meta.CoverRevision})
	if err != nil || stat.GetEntry().GetSize() != 14 {
		t.Fatalf("cover Stat: %v %v", stat, err)
	}
	out := &capture{ctx: ctx}
	if err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse-audiobooks", EntryId: meta.CoverEntryId, ExpectedRevision: meta.CoverRevision, Length: 14}, out); err != nil {
		t.Fatal(err)
	}
	if len(out.chunks) != 1 || string(out.chunks[0].Data) != "synthetic jpeg" || b.streamCount() != 0 {
		t.Fatal("cover read changed or hydrated audio")
	}
}

func TestAudioBrowseMissingTitleKeepsUsableStableDirectory(t *testing.T) {
	b, srv := audioFixture(t)
	b.books[0]["title"] = ""
	p := configuredAudio(t, srv.URL, nil)
	page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true})
	if err != nil || page.Entries[0].Id != "audiobooks/book-a" || page.Entries[0].Name != "book-a" {
		t.Fatalf("directory has no usable display name: %v %v", page, err)
	}
}

func TestAudioCatalogListingIssuesChangeTokens(t *testing.T) {
	b, srv := audioFixture(t)
	p := configuredAudio(t, srv.URL, nil)
	ctx := context.Background()
	list := func(req *storagev1.ListRequest) []*storagev1.ListResponse {
		t.Helper()
		var pages []*storagev1.ListResponse
		for range 20 {
			page, err := p.List(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			pages = append(pages, page)
			if page.Complete {
				return pages
			}
			req.Cursor = page.NextCursor
		}
		t.Fatal("listing did not complete")
		return nil
	}
	full := list(&storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks"})
	token := full[len(full)-1].ChangeToken
	if token == "" || len(full[0].Entries) != 2 {
		t.Fatalf("full listing: %v", full)
	}
	browse := list(&storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true})
	if browse[len(browse)-1].ChangeToken != "" {
		t.Fatal("browse issued a change token")
	}
	// Only the catalog directory is incremental; a book or a collection is listed in full.
	collection := collectionEntryID("authors", "m-r-forbes", "M. R. Forbes")
	for _, dir := range []string{"audiobooks/book-a", collection} {
		pages := list(&storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: dir})
		if pages[len(pages)-1].ChangeToken != "" {
			t.Fatalf("%s issued a change token", dir)
		}
		if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: dir, ChangesSince: token}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s accepted a change token: %v", dir, err)
		}
	}
	// An incremental listing returns the books changed since the token, then deletions.
	b.mu.Lock()
	b.books[1]["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	b.deletions = []map[string]any{{"audiobook_id": "book-gone", "deleted_at": time.Now().UTC().Format(time.RFC3339)}}
	b.mu.Unlock()
	delta := list(&storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", ChangesSince: token})
	var ids, removed []string
	for _, page := range delta {
		for _, e := range page.Entries {
			ids = append(ids, e.Id)
		}
		removed = append(removed, page.RemovedEntryIds...)
	}
	if !slices.Equal(ids, []string{"audiobooks/book-b"}) || !slices.Equal(removed, []string{"audiobooks/book-gone"}) || delta[len(delta)-1].ChangeToken == "" {
		t.Fatalf("incremental listing: entries %v removed %v", ids, removed)
	}
	partial, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", MaxEntries: 1})
	if err != nil || partial.NextCursor == "" {
		t.Fatalf("partial listing: %v %v", partial, err)
	}
	// A token from the earlier change feed, which listed no embedded chapters,
	// is refused so that every book is listed again.
	anchor := time.Now().Add(-time.Minute).Unix()
	earlier := encodeChangeToken(p.current, "audiobooks", anchor, anchor)
	for _, bad := range []string{token + "x", partial.NextCursor, earlier} {
		if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", ChangesSince: bad}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("foreign token: %v", err)
		}
	}
	if b.streamCount() != 0 {
		t.Fatal("catalog listing opened audio")
	}
}

// A track whose chapters alone outgrow a listing page is listed without
// them; the book is not withheld.
func TestAudioTrackWithOversizedChaptersListsWithoutThem(t *testing.T) {
	b, srv := audioFixture(t)
	title := strings.Repeat("t", 4<<10)
	var chapters []map[string]any
	for i := range 1024 {
		chapters = append(chapters, map[string]any{"id": 100 + i, "audiobook_id": "book-a", "file_id": 41, "title": title, "start_seconds": 0, "end_seconds": 1, "is_accurate": true})
	}
	b.mu.Lock()
	b.books[0]["chapters"] = chapters
	b.mu.Unlock()
	p := configuredAudio(t, srv.URL, nil)
	page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a", MaxEntries: 10})
	if err != nil || !page.GetComplete() || len(page.GetEntries()) != 2 {
		t.Fatalf("book withheld: %v %v", page, err)
	}
	for _, e := range page.GetEntries() {
		if len(e.GetAudioFile().GetChapters()) != 0 || e.GetAudioFile().GetDurationSeconds() != 2 {
			t.Fatalf("track %s: %d chapters", e.GetId(), len(e.GetAudioFile().GetChapters()))
		}
	}
}
