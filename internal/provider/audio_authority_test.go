package provider

import (
	"context"
	"testing"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAudioWarmCoverRechecksCurrentAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		deny func(*audioBackend)
		want codes.Code
	}{
		{"revoked key", func(b *audioBackend) { b.deny = true }, codes.PermissionDenied},
		{"deleted book", func(b *audioBackend) { b.books = b.books[1:] }, codes.NotFound},
		{"denied cover", func(b *audioBackend) { b.denyCover = true }, codes.PermissionDenied},
		{"deleted cover", func(b *audioBackend) { b.cover = nil }, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, srv := audioFixture(t)
			b.cover = []byte("synthetic jpeg")
			b.books[0]["has_cover"], b.books[0]["cover_id"] = true, "cover-a"
			p := configuredAudio(t, srv.URL, nil)
			ctx := context.Background()
			page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks", DirectoriesOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			meta := page.Entries[0].Audiobook
			stat := &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: meta.CoverEntryId, ExpectedRevision: meta.CoverRevision}
			if _, err := p.Stat(ctx, stat); err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			tc.deny(b)
			b.mu.Unlock()
			if got, err := p.Stat(ctx, stat); status.Code(err) != tc.want || got != nil {
				t.Errorf("warm cover Stat: %v %v; want %s", got, err, tc.want)
			}
			out := &capture{ctx: ctx}
			err = p.Read(&storagev1.ReadRequest{SourceId: stat.SourceId, EntryId: stat.EntryId, ExpectedRevision: stat.ExpectedRevision, Length: 14}, out)
			if status.Code(err) != tc.want || len(out.chunks) != 0 {
				t.Fatalf("warm cover Read: chunks=%d err=%v; want %s", len(out.chunks), err, tc.want)
			}
		})
	}
}

func TestAudioCollectionDirectoryRequiresCurrentIdentity(t *testing.T) {
	for _, tc := range []struct{ kind, id, directory string }{
		{"authors", "m-r-forbes", collectionEntryID("authors", "m-r-forbes", "M. R. Forbes")},
		{"series", "star-crossed", collectionEntryID("series", "star-crossed", "Star Crossed")},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			b, srv := audioFixture(t)
			p := configuredAudio(t, srv.URL, nil)
			ctx := context.Background()
			checkAbsent := func(directory string) {
				t.Helper()
				if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: directory, ExpectedRevision: audioDirectoryRevision}); status.Code(err) != codes.NotFound || got != nil {
					t.Errorf("absent collection Stat: %v %v", got, err)
				}
				if got, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: directory, DirectoriesOnly: true}); status.Code(err) != codes.NotFound || got != nil {
					t.Errorf("absent collection List: %v %v", got, err)
				}
			}
			// Unknown IDs are absent even though the book query can return empty.
			b.mu.Lock()
			b.books = nil
			b.mu.Unlock()
			checkAbsent(collectionEntryID(tc.kind, "nonexistent", "Nonexistent"))
			// A real collection without books remains a valid empty directory.
			if got, err := p.Stat(ctx, &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: tc.directory, ExpectedRevision: audioDirectoryRevision}); err != nil || got == nil {
				t.Fatalf("valid empty collection Stat: %v %v", got, err)
			}
			if got, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: tc.directory, DirectoriesOnly: true}); err != nil || !got.GetComplete() || len(got.GetEntries()) != 0 {
				t.Fatalf("valid empty collection List: %v %v", got, err)
			}
			// Previously listed labels do not prove existence after deletion.
			if _, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: tc.kind, DirectoriesOnly: true, MaxEntries: 1}); err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			b.omitCollection = tc.id
			b.mu.Unlock()
			checkAbsent(tc.directory)
		})
	}
}

func TestAudioCollectionContinuationReadsCurrentProviderRows(t *testing.T) {
	for _, kind := range []string{"authors", "series"} {
		t.Run(kind+"/revoked key", func(t *testing.T) {
			b, srv := audioFixture(t)
			p := configuredAudio(t, srv.URL, nil)
			req := &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: kind, DirectoriesOnly: true, MaxEntries: 1}
			page, err := p.List(context.Background(), req)
			if err != nil || page.GetNextCursor() == "" {
				t.Fatalf("initial collection page: %v %v", page, err)
			}
			b.mu.Lock()
			b.deny = true
			b.mu.Unlock()
			req.Cursor = page.NextCursor
			if got, err := p.List(context.Background(), req); status.Code(err) != codes.PermissionDenied || got != nil {
				t.Fatalf("continued after revocation: %v %v", got, err)
			}
		})
		t.Run(kind+"/deleted collection", func(t *testing.T) {
			b, srv := audioFixture(t)
			p := configuredAudio(t, srv.URL, nil)
			req := &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: kind, DirectoriesOnly: true, MaxEntries: 1}
			page, err := p.List(context.Background(), req)
			if err != nil || page.GetNextCursor() == "" {
				t.Fatalf("initial collection page: %v %v", page, err)
			}
			rows := b.collectionRows(kind)
			b.mu.Lock()
			b.omitCollection = rows[1].id // The row the next page would start with.
			b.mu.Unlock()
			req.Cursor = page.NextCursor
			got, err := p.List(context.Background(), req)
			if err != nil || len(got.Entries) != 1 || got.Entries[0].Name != rows[2].name {
				t.Fatalf("continued past deleted %s: %v %v", rows[1].name, got, err)
			}
		})
	}
}

func TestAudioEmptyCollectionRootsRemainValid(t *testing.T) {
	b, srv := audioFixture(t)
	b.emptyCollections = true
	b.books = nil
	p := configuredAudio(t, srv.URL, nil)
	for _, id := range []string{"audio-root", "audiobooks", "authors", "series"} {
		if got, err := p.Stat(context.Background(), &storagev1.StatRequest{SourceId: "bookwarehouse-audiobooks", EntryId: id, ExpectedRevision: audioDirectoryRevision}); err != nil || got == nil {
			t.Fatalf("empty root Stat %s: %v %v", id, got, err)
		}
		page, err := p.List(context.Background(), &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: id, DirectoriesOnly: true})
		if err != nil || !page.GetComplete() || (id != "audio-root" && len(page.GetEntries()) != 0) {
			t.Fatalf("empty root List %s: %v %v", id, page, err)
		}
	}
}
