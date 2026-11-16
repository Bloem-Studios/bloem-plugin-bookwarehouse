package requests

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Wanted is the audiobook a request asks for.
type Wanted struct{ Title, Author, Narrator string }

// Release is one AudiobookBay result as Bookwarehouse returns it
// (services.AudiobookBayResult).
type Release struct {
	Title         string `json:"title"`
	Author        string `json:"author"`
	Narrator      string `json:"narrator"`
	Format        string `json:"format"`
	Bitrate       string `json:"bitrate"`
	FileSize      string `json:"file_size"`
	TorrentURL    string `json:"torrent_url"`
	MagnetLink    string `json:"magnet_link"`
	Seeders       int    `json:"seeders"`
	Leechers      int    `json:"leechers"`
	UploadDate    string `json:"upload_date"`
	DetailPageURL string `json:"detail_page_url"`
	ASIN          string `json:"asin"`
}

// Ranked is how Choose judged one release. Rejected names the first rule it
// failed: skipped, magnet, format, collection, abridged, title, author or
// size. Score is in [0,1] and only meaningful when Rejected is empty.
type Ranked struct {
	Index    int
	Score    float64
	Rejected string
	Reason   string
	SizeMB   int64 // -1 when unknown
	Format   string
}

// Release size bounds, in MB. Below the floor is a sample or a single part;
// above the ceiling is a bundle or a mislabelled upload.
const (
	minReleaseMB = 20
	maxReleaseMB = 20 * 1024
	// Among three or more matching releases with known sizes, one more than
	// this factor away from the median is an outlier.
	sizeOutlierFactor = 5
)

var (
	collectionPattern = regexp.MustCompile(`\b(collection|omnibus|anthology|box ?set|boxed set|complete series|books \d)`)
	abridgedPattern   = regexp.MustCompile(`\babridged\b`)
	sizePattern       = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*([KMGT])i?B\b`)
	refPathPattern    = regexp.MustCompile(`^/abss/([A-Za-z0-9._~%-]{1,200})/?$`)
	stopwords         = map[string]bool{"the": true, "a": true, "an": true, "and": true, "of": true}
	// Words a release title adds around the book's own title.
	noiseWords = map[string]bool{"unabridged": true, "audiobook": true, "audio": true, "book": true, "edition": true, "narrated": true,
		"read": true, "by": true, "retail": true, "m4b": true, "mp3": true, "aac": true, "flac": true, "kbps": true, "vol": true,
		"volume": true, "part": true, "novel": true}
)

// Choose picks the release to fetch: the best-scoring one that passes every
// rule, or -1 when no release matches confidently. It is pure: the same
// inputs always give the same choice, so a refused pass is retried on the
// next pass rather than settled by luck.
//
// Rules, in order: a usable magnet link; an audio format (ebooks are
// refused); no collection or abridged edition unless asked for; the title
// (every significant word of the requested title, and no extra words in the
// release's main title); the author (a requested author's surname in the
// release's author or title); a plausible size, then no size outlier. Among
// the survivors m4b beats mp3 beats other audio beats unknown, then seeders,
// author and narrator agreement, and a known size decide.
func Choose(want Wanted, releases []Release, skip func(int) bool) (int, []Ranked) {
	ranked := make([]Ranked, len(releases))
	wantMain := tokens(mainTitle(want.Title))
	wantSet := set(wantMain)
	wantAll := set(tokens(want.Title))
	significant := slices.DeleteFunc(slices.Clone(wantMain), func(t string) bool { return stopwords[t] })
	if len(significant) == 0 {
		significant = wantMain
	}
	surnames := surnamesOf(want.Author)
	narrators := surnamesOf(want.Narrator)
	wantLower := strings.ToLower(want.Title)
	for i, r := range releases {
		rank := Ranked{Index: i, SizeMB: parseSizeMB(r.FileSize), Format: audioFormat(r)}
		lower := strings.ToLower(r.Title)
		all := set(tokens(r.Title))
		authorTokens := set(tokens(r.Author))
		switch {
		case skip != nil && skip(i):
			rank.Rejected = "skipped"
		case infoHash(r.MagnetLink) == "":
			rank.Rejected = "magnet"
		case rank.Format == "ebook":
			rank.Rejected = "format"
		case collectionPattern.MatchString(lower) && !collectionPattern.MatchString(wantLower):
			rank.Rejected = "collection"
		case abridgedPattern.MatchString(lower) && !abridgedPattern.MatchString(wantLower):
			rank.Rejected = "abridged"
		case len(significant) == 0 || !titleMatches(significant, wantSet, wantAll, all, mainTitle(r.Title), want.Author, r.Author):
			rank.Rejected = "title"
		case len(surnames) > 0 && !anyIn(surnames, authorTokens) && !anyIn(surnames, all):
			rank.Rejected = "author"
		case rank.SizeMB >= 0 && (rank.SizeMB < minReleaseMB || rank.SizeMB > maxReleaseMB):
			rank.Rejected = "size"
		}
		ranked[i] = rank
	}
	rejectSizeOutliers(ranked)
	best := -1
	for i, r := range releases {
		rank := &ranked[i]
		if rank.Rejected != "" {
			continue
		}
		score := 0.5
		var why []string
		switch rank.Format {
		case "m4b":
			score += 0.25
		case "mp3":
			score += 0.15
		case "audio":
			score += 0.1
		}
		if rank.Format != "" {
			why = append(why, rank.Format)
		} else {
			why = append(why, "format unknown")
		}
		score += 0.05 * math.Min(1, math.Log1p(float64(max(r.Seeders, 0)))/math.Log1p(50))
		why = append(why, strconv.Itoa(max(r.Seeders, 0))+" seeders")
		if len(surnames) > 0 && anyIn(surnames, set(tokens(r.Author))) {
			score += 0.05
		}
		if len(narrators) > 0 && strings.TrimSpace(r.Narrator) != "" {
			if anyIn(narrators, set(tokens(r.Narrator))) {
				score += 0.05
				why = append(why, "narrator matches")
			} else {
				score -= 0.05
				why = append(why, "other narrator")
			}
		}
		if rank.SizeMB < 0 {
			score -= 0.05
			why = append(why, "size unknown")
		} else {
			why = append(why, fmt.Sprintf("%d MB", rank.SizeMB))
		}
		rank.Score = math.Max(0, math.Min(1, score))
		rank.Reason = "Title and author match; " + strings.Join(why, ", ") + "."
		if best < 0 || rank.Score > ranked[best].Score {
			best = i
		}
	}
	return best, ranked
}

// titleMatches requires every significant requested word (or 80% of them
// for titles of four or more words) and no unexplained word in the
// release's main title, so a sequel ("Dune Messiah") never stands in for
// the book ("Dune").
func titleMatches(significant []string, wantSet, wantAll, releaseAll map[string]bool, releaseMain, wantAuthor, releaseAuthor string) bool {
	found := 0
	for _, t := range significant {
		if releaseAll[t] {
			found++
		}
	}
	if len(significant) <= 3 && found != len(significant) || float64(found) < 0.8*float64(len(significant)) {
		return false
	}
	authors := set(append(tokens(wantAuthor), tokens(releaseAuthor)...))
	for _, t := range tokens(releaseMain) {
		if wantSet[t] || wantAll[t] || stopwords[t] || noiseWords[t] || authors[t] || isDigits(t) {
			continue
		}
		return false
	}
	return true
}

func rejectSizeOutliers(ranked []Ranked) {
	var sizes []int64
	for _, r := range ranked {
		if r.Rejected == "" && r.SizeMB > 0 {
			sizes = append(sizes, r.SizeMB)
		}
	}
	if len(sizes) < 3 {
		return
	}
	slices.Sort(sizes)
	median := float64(sizes[len(sizes)/2])
	if len(sizes)%2 == 0 {
		median = float64(sizes[len(sizes)/2-1]+sizes[len(sizes)/2]) / 2
	}
	for i := range ranked {
		r := &ranked[i]
		if r.Rejected == "" && r.SizeMB > 0 && (float64(r.SizeMB) < median/sizeOutlierFactor || float64(r.SizeMB) > median*sizeOutlierFactor) {
			r.Rejected = "size"
		}
	}
}

// audioFormat classifies a release as m4b, mp3, audio (another audio
// codec), ebook, or "" when unknown.
func audioFormat(r Release) string {
	f := strings.ToLower(r.Format)
	for _, ebook := range []string{"epub", "pdf", "mobi", "azw", "ebook", "e-book"} {
		if strings.Contains(f, ebook) {
			return "ebook"
		}
	}
	switch {
	case strings.Contains(f, "m4b"):
		return "m4b"
	case strings.Contains(f, "mp3"):
		return "mp3"
	}
	for _, audio := range []string{"m4a", "aac", "flac", "ogg", "opus", "wma"} {
		if strings.Contains(f, audio) {
			return "audio"
		}
	}
	if strings.TrimSpace(f) == "" {
		title := set(tokens(r.Title))
		switch {
		case title["m4b"]:
			return "m4b"
		case title["mp3"]:
			return "mp3"
		}
	}
	return ""
}

// parseSizeMB reads AudiobookBay's size text ("512.3 MB", "1.2 GB"),
// returning whole megabytes, or -1 when there is no size.
func parseSizeMB(text string) int64 {
	m := sizePattern.FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v < 0 {
		return -1
	}
	scale := map[string]float64{"K": 1.0 / 1024, "M": 1, "G": 1024, "T": 1024 * 1024}[strings.ToUpper(m[2])]
	mb := v * scale
	if mb > 10_000_000 {
		return 10_000_000
	}
	return int64(mb)
}

// infoHash returns the lowercase hexadecimal BitTorrent v1 info hash of a
// magnet link, accepting the hexadecimal and base32 forms, or "".
func infoHash(magnet string) string {
	rest, ok := strings.CutPrefix(magnet, "magnet:?")
	if !ok {
		return ""
	}
	values, _ := url.ParseQuery(rest)
	for _, xt := range values["xt"] {
		if len(xt) < 9 || !strings.EqualFold(xt[:9], "urn:btih:") {
			continue
		}
		if h := normalizeHash(xt[9:]); h != "" {
			return h
		}
	}
	return ""
}

func normalizeHash(raw string) string {
	switch len(raw) {
	case 40:
		if b, err := hex.DecodeString(raw); err == nil {
			return hex.EncodeToString(b)
		}
	case 32:
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(raw)); err == nil && len(b) == 20 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

// releaseRef is the candidate reference the host stores and shows: the
// AudiobookBay post path, as the earlier audiobook plugin wrote it. It is ""
// when the detail page is not an AudiobookBay post.
func releaseRef(r Release) string {
	u, err := url.Parse(r.DetailPageURL)
	if err != nil || r.DetailPageURL == "" {
		return ""
	}
	m := refPathPattern.FindStringSubmatch(u.EscapedPath())
	if m == nil || m[1] == "." || m[1] == ".." {
		return ""
	}
	return "abb:/abss/" + m[1] + "/"
}

// mainTitle drops bracketed asides and everything after a subtitle or
// author separator.
func mainTitle(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	out := b.String()
	for _, sep := range []string{":", " - ", " – ", " — ", " | "} {
		if i := strings.Index(out, sep); i > 0 {
			out = out[:i]
		}
	}
	return out
}

// tokens lowercases s, joins apostrophe contractions and splits on anything
// that is not a letter or digit.
func tokens(s string) []string {
	s = strings.NewReplacer("'", "", "’", "", "‘", "").Replace(strings.ToLower(s))
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func surnamesOf(names string) []string {
	var out []string
	for _, name := range regexp.MustCompile(`(?i),|&|;| and `).Split(names, -1) {
		parts := tokens(name)
		if len(parts) > 0 && len([]rune(parts[len(parts)-1])) >= 2 {
			out = append(out, parts[len(parts)-1])
		}
	}
	return out
}

func set(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func anyIn(values []string, in map[string]bool) bool {
	for _, v := range values {
		if in[v] {
			return true
		}
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}
