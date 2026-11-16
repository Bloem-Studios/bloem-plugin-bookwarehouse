package requests

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The audiobook envelopes are the formats bloem-server parses in
// internal/bookrequests/audiobook_envelope.go, first written by the
// audiobook request plugin (bloem.requests.audiobooks). Field order is part
// of the wire format. This plugin adds two optional fields, each omitted
// when empty: the receipt's torrent_id and the decision's audiobook_id.

const (
	receiptPrefix  = "abr1."
	decisionPrefix = "abd1."
	receiptMax     = 2048
	decisionMax    = 6144
)

// audioReceipt is the host's durable record of one hand-off. For a
// Bookwarehouse torrent, Kind is "qbittorrent" (Bookwarehouse runs the
// torrent in its own qBittorrent), ID is the torrent's 40-hex info hash,
// Client fingerprints the Bookwarehouse base URL, and TorrentID is
// Bookwarehouse's monitored-torrent ID (/api/v1/audiobooks/torrents/{id}).
type audioReceipt struct {
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
	TorrentID string `json:"torrent_id,omitempty"` // added: Bookwarehouse torrent record ID
}

type decisionCandidate struct {
	Ref    string  `json:"ref"`
	Source string  `json:"source"`
	Title  string  `json:"title"`
	SizeMB int64   `json:"size_mb"`
	Format string  `json:"format,omitempty"`
	H      float64 `json:"h"`
}

type decisionHeuristic struct {
	H      float64 `json:"h"`
	Reason string  `json:"reason"`
}

type decisionSource struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// decision is the abd1 envelope. This plugin never asks an AI model, so AI
// is always omitted and AICalls is always 0.
type decision struct {
	V             int                 `json:"v"`
	Phase         string              `json:"phase"`
	Outcome       string              `json:"outcome"`
	Detail        string              `json:"detail,omitempty"`
	Chosen        *decisionCandidate  `json:"chosen,omitempty"`
	Candidates    []decisionCandidate `json:"candidates,omitempty"`
	Heuristic     *decisionHeuristic  `json:"heuristic,omitempty"`
	Issues        []string            `json:"issues,omitempty"`
	Queries       []string            `json:"queries,omitempty"`
	Sources       []decisionSource    `json:"sources,omitempty"`
	AICalls       int                 `json:"ai_calls"`
	AdminApproved bool                `json:"admin_approved"`
	// AudiobookID is added: set only on a verified verify decision, it is
	// the Bookwarehouse audiobook the completed torrent was imported as.
	AudiobookID string `json:"audiobook_id,omitempty"`
}

var (
	torrentHashPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
	fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{16}$`)
	formatPattern      = regexp.MustCompile(`^[A-Za-z0-9]{0,16}$`)
	audiobookIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

func encodeEnvelope(prefix string, v any) string {
	b, _ := json.Marshal(v)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func (r audioReceipt) encode() string { return encodeEnvelope(receiptPrefix, r) }

// encode keeps the envelope within the host's limit by dropping display
// detail first.
func (d decision) encode() string {
	raw := encodeEnvelope(decisionPrefix, d)
	for len(raw) > decisionMax && len(d.Candidates) > 0 {
		d.Candidates = d.Candidates[:len(d.Candidates)-1]
		raw = encodeEnvelope(decisionPrefix, d)
	}
	if len(raw) > decisionMax {
		d.Queries, d.Heuristic = nil, nil
		raw = encodeEnvelope(decisionPrefix, d)
	}
	if len(raw) > decisionMax {
		d.Chosen = nil
		raw = encodeEnvelope(decisionPrefix, d)
	}
	return raw
}

// parseReceipt accepts exactly what the host accepts, for this request and
// attempt.
func parseReceipt(raw string, d audioDescriptor) (audioReceipt, error) {
	var r audioReceipt
	bad := errors.New("invalid receipt")
	if len(raw) > receiptMax || !strings.HasPrefix(raw, receiptPrefix) {
		return r, bad
	}
	body, err := base64.RawURLEncoding.DecodeString(raw[len(receiptPrefix):])
	if err != nil || !utf8.Valid(body) {
		return r, bad
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || !errors.Is(dec.Decode(&struct{}{}), io.EOF) {
		return r, bad
	}
	if r.RequestID != d.id || r.Attempt != d.attempt || r.ExpMB < 0 || r.ExpMB > 10_000_000 || r.ExpDurS < 0 || r.ExpDurS > 10_000_000 || utf8.RuneCountInString(r.Sel) > 200 || sanitize(r.Sel) != r.Sel {
		return r, bad
	}
	if r.TorrentID != "" && !validID(r.TorrentID) {
		return r, bad
	}
	switch r.Kind {
	case "qbittorrent":
		if !torrentHashPattern.MatchString(r.ID) || !fingerprintPattern.MatchString(r.Client) {
			return r, bad
		}
	case "nzbget":
		// Never written by this plugin; parsed so CheckStatus can answer it.
	case "unknown":
		if r.ID != "" || r.Client != "" || r.Confirmed || r.TorrentID != "" {
			return r, bad
		}
	default:
		return r, bad
	}
	return r, nil
}

// sanitize drops control characters and bidirectional formatting
// characters, as the host does before it stores or shows free text.
func sanitize(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r):
			return -1
		case r == '؜' || r == '‎' || r == '‏' || r >= '‪' && r <= '‮' || r >= '⁦' && r <= '⁩':
			return -1
		}
		return r
	}, s)
}

// text sanitizes s and cuts it to at most runes characters.
func text(s string, runes int) string {
	s = strings.TrimSpace(sanitize(s))
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:runes]))
}

func candidateView(r Release, rank Ranked) *decisionCandidate {
	ref := releaseRef(r)
	if ref == "" {
		return nil
	}
	format := strings.TrimSpace(r.Format)
	if !formatPattern.MatchString(format) {
		format = ""
	}
	return &decisionCandidate{Ref: ref, Source: "audiobookbay", Title: text(r.Title, 200), SizeMB: max(rank.SizeMB, 0), Format: format, H: rank.Score}
}
