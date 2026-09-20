package musicbrainz

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// The port's scaffolding.
//
// Every test in this package came from internal/enrich, where it drove a
// *MusicBrainzProvider against an httptest.Server. The handlers are carried over
// VERBATIM — sdktest.WithHandlerFunc takes one without a listener or a port — and
// these helpers put the old call shapes back on top of the new contract, so the
// assertions below are the ones that were being made before the conversion rather
// than re-derived from it.
//
// The two hosts are spelled out here for the same reason: the MusicBrainz web
// service is Settings.URL and the COVER ART ARCHIVE is Settings.URL2, which is the
// substantive thing issue 06 changed, and a test that got them the wrong way round
// would still pass against a single handler.

const (
	// mbHost / caaHost are the two hosts these tests configure. Neither resolves;
	// sdktest answers them in memory. They carry NO path, so a handler's
	// r.URL.Path reads exactly as it did against an httptest.Server.
	mbHost  = "http://mb.test"
	caaHost = "http://caa.test"
)

// errUnavailable is what this package's tests match an OutcomeUnavailable answer
// against. It is a sentinel and not a string comparison, because the DETAIL is
// the SDK's sentence and this package must not grow assertions about its wording.
var errUnavailable = errors.New("musicbrainz: the source could not answer")

// unavailable carries the guest's Detail alongside errUnavailable, so a test that
// wants to see the reason can print it.
type unavailable struct{ detail string }

func (u *unavailable) Error() string        { return "unavailable: " + u.detail }
func (u *unavailable) Is(target error) bool { return target == errUnavailable }

// newProvider builds a provider on an in-memory host serving handler for BOTH
// hosts — the shape of a test whose stub answered the web service and the Cover
// Art Archive from one httptest.Server, which most of them did.
func newProvider(t *testing.T, handler http.HandlerFunc, opts ...sdktest.Option) (*Provider, *sdktest.Host) {
	t.Helper()
	all := append([]sdktest.Option{
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, URL: mbHost, URL2: caaHost, Language: "en"}),
		sdktest.WithHandlerFunc(handler),
	}, opts...)
	host := sdktest.New(all...)
	return New(host), host
}

// newTwoHostProvider builds a provider whose web service and Cover Art Archive
// answer from DIFFERENT handlers, which is how a test proves a URL was built off
// URL2 rather than off URL.
func newTwoHostProvider(t *testing.T, ws, caa http.HandlerFunc) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(pluginapi.Settings{Enabled: true, URL: mbHost, URL2: caaHost, Language: "en"}),
		sdktest.WithHostHandler("mb.test", ws),
		sdktest.WithHostHandler("caa.test", caa),
	)
	return New(host), host
}

// fastBackoff shrinks the 503 ladder's base wait for the length of one test, so
// proving that a ladder waits costs milliseconds rather than seconds. It is the
// Go provider's RetryBackoff field, which existed for exactly this.
func fastBackoff(t *testing.T) {
	t.Helper()
	prev := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = prev })
}

// noPacing is the option every test wants and no test should have to think about:
// the operator's rate limit set to 0, which is "do not throttle at all"
// (ADR-0049). These tests measure requests, not seconds.
//
// It is not the pacer being bypassed — main.go's PacedHost is not in the picture
// here at all, because sdktest.Host is handed to New directly. The setting is
// there so that a test which DOES wrap a pacer (see pacer_test.go) is visibly
// different from one that does not.
func noPacing() sdktest.Option { return sdktest.WithRateLimitMillis(0) }

// lookup drives Lookup and puts the Go provider's (record, error) shape back, so
// a ported assertion reads as it did. errNoMatch is OutcomeNoMatch; errUnavailable
// is OutcomeUnavailable, which the Go provider spelled as a transient error.
func lookup(p *Provider, ref pluginapi.MediaRef) (pluginapi.MetadataRecord, error) {
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	switch resp.Outcome {
	case pluginapi.OutcomeMatched:
		return resp.Record, nil
	case pluginapi.OutcomeNoMatch:
		return pluginapi.MetadataRecord{}, errNoMatch
	case pluginapi.OutcomeUnavailable:
		return pluginapi.MetadataRecord{}, &unavailable{resp.Detail}
	default:
		return pluginapi.MetadataRecord{}, errors.New("unexpected lookup outcome " + string(resp.Outcome))
	}
}

// search drives Search with the same translation.
func search(p *Provider, kind, query string, page pluginapi.Page, artist, release string) ([]pluginapi.SearchCandidate, error) {
	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{
		Kind: kind, Query: query, Artist: artist, Release: release, Page: page,
	})
	if err != nil {
		return nil, err
	}
	switch resp.Outcome {
	case pluginapi.OutcomeMatched:
		return resp.Candidates, nil
	case pluginapi.OutcomeUnavailable:
		return nil, &unavailable{resp.Detail}
	default:
		return nil, errors.New("unexpected search outcome " + string(resp.Outcome))
	}
}

// artworkCandidates drives ArtworkCandidates with the same translation.
func artworkCandidates(p *Provider, ref pluginapi.MediaRef, role string) ([]pluginapi.ArtworkCandidate, error) {
	resp, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{Ref: ref, Role: role})
	if err != nil {
		return nil, err
	}
	switch resp.Outcome {
	case pluginapi.OutcomeMatched:
		return resp.Candidates, nil
	case pluginapi.OutcomeUnavailable:
		return nil, &unavailable{resp.Detail}
	default:
		return nil, errors.New("unexpected artwork outcome " + string(resp.Outcome))
	}
}

// tracklist drives AlbumTracklist. OutcomeNoMatch is "this album has no
// tracklist", which the Go provider spelled ErrNoTracklist.
func tracklist(p *Provider, req pluginapi.TracklistRequest) ([]pluginapi.TrackCandidate, error) {
	resp, err := p.AlbumTracklist(context.Background(), req)
	if err != nil {
		return nil, err
	}
	switch resp.Outcome {
	case pluginapi.OutcomeMatched:
		return resp.Tracks, nil
	case pluginapi.OutcomeNoMatch:
		return nil, errNoTracklist
	case pluginapi.OutcomeUnavailable:
		return nil, &unavailable{resp.Detail}
	default:
		return nil, errors.New("unexpected tracklist outcome " + string(resp.Outcome))
	}
}

// editions drives ReleaseGroupEditions.
func editions(p *Provider, rgID string) ([]pluginapi.ReleaseEdition, error) {
	resp, err := p.ReleaseGroupEditions(context.Background(), pluginapi.ReleaseEditionsRequest{ReleaseGroupID: rgID})
	if err != nil {
		return nil, err
	}
	switch resp.Outcome {
	case pluginapi.OutcomeMatched:
		return resp.Editions, nil
	case pluginapi.OutcomeUnavailable:
		return nil, &unavailable{resp.Detail}
	default:
		return nil, errors.New("unexpected editions outcome " + string(resp.Outcome))
	}
}

// requestPaths is the `seen []string` those tests kept by hand.
func requestPaths(h *sdktest.Host) []string { return h.Paths() }

// requestURLs is the same for the tests that asserted the whole query string.
func requestURLs(h *sdktest.Host) []string {
	reqs := h.Requests()
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.URL)
	}
	return out
}

// jsonHandler answers every request with one canned body, which is the shape of
// the simplest ported stubs.
func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// assertUnavailable fails unless err is the "could not answer" answer.
func assertUnavailable(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("%s: err = %v, want the unavailable answer — an outage must never reach "+
			"the host as a Go error, which is a strike against the plugin", what, err)
	}
}

// assertGoError fails unless err is a plain Go error — a *pluginsdk.FetchError the
// SDK did NOT classify as an outage, which is what parks the item.
func assertGoError(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error at all", what)
	}
	if errors.Is(err, errUnavailable) {
		t.Fatalf("%s: err = %v, want a Go error — this answer describes OUR REQUEST and "+
			"asking again changes nothing", what, err)
	}
	var fe *pluginsdk.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("%s: err = %v (%T), want a *pluginsdk.FetchError", what, err, err)
	}
}
