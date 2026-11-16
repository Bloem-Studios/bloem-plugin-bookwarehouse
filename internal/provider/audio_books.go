package provider

import (
	"context"
	"sync"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
)

// A host lists a book, then opens and probes each of its tracks in quick
// succession, and every read needs the book's current track description. The
// description a listing or read fetched is reused for audioBookTTL instead of
// being fetched again per read. Each range reply still answers authority and
// presence and carries the current size, so only a rewrite that keeps a
// track's size can go unnoticed, and only until the description expires.
const (
	audioBookTTL      = 30 * time.Second
	maxAudioBookCache = 256
)

type audioBookCache struct {
	mu    sync.Mutex
	books map[string]cachedAudioBook
	order []string
}

type cachedAudioBook struct {
	book bookwarehouse.Audiobook
	at   time.Time
}

func (c *audioBookCache) get(id string) (bookwarehouse.Audiobook, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached, ok := c.books[id]
	if !ok || time.Since(cached.at) >= audioBookTTL {
		return bookwarehouse.Audiobook{}, false
	}
	return cached.book, true
}

func (c *audioBookCache) put(b bookwarehouse.Audiobook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.books == nil {
		c.books = map[string]cachedAudioBook{}
	}
	id := b.Book.ID
	if _, ok := c.books[id]; !ok {
		if len(c.order) == maxAudioBookCache {
			delete(c.books, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, id)
	}
	c.books[id] = cachedAudioBook{book: b, at: time.Now()}
}

// freshAudiobook fetches a book's current description and keeps it.
func (s *state) freshAudiobook(ctx context.Context, id string) (bookwarehouse.Audiobook, error) {
	b, err := s.client.Audiobook(ctx, id)
	if err == nil {
		s.books.put(b)
	}
	return b, err
}

// audiobook returns a recently fetched description of a book, or a fresh one.
func (s *state) audiobook(ctx context.Context, id string) (bookwarehouse.Audiobook, error) {
	if b, ok := s.books.get(id); ok {
		return b, nil
	}
	return s.freshAudiobook(ctx, id)
}
