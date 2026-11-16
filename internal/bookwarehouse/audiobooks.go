package bookwarehouse

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// AudioFile identifies one provider-owned track. Evidence digests how
// Bookwarehouse describes its bytes: the size, storage path and sampled folder
// file_hash. It changes when Bookwarehouse replaces the file, but the sampled
// hash cannot prove that every byte is unchanged.
type AudioFile struct {
	ID                            int64
	Size                          int64
	Evidence                      string
	Disc, Track                   uint32
	Duration                      float64
	Codec, Container              string
	Bitrate, SampleRate, Channels uint32
	Chapters                      []AudioChapter
}
type AudioChapter struct {
	ID, Title  string
	Start, End float64
}
type Audiobook struct {
	Book      Book
	Narrators []string
	Files     []AudioFile
}
type AudioPage struct {
	Books       []Audiobook
	Page, Limit int
}

// Collection is one spelling of a collection: Spelling is the provider's
// exact name, Name its display text.
type Collection struct{ ID, Name, Spelling string }
type CollectionPage struct {
	Collections []Collection
	Page, Limit int
}

const (
	// A continuation carries the last listed spelling.
	MaxCollectionSpelling  = 1 << 10
	maxCollectionSpellings = 256
)

type wireAudioBook struct {
	wireBook
	Narrators []wireName `json:"narrators"`
	Files     []struct {
		ID         int64    `json:"id"`
		BookID     string   `json:"audiobook_id"`
		Path       string   `json:"file_path"`
		Size       *int64   `json:"file_size"`
		Hash       *string  `json:"file_hash"`
		Disc       int64    `json:"disc_num"`
		Track      int64    `json:"track_num"`
		Duration   *float64 `json:"duration_seconds"`
		Codec      string   `json:"codec"`
		Bitrate    int64    `json:"bitrate"`
		SampleRate int64    `json:"sample_rate"`
		Channels   int64    `json:"channels"`
	} `json:"files"`
	Chapters []struct {
		ID       int64   `json:"id"`
		BookID   string  `json:"audiobook_id"`
		FileID   *int64  `json:"file_id"`
		Title    string  `json:"title"`
		Start    float64 `json:"start_seconds"`
		End      float64 `json:"end_seconds"`
		Accurate bool    `json:"is_accurate"`
	} `json:"chapters"`
}

func audioText(s string, max int) string {
	if len(s) > max || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	return strings.TrimSpace(s)
}

func (w wireAudioBook) metadata() (Audiobook, error) {
	if !validID(w.ID) || !validTimestamp(w.UpdatedAt) {
		return Audiobook{}, ErrInvalidMetadata
	}
	b := Audiobook{Book: Book{ID: w.ID, Title: audioText(w.Title, maxTitle), UpdatedAt: w.UpdatedAt, Catalog: w.catalog()}}
	for _, n := range w.Narrators {
		if name := audioText(n.Name, maxCatalogText); name != "" && len(b.Narrators) < maxCatalogList {
			b.Narrators = append(b.Narrators, name)
		}
	}
	return b, nil
}

// maxAudioChapters is the most chapters the host accepts on one track. A
// whole Bible read as one file has about 1,200.
const maxAudioChapters = 10000

func (w wireAudioBook) validateAudio() (Audiobook, error) {
	b, err := w.metadata()
	if err != nil {
		return b, err
	}
	if len(w.Files) == 0 || len(w.Files) > 512 {
		return Audiobook{}, ErrInvalidMetadata
	}
	files := map[int64]int{}
	validUint := func(n int64) bool { return n >= 0 && n <= math.MaxUint32 }
	for _, f := range w.Files {
		if f.ID <= 0 || f.BookID != w.ID || f.Size == nil || *f.Size < 0 || !validUint(f.Disc) || !validUint(f.Track) || !validUint(f.Bitrate) || !validUint(f.SampleRate) || !validUint(f.Channels) {
			return Audiobook{}, ErrInvalidMetadata
		}
		if _, exists := files[f.ID]; exists {
			return Audiobook{}, ErrInvalidMetadata
		}
		duration := float64(0)
		if f.Duration != nil {
			duration = *f.Duration
			if math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 {
				return Audiobook{}, ErrInvalidMetadata
			}
		}
		container := strings.ToLower(strings.TrimPrefix(path.Ext(f.Path), "."))
		if !slices.Contains([]string{"m4b", "mp3", "m4a", "aac", "flac", "ogg", "opus", "wav", "aiff", "aif"}, container) {
			return Audiobook{}, ErrInvalidMetadata
		}
		files[f.ID] = len(b.Files)
		hash := ""
		if f.Hash != nil {
			hash = *f.Hash
		}
		evidence := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%q\x00%q", w.ID, f.ID, *f.Size, f.Path, hash))
		b.Files = append(b.Files, AudioFile{ID: f.ID, Size: *f.Size, Evidence: hex.EncodeToString(evidence[:]), Disc: uint32(f.Disc), Track: uint32(f.Track), Duration: duration, Codec: audioText(f.Codec, maxCatalogText), Container: container, Bitrate: uint32(f.Bitrate), SampleRate: uint32(f.SampleRate), Channels: uint32(f.Channels)})
	}
	// Chapters are optional. A book with more than a track may carry keeps
	// none, and a chapter whose file or timing does not fit is left out, so
	// neither withholds the book.
	listed := w.Chapters
	if len(listed) > maxAudioChapters {
		listed = nil
	}
	chapters := map[int64]bool{}
	for _, ch := range listed {
		// Bookwarehouse lists a file's embedded chapters without a file
		// association and never marks them accurate. A book of one file can
		// only have that file's, so they are kept as its chapters, as a probe
		// of the file would read them; on a book of several their offsets are
		// ambiguous. Nearly every Bookwarehouse audiobook is one file whose only
		// chapters are these.
		fileID, embedded := ch.FileID, ch.FileID == nil
		if embedded {
			if len(b.Files) != 1 {
				continue
			}
			fileID = &b.Files[0].ID
		}
		index, ok := files[*fileID]
		if !ok || ch.ID <= 0 || chapters[ch.ID] || ch.BookID != w.ID || math.IsNaN(ch.Start) || math.IsNaN(ch.End) || math.IsInf(ch.Start, 0) || math.IsInf(ch.End, 0) || ch.Start < 0 || ch.End < ch.Start {
			continue
		}
		f := &b.Files[index]
		// A file's last chapter often ends a little past its listed duration,
		// which is measured separately; it ends with the file instead. A
		// chapter that starts at or after the file's end is left out.
		if f.Duration > 0 && ch.End > f.Duration {
			if ch.Start >= f.Duration {
				continue
			}
			ch.End = f.Duration
		}
		chapters[ch.ID] = true
		if (ch.Accurate || embedded) && f.Duration > 0 {
			f.Chapters = append(f.Chapters, AudioChapter{ID: strconv.FormatInt(ch.ID, 10), Title: audioText(ch.Title, maxCatalogText), Start: ch.Start, End: ch.End})
		}
	}
	for i := range b.Files {
		slices.SortFunc(b.Files[i].Chapters, func(a, b AudioChapter) int {
			if a.Start < b.Start {
				return -1
			}
			if a.Start > b.Start {
				return 1
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	slices.SortFunc(b.Files, func(a, b AudioFile) int {
		if a.Disc != b.Disc {
			if a.Disc < b.Disc {
				return -1
			}
			return 1
		}
		if a.Track != b.Track {
			if a.Track < b.Track {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return b, nil
}

func (c *Client) audioClient() Client {
	a := *c
	a.baseURL.Path = strings.TrimSuffix(c.baseURL.Path, "/books") + "/audiobooks"
	return a
}
func (c *Client) Audiobook(ctx context.Context, id string) (Audiobook, error) {
	if !validID(id) {
		return Audiobook{}, ErrInvalidMetadata
	}
	a := c.audioClient()
	var w wireAudioBook
	if err := a.readJSON(ctx, "/"+id, nil, &w); err != nil {
		return Audiobook{}, err
	}
	b, err := w.validateAudio()
	if err != nil || b.Book.ID != id {
		return Audiobook{}, ErrInvalidMetadata
	}
	return b, nil
}

// Audiobooks lists metadata only, including rows whose files cannot be played.
// Filters use retained collection IDs and include all their name aliases.
func (c *Client) Audiobooks(ctx context.Context, kind, id string, since time.Time, page, limit int) (AudioPage, error) {
	if page < 1 || page > maxPage || limit < 1 || limit > maxLimit || (kind != "" && kind != "authors" && kind != "series") || (kind != "" && !validCollectionID(id)) {
		return AudioPage{}, ErrInvalidMetadata
	}
	query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}, "sort": {"updated_at"}, "order": {"asc"}}
	if kind != "" {
		key := "series_id"
		if kind == "authors" {
			key = "author_id"
		}
		query.Set(key, id)
	}
	if !since.IsZero() {
		query.Set("updated_after", since.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	var w struct {
		Books json.RawMessage `json:"audiobooks"`
		Page  *int            `json:"page"`
		Limit *int            `json:"limit"`
		Total *int            `json:"total"`
	}
	a := c.audioClient()
	if err := a.readJSON(ctx, "", query, &w); err != nil {
		return AudioPage{}, err
	}
	var books []json.RawMessage
	// Actual handlers encode an empty nil slice as null. A missing field is
	// malformed; a present null array is a valid empty terminal page.
	if len(w.Books) == 0 || json.Unmarshal(w.Books, &books) != nil || w.Page == nil || *w.Page != page || w.Limit == nil || *w.Limit != limit || w.Total == nil || *w.Total < 0 || len(books) > limit {
		return AudioPage{}, ErrInvalidMetadata
	}
	out := AudioPage{Page: page, Limit: limit}
	seen := map[string]bool{}
	for _, raw := range books {
		var book wireAudioBook
		if json.Unmarshal(raw, &book) != nil {
			return AudioPage{}, ErrInvalidMetadata
		}
		b, err := book.metadata()
		if err != nil || seen[b.Book.ID] {
			return AudioPage{}, ErrInvalidMetadata
		}
		seen[b.Book.ID] = true
		out.Books = append(out.Books, b)
	}
	return out, nil
}

func validCollectionID(id string) bool {
	// authors/<base64url ID>.<spelling tag> must fit the host's 1024-byte opaque ID bound.
	return id != "" && len(id) <= 749 && utf8.ValidString(id) && strings.IndexFunc(id, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) && r != '-'
	}) < 0
}

// Collections lists one row per spelling, ordered by the spelling's exact
// bytes. Spellings that share an ID are separate rows, possibly far apart.
func (c *Client) Collections(ctx context.Context, kind string, page, limit int) (CollectionPage, error) {
	if (kind != "authors" && kind != "series") || page < 1 || page > maxPage || limit < 1 || limit > maxLimit {
		return CollectionPage{}, ErrInvalidMetadata
	}
	var w map[string]json.RawMessage
	a := c.audioClient()
	if err := a.readJSON(ctx, "/"+kind, url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(limit)}, "sort": {"name"}, "order": {"asc"}}, &w); err != nil {
		return CollectionPage{}, err
	}
	var rows []Collection
	var gotPage, gotLimit, total int
	if json.Unmarshal(w[kind], &rows) != nil || json.Unmarshal(w["page"], &gotPage) != nil || json.Unmarshal(w["limit"], &gotLimit) != nil || json.Unmarshal(w["total"], &total) != nil || gotPage != page || gotLimit != limit || total < 0 || len(rows) > limit {
		return CollectionPage{}, ErrInvalidMetadata
	}
	for i := range rows {
		if !validCollectionID(rows[i].ID) || len(rows[i].Name) > MaxCollectionSpelling {
			return CollectionPage{}, ErrInvalidMetadata
		}
		rows[i].Spelling = rows[i].Name
		rows[i].Name = audioText(rows[i].Name, maxCatalogText)
	}
	return CollectionPage{Collections: rows, Page: page, Limit: limit}, nil
}

// Collection returns every spelling Bookwarehouse groups under one ID.
func (c *Client) Collection(ctx context.Context, kind, id string) ([]Collection, error) {
	if (kind != "authors" && kind != "series") || !validCollectionID(id) {
		return nil, ErrInvalidMetadata
	}
	var w struct {
		ID    string   `json:"id"`
		Names []string `json:"names"`
	}
	a := c.audioClient()
	if err := a.readJSON(ctx, "/"+kind+"/"+id, nil, &w); err != nil {
		return nil, err
	}
	if w.ID != id || len(w.Names) == 0 || len(w.Names) > maxCollectionSpellings {
		return nil, ErrInvalidMetadata
	}
	rows := make([]Collection, len(w.Names))
	for i, spelling := range w.Names {
		rows[i] = Collection{ID: id, Name: audioText(spelling, maxCatalogText), Spelling: spelling}
	}
	return rows, nil
}

// AudioRange returns length bytes of a track from offset with one bounded
// Range request. Only 206 with exactly that span is accepted; Total is the
// size Bookwarehouse currently reports for the track.
func (c *Client) AudioRange(ctx context.Context, id string, fileID, offset, length int64) (*Body, error) {
	if !validID(id) || fileID <= 0 || offset < 0 || length <= 0 || offset > math.MaxInt64-length {
		return nil, ErrInvalidMetadata
	}
	a := c.audioClient()
	last := offset + length - 1
	header := http.Header{"Range": {"bytes=" + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(last, 10)}}
	response, err := a.request(ctx, "/"+id+"/stream", url.Values{"file_id": {strconv.FormatInt(fileID, 10)}}, header, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	first, end, total, ok := contentSpan(response.Header.Get("Content-Range"))
	if !ok || first != offset || end != last || (response.ContentLength >= 0 && response.ContentLength != length) {
		response.Body.Close()
		return nil, ErrUnavailable
	}
	return &Body{ReadCloser: &safeBody{body: response.Body, ctx: ctx}, Offset: offset, Total: total}, nil
}
func (c *Client) AudioCover(ctx context.Context, id string) ([]byte, string, error) {
	if !validID(id) {
		return nil, "", ErrInvalidMetadata
	}
	a := c.audioClient()
	return a.cover(ctx, "/"+id+"/cover")
}

// AuthorizeAudioCover checks the cover route before exposing retained bytes.
func (c *Client) AuthorizeAudioCover(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrInvalidMetadata
	}
	a := c.audioClient()
	r, err := a.request(ctx, "/"+id+"/cover", nil, http.Header{"Range": {"bytes=0-0"}}, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return err
	}
	if err := r.Body.Close(); err != nil {
		return safeError(ctx, err)
	}
	return nil
}
