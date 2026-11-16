package bookwarehouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFolderClientRejectsMalformedHierarchy(t *testing.T) {
	for _, body := range []string{
		`{"path":"A","exists":true,"directories":[{"name":"B","path":"outside/B"}],"next_after":""}`,
		`{"path":"A","exists":true,"directories":[{"name":"..","path":"A/.."}],"next_after":""}`,
		`{"path":"A","exists":true,"directories":[{"name":"B","path":"A/B"},{"name":"B","path":"A/B"}],"next_after":""}`,
		`{"path":"other","exists":true,"directories":[],"next_after":""}`,
		`{"path":"A","exists":true,"directories":null,"next_after":""}`,
		`{"path":"A","exists":false,"directories":[{"name":"B","path":"A/B"}],"next_after":""}`,
	} {
		t.Run(body, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if _, err := client.Folders(context.Background(), "epub", "A", "", 10); !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("accepted %s: %v", body, err)
			}
		})
	}
}

func TestFolderScanRejectsIgnoredScope(t *testing.T) {
	for _, path := range []string{"private/books/book.epub", "A-other/book.epub", "A/../book.epub", "/A/book.epub", ""} {
		t.Run(path, func(t *testing.T) {
			row := strings.Replace(fixtureBook, "private/books/book.epub", path, 1)
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"books":[%s],"pagination":{"page":1,"limit":100,"total_items":1,"total_pages":1}}`, row)
			})
			if _, err := client.ListFolderUpdatedSince(context.Background(), "epub", "A", time.Time{}, 1, 100); !errors.Is(err, ErrInvalidMetadata) {
				t.Fatalf("accepted ignored/invalid scope %q: %v", path, err)
			}
		})
	}
}

func TestDeepFolderScopeAllowsLongerFileKey(t *testing.T) {
	folder := strings.Repeat("a", 250) + "/" + strings.Repeat("b", 250) + "/" + strings.Repeat("c", 250)
	row := strings.Replace(fixtureBook, "private/books/book.epub", folder+"/book.epub", 1)
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"books":[%s],"pagination":{"page":1,"limit":100,"total_items":1,"total_pages":1}}`, row)
	})
	if page, err := client.ListFolderUpdatedSince(context.Background(), "epub", folder, time.Time{}, 1, 100); err != nil || len(page.Books) != 1 {
		t.Fatalf("deep scope: %+v %v", page, err)
	}
}
