package bookwarehouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// This fixture follows database.Book and GetBook at Bookwarehouse 12af1f9:
// GET returns a raw book; nullable integrity fields use file_* names.
const fixtureBook = `{"id":"book-1","cover_id":"cover-1","title":"The Book","language":"en","storage_path":"private/books/book.epub","has_cover":false,"file_size":3,"file_format":"epub","file_hash":"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-10-06T12:00:00Z","enriched":false,"enrichment_attempts":0,"calibre_enriched":false}`
const fixtureHash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"

func clientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "test-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestGetUsesActualRawBookAndHeaderAuthentication(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/books/book-1" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-API-Key") != "test-secret" || r.Header.Get("Authorization") != "" {
			t.Error("missing X-API-Key or unexpected alternate authentication")
		}
		io.WriteString(w, fixtureBook)
	})
	book, err := client.Get(context.Background(), "book-1")
	if err != nil {
		t.Fatal(err)
	}
	if book.ID != "book-1" || book.Title != "The Book" || book.Format != "epub" || book.SHA256 != fixtureHash || book.Size != 3 || book.UpdatedAt.Format(time.RFC3339) != "2026-10-06T12:00:00Z" {
		t.Fatalf("wrong mapped metadata: %+v", book)
	}
}

func TestListUsesStableSortAndActualPagination(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/v1/books" || len(q) != 5 || q.Get("page") != "2" || q.Get("limit") != "1" || q.Get("format") != "epub" || q.Get("sort") != "updated_at" || q.Get("order") != "asc" {
			t.Errorf("unexpected list request: %s", r.URL)
		}
		if r.Header.Get("X-API-Key") != "test-secret" {
			t.Error("missing header authentication")
		}
		fmt.Fprintf(w, `{"books":[%s],"pagination":{"page":2,"limit":1,"total_items":2,"total_pages":2}}`, fixtureBook)
	})
	page, err := client.List(context.Background(), "epub", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 1 || page.Books[0].ID != "book-1" || page.Page != 2 || page.Limit != 1 || page.TotalItems != 2 || page.TotalPages != 2 {
		t.Fatalf("wrong page: %+v", page)
	}
}

func TestListUpdatedSinceSendsWholeSecondUTCFilter(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if len(q) != 6 || q.Get("updated_after") != "2026-10-07T12:00:05Z" || q.Get("sort") != "updated_at" || q.Get("order") != "asc" || q.Get("page") != "3" {
			t.Errorf("unexpected list request: %s", r.URL)
		}
		fmt.Fprintf(w, `{"books":[%s],"pagination":{"page":3,"limit":1,"total_items":3,"total_pages":3}}`, fixtureBook)
	})
	since := time.Date(2026, 10, 7, 14, 0, 5, 900_000_000, time.FixedZone("CEST", 2*60*60))
	if page, err := client.ListUpdatedSince(context.Background(), "epub", since, 3, 1); err != nil || len(page.Books) != 1 {
		t.Fatalf("filtered page: %+v, %v", page, err)
	}
}

func TestEmptyLibraryIsAccepted(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"books":[],"pagination":{"page":1,"limit":100,"total_items":0,"total_pages":0}}`)
	})
	page, err := client.List(context.Background(), "pdf", 1, 100)
	if err != nil || len(page.Books) != 0 || page.TotalItems != 0 {
		t.Fatalf("empty page: %+v, %v", page, err)
	}
}

func TestProxyPrefixIsPreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/warehouse/api/v1/books/book-1" {
			t.Errorf("prefix lost: %s", r.URL.Path)
		}
		io.WriteString(w, fixtureBook)
	}))
	defer server.Close()
	client, err := New(server.URL+"/warehouse/", "test-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.Get(context.Background(), "book-1"); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsUnsafeURLsAndKeys(t *testing.T) {
	for _, raw := range []string{"", "example.com", "//example.com", "http://example.com", "ftp://example.com", "https://", "https://user:secret@example.com", "https://example.com?api_key=secret", "https://example.com#secret", "https://example.com#", "https://example.com?", "https://example.com:0", "https://example.com:65536", "https://example.com:bad", "https://example.com/a/../b", "https://example.com/a%2fb", "https://example.com/a%5cb", "https://example.com/a\\b"} {
		t.Run(raw, func(t *testing.T) {
			client, err := New(raw, "test-secret", false)
			if client != nil {
				client.Close()
			}
			if err == nil {
				t.Fatal("unsafe URL accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaks credential")
			}
		})
	}
	for _, key := range []string{"", " ", "bad\r\nInjected: value", strings.Repeat("x", 8193)} {
		if client, err := New("https://example.com", key, false); err == nil {
			client.Close()
			t.Fatal("invalid key accepted")
		}
	}
	for _, raw := range []string{"https://example.com", "https://example.com:443/base", "https://[::1]:8443/warehouse"} {
		client, err := New(raw, "test-secret", false)
		if err != nil {
			t.Fatalf("valid URL %q rejected: %v", raw, err)
		}
		client.Close()
	}
}

func TestInvalidRequestInputsNeverReachServer(t *testing.T) {
	var calls atomic.Int32
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 500) })
	for _, id := range []string{"", "../book", "book/a", "book%2fa", "book?secret=x", "book\r\n", strings.Repeat("a", 129)} {
		if _, err := client.Get(context.Background(), id); !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("Get(%q): %v", id, err)
		}
		if body, err := client.Download(context.Background(), id, 0); !errors.Is(err, ErrInvalidMetadata) {
			if body != nil {
				body.Close()
			}
			t.Errorf("Download(%q): %v", id, err)
		}
	}
	for _, tt := range []struct {
		format      string
		page, limit int
	}{{"mobi", 1, 10}, {"", 1, 10}, {"EPUB", 1, 10}, {"epub", 0, 10}, {"epub", -1, 10}, {"epub", 1, 0}, {"epub", 1, 101}, {"pdf", 2147483648, 1}} {
		if _, err := client.List(context.Background(), tt.format, tt.page, tt.limit); !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("List(%+v): %v", tt, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid inputs made %d requests", calls.Load())
	}
}

func TestGetRejectsMissingInvalidAndMismatchedMetadata(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		value       any
		remove      bool
	}{
		{name: "missing size", field: "file_size", remove: true}, {name: "null size", field: "file_size", value: nil}, {name: "negative size", field: "file_size", value: -1}, {name: "overflow size", field: "file_size", value: json.Number("9223372036854775808")},
		{name: "missing hash", field: "file_hash", remove: true}, {name: "null hash", field: "file_hash", value: nil}, {name: "short hash", field: "file_hash", value: "abc"}, {name: "nonhex hash", field: "file_hash", value: strings.Repeat("g", 64)},
		{name: "missing format", field: "file_format", remove: true}, {name: "unsupported format", field: "file_format", value: "mobi"},
		{name: "wrong id", field: "id", value: "book-2"}, {name: "traversal id", field: "id", value: "../book-1"}, {name: "missing id", field: "id", remove: true},
		{name: "missing timestamp", field: "updated_at", remove: true}, {name: "invalid timestamp", field: "updated_at", value: "yesterday"}, {name: "zero timestamp", field: "updated_at", value: "0001-01-01T00:00:00Z"},
		{name: "timestamp before nanosecond range", field: "updated_at", value: "1600-01-01T00:00:00Z"},
		{name: "timestamp after nanosecond range", field: "updated_at", value: "2500-01-01T00:00:00Z"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var book map[string]any
			if err := json.Unmarshal([]byte(fixtureBook), &book); err != nil {
				t.Fatal(err)
			}
			if tt.remove {
				delete(book, tt.field)
			} else {
				book[tt.field] = tt.value
			}
			payload, err := json.Marshal(book)
			if err != nil {
				t.Fatal(err)
			}
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.Write(payload) })
			if got, err := client.Get(context.Background(), "book-1"); !errors.Is(err, ErrInvalidMetadata) || got.ID != "" {
				t.Fatalf("invalid metadata accepted: %+v, %v", got, err)
			}
		})
	}
}

func TestMetadataSupportsZeroSizeAndNormalizesHexHash(t *testing.T) {
	payload := strings.Replace(fixtureBook, `"file_size":3`, `"file_size":0`, 1)
	payload = strings.Replace(payload, fixtureHash, strings.ToUpper(fixtureHash), 1)
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) })
	book, err := client.Get(context.Background(), "book-1")
	if err != nil || book.Size != 0 || book.SHA256 != fixtureHash {
		t.Fatalf("valid zero/uppercase metadata: %+v, %v", book, err)
	}
}

func TestGetRejectsMalformedUnboundedAndEnvelopeResponses(t *testing.T) {
	for _, payload := range []string{"", `null`, `{}`, `{"book":` + fixtureBook + `}`, fixtureBook + `{}`, strings.Repeat(" ", 8*1024*1024) + fixtureBook, strings.Replace(fixtureBook, `"file_size":3`, `"file_size":"server-secret"`, 1)} {
		t.Run(fmt.Sprintf("length-%d", len(payload)), func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) })
			_, err := client.Get(context.Background(), "book-1")
			if !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("expected invalid metadata, got %v", err)
			}
			if strings.Contains(err.Error(), "server-secret") {
				t.Fatal("decode error leaked server data")
			}
		})
	}
}

func TestListRejectsInvalidPaginationAndBooks(t *testing.T) {
	for _, payload := range []string{
		`{}`, `{"books":[],"pagination":null}`, `{"pagination":{"page":1,"limit":1,"total_items":0,"total_pages":0}}`,
		`{"books":null,"pagination":{"page":1,"limit":1,"total_items":0,"total_pages":0}}`,
		`{"books":[],"pagination":{"page":2,"limit":1,"total_items":0,"total_pages":0}}`,
		`{"books":[],"pagination":{"page":1,"limit":2,"total_items":0,"total_pages":0}}`,
		`{"books":[],"pagination":{"page":1,"limit":1,"total_items":-1,"total_pages":0}}`,
		`{"books":[],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":0}}`,
		`{"books":[],"pagination":{"page":1,"limit":1,"total_items":2147483648,"total_pages":2147483648}}`,
		`{"books":[],"pagination":{"page":1,"limit":1,"total_items":0}}`,
		`{"books":[` + fixtureBook + `,` + fixtureBook + `],"pagination":{"page":1,"limit":1,"total_items":2,"total_pages":2}}`,
		`{"books":[` + strings.Replace(fixtureBook, `"file_format":"epub"`, `"file_format":"pdf"`, 1) + `],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":1}}`,
		`{"books":[` + strings.Replace(fixtureBook, `"id":"book-1"`, `"id":"../book-1"`, 1) + `],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":1}}`,
		`{"books":[` + strings.Replace(fixtureBook, `"updated_at":"2026-10-06T12:00:00Z"`, `"updated_at":"yesterday"`, 1) + `],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":1}}`,
		`{"books":[` + strings.Replace(fixtureBook, `,"updated_at":"2026-10-06T12:00:00Z"`, "", 1) + `],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":1}}`,
		`{"books":["not an object"],"pagination":{"page":1,"limit":1,"total_items":1,"total_pages":1}}`,
	} {
		t.Run(payload[:min(len(payload), 50)], func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) })
			if page, err := client.List(context.Background(), "epub", 1, 1); !errors.Is(err, ErrInvalidMetadata) || len(page.Books) != 0 {
				t.Fatalf("invalid page accepted: %+v, %v", page, err)
			}
		})
	}
}

func TestStatusErrorsAreClassifiedWithoutResponseDetails(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   error
	}{{401, ErrDenied}, {403, ErrDenied}, {404, ErrNotFound}, {400, ErrUnavailable}, {429, ErrUnavailable}, {500, ErrUnavailable}, {503, ErrUnavailable}, {204, ErrUnavailable}, {206, ErrUnavailable}} {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, `{"error":"server-secret at /private/files, key=test-secret"}`)
			})
			_, getErr := client.Get(context.Background(), "book-1")
			_, listErr := client.List(context.Background(), "epub", 1, 1)
			body, downloadErr := client.Download(context.Background(), "book-1", 0)
			if body != nil {
				body.Close()
				t.Fatal("error download exposed body")
			}
			for _, err := range []error{getErr, listErr, downloadErr} {
				if !errors.Is(err, tt.want) {
					t.Errorf("wrong classification: %v", err)
				}
				if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private")) {
					t.Fatalf("error leaked details: %v", err)
				}
			}
		})
	}
}

func TestRedirectsNeverForwardCredentialsOrReachTargets(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, fixtureBook) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL+"/secret")
				w.WriteHeader(status)
			})
			_, getErr := client.Get(context.Background(), "book-1")
			_, listErr := client.List(context.Background(), "epub", 1, 1)
			body, downloadErr := client.Download(context.Background(), "book-1", 0)
			if body != nil {
				body.Close()
			}
			for _, err := range []error{getErr, listErr, downloadErr} {
				if !errors.Is(err, ErrUnavailable) {
					t.Errorf("redirect was not rejected: %v", err)
				}
			}
		})
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls.Load())
	}
}

func TestDownloadReturnsFullBodyWithoutRangeOrRevisionConditions(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/books/book-1/download" || r.Header.Get("X-API-Key") != "test-secret" {
			t.Errorf("unexpected download: %s %s", r.Method, r.URL)
		}
		for _, header := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Unmodified-Since"} {
			if r.Header.Get(header) != "" {
				t.Errorf("unexpected %s", header)
			}
		}
		io.WriteString(w, "abc")
	})
	body, err := client.Download(context.Background(), "book-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "abc" {
		t.Fatalf("download: %q, %v", data, err)
	}
}

func TestCancellationAbortsPendingRequests(t *testing.T) {
	for _, op := range []string{"get", "list", "download"} {
		t.Run(op, func(t *testing.T) {
			started := make(chan struct{})
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				switch op {
				case "get":
					_, err := client.Get(ctx, "book-1")
					done <- err
				case "list":
					_, err := client.List(ctx, "epub", 1, 1)
					done <- err
				case "download":
					body, err := client.Download(ctx, "book-1", 0)
					if body != nil {
						body.Close()
					}
					done <- err
				}
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("request did not start: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("request ignored cancellation")
			}
		})
	}
}

func TestCancellationInterruptsDownloadBody(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := client.Download(ctx, "book-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	cancel()
	_, err = io.ReadAll(body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("body ignored cancellation: %v", err)
	}
}

func TestTransportErrorsDoNotExposeEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	endpoint := server.URL + "/private-secret"
	server.Close()
	client, err := New(endpoint, "test-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Get(context.Background(), "book-1")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

func TestListRejectsDuplicateIDs(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"books":[%s,%s],"pagination":{"page":1,"limit":2,"total_items":2,"total_pages":1}}`, fixtureBook, fixtureBook)
	})
	if page, err := client.List(context.Background(), "epub", 1, 2); !errors.Is(err, ErrInvalidMetadata) || len(page.Books) != 0 {
		t.Fatalf("duplicate IDs accepted: %+v %v", page, err)
	}
}

func TestListTreatsValidTotalsAsAdvisoryForActualRowCount(t *testing.T) {
	for _, tt := range []struct {
		name, books                     string
		page, limit, total, pages, rows int
	}{
		{"count ahead of rows", "[]", 1, 2, 2, 1, 0},
		{"count behind rows", "[" + fixtureBook + "]", 2, 1, 1, 1, 1},
		{"zero count with row", "[" + fixtureBook + "]", 1, 1, 0, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"books":%s,"pagination":{"page":%d,"limit":%d,"total_items":%d,"total_pages":%d}}`, tt.books, tt.page, tt.limit, tt.total, tt.pages)
			})
			page, err := client.List(context.Background(), "epub", tt.page, tt.limit)
			if err != nil || len(page.Books) != tt.rows || page.TotalItems != tt.total || page.TotalPages != tt.pages {
				t.Fatalf("actual rows rejected by advisory count: %+v %v", page, err)
			}
		})
	}
}

func TestListAcceptsEmptyPageAfterTheLastPage(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"books":[],"pagination":{"page":2,"limit":100,"total_items":2,"total_pages":1}}`)
	})
	page, err := client.List(context.Background(), "epub", 2, 100)
	if err != nil || len(page.Books) != 0 || page.Page != 2 {
		t.Fatalf("empty trailing page: %+v, %v", page, err)
	}
}

func TestResponseBodyErrorsDoNotExposeWireContent(t *testing.T) {
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		buffer.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nserver-secret-invalid-chunk\r\n")
		buffer.Flush()
	})
	_, err := client.Get(context.Background(), "book-1")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("metadata read leaked details: %v", err)
	}
	body, err := client.Download(context.Background(), "book-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	_, err = io.ReadAll(body)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("download read leaked details: %v", err)
	}
}

func TestHTTPSRequiresTrustedCertificate(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureBook) }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	client, err := New(server.URL, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Get(context.Background(), "book-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("untrusted certificate was accepted: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("credentials reached an untrusted TLS server")
	}
}

func TestCancellationWhileReadingMetadataAndCallerDeadline(t *testing.T) {
	started := make(chan struct{})
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":`)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Get(ctx, "book-1"); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("metadata cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata read ignored cancellation")
	}
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	if _, err := client.Get(expired, "book-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline lost: %v", err)
	}
}

func TestCloseReleasesIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, fixtureBook) }))
	server.Config.ConnState = func(connection net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	client, err := New(server.URL, "test-secret", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Get(context.Background(), "book-1"); err != nil {
		t.Fatal(err)
	}
	client.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle connection was retained")
	}
}

func TestCloseDoesNotInterruptActiveDownload(t *testing.T) {
	release := make(chan struct{})
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "abc")
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := client.Download(ctx, "book-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	client.Close()
	close(release)
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "abc" {
		t.Fatalf("Close interrupted active download: %q, %v", data, err)
	}
}

func TestDownloadResumesFromOffsetWithSingleByteRange(t *testing.T) {
	for _, tt := range []struct {
		name          string
		reply         func(w http.ResponseWriter)
		offset, total int64
		body          string
		want          error
	}{
		{name: "partial content", offset: 3, total: 6, body: "def", reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 3-5/6")
			w.Header().Set("Content-Length", "3")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "def")
		}},
		// A server may ignore Range; the whole object then starts at zero.
		{name: "range ignored", offset: 0, total: 6, body: "abcdef", reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "6")
			io.WriteString(w, "abcdef")
		}},
		{name: "object shorter than offset", want: ErrRangeNotSatisfiable, reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes */2")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}},
		{name: "range from another offset", want: ErrUnavailable, reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 0-5/6")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "abcdef")
		}},
		{name: "range short of the end", want: ErrUnavailable, reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 3-4/6")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "de")
		}},
		{name: "unknown total", want: ErrUnavailable, reply: func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 3-5/*")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "def")
		}},
		{name: "missing content range", want: ErrUnavailable, reply: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "def")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/books/book-1/download" || r.Header.Get("Range") != "bytes=3-" {
					t.Errorf("unexpected resume request %s Range=%q", r.URL, r.Header.Get("Range"))
				}
				for _, header := range []string{"If-Range", "If-Match", "If-None-Match", "If-Unmodified-Since"} {
					if r.Header.Get(header) != "" {
						t.Errorf("unexpected %s", header)
					}
				}
				tt.reply(w)
			})
			body, err := client.Download(context.Background(), "book-1", 3)
			if tt.want != nil {
				if body != nil {
					body.Close()
					t.Fatal("failed resume exposed a body")
				}
				if !errors.Is(err, tt.want) {
					t.Fatalf("resume error = %v, want %v", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			data, err := io.ReadAll(body)
			if err != nil || string(data) != tt.body || body.Offset != tt.offset || body.Total != tt.total {
				t.Fatalf("resume = %q at %d of %d, %v", data, body.Offset, body.Total, err)
			}
		})
	}
	if body, err := clientFor(t, func(http.ResponseWriter, *http.Request) { t.Error("negative offset reached the server") }).Download(context.Background(), "book-1", -1); body != nil || !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("negative offset: %v", err)
	}
}

func TestFullDownloadReportsDeclaredLength(t *testing.T) {
	for _, tt := range []struct {
		name   string
		length string
		total  int64
	}{{"declared", "3", 3}, {"chunked", "", -1}} {
		t.Run(tt.name, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.length != "" {
					w.Header().Set("Content-Length", tt.length)
				} else {
					w.(http.Flusher).Flush() // Forces chunked encoding.
				}
				io.WriteString(w, "abc")
			})
			body, err := client.Download(context.Background(), "book-1", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			if body.Offset != 0 || body.Total != tt.total {
				t.Fatalf("declared %d at %d, want %d", body.Total, body.Offset, tt.total)
			}
		})
	}
}

func TestAuthorizeProbesDownloadAccessWithOneByteRange(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply func(w http.ResponseWriter)
		want  error
	}{
		{"partial content", func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 0-0/6")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "a")
		}, nil},
		{"range ignored", func(w http.ResponseWriter) { io.WriteString(w, "abcdef") }, nil},
		{"empty book", func(w http.ResponseWriter) { w.WriteHeader(http.StatusRequestedRangeNotSatisfiable) }, nil},
		{"denied", func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) }, ErrDenied},
		{"unauthenticated", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) }, ErrDenied},
		{"missing", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }, ErrNotFound},
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, ErrUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/books/book-1/download" || r.Header.Get("Range") != "bytes=0-0" || r.Header.Get("X-API-Key") != "test-secret" {
					t.Errorf("unexpected probe %s %s Range=%q", r.Method, r.URL, r.Header.Get("Range"))
				}
				tt.reply(w)
			})
			if err := client.Authorize(context.Background(), "book-1"); !errors.Is(err, tt.want) {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
		})
	}
	if err := clientFor(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid ID reached the server") }).Authorize(context.Background(), "../x"); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("invalid ID: %v", err)
	}
}

func TestListSkipsRowsThatCannotBePublished(t *testing.T) {
	row := func(id string, edit func(map[string]any)) string {
		var book map[string]any
		if err := json.Unmarshal([]byte(fixtureBook), &book); err != nil {
			t.Fatal(err)
		}
		book["id"] = id
		edit(book)
		payload, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	rows := []string{
		row("ok-1", func(map[string]any) {}),
		row("null-size", func(b map[string]any) { b["file_size"] = nil }),
		row("no-hash", func(b map[string]any) { delete(b, "file_hash") }),
		row("bad-hash", func(b map[string]any) { b["file_hash"] = "abc" }),
		row("null-format", func(b map[string]any) { b["file_format"] = nil }),
		row("typed-size", func(b map[string]any) { b["file_size"] = "three" }),
		row("long-title", func(b map[string]any) { b["title"] = strings.Repeat("\u00e9", 3000) }),
	}
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"books":[%s],"pagination":{"page":1,"limit":100,"total_items":%d,"total_pages":1}}`, strings.Join(rows, ","), len(rows))
	})
	page, err := client.List(context.Background(), "epub", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != len(rows) {
		t.Fatalf("listed %d of %d rows", len(page.Books), len(rows))
	}
	for _, b := range page.Books {
		skipped := b.Problem != ""
		if want := b.ID != "ok-1" && b.ID != "long-title"; skipped != want {
			t.Errorf("%s skipped=%v (%q), want %v", b.ID, skipped, b.Problem, want)
		}
		if b.UpdatedAt.IsZero() {
			t.Errorf("%s lost its listing key", b.ID)
		}
	}
	title := page.Books[6].Title
	if len(title) > 4096 || !utf8.ValidString(title) || !strings.HasPrefix(strings.Repeat("\u00e9", 3000), title) {
		t.Fatalf("title not truncated at a rune boundary: %d bytes", len(title))
	}
}

func TestGetTruncatesOversizedTitle(t *testing.T) {
	payload := strings.Replace(fixtureBook, `"title":"The Book"`, `"title":"`+strings.Repeat("x", 5000)+`"`, 1)
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, payload) })
	book, err := client.Get(context.Background(), "book-1")
	if err != nil || len(book.Title) != 4096 {
		t.Fatalf("oversized title: %d bytes, %v", len(book.Title), err)
	}
}
