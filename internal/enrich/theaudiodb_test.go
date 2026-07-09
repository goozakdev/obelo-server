package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The real TheAudioDBProvider's HTTP/parse layer, exercised against an httptest
// server serving canned TheAudioDB JSON — the secondary, lower seam (the project's
// black-box tests use a fake provider). No live network is ever touched.

const audiodbArtistJSON = `{
  "artists": [
    {
      "idArtist": "111239",
      "strArtist": "Radiohead",
      "strArtistThumb": "https://theaudiodb.com/images/media/artist/thumb/best.jpg",
      "strBiographyEN": "Radiohead are an English rock band formed in Abingdon.",
      "strBiographyDE": "Radiohead sind eine englische Rockband."
    }
  ]
}`

// audiodbStub serves the artist endpoints and records the request URIs (path +
// query) so a test can assert which endpoint was hit and how it was keyed.
func audiodbStub(t *testing.T, body string, status int) (*TheAudioDBProvider, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewTheAudioDBProvider("k", srv.URL, "en-US"), &seen
}

func TestTheAudioDBByMBID(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched || meta.Source != "theaudiodb" {
		t.Errorf("meta = %+v, want matched theaudiodb result", meta)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "poster" ||
		meta.Artwork[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" {
		t.Errorf("artwork = %+v, want the strArtistThumb as a poster", meta.Artwork)
	}
	// en-US selects strBiographyEN.
	if meta.Overview != "Radiohead are an English rock band formed in Abingdon." {
		t.Errorf("overview = %q, want the English biography", meta.Overview)
	}
	// The MBID lookup hits artist-mb.php?i=.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/artist-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single artist-mb.php?i=%s", *seen, mbid)
	}
}

func TestTheAudioDBByName(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" {
		t.Errorf("artwork = %+v, want the strArtistThumb as a poster", meta.Artwork)
	}
	// The name lookup hits search.php?s=.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/search.php?s=Radiohead") {
		t.Errorf("request = %v, want a single search.php?s=Radiohead", *seen)
	}
}

func TestTheAudioDBPicksLanguageBio(t *testing.T) {
	body := `{"artists":[{"strArtistThumb":"https://x/t.jpg","strBiographyEN":"English bio","strBiographyDE":"German bio"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewTheAudioDBProvider("k", srv.URL, "de-DE")

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "X"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Overview != "German bio" {
		t.Errorf("overview = %q, want the German biography for de-DE", meta.Overview)
	}
}

func TestTheAudioDBFallsBackToEnglishBio(t *testing.T) {
	// No German bio present — the provider falls back to strBiographyEN.
	body := `{"artists":[{"strArtistThumb":"https://x/t.jpg","strBiographyEN":"English bio"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewTheAudioDBProvider("k", srv.URL, "de-DE")

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "X"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Overview != "English bio" {
		t.Errorf("overview = %q, want the English fallback biography", meta.Overview)
	}
}

func TestTheAudioDBNoArtistsIsNoMatch(t *testing.T) {
	// TheAudioDB answers an unknown artist with {"artists":null}.
	p, _ := audiodbStub(t, `{"artists":null}`, 0)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Nobody"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestTheAudioDBNonArtistOrNoKeySkips(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	// A non-artist kind is not TheAudioDB's.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "album", MusicbrainzID: mbid}); err != ErrNoMatch {
		t.Errorf("album err = %v, want ErrNoMatch", err)
	}
	// An artist with neither MBID nor name has nothing to key a lookup by.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist"}); err != ErrNoMatch {
		t.Errorf("no-key err = %v, want ErrNoMatch", err)
	}
	if len(*seen) != 0 {
		t.Errorf("expected zero requests for non-artist / unkeyed lookups; saw %v", *seen)
	}
}

// TestTheAudioDBArtistCandidates: the Artist Photo picker (artwork-management/02)
// surfaces TheAudioDB's single strArtistThumb as one candidate, MBID-keyed and
// reusing the cached artist lookup.
func TestTheAudioDBArtistCandidates(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 (the single strArtistThumb)", len(cands))
	}
	if cands[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" || cands[0].Source != "theaudiodb" {
		t.Errorf("candidate[0] = %+v, want the strArtistThumb tagged theaudiodb", cands[0])
	}
	// MBID-keyed via artist-mb.php.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/artist-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single artist-mb.php?i=%s", *seen, mbid)
	}
}

func TestTheAudioDBArtistCandidatesByName(t *testing.T) {
	// With no MBID, TheAudioDB keys the candidate lookup by NAME (search.php) — so an
	// un-MBID'd artist still gets a photo, unlike the strictly-MBID fanart.tv.
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	cands, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" {
		t.Errorf("candidates = %+v, want the strArtistThumb", cands)
	}
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/search.php?s=Radiohead") {
		t.Errorf("request = %v, want a single search.php?s=Radiohead", *seen)
	}
}

func TestTheAudioDBArtistCandidatesNonArtistOrNoThumb(t *testing.T) {
	// A non-artist kind: TheAudioDB owns no listable set there.
	p, _ := audiodbStub(t, audiodbArtistJSON, 0)
	if _, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "album", MusicbrainzID: mbid}, "cover"); err != ErrSearchUnavailable {
		t.Errorf("album err = %v, want ErrSearchUnavailable", err)
	}
	// A record with a bio but no thumb yields no candidates (nil, nil).
	p2, _ := audiodbStub(t, `{"artists":[{"strBiographyEN":"words, no image"}]}`, 0)
	cands, err := p2.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("no-thumb = (%+v, %v), want (nil, nil)", cands, err)
	}
	// An unknown artist ({"artists":null}) likewise yields no candidates.
	p3, _ := audiodbStub(t, `{"artists":null}`, 0)
	cands, err = p3.ArtworkCandidates(context.Background(), TitleRef{Kind: "artist", Title: "Nobody"}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("unknown artist = (%+v, %v), want (nil, nil)", cands, err)
	}
}

// --- track synopses --------------------------------------------------------

const audiodbTrackJSON = `{
  "track": [
    {
      "idTrack": "32793497",
      "strTrack": "Creep",
      "strDescriptionEN": "Creep is the debut single by Radiohead.",
      "strDescriptionDE": "Creep ist die Debütsingle von Radiohead."
    }
  ]
}`

func TestTheAudioDBTrackByMBID(t *testing.T) {
	p, seen := audiodbStub(t, audiodbTrackJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", MusicbrainzID: mbid})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !meta.Matched || meta.Source != "theaudiodb" {
		t.Errorf("meta = %+v, want matched theaudiodb result", meta)
	}
	// en-US selects strDescriptionEN.
	if meta.Overview != "Creep is the debut single by Radiohead." {
		t.Errorf("overview = %q, want the English description", meta.Overview)
	}
	// A track lookup returns the synopsis only — never artwork (out of scope).
	if len(meta.Artwork) != 0 {
		t.Errorf("artwork = %+v, want none for a track lookup", meta.Artwork)
	}
	// The recording-MBID lookup hits track-mb.php?i=.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/track-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single track-mb.php?i=%s", *seen, mbid)
	}
}

func TestTheAudioDBTrackByName(t *testing.T) {
	p, seen := audiodbStub(t, audiodbTrackJSON, 0)
	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Overview != "Creep is the debut single by Radiohead." {
		t.Errorf("overview = %q, want the English description", meta.Overview)
	}
	// The name lookup hits searchtrack.php?s={artist}&t={track}.
	if len(*seen) != 1 || !strings.Contains((*seen)[0], "/searchtrack.php?s=Radiohead&t=Creep") {
		t.Errorf("request = %v, want a single searchtrack.php?s=Radiohead&t=Creep", *seen)
	}
}

func TestTheAudioDBTrackPicksLanguageDescription(t *testing.T) {
	body := `{"track":[{"strDescriptionEN":"English synopsis","strDescriptionDE":"German synopsis"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewTheAudioDBProvider("k", srv.URL, "de-DE")

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "X"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Overview != "German synopsis" {
		t.Errorf("overview = %q, want the German description for de-DE", meta.Overview)
	}
}

func TestTheAudioDBTrackFallsBackToEnglishDescription(t *testing.T) {
	// No German description present — the provider falls back to strDescriptionEN.
	body := `{"track":[{"strDescriptionEN":"English synopsis"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewTheAudioDBProvider("k", srv.URL, "de-DE")

	meta, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "X"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if meta.Overview != "English synopsis" {
		t.Errorf("overview = %q, want the English fallback description", meta.Overview)
	}
}

func TestTheAudioDBTrackNullIsNoMatch(t *testing.T) {
	// TheAudioDB answers an unknown track with {"track":null}.
	p, _ := audiodbStub(t, `{"track":null}`, 0)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Nobody"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestTheAudioDBTrackNoDescriptionIsNoMatch(t *testing.T) {
	// A track record with no description in any language is no usable data.
	p, _ := audiodbStub(t, `{"track":[{"strTrack":"Creep"}]}`, 0)
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", Artist: "Radiohead"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch", err)
	}
}

func TestTheAudioDBTrackNoKeySkips(t *testing.T) {
	p, seen := audiodbStub(t, audiodbTrackJSON, 0)
	// A track with neither MBID nor name has nothing to key a lookup by.
	if _, err := p.Lookup(context.Background(), TitleRef{Kind: "track"}); err != ErrNoMatch {
		t.Errorf("no-key err = %v, want ErrNoMatch", err)
	}
	if len(*seen) != 0 {
		t.Errorf("expected zero requests for an unkeyed track lookup; saw %v", *seen)
	}
}

func TestTheAudioDBCaches(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	for i := 0; i < 3; i++ {
		if _, err := p.Lookup(context.Background(), TitleRef{Kind: "artist", MusicbrainzID: mbid}); err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
	}
	// The response cache means a re-enrich of the same artist re-hits TheAudioDB once.
	if len(*seen) != 1 {
		t.Errorf("request count = %d, want 1 (cached); requests=%v", len(*seen), *seen)
	}
}
