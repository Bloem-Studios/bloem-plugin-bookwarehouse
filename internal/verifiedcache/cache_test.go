package verifiedcache_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/verifiedcache"
)

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// source serves data like Bookwarehouse: it declares the object size and
// resumes from any offset.
func source(data string) verifiedcache.Source {
	return func(_ context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		return io.NopCloser(strings.NewReader(data[min(offset, int64(len(data))):])), offset, int64(len(data)), nil
	}
}

// undeclared serves data whole on every request without declaring its size.
func undeclared(data string) verifiedcache.Source {
	return func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
		return io.NopCloser(strings.NewReader(data)), 0, -1, nil
	}
}

// opened adapts a body-returning function to a Source that never resumes.
func opened(open func(context.Context) (io.ReadCloser, error)) verifiedcache.Source {
	return func(ctx context.Context, _ int64) (io.ReadCloser, int64, int64, error) {
		body, err := open(ctx)
		return body, 0, -1, err
	}
}

func cacheFor(t *testing.T, book, total int64) (*verifiedcache.Cache, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	c, err := verifiedcache.New(book, total)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTimings(time.Minute, time.Minute, time.Millisecond)
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, root
}
func acquire(t *testing.T, c *verifiedcache.Cache, data string) *verifiedcache.Lease {
	t.Helper()
	lease, _, err := c.Acquire(context.Background(), digest(data), int64(len(data)), source(data))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	return lease
}

// get acquires without a download context deadline and fails the test on error.
func get(t *testing.T, c *verifiedcache.Cache, data string, src verifiedcache.Source) *verifiedcache.Lease {
	t.Helper()
	lease, _, err := c.Acquire(context.Background(), digest(data), int64(len(data)), src)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func readAt(t *testing.T, lease *verifiedcache.Lease, want string, off int64) {
	t.Helper()
	b := make([]byte, len(want))
	n, err := lease.ReadAt(b, off)
	if err != nil || n != len(want) || string(b) != want {
		t.Fatalf("ReadAt = %q, %d, %v; want %q", b, n, err, want)
	}
}
func cacheDir(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("expected one cache directory, got %v", entries)
	}
	return filepath.Join(root, entries[0].Name())
}
func fileBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

type result struct {
	lease *verifiedcache.Lease
	err   error
}

func await(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire did not finish")
		return result{}
	}
}
func mustSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not start")
	}
}

func TestVerifiedFileIsPrivateAndSharedLeasesSurviveClose(t *testing.T) {
	c, root := cacheFor(t, 10, 10)
	var calls atomic.Int32
	fetch := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		calls.Add(1)
		return source("abcdef")(ctx, offset)
	}
	first, downloaded, err := c.Acquire(context.Background(), digest("abcdef"), 6, fetch)
	if err != nil || !downloaded {
		t.Fatal(downloaded, err)
	}
	defer first.Release()
	readAt(t, first, "cde", 2)
	second, downloaded, err := c.Acquire(context.Background(), strings.ToUpper(digest("abcdef")), 6, fetch)
	if err != nil || downloaded {
		t.Fatal(downloaded, err)
	}
	defer second.Release()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
	dir := cacheDir(t, root)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %o", info.Mode().Perm())
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one verified file: %v", files)
	}
	info, err = files[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || info.Size() != 6 {
		t.Fatalf("file mode/size: %o/%d", info.Mode().Perm(), info.Size())
	}
	first.Release()
	first.Release()
	if _, err := first.ReadAt(make([]byte, 1), 0); !errors.Is(err, verifiedcache.ErrClosed) {
		t.Fatalf("released lease: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	readAt(t, second, "abcdef", 0)
	second.Release()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory survived final release: %v", err)
	}
	if lease, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, fetch); lease != nil || !errors.Is(err, verifiedcache.ErrClosed) {
		t.Fatalf("closed cache: %v, %v", lease, err)
	}
}

func TestNeverPublishesMismatchedTruncatedOrOversizedDownloads(t *testing.T) {
	for name, src := range map[string]verifiedcache.Source{
		"wrong bytes":            source("xxxxxx"),
		"declared shorter":       source("abc"),
		"declared longer":        source("abcdefEXTRA"),
		"undeclared wrong bytes": undeclared("xxxxxx"),
		"undeclared truncated":   undeclared("abc"),
		"undeclared longer":      undeclared("abcdefEXTRA"),
	} {
		t.Run(name, func(t *testing.T) {
			c, root := cacheFor(t, 6, 6)
			lease, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, src)
			if lease != nil || !errors.Is(err, verifiedcache.ErrMismatch) {
				t.Fatalf("unverified download: %v, %v", lease, err)
			}
			if fileBytes(t, root) != 0 {
				t.Fatal("failed download left bytes on disk")
			}
			readAt(t, acquire(t, c, "abcdef"), "abcdef", 0)
		})
	}
}

func TestRejectsInvalidMetadataAndImpossibleCapacityBeforeFetch(t *testing.T) {
	c, _ := cacheFor(t, 4, 3)
	for _, tc := range []struct {
		name, hash string
		size       int64
		want       error
	}{
		{"malformed hash", "bad", 1, verifiedcache.ErrMismatch},
		{"negative size", digest(""), -1, verifiedcache.ErrMismatch},
		{"book bound", digest("abcde"), 5, verifiedcache.ErrTooLarge},
		{"cache bound", digest("abcd"), 4, verifiedcache.ErrCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
				t.Error("invalid request fetched")
				return nil, 0, 0, errors.New("unexpected")
			}
			lease, _, err := c.Acquire(context.Background(), tc.hash, tc.size, fetch)
			if lease != nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %v, %v; want %v", lease, err, tc.want)
			}
		})
	}
	for _, limits := range [][2]int64{{0, 10}, {-1, 10}, {10, 0}, {10, -1}} {
		other, err := verifiedcache.New(limits[0], limits[1])
		if other != nil {
			other.Close()
		}
		if err == nil {
			t.Fatalf("accepted limits %v", limits)
		}
	}
}

func TestWaitsForEOFBeforePublishingLease(t *testing.T) {
	c, _ := cacheFor(t, 6, 6)
	reader, writer := io.Pipe()
	defer writer.Close()
	defer reader.Close()
	out := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, opened(func(context.Context) (io.ReadCloser, error) { return reader, nil }))
		out <- result{l, err}
	}()
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, "abcdef"); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case r := <-out:
		if r.lease != nil {
			r.lease.Release()
		}
		t.Fatalf("published before body consumed: %v", r.err)
	case <-time.After(3 * time.Second):
		t.Fatal("body was not consumed")
	}
	select {
	case r := <-out:
		if r.lease != nil {
			r.lease.Release()
		}
		t.Fatalf("published before EOF: %v", r.err)
	case <-time.After(30 * time.Millisecond):
	}
	writer.Close()
	r := await(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	readAt(t, r.lease, "abcdef", 0)
}

func TestPinnedCapacityWaitIsCancellableAndReleaseUnblocksIt(t *testing.T) {
	c, root := cacheFor(t, 4, 4)
	pinned := acquire(t, c, "aaaa")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var calls atomic.Int32
	fetch := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		calls.Add(1)
		return source("bbbb")(ctx, offset)
	}
	lease, _, err := c.Acquire(ctx, digest("bbbb"), 4, fetch)
	if lease != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("capacity wait: %v, %v", lease, err)
	}
	if calls.Load() != 0 {
		t.Fatal("download started without reserved capacity")
	}
	out := make(chan result, 1)
	go func() { l, _, err := c.Acquire(context.Background(), digest("bbbb"), 4, fetch); out <- result{l, err} }()
	readAt(t, pinned, "aaaa", 0)
	pinned.Release()
	r := await(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	readAt(t, r.lease, "bbbb", 0)
	if total := fileBytes(t, root); total > 4 {
		t.Fatalf("disk bound exceeded: %d", total)
	}
}

func TestEvictsLeastRecentlyUsedUnpinnedEntry(t *testing.T) {
	c, root := cacheFor(t, 3, 6)
	calls := map[string]int{}
	fetch := func(data string) verifiedcache.Source {
		return func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
			calls[data]++
			return source(data)(ctx, offset)
		}
	}
	get(t, c, "aaa", fetch("aaa")).Release()
	get(t, c, "bbb", fetch("bbb")).Release()
	get(t, c, "aaa", fetch("aaa")).Release()
	get(t, c, "ccc", fetch("ccc")).Release()
	pinned := get(t, c, "aaa", fetch("aaa"))
	defer pinned.Release()
	if calls["aaa"] != 1 {
		t.Fatal("evicted recently used entry")
	}
	get(t, c, "bbb", fetch("bbb")).Release()
	if calls["bbb"] != 2 {
		t.Fatal("did not evict least recently used entry")
	}
	readAt(t, pinned, "aaa", 0)
	if total := fileBytes(t, root); total != 6 {
		t.Fatalf("disk usage = %d, want 6", total)
	}
}

func TestSameHashRequiresConsistentSize(t *testing.T) {
	c, _ := cacheFor(t, 10, 10)
	acquire(t, c, "abc")
	l, _, err := c.Acquire(context.Background(), digest("abc"), 4, source("abcd"))
	if l != nil || !errors.Is(err, verifiedcache.ErrMismatch) {
		t.Fatalf("cached inconsistent size: %v, %v", l, err)
	}
}

// blockedSource starts a download that delivers nothing until release closes,
// then serves data. It records each request's offset.
type blockedSource struct {
	data    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	offsets []int64
}

func newBlockedSource(data string) *blockedSource {
	return &blockedSource{data: data, started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockedSource) open(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
	b.mu.Lock()
	b.offsets = append(b.offsets, offset)
	b.mu.Unlock()
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, 0, 0, ctx.Err()
	}
	return source(b.data)(ctx, offset)
}

func TestDownloadOutlivesItsCallerAndLaterCallersJoinIt(t *testing.T) {
	c, root := cacheFor(t, 10, 10)
	src := newBlockedSource("abc")
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(ctx, digest("abc"), 3, src.open)
		out <- result{l, err}
	}()
	mustSignal(t, src.started)
	cancel()
	if r := await(t, out); r.lease != nil || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled caller: %v, %v", r.lease, r.err)
	}
	// A size disagreement with the running download is refused at once.
	if l, _, err := c.Acquire(context.Background(), digest("abc"), 4, source("abcd")); l != nil || !errors.Is(err, verifiedcache.ErrMismatch) {
		t.Fatalf("inflight inconsistent size: %v, %v", l, err)
	}
	// Retried and concurrent callers join the running download.
	joined := make(chan result, 2)
	for range 2 {
		go func() {
			l, downloaded, err := c.Acquire(context.Background(), digest("abc"), 3, func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
				t.Error("joined caller started a second download")
				return nil, 0, 0, errors.New("unexpected")
			})
			if downloaded {
				t.Error("joined caller reported its own download")
			}
			joined <- result{l, err}
		}()
	}
	close(src.release)
	for range 2 {
		r := await(t, joined)
		if r.err != nil {
			t.Fatal(r.err)
		}
		readAt(t, r.lease, "abc", 0)
		r.lease.Release()
	}
	if len(src.offsets) != 1 || fileBytes(t, root) != 3 {
		t.Fatalf("requests at %v, %d bytes cached", src.offsets, fileBytes(t, root))
	}
}

func TestDifferentBooksDownloadConcurrentlyUpToTheLimit(t *testing.T) {
	c, _ := cacheFor(t, 10, 100)
	sources := make([]*blockedSource, verifiedcache.MaxDownloads)
	out := make(chan result, len(sources))
	for i := range sources {
		data := strings.Repeat(string(rune('a'+i)), 3)
		sources[i] = newBlockedSource(data)
		go func() {
			l, _, err := c.Acquire(context.Background(), digest(data), 3, sources[i].open)
			out <- result{l, err}
		}()
	}
	for _, src := range sources {
		mustSignal(t, src.started)
	}
	// Every slot is busy: another book waits rather than downloading.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer waitCancel()
	if l, _, err := c.Acquire(waitCtx, digest("zzz"), 3, func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
		t.Error("download exceeded the concurrency limit")
		return nil, 0, 0, errors.New("unexpected")
	}); l != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("download slot wait: %v, %v", l, err)
	}
	for _, src := range sources {
		close(src.release)
	}
	for range sources {
		r := await(t, out)
		if r.err != nil {
			t.Fatal(r.err)
		}
		r.lease.Release()
	}
	readAt(t, acquire(t, c, "zzz"), "zzz", 0)
}

// interrupted serves data in failing pieces: each request delivers at most
// step bytes and then breaks, as a server write timeout does.
type interrupted struct {
	data    string
	step    int
	resumes bool
	mu      sync.Mutex
	offsets []int64
}

func (s *interrupted) open(_ context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
	s.mu.Lock()
	s.offsets = append(s.offsets, offset)
	s.mu.Unlock()
	if !s.resumes {
		offset = 0
	}
	end := min(int(offset)+s.step, len(s.data))
	var reader io.Reader = strings.NewReader(s.data[offset:end])
	if end < len(s.data) {
		reader = io.MultiReader(reader, errorReader{})
	}
	return io.NopCloser(reader), offset, int64(len(s.data)), nil
}

func TestInterruptedDownloadResumesFromVerifiedOffset(t *testing.T) {
	c, _ := cacheFor(t, 32, 32)
	src := &interrupted{data: "the whole verified book", step: 5, resumes: true}
	lease, downloaded, err := c.Acquire(context.Background(), digest(src.data), int64(len(src.data)), src.open)
	if err != nil || !downloaded {
		t.Fatal(downloaded, err)
	}
	defer lease.Release()
	readAt(t, lease, src.data, 0)
	if fmt.Sprint(src.offsets) != "[0 5 10 15 20]" {
		t.Fatalf("requests at offsets %v", src.offsets)
	}
}

func TestDownloadRestartsWhenSourceCannotResume(t *testing.T) {
	c, _ := cacheFor(t, 32, 32)
	attempts := 0
	data := "restarted book"
	src := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		attempts++
		if attempts == 1 {
			return io.NopCloser(io.MultiReader(strings.NewReader("XXXX"), errorReader{})), 0, int64(len(data)), nil
		}
		// The second request ignores the range and sends the whole object.
		return undeclared(data)(ctx, offset)
	}
	lease, _, err := c.Acquire(context.Background(), digest(data), int64(len(data)), src)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	readAt(t, lease, data, 0)
}

func TestObjectChangedBetweenResumedRequestsIsRejected(t *testing.T) {
	c, root := cacheFor(t, 32, 32)
	attempts := 0
	src := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		attempts++
		if attempts == 1 {
			return io.NopCloser(io.MultiReader(strings.NewReader("abc"), errorReader{})), 0, 6, nil
		}
		return source("abcXYZ")(ctx, offset) // Replaced at the same size.
	}
	if lease, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, src); lease != nil || !errors.Is(err, verifiedcache.ErrMismatch) {
		t.Fatalf("spliced download: %v, %v", lease, err)
	}
	if fileBytes(t, root) != 0 {
		t.Fatal("spliced download left bytes")
	}
}

func TestStalledRequestIsAbandonedAndResumed(t *testing.T) {
	c, _ := cacheFor(t, 32, 32)
	c.SetTimings(time.Minute, 50*time.Millisecond, time.Millisecond)
	var offsets []int64
	src := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		offsets = append(offsets, offset)
		if len(offsets) == 1 {
			reader, writer := io.Pipe()
			go func() {
				io.WriteString(writer, "abc")
				<-ctx.Done() // Stall until the idle timeout abandons the request.
				writer.CloseWithError(ctx.Err())
			}()
			return reader, 0, 6, nil
		}
		return source("abcdef")(ctx, offset)
	}
	lease, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, src)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	readAt(t, lease, "abcdef", 0)
	if fmt.Sprint(offsets) != "[0 3]" {
		t.Fatalf("requests at offsets %v", offsets)
	}
}

func TestPermanentSourceErrorsAreNotRetried(t *testing.T) {
	c, root := cacheFor(t, 3, 3)
	denied := errors.New("denied")
	calls := 0
	lease, _, err := c.Acquire(context.Background(), digest("abc"), 3, func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
		calls++
		return nil, 0, 0, verifiedcache.Permanent(denied)
	})
	if lease != nil || !errors.Is(err, denied) || calls != 1 {
		t.Fatalf("permanent error: %v, %v after %d requests", lease, err, calls)
	}
	if fileBytes(t, root) != 0 {
		t.Fatal("failed source left a file")
	}
}

func TestDownloadGivesUpAfterTimeout(t *testing.T) {
	c, root := cacheFor(t, 3, 3)
	c.SetTimings(50*time.Millisecond, time.Minute, time.Millisecond)
	src := newBlockedSource("abc")
	lease, _, err := c.Acquire(context.Background(), digest("abc"), 3, src.open)
	if lease != nil || err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed out download: %v, %v", lease, err)
	}
	if fileBytes(t, root) != 0 {
		t.Fatal("timed out download left bytes")
	}
	readAt(t, acquire(t, c, "abc"), "abc", 0)
}

func TestCloseCancelsReadingAndWaitingAcquiresAndRemovesTempDirectory(t *testing.T) {
	c, root := cacheFor(t, 10, 10)
	started, bodyClosed := make(chan struct{}), make(chan struct{})
	first := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(context.Background(), digest("abc"), 3, opened(func(ctx context.Context) (io.ReadCloser, error) {
			reader, writer := io.Pipe()
			stop := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
			close(started)
			return &observedBody{ReadCloser: reader, close: func() { stop(); writer.Close(); close(bodyClosed) }}, nil
		}))
		first <- result{l, err}
	}()
	mustSignal(t, started)
	second := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(context.Background(), digest("abc"), 3, source("abc"))
		second <- result{l, err}
	}()
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel download")
	}
	for _, out := range []chan result{first, second} {
		r := await(t, out)
		if r.lease != nil || !errors.Is(r.err, verifiedcache.ErrClosed) {
			t.Fatalf("closed acquisition: %v, %v", r.lease, r.err)
		}
	}
	mustSignal(t, bodyClosed)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Close leaked files: %v, %v", entries, err)
	}
}

type observedBody struct {
	io.ReadCloser
	close func()
}

func (b *observedBody) Close() error { err := b.ReadCloser.Close(); b.close(); return err }

func TestConcurrentAcquiresShareOneVerifiedFile(t *testing.T) {
	c, root := cacheFor(t, 6, 6)
	var calls atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
				calls.Add(1)
				return source("abcdef")(ctx, offset)
			})
			if err != nil {
				t.Error(err)
				return
			}
			defer l.Release()
			for j := 0; j < 30; j++ {
				b := make([]byte, 3)
				if n, err := l.ReadAt(b, 2); err != nil || n != 3 || string(b) != "cde" {
					t.Errorf("concurrent read: %q, %d, %v", b, n, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("downloaded %d times", calls.Load())
	}
	if total := fileBytes(t, root); total != 6 {
		t.Fatalf("disk usage %d", total)
	}
}

func TestCancelledContextCannotAcquireCachedLease(t *testing.T) {
	c, _ := cacheFor(t, 3, 3)
	acquire(t, c, "abc")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l, _, err := c.Acquire(ctx, digest("abc"), 3, source("abc"))
	if l != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled hit: %v, %v", l, err)
	}
}

func TestInflightBytesRespectBudgetAndFailureCleansPartialFile(t *testing.T) {
	c, root := cacheFor(t, 6, 10)
	pinned := acquire(t, c, "keep")
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	out := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(context.Background(), digest("abcdef"), 6, opened(func(ctx context.Context) (io.ReadCloser, error) {
			stop := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
			return &observedBody{ReadCloser: reader, close: func() { stop(); writer.Close() }}, nil
		}))
		out <- result{l, err}
	}()
	// The writer's second write cannot be read until the first chunk has been
	// written to disk. Leave the extra-byte EOF probe blocked after all 6 bytes.
	if _, err := io.WriteString(writer, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "def"); err != nil {
		t.Fatal(err)
	}
	// A capacity wait that ends must never start another source.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer waitCancel()
	lease, _, err := c.Acquire(waitCtx, digest("new"), 3, func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
		t.Error("unreserved download started")
		return nil, 0, 0, errors.New("unexpected")
	})
	if lease != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting acquire: %v, %v", lease, err)
	}
	if used := fileBytes(t, root); used != 10 {
		t.Fatalf("inflight + pinned bytes = %d, want 10", used)
	}
	// The source sends a seventh byte: more than declared.
	if _, err := io.WriteString(writer, "!"); err != nil {
		t.Fatal(err)
	}
	r := await(t, out)
	if r.lease != nil || !errors.Is(r.err, verifiedcache.ErrMismatch) {
		t.Fatalf("oversized read: %v, %v", r.lease, r.err)
	}
	if used := fileBytes(t, root); used != 4 {
		t.Fatalf("partial file leaked: %d bytes remain", used)
	}
	readAt(t, pinned, "keep", 0)
	readAt(t, acquire(t, c, "abcdef"), "abcdef", 0)
}

func TestFailedSourceClosesBodiesAndReleasesReservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fetch func(*atomic.Int32) verifiedcache.Source
	}{
		{"fetch error with body", func(closed *atomic.Int32) verifiedcache.Source {
			return opened(func(context.Context) (io.ReadCloser, error) {
				return &observedBody{ReadCloser: io.NopCloser(strings.NewReader("abc")), close: func() { closed.Add(1) }}, errors.New("fetch failed")
			})
		}},
		{"read error", func(closed *atomic.Int32) verifiedcache.Source {
			return opened(func(context.Context) (io.ReadCloser, error) {
				return &observedBody{ReadCloser: io.NopCloser(errorReader{}), close: func() { closed.Add(1) }}, nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, root := cacheFor(t, 3, 3)
			var closed atomic.Int32
			lease, _, err := c.Acquire(context.Background(), digest("abc"), 3, tc.fetch(&closed))
			if lease != nil || err == nil {
				t.Fatalf("failed source published: %v, %v", lease, err)
			}
			// Each retried request closed its body before giving up.
			if closed.Load() < 2 {
				t.Fatalf("closed %d bodies", closed.Load())
			}
			if fileBytes(t, root) != 0 {
				t.Fatal("failed source left a file")
			}
			readAt(t, acquire(t, c, "abc"), "abc", 0)
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestEmptyBookIsVerifiedAndMissingSourceFailsCleanly(t *testing.T) {
	c, root := cacheFor(t, 1, 1)
	empty := acquire(t, c, "")
	b := make([]byte, 1)
	if n, err := empty.ReadAt(b, 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("empty file: %d, %v", n, err)
	}
	for _, fetch := range []verifiedcache.Source{nil, func(context.Context, int64) (io.ReadCloser, int64, int64, error) { return nil, 0, 0, nil }} {
		l, _, err := c.Acquire(context.Background(), digest("x"), 1, fetch)
		if l != nil || err == nil {
			t.Fatalf("accepted missing source: %v, %v", l, err)
		}
	}
	if used := fileBytes(t, root); used != 0 {
		t.Fatalf("missing source left %d bytes", used)
	}
	readAt(t, acquire(t, c, "x"), "x", 0)
}

func TestConcurrentReadReleaseAndClose(t *testing.T) {
	c, root := cacheFor(t, 3, 3)
	leases := make([]*verifiedcache.Lease, 16)
	for i := range leases {
		leases[i] = acquire(t, c, "abc")
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, lease := range leases {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 30; j++ {
				b := make([]byte, 3)
				n, err := lease.ReadAt(b, 0)
				if errors.Is(err, verifiedcache.ErrClosed) {
					return
				}
				if err != nil || n != 3 || string(b) != "abc" {
					t.Errorf("racing read: %q, %d, %v", b, n, err)
					return
				}
			}
		}()
		go func() { defer wg.Done(); <-start; lease.Release(); lease.Release() }()
	}
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("concurrent Close/Release leaked: %v, %v", entries, err)
	}
}

func TestEntryLimitEvictsLeastRecentlyUsedTinyBook(t *testing.T) {
	c, root := cacheFor(t, 16, 2048)
	calls := make(map[string]int)
	fetch := func(data string) verifiedcache.Source {
		return func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
			calls[data]++
			return source(data)(ctx, offset)
		}
	}
	for i := 0; i < 128; i++ {
		data := string([]byte{byte(i)})
		get(t, c, data, fetch(data)).Release()
	}
	// Refresh the oldest entry. Entry 1 should now be the eviction candidate.
	get(t, c, string([]byte{0}), fetch(string([]byte{0}))).Release()
	get(t, c, string([]byte{128}), fetch(string([]byte{128}))).Release()
	files, err := os.ReadDir(cacheDir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 128 {
		t.Fatalf("tiny-book cache retains %d files; limit is 128", len(files))
	}
	get(t, c, string([]byte{0}), fetch(string([]byte{0}))).Release()
	get(t, c, string([]byte{127}), fetch(string([]byte{127}))).Release()
	if calls[string([]byte{0})] != 1 || calls[string([]byte{127})] != 1 {
		t.Fatal("entry limit evicted a recently used book")
	}
	get(t, c, string([]byte{1}), fetch(string([]byte{1}))).Release()
	if calls[string([]byte{1})] != 2 {
		t.Fatal("entry limit did not evict the least recently used book")
	}
}

func TestPinnedEntryLimitWaitIsCancellableAndReleaseUnblocksIt(t *testing.T) {
	c, root := cacheFor(t, 16, 2048)
	pinned := make([]*verifiedcache.Lease, 128)
	for i := range pinned {
		pinned[i] = acquire(t, c, string([]byte{byte(i)}))
	}
	data := string([]byte{128})
	var calls atomic.Int32
	fetch := func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		calls.Add(1)
		return source(data)(ctx, offset)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	lease, _, err := c.Acquire(ctx, digest(data), 1, fetch)
	if lease != nil {
		lease.Release()
	}
	if lease != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pinned entry wait: %v, %v", lease, err)
	}
	if calls.Load() != 0 {
		t.Fatal("download started without a reserved entry slot")
	}
	out := make(chan result, 1)
	go func() { l, _, err := c.Acquire(context.Background(), digest(data), 1, fetch); out <- result{l, err} }()
	pinned[0].Release()
	r := await(t, out)
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.lease.Release()
	readAt(t, r.lease, data, 0)
	for i := 1; i < len(pinned); i++ {
		readAt(t, pinned[i], string([]byte{byte(i)}), 0)
	}
	files, err := os.ReadDir(cacheDir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 128 {
		t.Fatalf("pinned entry replacement retained %d files", len(files))
	}
}

func TestEntryLimitIncludesInflightDownload(t *testing.T) {
	c, root := cacheFor(t, 16, 2048)
	for i := 0; i < 128; i++ {
		acquire(t, c, string([]byte{byte(i)})).Release()
	}
	failed := errors.New("failed")
	started, fail := make(chan struct{}), make(chan struct{})
	out := make(chan result, 1)
	go func() {
		l, _, err := c.Acquire(context.Background(), digest("pending"), 7, func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
			close(started)
			<-fail
			return nil, 0, 0, verifiedcache.Permanent(failed)
		})
		out <- result{l, err}
	}()
	mustSignal(t, started)
	files, err := os.ReadDir(cacheDir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 128 {
		t.Fatalf("cache plus inflight download uses %d files; limit is 128", len(files))
	}
	close(fail)
	r := await(t, out)
	if r.lease != nil || !errors.Is(r.err, failed) {
		t.Fatalf("failed inflight entry: %v, %v", r.lease, r.err)
	}
	files, err = os.ReadDir(cacheDir(t, root))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 127 {
		t.Fatalf("failed entry was not removed: %d files", len(files))
	}
}
