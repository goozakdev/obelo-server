package theaudiodb

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The TheAudioDB provider's request/parse layer, exercised against an in-memory
// host serving canned TheAudioDB JSON. These are the tests
// internal/enrich/theaudiodb_test.go carried, assertion for assertion, against
// the same canned bodies — what changed is the seam: the old httptest.Server
// handler is now an sdktest.Host handler. No live network is ever touched, and
// none ever was.

// baseURL is a host with NO path, so an asserted request URI reads exactly as it
// did when the old test asserted r.URL.RequestURI() against an httptest server.
const baseURL = "https://www.theaudiodb.com"

const mbid = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

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

// requests is the `seen []string` the old stub kept by hand: the request URI
// (path + query) of every call, which is what says WHICH endpoint was hit and how
// it was keyed. sdktest.Host.Paths() drops the query, and the query is half the
// assertion here, so the handler records it instead.
type requests struct {
	mu   sync.Mutex
	seen []string
}

func (r *requests) add(uri string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, uri)
}

func (r *requests) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// audiodbStub serves the artist endpoints and records the request URIs so a test
// can assert which endpoint was hit and how it was keyed.
func audiodbStub(t *testing.T, body string, status int) (*Provider, *requests) {
	t.Helper()
	return audiodbStubWithLanguage(t, body, status, "en-US")
}

func audiodbStubWithLanguage(t *testing.T, body string, status int, language string) (*Provider, *requests) {
	t.Helper()
	seen := &requests{}
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{
			Enabled:  true,
			Secret:   "k",
			URL:      baseURL,
			Language: language,
		}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.add(r.URL.RequestURI())
			w.Header().Set("Content-Type", "application/json")
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		}),
	)
	return New(host), seen
}

// lookup is the call the old test spelled p.Lookup(ctx, TitleRef{...}).
func lookup(t *testing.T, p *Provider, ref pluginapi.MediaRef) pluginapi.LookupResponse {
	t.Helper()
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return resp
}

// candidates is the call the old test spelled p.ArtworkCandidates(ctx, ref, role).
func candidates(t *testing.T, p *Provider, ref pluginapi.MediaRef, role string) pluginapi.ArtworkCandidatesResponse {
	t.Helper()
	resp, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{Ref: ref, Role: role})
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	return resp
}

func TestTheAudioDBByMBID(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid})
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	meta := resp.Record
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
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/artist-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single artist-mb.php?i=%s", got, mbid)
	}
}

func TestTheAudioDBByName(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	meta := lookup(t, p, pluginapi.MediaRef{Kind: "artist", Title: "Radiohead"}).Record
	if len(meta.Artwork) != 1 || meta.Artwork[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" {
		t.Errorf("artwork = %+v, want the strArtistThumb as a poster", meta.Artwork)
	}
	// The name lookup hits search.php?s=.
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/search.php?s=Radiohead") {
		t.Errorf("request = %v, want a single search.php?s=Radiohead", got)
	}
}

func TestTheAudioDBPicksLanguageBio(t *testing.T) {
	body := `{"artists":[{"strArtistThumb":"https://x/t.jpg","strBiographyEN":"English bio","strBiographyDE":"German bio"}]}`
	p, _ := audiodbStubWithLanguage(t, body, 0, "de-DE")

	meta := lookup(t, p, pluginapi.MediaRef{Kind: "artist", Title: "X"}).Record
	if meta.Overview != "German bio" {
		t.Errorf("overview = %q, want the German biography for de-DE", meta.Overview)
	}
}

func TestTheAudioDBFallsBackToEnglishBio(t *testing.T) {
	// No German bio present — the provider falls back to strBiographyEN.
	body := `{"artists":[{"strArtistThumb":"https://x/t.jpg","strBiographyEN":"English bio"}]}`
	p, _ := audiodbStubWithLanguage(t, body, 0, "de-DE")

	meta := lookup(t, p, pluginapi.MediaRef{Kind: "artist", Title: "X"}).Record
	if meta.Overview != "English bio" {
		t.Errorf("overview = %q, want the English fallback biography", meta.Overview)
	}
}

func TestTheAudioDBNoArtistsIsNoMatch(t *testing.T) {
	// TheAudioDB answers an unknown artist with {"artists":null}.
	p, _ := audiodbStub(t, `{"artists":null}`, 0)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist", Title: "Nobody"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestTheAudioDBNonArtistOrNoKeySkips(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	// A non-artist kind is not TheAudioDB's.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("album outcome = %q, want no-match", got)
	}
	// An artist with neither MBID nor name has nothing to key a lookup by.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("no-key outcome = %q, want no-match", got)
	}
	if got := seen.all(); len(got) != 0 {
		t.Errorf("expected zero requests for non-artist / unkeyed lookups; saw %v", got)
	}
}

// TestTheAudioDBArtistCandidates: the Artist Photo picker (artwork-management/02)
// surfaces TheAudioDB's single strArtistThumb as one candidate, MBID-keyed and
// reusing the cached artist lookup.
func TestTheAudioDBArtistCandidates(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	resp := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	cands := resp.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 (the single strArtistThumb)", len(cands))
	}
	if cands[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" || cands[0].Source != "theaudiodb" {
		t.Errorf("candidate[0] = %+v, want the strArtistThumb tagged theaudiodb", cands[0])
	}
	// MBID-keyed via artist-mb.php.
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/artist-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single artist-mb.php?i=%s", got, mbid)
	}
}

func TestTheAudioDBArtistCandidatesByName(t *testing.T) {
	// With no MBID, TheAudioDB keys the candidate lookup by NAME (search.php) — so an
	// un-MBID'd artist still gets a photo, unlike the strictly-MBID fanart.tv.
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	cands := candidates(t, p, pluginapi.MediaRef{Kind: "artist", Title: "Radiohead"}, "poster").Candidates
	if len(cands) != 1 || cands[0].URL != "https://theaudiodb.com/images/media/artist/thumb/best.jpg" {
		t.Errorf("candidates = %+v, want the strArtistThumb", cands)
	}
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/search.php?s=Radiohead") {
		t.Errorf("request = %v, want a single search.php?s=Radiohead", got)
	}
}

func TestTheAudioDBArtistCandidatesNonArtistOrNoThumb(t *testing.T) {
	// A non-artist kind: TheAudioDB owns no listable set there. ErrSearchUnavailable
	// was the Go provider's word for it; the contract's is OutcomeUnavailable, and
	// the host maps one back to the other.
	p, _ := audiodbStub(t, audiodbArtistJSON, 0)
	if got := candidates(t, p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: mbid}, "cover").Outcome; got != pluginapi.OutcomeUnavailable {
		t.Errorf("album outcome = %q, want unavailable", got)
	}
	// A record with a bio but no thumb yields no candidates.
	p2, _ := audiodbStub(t, `{"artists":[{"strBiographyEN":"words, no image"}]}`, 0)
	resp := candidates(t, p2, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 0 {
		t.Errorf("no-thumb = %+v, want matched with no candidates", resp)
	}
	// An unknown artist ({"artists":null}) likewise yields no candidates.
	p3, _ := audiodbStub(t, `{"artists":null}`, 0)
	resp = candidates(t, p3, pluginapi.MediaRef{Kind: "artist", Title: "Nobody"}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 0 {
		t.Errorf("unknown artist = %+v, want matched with no candidates", resp)
	}
}

// audiodbArtistArtworkJSON carries the fallback logo + backgrounds (the fanart
// fields), alongside the thumb, so the multi-role parse can be exercised.
const audiodbArtistArtworkJSON = `{
  "artists": [
    {
      "strArtist": "Radiohead",
      "strArtistThumb": "https://theaudiodb.com/thumb.jpg",
      "strArtistLogo": "https://theaudiodb.com/logo.png",
      "strArtistFanart": "https://theaudiodb.com/fan1.jpg",
      "strArtistFanart3": "https://theaudiodb.com/fan3.jpg",
      "strBiographyEN": "bio"
    }
  ]
}`

// TestTheAudioDBArtistArtworkRoles: the fallback source parses strArtistLogo into a
// logo ref and the strArtistFanart* set into a background ref (the first, best-of),
// alongside the thumb poster — so it can fill a logo/background fanart.tv left empty.
func TestTheAudioDBArtistArtworkRoles(t *testing.T) {
	p, _ := audiodbStub(t, audiodbArtistArtworkJSON, 0)
	meta := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Record
	got := map[string]string{}
	for _, a := range meta.Artwork {
		got[a.Role] = a.URL
	}
	want := map[string]string{
		"poster":     "https://theaudiodb.com/thumb.jpg",
		"logo":       "https://theaudiodb.com/logo.png",
		"background": "https://theaudiodb.com/fan1.jpg", // the first non-empty fanart
	}
	for role, url := range want {
		if got[role] != url {
			t.Errorf("artwork[%s] = %q, want %q (all: %+v)", role, got[role], url, meta.Artwork)
		}
	}
}

// TestTheAudioDBArtistCandidatesByRole: the role parameter selects the set —
// "logo" → the single strArtistLogo, "background" → the strArtistFanart* list (in
// order, skipping the absent Fanart2).
func TestTheAudioDBArtistCandidatesByRole(t *testing.T) {
	p, _ := audiodbStub(t, audiodbArtistArtworkJSON, 0)
	logo := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "logo").Candidates
	if len(logo) != 1 || logo[0].URL != "https://theaudiodb.com/logo.png" {
		t.Errorf("logo candidates = %+v, want the single strArtistLogo", logo)
	}
	bg := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "background").Candidates
	want := []string{"https://theaudiodb.com/fan1.jpg", "https://theaudiodb.com/fan3.jpg"}
	if len(bg) != len(want) {
		t.Fatalf("background candidates = %d, want %d", len(bg), len(want))
	}
	for i, w := range want {
		if bg[i].URL != w {
			t.Errorf("background[%d].URL = %q, want %q", i, bg[i].URL, w)
		}
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
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "track", MusicbrainzID: mbid})
	meta := resp.Record
	if resp.Outcome != pluginapi.OutcomeMatched || !meta.Matched || meta.Source != "theaudiodb" {
		t.Errorf("meta = %+v (outcome %q), want matched theaudiodb result", meta, resp.Outcome)
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
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/track-mb.php?i="+mbid) {
		t.Errorf("request = %v, want a single track-mb.php?i=%s", got, mbid)
	}
}

func TestTheAudioDBTrackByName(t *testing.T) {
	p, seen := audiodbStub(t, audiodbTrackJSON, 0)
	meta := lookup(t, p, pluginapi.MediaRef{Kind: "track", Track: "Creep", Artist: "Radiohead"}).Record
	if meta.Overview != "Creep is the debut single by Radiohead." {
		t.Errorf("overview = %q, want the English description", meta.Overview)
	}
	// The name lookup hits searchtrack.php?s={artist}&t={track}.
	got := seen.all()
	if len(got) != 1 || !strings.Contains(got[0], "/searchtrack.php?s=Radiohead&t=Creep") {
		t.Errorf("request = %v, want a single searchtrack.php?s=Radiohead&t=Creep", got)
	}
}

func TestTheAudioDBTrackPicksLanguageDescription(t *testing.T) {
	body := `{"track":[{"strDescriptionEN":"English synopsis","strDescriptionDE":"German synopsis"}]}`
	p, _ := audiodbStubWithLanguage(t, body, 0, "de-DE")

	meta := lookup(t, p, pluginapi.MediaRef{Kind: "track", Track: "X"}).Record
	if meta.Overview != "German synopsis" {
		t.Errorf("overview = %q, want the German description for de-DE", meta.Overview)
	}
}

func TestTheAudioDBTrackFallsBackToEnglishDescription(t *testing.T) {
	// No German description present — the provider falls back to strDescriptionEN.
	body := `{"track":[{"strDescriptionEN":"English synopsis"}]}`
	p, _ := audiodbStubWithLanguage(t, body, 0, "de-DE")

	meta := lookup(t, p, pluginapi.MediaRef{Kind: "track", Track: "X"}).Record
	if meta.Overview != "English synopsis" {
		t.Errorf("overview = %q, want the English fallback description", meta.Overview)
	}
}

func TestTheAudioDBTrackNullIsNoMatch(t *testing.T) {
	// TheAudioDB answers an unknown track with {"track":null}.
	p, _ := audiodbStub(t, `{"track":null}`, 0)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "track", Track: "Nobody"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestTheAudioDBTrackNoDescriptionIsNoMatch(t *testing.T) {
	// A track record with no description in any language is no usable data.
	p, _ := audiodbStub(t, `{"track":[{"strTrack":"Creep"}]}`, 0)
	ref := pluginapi.MediaRef{Kind: "track", Track: "Creep", Artist: "Radiohead"}
	if got := lookup(t, p, ref).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestTheAudioDBTrackNoKeySkips(t *testing.T) {
	p, seen := audiodbStub(t, audiodbTrackJSON, 0)
	// A track with neither MBID nor name has nothing to key a lookup by.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "track"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("no-key outcome = %q, want no-match", got)
	}
	if got := seen.all(); len(got) != 0 {
		t.Errorf("expected zero requests for an unkeyed track lookup; saw %v", got)
	}
}

func TestTheAudioDBCaches(t *testing.T) {
	p, seen := audiodbStub(t, audiodbArtistJSON, 0)
	for i := 0; i < 3; i++ {
		if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeMatched {
			t.Fatalf("Lookup %d outcome = %q, want matched", i, got)
		}
	}
	// The response cache means a re-enrich of the same artist re-hits TheAudioDB once.
	if got := seen.all(); len(got) != 1 {
		t.Errorf("request count = %d, want 1 (cached); requests=%v", len(got), got)
	}
}

// TestTheAudioDBArtistAndTrackCachesDoNotCollide: the two caches are namespaced,
// so an artist and a track keyed by the same MBID never serve each other's result.
func TestTheAudioDBArtistAndTrackCachesDoNotCollide(t *testing.T) {
	body := `{
      "artists":[{"strArtistThumb":"https://x/artist.jpg"}],
      "track":[{"strDescriptionEN":"a synopsis"}]
    }`
	p, seen := audiodbStub(t, body, 0)
	artist := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Record
	if len(artist.Artwork) != 1 || artist.Artwork[0].URL != "https://x/artist.jpg" {
		t.Errorf("artist artwork = %+v, want the strArtistThumb", artist.Artwork)
	}
	track := lookup(t, p, pluginapi.MediaRef{Kind: "track", MusicbrainzID: mbid}).Record
	if track.Overview != "a synopsis" {
		t.Errorf("track overview = %q, want the strDescriptionEN", track.Overview)
	}
	got := seen.all()
	if len(got) != 2 {
		t.Fatalf("request count = %d, want 2 (artist + track, no cache collision); requests=%v", len(got), got)
	}
	if !strings.Contains(got[0], "/artist-mb.php?i=") || !strings.Contains(got[1], "/track-mb.php?i=") {
		t.Errorf("requests = %v, want artist-mb.php then track-mb.php", got)
	}
}

// TestTheAudioDBReadsItsSettingsPerCall: the key, the base URL and the language are
// not fields on the provider — they are read from the Host on every call, so an
// operator's save takes effect on the next call rather than on the next rebuild.
// TheAudioDB puts its key in the PATH, which is the one place that shows.
func TestTheAudioDBReadsItsSettingsPerCall(t *testing.T) {
	var paths []string
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "first", URL: baseURL, Language: "en-US"}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			_, _ = w.Write([]byte(audiodbArtistJSON))
		}),
	)
	p := New(host)
	lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: "one"})
	host.SetSettings(pluginapi.Settings{Enabled: true, Secret: "second", URL: baseURL, Language: "en-US"})
	lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: "two"})

	if len(paths) != 2 || !strings.HasPrefix(paths[0], "/first/") || !strings.HasPrefix(paths[1], "/second/") {
		t.Errorf("paths = %v, want the key read per call (/first/… then /second/…)", paths)
	}
}

// TestTheAudioDBLanguageFields states the two field-name rules directly, so the
// language mapping is asserted rather than inferred from two bodies.
func TestTheAudioDBLanguageFields(t *testing.T) {
	for _, tc := range []struct{ language, bio, desc string }{
		{"en-US", "strBiographyEN", "strDescriptionEN"},
		{"de-DE", "strBiographyDE", "strDescriptionDE"},
		{"fr", "strBiographyFR", "strDescriptionFR"},
		{"pt_BR", "strBiographyPT", "strDescriptionPT"},
		{"", "strBiographyEN", "strDescriptionEN"},
		{"  ", "strBiographyEN", "strDescriptionEN"},
	} {
		if got := biographyField(tc.language); got != tc.bio {
			t.Errorf("biographyField(%q) = %q, want %q", tc.language, got, tc.bio)
		}
		if got := descriptionField(tc.language); got != tc.desc {
			t.Errorf("descriptionField(%q) = %q, want %q", tc.language, got, tc.desc)
		}
	}
}
