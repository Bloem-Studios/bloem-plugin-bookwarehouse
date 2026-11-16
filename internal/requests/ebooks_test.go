package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEbookSubmitAndDurableStatus(t *testing.T) {
	var posts atomic.Int32
	completed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != storageKey {
			t.Error("ebook requests must use the installation's api_key")
		}
		switch r.URL.Path {
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring":
			if posts.Load() == 0 {
				emptyMonitoring(w)
			} else {
				jsonResponse(w, map[string]any{"requests": []any{map[string]any{"id": "monitor-1", "isbn13": isbn, "status": "searching", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
			}
		case "/api/v1/monitoring/add":
			posts.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["isbn"] != isbn || body["preferred_format"] != "epub" || len(body) != 2 {
				t.Errorf("body=%v", body)
			}
			jsonStatus(w, 202, map[string]string{"request_id": "monitor-1", "status": "searching", "message": "Download request queued successfully"})
		case "/api/v1/monitoring/monitor-1":
			state := "downloading"
			if completed {
				state = "completed"
			}
			jsonResponse(w, map[string]any{"id": "monitor-1", "isbn13": isbn, "status": state, "format": "epub", "book_id": "book-1"})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := conn(t, nil)
	router := ebookRouter(server.URL)
	out, err := router.Fulfill(context.Background(), req(c))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Targets) != 1 || out.Targets[0].Status != "queued" || out.Targets[0].ExternalId == "" || out.Targets[0].ConnectionId != "test" {
		t.Fatalf("target=%v", out)
	}
	again, err := router.Fulfill(context.Background(), req(c))
	if err != nil || posts.Load() != 1 || again.Targets[0].ExternalId != out.Targets[0].ExternalId {
		t.Fatalf("duplicate posts=%d response=%v err=%v", posts.Load(), again, err)
	}
	target := out.Targets[0]
	check := &pb.CheckStatusRequest{CapabilityId: EbooksCapability, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{Quality: target.Quality, ConnectionId: target.ConnectionId, ExternalId: target.ExternalId}}}
	for _, state := range []string{"downloading", "completed"} {
		completed = state == "completed"
		got, err := ebookRouter(server.URL).CheckStatus(context.Background(), check)
		if err != nil || len(got.Statuses) != 1 || got.Statuses[0].Status != state {
			t.Fatalf("state=%s got=%v err=%v", state, got, err)
		}
		if completed && !strings.Contains(got.Statuses[0].Message, "awaiting") {
			t.Fatal("missing library scan message")
		}
	}
}

// The receipt is the host's durable record; its field names and order are the
// documented bwh1 wire format and must not change.
func TestEbookReceiptWireFormat(t *testing.T) {
	got := encodeReceipt(receiptData{Kind: "monitoring", ID: "monitor-1", ISBN: isbn, Format: "epub", RequestID: bookRequest})
	want := "bwh1." + base64JSON(`{"kind":"monitoring","id":"monitor-1","isbn":"9780547928227","format":"epub","request_id":"stable-host-request"}`)
	if got != want {
		t.Fatalf("receipt = %s, want %s", got, want)
	}
}

func TestEbookPreflightExactISBNAndPresence(t *testing.T) {
	for _, kind := range []string{"monitoring", "book", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/books":
					if kind == "book" {
						jsonResponse(w, map[string]any{"books": []any{map[string]string{"id": "book-1", "isbn13": isbn, "file_format": "epub"}}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 1, "total_pages": 1}})
					} else {
						emptyBooks(w)
					}
				case "/api/v1/monitoring":
					if kind == "monitoring" {
						jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "existing", "isbn13": isbn, "status": "monitored", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
					} else {
						emptyMonitoring(w)
					}
				case "/api/v1/monitoring/add":
					posts++
					jsonStatus(w, 409, map[string]string{"error": "book_exists", "message": "book already exists (ID: synthetic-secret)"})
				default:
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			out, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil)))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "conflict" {
				want = 1
			}
			if posts != want || len(out.Targets) != 1 {
				t.Fatalf("posts=%d result=%v", posts, out)
			}
			if kind != "monitoring" && out.Targets[0].Status != "completed" {
				t.Fatal(out)
			}
			if kind == "conflict" && out.Targets[0].ExternalStatus != "book_exists" {
				t.Fatalf("duplicate must read as book_exists: %v", out)
			}
			if strings.Contains(out.String(), "synthetic-secret") {
				t.Fatal("secret exposed")
			}
		})
	}
}

// Bookwarehouse answers a 500 when the latest request for the ISBN already
// completed. That is neither a refusal nor proof of anything: keep an
// unknown receipt and let CheckStatus resolve it read-only.
func TestEbookAlreadyDownloadedIsUnknownNotRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring":
			emptyMonitoring(w)
		case "/api/v1/monitoring/add":
			jsonStatus(w, 500, map[string]string{"error": "book already downloaded (request: r, book_id: b)"})
		}
	}))
	defer server.Close()
	out, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil)))
	if err != nil || out.Targets[0].ExternalStatus != "unknown" || out.Targets[0].Status != "queued" {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestEbookStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		raw, errorMessage, status, external string
	}{
		{"monitored", "", "queued", "monitored"},
		{"searching_now", "", "queued", "searching_now"},
		{"not_found", "", "queued", "not_found"},
		{"found", "", "queued", "found"},
		{"found_pending", "", "queued", "found_pending"},
		{"grabbed", "", "downloading", "grabbed"},
		{"downloading", "", "downloading", "downloading"},
		{"processing", "", "downloading", "processing"},
		{"completed", "", "completed", "completed"},
		{"failed", "No matching download found", "failed", "failed"},
		{"failed", "Cancelled by user", "failed", "cancelled"},
		{"something_new", "", "failed", "unsupported"},
	} {
		t.Run(tc.raw+"/"+tc.external, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/monitoring/monitor-1" {
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				jsonResponse(w, map[string]any{"id": "monitor-1", "isbn13": isbn, "status": tc.raw, "format": "epub", "error_message": tc.errorMessage})
			}))
			defer server.Close()
			c := conn(t, nil)
			receipt := encodeReceipt(receiptData{Kind: "monitoring", ID: "monitor-1", ISBN: isbn, Format: "epub", RequestID: bookRequest})
			got, err := ebookRouter(server.URL).CheckStatus(context.Background(), &pb.CheckStatusRequest{CapabilityId: EbooksCapability, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: receipt}}})
			if err != nil || got.Statuses[0].Status != tc.status || got.Statuses[0].ExternalStatus != tc.external {
				t.Fatalf("got=%v err=%v", got, err)
			}
			if tc.external == "cancelled" && !strings.Contains(got.Statuses[0].Message, "ancel") {
				t.Fatalf("cancellation must read as a cancellation: %q", got.Statuses[0].Message)
			}
		})
	}
}

func TestEbookUnknownReceiptNeverSubmits(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		jsonStatus(w, 404, map[string]string{"error": "download request not found"})
	}))
	defer server.Close()
	c := conn(t, nil)
	receipt := encodeReceipt(receiptData{Kind: "monitoring", ID: "missing", ISBN: isbn, Format: "epub", RequestID: bookRequest})
	out, err := ebookRouter(server.URL).CheckStatus(context.Background(), &pb.CheckStatusRequest{CapabilityId: EbooksCapability, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: receipt}}})
	if err != nil || len(out.Statuses) != 1 || out.Statuses[0].Status != "failed" || posts != 0 {
		t.Fatalf("out=%v err=%v posts=%d", out, err, posts)
	}
}

func TestEbookConnectionAndHTTPFailures(t *testing.T) {
	for _, mode := range []string{"ok", "denied", "redirect", "large", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			hits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				switch mode {
				case "ok":
					emptyMonitoring(w)
				case "denied":
					jsonStatus(w, 401, map[string]string{"error": "synthetic-secret"})
				case "redirect":
					http.Redirect(w, r, "/escaped", http.StatusFound)
				case "large":
					_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
				case "malformed":
					_, _ = w.Write([]byte(`{"requests":null}`))
				}
			}))
			defer server.Close()
			got, err := ebookRouter(server.URL).TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: EbooksCapability, Connection: conn(t, nil)})
			if err != nil {
				t.Fatal(err)
			}
			if got.Ok != (mode == "ok") || strings.Contains(got.Message, "synthetic-secret") || strings.Contains(got.Message, server.URL) || hits != 1 {
				t.Fatalf("got=%v hits=%d", got, hits)
			}
		})
	}
}

func TestEbookValidationAndPDFGate(t *testing.T) {
	r := ebookRouter("http://localhost")
	input := req(conn(t, nil))
	for _, mutate := range []func(){func() { input.Request.MediaType = "audiobook" }, func() { input.Request.MediaType = "book"; input.Request.ExternalIds["isbn"] = "9780547928228" }, func() { input.Request.ExternalIds["isbn"] = isbn; input.Request.ExternalIds["bloem_request_id"] = "" }} {
		mutate()
		if _, err := r.Fulfill(context.Background(), input); err == nil {
			t.Fatal("invalid request admitted")
		}
	}
	for _, config := range []map[string]any{{"preferred_format": "pdf"}, {"allow_pdf": "true"}, {"preferred_format": "mobi"}} {
		got, err := r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: EbooksCapability, Connection: conn(t, config)})
		if err != nil || len(got.FieldErrors) == 0 {
			t.Fatalf("config=%v out=%v err=%v", config, got, err)
		}
	}
	got, err := r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: EbooksCapability, Connection: conn(t, map[string]any{"preferred_format": "pdf", "allow_pdf": true})})
	if err != nil || len(got.FieldErrors) != 0 || got.FormError != "" {
		t.Fatalf("valid PDF rejected: %v %v", got, err)
	}
	got, err = r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: EbooksCapability, Connection: conn(t, nil), Siblings: []*pb.RouterConnection{{Id: "other"}}})
	if err != nil || got.FormError == "" {
		t.Fatalf("second connection admitted: %v %v", got, err)
	}
	unconfigured := New(func() (Settings, bool) { return Settings{}, false })
	got, err = unconfigured.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: EbooksCapability, Connection: conn(t, nil)})
	if err != nil || !strings.Contains(got.FormError, "Bookwarehouse connection") {
		t.Fatalf("unconfigured installation: %v %v", got, err)
	}
	if _, err := unconfigured.Fulfill(context.Background(), req(conn(t, nil))); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unconfigured Fulfill: %v", err)
	}
}

func TestEbookAmbiguousSubmissionRecoversWithoutAnotherPOST(t *testing.T) {
	posts := 0
	known := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring":
			if known {
				jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "accepted", "isbn13": isbn, "status": "searching_now", "selected_format": "epub"}}, "page": 1, "limit": 100, "total": 1})
			} else {
				emptyMonitoring(w)
			}
		case "/api/v1/monitoring/add":
			posts++
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`not-json synthetic-secret`))
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := conn(t, nil)
	out, err := ebookRouter(server.URL).Fulfill(context.Background(), req(c))
	if err != nil || len(out.Targets) != 1 || out.Targets[0].ExternalStatus != "unknown" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	target := out.Targets[0]
	check := &pb.CheckStatusRequest{CapabilityId: EbooksCapability, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: target.ExternalId}}}
	got, err := ebookRouter(server.URL).CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].ExternalStatus != "unknown" || got.Statuses[0].Status != "queued" {
		t.Fatalf("absent out=%v err=%v", got, err)
	}
	known = true
	got, err = ebookRouter(server.URL).CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].ExternalStatus != "searching_now" || posts != 1 {
		t.Fatalf("recovered out=%v err=%v posts=%d", got, err, posts)
	}
}

func TestEbookPreflightRefusesInconsistentPagination(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		jsonResponse(w, map[string]any{"requests": []any{}, "page": 1, "limit": 100, "total": 1, "books": []any{}, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 1, "total_pages": 1}})
	}))
	defer server.Close()
	if _, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil))); err == nil || posts != 0 {
		t.Fatalf("err=%v posts=%d", err, posts)
	}
}

func TestEbookCancellationAfterSubmissionPreservesUnknownReceipt(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/monitoring":
			emptyMonitoring(w)
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring/add":
			close(started)
			<-release
			w.WriteHeader(202)
		}
	}))
	defer server.Close()
	c := conn(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		out *pb.FulfillResponse
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := ebookRouter(server.URL).Fulfill(ctx, req(c)); done <- result{out, err} }()
	<-started
	cancel()
	got := <-done
	close(release)
	if got.err != nil || len(got.out.Targets) != 1 || got.out.Targets[0].ExternalStatus != "unknown" {
		t.Fatalf("out=%v err=%v", got.out, got.err)
	}
	receipt, err := decodeReceipt(got.out.Targets[0].ExternalId)
	if err != nil || receipt.Kind != "unknown" || receipt.RequestID != bookRequest || receipt.ISBN != isbn {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func TestEbookPDFSubmissionAndOptions(t *testing.T) {
	for _, source := range []string{"connection", "installation"} {
		t.Run(source, func(t *testing.T) {
			posted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/monitoring":
					emptyMonitoring(w)
				case "/api/v1/books":
					if r.URL.Query().Get("format") != "pdf" {
						t.Error("wrong list format")
					}
					emptyBooks(w)
				case "/api/v1/monitoring/add":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["preferred_format"] != "pdf" {
						t.Error("wrong format")
					}
					posted = true
					jsonStatus(w, 202, map[string]string{"request_id": "pdf-1"})
				}
			}))
			defer server.Close()
			c := conn(t, map[string]any{"allow_pdf": true, "preferred_format": "pdf"})
			r := ebookRouter(server.URL)
			if source == "installation" {
				c = conn(t, nil)
				r = ebookRouter(server.URL, func(s *Settings) { s.PreferredFormat = "pdf" })
			}
			out, err := r.Fulfill(context.Background(), req(c))
			if err != nil || !posted || out.Targets[0].Quality != "pdf" {
				t.Fatalf("out=%v err=%v", out, err)
			}
			options, err := r.ListConfigOptions(context.Background(), &pb.ListConfigOptionsRequest{CapabilityId: EbooksCapability, Connection: c})
			if err != nil || len(options.OptionsByField["preferred_format"].Options) != 2 {
				t.Fatalf("options=%v err=%v", options, err)
			}
		})
	}
	options, err := ebookRouter("http://localhost").ListConfigOptions(context.Background(), &pb.ListConfigOptionsRequest{CapabilityId: EbooksCapability, Connection: conn(t, map[string]any{"allow_pdf": false})})
	if err != nil || len(options.OptionsByField["preferred_format"].Options) != 1 {
		t.Fatalf("options=%v err=%v", options, err)
	}
	// The host owns quality governance: a quality other than the effective
	// format is refused rather than silently substituted.
	in := req(conn(t, nil))
	in.Qualities = []*pb.RequestedQuality{{Id: "pdf"}}
	if _, err := ebookRouter("http://localhost").Fulfill(context.Background(), in); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mismatched quality: %v", err)
	}
}

func TestEbookReceiptAndBackendIdentityFailClosed(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		jsonResponse(w, map[string]string{"id": "monitor-1", "isbn13": "9780306406157", "format": "epub", "status": "completed", "error_message": "synthetic-secret"})
	}))
	defer server.Close()
	c := conn(t, nil)
	r := receiptData{Kind: "monitoring", ID: "monitor-1", ISBN: isbn, Format: "epub", RequestID: bookRequest}
	check := &pb.CheckStatusRequest{CapabilityId: EbooksCapability, Request: req(c).Request, Connections: []*pb.RouterConnection{c}, Targets: []*pb.TargetRef{{ConnectionId: c.Id, Quality: "epub", ExternalId: encodeReceipt(r)}}}
	got, err := ebookRouter(server.URL).CheckStatus(context.Background(), check)
	if err != nil || got.Statuses[0].Status != "failed" || strings.Contains(got.String(), "synthetic-secret") {
		t.Fatalf("got=%v err=%v", got, err)
	}
	check.Request.ExternalIds["bloem_request_id"] = "another-request"
	if _, err := ebookRouter(server.URL).CheckStatus(context.Background(), check); err == nil || hits != 1 {
		t.Fatal("foreign receipt read backend")
	}
}

func TestEbookRedirectNeverForwardsCredentials(t *testing.T) {
	leaks := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaks++ }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
	}))
	defer origin.Close()
	got, err := ebookRouter(origin.URL).TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: EbooksCapability, Connection: conn(t, nil)})
	if err != nil || got.Ok || leaks != 0 {
		t.Fatalf("got=%v err=%v leaked=%d", got, err, leaks)
	}
}

func TestEbookOnlyExactISBNFormatIsAdopted(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/monitoring":
			jsonResponse(w, map[string]any{"requests": []any{map[string]string{"id": "other", "isbn13": "9780306406157", "status": "completed", "selected_format": "epub"}, map[string]string{"id": "different-format", "isbn13": isbn, "status": "searching", "selected_format": "pdf"}}, "page": 1, "limit": 100, "total": 2})
		case "/api/v1/books":
			emptyBooks(w)
		case "/api/v1/monitoring/add":
			posts++
			jsonStatus(w, 202, map[string]string{"request_id": "new-epub"})
		}
	}))
	defer server.Close()
	out, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil)))
	if err != nil || posts != 1 || out.Targets[0].Status != "queued" {
		t.Fatalf("got=%v err=%v posts=%d", out, err, posts)
	}
}

func TestEbookLargeCatalogDoesNotBlockSubmission(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			var posts, lists atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/monitoring":
					emptyMonitoring(w)
				case "/api/v1/books":
					lists.Add(1)
					rows := make([]map[string]string, 100)
					for i := range rows {
						rows[i] = map[string]string{"id": fmt.Sprintf("book-%d", i), "file_format": "epub"}
					}
					jsonResponse(w, map[string]any{"books": rows, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": 245000, "total_pages": 2450}})
				case "/api/v1/monitoring/add":
					posts.Add(1)
					if present {
						jsonStatus(w, http.StatusConflict, map[string]string{"error": "book_exists"})
					} else {
						jsonStatus(w, http.StatusAccepted, map[string]string{"request_id": "monitor-large", "status": "searching"})
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			out, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil)))
			if err != nil {
				t.Fatalf("large catalog blocked request: %v", err)
			}
			if posts.Load() != 1 || lists.Load() != 1 || len(out.Targets) != 1 {
				t.Fatalf("posts=%d lists=%d response=%v", posts.Load(), lists.Load(), out)
			}
			want := "queued"
			if present {
				want = "completed"
			}
			if out.Targets[0].Status != want {
				t.Fatalf("status=%s want=%s", out.Targets[0].Status, want)
			}
		})
	}
}

func TestEbookPreflightFailureProvesNothingSubmitted(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		jsonStatus(w, http.StatusUnauthorized, map[string]string{"error": "synthetic-secret"})
	}))
	defer server.Close()
	_, err := ebookRouter(server.URL).Fulfill(context.Background(), req(conn(t, nil)))
	if status.Code(err) != codes.FailedPrecondition || posts.Load() != 0 {
		t.Fatalf("preflight must refuse before submission: code=%s posts=%d", status.Code(err), posts.Load())
	}
}
