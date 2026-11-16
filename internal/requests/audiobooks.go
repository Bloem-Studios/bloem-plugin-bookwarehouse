package requests

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Audiobook requests go through Bookwarehouse's built-in torrents: search
// AudiobookBay through Bookwarehouse, choose a release (Choose), add it with
// POST /audiobooks/torrents, and follow /audiobooks/torrents/{id} until
// Bookwarehouse imports it and reports its audiobook_id. Adding a torrent
// needs the Curator role (books.create).

const (
	curatorMissing  = "The Bookwarehouse API key used for audiobook requests lacks the Curator role (permission books.create), so it cannot add audiobook torrents. Set request_api_key to a Curator key."
	keyRejected     = "Bookwarehouse rejected the API key used for audiobook requests."
	torrentsOff     = "Bookwarehouse's audiobook torrent downloads are not enabled (audiobook.qbittorrent)."
	searchForbidden = "The Bookwarehouse API key used for audiobook requests may not search AudiobookBay (permission search.external)."
	searchMissing   = "This Bookwarehouse has no AudiobookBay search."
	unusable        = "The installation's Bookwarehouse address or audiobook API key is not usable."
)

var (
	uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	// refPattern is the host's candidate reference pattern.
	refPattern = regexp.MustCompile(`^(abb:/abss/[A-Za-z0-9._~%-]{1,200}/|abook:[0-9]{1,10}|nzbking:[a-f0-9]{8,64})$`)
)

type audioDescriptor struct {
	want     Wanted
	id       string
	attempt  int
	excluded map[string]bool
	approved string
}

func printable(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// parseAudioDescriptor reads the host's audiobook request descriptor
// (bloem-server audiobookRequestDescriptor).
func parseAudioDescriptor(req *pb.RequestDescriptor) (audioDescriptor, error) {
	bad := status.Error(codes.InvalidArgument, "Invalid audiobook request descriptor.")
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if req == nil || req.GetMediaType() != "audiobook" || len(req.GetSeasons()) != 0 || oneLine(req.Title) == "" || !printable(oneLine(req.Title), 500) {
		return audioDescriptor{}, bad
	}
	e := req.ExternalIds
	d := audioDescriptor{
		want:     Wanted{Title: oneLine(req.Title), Author: oneLine(e["author"]), Narrator: oneLine(e["narrator"])},
		id:       e["bloem_request_id"],
		excluded: map[string]bool{},
		approved: e["bloem_approved_candidate"],
	}
	if !uuidPattern.MatchString(d.id) || !printable(d.want.Author, 500) || !printable(d.want.Narrator, 500) {
		return audioDescriptor{}, bad
	}
	attempt, err := strconv.Atoi(e["bloem_attempt"])
	if err != nil || attempt < 1 || attempt > 1000 {
		return audioDescriptor{}, bad
	}
	d.attempt = attempt
	for key, limit := range map[string]int{"bloem_search_pass": 1_000_000, "bloem_ai_calls": 1_000_000} {
		if v, ok := e[key]; ok {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > limit {
				return audioDescriptor{}, bad
			}
		}
	}
	if refs := e["bloem_excluded_refs"]; refs != "" {
		parts := strings.Split(refs, ",")
		if len(parts) > 10 {
			return audioDescriptor{}, bad
		}
		for _, ref := range parts {
			if !refPattern.MatchString(ref) {
				return audioDescriptor{}, bad
			}
			d.excluded[ref] = true
		}
	}
	if d.approved != "" && (!refPattern.MatchString(d.approved) || d.excluded[d.approved]) {
		return audioDescriptor{}, bad
	}
	return d, nil
}

// torrentRecord is the part of Bookwarehouse's MonitoredAudiobookTorrent
// this plugin reads.
type torrentRecord struct {
	ID          string  `json:"id"`
	AudiobookID *string `json:"audiobook_id"`
	Title       string  `json:"title"`
	Infohash    string  `json:"infohash"`
	Status      string  `json:"status"`
}

func (t torrentRecord) hash() string { return normalizeHash(t.Infohash) }

// findTorrent looks for Bookwarehouse's record of a torrent by info hash,
// through the title filter (Bookwarehouse has no info-hash filter and
// refuses a second record with the same info hash).
func findTorrent(ctx context.Context, cl *client, title, hash string) (*torrentRecord, error) {
	var page struct {
		Torrents []torrentRecord `json:"torrents"`
	}
	query := url.Values{"title": {title}, "page": {"1"}, "limit": {"100"}}
	if code, err := cl.json(ctx, http.MethodGet, "/audiobooks/torrents", query, nil, &page); err != nil || code != 200 || page.Torrents == nil {
		return nil, errBackend
	}
	for _, t := range page.Torrents {
		if t.hash() == hash && validID(t.ID) {
			return &t, nil
		}
	}
	return nil, nil
}

func (r *Router) fulfillAudiobook(ctx context.Context, s Settings, in *pb.FulfillRequest) (*pb.FulfillResponse, error) {
	if len(in.GetConnections()) != 1 || in.Connections[0] == nil {
		return nil, status.Error(codes.InvalidArgument, "One audiobook connection is required.")
	}
	if q := in.GetQualities(); len(q) > 1 || (len(q) == 1 && (q[0].GetId() != "audiobook" || q[0].GetIs4K())) {
		return nil, status.Error(codes.InvalidArgument, "Unsupported audiobook quality.")
	}
	d, err := parseAudioDescriptor(in.Request)
	if err != nil {
		return nil, err
	}
	conn := in.Connections[0]
	cl, err := newClient(s.BaseURL, s.audiobookKey(), s.AllowHTTP, 60*time.Second)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, unusable)
	}
	defer cl.close()
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := bounded(ctx, 70*time.Second, 5*time.Second)
	defer cancel()

	e := decision{V: 1, Phase: "select", Outcome: "no_match", AdminApproved: d.approved != ""}
	e.Queries = []string{text(strings.TrimSpace(d.want.Title+" "+d.want.Author), 120)}
	answer := func(outcome string) *pb.FulfillResponse {
		e.Outcome = outcome
		return &pb.FulfillResponse{Message: e.encode()}
	}
	if ctx.Err() != nil {
		e.Detail = "deferred"
		return answer("unavailable"), nil
	}

	query := url.Values{"title": {d.want.Title}}
	if d.want.Author != "" {
		query.Set("author", d.want.Author)
	}
	var releases []Release
	searchCtx, done := bounded(ctx, 50*time.Second, 10*time.Second)
	code, err := cl.json(searchCtx, http.MethodGet, "/audiobooks/search/audiobookbay", query, nil, &releases)
	done()
	switch {
	case code == http.StatusUnauthorized:
		return nil, status.Error(codes.FailedPrecondition, keyRejected)
	case code == http.StatusForbidden:
		return nil, status.Error(codes.FailedPrecondition, searchForbidden)
	case code == http.StatusNotFound:
		return nil, status.Error(codes.FailedPrecondition, searchMissing)
	case err != nil || code != http.StatusOK:
		e.Sources = []decisionSource{{Name: "audiobookbay", Status: "error"}}
		return answer("unavailable"), nil
	case len(releases) == 0:
		e.Sources = []decisionSource{{Name: "audiobookbay", Status: "empty"}}
		e.Detail = "no_release_found"
		return answer("no_match"), nil
	}
	e.Sources = []decisionSource{{Name: "audiobookbay", Status: "ok"}}
	releases = releases[:min(len(releases), 50)]

	// Choose, then check Bookwarehouse's own record of that torrent: an
	// active one is adopted, a failed or cancelled one can never be added
	// again, so the next-best release is tried.
	skip := map[int]bool{}
	for i, rel := range releases {
		if ref := releaseRef(rel); ref != "" && d.excluded[ref] {
			skip[i] = true
		}
	}
	var pick int
	var ranked []Ranked
	var existing *torrentRecord
	for range 3 {
		pick, ranked = Choose(d.want, releases, func(i int) bool { return skip[i] })
		if d.approved != "" {
			pick = -1
			for i, rel := range releases {
				if !skip[i] && releaseRef(rel) == d.approved && infoHash(rel.MagnetLink) != "" {
					pick = i
					break
				}
			}
		}
		if pick < 0 {
			break
		}
		existing, err = findTorrent(ctx, cl, releases[pick].Title, infoHash(releases[pick].MagnetLink))
		if err != nil {
			e.Detail = "bookwarehouse_unavailable"
			return answer("unavailable"), nil
		}
		if existing == nil || (existing.Status != "failed" && existing.Status != "cancelled") {
			break
		}
		skip[pick] = true
		existing = nil
		pick = -1
	}
	if pick < 0 {
		e.Detail = "no_release_found"
		e.Candidates = topCandidates(releases, ranked, 5)
		return answer("no_match"), nil
	}

	chosen := releases[pick]
	hash := infoHash(chosen.MagnetLink)
	e.Chosen = candidateView(chosen, ranked[pick])
	reason := ranked[pick].Reason
	if d.approved != "" {
		reason = "Approved by an administrator."
	}
	e.Heuristic = &decisionHeuristic{H: ranked[pick].Score, Reason: text(reason, 300)}
	receipt := audioReceipt{Kind: "qbittorrent", ID: hash, RequestID: d.id, Attempt: d.attempt, Client: fingerprint(s.BaseURL), Confirmed: true, Sel: text(chosen.Title, 200), ExpMB: max(ranked[pick].SizeMB, 0)}
	handOff := func(record torrentRecord, adopted bool) *pb.FulfillResponse {
		receipt.TorrentID, receipt.Adopted = record.ID, adopted
		e.Outcome = "handed_off"
		if adopted {
			e.Outcome = "adopted"
		}
		state, external := "queued", "queued"
		if record.Status == "downloading" || record.Status == "seeding" || record.Status == "importing" {
			state, external = "downloading", "downloading"
		}
		return &pb.FulfillResponse{Message: e.encode(), Targets: []*pb.FulfillmentTarget{{Quality: "audiobook", ConnectionId: conn.Id, ExternalId: receipt.encode(), Status: state, ExternalStatus: external, Message: "Bookwarehouse is fetching the audiobook."}}}
	}
	if existing != nil {
		return handOff(*existing, true), nil
	}
	if deadline, ok := ctx.Deadline(); ctx.Err() != nil || ok && time.Until(deadline) < 5*time.Second {
		e.Detail = "deferred"
		return answer("unavailable"), nil
	}

	body := map[string]string{"title": chosen.Title, "author": chosen.Author, "narrator": chosen.Narrator, "format": chosen.Format, "file_size": chosen.FileSize, "upload_date": chosen.UploadDate, "torrent_url": chosen.TorrentURL, "magnet_link": chosen.MagnetLink, "asin": chosen.ASIN}
	var record torrentRecord
	code, err = cl.json(ctx, http.MethodPost, "/audiobooks/torrents", nil, body, &record)
	switch {
	case err == nil && code == http.StatusOK && validID(record.ID) && record.hash() == hash:
		return handOff(record, false), nil
	case code == http.StatusUnauthorized:
		return nil, status.Error(codes.FailedPrecondition, keyRejected)
	case code == http.StatusForbidden:
		return nil, status.Error(codes.FailedPrecondition, curatorMissing)
	case code == http.StatusNotFound:
		return nil, status.Error(codes.FailedPrecondition, torrentsOff)
	case code == http.StatusBadRequest:
		return answer("rejected"), nil
	}
	// No usable answer: Bookwarehouse may or may not hold the torrent now.
	lookup, cancelLookup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelLookup()
	found, lookupErr := findTorrent(lookup, cl, chosen.Title, hash)
	switch {
	case lookupErr == nil && found != nil:
		return handOff(*found, false), nil
	case lookupErr == nil && code >= 500:
		// Bookwarehouse saves the record only after a successful add.
		return answer("unavailable"), nil
	}
	unknown := audioReceipt{Kind: "unknown", RequestID: d.id, Attempt: d.attempt, Sel: receipt.Sel, ExpMB: receipt.ExpMB}
	e.Outcome = "handed_off"
	return &pb.FulfillResponse{Message: e.encode(), Targets: []*pb.FulfillmentTarget{{Quality: "audiobook", ConnectionId: conn.Id, ExternalId: unknown.encode(), Status: "queued", ExternalStatus: "unknown", Message: "Submission outcome unknown; awaiting Bookwarehouse."}}}, nil
}

func topCandidates(releases []Release, ranked []Ranked, n int) []decisionCandidate {
	order := make([]int, 0, len(ranked))
	for i := range ranked {
		order = append(order, i)
	}
	// Survivors first by score, then rejected releases in search order.
	for i := 1; i < len(order); i++ {
		for j := i; j > 0; j-- {
			a, b := ranked[order[j-1]], ranked[order[j]]
			if (a.Rejected != "") && (b.Rejected == "") || a.Rejected == "" && b.Rejected == "" && b.Score > a.Score {
				order[j-1], order[j] = order[j], order[j-1]
			}
		}
	}
	var out []decisionCandidate
	for _, i := range order {
		if len(out) == n {
			break
		}
		if c := candidateView(releases[i], ranked[i]); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

func (r *Router) checkAudiobook(ctx context.Context, s Settings, in *pb.CheckStatusRequest) (*pb.CheckStatusResponse, error) {
	if len(in.GetTargets()) != 1 || len(in.GetConnections()) != 1 || in.Connections[0] == nil {
		return nil, status.Error(codes.InvalidArgument, "One audiobook target is required.")
	}
	d, err := parseAudioDescriptor(in.Request)
	if err != nil {
		return nil, err
	}
	conn := in.Connections[0]
	target := in.Targets[0]
	if target.GetQuality() != "audiobook" || target.GetConnectionId() != conn.GetId() {
		return nil, status.Error(codes.InvalidArgument, "Target connection or quality changed.")
	}
	receipt, err := parseReceipt(target.ExternalId, d)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "Invalid audiobook receipt.")
	}
	// "unknown" with external "unknown" tells the host Bookwarehouse could
	// not be read, which never counts toward a lost download.
	st := &pb.TargetStatus{Quality: "audiobook", ConnectionId: conn.Id, Status: "unknown", ExternalStatus: "unknown"}
	out := &pb.CheckStatusResponse{Statuses: []*pb.TargetStatus{st}}
	set := func(state, external string) *pb.CheckStatusResponse {
		st.Status, st.ExternalStatus = state, external
		return out
	}
	if receipt.Kind == "unknown" {
		// The hand-off may not have happened. The host searches again after
		// repeated absent answers, and that pass adopts a late add.
		return set("unknown", "absent"), nil
	}
	if receipt.Kind != "qbittorrent" || receipt.TorrentID == "" || receipt.Client != fingerprint(s.BaseURL) {
		return set("failed", "client_changed"), nil
	}
	cl, err := newClient(s.BaseURL, s.audiobookKey(), s.AllowHTTP, 30*time.Second)
	if err != nil {
		return out, nil
	}
	defer cl.close()
	ctx, cancel := bounded(ctx, 30*time.Second, 0)
	defer cancel()
	var record torrentRecord
	code, err := cl.json(ctx, http.MethodGet, "/audiobooks/torrents/"+url.PathEscape(receipt.TorrentID), nil, nil, &record)
	switch {
	case code == http.StatusNotFound:
		return set("unknown", "missing"), nil
	case err != nil || code != http.StatusOK:
		return out, nil
	case record.ID != receipt.TorrentID || record.hash() != receipt.ID:
		return set("failed", "client_changed"), nil
	}
	switch record.Status {
	case "pending", "adding":
		return set("queued", "queued"), nil
	case "downloading", "seeding", "importing":
		return set("downloading", "downloading"), nil
	case "failed":
		return set("failed", "client_error"), nil
	case "cancelled":
		return set("failed", "deleted"), nil
	case "completed":
		v := decision{V: 1, Phase: "verify", Outcome: "verified"}
		if record.AudiobookID == nil || !audiobookIDPattern.MatchString(*record.AudiobookID) {
			// Completed without a library item to point at: an
			// administrator decides.
			v.Outcome, v.Issues = "review", []string{"other"}
			st.Message = v.encode()
			return set("review", "review"), nil
		}
		v.AudiobookID = *record.AudiobookID
		st.Message = v.encode()
		return set("completed", "verified"), nil
	}
	return out, nil
}

// probeCurator asks whether the audiobook key may add torrents without
// adding one: an empty body passes the permission check and fails the
// handler's validation (400), while a key without books.create gets 403.
func probeCurator(ctx context.Context, s Settings) (int, error) {
	cl, err := newClient(s.BaseURL, s.audiobookKey(), s.AllowHTTP, 15*time.Second)
	if err != nil {
		return 0, err
	}
	defer cl.close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, _ := cl.json(ctx, http.MethodPost, "/audiobooks/torrents", nil, struct{}{}, nil)
	return code, nil
}

// probeMessage is a plain refusal for a probe answer, or "" when the key
// may add torrents (or the probe could not tell).
func probeMessage(code int) string {
	switch code {
	case http.StatusUnauthorized:
		return keyRejected
	case http.StatusForbidden:
		return curatorMissing
	case http.StatusNotFound:
		return torrentsOff
	}
	return ""
}

func validateAudiobook(ctx context.Context, s Settings, in *pb.ValidateRequest) *pb.ValidateResponse {
	out := &pb.ValidateResponse{FieldErrors: map[string]string{}}
	for _, sibling := range in.Siblings {
		if sibling.GetId() != in.Connection.GetId() {
			out.FormError = "Only one Bookwarehouse audiobook connection is supported."
			return out
		}
	}
	code, err := probeCurator(ctx, s)
	if err != nil {
		out.FormError = unusable
	} else if code == http.StatusUnauthorized || code == http.StatusForbidden {
		out.FormError = probeMessage(code)
	}
	return out
}

func audiobookOptions(ctx context.Context, s Settings) (*pb.ListConfigOptionsResponse, error) {
	code, err := probeCurator(ctx, s)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, unusable)
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return nil, status.Error(codes.FailedPrecondition, probeMessage(code))
	}
	return &pb.ListConfigOptionsResponse{OptionsByField: map[string]*pb.ConfigOptionList{}}, nil
}

func testAudiobook(ctx context.Context, s Settings) *pb.TestConnectionResponse {
	code, err := probeCurator(ctx, s)
	switch {
	case err != nil:
		return &pb.TestConnectionResponse{Message: unusable}
	case code == http.StatusBadRequest:
		return &pb.TestConnectionResponse{Ok: true, Message: "Bookwarehouse accepts this key for audiobook torrent requests."}
	case probeMessage(code) != "":
		return &pb.TestConnectionResponse{Message: probeMessage(code)}
	}
	return &pb.TestConnectionResponse{Message: "Bookwarehouse did not answer the audiobook torrent check."}
}
