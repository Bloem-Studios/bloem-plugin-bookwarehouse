package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"

	storagev1 "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/bloem/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type interruptedCapture struct {
	capture
	cancel context.CancelFunc
	err    error
	sends  int
}

func (s *interruptedCapture) Send(c *storagev1.ReadChunk) error {
	s.sends++
	if s.err != nil {
		return s.err
	}
	s.capture.Send(c)
	s.cancel()
	return nil
}

func TestReadTransportAcrossFeeds(t *testing.T) {
	for _, audio := range []bool{false, true} {
		name := "ebook"
		if audio {
			name = "audio"
		}
		t.Run(name, func(t *testing.T) {
			body := bytes.Repeat([]byte("ebook"), 40000)
			var p *Provider
			req := &storagev1.ReadRequest{Offset: 11, Length: 170000}
			ctx := context.Background()
			if audio {
				b, srv := audioFixture(t)
				b.bodies["41"] = body
				b.books[0]["files"].([]map[string]any)[1]["file_size"] = len(body)
				p = configuredAudio(t, srv.URL, nil)
				page, err := p.List(ctx, &storagev1.ListRequest{SourceId: "bookwarehouse-audiobooks", DirectoryId: "audiobooks/book-a"})
				if err != nil {
					t.Fatal(err)
				}
				req.SourceId, req.EntryId, req.ExpectedRevision = "bookwarehouse-audiobooks", "audio/book-a/41", page.Entries[0].Revision
			} else {
				_, srv := fixture(t, 1)
				p = configured(t, srv.URL)
				req.SourceId, req.EntryId = "bookwarehouse", "epub/book-000"
				req.ExpectedRevision = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
			}
			out := &capture{ctx: ctx}
			if err := p.Read(req, out); err != nil {
				t.Fatal(err)
			}
			if len(out.chunks) != 2 {
				t.Fatalf("chunk count=%d; want 2", len(out.chunks))
			}
			first, last := out.chunks[0], out.chunks[1]
			if first.Offset != 11 || len(first.Data) != 131072 || first.Eof || last.Offset != 131083 || len(last.Data) != 38928 || !last.Eof || !bytes.Equal(append(first.Data, last.Data...), body[11:170011]) {
				t.Fatal("range bytes, absolute offsets, chunk bound or final EOF changed")
			}
			req.Offset, req.Length = 200000, 0
			out = &capture{ctx: ctx}
			if err := p.Read(req, out); err != nil || len(out.chunks) != 1 || out.chunks[0].Offset != 200000 || len(out.chunks[0].Data) != 0 || !out.chunks[0].Eof {
				t.Fatalf("zero-length EOF: chunks=%v err=%v", out.chunks, err)
			}
			req.Offset, req.Length = 199999, 2
			out = &capture{ctx: ctx}
			if err := p.Read(req, out); status.Code(err) != codes.InvalidArgument || len(out.chunks) != 0 {
				t.Fatalf("range beyond EOF: chunks=%d err=%v", len(out.chunks), err)
			}
			req.Offset, req.Length = 11, 170000
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			stopped := &interruptedCapture{capture: capture{ctx: cancelCtx}, cancel: cancel}
			if err := p.Read(req, stopped); status.Code(err) != codes.Canceled || len(stopped.chunks) != 1 || stopped.chunks[0].Eof {
				t.Fatalf("cancellation between chunks: chunks=%d err=%v", len(stopped.chunks), err)
			}
			sendErr := status.Error(codes.Aborted, "receiver stopped")
			failed := &interruptedCapture{capture: capture{ctx: ctx}, err: sendErr}
			if err := p.Read(req, failed); err != sendErr || failed.sends != 1 || len(failed.chunks) != 0 {
				t.Fatalf("send failure: sends=%d chunks=%d err=%v", failed.sends, len(failed.chunks), err)
			}
		})
	}
}
