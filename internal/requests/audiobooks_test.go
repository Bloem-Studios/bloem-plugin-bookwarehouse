package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	hashMP3  = "1111111111111111111111111111111111111111"
	hashM4B  = "c12fe1c06bba254a9dc9f519b335aa7c1367a88a"
	hashSilm = "3333333333333333333333333333333333333333"
)

func abbResult(title, format, size, slug, hash string, seeders int) map[string]any {
	return map[string]any{
		"title": title, "author": "J.R.R. Tolkien", "narrator": "", "format": format, "bitrate": "64 Kbps",
		"file_size": size, "torrent_url": "https://tracker.example/" + slug + ".torrent",
		"magnet_link": "magnet:?xt=urn:btih:" + strings.ToUpper(hash) + "&dn=" + slug, "seeders": seeders, "leechers": 1,
		"upload_date": "2024-01-02", "detail_page_url": "/abss/" + slug + "/",
	}
}

// fakeWarehouse answers with the response shapes of Bookwarehouse's source
// (handlers/torrent_downloads.go, handlers/audiobooks.go, middleware/auth.go).
type fakeWarehouse struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	results  []map[string]any
	search   int // non-zero: the search answers this status
	curators map[string]bool
	torrents map[string]map[string]any
	order    []string
	posts    []map[string]any
	searches []string
	keys     map[string]bool
	dropPost bool // create the record, then drop the connection
}

func newWarehouse(t *testing.T) *fakeWarehouse {
	f := &fakeWarehouse{t: t, curators: map[string]bool{curatorKey: true}, torrents: map[string]map[string]any{}, keys: map[string]bool{}}
	f.results = []map[string]any{
		abbResult("The Hobbit", "MP3", "320 MB", "the-hobbit-mp3", hashMP3, 30),
		abbResult("The Hobbit", "M4B", "300 MB", "the-hobbit-m4b", hashM4B, 5),
		abbResult("The Silmarillion", "M4B", "600 MB", "the-silmarillion", hashSilm, 50),
	}
	f.results[1]["narrator"] = "Andy Serkis"
	f.results[1]["asin"] = "B0099RKRTY"
	f.results[1]["detail_page_url"] = "https://tracker.example/abss/the-hobbit-m4b/"
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeWarehouse) base() string { return f.server.URL + "/warehouse" }

func (f *fakeWarehouse) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Header.Get("X-API-Key")
	f.keys[key] = true
	if key != storageKey && key != curatorKey {
		jsonStatus(w, 401, map[string]string{"error": "invalid or expired API key"})
		return
	}
	const prefix = "/warehouse/api/v1/audiobooks"
	switch {
	case r.Method == "GET" && r.URL.Path == prefix+"/search/audiobookbay":
		f.searches = append(f.searches, r.URL.RawQuery)
		if f.search != 0 {
			jsonStatus(w, f.search, map[string]string{"error": "Search failed: upstream unreachable"})
			return
		}
		jsonResponse(w, f.results)
	case r.URL.Path == prefix+"/torrents" && r.Method == "GET":
		title := strings.ToLower(r.URL.Query().Get("title"))
		rows := []map[string]any{}
		for i := len(f.order) - 1; i >= 0; i-- {
			row := f.torrents[f.order[i]]
			if row != nil && strings.Contains(strings.ToLower(row["title"].(string)), title) {
				rows = append(rows, row)
			}
		}
		jsonResponse(w, map[string]any{"torrents": rows, "pagination": map[string]int{"page": 1, "limit": 100, "total_items": len(rows), "total_pages": 1}})
	case r.URL.Path == prefix+"/torrents" && r.Method == "POST":
		if !f.curators[key] {
			jsonStatus(w, 403, map[string]string{"error": "missing permission: books.create"})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["title"] == nil || body["magnet_link"] == nil {
			jsonStatus(w, 400, map[string]string{"error": "Key: 'AddTorrentDownloadRequest.Title' Error:Field validation for 'Title' failed on the 'required' tag"})
			return
		}
		f.posts = append(f.posts, body)
		magnet := body["magnet_link"].(string)
		hash := strings.ToLower(strings.SplitN(strings.TrimPrefix(magnet, "magnet:?xt=urn:btih:"), "&", 2)[0])
		for _, row := range f.torrents {
			if row["infohash"] == hash {
				jsonStatus(w, 500, map[string]string{"error": "Failed to add torrent to download queue"})
				return
			}
		}
		id := fmt.Sprintf("8f14e45f-ceea-467f-a0e6-%012d", len(f.order)+1)
		row := map[string]any{
			"id": id, "title": body["title"], "author": body["author"], "magnet_link": magnet, "infohash": hash,
			"status": "downloading", "progress": 0.0, "retry_count": 0, "max_retries": 3, "qb_hash": hash,
			"created_at": "2026-11-12T10:00:00Z", "updated_at": "2026-11-12T10:00:00Z", "search_metadata": "{}",
		}
		f.torrents[id] = row
		f.order = append(f.order, id)
		if f.dropPost {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		jsonResponse(w, row)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, prefix+"/torrents/"):
		row := f.torrents[strings.TrimPrefix(r.URL.Path, prefix+"/torrents/")]
		if row == nil {
			jsonStatus(w, 404, map[string]string{"error": "torrent not found: sql: no rows in result set"})
			return
		}
		jsonResponse(w, row)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func (f *fakeWarehouse) set(id string, values map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range values {
		if v == nil {
			delete(f.torrents[id], k)
		} else {
			f.torrents[id][k] = v
		}
	}
}

func audioRouter(f *fakeWarehouse, edit ...func(*Settings)) *Router {
	all := append([]func(*Settings){func(s *Settings) { s.RequestAPIKey = curatorKey }}, edit...)
	return New(installation("audiobooks", f.base(), all...))
}

func audioReq(extra map[string]string) *pb.RequestDescriptor {
	ids := map[string]string{"bloem_request_id": audioRequest, "bloem_attempt": "1", "bloem_search_pass": "1", "bloem_ai_calls": "0", "language": "en", "author": "J.R.R. Tolkien", "isbn": isbn}
	for k, v := range extra {
		ids[k] = v
	}
	return &pb.RequestDescriptor{MediaType: "audiobook", Title: "The Hobbit", ExternalIds: ids}
}

func audioConn() *pb.RouterConnection {
	return &pb.RouterConnection{Id: "audio-conn", BaseUrl: "https://ignored.invalid", ApiKey: "ignored-host-key"}
}

func audioFulfill(extra map[string]string) *pb.FulfillRequest {
	return &pb.FulfillRequest{CapabilityId: AudiobooksCapability, Request: audioReq(extra), Connections: []*pb.RouterConnection{audioConn()}, Qualities: []*pb.RequestedQuality{{Id: "audiobook"}}}
}

func audioCheck(receipt string) *pb.CheckStatusRequest {
	return &pb.CheckStatusRequest{CapabilityId: AudiobooksCapability, Request: audioReq(nil), Connections: []*pb.RouterConnection{audioConn()}, Targets: []*pb.TargetRef{{Quality: "audiobook", ConnectionId: "audio-conn", ExternalId: receipt}}}
}

func TestAudiobookSearchSelectAddAndTrackToCompletion(t *testing.T) {
	f := newWarehouse(t)
	r := audioRouter(f)
	out, err := r.Fulfill(context.Background(), audioFulfill(nil))
	if err != nil {
		t.Fatal(err)
	}
	decision := hostParseDecision(t, out.Message, "select")
	if decision.Outcome != "handed_off" || decision.Chosen == nil || decision.Chosen.Ref != "abb:/abss/the-hobbit-m4b/" || decision.Chosen.Format != "M4B" || decision.Chosen.SizeMB != 300 || decision.Heuristic == nil || decision.AICalls != 0 {
		t.Fatalf("decision = %+v", decision)
	}
	if len(decision.Sources) != 1 || decision.Sources[0].Status != "ok" || len(decision.Queries) != 1 {
		t.Fatalf("sources/queries = %+v", decision)
	}
	if len(out.Targets) != 1 {
		t.Fatalf("targets = %v", out.Targets)
	}
	target := out.Targets[0]
	if target.ConnectionId != "audio-conn" || target.Quality != "audiobook" || target.Status != "downloading" || target.ExternalStatus != "downloading" {
		t.Fatalf("target = %v", target)
	}
	receipt := hostParseReceipt(t, target.ExternalId, 1)
	if receipt.Kind != "qbittorrent" || receipt.ID != hashM4B || !receipt.Confirmed || receipt.Adopted || receipt.Sel != "The Hobbit" || receipt.ExpMB != 300 || receipt.TorrentID == "" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if len(f.posts) != 1 {
		t.Fatalf("posts = %d", len(f.posts))
	}
	body := f.posts[0]
	for k, want := range map[string]string{"title": "The Hobbit", "author": "J.R.R. Tolkien", "narrator": "Andy Serkis", "format": "M4B", "file_size": "300 MB", "upload_date": "2024-01-02", "asin": "B0099RKRTY", "torrent_url": "https://tracker.example/the-hobbit-m4b.torrent"} {
		if body[k] != want {
			t.Errorf("posted %s = %v, want %q", k, body[k], want)
		}
	}
	if !strings.Contains(body["magnet_link"].(string), strings.ToUpper(hashM4B)) {
		t.Errorf("magnet = %v", body["magnet_link"])
	}
	if len(f.searches) != 1 || !strings.Contains(f.searches[0], "title=The+Hobbit") || !strings.Contains(f.searches[0], "author=J.R.R.+Tolkien") {
		t.Fatalf("search query = %v", f.searches)
	}
	if f.keys[storageKey] || !f.keys[curatorKey] {
		t.Fatalf("audiobook requests must use request_api_key: %v", f.keys)
	}

	for _, step := range []struct {
		values           map[string]any
		status, external string
	}{
		{map[string]any{"status": "pending"}, "queued", "queued"},
		{map[string]any{"status": "adding"}, "queued", "queued"},
		{map[string]any{"status": "downloading", "progress": 0.4}, "downloading", "downloading"},
		{map[string]any{"status": "seeding", "progress": 1.0}, "downloading", "downloading"},
		{map[string]any{"status": "importing"}, "downloading", "downloading"},
		{map[string]any{"status": "completed", "audiobook_id": "4c1d2e9a-7b7e-4a2b-9a51-0f7d6f3b2a10"}, "completed", "verified"},
	} {
		f.set(receipt.TorrentID, step.values)
		got, err := audioRouter(f).CheckStatus(context.Background(), audioCheck(target.ExternalId))
		if err != nil || len(got.Statuses) != 1 {
			t.Fatalf("%v: %v %v", step.values, got, err)
		}
		s := got.Statuses[0]
		hostCheckStatus(t, s)
		if s.Status != step.status || s.ExternalStatus != step.external || s.ConnectionId != "audio-conn" || s.Quality != "audiobook" {
			t.Fatalf("%v: status = %v", step.values, s)
		}
		if step.status != "completed" {
			if s.Message != "" {
				t.Fatalf("in-flight status must not carry a decision: %q", s.Message)
			}
			continue
		}
		verify := hostParseDecision(t, s.Message, "verify")
		if verify.Outcome != "verified" || verify.AudiobookID != "4c1d2e9a-7b7e-4a2b-9a51-0f7d6f3b2a10" {
			t.Fatalf("verify decision = %+v", verify)
		}
	}

	// A later pass for the same attempt adopts the existing torrent rather
	// than adding it again.
	again, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
	if err != nil || len(f.posts) != 1 || len(again.Targets) != 1 {
		t.Fatalf("again = %v err = %v posts = %d", again, err, len(f.posts))
	}
	if d := hostParseDecision(t, again.Message, "select"); d.Outcome != "adopted" {
		t.Fatalf("again decision = %+v", d)
	}
	if adopted := hostParseReceipt(t, again.Targets[0].ExternalId, 1); !adopted.Adopted || adopted.TorrentID != receipt.TorrentID || adopted.ID != hashM4B {
		t.Fatalf("adopted receipt = %+v", adopted)
	}
}

func TestAudiobookStatusEndings(t *testing.T) {
	for _, tc := range []struct {
		name             string
		values           map[string]any
		remove           bool
		status, external string
		issues           bool
	}{
		{name: "failed", values: map[string]any{"status": "failed", "error_message": "Import failed: synthetic"}, status: "failed", external: "client_error"},
		{name: "cancelled", values: map[string]any{"status": "cancelled"}, status: "failed", external: "deleted"},
		{name: "completed without an audiobook", values: map[string]any{"status": "completed", "audiobook_id": nil}, status: "review", external: "review", issues: true},
		{name: "unrecognised status", values: map[string]any{"status": "paused_by_operator"}, status: "unknown", external: "unknown"},
		{name: "record gone", remove: true, status: "unknown", external: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWarehouse(t)
			out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
			if err != nil || len(out.Targets) != 1 {
				t.Fatalf("%v %v", out, err)
			}
			receipt := hostParseReceipt(t, out.Targets[0].ExternalId, 1)
			if tc.remove {
				f.mu.Lock()
				delete(f.torrents, receipt.TorrentID)
				f.mu.Unlock()
			} else {
				f.set(receipt.TorrentID, tc.values)
			}
			got, err := audioRouter(f).CheckStatus(context.Background(), audioCheck(out.Targets[0].ExternalId))
			if err == nil {
				hostCheckStatus(t, got.Statuses[0])
			}
			if err != nil || got.Statuses[0].Status != tc.status || got.Statuses[0].ExternalStatus != tc.external {
				t.Fatalf("got = %v err = %v", got, err)
			}
			if tc.issues {
				d := hostParseDecision(t, got.Statuses[0].Message, "verify")
				if d.Outcome != "review" || len(d.Issues) == 0 {
					t.Fatalf("review decision = %+v", d)
				}
			}
			if strings.Contains(got.String(), "synthetic") {
				t.Fatal("backend text leaked")
			}
		})
	}
}

func TestAudiobookNoConfidentMatchRetriesNextPass(t *testing.T) {
	f := newWarehouse(t)
	f.results = f.results[2:] // only The Silmarillion
	out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
	if err != nil || len(out.Targets) != 0 || len(f.posts) != 0 {
		t.Fatalf("out = %v err = %v posts = %d", out, err, len(f.posts))
	}
	d := hostParseDecision(t, out.Message, "select")
	if d.Outcome != "no_match" || d.Detail != "no_release_found" || len(d.Candidates) != 1 || d.Candidates[0].Ref != "abb:/abss/the-silmarillion/" || d.Chosen != nil {
		t.Fatalf("decision = %+v", d)
	}
	f.results = append(f.results, abbResult("The Hobbit", "M4B", "300 MB", "the-hobbit-m4b", hashM4B, 5))
	out, err = audioRouter(f).Fulfill(context.Background(), audioFulfill(map[string]string{"bloem_search_pass": "2"}))
	if err != nil || len(out.Targets) != 1 || hostParseDecision(t, out.Message, "select").Outcome != "handed_off" {
		t.Fatalf("second pass = %v err = %v", out, err)
	}

	f.results = nil
	out, err = audioRouter(f).Fulfill(context.Background(), audioFulfill(map[string]string{"bloem_search_pass": "3"}))
	if d := hostParseDecision(t, out.Message, "select"); err != nil || d.Outcome != "no_match" || len(d.Sources) != 1 || d.Sources[0].Status != "empty" {
		t.Fatalf("empty search = %+v err = %v", d, err)
	}
}

func TestAudiobookSearchFailures(t *testing.T) {
	for _, tc := range []struct {
		code    int
		outcome string
		refused bool
	}{
		{code: 504, outcome: "unavailable"},
		{code: 500, outcome: "unavailable"},
		{code: 503, outcome: "unavailable"},
		{code: 403, refused: true},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			f := newWarehouse(t)
			f.search = tc.code
			out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
			if tc.refused {
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || len(out.Targets) != 0 || len(f.posts) != 0 {
				t.Fatalf("out = %v err = %v", out, err)
			}
			if d := hostParseDecision(t, out.Message, "select"); d.Outcome != tc.outcome || len(d.Sources) != 1 || d.Sources[0].Status != "error" {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestAudiobookExcludedApprovedAndUnusableReleases(t *testing.T) {
	t.Run("excluded ref", func(t *testing.T) {
		f := newWarehouse(t)
		out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(map[string]string{"bloem_excluded_refs": "abb:/abss/the-hobbit-m4b/"}))
		if err != nil || len(out.Targets) != 1 || hostParseReceipt(t, out.Targets[0].ExternalId, 1).ID != hashMP3 {
			t.Fatalf("out = %v err = %v", out, err)
		}
	})
	t.Run("approved candidate", func(t *testing.T) {
		f := newWarehouse(t)
		out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(map[string]string{"bloem_approved_candidate": "abb:/abss/the-hobbit-mp3/"}))
		if err != nil || len(out.Targets) != 1 || hostParseReceipt(t, out.Targets[0].ExternalId, 1).ID != hashMP3 {
			t.Fatalf("out = %v err = %v", out, err)
		}
		if d := hostParseDecision(t, out.Message, "select"); !d.AdminApproved || d.Chosen.Ref != "abb:/abss/the-hobbit-mp3/" {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("approved candidate gone", func(t *testing.T) {
		f := newWarehouse(t)
		out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(map[string]string{"bloem_approved_candidate": "abb:/abss/gone/"}))
		if err != nil || len(out.Targets) != 0 || hostParseDecision(t, out.Message, "select").Outcome != "no_match" || len(f.posts) != 0 {
			t.Fatalf("out = %v err = %v", out, err)
		}
	})
	t.Run("cancelled earlier torrent is never re-added", func(t *testing.T) {
		f := newWarehouse(t)
		f.torrents["old"] = map[string]any{"id": "old", "title": "The Hobbit", "infohash": hashM4B, "status": "cancelled", "magnet_link": "magnet:?xt=urn:btih:" + hashM4B}
		f.order = append(f.order, "old")
		out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
		if err != nil || len(out.Targets) != 1 || hostParseReceipt(t, out.Targets[0].ExternalId, 1).ID != hashMP3 || len(f.posts) != 1 {
			t.Fatalf("out = %v err = %v posts = %d", out, err, len(f.posts))
		}
	})
}

func TestAudiobookAmbiguousAddRecoversTheRecord(t *testing.T) {
	f := newWarehouse(t)
	f.dropPost = true
	out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
	if err != nil || len(out.Targets) != 1 || len(f.posts) != 1 {
		t.Fatalf("out = %v err = %v posts = %d", out, err, len(f.posts))
	}
	receipt := hostParseReceipt(t, out.Targets[0].ExternalId, 1)
	if receipt.Kind != "qbittorrent" || receipt.ID != hashM4B || receipt.TorrentID != f.order[0] {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestAudiobookReceiptGuards(t *testing.T) {
	f := newWarehouse(t)
	out, err := audioRouter(f).Fulfill(context.Background(), audioFulfill(nil))
	if err != nil || len(out.Targets) != 1 {
		t.Fatal(out, err)
	}
	moved := audioRouter(f, func(s *Settings) { s.BaseURL = f.server.URL + "/elsewhere" })
	got, err := moved.CheckStatus(context.Background(), audioCheck(out.Targets[0].ExternalId))
	if err != nil || got.Statuses[0].Status != "failed" || got.Statuses[0].ExternalStatus != "client_changed" {
		t.Fatalf("moved = %v err = %v", got, err)
	}
	// The host's recovery receipt after a lost Fulfill outcome.
	recovery := "abr1." + base64JSON(`{"kind":"unknown","id":"","request_id":"`+audioRequest+`","attempt":1,"client":"","confirmed":false,"adopted":false,"sel":"","exp_mb":0,"exp_dur_s":0}`)
	got, err = audioRouter(f).CheckStatus(context.Background(), audioCheck(recovery))
	if err != nil || got.Statuses[0].Status != "unknown" || got.Statuses[0].ExternalStatus != "absent" {
		t.Fatalf("recovery = %v err = %v", got, err)
	}
	foreign := audioCheck(out.Targets[0].ExternalId)
	foreign.Request.ExternalIds["bloem_attempt"] = "2"
	if _, err := audioRouter(f).CheckStatus(context.Background(), foreign); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("receipt from another attempt: %v", err)
	}
}

func TestAudiobookMissingCuratorPermission(t *testing.T) {
	f := newWarehouse(t)
	userOnly := New(installation("audiobooks", f.base())) // api_key is a User key, no request_api_key
	_, err := userOnly.Fulfill(context.Background(), audioFulfill(nil))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "Curator") || len(f.posts) != 0 {
		t.Fatalf("Fulfill = %v posts = %d", err, len(f.posts))
	}
	test, err := userOnly.TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()})
	if err != nil || test.Ok || !strings.Contains(test.Message, "Curator") {
		t.Fatalf("TestConnection = %v %v", test, err)
	}
	valid, err := userOnly.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()})
	if err != nil || !strings.Contains(valid.FormError, "Curator") {
		t.Fatalf("Validate = %v %v", valid, err)
	}
	_, err = userOnly.ListConfigOptions(context.Background(), &pb.ListConfigOptionsRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "Curator") {
		t.Fatalf("ListConfigOptions = %v", err)
	}
	if len(f.torrents) != 0 {
		t.Fatal("the permission probe must not create anything")
	}

	for name, r := range map[string]*Router{
		"request_api_key":           audioRouter(f),
		"api_key with Curator role": New(installation("audiobooks", f.base(), func(s *Settings) { s.APIKey = curatorKey })),
	} {
		test, err := r.TestConnection(context.Background(), &pb.TestConnectionRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()})
		if err != nil || !test.Ok {
			t.Fatalf("%s: TestConnection = %v %v", name, test, err)
		}
		valid, err := r.Validate(context.Background(), &pb.ValidateRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()})
		if err != nil || valid.FormError != "" || len(valid.FieldErrors) != 0 {
			t.Fatalf("%s: Validate = %v %v", name, valid, err)
		}
		if _, err := r.ListConfigOptions(context.Background(), &pb.ListConfigOptionsRequest{CapabilityId: AudiobooksCapability, Connection: audioConn()}); err != nil {
			t.Fatalf("%s: ListConfigOptions = %v", name, err)
		}
	}
	if len(f.torrents) != 0 {
		t.Fatal("the permission probe must not create anything")
	}
}

func TestAudiobookDescriptorAndQuality(t *testing.T) {
	f := newWarehouse(t)
	for name, in := range map[string]*pb.FulfillRequest{
		"book media type": func() *pb.FulfillRequest { x := audioFulfill(nil); x.Request.MediaType = "book"; return x }(),
		"no request id":   func() *pb.FulfillRequest { x := audioFulfill(map[string]string{"bloem_request_id": ""}); return x }(),
		"bad attempt":     audioFulfill(map[string]string{"bloem_attempt": "0"}),
		"bad excluded":    audioFulfill(map[string]string{"bloem_excluded_refs": "https://x"}),
		"4k quality": func() *pb.FulfillRequest {
			x := audioFulfill(nil)
			x.Qualities = []*pb.RequestedQuality{{Id: "2160p", Is4K: true}}
			return x
		}(),
		"two connections": func() *pb.FulfillRequest {
			x := audioFulfill(nil)
			x.Connections = append(x.Connections, audioConn())
			return x
		}(),
	} {
		if _, err := audioRouter(f).Fulfill(context.Background(), in); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.posts) != 0 || len(f.searches) != 0 {
		t.Fatal("an invalid request reached Bookwarehouse")
	}
}

// Each installation serves only the kind its content setting names.
func TestWrongContentKindIsRefused(t *testing.T) {
	f := newWarehouse(t)
	for _, tc := range []struct {
		content, capability string
	}{{"ebooks", AudiobooksCapability}, {"audiobooks", EbooksCapability}} {
		r := New(installation(tc.content, f.base(), func(s *Settings) { s.RequestAPIKey = curatorKey }))
		ctx := context.Background()
		c := audioConn()
		calls := map[string]func() error{
			"Fulfill": func() error {
				_, err := r.Fulfill(ctx, &pb.FulfillRequest{CapabilityId: tc.capability, Request: audioReq(nil), Connections: []*pb.RouterConnection{c}})
				return err
			},
			"CheckStatus": func() error {
				_, err := r.CheckStatus(ctx, &pb.CheckStatusRequest{CapabilityId: tc.capability, Request: audioReq(nil), Connections: []*pb.RouterConnection{c}})
				return err
			},
			"Validate": func() error {
				_, err := r.Validate(ctx, &pb.ValidateRequest{CapabilityId: tc.capability, Connection: c})
				return err
			},
			"ListConfigOptions": func() error {
				_, err := r.ListConfigOptions(ctx, &pb.ListConfigOptionsRequest{CapabilityId: tc.capability, Connection: c})
				return err
			},
			"TestConnection": func() error {
				_, err := r.TestConnection(ctx, &pb.TestConnectionRequest{CapabilityId: tc.capability, Connection: c})
				return err
			},
		}
		for name, call := range calls {
			err := call()
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), tc.content) {
				t.Errorf("%s installation, %s %s: %v", tc.content, tc.capability, name, err)
			}
		}
		if _, err := r.Validate(ctx, &pb.ValidateRequest{CapabilityId: "bookwarehouse", Connection: c}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("unknown capability: %v", err)
		}
	}
	if len(f.keys) != 0 {
		t.Fatal("a refused call reached Bookwarehouse")
	}
}
