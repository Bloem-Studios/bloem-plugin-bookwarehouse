package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const maxCatalogTextForTest = 4 << 10

var listingBase = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// spread gives book i the timestamp base + i/perSecond seconds.
func spread(b *backend, perSecond int) {
	for i, book := range b.books {
		book["updated_at"] = listingBase.Add(time.Duration(i/perSecond) * time.Second).Format(time.RFC3339)
	}
}

// listAll pages through dir, calling between after every page, and fails on
// any error, duplicate entry or cursor that does not advance.
func listAll(t *testing.T, p *Provider, dir string, limit uint32, between func(page int)) []string {
	t.Helper()
	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: dir, MaxEntries: limit}
	var ids []string
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page > 10000 {
			t.Fatal("listing did not terminate")
		}
		out, err := p.List(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if len(out.Entries) > int(limit) {
			t.Fatalf("page %d: %d entries exceed limit %d", page, len(out.Entries), limit)
		}
		for _, e := range out.Entries {
			if seen[e.Id] {
				t.Fatalf("page %d: duplicate entry %s", page, e.Id)
			}
			seen[e.Id] = true
			ids = append(ids, e.Id)
		}
		if out.Complete {
			if out.NextCursor != "" {
				t.Fatal("terminal page retained a cursor")
			}
			return ids
		}
		if out.NextCursor == "" || out.NextCursor == req.Cursor {
			t.Fatalf("page %d: cursor did not advance", page)
		}
		req.Cursor = out.NextCursor
		if between != nil {
			between(page)
		}
	}
}

func expectedIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("epub/book-%03d", i)
	}
	return ids
}

func sameIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("listed %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("entry %d is %s, want %s", i, got[i], want[i])
		}
	}
}

func TestListingVisitsEveryBookOnceInKeyOrder(t *testing.T) {
	// 250 per second puts the whole catalog in one second, exercising paging
	// within a single updated_after filter.
	for _, perSecond := range []int{1, 37, 100, 250} {
		for _, limit := range []uint32{1, 7, 100, 512} {
			t.Run(fmt.Sprintf("per-second-%d-limit-%d", perSecond, limit), func(t *testing.T) {
				b, srv := fixture(t, 250)
				spread(b, perSecond)
				sameIDs(t, listAll(t, configured(t, srv.URL), "epub", limit, nil), expectedIDs(250))
			})
		}
	}
}

func TestListingCompletesOnExactlyFullFinalPage(t *testing.T) {
	b, srv := fixture(t, 100)
	spread(b, 10)
	sameIDs(t, listAll(t, configured(t, srv.URL), "epub", 100, nil), expectedIDs(100))
}

func TestListingIgnoresAdvisoryTotals(t *testing.T) {
	for _, total := range []int{0, 90, 5000} {
		t.Run(fmt.Sprintf("total-%d", total), func(t *testing.T) {
			b, srv := fixture(t, 250)
			spread(b, 30)
			b.totalByPage = map[int]int{1: total, 2: total, 3: total}
			sameIDs(t, listAll(t, configured(t, srv.URL), "epub", 100, nil), expectedIDs(250))
		})
	}
}

func TestListingSurvivesCatalogChangesWithoutDuplicates(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tt := range []struct {
		name   string
		change func(b *backend)
		want   []string
	}{
		{
			// Seen already, then moved past the listing's upper bound.
			name:   "listed book updated",
			change: func(b *backend) { b.books[3]["updated_at"] = future },
			want:   expectedIDs(250),
		},
		{
			// Not yet seen; left for the next scan rather than listed twice.
			name:   "unlisted book updated",
			change: func(b *backend) { b.books[200]["updated_at"] = future },
			want:   append(expectedIDs(200), expectedIDs(250)[201:]...),
		},
		{
			// Shifts every later page offset back by one.
			name:   "listed book deleted",
			change: func(b *backend) { b.books = append(b.books[:2:2], b.books[3:]...) },
			want:   expectedIDs(250),
		},
		{
			// Shifts every later page offset forward by one.
			name: "book inserted behind cursor",
			change: func(b *backend) {
				added := cloneFixtureBook(b.books[0])
				added["id"] = "book-000a"
				b.books = append(b.books, added)
			},
			want: expectedIDs(250),
		},
		{
			name:   "listed book retitled",
			change: func(b *backend) { b.books[0]["title"] = "Changed title" },
			want:   expectedIDs(250),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, srv := fixture(t, 250)
			spread(b, 30)
			got := listAll(t, configured(t, srv.URL), "epub", 50, func(page int) {
				if page == 0 {
					b.mu.Lock()
					tt.change(b)
					b.mu.Unlock()
				}
			})
			sameIDs(t, got, tt.want)
		})
	}
}

func TestListingFiltersByLastSeenSecond(t *testing.T) {
	b, srv := fixture(t, 250)
	spread(b, 100)
	listAll(t, configured(t, srv.URL), "epub", 100, nil)
	b.mu.Lock()
	defer b.mu.Unlock()
	// Seconds 0 and 1 hold 100 books each: the first page is unfiltered, then
	// each new second restarts at page 1 and a full same-second page advances.
	want := []int{1, 1, 2, 1, 2}
	if fmt.Sprint(b.listPages) != fmt.Sprint(want) {
		t.Fatalf("page requests %v, want %v", b.listPages, want)
	}
}

func TestListingRejectsPreviousCursorVersions(t *testing.T) {
	_, srv := fixture(t, 101)
	p := configured(t, srv.URL)
	for _, version := range []int{1, 2} {
		// Authentic MACs under the current connection key: rejection must come
		// from the old cursor schema, not a bad signature or changed connection.
		old := encodeCursor(p.current, cursor{Version: version, Directory: "epub", Page: 1})
		if out, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", Cursor: old}); out != nil || status.Code(err) != codes.InvalidArgument {
			t.Fatalf("v%d cursor admitted: %v %v", version, out, err)
		}
	}
}

func TestListingRejectsMalformedKeysetCursor(t *testing.T) {
	_, srv := fixture(t, 101)
	p := configured(t, srv.URL)
	upper := time.Now().Unix()
	for name, c := range map[string]cursor{
		"no upper bound":         {Page: 1, AfterSec: 10, AfterID: "book-001"},
		"after beyond upper":     {Page: 1, Upper: upper, AfterSec: upper + 1, AfterID: "book-001"},
		"second without id":      {Page: 1, Upper: upper, AfterSec: 10},
		"page without key":       {Page: 2, Upper: upper},
		"invalid id":             {Page: 1, Upper: upper, AfterSec: 10, AfterID: "../x"},
		"offset on book listing": {Page: 1, Upper: upper, Offset: 3},
	} {
		c.Version, c.Directory = cursorVersion, "epub"
		token := encodeCursor(p.current, c)
		if out, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", Cursor: token}); out != nil || status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s admitted: %v %v", name, out, err)
		}
	}
}

func cloneFixtureBook(book map[string]any) map[string]any {
	clone := make(map[string]any, len(book))
	for k, v := range book {
		clone[k] = v
	}
	return clone
}

func TestListingCarriesCatalogMetadata(t *testing.T) {
	b, srv := fixture(t, 1)
	book := b.books[0]
	book["title"] = "Dune"
	book["authors"] = []map[string]any{{"id": 1, "name": "Frank Herbert"}}
	book["genres"] = []map[string]any{{"id": 1, "name": "Science Fiction", "slug": "sf"}}
	book["description"] = "Spice."
	book["isbn13"] = "9780441013593"
	book["series_name"] = "Dune"
	book["series_index"] = 1.0
	book["page_count"] = 412
	book["has_cover"] = true
	book["cover_id"] = "cover-1"
	book["cover_thumbhash"] = "thumb"
	out, err := configured(t, srv.URL).List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	if err != nil || len(out.Entries) != 1 {
		t.Fatalf("%v %v", out, err)
	}
	meta := out.Entries[0].GetEbook()
	if meta.GetTitle() != "Dune" || fmt.Sprint(meta.GetAuthors()) != "[Frank Herbert]" || fmt.Sprint(meta.GetGenres()) != "[Science Fiction]" ||
		meta.GetIsbn() != "9780441013593" || meta.GetSeries() != "Dune" || meta.GetSeriesIndex() != "1" || meta.GetPageCount() != 412 ||
		meta.GetCoverEntryId() != "cover/book-000" || meta.GetCoverRevision() == "" || meta.GetCoverThumbhash() != "thumb" {
		t.Fatalf("metadata = %v", meta)
	}
}

func TestIncrementalListingListsChangesAndDeletions(t *testing.T) {
	b, srv := fixture(t, 250)
	spread(b, 30)
	p := configured(t, srv.URL)
	full := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 512}
	var token string
	for {
		out, err := p.List(context.Background(), full)
		if err != nil {
			t.Fatal(err)
		}
		if out.Complete {
			token = out.ChangeToken
			break
		}
		full.Cursor = out.NextCursor
	}
	if token == "" {
		t.Fatal("complete listing issued no change token")
	}
	// After the listing: one book changes, one is added and one deleted.
	now := time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339)
	b.mu.Lock()
	b.books[10]["updated_at"] = now
	added := cloneFixtureBook(b.books[0])
	added["id"], added["updated_at"] = "book-new", now
	b.books = append(b.books, added)
	b.deletions = []map[string]any{
		{"book_id": "book-020", "file_format": "epub", "deleted_at": now},
		{"book_id": "book-pdf", "file_format": "pdf", "deleted_at": now},
	}
	b.mu.Unlock()
	time.Sleep(time.Until(time.Now().Add(3 * time.Second).Truncate(time.Second)))

	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 512, ChangesSince: token}
	var listed, removed []string
	for {
		out, err := p.List(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range out.Entries {
			listed = append(listed, e.Id)
		}
		removed = append(removed, out.RemovedEntryIds...)
		if out.Complete {
			if out.ChangeToken == "" || out.ChangeToken == token {
				t.Fatal("incremental listing did not issue a new token")
			}
			break
		}
		req.Cursor, req.ChangesSince = out.NextCursor, ""
	}
	if fmt.Sprint(listed) != "[epub/book-010 epub/book-new]" || fmt.Sprint(removed) != "[epub/book-020]" {
		t.Fatalf("listed=%v removed=%v", listed, removed)
	}
}

func TestChangeTokenIsBoundToItsDirectory(t *testing.T) {
	_, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	anchor := time.Now().Add(-time.Hour).Unix()
	foreign := encodeChangeToken(p.current, "pdf", anchor, anchor)
	for _, token := range []string{foreign, "garbage", encodeChangeToken(p.current, "epub", time.Now().Add(time.Hour).Unix(), anchor)} {
		if out, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: token}); out != nil || status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("token %q admitted: %v %v", token, out, err)
		}
	}
}

func TestListingShrinksPagesToTheReplyBudget(t *testing.T) {
	b, srv := fixture(t, 600)
	spread(b, 7)
	for i, book := range b.books {
		// Enriched catalogs carry long descriptions: 512 of these exceed 1 MiB.
		book["description"] = strings.Repeat(string(rune('a'+i%26)), 4000)
	}
	p := configured(t, srv.URL)
	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 512}
	var ids []string
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatal("listing did not terminate")
		}
		out, err := p.List(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		if size := proto.Size(out); size > 1<<20 {
			t.Fatalf("page %d is %d bytes", pages, size)
		}
		for _, e := range out.Entries {
			ids = append(ids, e.Id)
		}
		if out.Complete {
			break
		}
		req.Cursor = out.NextCursor
	}
	want := make([]string, 600)
	for i := range want {
		want[i] = fmt.Sprintf("epub/book-%03d", i)
	}
	sameIDs(t, ids, want)
}

func TestLargestPermittedEntryFitsOnePage(t *testing.T) {
	text := strings.Repeat("é", maxCatalogTextForTest/2)
	list := make([]string, 64)
	for i := range list {
		list[i] = text
	}
	e := bookEntry(bookwarehouse.Book{ID: strings.Repeat("a", 128), Format: "epub", SHA256: strings.Repeat("0", 64), Size: 1, UpdatedAt: listingBase,
		Title: text, Catalog: bookwarehouse.Catalog{Authors: list, Genres: list, Description: strings.Repeat("d", 64<<10), Publisher: text, PublishedDate: "2020",
			Language: text, ISBN: text, Series: text, SeriesIndex: text, HasCover: true, CoverID: strings.Repeat("c", 128), CoverThumbhash: text}})
	if size := entryBytes(e); size > maxPageBytes {
		t.Fatalf("largest entry is %d bytes, page budget %d", size, maxPageBytes)
	}
}

// Invalid integrity fields must not block later valid entries, including when
// whole backend pages contain only skipped rows with the same timestamp.
func TestListingAdvancesPastUnpublishableRows(t *testing.T) {
	b, srv := fixture(t, 260)
	spread(b, 300)
	for i, book := range b.books {
		if i != 123 && i != 259 {
			book["file_size"] = nil
		}
	}
	p := configured(t, srv.URL)
	got := listAll(t, p, "epub", 1, nil)
	sameIDs(t, got, []string{"epub/book-123", "epub/book-259"})
}

// listWithin pages through req giving every List call budget, as the host's
// per-call deadline does, and reports the entries and how many incomplete
// pages came back short of the limit.
func listWithin(t *testing.T, p *Provider, req *storagev1.ListRequest, budget time.Duration) ([]string, int) {
	t.Helper()
	var ids []string
	seen := map[string]bool{}
	short := 0
	for call := 0; ; call++ {
		if call > 100 {
			t.Fatal("listing did not terminate")
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		out, err := p.List(ctx, req)
		cancel()
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		for _, e := range out.Entries {
			if seen[e.Id] {
				t.Fatalf("call %d: duplicate entry %s", call, e.Id)
			}
			seen[e.Id] = true
			ids = append(ids, e.Id)
		}
		if out.Complete {
			return ids, short
		}
		if out.NextCursor == "" || out.NextCursor == req.Cursor {
			t.Fatalf("call %d: cursor did not advance", call)
		}
		if len(out.Entries) < int(req.MaxEntries) {
			short++
		}
		req.Cursor = out.NextCursor
	}
}

// Filling a 512-entry page takes six backend pages. When the host's deadline
// leaves no time for the next one, List returns the books it already fetched
// with a cursor instead of failing and discarding them, so a slow
// Bookwarehouse still lets the scan advance.
func TestListingReturnsFetchedBooksBeforeTheDeadline(t *testing.T) {
	b, srv := fixture(t, 600)
	spread(b, 1)
	b.listDelay = 200 * time.Millisecond
	p := configured(t, srv.URL)
	req := &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 512}
	got, short := listWithin(t, p, req, time.Second)
	sameIDs(t, got, expectedIDs(600))
	if short == 0 {
		t.Fatal("no page stopped early for the deadline")
	}
}

// A backend page that cannot finish in time still fails the call: without
// fetched books there is nothing to return and no position to advance.
func TestListingFailsWhenNoBackendPageFinishes(t *testing.T) {
	b, srv := fixture(t, 10)
	b.listDelay = 5 * time.Second
	p := configured(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", MaxEntries: 512})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("List error = %v, want DeadlineExceeded", err)
	}
}

func completedChangeToken(t *testing.T, p *Provider, req *storagev1.ListRequest) (string, []string, []string) {
	t.Helper()
	var ids, removed []string
	for pages := 0; pages < 100; pages++ {
		out, err := p.List(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range out.Entries {
			ids = append(ids, e.Id)
		}
		removed = append(removed, out.RemovedEntryIds...)
		if out.Complete {
			return out.ChangeToken, ids, removed
		}
		req.Cursor, req.ChangesSince = out.NextCursor, ""
	}
	t.Fatal("listing did not complete")
	return "", nil, nil
}

func TestIncrementalListingIncludesLateCommittedUpdates(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	token, _, _ := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	// Both writes become visible after the full listing, with timestamps from
	// an earlier transaction or a backend clock that lags the plugin clock.
	late := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	b.mu.Lock()
	b.books[0]["updated_at"] = late
	b.deletions = []map[string]any{{"book_id": "book-removed", "file_format": "epub", "deleted_at": late}}
	b.mu.Unlock()
	_, ids, removed := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: token})
	sameIDs(t, ids, []string{"epub/book-000"})
	sameIDs(t, removed, []string{"epub/book-removed"})
}

func changeTokenFields(t *testing.T, token string) map[string]any {
	t.Helper()
	body, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestIncrementalTokensRetainFullScanAnchor(t *testing.T) {
	_, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	token, _, _ := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	fields := changeTokenFields(t, token)
	if fields["f"] == nil {
		t.Fatal("change token has no full-scan anchor")
	}
	anchor := time.Now().Add(-6 * 24 * time.Hour).Unix()
	fields["f"] = anchor
	raw, _ := json.Marshal(fields)
	token = sign(p.current, raw)
	next, _, _ := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: token})
	if got := changeTokenFields(t, next)["f"]; got != float64(anchor) {
		t.Fatalf("incremental scan refreshed the full-scan anchor: %v", got)
	}
	fields = changeTokenFields(t, next)
	fields["f"] = time.Now().Add(-8 * 24 * time.Hour).Unix()
	raw, _ = json.Marshal(fields)
	_, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: sign(p.current, raw)})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("aged anchor must force a full listing, got %v", err)
	}
}

func TestLegacyChangeTokenAddsOverlap(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	upper := time.Now().Add(-time.Minute).Unix()
	b.books[0]["updated_at"] = time.Unix(upper-60, 0).UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(map[string]any{"v": 1, "d": "epub", "s": upper})
	_, ids, _ := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: sign(p.current, raw)})
	sameIDs(t, ids, []string{"epub/book-000"})
}

func TestIncrementalListingAdvancesPastUnknownDeletionFormats(t *testing.T) {
	b, srv := fixture(t, 1)
	p := configured(t, srv.URL)
	token, _, _ := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	deletedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	b.mu.Lock()
	for i := 0; i < 100; i++ {
		var format any
		if i%2 == 0 {
			format = "mobi"
		}
		b.deletions = append(b.deletions, map[string]any{"book_id": fmt.Sprintf("unknown-%03d", i), "file_format": format, "deleted_at": deletedAt})
	}
	b.deletions = append(b.deletions, map[string]any{"book_id": "book-removed", "file_format": "epub", "deleted_at": deletedAt})
	b.mu.Unlock()
	_, _, removed := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", ChangesSince: token})
	sameIDs(t, removed, []string{"epub/book-removed"})
}

func TestLegacyContinuationCursorsFinishInPlace(t *testing.T) {
	upper := time.Now().Add(-time.Second).Unix()
	since := upper - 120
	for _, tt := range []struct {
		name    string
		c       cursor
		ids     []string
		removed []string
		fullAt  int64
	}{
		{"full books", cursor{Version: 3, Directory: "epub", Page: 1, Upper: upper, AfterSec: since + 10, AfterID: "book-000"}, []string{"epub/book-001"}, nil, upper},
		{"delta books", cursor{Version: 3, Directory: "epub", Page: 1, Upper: upper, Since: since, AfterSec: since + 10, AfterID: "book-000"}, []string{"epub/book-001"}, []string{"epub/book-removed"}, since},
		{"delta deletions", cursor{Version: 3, Directory: "epub", Page: 1, Upper: upper, Since: since, Phase: 1}, nil, []string{"epub/book-removed"}, since},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, srv := fixture(t, 2)
			b.mu.Lock()
			b.books[0]["updated_at"] = time.Unix(since+10, 0).UTC().Format(time.RFC3339)
			b.books[1]["updated_at"] = time.Unix(since+20, 0).UTC().Format(time.RFC3339)
			b.deletions = []map[string]any{{"book_id": "book-removed", "file_format": "epub", "deleted_at": time.Unix(since+30, 0).UTC().Format(time.RFC3339)}}
			b.mu.Unlock()
			p := configured(t, srv.URL)
			token, ids, removed := completedChangeToken(t, p, &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub", Cursor: encodeCursor(p.current, tt.c)})
			sameIDs(t, ids, tt.ids)
			sameIDs(t, removed, tt.removed)
			if got := changeTokenFields(t, token)["f"]; got != float64(tt.fullAt) {
				t.Fatalf("legacy continuation anchor = %v, want %d", got, tt.fullAt)
			}
		})
	}
}
