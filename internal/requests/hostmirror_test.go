package requests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	pb "github.com/Bloem-Studios/bloem-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// These types mirror bloem-server internal/bookrequests/audiobook_envelope.go
// field for field, plus the fields this plugin adds (marked). The server
// decodes with DisallowUnknownFields, so a field missing here would fail
// every envelope on the server too.

type hostReceipt struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	RequestID string `json:"request_id"`
	Attempt   int    `json:"attempt"`
	Client    string `json:"client"`
	Confirmed bool   `json:"confirmed"`
	Adopted   bool   `json:"adopted"`
	Sel       string `json:"sel"`
	ExpMB     int64  `json:"exp_mb"`
	ExpDurS   int64  `json:"exp_dur_s"`
	TorrentID string `json:"torrent_id,omitempty"` // added by this plugin
}

type hostCandidate struct {
	Ref    string  `json:"ref"`
	Source string  `json:"source"`
	Title  string  `json:"title"`
	SizeMB int64   `json:"size_mb"`
	Format string  `json:"format,omitempty"`
	H      float64 `json:"h"`
}

type hostDecision struct {
	V          int             `json:"v"`
	Phase      string          `json:"phase"`
	Outcome    string          `json:"outcome"`
	Detail     string          `json:"detail,omitempty"`
	Chosen     *hostCandidate  `json:"chosen,omitempty"`
	Candidates []hostCandidate `json:"candidates,omitempty"`
	Heuristic  *struct {
		H      float64 `json:"h"`
		Reason string  `json:"reason"`
	} `json:"heuristic,omitempty"`
	AI *struct {
		Used       bool     `json:"used"`
		Model      string   `json:"model"`
		Choice     string   `json:"choice"`
		Confidence float64  `json:"confidence"`
		Reason     string   `json:"reason"`
		Flags      []string `json:"flags"`
		Error      string   `json:"error"`
	} `json:"ai,omitempty"`
	Issues  []string `json:"issues,omitempty"`
	Queries []string `json:"queries,omitempty"`
	Sources []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"sources,omitempty"`
	AICalls       int    `json:"ai_calls"`
	AdminApproved bool   `json:"admin_approved"`
	AudiobookID   string `json:"audiobook_id,omitempty"` // added by this plugin
}

var (
	hostCandidateRef = regexp.MustCompile(`^(abb:/abss/[A-Za-z0-9._~%-]{1,200}/|abook:[0-9]{1,10}|nzbking:[a-f0-9]{8,64})$`)
	hostDetail       = regexp.MustCompile(`^[a-z_]{0,32}$`)
	hostTorrentHash  = regexp.MustCompile(`^[a-f0-9]{40}$`)
	hostFingerprint  = regexp.MustCompile(`^[a-f0-9]{16}$`)
	hostExternal     = []string{"queued", "downloading", "stalled", "paused", "verified", "review", "client_error", "deleted", "nzb_fetch_failed", "missing", "absent", "client_changed", "unknown"}
	hostIssues       = []string{"wrong_book", "abridged", "wrong_language", "sample", "partial", "missing_audio", "extra_content", "unsafe_content", "archive_only", "too_small", "too_short", "size_mismatch", "client_warning", "no_listing", "multiple_items", "other"}
)

func hostDecode(t *testing.T, prefix, raw string, max int, out any) {
	t.Helper()
	if len(raw) > max || !strings.HasPrefix(raw, prefix) {
		t.Fatalf("envelope %q: wrong prefix or longer than %d", raw, max)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("host would refuse %s: %v", body, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatal("trailing data")
	}
}

func hostParseReceipt(t *testing.T, raw string, attempt int) hostReceipt {
	t.Helper()
	var r hostReceipt
	hostDecode(t, "abr1.", raw, 2048, &r)
	if r.RequestID != audioRequest || r.Attempt != attempt || r.ExpMB < 0 || r.ExpMB > 10_000_000 || r.ExpDurS != 0 || len([]rune(r.Sel)) > 200 {
		t.Fatalf("receipt fails host checks: %+v", r)
	}
	switch r.Kind {
	case "qbittorrent":
		if !hostTorrentHash.MatchString(r.ID) || !hostFingerprint.MatchString(r.Client) {
			t.Fatalf("receipt id/client fail host checks: %+v", r)
		}
	case "unknown":
		if r.ID != "" || r.Client != "" || r.Confirmed {
			t.Fatalf("unknown receipt fails host checks: %+v", r)
		}
	default:
		t.Fatalf("receipt kind %q", r.Kind)
	}
	return r
}

// hostCheckStatus applies the host adapter's checks on a CheckStatus answer.
func hostCheckStatus(t *testing.T, s *pb.TargetStatus) {
	t.Helper()
	if s.ConnectionId != "audio-conn" || s.Quality != "audiobook" || !slices.Contains(hostExternal, s.ExternalStatus) ||
		!slices.Contains([]string{"queued", "downloading", "completed", "review", "failed", "unknown"}, s.Status) {
		t.Fatalf("host refuses status %v", s)
	}
	if s.Status == "completed" && s.ExternalStatus != "verified" || s.ExternalStatus == "review" && s.Message == "" {
		t.Fatalf("host refuses status %v", s)
	}
}

func hostParseDecision(t *testing.T, raw, phase string) hostDecision {
	t.Helper()
	var d hostDecision
	hostDecode(t, "abd1.", raw, 6144, &d)
	outcomes := map[string][]string{"select": {"handed_off", "adopted", "no_match", "held", "rejected", "unavailable"}, "verify": {"verified", "review"}}
	if d.V != 1 || d.Phase != phase || !slices.Contains(outcomes[phase], d.Outcome) || !hostDetail.MatchString(d.Detail) || len(d.Candidates) > 5 || len(d.Issues) > 10 || len(d.Queries) > 4 || len(d.Sources) > 3 {
		t.Fatalf("decision fails host checks: %+v", d)
	}
	for _, c := range append(slices.Clone(d.Candidates), func() []hostCandidate {
		if d.Chosen != nil {
			return []hostCandidate{*d.Chosen}
		}
		return nil
	}()...) {
		if !hostCandidateRef.MatchString(c.Ref) || c.Source != "audiobookbay" || c.H < 0 || c.H > 1 || len([]rune(c.Title)) > 200 {
			t.Fatalf("candidate fails host checks: %+v", c)
		}
	}
	for _, issue := range d.Issues {
		if !slices.Contains(hostIssues, issue) {
			t.Fatalf("issue %q", issue)
		}
	}
	for _, s := range d.Sources {
		if s.Name != "audiobookbay" || !slices.Contains([]string{"ok", "empty", "rate_limited", "blocked", "error", "disabled"}, s.Status) {
			t.Fatalf("source %+v", s)
		}
	}
	return d
}
