package verifiedcache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/verifiedcache"
)

func TestSnapshotHashesWholeFileAndRefreshesIdentity(t *testing.T) {
	c, _ := cacheFor(t, 32, 64)
	a, hash, err := c.AcquireSnapshot(context.Background(), "book/file", 5, source("hello"))
	if err != nil || hash != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("snapshot: %s %v", hash, err)
	}
	defer a.Release()
	b, changed, err := c.AcquireSnapshot(context.Background(), "book/file", 5, source("hallo"))
	if err != nil || changed == hash {
		t.Fatalf("refresh reused unversioned bytes: %s %v", changed, err)
	}
	defer b.Release()
	readAt(t, a, "hello", 0)
	readAt(t, b, "hallo", 0)
	exact, downloaded, err := c.Acquire(context.Background(), hash, 5, nil)
	if err != nil || downloaded {
		t.Fatalf("immutable admission: %v", err)
	}
	exact.Release()
}

func TestSnapshotLimitsMismatchAndRestart(t *testing.T) {
	c, _ := cacheFor(t, 5, 5)
	var calls atomic.Int32
	src := func(context.Context, int64) (io.ReadCloser, int64, int64, error) {
		calls.Add(1)
		return nil, 0, 0, errors.New("unexpected download")
	}
	if _, _, err := c.AcquireSnapshot(context.Background(), "too-large", 6, src); !errors.Is(err, verifiedcache.ErrTooLarge) || calls.Load() != 0 {
		t.Fatalf("capacity preflight: %v", err)
	}
	if _, _, err := c.AcquireSnapshot(context.Background(), "bad-size", 4, source("hello")); !errors.Is(err, verifiedcache.ErrMismatch) {
		t.Fatalf("size: %v", err)
	}
	a, hash, err := c.AcquireSnapshot(context.Background(), "file", 5, source("hello"))
	if err != nil {
		t.Fatal(err)
	}
	a.Release()
	c.Close()
	c2, _ := cacheFor(t, 5, 5)
	if _, _, err := c2.Acquire(context.Background(), hash, 5, source("hallo")); !errors.Is(err, verifiedcache.ErrMismatch) {
		t.Fatalf("restart substituted bytes: %v", err)
	}
}

func TestSnapshotCancellationJoinsBoundedFlight(t *testing.T) {
	c, _ := cacheFor(t, 5, 5)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	src := func(ctx context.Context, off int64) (io.ReadCloser, int64, int64, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return source("hello")(ctx, off)
		case <-ctx.Done():
			return nil, 0, 0, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := c.AcquireSnapshot(ctx, "file", 5, src); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	wait, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	lease, _, err := c.AcquireSnapshot(wait, "file", 5, src)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	readAt(t, lease, "hello", 0)
	// A pinned snapshot reserves the full budget: no second download starts.
	wait2, stop2 := context.WithCancel(context.Background())
	stop2()
	if _, _, err := c.AcquireSnapshot(wait2, "other", 5, src); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCompletedUnclaimedSnapshotSurvivesCallerDeadline(t *testing.T) {
	c, _ := cacheFor(t, 5, 10)
	entered, release := make(chan struct{}), make(chan struct{})
	src := func(ctx context.Context, off int64) (io.ReadCloser, int64, int64, error) {
		close(entered)
		select {
		case <-release:
			return source("hello")(ctx, off)
		case <-ctx.Done():
			return nil, 0, 0, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan error, 1)
	go func() { _, _, err := c.AcquireSnapshot(ctx, "book/file", 5, src); waiting <- err }()
	<-entered
	published := c.SnapshotDone("book/file")
	cancel()
	if err := <-waiting; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	<-published
	lease, hash, err := c.AcquireSnapshot(context.Background(), "book/file", 5, source("hallo"))
	if err != nil || hash != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("unclaimed verified result was lost: %s %v", hash, err)
	}
	readAt(t, lease, "hello", 0)
	lease.Release()
	// Once handed to a caller, the identity no longer implies unchanged bytes.
	fresh, newHash, err := c.AcquireSnapshot(context.Background(), "book/file", 5, source("hallo"))
	if err != nil || newHash == hash {
		t.Fatalf("claimed snapshot prevented refresh: %s %v", newHash, err)
	}
	fresh.Release()
}

func TestUnknownDigestRetriesCannotSpliceProviderGenerations(t *testing.T) {
	c, _ := cacheFor(t, 6, 12)
	var offsets []int64
	src := func(ctx context.Context, off int64) (io.ReadCloser, int64, int64, error) {
		offsets = append(offsets, off)
		if len(offsets) == 1 {
			return io.NopCloser(io.MultiReader(strings.NewReader("abc"), errorReader{})), 0, 6, nil
		}
		return source("ABCXYZ")(ctx, off)
	}
	lease, hash, err := c.AcquireSnapshot(context.Background(), "file", 6, src)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	readAt(t, lease, "ABCXYZ", 0)
	if hash != digest("ABCXYZ") || fmt.Sprint(offsets) != "[0 0]" {
		t.Fatalf("unknown revision resumed across generations: digest=%s offsets=%v", hash, offsets)
	}
}
