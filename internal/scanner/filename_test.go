package scanner

import "testing"

// TestEditionName covers the Edition discriminator from a filename: an explicit
// {edition-…} tag wins; otherwise a quality token; otherwise the ffprobed
// resolution.
func TestEditionName(t *testing.T) {
	cases := []struct {
		name   string
		height int
		want   string
	}{
		{"Dune (2021) {edition-Director's Cut}.mkv", 1080, "Director's Cut"},
		{"Dune (2021) - 2160p.mkv", 0, "2160p"},
		{"Dune (2021) - 1080p.mkv", 0, "1080p"},
		{"Dune (2021) - bluray.mkv", 0, "bluray"},
		{"Dune (2021).mkv", 2160, "2160p"}, // no filename token → resolution
		{"Dune (2021).mkv", 1080, "1080p"},
		{"Dune (2021).mkv", 0, ""}, // unknown
	}
	for _, tc := range cases {
		if got := editionName(tc.name, tc.height); got != tc.want {
			t.Errorf("editionName(%q, %d) = %q, want %q", tc.name, tc.height, got, tc.want)
		}
	}
}

// TestPartNumber covers multi-part suffix detection + aliases.
func TestPartNumber(t *testing.T) {
	cases := map[string]int{
		"Split (2012) - part1.mp4": 1,
		"Split (2012) - part2.mp4": 2,
		"Split (2012) - cd1.mp4":   1,
		"Split (2012) - pt2.mkv":   2,
		"Split (2012) - disc3.mkv": 3,
		"Split (2012).mp4":         0,
	}
	for name, want := range cases {
		if got := partNumber(name); got != want {
			t.Errorf("partNumber(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestExtraClassification covers extra-by-suffix and extra-by-folder.
func TestExtraClassification(t *testing.T) {
	if et := extraTypeFromSuffix("Movie (2013)-trailer.mp4"); et != "trailer" {
		t.Errorf("suffix trailer = %q, want trailer", et)
	}
	if et := extraTypeFromSuffix("Movie (2013)-behindthescenes.mp4"); et != "behindthescenes" {
		t.Errorf("suffix bts = %q, want behindthescenes", et)
	}
	if et := extraTypeFromSuffix("Movie (2013).mp4"); et != "" {
		t.Errorf("plain main file classified as extra %q", et)
	}
	if et := extraTypeFromFolder("Trailers"); et != "trailer" {
		t.Errorf("folder Trailers = %q, want trailer", et)
	}
	if et := extraTypeFromFolder("Behind The Scenes"); et != "behindthescenes" {
		t.Errorf("folder BTS = %q, want behindthescenes", et)
	}
	if et := extraTypeFromFolder("Season 01"); et != "" {
		t.Errorf("non-extra folder classified as extra %q", et)
	}
}

// TestJunkAndAllowlist covers sample/junk + the extension allowlist.
func TestJunkAndAllowlist(t *testing.T) {
	if !isJunk("sample.mp4", 5000) {
		t.Error("sample.mp4 should be junk")
	}
	if !isJunk("Movie-sample.mkv", 5000) {
		t.Error("-sample suffix should be junk")
	}
	if !isJunk("Movie.mkv", 10) {
		t.Error("sub-floor size should be junk")
	}
	if isJunk("Movie (2021).mkv", 5_000_000) {
		t.Error("a real movie should not be junk")
	}
	if !isMedia("Movie.mkv") || !isMedia("Movie.MP4") {
		t.Error("recognized video extensions should be allowlisted")
	}
	if isMedia("Movie.txt") || isMedia("Movie.nfo") {
		t.Error("non-media extensions must be rejected")
	}
}

// TestArtworkRole covers poster/background recognition.
func TestArtworkRole(t *testing.T) {
	cases := map[string]string{
		"poster.jpg":       "poster",
		"cover.jpg":        "poster",
		"Movie-poster.jpg": "poster",
		"fanart.jpg":       "background",
		"backdrop.png":     "background",
		"random.jpg":       "",
		"poster.txt":       "",
	}
	for name, want := range cases {
		if got := artworkRole(name); got != want {
			t.Errorf("artworkRole(%q) = %q, want %q", name, got, want)
		}
	}
}
