package requests

import (
	"testing"
)

func release(title, author, format, size string, seeders int) Release {
	return Release{Title: title, Author: author, Format: format, FileSize: size, Seeders: seeders, MagnetLink: "magnet:?xt=urn:btih:" + hexHash(title+format+size)}
}

func hexHash(seed string) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 40)
	h := uint32(2166136261)
	for i := range out {
		for _, c := range []byte(seed) {
			h = (h ^ uint32(c)) * 16777619
		}
		h = (h ^ uint32(i)) * 16777619
		out[i] = digits[h%16]
	}
	return string(out)
}

func TestChooseRelease(t *testing.T) {
	hobbit := Wanted{Title: "The Hobbit", Author: "J.R.R. Tolkien"}
	for _, tc := range []struct {
		name     string
		want     Wanted
		releases []Release
		skip     map[int]bool
		chosen   int // -1: no confident match
		reason   string
	}{
		{name: "empty", want: hobbit, chosen: -1},
		{
			name: "m4b over mp3",
			want: hobbit,
			releases: []Release{
				release("The Hobbit", "J.R.R. Tolkien", "MP3", "320 MB", 40),
				release("The Hobbit", "J.R.R. Tolkien", "M4B", "300 MB", 10),
			},
			chosen: 1,
		},
		{
			name: "more seeders between equal formats",
			want: hobbit,
			releases: []Release{
				release("The Hobbit", "J.R.R. Tolkien", "M4B", "300 MB", 2),
				release("The Hobbit (Unabridged)", "J.R.R. Tolkien", "M4B", "310 MB", 55),
			},
			chosen: 1,
		},
		{
			name:     "wrong title",
			want:     hobbit,
			releases: []Release{release("The Silmarillion", "J.R.R. Tolkien", "M4B", "600 MB", 30)},
			chosen:   -1,
			reason:   "title",
		},
		{
			name:     "sequel is not the book",
			want:     Wanted{Title: "Dune", Author: "Frank Herbert"},
			releases: []Release{release("Dune Messiah", "Frank Herbert", "M4B", "400 MB", 30)},
			chosen:   -1,
			reason:   "title",
		},
		{
			name: "series and author decoration still match",
			want: Wanted{Title: "Dune", Author: "Frank Herbert"},
			releases: []Release{
				release("Dune Messiah - Frank Herbert", "Frank Herbert", "M4B", "400 MB", 90),
				release("Dune (Dune Chronicles, Book 1) - Frank Herbert", "", "M4B", "900 MB", 12),
			},
			chosen: 1,
		},
		{
			name: "subtitle on the release",
			want: Wanted{Title: "Sapiens", Author: "Yuval Noah Harari"},
			releases: []Release{
				release("Sapiens: A Brief History of Humankind", "Yuval Noah Harari", "MP3", "500 MB", 4),
			},
			chosen: 0,
		},
		{
			name:     "wrong author",
			want:     hobbit,
			releases: []Release{release("The Hobbit", "Someone Else", "M4B", "300 MB", 30)},
			chosen:   -1,
			reason:   "author",
		},
		{
			name:     "author only in the title",
			want:     hobbit,
			releases: []Release{release("The Hobbit - J.R.R. Tolkien", "", "M4B", "300 MB", 3)},
			chosen:   0,
		},
		{
			name:     "author unknown is not confident",
			want:     hobbit,
			releases: []Release{release("The Hobbit", "", "M4B", "300 MB", 3)},
			chosen:   -1,
			reason:   "author",
		},
		{
			name:     "no author requested",
			want:     Wanted{Title: "The Hobbit"},
			releases: []Release{release("The Hobbit", "", "M4B", "300 MB", 3)},
			chosen:   0,
		},
		{
			name: "ebook format rejected",
			want: hobbit,
			releases: []Release{
				release("The Hobbit", "J.R.R. Tolkien", "EPUB", "2 MB", 300),
				release("The Hobbit", "J.R.R. Tolkien", "PDF", "300 MB", 300),
			},
			chosen: -1,
			reason: "format",
		},
		{
			name:     "too small to be a whole audiobook",
			want:     hobbit,
			releases: []Release{release("The Hobbit", "J.R.R. Tolkien", "MP3", "5 MB", 30)},
			chosen:   -1,
			reason:   "size",
		},
		{
			name:     "implausibly large",
			want:     hobbit,
			releases: []Release{release("The Hobbit", "J.R.R. Tolkien", "M4B", "45 GB", 30)},
			chosen:   -1,
			reason:   "size",
		},
		{
			name: "size outlier among the matches",
			want: hobbit,
			releases: []Release{
				release("The Hobbit", "J.R.R. Tolkien", "M4B", "40 MB", 500),
				release("The Hobbit", "J.R.R. Tolkien", "MP3", "400 MB", 4),
				release("The Hobbit", "J.R.R. Tolkien", "MP3", "420 MB", 3),
				release("The Hobbit", "J.R.R. Tolkien", "MP3", "1.1 GB", 2),
			},
			chosen: 1,
		},
		{
			name: "collection rejected",
			want: hobbit,
			releases: []Release{
				release("The Hobbit and The Lord of the Rings Collection", "J.R.R. Tolkien", "M4B", "3 GB", 80),
			},
			chosen: -1,
			reason: "collection",
		},
		{
			name: "missing magnet",
			want: hobbit,
			releases: []Release{
				{Title: "The Hobbit", Author: "J.R.R. Tolkien", Format: "M4B", FileSize: "300 MB", Seeders: 9},
			},
			chosen: -1,
			reason: "magnet",
		},
		{
			name: "narrator preferred",
			want: Wanted{Title: "The Hobbit", Author: "J.R.R. Tolkien", Narrator: "Andy Serkis"},
			releases: []Release{
				{Title: "The Hobbit", Author: "J.R.R. Tolkien", Narrator: "Rob Inglis", Format: "M4B", FileSize: "300 MB", Seeders: 9, MagnetLink: "magnet:?xt=urn:btih:" + hexHash("a")},
				{Title: "The Hobbit", Author: "J.R.R. Tolkien", Narrator: "Andy Serkis", Format: "M4B", FileSize: "320 MB", Seeders: 8, MagnetLink: "magnet:?xt=urn:btih:" + hexHash("b")},
			},
			chosen: 1,
		},
		{
			name: "skipped release falls back to the next",
			want: hobbit,
			releases: []Release{
				release("The Hobbit", "J.R.R. Tolkien", "M4B", "300 MB", 40),
				release("The Hobbit", "J.R.R. Tolkien", "MP3", "330 MB", 4),
			},
			skip:   map[int]bool{0: true},
			chosen: 1,
		},
		{
			name: "case, punctuation and apostrophes",
			want: Wanted{Title: "Harry Potter and the Sorcerer's Stone", Author: "J. K. Rowling"},
			releases: []Release{
				release("HARRY POTTER AND THE SORCERERS STONE, Book 1", "J.K. Rowling", "m4b", "700 MB", 21),
			},
			chosen: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ranked := Choose(tc.want, tc.releases, func(i int) bool { return tc.skip[i] })
			if got != tc.chosen {
				t.Fatalf("chosen = %d, want %d; ranked %+v", got, tc.chosen, ranked)
			}
			if len(ranked) != len(tc.releases) {
				t.Fatalf("every release must be ranked: %+v", ranked)
			}
			if got >= 0 {
				if ranked[got].Rejected != "" || ranked[got].Score <= 0 || ranked[got].Score > 1 {
					t.Fatalf("chosen release ranking %+v", ranked[got])
				}
			}
			if tc.reason != "" {
				for _, r := range ranked {
					if r.Rejected == "" {
						t.Fatalf("release %d not rejected: %+v", r.Index, r)
					}
				}
				if ranked[0].Rejected != tc.reason {
					t.Fatalf("rejection = %q, want %q", ranked[0].Rejected, tc.reason)
				}
			}
		})
	}
}

func TestParseSizeMB(t *testing.T) {
	for in, want := range map[string]int64{"512 MB": 512, "1.5 GB": 1536, "900 KB": 0, "2 GiB": 2048, "": -1, "big": -1, "1.2 TB": 1258291} {
		if got := parseSizeMB(in); got != want {
			t.Errorf("parseSizeMB(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestInfoHash(t *testing.T) {
	hex40 := "c12fe1c06bba254a9dc9f519b335aa7c1367a88a"
	for in, want := range map[string]string{
		"magnet:?xt=urn:btih:" + hex40 + "&dn=x":                                  hex40,
		"magnet:?dn=x&xt=urn:btih:C12FE1C06BBA254A9DC9F519B335AA7C1367A88A":       hex40,
		"magnet:?xt=urn:btih:YEX6DQDLXISUVHOJ6UM3GNNKPQJWPKEK":                    hex40,
		"magnet:?xt=urn:sha1:" + hex40:                                            "",
		"https://example.invalid/file.torrent":                                    "",
		"magnet:?xt=urn:btih:zz2fe1c06bba254a9dc9f519b335aa7c1367a88a&tr=udp://x": "",
	} {
		if got := infoHash(in); got != want {
			t.Errorf("infoHash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReleaseRef(t *testing.T) {
	for in, want := range map[string]string{
		"/abss/the-hobbit-j-r-r-tolkien/":          "abb:/abss/the-hobbit-j-r-r-tolkien/",
		"https://tracker.example/abss/the-hobbit/": "abb:/abss/the-hobbit/",
		"/abss/the-hobbit":                         "abb:/abss/the-hobbit/",
		"/audio-books/the-hobbit/":                 "",
		"/abss/../etc/":                            "",
		"":                                         "",
	} {
		if got := releaseRef(Release{DetailPageURL: in}); got != want {
			t.Errorf("releaseRef(%q) = %q, want %q", in, got, want)
		}
	}
}
