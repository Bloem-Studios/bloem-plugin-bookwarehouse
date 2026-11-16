package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Covers are virtual file entries, cover/<book id>, outside the listed
// directories. The host reads one after publishing its book. Bookwarehouse
// publishes no cover digest and overwrites cover objects under the same ID.
// Pin the available update metadata instead: a changed timestamp or thumbhash
// changes the revision even when the cover ID stays the same.
const coverDirectory = "cover"

func coverEntryID(bookID string) string { return coverDirectory + "/" + bookID }
func coverRevision(b bookwarehouse.Book) string {
	raw, _ := json.Marshal(struct {
		ID, Thumbhash string
		Updated       int64
	}{b.Catalog.CoverID, b.Catalog.CoverThumbhash, b.UpdatedAt.UnixNano()})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("cover:v2:%x", digest)
}

// coverCache keeps recently fetched covers, keyed by book and cover revision.
// A host reads a cover by stating it for its size and then reading it; a
// ebook cover checked against the book within coverFreshFor serves both calls
// with one book lookup and one download. Audio covers recheck current metadata
// and cover-route authority even while the retained bytes are fresh.
type coverCache struct {
	mu    sync.Mutex
	order []string
	data  map[string]cachedCover
}

type cachedCover struct {
	data     []byte
	modified time.Time
	checked  time.Time
}

const (
	coverCacheEntries = 256
	coverFreshFor     = time.Minute
)

// coverClock is replaced in tests.
var coverClock = time.Now

func (c *coverCache) get(key string, now time.Time) (cachedCover, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cover, ok := c.data[key]
	return cover, ok && now.Sub(cover.checked) < coverFreshFor
}

func (c *coverCache) put(key string, cover cachedCover) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = map[string]cachedCover{}
	}
	if _, ok := c.data[key]; !ok {
		if len(c.order) == coverCacheEntries {
			delete(c.data, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.data[key] = cover
}

// coverEntry fetches the cover pinned to expected and describes it. The
// book's current cover identity is checked on every audio call; ebooks reuse
// that check within coverFreshFor. Once that window expires, the bytes are
// fetched again, even if the revision still matches.
func coverEntry(ctx context.Context, s *state, bookID, expected string) (*storagev1.Entry, []byte, error) {
	key := bookID + "\x00" + expected
	cover, fresh := s.covers.get(key, coverClock())
	if !fresh || s.config.Content == "audiobooks" {
		b, err := metadataBook(ctx, s, bookID)
		if err != nil {
			return nil, nil, mapError(ctx, err)
		}
		if b.ID != bookID {
			return nil, nil, status.Error(codes.Unavailable, "invalid Bookwarehouse metadata")
		}
		if !b.Catalog.HasCover {
			return nil, nil, status.Error(codes.NotFound, "entry absent")
		}
		if coverRevision(b) != expected {
			return nil, nil, status.Error(codes.FailedPrecondition, "revision changed")
		}
		if fresh {
			if err := s.client.AuthorizeAudioCover(ctx, bookID); err != nil {
				return nil, nil, mapError(ctx, err)
			}
		} else {
			var data []byte
			if s.config.Content == "audiobooks" {
				data, _, err = s.client.AudioCover(ctx, bookID)
			} else {
				data, _, err = s.client.Cover(ctx, bookID)
			}
			if err != nil {
				return nil, nil, mapError(ctx, err)
			}
			cover = cachedCover{data: data, modified: b.UpdatedAt, checked: coverClock()}
			s.covers.put(key, cover)
		}
	}
	if ctx.Err() != nil {
		return nil, nil, status.FromContextError(ctx.Err()).Err()
	}
	data := cover.data
	id := coverEntryID(bookID)
	return &storagev1.Entry{Id: id, Name: bookID + ".jpg", LogicalPath: id + ".jpg", Kind: storagev1.EntryKind_ENTRY_KIND_FILE,
		Size: int64(len(data)), ModifiedUnixNano: cover.modified.UnixNano(), Revision: expected}, data, nil
}

func readCover(ctx context.Context, s *state, bookID string, r *storagev1.ReadRequest, stream storagev1.StorageProvider_ReadServer) error {
	e, data, err := coverEntry(ctx, s, bookID, r.GetExpectedRevision())
	if err != nil {
		return err
	}
	if r.Offset > e.Size || r.Length > e.Size-r.Offset {
		return status.Error(codes.InvalidArgument, "invalid file range")
	}
	if r.Length == 0 {
		return stream.Send(&storagev1.ReadChunk{Offset: r.Offset, Eof: true})
	}
	end := r.Offset + r.Length
	for offset := r.Offset; offset < end; {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		next := min(offset+128<<10, end)
		if err := stream.Send(&storagev1.ReadChunk{Offset: offset, Data: data[offset:next], Eof: next == end}); err != nil {
			return err
		}
		offset = next
	}
	return nil
}
