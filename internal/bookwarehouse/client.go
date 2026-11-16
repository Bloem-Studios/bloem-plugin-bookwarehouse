// Package bookwarehouse implements the read-only Bookwarehouse HTTP API.
package bookwarehouse

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrNotFound        = errors.New("bookwarehouse: book not found")
	ErrDenied          = errors.New("bookwarehouse: access denied")
	ErrInvalidMetadata = errors.New("bookwarehouse: invalid metadata")
	ErrUnavailable     = errors.New("bookwarehouse: unavailable")
	// ErrRangeNotSatisfiable reports HTTP 416: the object is no longer than
	// the requested offset.
	ErrRangeNotSatisfiable = errors.New("bookwarehouse: range not satisfiable")
)

const (
	maxJSONBytes = 8 << 20
	maxPage      = 1<<31 - 1
	maxLimit     = 100 // Bookwarehouse's PaginationParams binding limit.
)

// Book is the validated metadata needed to identify and verify one book.
// SHA256 is canonical lowercase hexadecimal. Size is present and nonnegative.
// UpdatedAt is metadata only; it is not a download revision guarantee.
//
// A listed row that cannot be published, because it lacks integrity metadata
// or is malformed, still carries its listing key (ID and UpdatedAt) and says
// why in Problem; its other fields are unset.
type Book struct {
	ID, Title, Format, SHA256 string
	Size                      int64
	UpdatedAt                 time.Time
	Catalog                   Catalog
	Problem                   string
}

// Catalog is the book's catalog metadata as Bookwarehouse holds it. Every
// field is optional and bounded; it is display data, not identity.
type Catalog struct {
	Authors, Genres                       []string
	Description, Publisher, PublishedDate string
	Language, ISBN, Series, SeriesIndex   string
	PageCount                             int
	HasCover                              bool
	CoverID, CoverThumbhash               string
}

// Deletion is one book removed from Bookwarehouse.
type Deletion struct {
	ID, Format string
	DeletedAt  time.Time
}

// DeletionPage is one page of the deletion feed.
type DeletionPage struct {
	Deletions []Deletion
	Page      int
}

// Page contains one Bookwarehouse page with validated pagination metadata.
// Totals are advisory: Bookwarehouse counts and selects rows separately and
// may cache different pages independently. They do not describe a snapshot.
type Page struct {
	Books                               []Book
	Page, Limit, TotalItems, TotalPages int
}

// Client owns its transport and may be used concurrently.
type Client struct {
	baseURL   url.URL
	apiKey    string
	http      *http.Client
	transport *http.Transport
}

// New accepts an origin, optionally followed by a reverse-proxy path prefix.
// The API path /api/v1 is appended by the client. Plain HTTP needs explicit opt-in.
func New(baseURL, apiKey string, allowHTTP bool) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || strings.Contains(baseURL, "#") || !validBaseURL(u, allowHTTP) || len(apiKey) == 0 || len(apiKey) > 8192 || strings.TrimSpace(apiKey) != apiKey {
		return nil, ErrInvalidMetadata
	}
	for _, r := range apiKey {
		if r < 0x21 || r > 0x7e {
			return nil, ErrInvalidMetadata
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/books"
	u.RawPath = ""
	transport := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           64,
		MaxIdleConnsPerHost:    32,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		DisableCompression:     true,
	}
	return &Client{
		baseURL: *u, apiKey: apiKey, transport: transport,
		http: &http.Client{
			Transport:     transport,
			Timeout:       5 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func validBaseURL(u *url.URL, allowHTTP bool) bool {
	if u == nil || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	// Reject ambiguous proxy paths rather than letting a server normalize them
	// into a different API route. Plain prefix segments remain supported.
	escaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(u.Path, "\\") {
		return false
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	for _, r := range u.Host + u.Path {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// List asks the server for updated_at ASC, which Bookwarehouse orders by ID ASC
// within each timestamp. The client does not claim a snapshot across pages.
func (c *Client) List(ctx context.Context, format string, page, limit int) (Page, error) {
	return c.ListUpdatedSince(ctx, format, time.Time{}, page, limit)
}

// ListUpdatedSince is List restricted to books whose whole-second updated_at is
// at or after since. A zero since omits the filter. Bookwarehouse compares
// updated_after inclusively at second granularity, so callers must skip rows
// they have already seen within that second.
func (c *Client) ListUpdatedSince(ctx context.Context, format string, since time.Time, page, limit int) (Page, error) {
	return c.listFolderUpdatedSince(ctx, format, "", since, page, limit)
}

// ListFolderUpdatedSince retains the stable file identities within a directory
// scope. Returned storage keys must confirm membership at a slash boundary.
func (c *Client) ListFolderUpdatedSince(ctx context.Context, format, folder string, since time.Time, page, limit int) (Page, error) {
	if folder == "" || !ValidFolder(folder) {
		return Page{}, ErrInvalidMetadata
	}
	return c.listFolderUpdatedSince(ctx, format, folder, since, page, limit)
}

func (c *Client) listFolderUpdatedSince(ctx context.Context, format, folder string, since time.Time, page, limit int) (Page, error) {
	if !validFormat(format) || page < 1 || page > maxPage || limit < 1 || limit > maxLimit {
		return Page{}, ErrInvalidMetadata
	}
	query := url.Values{"format": {format}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}, "sort": {"updated_at"}, "order": {"asc"}}
	if folder != "" {
		query.Set("folder", folder)
	}
	if !since.IsZero() {
		query.Set("updated_after", since.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	var wire wirePage
	if err := c.readJSON(ctx, "", query, &wire); err != nil {
		return Page{}, err
	}
	p := wire.Pagination
	if wire.Books == nil || p == nil || p.Page == nil || p.Limit == nil || p.TotalItems == nil || p.TotalPages == nil {
		return Page{}, ErrInvalidMetadata
	}
	if *p.Page != page || *p.Limit != limit || *p.TotalItems < 0 || *p.TotalItems > maxPage || *p.TotalPages < 0 || *p.TotalPages > maxPage || len(wire.Books) > limit {
		return Page{}, ErrInvalidMetadata
	}
	expectedPages := *p.TotalItems / limit
	if *p.TotalItems%limit != 0 {
		expectedPages++
	}
	if *p.TotalPages != expectedPages {
		return Page{}, ErrInvalidMetadata
	}
	result := Page{Books: make([]Book, 0, len(wire.Books)), Page: page, Limit: limit, TotalItems: *p.TotalItems, TotalPages: *p.TotalPages}
	seen := make(map[string]bool, len(wire.Books))
	for _, raw := range wire.Books {
		if folder != "" {
			var scope struct {
				Path string `json:"storage_path"`
			}
			if json.Unmarshal(raw, &scope) != nil || !validRelativePath(scope.Path, 64<<10) || !strings.HasPrefix(scope.Path, folder+"/") {
				return Page{}, ErrInvalidMetadata
			}
		}
		book, err := listedBook(raw, format)
		if err != nil || seen[book.ID] {
			return Page{}, ErrInvalidMetadata
		}
		seen[book.ID] = true
		result.Books = append(result.Books, book)
	}
	return result, nil
}

// listedBook validates one listed row. Only a row without a usable listing
// key, or of another format than requested, fails the page: a key is what
// keeps a listing ordered and resumable. A row that merely cannot be
// published is returned with its key and a Problem.
func listedBook(raw json.RawMessage, format string) (Book, error) {
	var key struct {
		ID        string    `json:"id"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if json.Unmarshal(raw, &key) != nil || !validID(key.ID) || !validTimestamp(key.UpdatedAt) {
		return Book{}, ErrInvalidMetadata
	}
	skipped := func(problem string) (Book, error) {
		return Book{ID: key.ID, UpdatedAt: key.UpdatedAt, Problem: problem}, nil
	}
	var wire wireBook
	if json.Unmarshal(raw, &wire) != nil {
		return skipped("malformed metadata")
	}
	if wire.Format != nil && *wire.Format != format {
		return Book{}, ErrInvalidMetadata
	}
	book, err := wire.validate()
	if err != nil {
		return skipped("missing or invalid file_format, file_hash or file_size")
	}
	return book, nil
}

func validTimestamp(t time.Time) bool {
	return !t.IsZero() && time.Unix(0, t.UnixNano()).Equal(t)
}

// Deletions lists books deleted at or after since, oldest first.
// AudiobookDeletions reads one page of Bookwarehouse's audiobook deletion
// feed from since, truncated to the second.
func (c *Client) AudiobookDeletions(ctx context.Context, since time.Time, page, limit int) (DeletionPage, error) {
	if page < 1 || page > maxPage || limit < 1 || limit > maxLimit {
		return DeletionPage{}, ErrInvalidMetadata
	}
	query := url.Values{"since": {since.UTC().Truncate(time.Second).Format(time.RFC3339)}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	var wire struct {
		Deletions []struct {
			ID        string    `json:"audiobook_id"`
			DeletedAt time.Time `json:"deleted_at"`
		} `json:"deletions"`
		Pagination *struct {
			Page *int `json:"page"`
		} `json:"pagination"`
	}
	a := c.audioClient()
	if err := a.readJSON(ctx, "/deletions", query, &wire); err != nil {
		return DeletionPage{}, err
	}
	if wire.Deletions == nil || wire.Pagination == nil || wire.Pagination.Page == nil || *wire.Pagination.Page != page || len(wire.Deletions) > limit {
		return DeletionPage{}, ErrInvalidMetadata
	}
	out := DeletionPage{Page: page, Deletions: make([]Deletion, 0, len(wire.Deletions))}
	for _, d := range wire.Deletions {
		if !validID(d.ID) || d.DeletedAt.IsZero() {
			return DeletionPage{}, ErrInvalidMetadata
		}
		out.Deletions = append(out.Deletions, Deletion{ID: d.ID, Format: "audiobook", DeletedAt: d.DeletedAt})
	}
	return out, nil
}

func (c *Client) Deletions(ctx context.Context, since time.Time, page, limit int) (DeletionPage, error) {
	if page < 1 || page > maxPage || limit < 1 || limit > maxLimit {
		return DeletionPage{}, ErrInvalidMetadata
	}
	query := url.Values{"since": {since.UTC().Truncate(time.Second).Format(time.RFC3339)}, "page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}}
	var wire struct {
		Deletions []struct {
			BookID     string    `json:"book_id"`
			FileFormat *string   `json:"file_format"`
			DeletedAt  time.Time `json:"deleted_at"`
		} `json:"deletions"`
		Pagination *struct {
			Page *int `json:"page"`
		} `json:"pagination"`
	}
	if err := c.readJSON(ctx, "/deletions", query, &wire); err != nil {
		return DeletionPage{}, err
	}
	if wire.Deletions == nil || wire.Pagination == nil || wire.Pagination.Page == nil || *wire.Pagination.Page != page || len(wire.Deletions) > limit {
		return DeletionPage{}, ErrInvalidMetadata
	}
	out := DeletionPage{Page: page, Deletions: make([]Deletion, 0, len(wire.Deletions))}
	for _, d := range wire.Deletions {
		if !validID(d.BookID) || d.DeletedAt.IsZero() {
			return DeletionPage{}, ErrInvalidMetadata
		}
		// Unknown formats were never published. Keep their rows so callers
		// still see a full page and continue to later supported tombstones.
		format := ""
		if d.FileFormat != nil {
			format = *d.FileFormat
		}
		out.Deletions = append(out.Deletions, Deletion{ID: d.BookID, Format: format, DeletedAt: d.DeletedAt})
	}
	return out, nil
}

// maxCoverBytes bounds a cover download; Bookwarehouse's medium covers are
// far smaller.
const maxCoverBytes = 16 << 20

// Cover downloads the book's medium-size cover image whole.
func (c *Client) Cover(ctx context.Context, id string) ([]byte, string, error) {
	if !validID(id) {
		return nil, "", ErrInvalidMetadata
	}
	return c.cover(ctx, "/"+id+"/cover/medium")
}

func (c *Client) cover(ctx context.Context, suffix string) ([]byte, string, error) {
	response, err := c.request(ctx, suffix, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.ContentLength > maxCoverBytes {
		return nil, "", ErrInvalidMetadata
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxCoverBytes+1))
	if err != nil {
		return nil, "", safeError(ctx, err)
	}
	if len(data) == 0 || len(data) > maxCoverBytes {
		return nil, "", ErrInvalidMetadata
	}
	return data, response.Header.Get("Content-Type"), nil
}

// Get decodes the raw database.Book returned by the actual GetBook handler.
func (c *Client) Get(ctx context.Context, id string) (Book, error) {
	if !validID(id) {
		return Book{}, ErrInvalidMetadata
	}
	var wire wireBook
	if err := c.readJSON(ctx, "/"+id, nil, &wire); err != nil {
		return Book{}, err
	}
	book, err := wire.validate()
	if err != nil || book.ID != id {
		return Book{}, ErrInvalidMetadata
	}
	return book, nil
}

// Body is a download response body. Offset is the absolute position of its
// first byte; Total is the object size the server declared, or -1 when it
// declared none.
type Body struct {
	io.ReadCloser
	Offset, Total int64
}

// Download returns the book's bytes from offset. Offset 0 asks for the whole
// object with a plain GET. A positive offset sends a single open-ended Range
// and accepts 206 only with a Content-Range that starts at offset and runs to
// the declared end. A server that ignores the range answers 200 with the whole
// object, reported as Offset 0, so callers restart. No revision conditions are
// sent because Bookwarehouse does not honor them: callers must verify the
// complete object's exact size and SHA256 before exposing any bytes.
func (c *Client) Download(ctx context.Context, id string, offset int64) (*Body, error) {
	if !validID(id) || offset < 0 {
		return nil, ErrInvalidMetadata
	}
	return c.download(ctx, "/"+id+"/download", nil, offset)
}

func (c *Client) download(ctx context.Context, suffix string, query url.Values, offset int64) (*Body, error) {
	header := http.Header{}
	accept := []int{http.StatusOK}
	if offset > 0 {
		header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		accept = append(accept, http.StatusPartialContent)
	}
	response, err := c.request(ctx, suffix, query, header, accept...)
	if err != nil {
		return nil, err
	}
	body := &Body{ReadCloser: &safeBody{body: response.Body, ctx: ctx}, Total: max(response.ContentLength, -1)}
	if response.StatusCode == http.StatusPartialContent {
		total, ok := contentRange(response.Header.Get("Content-Range"), offset)
		if !ok || (response.ContentLength >= 0 && response.ContentLength != total-offset) {
			response.Body.Close()
			return nil, ErrUnavailable
		}
		body.Offset, body.Total = offset, total
	}
	return body, nil
}

// Authorize checks that the API key may still download book id, without
// transferring the book: it requests one byte. 206, a 200 from a server that
// ignores ranges (closed unread), and 416 for an empty book all show access.
func (c *Client) Authorize(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrInvalidMetadata
	}
	response, err := c.request(ctx, "/"+id+"/download", nil, http.Header{"Range": {"bytes=0-0"}},
		http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable)
	if err != nil {
		return err
	}
	if err := response.Body.Close(); err != nil {
		return safeError(ctx, err)
	}
	return nil
}

// contentRange parses "bytes <start>-<total-1>/<total>" for a range that must
// start at start and run to the end of a declared total.
func contentRange(value string, start int64) (int64, bool) {
	a, b, total, ok := contentSpan(value)
	if !ok || a != start || b != total-1 {
		return 0, false
	}
	return total, true
}

// contentSpan parses "bytes first-last/total" for a span inside the object.
func contentSpan(value string) (first, last, total int64, ok bool) {
	spec, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return 0, 0, 0, false
	}
	span, size, ok := strings.Cut(spec, "/")
	a, b, found := strings.Cut(span, "-")
	if !ok || !found {
		return 0, 0, 0, false
	}
	first, errA := strconv.ParseInt(a, 10, 64)
	last, errB := strconv.ParseInt(b, 10, 64)
	total, errT := strconv.ParseInt(size, 10, 64)
	if errA != nil || errB != nil || errT != nil || first < 0 || last < first || total <= last {
		return 0, 0, 0, false
	}
	return first, last, total, true
}

// Close releases idle connections without interrupting active requests.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// request sends an authenticated GET and returns a response whose status is
// one of accept (200 when none is given). Every other status is classified
// without exposing response details.
func (c *Client) request(ctx context.Context, suffix string, query url.Values, header http.Header, accept ...int) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(accept) == 0 {
		accept = []int{http.StatusOK}
	}
	endpoint := c.baseURL
	endpoint.Path += suffix
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrUnavailable
	}
	for key, values := range header {
		request.Header[key] = values
	}
	request.Header.Set("X-API-Key", c.apiKey)
	request.Header.Set("Accept-Encoding", "identity")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, safeError(ctx, err)
	}
	if slices.Contains(accept, response.StatusCode) {
		return response, nil
	}
	response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrDenied
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusRequestedRangeNotSatisfiable:
		if header.Get("Range") != "" {
			return nil, ErrRangeNotSatisfiable
		}
		return nil, ErrUnavailable
	default:
		return nil, ErrUnavailable
	}
}

func (c *Client) readJSON(ctx context.Context, suffix string, query url.Values, destination any) error {
	response, err := c.request(ctx, suffix, query, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength > maxJSONBytes {
		return ErrInvalidMetadata
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBytes+1))
	if err != nil {
		return safeError(ctx, err)
	}
	if len(data) > maxJSONBytes {
		return ErrInvalidMetadata
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return ErrInvalidMetadata
	}
	return nil
}

// Pointer fields distinguish absent/null integrity metadata from a valid zero.
type wireBook struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Format    *string   `json:"file_format"`
	SHA256    *string   `json:"file_hash"`
	Size      *int64    `json:"file_size"`
	UpdatedAt time.Time `json:"updated_at"`

	Author         *string    `json:"author"`
	Authors        []wireName `json:"authors"`
	Genres         []wireName `json:"genres"`
	Description    *string    `json:"description"`
	Publisher      *string    `json:"publisher"`
	PublishedDate  *string    `json:"published_date"`
	Language       string     `json:"language"`
	ISBN13         *string    `json:"isbn13"`
	ISBN10         *string    `json:"isbn10"`
	SeriesName     *string    `json:"series_name"`
	SeriesIndex    *float64   `json:"series_index"`
	PageCount      *int       `json:"page_count"`
	HasCover       bool       `json:"has_cover"`
	CoverID        string     `json:"cover_id"`
	CoverThumbhash *string    `json:"cover_thumbhash"`
}

type wireName struct {
	Name string `json:"name"`
}

const (
	maxCatalogText        = 4 << 10
	maxCatalogDescription = 64 << 10
	maxCatalogList        = 64
)

// catalog keeps the optional metadata, dropping any value over its bound
// rather than failing the book: identity and integrity fields are validated
// separately and strictly.
func (w wireBook) catalog() Catalog {
	text := func(p *string, limit int) string {
		if p == nil || len(*p) > limit || !utf8.ValidString(*p) {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	names := func(values []wireName) []string {
		out := make([]string, 0, min(len(values), maxCatalogList))
		for _, v := range values {
			if len(out) == maxCatalogList {
				break
			}
			if name := text(&v.Name, maxCatalogText); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	c := Catalog{
		Authors:        names(w.Authors),
		Genres:         names(w.Genres),
		Description:    text(w.Description, maxCatalogDescription),
		Publisher:      text(w.Publisher, maxCatalogText),
		PublishedDate:  text(w.PublishedDate, maxCatalogText),
		Language:       text(&w.Language, maxCatalogText),
		ISBN:           text(w.ISBN13, maxCatalogText),
		Series:         text(w.SeriesName, maxCatalogText),
		HasCover:       w.HasCover,
		CoverThumbhash: text(w.CoverThumbhash, maxCatalogText),
	}
	if c.ISBN == "" {
		c.ISBN = text(w.ISBN10, maxCatalogText)
	}
	if len(c.Authors) == 0 {
		if author := text(w.Author, maxCatalogText); author != "" {
			c.Authors = []string{author}
		}
	}
	if w.SeriesIndex != nil && c.Series != "" {
		c.SeriesIndex = strconv.FormatFloat(*w.SeriesIndex, 'f', -1, 64)
	}
	if w.PageCount != nil && *w.PageCount > 0 {
		c.PageCount = *w.PageCount
	}
	if c.HasCover && validID(w.CoverID) {
		c.CoverID = w.CoverID
	} else {
		c.HasCover = false
	}
	return c
}

type wirePage struct {
	Books      []json.RawMessage `json:"books"`
	Pagination *struct {
		Page       *int `json:"page"`
		Limit      *int `json:"limit"`
		TotalItems *int `json:"total_items"`
		TotalPages *int `json:"total_pages"`
	} `json:"pagination"`
}

// maxTitle bounds a title. A longer one is display data, so it is truncated
// rather than failing the book.
const maxTitle = 4096

func (w wireBook) validate() (Book, error) {
	if !validID(w.ID) || w.Format == nil || !validFormat(*w.Format) || w.SHA256 == nil || len(*w.SHA256) != 64 || w.Size == nil || *w.Size < 0 || !validTimestamp(w.UpdatedAt) {
		return Book{}, ErrInvalidMetadata
	}
	if _, err := hex.DecodeString(*w.SHA256); err != nil {
		return Book{}, ErrInvalidMetadata
	}
	return Book{ID: w.ID, Title: truncate(w.Title, maxTitle), Format: *w.Format, SHA256: strings.ToLower(*w.SHA256), Size: *w.Size, UpdatedAt: w.UpdatedAt, Catalog: w.catalog()}, nil
}

// truncate shortens s to at most limit bytes at a UTF-8 boundary.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func validFormat(format string) bool { return format == "epub" || format == "pdf" }

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func safeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrUnavailable
}

type safeBody struct {
	body io.ReadCloser
	ctx  context.Context
}

func (b *safeBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && err != io.EOF {
		return n, safeError(b.ctx, err)
	}
	return n, err
}
func (b *safeBody) Close() error {
	if err := b.body.Close(); err != nil {
		return safeError(b.ctx, err)
	}
	return nil
}
