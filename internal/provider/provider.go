package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/bookwarehouse"
	"github.com/Bloem-Studios/bloem-plugin-bookwarehouse/internal/verifiedcache"
	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const directoryRevision = "directories-v1"

func (p *Provider) Describe(ctx context.Context, _ *storagev1.DescribeRequest) (*storagev1.DescribeResponse, error) {
	s, ctx, done, err := p.begin(ctx, "")
	if err != nil {
		return nil, err
	}
	defer done()
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	root := "root"
	if s.config.Content == "audiobooks" {
		root = audioRoot
	}
	return &storagev1.DescribeResponse{Revision: 1, Sources: []*storagev1.Source{{Id: s.config.SourceID, Name: s.config.SourceName, RootEntryId: root, RevisionPinnedReads: true, MediaKinds: []string{s.config.Content}, DirectoryOnlyListing: true}}}, nil
}
func directory(id string) *storagev1.Entry {
	path := id
	if id == "root" {
		path = ""
	}
	return &storagev1.Entry{Id: id, Name: id, LogicalPath: path, Kind: storagev1.EntryKind_ENTRY_KIND_DIRECTORY, Revision: directoryRevision}
}
func bookEntry(b bookwarehouse.Book) *storagev1.Entry {
	// Stable names never use untrusted backend paths or mutable titles.
	name := b.ID + "." + b.Format
	return &storagev1.Entry{Id: b.Format + "/" + b.ID, Name: name, LogicalPath: b.Format + "/" + name, Kind: storagev1.EntryKind_ENTRY_KIND_FILE, Size: b.Size, ModifiedUnixNano: b.UpdatedAt.UnixNano(), Revision: "sha256:" + b.SHA256, Ebook: ebookMetadata(b)}
}

// ebookMetadata hands the host Bookwarehouse's catalog metadata, so it can
// publish the book without downloading it.
func ebookMetadata(b bookwarehouse.Book) *storagev1.EbookMetadata {
	c := b.Catalog
	meta := &storagev1.EbookMetadata{
		Title: b.Title, Authors: c.Authors, Description: c.Description, Publisher: c.Publisher,
		PublishedDate: c.PublishedDate, Language: c.Language, Isbn: c.ISBN, Series: c.Series,
		SeriesIndex: c.SeriesIndex, Genres: c.Genres, PageCount: int32(min(c.PageCount, 1<<30)),
	}
	if c.HasCover {
		meta.CoverEntryId, meta.CoverRevision, meta.CoverThumbhash = coverEntryID(b.ID), coverRevision(b), c.CoverThumbhash
	}
	return meta
}
func splitEntry(id string) (string, string, error) {
	format, key, ok := strings.Cut(id, "/")
	if !ok || (format != "epub" && format != "pdf" && format != coverDirectory) || !validID(key) {
		return "", "", status.Error(codes.InvalidArgument, "invalid entry identity")
	}
	return format, key, nil
}
func entry(ctx context.Context, s *state, id, expected string) (*storagev1.Entry, error) {
	if expected == "" && !isDirectoryID(s, id) {
		return nil, status.Error(codes.InvalidArgument, "expected revision required")
	}
	if s.config.Content == "audiobooks" {
		return audioEntry(ctx, s, id, expected)
	}
	var e *storagev1.Entry
	if id == "root" || id == "epub" || id == "pdf" {
		e = directory(id)
	} else if strings.HasPrefix(id, "folders/") {
		format, folder, err := splitFolder(id)
		if err != nil {
			return nil, err
		}
		if err := currentFolder(ctx, s, format, folder); err != nil {
			return nil, err
		}
		e = folderEntry(format, folder)
	} else {
		format, key, err := splitEntry(id)
		if err != nil {
			return nil, err
		}
		if format == coverDirectory {
			e, _, err := coverEntry(ctx, s, key, expected)
			return e, err
		}
		b, err := s.client.Get(ctx, key)
		if err != nil {
			return nil, mapError(ctx, err)
		}
		if b.ID != key {
			return nil, status.Error(codes.Unavailable, "invalid Bookwarehouse metadata")
		}
		if b.Format != format {
			return nil, status.Error(codes.NotFound, "entry absent")
		}
		e = bookEntry(b)
	}
	if expected != "" && e.Revision != expected {
		return nil, status.Error(codes.FailedPrecondition, "revision changed")
	}
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return e, nil
}

func isDirectoryID(s *state, id string) bool {
	if s.config.Content == "audiobooks" {
		_, _, _, err := splitAudioDirectory(id)
		return err == nil
	}
	return id == "root" || id == "epub" || id == "pdf" || strings.HasPrefix(id, "folders/")
}
func (p *Provider) Stat(ctx context.Context, r *storagev1.StatRequest) (*storagev1.StatResponse, error) {
	if r.GetSourceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source required")
	}
	s, ctx, done, err := p.begin(ctx, r.GetSourceId())
	if err != nil {
		return nil, err
	}
	defer done()
	e, err := entry(ctx, s, r.GetEntryId(), r.GetExpectedRevision())
	if err != nil {
		return nil, err
	}
	return &storagev1.StatResponse{Entry: e}, nil
}
func (p *Provider) Read(r *storagev1.ReadRequest, stream storagev1.StorageProvider_ReadServer) error {
	if r.GetSourceId() == "" {
		return status.Error(codes.InvalidArgument, "source required")
	}
	if r.GetOffset() < 0 || r.GetLength() < 0 || r.GetLength() > 8<<20 {
		return status.Error(codes.InvalidArgument, "invalid file range")
	}
	s, ctx, done, err := p.begin(stream.Context(), r.GetSourceId())
	if err != nil {
		return err
	}
	defer done()
	if s.config.Content == "audiobooks" {
		return readAudio(ctx, s, r, stream)
	}
	if format, id, err := splitEntry(r.GetEntryId()); err == nil && format == coverDirectory {
		return readCover(ctx, s, id, r, stream)
	}
	e, err := entry(ctx, s, r.GetEntryId(), r.GetExpectedRevision())
	if err != nil {
		return err
	}
	if e.Kind != storagev1.EntryKind_ENTRY_KIND_FILE || r.Offset > e.Size || r.Length > e.Size-r.Offset {
		return status.Error(codes.InvalidArgument, "invalid file range")
	}
	_, id, err := splitEntry(e.Id)
	if err != nil {
		return err
	}
	waitCtx, stopWaiting := downloadWait(ctx)
	defer stopWaiting()
	lease, downloaded, err := s.cache.Acquire(waitCtx, strings.TrimPrefix(e.Revision, "sha256:"), e.Size, bookSource(s.client, id))
	if err != nil {
		if waitCtx.Err() != nil && ctx.Err() == nil {
			// The verified download continues in the cache; a retry joins it.
			return status.Error(codes.Unavailable, "verified download in progress; retry")
		}
		return mapError(ctx, err)
	}
	defer lease.Release()
	// Metadata access and download access are separate permissions. Unless
	// this read's own download produced the file, ask the real download route
	// for one byte before exposing bytes from the verified file.
	if !downloaded {
		if err := s.client.Authorize(ctx, id); err != nil {
			return mapError(ctx, err)
		}
	}
	return streamLease(ctx, lease, r, stream)
}

// streamLease emits an already authorized immutable lease. Each feed owns its
// metadata, range and acquisition checks before reaching this shared transport.
func streamLease(ctx context.Context, lease *verifiedcache.Lease, r *storagev1.ReadRequest, stream storagev1.StorageProvider_ReadServer) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if r.Length == 0 {
		return stream.Send(&storagev1.ReadChunk{Offset: r.Offset, Eof: true})
	}
	end := r.Offset + r.Length
	for offset := r.Offset; offset < end; {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		data := make([]byte, min(int64(128<<10), end-offset))
		n, readErr := lease.ReadAt(data, offset)
		if n != len(data) || (readErr != nil && !errors.Is(readErr, io.EOF)) {
			return status.Error(codes.Unavailable, "verified cache read failed")
		}
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if err := stream.Send(&storagev1.ReadChunk{Offset: offset, Data: data, Eof: offset+int64(n) == end}); err != nil {
			return err
		}
		offset += int64(n)
	}
	return nil
}

// maxDownloadWait is how long before its deadline a read stops waiting for a
// verified download, so it can answer Unavailable instead of running out of
// time; the host's RPC deadline is otherwise reported as DeadlineExceeded.
const maxDownloadWait = 500 * time.Millisecond

// downloadWait bounds how long a read waits for the verified cache.
func downloadWait(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-min(maxDownloadWait, time.Until(deadline)/4)))
}

// bookSource downloads book id for the verified cache, resuming at an offset.
// Errors that a retry cannot fix end the download.
func bookSource(client *bookwarehouse.Client, id string) verifiedcache.Source {
	return func(ctx context.Context, offset int64) (io.ReadCloser, int64, int64, error) {
		body, err := client.Download(ctx, id, offset)
		switch {
		case err == nil:
			return body, body.Offset, body.Total, nil
		case errors.Is(err, bookwarehouse.ErrRangeNotSatisfiable):
			// The object is no longer than the bytes already verified.
			return nil, 0, 0, verifiedcache.ErrMismatch
		case errors.Is(err, bookwarehouse.ErrDenied), errors.Is(err, bookwarehouse.ErrNotFound), errors.Is(err, bookwarehouse.ErrInvalidMetadata):
			return nil, 0, 0, verifiedcache.Permanent(err)
		default:
			return nil, 0, 0, err
		}
	}
}

func mapError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, bookwarehouse.ErrDenied):
		return status.Error(codes.PermissionDenied, "Bookwarehouse access denied")
	case errors.Is(err, bookwarehouse.ErrNotFound):
		return status.Error(codes.NotFound, "entry absent")
	case errors.Is(err, verifiedcache.ErrMismatch):
		return status.Error(codes.FailedPrecondition, "download does not match pinned revision")
	case errors.Is(err, verifiedcache.ErrTooLarge), errors.Is(err, verifiedcache.ErrCapacity):
		return status.Error(codes.ResourceExhausted, "configured cache limit exceeded")
	case errors.Is(err, verifiedcache.ErrClosed):
		return status.Error(codes.Canceled, "configuration replaced")
	default:
		return status.Error(codes.Unavailable, "Bookwarehouse operation unavailable")
	}
}
