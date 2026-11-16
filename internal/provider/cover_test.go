package provider

import (
	"bytes"
	"context"
	"testing"
	"time"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCoverEntryIsReadableAtItsRevision(t *testing.T) {
	b, srv := fixture(t, 1)
	b.cover = bytes.Repeat([]byte("jpeg"), 50000)
	b.books[0]["has_cover"], b.books[0]["cover_id"] = true, "cover-1"
	p := configured(t, srv.URL)
	revision := listedCoverRevision(t, p)
	ctx := context.Background()
	stat, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision})
	if err != nil || stat.Entry.Size != int64(len(b.cover)) {
		t.Fatalf("%v %v", stat, err)
	}
	out := &capture{ctx: ctx}
	if err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision, Offset: 10, Length: 150000}, out); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for i, c := range out.chunks {
		if c.Eof != (i == len(out.chunks)-1) {
			t.Fatal("eof misplaced")
		}
		got = append(got, c.Data...)
	}
	if !bytes.Equal(got, b.cover[10:150010]) {
		t.Fatal("wrong cover bytes")
	}
	// Stat then Read costs one book lookup and one cover download.
	b.mu.Lock()
	lookups, coverReads := b.lookups, b.coverReads
	b.mu.Unlock()
	if lookups != 1 || coverReads != 1 {
		t.Fatalf("lookups=%d cover downloads=%d", lookups, coverReads)
	}
	// Once the check is older than coverFreshFor, a replaced cover fails the old revision.
	later := time.Now().Add(coverFreshFor)
	coverClock = func() time.Time { return later }
	t.Cleanup(func() { coverClock = time.Now })
	b.mu.Lock()
	b.books[0]["cover_id"] = "cover-2"
	b.mu.Unlock()
	if err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision, Length: 4}, &capture{ctx: ctx}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale cover read: %v", err)
	}
	b.mu.Lock()
	b.books[0]["has_cover"] = false
	b.mu.Unlock()
	if _, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision}); status.Code(err) != codes.NotFound {
		t.Fatalf("absent cover: %v", err)
	}
}

func listedCoverRevision(t *testing.T, p *Provider) string {
	t.Helper()
	out, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse", DirectoryId: "epub"})
	if err != nil || len(out.GetEntries()) != 1 || out.Entries[0].GetEbook().GetCoverRevision() == "" {
		t.Fatalf("cover listing: %v %v", out, err)
	}
	return out.Entries[0].GetEbook().GetCoverRevision()
}

func TestCoverRevisionChangesOnInPlaceReplacement(t *testing.T) {
	b, srv := fixture(t, 1)
	b.books[0]["has_cover"], b.books[0]["cover_id"] = true, "cover-1"
	p := configured(t, srv.URL)
	old := listedCoverRevision(t, p)
	b.mu.Lock()
	b.books[0]["updated_at"] = "2026-10-07T12:00:01Z"
	b.mu.Unlock()
	updated := listedCoverRevision(t, p)
	if updated == old {
		t.Fatal("in-place cover update kept the same revision")
	}
	if _, err := p.Stat(context.Background(), &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: old}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("old in-place cover revision admitted: %v", err)
	}
	b.mu.Lock()
	b.books[0]["cover_thumbhash"] = "replacement-thumb"
	b.mu.Unlock()
	if got := listedCoverRevision(t, p); got == updated {
		t.Fatal("changed thumbhash kept the same revision")
	}
}

func TestExpiredCoverCacheRefreshesBytes(t *testing.T) {
	b, srv := fixture(t, 1)
	b.books[0]["has_cover"], b.books[0]["cover_id"] = true, "cover-1"
	b.cover = []byte("old")
	p := configured(t, srv.URL)
	revision := listedCoverRevision(t, p)
	_, err := p.Stat(context.Background(), &storagev1.StatRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	// Backend metadata responses can lag the in-place byte replacement.
	b.mu.Lock()
	b.cover = []byte("new")
	b.mu.Unlock()
	later := time.Now().Add(2 * time.Minute)
	coverClock = func() time.Time { return later }
	t.Cleanup(func() { coverClock = time.Now })
	out := &capture{ctx: context.Background()}
	if err := p.Read(&storagev1.ReadRequest{SourceId: "bookwarehouse", EntryId: "cover/book-000", ExpectedRevision: revision, Length: 3}, out); err != nil {
		t.Fatal(err)
	}
	if len(out.chunks) != 1 || string(out.chunks[0].Data) != "new" {
		t.Fatalf("expired cover cache still serves old bytes: %v", out.chunks)
	}
}
