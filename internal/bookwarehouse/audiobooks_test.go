package bookwarehouse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const audioFixture = `{"id":"book-a","title":"Book A","author":"M.R. Forbes","author_id":"m-r-forbes","series_name":"Star-Crossed","series_id":"star-crossed","updated_at":"2026-10-10T12:00:00Z","narrators":[{"id":1,"name":"Reader"}],"files":[{"id":42,"audiobook_id":"book-a","file_path":"private/track02.mp3","file_size":3,"file_hash":"sampled-folder-hash","track_num":2,"disc_num":1,"duration_seconds":3,"codec":"mp3"},{"id":41,"audiobook_id":"book-a","file_path":"private/track01.mp3","file_size":3,"file_hash":"sampled-folder-hash","track_num":1,"disc_num":1,"duration_seconds":2,"codec":"mp3"}],"chapters":[{"id":3,"audiobook_id":"book-a","chapter_index":0,"start_seconds":0,"end_seconds":1,"is_accurate":true},{"id":4,"audiobook_id":"book-a","file_id":41,"chapter_index":0,"title":"First","start_seconds":0,"end_seconds":2,"is_accurate":true}]}`

func TestAudiobookRetainsOrderedFileIDsAndOnlyAssociatedChapters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, audioFixture) }))
	defer srv.Close()
	c, err := New(srv.URL, "key", true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, err := c.Audiobook(context.Background(), "book-a")
	if err != nil {
		t.Fatal(err)
	}
	if b.Book.Title != "Book A" || len(b.Files) != 2 || b.Files[0].ID != 41 || b.Files[1].ID != 42 || b.Files[0].Size != 3 || len(b.Files[0].Chapters) != 1 || b.Files[0].Chapters[0].ID != "4" {
		t.Fatalf("metadata: %+v", b)
	}
}

func TestAudiobookRejectsMissingSizeOwnershipAndMalformedAssociatedTiming(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing size", func(m map[string]any) { delete(m["files"].([]any)[0].(map[string]any), "file_size") }},
		{"foreign file", func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["audiobook_id"] = "other" }},
		{"negative track", func(m map[string]any) { m["files"].([]any)[0].(map[string]any)["track_num"] = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			json.Unmarshal([]byte(audioFixture), &m)
			tc.change(m)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m) }))
			defer srv.Close()
			c, _ := New(srv.URL, "key", true)
			defer c.Close()
			if _, err := c.Audiobook(context.Background(), "book-a"); !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("accepted invalid metadata: %v", err)
			}
		})
	}
}

// Chapters never withhold a book: one that does not fit is left out, and a
// book with more than a track may carry keeps none.
func TestAudiobookLeavesOutChaptersThatDoNotFit(t *testing.T) {
	many := func(m map[string]any) {
		var list []any
		for i := range maxAudioChapters + 1 {
			list = append(list, map[string]any{"id": 100 + i, "audiobook_id": "book-a", "file_id": 41, "start_seconds": 0, "end_seconds": 1, "is_accurate": true})
		}
		m["chapters"] = list
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"past its file", func(m map[string]any) {
			m["chapters"].([]any)[1].(map[string]any)["start_seconds"] = 2
			m["chapters"].([]any)[1].(map[string]any)["end_seconds"] = 99
		}},
		{"foreign file", func(m map[string]any) { m["chapters"].([]any)[1].(map[string]any)["file_id"] = 999 }},
		{"too many", many},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			json.Unmarshal([]byte(audioFixture), &m)
			tc.change(m)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m) }))
			defer srv.Close()
			c, _ := New(srv.URL, "key", true)
			defer c.Close()
			b, err := c.Audiobook(context.Background(), "book-a")
			if err != nil || len(b.Files) != 2 || len(b.Files[0].Chapters) != 0 || len(b.Files[1].Chapters) != 0 {
				t.Fatalf("book=%+v err=%v", b, err)
			}
		})
	}
}

func TestAudioRangeRejectsRedirectsAndPinsExplicitFileSelection(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audiobooks/book-a/stream" || r.URL.Query().Get("file_id") != "41" || r.Header.Get("X-API-Key") != "key" {
			t.Error("incorrect authenticated route")
		}
		http.Redirect(w, r, target.URL, 302)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	if _, err := c.AudioRange(context.Background(), "book-a", 41, 0, 5); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("redirect followed")
	}
}

func TestAudioRangeAcceptsOnlyTheRequestedSpan(t *testing.T) {
	var gotRange string
	var reply func(w http.ResponseWriter)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		reply(w)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	partial := func(span, body string) func(w http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", span)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, body)
		}
	}
	reply = partial("bytes 1-3/5", "ell")
	body, err := c.AudioRange(context.Background(), "book-a", 41, 1, 3)
	if err != nil || gotRange != "bytes=1-3" || body.Offset != 1 || body.Total != 5 {
		t.Fatalf("range: %+v %v, sent %q", body, err, gotRange)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "ell" {
		t.Fatalf("data %q", data)
	}
	for name, tc := range map[string]struct {
		reply func(w http.ResponseWriter)
		want  error
	}{
		"whole object instead of the range": {func(w http.ResponseWriter) { io.WriteString(w, "hello") }, ErrUnavailable},
		"longer span":                       {partial("bytes 1-4/5", "ello"), ErrUnavailable},
		"other start":                       {partial("bytes 0-2/5", "hel"), ErrUnavailable},
		"span past the declared size":       {partial("bytes 1-3/3", "ell"), ErrUnavailable},
		"missing file":                      {func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, ErrNotFound},
		"object shorter than the range":     {func(w http.ResponseWriter) { w.WriteHeader(http.StatusRequestedRangeNotSatisfiable) }, ErrRangeNotSatisfiable},
	} {
		reply = tc.reply
		if body, err := c.AudioRange(context.Background(), "book-a", 41, 1, 3); !errors.Is(err, tc.want) || body != nil {
			t.Errorf("%s: %v %v", name, body, err)
		}
	}
	for _, bad := range [][3]int64{{0, 1, 3}, {41, -1, 3}, {41, 1, 0}} {
		if _, err := c.AudioRange(context.Background(), "book-a", bad[0], bad[1], bad[2]); !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("arguments %v: %v", bad, err)
		}
	}
}

func TestAudioEvidenceFollowsProviderDescription(t *testing.T) {
	book := audioFixture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, book) }))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	evidence := func() string {
		t.Helper()
		b, err := c.Audiobook(context.Background(), "book-a")
		if err != nil {
			t.Fatal(err)
		}
		if b.Files[0].Evidence == "" || b.Files[0].Evidence == b.Files[1].Evidence {
			t.Fatalf("evidence: %+v", b.Files)
		}
		return b.Files[0].Evidence
	}
	base := evidence()
	if evidence() != base {
		t.Fatal("evidence is not stable")
	}
	for name, change := range map[string][2]string{
		"sampled hash": {`"file_hash":"sampled-folder-hash","track_num":1`, `"file_hash":"replaced-folder-hash","track_num":1`},
		"size":         {`"file_size":3,"file_hash":"sampled-folder-hash","track_num":1`, `"file_size":4,"file_hash":"sampled-folder-hash","track_num":1`},
		"storage path": {`private/track01.mp3`, `private/moved/track01.mp3`},
		"absent hash":  {`"file_hash":"sampled-folder-hash","track_num":1`, `"track_num":1`},
	} {
		book = strings.Replace(audioFixture, change[0], change[1], 1)
		if book == audioFixture {
			t.Fatalf("%s: fixture unchanged", name)
		}
		if evidence() == base {
			t.Errorf("%s did not change the evidence", name)
		}
	}
}

func TestAudioEmptyPagesAcceptProviderNullArrays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/audiobooks" {
			io.WriteString(w, `{"audiobooks":null,"page":1,"limit":100,"total":0}`)
		} else {
			io.WriteString(w, `{"authors":null,"page":1,"limit":100,"total":0}`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	if page, err := c.Audiobooks(context.Background(), "", "", time.Time{}, 1, 100); err != nil || len(page.Books) != 0 {
		t.Fatalf("empty audio page: %v %v", page, err)
	}
	if page, err := c.Collections(context.Background(), "authors", 1, 100); err != nil || len(page.Collections) != 0 {
		t.Fatalf("empty collections: %v %v", page, err)
	}
}

func TestAudioCollectionIDsFitHostOpaqueIdentityBound(t *testing.T) {
	var row map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authors": []map[string]any{row}, "page": 1, "limit": 100, "total": 1})
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	row = map[string]any{"id": strings.Repeat("a", 749), "name": " Author "}
	page, err := c.Collections(context.Background(), "authors", 1, 100)
	if err != nil || len(page.Collections) != 1 || page.Collections[0].Name != "Author" || page.Collections[0].Spelling != " Author " {
		t.Fatalf("largest identity that fits: %v %v", page, err)
	}
	// authors/<base64url ID>.<spelling tag> must fit the host's 1024-byte limit.
	row = map[string]any{"id": strings.Repeat("a", 750), "name": "Author"}
	if _, err := c.Collections(context.Background(), "authors", 1, 100); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("identity cannot fit host 1024-byte limit: %v", err)
	}
	// A continuation carries the last spelling, so it must fit the cursor.
	row = map[string]any{"id": "author", "name": strings.Repeat("n", 1025)}
	if _, err := c.Collections(context.Background(), "authors", 1, 100); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("spelling cannot fit a continuation: %v", err)
	}
}

func TestAudioCollectionLookupReturnsEverySpelling(t *testing.T) {
	var body string
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		if strings.HasSuffix(path, "/absent") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	ctx := context.Background()
	body = `{"id":"m-r-forbes","name":"M. R. Forbes","names":["M. R. Forbes","M.R. Forbes"],"num_books":2,"audiobook_ids":["book-a","book-b"]}`
	got, err := c.Collection(ctx, "authors", "m-r-forbes")
	if err != nil || len(got) != 2 || got[1] != (Collection{ID: "m-r-forbes", Name: "M.R. Forbes", Spelling: "M.R. Forbes"}) || path != "/api/v1/audiobooks/authors/m-r-forbes" {
		t.Fatalf("author: %+v %v at %s", got, err, path)
	}
	body = `{"id":"émile-zola","name":"Émile Zola","names":[" Émile Zola"]}`
	if got, err := c.Collection(ctx, "series", "émile-zola"); err != nil || got[0].Name != "Émile Zola" || got[0].Spelling != " Émile Zola" || path != "/api/v1/audiobooks/series/%C3%A9mile-zola" {
		t.Fatalf("escaped series: %+v %v at %s", got, err, path)
	}
	if _, err := c.Collection(ctx, "authors", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent collection: %v", err)
	}
	body = `{"id":"someone-else","names":["Someone Else"]}`
	if _, err := c.Collection(ctx, "authors", "m-r-forbes"); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("answer for another collection: %v", err)
	}
	for _, kind := range []string{"", "books", "authors/x"} {
		if _, err := c.Collection(ctx, kind, "m-r-forbes"); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("kind %q: %v", kind, err)
		}
	}
}

func TestAudiobookDeletionsReadTheAudioFeed(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audiobooks/deletions" {
			t.Errorf("path %s", r.URL.Path)
		}
		query = r.URL.RawQuery
		io.WriteString(w, `{"deletions":[{"audiobook_id":"book-gone","title":"Gone","deleted_at":"2026-10-28T12:00:00Z"}],"pagination":{"page":2,"limit":100,"total_items":101,"total_pages":2}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	page, err := c.AudiobookDeletions(context.Background(), time.Date(2026, 10, 28, 11, 0, 0, 500, time.UTC), 2, 100)
	if err != nil || len(page.Deletions) != 1 || page.Deletions[0].ID != "book-gone" || page.Page != 2 {
		t.Fatalf("deletions: %+v %v", page, err)
	}
	if query != "limit=100&page=2&since=2026-10-28T11%3A00%3A00Z" {
		t.Fatalf("query %s", query)
	}
	if _, err := c.AudiobookDeletions(context.Background(), time.Now(), 3, 100); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("answer for another page: %v", err)
	}
}

// A whole Bible read as one file lists about 1,200 chapters, all kept.
func TestAudiobookKeepsABiblesChapters(t *testing.T) {
	var m map[string]any
	json.Unmarshal([]byte(audioFixture), &m)
	var list []any
	for i := range 1194 {
		list = append(list, map[string]any{"id": 100 + i, "audiobook_id": "book-a", "file_id": 41, "title": "Chapter", "start_seconds": float64(i) / 1000, "end_seconds": float64(i+1) / 1000, "is_accurate": true})
	}
	m["chapters"] = list
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(m) }))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	b, err := c.Audiobook(context.Background(), "book-a")
	if err != nil || len(b.Files[0].Chapters) != 1194 {
		t.Fatalf("chapters %d err %v", len(b.Files[0].Chapters), err)
	}
}

// Bookwarehouse lists a file's embedded chapters without a file and never
// accurate. A one-file book keeps them, its last ending with the file.
func TestAudiobookKeepsAOneFileBooksEmbeddedChapters(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(audioFixture), &m); err != nil {
		t.Fatal(err)
	}
	m["files"] = m["files"].([]any)[1:]
	m["chapters"] = []any{
		map[string]any{"id": 7, "audiobook_id": "book-a", "chapter_index": 1, "title": "Chapter 2", "start_seconds": 1, "end_seconds": 2.6, "is_accurate": false},
		map[string]any{"id": 6, "audiobook_id": "book-a", "chapter_index": 0, "title": "Chapter 1", "start_seconds": 0, "end_seconds": 1, "is_accurate": false},
		map[string]any{"id": 8, "audiobook_id": "book-a", "chapter_index": 2, "title": "After", "start_seconds": 2, "end_seconds": 3, "is_accurate": false},
		map[string]any{"id": 9, "audiobook_id": "book-a", "file_id": 41, "title": "Inaccurate", "start_seconds": 0, "end_seconds": 1, "is_accurate": false},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(m) }))
	defer srv.Close()
	c, _ := New(srv.URL, "key", true)
	defer c.Close()
	b, err := c.Audiobook(context.Background(), "book-a")
	if err != nil || len(b.Files) != 1 {
		t.Fatalf("book=%+v err=%v", b, err)
	}
	want := []AudioChapter{{ID: "6", Title: "Chapter 1", Start: 0, End: 1}, {ID: "7", Title: "Chapter 2", Start: 1, End: 2}}
	if !slices.Equal(b.Files[0].Chapters, want) {
		t.Fatalf("chapters %+v, want %+v", b.Files[0].Chapters, want)
	}
}
