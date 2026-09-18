package musicbrainz

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The provider's request/parse layer, ported from internal/enrich's
// tv_music_provider_test.go, search_improvements_test.go and search_test.go. The
// canned JSON and the assertions are the ones those tests made; what changed is
// that the bytes arrive from an in-memory host instead of an httptest.Server.

// mbStub is the canned MusicBrainz those tests shared: one artist, one
// release-group, one recording, addressed by path.
func mbStub(t *testing.T) (*Provider, *sdktest.Host) {
	t.Helper()
	return newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/artist":
			_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","disambiguation":"English rock band","area":{"name":"Oxford"},"tags":[{"name":"alternative rock"},{"name":"art rock"}]}]}`))
		case "/release-group":
			_, _ = w.Write([]byte(`{"release-groups":[{"id":"rg-1","first-release-date":"1997-05-21","tags":[{"name":"alternative rock"}]}]}`))
		case "/recording":
			_, _ = w.Write([]byte(`{"recordings":[{"id":"rec-1","title":"Paranoid Android"}]}`))
		default:
			http.NotFound(w, r)
		}
	}, noPacing())
}

func TestMusicBrainzArtistAlbumTrack(t *testing.T) {
	p, _ := mbStub(t)

	artist, err := lookup(p, pluginapi.MediaRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("artist lookup: %v", err)
	}
	if !artist.Matched || artist.Overview == "" || len(artist.Genres) == 0 || artist.ExternalID != "mb-1" {
		t.Errorf("artist metadata wrong: %+v", artist)
	}

	album, err := lookup(p, pluginapi.MediaRef{Kind: "album", Album: "OK Computer", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("album lookup: %v", err)
	}
	if !album.Matched || album.ReleaseDate != "1997-05-21" || len(album.Genres) == 0 {
		t.Errorf("album metadata wrong: %+v", album)
	}
	// THE COVER COMES OFF URL2. This is the assertion issue 06 is about: the Cover
	// Art Archive used to be a provider row of its own whose base URL the host
	// resolved into this source's second URL, and it is now this plugin's own
	// second host, defaulted by its own manifest.
	if len(album.Artwork) != 1 || album.Artwork[0].Role != "cover" ||
		album.Artwork[0].URL != caaHost+"/release-group/rg-1/front-500" {
		t.Errorf("album cover wrong: %+v", album.Artwork)
	}

	track, err := lookup(p, pluginapi.MediaRef{Kind: "track", Track: "Paranoid Android", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("track lookup: %v", err)
	}
	if !track.Matched || track.Name != "Paranoid Android" {
		t.Errorf("track metadata wrong: %+v", track)
	}
	if !track.FromSearch {
		t.Error("a track resolved by SEARCH must say so, or the host cannot judge it (ADR-0057)")
	}
}

func TestMusicBrainzNoResultsIsNoMatch(t *testing.T) {
	p, _ := newProvider(t, jsonHandler(`{"artists":[],"release-groups":[],"recordings":[]}`), noPacing())
	if _, err := lookup(p, pluginapi.MediaRef{Kind: "artist", Title: "Nobody"}); !errors.Is(err, errNoMatch) {
		t.Errorf("artist err = %v, want no-match", err)
	}
}

// A 503 (MusicBrainz rate-limit/temporary-unavailable) whose headers say the
// refusal is OURS is retried with back-off rather than dropped: the second attempt
// succeeds.
func TestMusicBrainzRetriesOn503(t *testing.T) {
	fastBackoff(t)
	var calls int
	p, _ := newProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// "slow down" is OUR quota by the ourQuota rule (no x-ratelimit-who, and
			// nothing in the text saying otherwise), which is what makes this one
			// retryable in place.
			http.Error(w, "slow down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[{"id":"mb-1","type":"Group","tags":[{"name":"rock"}]}]}`))
	}, noPacing())

	got, err := lookup(p, pluginapi.MediaRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("lookup after 503 retry: %v", err)
	}
	if !got.Matched || got.ExternalID != "mb-1" {
		t.Errorf("metadata wrong after retry: %+v", got)
	}
	if calls != 2 {
		t.Errorf("want 2 attempts (503 then 200), got %d", calls)
	}
}

// Non-Music kinds are not this source's and cost no request. The Go provider
// answered ErrNoMatch for them; the contract spells that OutcomeNoMatch.
func TestMusicBrainzDoesNotAnswerVideoKinds(t *testing.T) {
	p, host := newProvider(t, jsonHandler(`{}`), noPacing())
	for _, kind := range []string{"movie", "show", "season", "episode", ""} {
		if _, err := lookup(p, pluginapi.MediaRef{Kind: kind, Title: "Inception"}); !errors.Is(err, errNoMatch) {
			t.Errorf("%q err = %v, want no-match", kind, err)
		}
	}
	if n := len(requestPaths(host)); n != 0 {
		t.Errorf("a video kind cost %d requests; it must cost none", n)
	}
}

// TestSettingsAreReadPerCall: both base URLs and the language come from
// Host.Settings() on every call, not from construction, because that is where the
// host publishes them and a settings save is not a rebuild from inside a guest.
func TestSettingsAreReadPerCall(t *testing.T) {
	p, host := newProvider(t, jsonHandler(
		`{"release-groups":[{"id":"rg-9","first-release-date":"2001-01-01"}]}`), noPacing())

	first, err := lookup(p, pluginapi.MediaRef{Kind: "album", Album: "A"})
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if first.Artwork[0].URL != caaHost+"/release-group/rg-9/front-500" {
		t.Fatalf("cover = %q, want it off the first URL2", first.Artwork[0].URL)
	}

	host.SetSettings(pluginapi.Settings{Enabled: true, URL: mbHost, URL2: "http://mirror.test/caa"})
	second, err := lookup(p, pluginapi.MediaRef{Kind: "album", Album: "A"})
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if second.Artwork[0].URL != "http://mirror.test/caa/release-group/rg-9/front-500" {
		t.Fatalf("cover = %q, want it off the operator's NEW second URL — a settings save "+
			"must reach a running plugin", second.Artwork[0].URL)
	}
}

// TestYearFromDate is the date parser's table, moved with the provider. The host
// keeps its own copy (internal/enrich/externalid.go) for the provider dates it
// parses itself; this one is the plugin's.
func TestYearFromDate(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"1997-05-21", 1997},
		{"1997", 1997},
		{"", 0},
		{"199", 0},
		{"abcd-01-01", 0},
	} {
		if got := YearFromDate(c.in); got != c.want {
			t.Errorf("YearFromDate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- The Cover Art Archive, which is the second host --------------------------

const caaReleaseGroupJSON = `{
  "images": [
    {"image": "https://caa/full-1.jpg", "front": true, "thumbnails": {"250": "https://caa/250-1.jpg", "500": "https://caa/500-1.jpg"}},
    {"image": "https://caa/full-2.jpg", "front": false, "thumbnails": {"500": "https://caa/500-2.jpg"}}
  ]
}`

// TestMusicBrainzArtworkCandidatesAlbumCovers: an album image query hits the Cover
// Art Archive release-group endpoint — ON URL2, NOT on the web service — and maps
// the images into candidates, preferring the 500px derivative.
func TestMusicBrainzArtworkCandidatesAlbumCovers(t *testing.T) {
	var seen []string
	p, _ := newTwoHostProvider(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the cover-art manifest was fetched from the WEB SERVICE host: %s", r.URL.Path)
			http.NotFound(w, r)
		},
		func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.URL.Path)
			if strings.HasPrefix(r.URL.Path, "/release-group/") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(caaReleaseGroupJSON))
				return
			}
			http.NotFound(w, r)
		})

	cands, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-123"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(seen) != 1 || seen[0] != "/release-group/rg-123" {
		t.Fatalf("expected one /release-group/rg-123 call, saw %v", seen)
	}
	if len(cands) != 2 {
		t.Fatalf("cover candidates = %d, want 2", len(cands))
	}
	if cands[0].URL != "https://caa/500-1.jpg" || cands[0].Source != CoverArtSource {
		t.Errorf("candidate[0] = %+v (want the 500px derivative)", cands[0])
	}
}

// TestMusicBrainzArtworkCandidatesArtistNone: an Artist has no listable image set
// (CAA is release-group keyed), so it yields no candidates and makes no call.
func TestMusicBrainzArtworkCandidatesArtistNone(t *testing.T) {
	p, host := newProvider(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, noPacing())

	cands, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: "art-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if n := len(requestPaths(host)); n != 0 || len(cands) != 0 {
		t.Errorf("artist yielded candidates/made a call: calls=%d cands=%d", n, len(cands))
	}
}

// An album with no pinned MBID has nothing to list and makes no call either.
func TestMusicBrainzArtworkCandidatesUnresolvedAlbumNone(t *testing.T) {
	p, host := newProvider(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, noPacing())

	cands, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if n := len(requestPaths(host)); n != 0 || len(cands) != 0 {
		t.Errorf("an unresolved album yielded candidates/made a call: calls=%d cands=%d", n, len(cands))
	}
}

// TestMusicBrainzArtworkCandidatesNoArt404: a 404 from the Cover Art Archive is the
// normal "no cover art" outcome — no candidates, no error, and NOT the unavailable
// answer, which would have the host retry a release-group that simply has no art.
func TestMusicBrainzArtworkCandidatesNoArt404(t *testing.T) {
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, noPacing())

	cands, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "rg-none"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %d, want 0 on a 404", len(cands))
	}
}

// TestTheGuestSendsNoUserAgentOfItsOwn is what replaced
// internal/enrich's TestMusicBrainzSendsUserAgent.
//
// MusicBrainz requires "Application name/<version> ( contact )" and throttles
// anonymous agents harder than identified ones, so the Go provider set the header
// on every request it made, to BOTH hosts, and a test asserted it. A guest cannot:
// the host writes the agent for every fetch and DROPS one a guest sent
// (.scratch/bundled-plugins issue 02, ADR-0059 decision 7), which is why
// internal/useragent owns that string and internal/plugins owns the test that it
// leaves the server.
//
// What is left to assert here is the half this package can get wrong: that it does
// not try. A guest that set its own agent would be silently overridden, and the
// silence is the problem — so this fails if one ever appears.
func TestTheGuestSendsNoUserAgentOfItsOwn(t *testing.T) {
	p, host := newTwoHostProvider(t,
		jsonHandler(`{"id":"mbid-1","title":"Doolittle"}`),
		jsonHandler(`{"images":[{"image":"http://img/1.jpg","front":true}]}`))

	if _, err := lookup(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "mbid-1"}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := artworkCandidates(p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: "mbid-1"}, "cover"); err != nil {
		t.Fatalf("artwork candidates: %v", err)
	}
	reqs := host.Requests()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (the web service and the cover-art manifest)", len(reqs))
	}
	for _, r := range reqs {
		for _, h := range r.Headers {
			if strings.EqualFold(h.Name, "User-Agent") {
				t.Errorf("%s carries a guest User-Agent %q; the host writes that header and "+
					"drops this one, so setting it here only hides the real identity", r.URL, h.Value)
			}
		}
		if !hasAccept(r.Headers) {
			t.Errorf("%s sent no Accept header; the Go provider asked for application/json", r.URL)
		}
	}
}

func hasAccept(hs []pluginapi.FetchHeader) bool {
	for _, h := range hs {
		if strings.EqualFold(h.Name, "Accept") && h.Value == "application/json" {
			return true
		}
	}
	return false
}
