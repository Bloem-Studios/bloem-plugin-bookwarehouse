package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	audioRoot              = "audio-root"
	audioDirectoryRevision = "audio-directories-v1"
	maxAudioCursorOffset   = 10000
	audioRevisionPrefix    = "bwaudio-v1:"
	// audioChangeFeed names the audiobook change feed in change tokens. A token
	// naming another feed is refused, so the host lists every book in full.
	// It changes when what a listed book carries changes for books Bookwarehouse
	// has not updated: "audiobooks-2" lists one-file books' embedded chapters.
	audioChangeFeed = "audiobooks-2"
)

type audioCursor struct {
	Version     int    `json:"v"`
	Directory   string `json:"d"`
	Browse      bool   `json:"b"`
	Page        int    `json:"p"`
	Offset      int    `json:"o,omitempty"`
	Upper       int64  `json:"u,omitempty"`
	AfterSec    int64  `json:"s,omitempty"`
	AfterID     string `json:"a,omitempty"`
	After       string `json:"n,omitempty"`
	BookVersion string `json:"f,omitempty"`
	// Since, FullAt and Phase follow list.go's cursor for the incremental
	// catalog listing.
	Since  int64 `json:"cs,omitempty"`
	FullAt int64 `json:"fa,omitempty"`
	Phase  int   `json:"ph,omitempty"`
}

func audioDirectory(id, name string) *storagev1.Entry {
	if name == "" {
		name = id
	}
	logical := id
	if id == audioRoot {
		logical = ""
	}
	return &storagev1.Entry{Id: id, Name: name, LogicalPath: logical, Kind: storagev1.EntryKind_ENTRY_KIND_DIRECTORY, Revision: audioDirectoryRevision}
}
func audiobookMetadata(b bookwarehouse.Audiobook) *storagev1.AudiobookMetadata {
	c := b.Book.Catalog
	m := &storagev1.AudiobookMetadata{BookId: b.Book.ID, Title: b.Book.Title, Authors: c.Authors, Narrators: b.Narrators, Description: c.Description, Publisher: c.Publisher, PublishedDate: c.PublishedDate, Language: c.Language, Isbn: c.ISBN, Series: c.Series, SeriesIndex: c.SeriesIndex, Genres: c.Genres}
	if c.HasCover {
		m.CoverEntryId, m.CoverRevision, m.CoverThumbhash = coverEntryID(b.Book.ID), coverRevision(b.Book), c.CoverThumbhash
	}
	return m
}
func audioBookDirectory(b bookwarehouse.Audiobook) *storagev1.Entry {
	name := b.Book.Title
	if name == "" {
		name = b.Book.ID
	}
	e := audioDirectory("audiobooks/"+b.Book.ID, name)
	e.Audiobook = audiobookMetadata(b)
	return e
}

// audioFileEntry describes a track as Bookwarehouse does; its revision names
// that description (see bookwarehouse.AudioFile.Evidence), not verified bytes.
func audioFileEntry(b bookwarehouse.Audiobook, f bookwarehouse.AudioFile) *storagev1.Entry {
	id := "audio/" + b.Book.ID + "/" + strconv.FormatInt(f.ID, 10)
	name := strconv.FormatInt(f.ID, 10) + "." + f.Container
	meta := &storagev1.AudioFileMetadata{BookId: b.Book.ID, DiscNumber: f.Disc, TrackNumber: f.Track, DurationSeconds: f.Duration, Codec: f.Codec, Container: f.Container, Bitrate: f.Bitrate, SampleRate: f.SampleRate, Channels: f.Channels}
	for _, ch := range f.Chapters {
		meta.Chapters = append(meta.Chapters, &storagev1.AudioChapter{Id: ch.ID, Title: ch.Title, StartSeconds: ch.Start, EndSeconds: ch.End})
	}
	return &storagev1.Entry{Id: id, Name: name, LogicalPath: "audiobooks/" + b.Book.ID + "/" + name, Kind: storagev1.EntryKind_ENTRY_KIND_FILE, Size: f.Size, ModifiedUnixNano: b.Book.UpdatedAt.UnixNano(), Revision: audioRevisionPrefix + f.Evidence, Audiobook: audiobookMetadata(b), AudioFile: meta}
}
func metadataBook(ctx context.Context, s *state, id string) (bookwarehouse.Book, error) {
	if s.config.Content != "audiobooks" {
		return s.client.Get(ctx, id)
	}
	b, err := s.client.Audiobook(ctx, id)
	return b.Book, err
}

func splitAudioFile(id string) (string, int64, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] != "audio" || !validID(parts[1]) {
		return "", 0, status.Error(codes.InvalidArgument, "invalid audio identity")
	}
	file, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || file <= 0 || strconv.FormatInt(file, 10) != parts[2] {
		return "", 0, status.Error(codes.InvalidArgument, "invalid audio identity")
	}
	return parts[1], file, nil
}
func splitAudioDirectory(id string) (kind, key, tag string, err error) {
	if id == audioRoot || id == "audiobooks" || id == "authors" || id == "series" {
		return id, "", "", nil
	}
	kind, key, ok := strings.Cut(id, "/")
	if !ok {
		return "", "", "", status.Error(codes.InvalidArgument, "invalid audio directory")
	}
	if kind == "audiobooks" && validID(key) {
		return kind, key, "", nil
	}
	if kind == "authors" || kind == "series" {
		encoded, tag, _ := strings.Cut(key, ".")
		raw, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
		if decodeErr == nil && base64.RawURLEncoding.EncodeToString(raw) == encoded && len(raw) > 0 && len(raw) <= 749 && !strings.ContainsAny(string(raw), "/\\\x00") && validSpellingTag(tag) {
			return kind, string(raw), tag, nil
		}
	}
	return "", "", "", status.Error(codes.InvalidArgument, "invalid audio directory")
}

// collectionEntryID names one spelling of a collection. Spellings that share
// an ID are separate directories listing the same books, so a listing can
// stream the provider's rows without reading them all to merge aliases.
func collectionEntryID(kind, id, spelling string) string {
	return kind + "/" + base64.RawURLEncoding.EncodeToString([]byte(id)) + "." + spellingTag(spelling)
}
func spellingTag(spelling string) string {
	sum := sha256.Sum256([]byte(spelling))
	return hex.EncodeToString(sum[:8])
}
func collectionName(row bookwarehouse.Collection) string {
	if row.Name == "" {
		return row.ID
	}
	return row.Name
}
func validSpellingTag(tag string) bool {
	raw, err := hex.DecodeString(tag)
	return err == nil && len(raw) == 8 && hex.EncodeToString(raw) == tag
}

// validAudioRevision accepts the revisions audioFileEntry issues.
func validAudioRevision(revision string) bool {
	evidence, ok := strings.CutPrefix(revision, audioRevisionPrefix)
	raw, err := hex.DecodeString(evidence)
	return ok && err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == evidence
}

// currentAudio checks a track against Bookwarehouse's current description of
// it. The revision names that description, so a read never downloads a track
// to identify it.
func currentAudio(ctx context.Context, s *state, id, expected string) (*storagev1.Entry, error) {
	book, file, err := splitAudioFile(id)
	if err != nil {
		return nil, err
	}
	if !validAudioRevision(expected) {
		return nil, status.Error(codes.InvalidArgument, "invalid expected audio revision")
	}
	b, err := s.audiobook(ctx, book)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, f := range b.Files {
		if f.ID == file {
			e := audioFileEntry(b, f)
			if e.Revision != expected {
				return nil, status.Error(codes.FailedPrecondition, "revision changed")
			}
			return e, nil
		}
	}
	return nil, status.Error(codes.NotFound, "audio file absent")
}
func audioEntry(ctx context.Context, s *state, id, expected string) (*storagev1.Entry, error) {
	if strings.HasPrefix(id, "cover/") {
		_, key, err := splitEntry(id)
		if err != nil {
			return nil, err
		}
		e, _, err := coverEntry(ctx, s, key, expected)
		return e, err
	}
	if strings.HasPrefix(id, "audio/") {
		return currentAudio(ctx, s, id, expected)
	}
	kind, key, tag, err := splitAudioDirectory(id)
	if err != nil {
		return nil, err
	}
	if expected != "" && expected != audioDirectoryRevision {
		return nil, status.Error(codes.FailedPrecondition, "revision changed")
	}
	if kind == "audiobooks" && key != "" {
		b, err := s.audiobook(ctx, key)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		return audioBookDirectory(b), nil
	}
	if (kind == "authors" || kind == "series") && key != "" {
		name, err := currentCollection(ctx, s, kind, key, tag)
		if err != nil {
			return nil, err
		}
		return audioDirectory(id, name), nil
	}
	return audioDirectory(id, kind), nil
}
func readAudio(ctx context.Context, s *state, r *storagev1.ReadRequest, stream storagev1.StorageProvider_ReadServer) error {
	if strings.HasPrefix(r.GetEntryId(), "cover/") {
		_, key, err := splitEntry(r.GetEntryId())
		if err != nil {
			return err
		}
		return readCover(ctx, s, key, r, stream)
	}
	e, err := currentAudio(ctx, s, r.GetEntryId(), r.GetExpectedRevision())
	if err != nil {
		return err
	}
	if r.Offset > e.Size || r.Length > e.Size-r.Offset {
		return status.Error(codes.InvalidArgument, "invalid file range")
	}
	if r.Length == 0 {
		return stream.Send(&storagev1.ReadChunk{Offset: r.Offset, Eof: true})
	}
	book, file, _ := splitAudioFile(e.Id)
	body, err := s.client.AudioRange(ctx, book, file, r.Offset, r.Length)
	if err != nil {
		return mapError(ctx, err)
	}
	defer body.Close()
	if body.Total != e.Size {
		return status.Error(codes.FailedPrecondition, "audio size changed")
	}
	end := r.Offset + r.Length
	for offset := r.Offset; offset < end; {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		data := make([]byte, min(int64(128<<10), end-offset))
		if _, err := io.ReadFull(body, data); err != nil {
			return mapError(ctx, err)
		}
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if err := stream.Send(&storagev1.ReadChunk{Offset: offset, Data: data, Eof: offset+int64(len(data)) == end}); err != nil {
			return err
		}
		offset += int64(len(data))
	}
	return nil
}

func listAudio(ctx context.Context, s *state, r *storagev1.ListRequest) (*storagev1.ListResponse, error) {
	kind, key, tag, err := splitAudioDirectory(r.GetDirectoryId())
	if err != nil {
		return nil, err
	}
	// Only the catalog directory is incremental: a book or collection must be
	// listed whole for the host to assemble and reconcile it.
	catalog := kind == "audiobooks" && key == "" && !r.GetDirectoriesOnly()
	if r.GetChangesSince() != "" && !catalog {
		return nil, status.Error(codes.FailedPrecondition, "audio listing requires full inventory")
	}
	limit := int(r.GetMaxEntries())
	if limit == 0 {
		limit = 100
	}
	limit = min(limit, 512)
	c := audioCursor{Version: 1, Directory: r.GetDirectoryId(), Browse: r.GetDirectoriesOnly(), Page: 1}
	if r.GetCursor() != "" {
		raw, ok := verified(s, r.GetCursor(), 8192)
		if !ok || json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Directory != r.GetDirectoryId() || c.Browse != r.GetDirectoriesOnly() || c.Page < 1 || c.Page > 21474836 || c.Offset < 0 || c.Offset > maxAudioCursorOffset || c.AfterSec < 0 || c.AfterSec > c.Upper || (c.AfterID != "" && !validID(c.AfterID)) || len(c.After) > bookwarehouse.MaxCollectionSpelling ||
			c.Since < 0 || c.Since > c.Upper || c.FullAt < 0 || c.FullAt > c.Upper || (c.Phase != phaseBooks && c.Phase != phaseDeletions) || (c.Phase == phaseDeletions && c.Since == 0) || (!catalog && (c.Since != 0 || c.Phase != phaseBooks)) {
			return nil, status.Error(codes.InvalidArgument, "invalid audio continuation")
		}
	} else if r.GetChangesSince() != "" {
		token, err := decodeChangeToken(s, audioChangeFeed, r.GetChangesSince())
		if err != nil {
			return nil, err
		}
		c.Since = max(1, token.Since-int64(changeOverlap/time.Second))
		c.FullAt = token.FullAt
	}
	if (kind == "authors" || kind == "series") && key != "" {
		if _, err := currentCollection(ctx, s, kind, key, tag); err != nil {
			return nil, err
		}
	}
	out := &storagev1.ListResponse{}
	if kind == audioRoot {
		dirs := []string{"audiobooks", "authors", "series"}
		if c.Offset >= len(dirs) {
			return nil, status.Error(codes.InvalidArgument, "invalid audio continuation")
		}
		end := min(c.Offset+limit, len(dirs))
		for _, id := range dirs[c.Offset:end] {
			out.Entries = append(out.Entries, audioDirectory(id, id))
		}
		c.Offset = end
		out.Complete = end == len(dirs)
	} else if (kind == "authors" || kind == "series") && key == "" {
		return listCollections(ctx, s, c, kind, limit)
	} else if kind == "audiobooks" && key != "" {
		if c.Browse {
			return &storagev1.ListResponse{Complete: true}, nil
		}
		b, err := s.freshAudiobook(ctx, key)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		raw, _ := json.Marshal(b.Files)
		sum := sha256.Sum256(raw)
		version := fmt.Sprintf("%x", sum)
		if c.BookVersion != "" && c.BookVersion != version {
			return nil, status.Error(codes.FailedPrecondition, "audio file inventory changed")
		}
		c.BookVersion = version
		if c.Offset >= len(b.Files) {
			return nil, status.Error(codes.InvalidArgument, "invalid audio continuation")
		}
		pageBytes := 0
		for c.Offset < len(b.Files) && len(out.Entries) < limit {
			e := audioFileEntry(b, b.Files[c.Offset])
			size := entryBytes(e)
			if size > maxPageBytes {
				// A track whose chapters alone outgrow a page is listed without
				// them, rather than withholding the book.
				e.AudioFile.Chapters = nil
				size = entryBytes(e)
			}
			if size > maxPageBytes {
				return nil, status.Error(codes.ResourceExhausted, "audio metadata exceeds page limit")
			}
			if pageBytes+size > maxPageBytes {
				break
			}
			out.Entries = append(out.Entries, e)
			pageBytes += size
			c.Offset++
		}
		out.Complete = c.Offset == len(b.Files)
	} else {
		if c.Upper == 0 {
			c.Upper = time.Now().Unix()
		}
		if c.FullAt == 0 {
			c.FullAt = c.Upper
		}
		if c.Phase == phaseDeletions {
			return listAudioDeletions(ctx, s, c)
		}
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
			filter := ""
			if key != "" {
				filter = kind
			}
			var page bookwarehouse.AudioPage
			fetched, err := budget.fetch(ctx, fetch == 0, func(ctx context.Context) (err error) {
				page, err = s.client.Audiobooks(ctx, filter, key, since, c.Page, backendPageSize)
				if err != nil {
					return mapError(ctx, err)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if !fetched {
				break
			}
			next := c
			consumed, reachedUpper := true, false
			for i, b := range page.Books {
				if i > 0 && !after(b.Book, page.Books[i-1].Book.UpdatedAt.Unix(), page.Books[i-1].Book.ID) {
					return nil, status.Error(codes.Unavailable, "unstable audiobook ordering")
				}
				if b.Book.UpdatedAt.Unix() > c.Upper {
					reachedUpper = true
					break
				}
				if c.AfterID != "" && !after(b.Book, c.AfterSec, c.AfterID) {
					continue
				}
				e := audioBookDirectory(b)
				size := entryBytes(e)
				if len(out.Entries) == limit || (len(out.Entries) > 0 && pageBytes+size > maxPageBytes) {
					consumed, full = false, true
					break
				}
				out.Entries = append(out.Entries, e)
				pageBytes += size
				next.AfterSec, next.AfterID = b.Book.UpdatedAt.Unix(), b.Book.ID
			}
			out.Complete = reachedUpper || (consumed && len(page.Books) < backendPageSize)
			switch {
			case next.AfterSec != c.AfterSec || c.AfterID == "":
				next.Page = 1
			case consumed:
				next.Page++
			}
			c = next
		}
		if out.Complete && c.Since > 0 {
			// An incremental listing reports deletions after its books.
			out.Complete = false
			c.Phase, c.Page = phaseDeletions, 1
		}
		if out.Complete && catalog {
			out.ChangeToken = encodeChangeToken(s, audioChangeFeed, c.Upper, c.FullAt)
		}
	}
	if !out.Complete {
		raw, _ := json.Marshal(c)
		out.NextCursor = sign(s, raw)
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return out, nil
}

// listAudioDeletions reports one page of audiobooks deleted since the
// incremental catalog listing's Since.
func listAudioDeletions(ctx context.Context, s *state, c audioCursor) (*storagev1.ListResponse, error) {
	page, err := s.client.AudiobookDeletions(ctx, time.Unix(c.Since, 0), c.Page, backendPageSize)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	out := &storagev1.ListResponse{}
	for _, d := range page.Deletions {
		out.RemovedEntryIds = append(out.RemovedEntryIds, "audiobooks/"+d.ID)
	}
	if len(page.Deletions) < backendPageSize {
		out.Complete = true
		out.ChangeToken = encodeChangeToken(s, audioChangeFeed, c.Upper, c.FullAt)
	} else {
		c.Page++
		raw, _ := json.Marshal(c)
		out.NextCursor = sign(s, raw)
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return out, nil
}

// currentCollection asks Bookwarehouse for one collection's spellings,
// including collections without books: filtered book results cannot prove a
// collection exists. It returns the display name of the spelling tag names.
func currentCollection(ctx context.Context, s *state, kind, id, tag string) (string, error) {
	spellings, err := s.client.Collection(ctx, kind, id)
	if err != nil {
		return "", mapError(ctx, err)
	}
	for _, row := range spellings {
		if spellingTag(row.Spelling) == tag {
			return collectionName(row), nil
		}
	}
	return "", status.Error(codes.NotFound, "collection spelling absent")
}

// listCollections streams the provider's rows: one per spelling, ordered by the
// spelling's exact bytes. A continuation resumes after the last listed
// spelling, so rows added or removed before it neither repeat nor skip one.
func listCollections(ctx context.Context, s *state, c audioCursor, kind string, limit int) (*storagev1.ListResponse, error) {
	budget := newListBudget(ctx)
	defer budget.cancel()
	out := &storagev1.ListResponse{}
	pageBytes := 0
	// Removed rows shift later ones onto earlier pages; step back until the
	// page starts at or before the last listed spelling.
	stepBack := c.After != ""
	for fetch := 0; fetch < maxFetchesPerList && len(out.Entries) < limit; fetch++ {
		if fetch > 0 && !budget.fits() {
			break
		}
		var page bookwarehouse.CollectionPage
		fetched, err := budget.fetch(ctx, fetch == 0, func(ctx context.Context) (err error) {
			page, err = s.client.Collections(ctx, kind, c.Page, backendPageSize)
			if err != nil {
				return mapError(ctx, err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if !fetched {
			break
		}
		rows := page.Collections
		for i := 1; i < len(rows); i++ {
			if rows[i-1].Spelling >= rows[i].Spelling {
				return nil, status.Error(codes.Unavailable, "unstable collection ordering")
			}
		}
		if stepBack && c.Page > 1 && (len(rows) == 0 || rows[0].Spelling > c.After) {
			c.Page--
			continue
		}
		stepBack = false
		full := false
		for _, row := range rows {
			if c.After != "" && row.Spelling <= c.After {
				continue
			}
			e := audioDirectory(collectionEntryID(kind, row.ID, row.Spelling), collectionName(row))
			size := entryBytes(e)
			if size > maxPageBytes {
				return nil, status.Error(codes.ResourceExhausted, "collection metadata exceeds page limit")
			}
			if len(out.Entries) == limit || (len(out.Entries) > 0 && pageBytes+size > maxPageBytes) {
				full = true
				break
			}
			out.Entries = append(out.Entries, e)
			pageBytes += size
			c.After = row.Spelling
		}
		if full {
			break
		}
		if len(rows) < backendPageSize {
			out.Complete = true
			break
		}
		c.Page++
	}
	if !out.Complete {
		raw, _ := json.Marshal(c)
		out.NextCursor = sign(s, raw)
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return out, nil
}
