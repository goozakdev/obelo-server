package scanner

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Local TV artwork discovery (naming-convention.md "Local artwork"). The Movie
// path has discovered `poster.jpg`/`fanart.jpg` per folder since the beginning;
// the TV path never did, so `Show/poster.jpg`, `Show/fanart.jpg`, and
// `Show/Season NN.jpg` were read off disk and silently dropped, leaving a TV
// Library with artwork ONLY from Enrichment. These tests pin the discovery at the
// resolver level, where the ShowTree can be inspected without a database.

// scanTVTree scans one temp TV root and returns the single resolved ShowTree.
func scanTVTree(t *testing.T, root string) store.ShowTree {
	t.Helper()
	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, &recordingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(cs.showTrees) != 1 {
		t.Fatalf("resolved %d show trees, want 1", len(cs.showTrees))
	}
	return cs.showTrees[0]
}

// artworkPath returns the path recorded for role, or "" when the role is absent.
func artworkPath(rows []store.EntityArtworkRow, role string) string {
	for _, a := range rows {
		if a.Role == role {
			return a.Path
		}
	}
	return ""
}

// TestShowFolderArtworkDiscovered: `poster.jpg` and `fanart.jpg` in a Show folder
// become the Show's local poster/background — the same basenames, and the same
// artworkRole classifier, the Movie path already honors.
func TestShowFolderArtworkDiscovered(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Wire (2002)")
	writeFile(t, filepath.Join(show, "Season 01", "The Wire (2002) - S01E01 - The Target.mkv"))
	writeFile(t, filepath.Join(show, "poster.jpg"))
	writeFile(t, filepath.Join(show, "fanart.jpg"))

	tree := scanTVTree(t, root)
	if got := artworkPath(tree.Artwork, "poster"); got != filepath.Join(show, "poster.jpg") {
		t.Errorf("show poster = %q, want the folder's poster.jpg", got)
	}
	if got := artworkPath(tree.Artwork, "background"); got != filepath.Join(show, "fanart.jpg") {
		t.Errorf("show background = %q, want the folder's fanart.jpg", got)
	}
}

// TestSeasonPosterDiscovered: `Season NN.jpg` in the SHOW folder posters that
// season — the convention naming-convention.md documents (the image sits beside
// the season folder, not inside it). Also pins that it attaches to the right
// season when several are present, rather than to the Show or to season 1.
func TestSeasonPosterDiscovered(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Wire (2002)")
	writeFile(t, filepath.Join(show, "Season 01", "The Wire (2002) - S01E01 - The Target.mkv"))
	writeFile(t, filepath.Join(show, "Season 02", "The Wire (2002) - S02E01 - Ebb Tide.mkv"))
	writeFile(t, filepath.Join(show, "Season 01.jpg"))
	writeFile(t, filepath.Join(show, "Season 02.jpg"))

	tree := scanTVTree(t, root)
	if len(tree.Seasons) != 2 {
		t.Fatalf("resolved %d seasons, want 2", len(tree.Seasons))
	}
	for _, st := range tree.Seasons {
		want := filepath.Join(show, fmt.Sprintf("Season %02d.jpg", st.SeasonNumber))
		if got := artworkPath(st.Artwork, "poster"); got != want {
			t.Errorf("season %d poster = %q, want %q", st.SeasonNumber, got, want)
		}
	}
	// A season poster must not leak onto the Show itself.
	if len(tree.Artwork) != 0 {
		t.Errorf("show artwork = %+v, want none — only Season NN.jpg was present", tree.Artwork)
	}
}

// TestSpecialsPosterDiscovered: `Specials.jpg` posters season 0, because the
// classifier reuses ParseSeasonFolder — the same grammar that makes a `Specials/`
// folder season 0. The two cannot drift apart.
func TestSpecialsPosterDiscovered(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Wire (2002)")
	writeFile(t, filepath.Join(show, "Specials", "The Wire (2002) - S00E01 - Prequel.mkv"))
	writeFile(t, filepath.Join(show, "Specials.jpg"))

	tree := scanTVTree(t, root)
	if len(tree.Seasons) != 1 || tree.Seasons[0].SeasonNumber != 0 {
		t.Fatalf("seasons = %+v, want a single season 0", tree.Seasons)
	}
	if got := artworkPath(tree.Seasons[0].Artwork, "poster"); got != filepath.Join(show, "Specials.jpg") {
		t.Errorf("specials poster = %q, want Specials.jpg", got)
	}
}

// TestSeasonPosterForAbsentSeasonIgnored: a `Season NN.jpg` naming a season no
// media backs is dropped, not turned into an empty Season. A poster is not
// evidence that episodes exist.
func TestSeasonPosterForAbsentSeasonIgnored(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Wire (2002)")
	writeFile(t, filepath.Join(show, "Season 01", "The Wire (2002) - S01E01 - The Target.mkv"))
	writeFile(t, filepath.Join(show, "Season 09.jpg"))

	tree := scanTVTree(t, root)
	if len(tree.Seasons) != 1 || tree.Seasons[0].SeasonNumber != 1 {
		t.Fatalf("seasons = %+v, want only the season that has episodes", tree.Seasons)
	}
}

// TestShowArtworkIsNotAnEpisode: the artwork files must not be mistaken for media
// and routed to Unmatched — the Show folder's loose-episode branch sees them
// first. Regression guard for the walk order in resolveShowFolder.
func TestShowArtworkIsNotAnEpisode(t *testing.T) {
	root := t.TempDir()
	show := filepath.Join(root, "The Wire (2002)")
	writeFile(t, filepath.Join(show, "Season 01", "The Wire (2002) - S01E01 - The Target.mkv"))
	writeFile(t, filepath.Join(show, "poster.jpg"))
	writeFile(t, filepath.Join(show, "Season 01.jpg"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	svc := NewService(cs, &recordingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, u := range cs.unmatched {
		t.Errorf("artwork routed to Unmatched: %q (%s)", u.Path, u.Reason)
	}
}

// TestParseSeasonPoster covers the classifier directly, including the names it
// must REFUSE — the Show's own poster.jpg (artworkRole's job) and a non-image.
func TestParseSeasonPoster(t *testing.T) {
	cases := []struct {
		name       string
		wantSeason int
		wantOK     bool
	}{
		{"Season 01.jpg", 1, true},
		{"Season 1.png", 1, true},
		{"Season 00.jpg", 0, true},
		{"Specials.jpg", 0, true},
		{"specials.webp", 0, true},
		{"Season 12.jpeg", 12, true},
		{"poster.jpg", 0, false},    // the Show's own poster — artworkRole classifies it
		{"fanart.jpg", 0, false},    // likewise the Show's background
		{"Season 01.mkv", 0, false}, // not an image
		{"Season 01.nfo", 0, false},
		{"The Wire.jpg", 0, false},
	}
	for _, c := range cases {
		season, ok := ParseSeasonPoster(c.name)
		if ok != c.wantOK || season != c.wantSeason {
			t.Errorf("ParseSeasonPoster(%q) = (%d, %v), want (%d, %v)",
				c.name, season, ok, c.wantSeason, c.wantOK)
		}
	}
}
