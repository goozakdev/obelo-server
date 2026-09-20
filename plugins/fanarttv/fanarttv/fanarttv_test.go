package fanarttv

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The fanart.tv provider's request/parse layer, exercised against an in-memory
// host serving canned fanart.tv JSON. These are the tests
// internal/enrich/fanarttv_test.go carried, assertion for assertion, against the
// same canned bodies — what changed is the seam: the old httptest.Server handler
// is now an sdktest.Host handler, and the `seen []string` the old stub kept by
// hand is host.Paths(). No live network is ever touched, and none ever was.

// baseURL is a host with NO path, so an asserted request path reads exactly as it
// did when the old test asserted r.URL.Path against an httptest server.
const baseURL = "https://webservice.fanart.tv"

const mbid = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

const fanartArtistJSON = `{
  "name": "Radiohead",
  "mbid_id": "a74b1b7f-71a5-4011-9441-d0b5e4122711",
  "artistthumb": [
     {"id": "1", "url": "https://assets.fanart.tv/thumb-low.jpg", "likes": "3"},
     {"id": "2", "url": "https://assets.fanart.tv/thumb-best.jpg", "likes": "27"},
     {"id": "3", "url": "https://assets.fanart.tv/thumb-mid.jpg", "likes": "9"}
  ],
  "artistbackground": [
     {"id": "4", "url": "https://assets.fanart.tv/bg-low.jpg", "likes": "2"},
     {"id": "5", "url": "https://assets.fanart.tv/bg-best.jpg", "likes": "8"}
  ],
  "hdmusiclogo": [
     {"id": "6", "url": "https://assets.fanart.tv/hdlogo.png", "likes": "4"}
  ],
  "musiclogo": [
     {"id": "7", "url": "https://assets.fanart.tv/logo-sd.png", "likes": "40"}
  ]
}`

// fanartStub is the old test's stub: one canned body and status for every request,
// with the paths recorded so a test can assert the lookup is id-keyed. The Host is
// returned so a test can read them back.
func fanartStub(t *testing.T, body string, status int) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "k", URL: baseURL}),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if status != 0 {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		}),
	)
	return New(host), host
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

func TestFanartTVBestArtistThumb(t *testing.T) {
	p, host := fanartStub(t, fanartArtistJSON, 0)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid})
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	meta := resp.Record
	if !meta.Matched || meta.Source != "fanart.tv" {
		t.Errorf("meta = %+v, want matched fanart.tv result", meta)
	}
	// The highest-likes image for each role is parsed into its own ref: the artist
	// photo (poster), the artistbackground (background), and the logo (logo). Logos
	// coalesce hdmusiclogo AHEAD of musiclogo, so the HD lettering leads regardless of
	// the SD logo's higher likes.
	got := map[string]string{}
	for _, a := range meta.Artwork {
		got[a.Role] = a.URL
	}
	want := map[string]string{
		"poster":     "https://assets.fanart.tv/thumb-best.jpg",
		"background": "https://assets.fanart.tv/bg-best.jpg",
		"logo":       "https://assets.fanart.tv/hdlogo.png",
	}
	for role, url := range want {
		if got[role] != url {
			t.Errorf("artwork[%s] = %q, want %q (all: %+v)", role, got[role], url, meta.Artwork)
		}
	}
	// The request is MBID-keyed.
	seen := host.Paths()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/music/"+mbid) {
		t.Errorf("request paths = %v, want a single /music/%s", seen, mbid)
	}
}

func TestFanartTVBackgroundOrLogoOnlyMatches(t *testing.T) {
	// A record with no artist photo still matches when it carries a background or a
	// logo — those roles stand on their own (the artist photo is no longer required).
	p, _ := fanartStub(t, `{"name":"X","artistbackground":[{"url":"https://x/bg.jpg","likes":"1"}]}`, 0)
	resp := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid})
	if len(resp.Record.Artwork) != 1 || resp.Record.Artwork[0].Role != "background" {
		t.Errorf("artwork = %+v, want a single background ref", resp.Record.Artwork)
	}
}

func TestFanartTVNoImageIsNoMatch(t *testing.T) {
	// A 200 carrying none of the three image roles (thumb/background/logo) is a
	// no-match — there is nothing for the fill-only chain to contribute.
	p, _ := fanartStub(t, `{"name":"X"}`, 0)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestFanartTVNotFoundIsNoMatch(t *testing.T) {
	// fanart.tv answers an unknown MBID with 404 — the normal "no record" outcome.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match", got)
	}
}

func TestFanartTVNonArtistOrNoMBIDSkips(t *testing.T) {
	p, host := fanartStub(t, fanartArtistJSON, 0)
	// A non-artist (non-video) kind is not fanart.tv's.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("album outcome = %q, want no-match", got)
	}
	// An artist with no MBID is skipped entirely — fanart.tv is strictly MBID-keyed.
	if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist"}).Outcome; got != pluginapi.OutcomeNoMatch {
		t.Errorf("no-mbid outcome = %q, want no-match", got)
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("expected zero requests for non-artist / no-mbid lookups; saw %v", seen)
	}
}

func TestFanartTVCachesByMBID(t *testing.T) {
	p, host := fanartStub(t, fanartArtistJSON, 0)
	for i := 0; i < 3; i++ {
		if got := lookup(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}).Outcome; got != pluginapi.OutcomeMatched {
			t.Fatalf("Lookup %d outcome = %q, want matched", i, got)
		}
	}
	// The response cache means a re-enrich of the same artist re-hits fanart.tv once.
	if seen := host.Paths(); len(seen) != 1 {
		t.Errorf("request count = %d, want 1 (cached); paths=%v", len(seen), seen)
	}
}

// TestFanartTVArtistCandidates: the Artist Photo picker (artwork-management/02)
// surfaces the FULL artistthumb[] as candidates — not just the one "best" the
// single-image Lookup collapses to — highest-"likes" first, tagged fanart.tv, and
// MBID-keyed.
func TestFanartTVArtistCandidates(t *testing.T) {
	p, host := fanartStub(t, fanartArtistJSON, 0)
	resp := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	cands := resp.Candidates
	// All three thumbs are surfaced (the list is no longer discarded).
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3 (the full artistthumb[])", len(cands))
	}
	// Ordered by likes descending: best(27) → mid(9) → low(3).
	want := []string{
		"https://assets.fanart.tv/thumb-best.jpg",
		"https://assets.fanart.tv/thumb-mid.jpg",
		"https://assets.fanart.tv/thumb-low.jpg",
	}
	for i, w := range want {
		if cands[i].URL != w {
			t.Errorf("candidate[%d].URL = %q, want %q", i, cands[i].URL, w)
		}
		if cands[i].Source != "fanart.tv" {
			t.Errorf("candidate[%d].Source = %q, want fanart.tv", i, cands[i].Source)
		}
	}
	// The request is MBID-keyed, exactly like the single-image Lookup.
	seen := host.Paths()
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "/music/"+mbid) {
		t.Errorf("request paths = %v, want a single /music/%s", seen, mbid)
	}
}

// TestFanartTVArtistCandidatesByRole: the role parameter selects which fanart.tv
// image set the picker surfaces — "background" → artistbackground[], "logo" → the
// coalesced logos (hdmusiclogo ahead of musiclogo). Each is highest-"likes" first
// within its own set, and the artist-photo grid is unaffected.
func TestFanartTVArtistCandidatesByRole(t *testing.T) {
	p, _ := fanartStub(t, fanartArtistJSON, 0)
	bg := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "background").Candidates
	wantBG := []string{"https://assets.fanart.tv/bg-best.jpg", "https://assets.fanart.tv/bg-low.jpg"}
	if len(bg) != len(wantBG) {
		t.Fatalf("background candidates = %d, want %d", len(bg), len(wantBG))
	}
	for i, w := range wantBG {
		if bg[i].URL != w {
			t.Errorf("background[%d].URL = %q, want %q", i, bg[i].URL, w)
		}
	}
	logos := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "logo").Candidates
	// HD logo leads the SD one despite the SD's higher likes (HD is prepended first).
	wantLogos := []string{"https://assets.fanart.tv/hdlogo.png", "https://assets.fanart.tv/logo-sd.png"}
	if len(logos) != len(wantLogos) {
		t.Fatalf("logo candidates = %d, want %d", len(logos), len(wantLogos))
	}
	for i, w := range wantLogos {
		if logos[i].URL != w {
			t.Errorf("logo[%d].URL = %q, want %q", i, logos[i].URL, w)
		}
	}
}

func TestFanartTVArtistCandidatesNonArtistOrNoMBID(t *testing.T) {
	p, host := fanartStub(t, fanartArtistJSON, 0)
	// A non-artist kind: fanart.tv owns no listable set there (video lists via the
	// video lead). ErrSearchUnavailable was the Go provider's word for it; the
	// contract's is OutcomeUnavailable, and the host maps one back to the other.
	if got := candidates(t, p, pluginapi.MediaRef{Kind: "album", MusicbrainzID: mbid}, "cover").Outcome; got != pluginapi.OutcomeUnavailable {
		t.Errorf("album outcome = %q, want unavailable", got)
	}
	// An artist with no MBID is skipped entirely — strictly MBID-keyed — no call, no
	// candidates, no error (the picker degrades gracefully).
	resp := candidates(t, p, pluginapi.MediaRef{Kind: "artist"}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 0 {
		t.Errorf("no-mbid = %+v, want matched with no candidates", resp)
	}
	if seen := host.Paths(); len(seen) != 0 {
		t.Errorf("expected zero requests for non-artist / no-mbid candidates; saw %v", seen)
	}
}

func TestFanartTVArtistCandidatesNotFoundIsEmpty(t *testing.T) {
	// A 404 (unknown MBID) is the normal "no images" outcome — empty, not an error.
	p, _ := fanartStub(t, `{"status":"error","error message":"Not found"}`, http.StatusNotFound)
	resp := candidates(t, p, pluginapi.MediaRef{Kind: "artist", MusicbrainzID: mbid}, "poster")
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 0 {
		t.Errorf("404 = %+v, want matched with no candidates", resp)
	}
}
