package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	backendPageSize   = 100
	cursorVersion     = 3
	maxFetchesPerList = 8
	changeOverlap     = 10 * time.Minute
	fullScanInterval  = 7 * 24 * time.Hour
	// maxPageBytes bounds the encoded entries of one List reply. The host
	// rejects replies over 1 MiB; the rest is room for the cursor, change
	// token and envelope. The largest entry the catalog bounds permit is far
	// smaller, so a page always holds at least one entry.
	maxPageBytes = 1<<20 - 64<<10
)

// entryBytes is e's encoded size as a ListResponse entries field.
func entryBytes(e *storagev1.Entry) int {
	return protowire.SizeTag(1) + protowire.SizeBytes(proto.Size(e))
}

// cursor is a keyset position over Bookwarehouse's (whole-second updated_at, id)
// ordering. Bookwarehouse caches each list page independently and has no
// snapshot reads, so page offsets over a live catalog drift between requests.
// Keys do not: each request filters by updated_after=AfterSec and skips rows at
// or before (AfterSec, AfterID), so a stale or shifted page can repeat rows but
// never makes the listing fail or emit an entry twice.
//
// Upper is fixed when a directory listing starts. Books updated after it are
// left for the next scan; otherwise a book updated mid-listing would reappear
// later in the same listing under its new timestamp.
//
// Page counts pages within the current AfterSec filter. It only exceeds 1 while
// more than one backend page of books share the AfterSec second.
//
// An incremental listing (see changeToken) starts its filter at Since instead
// of the beginning of the catalog, and after the books moves to a deletions
// phase that pages through Bookwarehouse's deletion feed from Since.
type cursor struct {
	Browse    bool   `json:"b,omitempty"`
	Version   int    `json:"v"`
	Directory string `json:"d"`
	Page      int    `json:"p"`
	Offset    int    `json:"o,omitempty"`
	Upper     int64  `json:"u,omitempty"`
	AfterSec  int64  `json:"s,omitempty"`
	AfterID   string `json:"a,omitempty"`
	Since     int64  `json:"c,omitempty"`
	FullAt    int64  `json:"f,omitempty"`
	Phase     int    `json:"h,omitempty"`
}

const (
	phaseBooks     = 0
	phaseDeletions = 1
)

// changeToken retains the last listing's upper bound and the original full
// scan's start. Delta queries overlap that bound to catch late commits and
// modest clock skew; the full-scan anchor expires independently of delta scans.
type changeToken struct {
	Version   int    `json:"v"`
	Directory string `json:"d"`
	Since     int64  `json:"s"`
	FullAt    int64  `json:"f,omitempty"`
}

func sign(s *state, raw []byte) string {
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verified returns the payload of a token signed under the current
// configuration's key, or false.
func verified(s *state, token string, limit int) ([]byte, bool) {
	if len(token) > limit {
		return nil, false
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.key[:])
	_, _ = mac.Write(raw)
	return raw, hmac.Equal(signature, mac.Sum(nil))
}

func encodeChangeToken(s *state, dir string, since, fullAt int64) string {
	raw, _ := json.Marshal(changeToken{Version: 2, Directory: dir, Since: since, FullAt: fullAt})
	return sign(s, raw)
}

// decodeChangeToken refuses a token from another directory or configuration
// with FailedPrecondition, which tells the host to list the directory fully.
func decodeChangeToken(s *state, dir, token string) (changeToken, error) {
	refused := status.Error(codes.FailedPrecondition, "change token not recognized; list fully")
	raw, ok := verified(s, token, 1024)
	if !ok {
		return changeToken{}, refused
	}
	var t changeToken
	now := time.Now().Unix()
	if json.Unmarshal(raw, &t) != nil || (t.Version != 1 && t.Version != 2) || t.Directory != dir || t.Since <= 0 || t.Since > now {
		return changeToken{}, refused
	}
	if t.Version == 1 {
		// Old tokens only carried the last upper bound. Start their full-scan
		// deadline from that boundary when upgrading to the anchored format.
		t.FullAt = t.Since
	}
	if t.FullAt <= 0 || t.FullAt > t.Since || now-t.FullAt >= int64(fullScanInterval/time.Second) {
		return changeToken{}, refused
	}
	return t, nil
}

func encodeCursor(s *state, c cursor) string {
	raw, _ := json.Marshal(c)
	return sign(s, raw)
}
func decodeCursor(s *state, dir, token string) (cursor, error) {
	invalid := status.Error(codes.InvalidArgument, "invalid continuation cursor")
	var c cursor
	raw, ok := verified(s, token, 16384)
	if !ok || json.Unmarshal(raw, &c) != nil ||
		c.Version != cursorVersion || c.Directory != dir || c.Page < 1 || c.Page > 21474836 || c.Offset < 0 || c.Offset >= backendPageSize {
		return c, invalid
	}
	if dir != "root" && (c.Offset != 0 || c.Upper <= 0 || c.AfterSec < 0 || c.AfterSec > c.Upper || c.Since < 0 || c.Since > c.Upper || c.FullAt < 0 || c.FullAt > c.Upper ||
		(c.Phase != phaseBooks && c.Phase != phaseDeletions) || (c.Phase == phaseDeletions && c.Since == 0) ||
		(c.Phase == phaseBooks && c.AfterID == "" && (c.AfterSec != 0 || c.Page != 1)) || (c.AfterID != "" && !validID(c.AfterID))) {
		return c, invalid
	}
	return c, nil
}

// after reports whether book b sorts strictly after the key (sec, id).
func after(b bookwarehouse.Book, sec int64, id string) bool {
	return b.UpdatedAt.Unix() > sec || (b.UpdatedAt.Unix() == sec && b.ID > id)
}

// maxListReplyWait is how long before its deadline a listing stops fetching
// backend pages, so it can reply with the books it already has and a cursor.
// The host's RPC deadline would otherwise fail the call and discard them.
const maxListReplyWait = 2 * time.Second

// listBudget is the time a listing may spend fetching backend pages. A fetch
// starts only while the budget still covers the slowest fetch so far, and once
// an earlier fetch has advanced the cursor, running out of budget ends the
// page early rather than failing it.
type listBudget struct {
	ctx     context.Context
	cancel  context.CancelFunc
	slowest time.Duration
}

func newListBudget(ctx context.Context) *listBudget {
	b := &listBudget{}
	if deadline, ok := ctx.Deadline(); ok {
		b.ctx, b.cancel = context.WithDeadline(ctx, deadline.Add(-min(maxListReplyWait, time.Until(deadline)/4)))
	} else {
		b.ctx, b.cancel = context.WithCancel(ctx)
	}
	return b
}

// fits reports whether another fetch is likely to finish within the budget.
func (b *listBudget) fits() bool {
	deadline, ok := b.ctx.Deadline()
	return !ok || time.Until(deadline) > b.slowest
}

// fetch runs one backend fetch within the budget. It reports false without an
// error when the budget ran out during a fetch after the page's first while
// the call itself still has time; the page then ends at the cursor the earlier
// fetches advanced.
func (b *listBudget) fetch(ctx context.Context, first bool, fetch func(context.Context) error) (bool, error) {
	started := time.Now()
	if err := fetch(b.ctx); err != nil {
		if !first && ctx.Err() == nil && b.ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	b.slowest = max(b.slowest, time.Since(started))
	return true, nil
}

func fetchPage(ctx context.Context, s *state, format, folder string, since time.Time, num int) (bookwarehouse.Page, error) {
	var p bookwarehouse.Page
	var err error
	if folder == "" {
		p, err = s.client.ListUpdatedSince(ctx, format, since, num, backendPageSize)
	} else {
		p, err = s.client.ListFolderUpdatedSince(ctx, format, folder, since, num, backendPageSize)
	}
	if errors.Is(err, bookwarehouse.ErrNotFound) {
		return p, status.Error(codes.Unavailable, "Bookwarehouse listing unavailable")
	}
	if err != nil {
		return p, mapError(ctx, err)
	}
	if p.Page != num || p.Limit != backendPageSize || len(p.Books) > backendPageSize {
		return p, status.Error(codes.Unavailable, "invalid Bookwarehouse pagination")
	}
	seen := map[string]bool{}
	for i, b := range p.Books {
		if (b.Problem == "" && b.Format != format) || seen[b.ID] || (!since.IsZero() && b.UpdatedAt.Unix() < since.Unix()) {
			return p, status.Error(codes.Unavailable, "invalid Bookwarehouse listing")
		}
		seen[b.ID] = true
		if i > 0 && !after(b, p.Books[i-1].UpdatedAt.Unix(), p.Books[i-1].ID) {
			return p, status.Error(codes.Unavailable, "unstable Bookwarehouse ordering")
		}
	}
	return p, nil
}
func (p *Provider) List(ctx context.Context, r *storagev1.ListRequest) (*storagev1.ListResponse, error) {
	if r.GetSourceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source required")
	}
	s, ctx, done, err := p.begin(ctx, r.GetSourceId())
	if err != nil {
		return nil, err
	}
	defer done()
	if s.config.Content == "audiobooks" {
		return listAudio(ctx, s, r)
	}
	dir := r.GetDirectoryId()
	format, folder := dir, ""
	if strings.HasPrefix(dir, "folders/") {
		format, folder, err = splitFolder(dir)
		if err != nil {
			return nil, err
		}
	} else if dir != "root" && dir != "epub" && dir != "pdf" {
		return nil, status.Error(codes.InvalidArgument, "invalid directory identity")
	}
	if r.GetDirectoriesOnly() && dir != "root" {
		return listFolders(ctx, s, r, format, folder)
	}
	if folder != "" {
		if r.GetChangesSince() != "" {
			return nil, status.Error(codes.FailedPrecondition, "directory scope requires full inventory")
		}
		if err := currentFolder(ctx, s, format, folder); err != nil {
			return nil, err
		}
	}
	limit := int(r.GetMaxEntries())
	if limit == 0 {
		limit = 100
	}
	limit = min(limit, 512)
	c := cursor{Version: cursorVersion, Directory: dir, Page: 1, Browse: r.GetDirectoriesOnly()}
	if r.GetCursor() != "" {
		c, err = decodeCursor(s, dir, r.Cursor)
		if err != nil {
			return nil, err
		}
		if c.Browse != r.GetDirectoriesOnly() {
			return nil, status.Error(codes.InvalidArgument, "continuation intent changed")
		}
	} else if r.GetChangesSince() != "" {
		if r.GetDirectoriesOnly() {
			return nil, status.Error(codes.FailedPrecondition, "browse does not accept change tokens")
		}
		if dir == "root" {
			return nil, status.Error(codes.FailedPrecondition, "change token not recognized; list fully")
		}
		token, err := decodeChangeToken(s, dir, r.GetChangesSince())
		if err != nil {
			return nil, err
		}
		c.Since = max(1, token.Since-int64(changeOverlap/time.Second))
		c.FullAt = token.FullAt
	}
	out := &storagev1.ListResponse{}
	if dir == "root" {
		if c.Page != 1 || c.Offset > 1 {
			return nil, status.Error(codes.InvalidArgument, "invalid continuation cursor")
		}
		dirs := []string{"epub", "pdf"}
		end := min(c.Offset+limit, len(dirs))
		for _, id := range dirs[c.Offset:end] {
			out.Entries = append(out.Entries, directory(id))
		}
		out.Complete = end == len(dirs)
		if !out.Complete {
			c.Offset = end
			out.NextCursor = encodeCursor(s, c)
		}
		return out, nil
	}
	if c.Upper == 0 {
		c.Upper = time.Now().Unix()
	}
	if c.FullAt == 0 {
		// Version-3 continuation cursors predate the optional full-scan anchor.
		// Finish them in place; only a new scan can safely add query overlap.
		c.FullAt = c.Upper
		if c.Since > 0 {
			c.FullAt = c.Since
		}
	}
	if c.Phase == phaseDeletions {
		return p.listDeletions(ctx, s, dir, c)
	}
	// Fill the request from a few backend pages. A page that starts a new filter
	// second usually repeats rows already listed, so one fetch per call would
	// often return nothing. The page ends early when its encoded entries would
	// exceed maxPageBytes, or when the call's deadline leaves no time for
	// another fetch; the cursor then resumes after the last entry sent.
	budget := newListBudget(ctx)
	defer budget.cancel()
	pageBytes, full := 0, false
	for fetch := 0; fetch < maxFetchesPerList && !out.Complete && !full && len(out.Entries) < limit; fetch++ {
		if fetch > 0 && !budget.fits() {
			break
		}
		var since time.Time
		if c.AfterID != "" {
			since = time.Unix(c.AfterSec, 0)
		} else if c.Since > 0 {
			since = time.Unix(c.Since, 0)
		}
		var page bookwarehouse.Page
		fetched, err := budget.fetch(ctx, fetch == 0, func(ctx context.Context) (err error) {
			page, err = fetchPage(ctx, s, format, folder, since, c.Page)
			return err
		})
		if err != nil {
			return nil, err
		}
		if !fetched {
			break
		}
		next := c
		consumed, reachedUpper := true, false
		for _, b := range page.Books {
			if b.UpdatedAt.Unix() > c.Upper {
				reachedUpper = true
				break
			}
			if c.AfterID != "" && !after(b, c.AfterSec, c.AfterID) {
				continue
			}
			if b.Problem != "" {
				// The client validated the listing key, but this row cannot be
				// pinned. Consume its key so it cannot wedge this or later pages.
				next.AfterSec, next.AfterID = b.UpdatedAt.Unix(), b.ID
				continue
			}
			e := bookEntry(b)
			size := entryBytes(e)
			if len(out.Entries) == limit || (len(out.Entries) > 0 && pageBytes+size > maxPageBytes) {
				consumed, full = false, true
				break
			}
			out.Entries = append(out.Entries, e)
			pageBytes += size
			next.AfterSec, next.AfterID = b.UpdatedAt.Unix(), b.ID
		}
		// Totals are advisory and may come from a differently cached count, so
		// only an actual short page, fully consumed, ends the listing.
		out.Complete = reachedUpper || (consumed && len(page.Books) < backendPageSize)
		switch {
		case next.AfterSec != c.AfterSec || c.AfterID == "":
			// A new filter second starts again at its first page.
			next.Page = 1
		case consumed:
			// The whole page shared the filter second; continue within it.
			next.Page++
		}
		c = next
	}
	if out.Complete && c.Since > 0 {
		// An incremental listing reports deletions after its books.
		out.Complete = false
		c.Phase, c.Page = phaseDeletions, 1
	}
	if out.Complete {
		if folder == "" {
			out.ChangeToken = encodeChangeToken(s, dir, c.Upper, c.FullAt)
		}
	} else {
		out.NextCursor = encodeCursor(s, c)
	}
	if proto.Size(out) > 1<<20 {
		// Unreachable within maxPageBytes; never send the host an oversized page.
		return nil, status.Error(codes.Internal, "listing response too large")
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return out, nil
}

// listDeletions reports one page of books of this directory's format deleted
// since the incremental listing's Since.
func (p *Provider) listDeletions(ctx context.Context, s *state, dir string, c cursor) (*storagev1.ListResponse, error) {
	page, err := s.client.Deletions(ctx, time.Unix(c.Since, 0), c.Page, backendPageSize)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	out := &storagev1.ListResponse{}
	for _, d := range page.Deletions {
		if d.Format == dir {
			out.RemovedEntryIds = append(out.RemovedEntryIds, dir+"/"+d.ID)
		}
	}
	if len(page.Deletions) < backendPageSize {
		out.Complete = true
		out.ChangeToken = encodeChangeToken(s, dir, c.Upper, c.FullAt)
	} else {
		c.Page++
		out.NextCursor = encodeCursor(s, c)
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return out, nil
}
