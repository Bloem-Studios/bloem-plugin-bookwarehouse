// Package verifiedcache stores only complete, SHA256-verified book files.
package verifiedcache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ErrMismatch = errors.New("verified cache content mismatch")
	ErrTooLarge = errors.New("verified cache book exceeds limit")
	ErrCapacity = errors.New("verified cache capacity exceeded")
	ErrClosed   = errors.New("verified cache closed")
	errStorage  = errors.New("verified cache storage failed")
	errDownload = errors.New("verified cache download failed")
)

const (
	// maxEntries bounds retained file descriptors and metadata even for tiny
	// books. Each active download consumes one entry slot before creating its
	// temporary file.
	maxEntries = 128
	// maxDownloads bounds concurrent verified downloads, so opening one book
	// never waits for another book's download to finish.
	maxDownloads = 4
	// maxAttempts bounds the requests one download may make; a download also
	// gives up after maxStalledAttempts consecutive requests without progress.
	maxAttempts        = 64
	maxStalledAttempts = 4
)

// Source opens the book's bytes beginning at offset. It returns the reader,
// the absolute position of the reader's first byte and the object size the
// backend declared, or -1 when it declared none. A source that cannot resume
// returns position 0 and the cache restarts the download. The reader must
// honor ctx, as an HTTP response body does. Errors wrapped with Permanent,
// and ErrMismatch, end the download; other errors are retried.
type Source func(ctx context.Context, offset int64) (body io.ReadCloser, position, total int64, err error)

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks a Source error the cache must not retry.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func isPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p) || errors.Is(err, ErrMismatch) || errors.Is(err, errStorage)
}

// Cache owns a private temporary directory. Its byte budget includes complete
// files and the declared size of each active download. It also limits files to
// 128 entries including active downloads.
//
// Downloads belong to the cache, not to the callers that asked for them. A
// caller whose context ends stops waiting while the download continues, so a
// later or concurrent Acquire of the same book joins it or finds the verified
// file. Up to maxDownloads books download at once; an interrupted transfer
// resumes from the bytes already written and hashed. Waiting callers consume
// no file descriptors, disk reservations or workers. A Cache must not be
// copied after first use.
type Cache struct {
	mu                      sync.Mutex
	maxBook, maxBytes, used int64
	dir                     string
	entries                 map[string]*entry
	recent                  list.List // Most recently acquired entry first.
	changed                 chan struct{}
	flights                 map[string]*flight
	unclaimed               map[string]string // Snapshot identity -> verified digest awaiting a caller.
	ctx                     context.Context
	cancel                  context.CancelFunc
	closed                  bool
	closeDone               chan struct{}
	closeErr                error

	// downloadTimeout bounds one download from its first request; idleTimeout
	// abandons a request that delivers no bytes for that long and resumes;
	// retryDelay is the first backoff after a request that made no progress.
	downloadTimeout, idleTimeout, retryDelay time.Duration
}
type entry struct {
	hash   string
	size   int64
	file   *os.File
	refs   int
	recent *list.Element
}

// flight is one book's active download. done closes after entry or err is set.
type flight struct {
	key   string
	hash  string
	size  int64
	done  chan struct{}
	entry *entry
	err   error
}

// Lease pins one immutable verified file. ReadAt is safe for concurrent callers,
// including callers racing Release. Release is idempotent.
type Lease struct {
	mu       sync.RWMutex
	cache    *Cache
	entry    *entry
	released bool
}

// New creates a cache whose two limits must both be positive. A book that fits
// maxBookBytes but cannot fit maxCacheBytes is rejected by Acquire.
func New(maxBookBytes, maxCacheBytes int64) (*Cache, error) {
	if maxBookBytes <= 0 || maxCacheBytes <= 0 {
		return nil, ErrCapacity
	}
	dir, err := os.MkdirTemp("", "bookwarehouse-verified-")
	if err != nil {
		return nil, errStorage
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Cache{
		maxBook: maxBookBytes, maxBytes: maxCacheBytes, dir: dir,
		entries: make(map[string]*entry), changed: make(chan struct{}), flights: make(map[string]*flight), unclaimed: make(map[string]string),
		ctx: ctx, cancel: cancel, closeDone: make(chan struct{}),
		downloadTimeout: 30 * time.Minute, idleTimeout: time.Minute, retryDelay: 500 * time.Millisecond,
	}, nil
}

// Acquire returns a lease only after the exact declared size and SHA256 have
// been verified. When the book is neither cached nor downloading, it starts a
// download from src if a download slot and capacity are free, and otherwise
// waits for them. If ctx ends first Acquire returns ctx's error, and a download
// it started keeps running for later callers. downloaded reports that this
// call started the download that produced the lease.
func (c *Cache) Acquire(ctx context.Context, hash string, size int64, src Source) (lease *Lease, downloaded bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size || size < 0 {
		return nil, false, ErrMismatch
	}
	hash = strings.ToLower(hash)
	return c.acquire(ctx, hash, hash, size, src)
}

// AcquireSnapshot verifies the whole declared file without assuming an initial
// digest. Concurrent calls for identity join one bounded transfer. A subsequent
// call starts a fresh verification: an unversioned provider fingerprint cannot
// establish that current bytes still match an earlier snapshot.
func (c *Cache) AcquireSnapshot(ctx context.Context, identity string, declaredSize int64, src Source) (*Lease, string, error) {
	if identity == "" || len(identity) > 4096 || declaredSize < 0 {
		return nil, "", ErrMismatch
	}
	lease, _, err := c.acquire(ctx, "snapshot:"+identity, "", declaredSize, src)
	if err != nil {
		return nil, "", err
	}
	return lease, lease.entry.hash, nil
}

func (c *Cache) acquire(ctx context.Context, key, hash string, size int64, src Source) (*Lease, bool, error) {
	if size > c.maxBook {
		return nil, false, ErrTooLarge
	}
	if size > c.maxBytes {
		return nil, false, ErrCapacity
	}
	var started *flight
	c.mu.Lock()
	for {
		if c.closed {
			c.mu.Unlock()
			return nil, false, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, false, err
		}
		if hash == "" {
			if digest := c.unclaimed[key]; digest != "" {
				delete(c.unclaimed, key)
				if e := c.entries[digest]; e != nil {
					if e.size != size {
						c.mu.Unlock()
						return nil, false, ErrMismatch
					}
					return c.leaseLocked(e), false, nil
				}
			}
		}
		if e := c.entries[hash]; hash != "" && e != nil {
			if e.size != size {
				c.mu.Unlock()
				return nil, false, ErrMismatch
			}
			return c.leaseLocked(e), false, nil
		}
		f := c.flights[key]
		if f != nil && f.size != size {
			c.mu.Unlock()
			return nil, false, ErrMismatch
		}
		if f == nil && len(c.flights) < maxDownloads {
			if src == nil {
				c.mu.Unlock()
				return nil, false, errDownload
			}
			fits, err := c.makeRoomLocked(size)
			if err != nil {
				c.mu.Unlock()
				return nil, false, err
			}
			if fits {
				f = &flight{key: key, hash: hash, size: size, done: make(chan struct{})}
				c.flights[key] = f
				c.used += size
				started = f
				go c.run(f, src)
			}
		}
		wait := c.changed
		if f != nil {
			wait = f.done
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-wait:
		}
		c.mu.Lock()
		if f == nil {
			continue
		}
		if f.err != nil {
			c.mu.Unlock()
			return nil, false, f.err
		}
		// The finished file may already have been evicted under pressure; the
		// loop then finds or starts another download.
		if !c.closed && f.entry != nil && c.entries[f.entry.hash] == f.entry {
			if c.unclaimed[key] == f.entry.hash {
				delete(c.unclaimed, key)
			}
			return c.leaseLocked(f.entry), f == started, nil
		}
	}
}

// leaseLocked pins e for a new lease and releases c.mu.
func (c *Cache) leaseLocked(e *entry) *Lease {
	e.refs++
	c.recent.MoveToFront(e.recent)
	c.mu.Unlock()
	return &Lease{cache: c, entry: e}
}

// run downloads one flight under the cache's lifetime and publishes the result.
func (c *Cache) run(f *flight, src Source) {
	ctx, cancel := context.WithTimeout(c.ctx, c.downloadTimeout)
	e, err := c.download(ctx, f.hash, f.size, src)
	timedOut := err != nil && ctx.Err() != nil && c.ctx.Err() == nil
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		err = ErrClosed
	case timedOut:
		err = errDownload
	}
	if err != nil {
		if e != nil {
			c.discardFileLocked(e.file)
			e = nil
		}
		c.used -= f.size
	} else {
		if existing := c.entries[e.hash]; existing != nil {
			// Equal full digests share one immutable entry and one reservation.
			c.discardFileLocked(e.file)
			c.used -= f.size
			e = existing
		} else {
			e.recent = c.recent.PushFront(e)
			c.entries[e.hash] = e
		}
		if f.hash == "" {
			if len(c.unclaimed) >= maxEntries {
				for key := range c.unclaimed {
					delete(c.unclaimed, key)
					break
				}
			}
			c.unclaimed[f.key] = e.hash
		}
	}
	delete(c.flights, f.key)
	f.entry, f.err = e, err
	close(f.done)
	c.notifyLocked()
	c.removeDirectoryLocked()
}

// makeRoomLocked never evicts pinned entries and avoids addition overflow.
func (c *Cache) makeRoomLocked(size int64) (bool, error) {
	for c.used > c.maxBytes-size || len(c.entries)+len(c.flights) >= maxEntries {
		var victim *entry
		for it := c.recent.Back(); it != nil; it = it.Prev() {
			e := it.Value.(*entry)
			if e.refs == 0 {
				victim = e
				break
			}
		}
		if victim == nil {
			return false, nil
		}
		if err := c.removeEntryLocked(victim); err != nil {
			return false, err
		}
	}
	return true, nil
}

// download writes and hashes exactly size bytes, resuming after interrupted
// requests, and returns the file only when its SHA256 matches hash.
func (c *Cache) download(ctx context.Context, hash string, size int64, src Source) (e *entry, err error) {
	file, err := os.CreateTemp(c.dir, "book-")
	if err != nil {
		return nil, errStorage
	}
	defer func() {
		if err != nil {
			file.Close()
			os.Remove(file.Name())
		}
	}()
	progress := &transfer{file: file, sum: sha256.New(), size: size}
	stalled := 0
	for attempt := 1; ; attempt++ {
		if hash == "" && progress.written > 0 {
			// Without an expected digest, a range retry could splice bytes from
			// two provider generations and hash a file that never existed.
			if err := progress.restart(); err != nil {
				return nil, err
			}
		}
		before := progress.written
		err = c.attempt(ctx, src, progress)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isPermanent(err) {
			return nil, err
		}
		if progress.written > before {
			if hash == "" && attempt >= maxAttempts {
				return nil, errDownload
			}
			stalled = 0
			continue
		}
		stalled++
		if stalled >= maxStalledAttempts || attempt >= maxAttempts {
			// A body that keeps ending cleanly before the declared size is a
			// shorter object, not an interrupted transfer.
			if errors.Is(err, io.EOF) {
				return nil, ErrMismatch
			}
			return nil, errDownload
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.retryDelay << (stalled - 1)):
		}
	}
	actual := hex.EncodeToString(progress.sum.Sum(nil))
	if hash != "" && actual != hash {
		return nil, ErrMismatch
	}
	return &entry{hash: actual, size: size, file: file}, nil
}

// transfer is one download's verified progress: written bytes, all hashed.
type transfer struct {
	file    *os.File
	sum     hash.Hash
	size    int64
	written int64
}

func (t *transfer) restart() error {
	t.sum.Reset()
	t.written = 0
	if err := t.file.Truncate(0); err != nil {
		return Permanent(errStorage)
	}
	return nil
}

// Write appends to the file at the verified offset and to the running hash.
func (t *transfer) Write(p []byte) (int, error) {
	n, err := t.file.WriteAt(p, t.written)
	t.sum.Write(p[:n])
	t.written += int64(n)
	if err != nil {
		return n, Permanent(errStorage)
	}
	return n, nil
}

// attempt makes one request for the bytes after t.written.
func (c *Cache) attempt(ctx context.Context, src Source, t *transfer) error {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(c.idleTimeout, cancel)
	defer idle.Stop()
	body, position, total, err := src(attemptCtx, t.written)
	if body != nil {
		defer body.Close()
	}
	if err != nil {
		return err
	}
	if body == nil {
		return Permanent(errDownload)
	}
	if total >= 0 && total != t.size {
		return ErrMismatch
	}
	if position != t.written {
		if position != 0 {
			return errDownload
		}
		if err := t.restart(); err != nil {
			return err
		}
	}
	reader := &idleReader{reader: body, idle: idle, timeout: c.idleTimeout}
	// Copy exactly the remaining declared bytes to disk. The extra-byte probe
	// below stays in memory, so even an oversized source cannot exceed the
	// disk budget.
	if _, err := io.CopyN(t, reader, t.size-t.written); err != nil {
		return err
	}
	if total == t.size {
		// The transport enforces the declared length.
		return nil
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n != 0 {
		return ErrMismatch
	}
	if err != io.EOF {
		// The end was never confirmed and nothing is left to resume.
		if restartErr := t.restart(); restartErr != nil {
			return restartErr
		}
		return errDownload
	}
	return nil
}

// idleReader re-arms the request's idle timer whenever bytes arrive.
type idleReader struct {
	reader  io.Reader
	idle    *time.Timer
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.idle.Reset(r.timeout)
	}
	return n, err
}

func (l *Lease) ReadAt(p []byte, off int64) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.released {
		return 0, ErrClosed
	}
	return l.entry.file.ReadAt(p, off)
}

func (l *Lease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	c := l.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	l.entry.refs--
	if c.closed && l.entry.refs == 0 {
		c.recordCloseErrorLocked(c.removeEntryLocked(l.entry))
	}
	c.notifyLocked()
	c.removeDirectoryLocked()
}

// Close rejects new acquisitions, cancels and joins active downloads, and
// removes unpinned files. Existing leases stay readable until their Release;
// the last Release removes the remaining file and private directory.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closeErr
	}
	c.closed = true
	c.cancel()
	c.notifyLocked()
	for _, e := range c.entries {
		if e.refs == 0 {
			c.recordCloseErrorLocked(c.removeEntryLocked(e))
		}
	}
	active := make([]chan struct{}, 0, len(c.flights))
	for _, f := range c.flights {
		active = append(active, f.done)
	}
	c.mu.Unlock()
	for _, done := range active {
		<-done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeDirectoryLocked()
	close(c.closeDone)
	return c.closeErr
}

func (c *Cache) notifyLocked()                    { close(c.changed); c.changed = make(chan struct{}) }
func (c *Cache) recordCloseErrorLocked(err error) { c.closeErr = errors.Join(c.closeErr, err) }
func (c *Cache) removeEntryLocked(e *entry) error {
	// Unlink first: on failure, retain a valid entry and its byte reservation.
	if err := os.Remove(e.file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errStorage
	}
	delete(c.entries, e.hash)
	for key, digest := range c.unclaimed {
		if digest == e.hash {
			delete(c.unclaimed, key)
		}
	}
	c.recent.Remove(e.recent)
	c.used -= e.size
	if err := e.file.Close(); err != nil {
		return errStorage
	}
	return nil
}
func (c *Cache) discardFileLocked(file *os.File) {
	if err := file.Close(); err != nil {
		c.recordCloseErrorLocked(errStorage)
	}
	if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.recordCloseErrorLocked(errStorage)
	}
}
func (c *Cache) removeDirectoryLocked() {
	if !c.closed || len(c.flights) != 0 || len(c.entries) != 0 || c.dir == "" {
		return
	}
	if err := os.Remove(c.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.recordCloseErrorLocked(errStorage)
		return
	}
	c.dir = ""
}
