package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real providers' ArtworkCandidates HTTP/parse layer (the Edit-item image
// picker, item-editing/03), exercised against httptest serving canned JSON — the
// secondary, lower seam. The project's black-box tests use a fake provider's
// ArtworkCandidates instead; no live network is ever touched here.

const caaReleaseGroupJSON = `{
  "images": [
    {"image": "https://caa/full-1.jpg", "front": true, "thumbnails": {"250": "https://caa/250-1.jpg", "500": "https://caa/500-1.jpg"}},
    {"image": "https://caa/full-2.jpg", "front": false, "thumbnails": {"500": "https://caa/500-2.jpg"}}
  ]
}`

// TestMusicBrainzArtworkCandidatesAlbumCovers: an album image query hits the Cover
// Art Archive release-group endpoint and maps the images into candidates,
// preferring the 500px derivative.
func TestMusicBrainzArtworkCandidatesAlbumCovers(t *testing.T) {
	var seen []string
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/release-group/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(caaReleaseGroupJSON))
			return
		}
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-123"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/release-group/rg-123" {
		t.Fatalf("expected one /release-group/rg-123 call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("cover candidates = %d, want 2", len(cands))
	}
	if cands[0].URL != "https://caa/500-1.jpg" || cands[0].Source != "coverartarchive" {
		t.Errorf("candidate[0] = %+v (want the 500px derivative)", cands[0])
	}
}

// TestMusicBrainzArtworkCandidatesArtistNone: an Artist has no listable image set
// (CAA is release-group keyed), so it yields no candidates and makes no call.
func TestMusicBrainzArtworkCandidatesArtistNone(t *testing.T) {
	called := false
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "art-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if called || len(cands) != 0 {
		t.Errorf("artist yielded candidates/made a call: called=%v cands=%d", called, len(cands))
	}
}

// TestMusicBrainzArtworkCandidatesNoArt404: a 404 from the Cover Art Archive is the
// normal "no cover art" outcome — no candidates, no error.
func TestMusicBrainzArtworkCandidatesNoArt404(t *testing.T) {
	caa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caa.Close()
	p := NewMusicBrainzProvider("https://mb/ws/2", caa.URL, "en")
	p.MinInterval = 0

	cands, err := p.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-none"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want 0 on a 404", len(cands))
	}
}
