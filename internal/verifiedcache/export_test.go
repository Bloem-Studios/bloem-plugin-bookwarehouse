package verifiedcache

import "time"

// SetTimings shortens download timeouts and retry backoff for tests.
func (c *Cache) SetTimings(download, idle, retry time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downloadTimeout, c.idleTimeout, c.retryDelay = download, idle, retry
}

// MaxDownloads exposes the concurrent download bound to tests.
const MaxDownloads = maxDownloads

// SnapshotDone captures the active snapshot's publication signal for fixtures.
func (c *Cache) SnapshotDone(identity string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flights["snapshot:"+identity].done
}
