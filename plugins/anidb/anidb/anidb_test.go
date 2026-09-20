package anidb

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// This provider's HTTP/XML layer, exercised against canned AniDB HTTP-API XML
// served in memory by sdktest.Host — the port of internal/enrich/anidb_test.go,
// with every assertion intact. The httptest handler came across verbatim; what
// changed is that there is no server and no port, because the provider asks its
// Host rather than a net/http client. No live network is ever touched.
//
// The base URL below is AniDB's own: PLAIN HTTP, on an EXPLICIT PORT, with a
// path. That is not decoration — it is what AniDB publishes and what the manifest
// defaults to, and every URL these tests watch the provider build has to survive
// it. What the SERVER's fetch check does with such a URL is proved where that
// check lives, in internal/plugins.
const apiBase = "http://api.anidb.net:9001/httpapi"

// settings is what the host publishes for one call. Secret is the registered
// AniDB HTTP-API CLIENT NAME rather than a token; AniDB keys its API by one.
func settings() pluginapi.Settings {
	return pluginapi.Settings{Enabled: true, Secret: "obelo-client", Language: "en-US", URL: apiBase}
}

const animeXML = `<?xml version="1.0" encoding="UTF-8"?>
<anime id="1">
  <titles>
    <title xml:lang="x-jat" type="main">Cowboy Bebop</title>
    <title xml:lang="en" type="official">Cowboy Bebop</title>
    <title xml:lang="ja" type="official">カウボーイビバップ</title>
  </titles>
  <description>In 2071, a ragtag crew of bounty hunters chases a bounty.</description>
  <picture>12345.jpg</picture>
  <startdate>1998-04-03</startdate>
  <tags><tag><name>space</name></tag><tag><name>noir</name></tag></tags>
</anime>`

// stub serves the AniDB HTTP API with canned XML and hands back the Host, whose
// Requests() carry the URL each call asked for.
func stub(t *testing.T, body string) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(body))
		}),
	)
	return New(host), host
}

// query is the parsed query of the n-th fetch this Host was asked for.
func query(t *testing.T, host *sdktest.Host, n int) url.Values {
	t.Helper()
	reqs := host.Requests()
	if n >= len(reqs) {
		t.Fatalf("the provider made %d requests, want at least %d", len(reqs), n+1)
	}
	u, err := url.Parse(reqs[n].URL)
	if err != nil {
		t.Fatalf("the provider built an unparseable URL %q: %v", reqs[n].URL, err)
	}
	return u.Query()
}

// TestAniDBLookupByID asserts the AniDB client resolves BY a pinned anime id into
// enrichment-only descriptive fields (title, overview, year, poster, genres) and
// NEVER surfaces anything beyond the AniDB id as identity.
func TestAniDBLookupByID(t *testing.T) {
	p, host := stub(t, animeXML)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", Title: "whatever", AniDBID: "1"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if got := query(t, host, 0).Get("aid"); got != "1" {
		t.Errorf("requested aid = %q, want 1", got)
	}
	meta := resp.Record
	if !meta.Matched || meta.Source != Source || meta.ExternalID != "1" {
		t.Errorf("meta identity = matched:%v source:%q id:%q, want matched anidb/1",
			meta.Matched, meta.Source, meta.ExternalID)
	}
	if meta.Name != "Cowboy Bebop" || meta.Year != 1998 {
		t.Errorf("title/year = %q/%d, want Cowboy Bebop/1998", meta.Name, meta.Year)
	}
	if meta.Overview == "" || len(meta.Genres) != 2 {
		t.Errorf("overview/genres = %q/%v, want both populated", meta.Overview, meta.Genres)
	}
	if len(meta.Artwork) != 1 || meta.Artwork[0].Role != "poster" {
		t.Errorf("artwork = %+v, want one poster", meta.Artwork)
	}
	if want := ImageBaseURL + "/12345.jpg"; meta.Artwork[0].URL != want {
		t.Errorf("poster = %q, want %q", meta.Artwork[0].URL, want)
	}
}

// The registered client name, the client version and protover travel on every
// request, because AniDB requires all three — and the client name is the SECRET
// the host resolved for this call, read per call rather than stashed.
func TestAniDBSendsItsRegisteredClientOnEveryRequest(t *testing.T) {
	p, host := stub(t, animeXML)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	q := query(t, host, 0)
	for field, want := range map[string]string{
		"request":   "anime",
		"client":    "obelo-client",
		"clientver": ClientVer,
		"protover":  "1",
		"aid":       "1",
	} {
		if got := q.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}

	// A rotated client name reaches the next call: Settings is read per call.
	s := settings()
	s.Secret = "obelo-client-2"
	host.SetSettings(s)
	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "2"},
	}); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}
	if got := query(t, host, 1).Get("client"); got != "obelo-client-2" {
		t.Errorf("client on the second call = %q, want the rotated name", got)
	}
}

// The request URL keeps the operator's scheme, host, PORT and path exactly as
// configured, with the query appended. AniDB's own base is plain http on :9001,
// so a provider that quietly normalized either would stop reaching the source.
func TestAniDBKeepsThePlainHTTPBaseAndItsExplicitPort(t *testing.T) {
	p, host := stub(t, animeXML)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	raw := host.Requests()[0].URL
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("the provider built an unparseable URL %q: %v", raw, err)
	}
	if u.Scheme != "http" {
		t.Errorf("scheme = %q, want http — AniDB's HTTP API is not served over TLS", u.Scheme)
	}
	if u.Hostname() != "api.anidb.net" {
		t.Errorf("host = %q, want api.anidb.net (the manifest's only allowlisted host)", u.Hostname())
	}
	if u.Port() != "9001" {
		t.Errorf("port = %q, want 9001 — the explicit port is part of the endpoint", u.Port())
	}
	if u.Path != "/httpapi" {
		t.Errorf("path = %q, want /httpapi", u.Path)
	}
}

// TestAniDBNoIDIsNoMatch asserts a lookup with no anime id is a graceful
// OutcomeNoMatch (AniDB ids are not naming-derived; matching by name is out of
// scope) — so an AniDB-led chain leaves an unpinned Title unmatched rather than
// guessing identity.
func TestAniDBNoIDIsNoMatch(t *testing.T) {
	p, host := stub(t, animeXML)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", Title: "No ID"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match for an unpinned title", resp.Outcome)
	}
	if got := len(host.Requests()); got != 0 {
		t.Errorf("AniDB was queried for a title with no anime id (%d requests); want zero", got)
	}
}

// A non-video kind is a no-match with no outbound call: AniDB serves the video
// kinds only.
func TestAniDBNonVideoKindIsNoMatch(t *testing.T) {
	p, host := stub(t, animeXML)

	for _, kind := range []string{"artist", "album", "track"} {
		resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: kind, AniDBID: "1"},
		})
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if resp.Outcome != pluginapi.OutcomeNoMatch {
			t.Errorf("kind %q: outcome = %q, want no-match (AniDB serves video only)", kind, resp.Outcome)
		}
	}
	if got := len(host.Requests()); got != 0 {
		t.Errorf("AniDB was queried for a non-video kind (%d requests); want zero", got)
	}
}

// TestAniDBUnknownAIDIsNoMatch asserts AniDB's <error>Unknown</error> reply is a
// no-match, not a hard error the chain would surface.
func TestAniDBUnknownAIDIsNoMatch(t *testing.T) {
	p, _ := stub(t, `<error>Unknown anime</error>`)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "movie", AniDBID: "999999"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match for an unknown aid", resp.Outcome)
	}
}

// Any OTHER <error> body — a banned or invalid client is the case AniDB is known
// for — is a Go error, because it describes OUR REQUEST and only an operator can
// fix it.
func TestAniDBABannedClientIsAGoError(t *testing.T) {
	p, _ := stub(t, `<error>Client Banned</error>`)

	if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	}); err == nil {
		t.Fatal("a banned client answered with no error; it must reach the operator")
	}
}

// The title preference: the configured language's main/official title wins, then
// any "main", then the first title present.
func TestAniDBPrefersTheConfiguredLanguagesTitle(t *testing.T) {
	const xml = `<anime id="1"><titles>
	  <title xml:lang="x-jat" type="main">Kaubōi Bibappu</title>
	  <title xml:lang="ja" type="official">カウボーイビバップ</title>
	  <title xml:lang="en" type="official">Cowboy Bebop</title>
	</titles><description>d</description></anime>`

	for lang, want := range map[string]string{
		"en-US": "Cowboy Bebop",
		"ja-JP": "カウボーイビバップ",
		"de-DE": "Kaubōi Bibappu", // no German title: the "main" one
	} {
		host := sdktest.New(
			sdktest.WithSettings(pluginapi.Settings{Enabled: true, Secret: "c", Language: lang, URL: apiBase}),
			sdktest.WithHandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(xml))
			}),
		)
		resp, err := New(host).Lookup(context.Background(), pluginapi.LookupRequest{
			Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
		})
		if err != nil {
			t.Fatalf("%s: %v", lang, err)
		}
		if resp.Record.Name != want {
			t.Errorf("language %s: title = %q, want %q", lang, resp.Record.Name, want)
		}
	}
}

// ParseYear is deliberately narrow: four digits inside a plausible range, and 0
// for anything else, so a malformed startdate never writes a nonsense year.
func TestAniDBParseYear(t *testing.T) {
	for in, want := range map[string]int{
		"1998": 1998,
		"2024": 2024,
		"1899": 0,
		"2201": 0,
		"19x8": 0,
		"":     0,
	} {
		if got := ParseYear(in); got != want {
			t.Errorf("ParseYear(%q) = %d, want %d", in, got, want)
		}
	}
}

// A record AniDB answers with nothing usable in it is a no-match, not a match
// with an empty name.
func TestAniDBAnEmptyRecordIsNoMatch(t *testing.T) {
	p, _ := stub(t, `<anime id="1"></anime>`)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", AniDBID: "1"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match for a record with no usable fields", resp.Outcome)
	}
}

// The instance response cache means a re-enrichment pass does not re-hit AniDB —
// which matters more here than anywhere else, because AniDB bans bursty clients.
func TestAniDBCachesRepeatLookups(t *testing.T) {
	p, host := stub(t, animeXML)
	ref := pluginapi.MediaRef{Kind: "show", AniDBID: "1"}

	for i := 0; i < 3; i++ {
		if _, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref}); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	// And the artwork picker shares the cache with the lookup, because both read
	// the same anime record.
	if _, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: ref, Role: "poster",
	}); err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if got := len(host.Requests()); got != 1 {
		t.Errorf("AniDB was hit %d times, want 1 (the rest served from cache)", got)
	}
}

// Search answers an EMPTY candidate list rather than "unavailable", and that is
// the difference the manifest's `search` declaration buys: the Edit-item box
// renders "no results" rather than "search unavailable". AniDB's HTTP API offers
// no free-text search, so there is nothing to fetch either.
func TestAniDBSearchAnswersNoCandidatesWithoutAsking(t *testing.T) {
	p, host := stub(t, animeXML)

	resp, err := p.Search(context.Background(), pluginapi.SearchRequest{Kind: "show", Query: "Bebop"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Errorf("outcome = %q, want matched with no candidates (not unavailable)", resp.Outcome)
	}
	if len(resp.Candidates) != 0 {
		t.Errorf("candidates = %+v, want none", resp.Candidates)
	}
	if got := len(host.Requests()); got != 0 {
		t.Errorf("a search fetched something (%d requests); want zero", got)
	}
}

// ArtworkCandidates offers the one cover image for the poster role on a pinned
// anime, and nothing at all for any other role — without a call.
func TestAniDBArtworkCandidates(t *testing.T) {
	p, host := stub(t, animeXML)
	ref := pluginapi.MediaRef{Kind: "show", AniDBID: "1"}

	resp, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: ref, Role: "poster",
	})
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want matched", resp.Outcome)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].Source != Source ||
		resp.Candidates[0].URL != ImageBaseURL+"/12345.jpg" {
		t.Errorf("candidates = %+v, want the one AniDB cover", resp.Candidates)
	}

	before := len(host.Requests())
	for _, role := range []string{"background", "logo", "cover"} {
		r, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
			Ref: ref, Role: role,
		})
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		if len(r.Candidates) != 0 {
			t.Errorf("role %q: candidates = %+v, want none (AniDB offers cover art only)", role, r.Candidates)
		}
	}
	// An unpinned ref offers nothing and asks nobody.
	r, err := p.ArtworkCandidates(context.Background(), pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "show", Title: "No ID"}, Role: "poster",
	})
	if err != nil || len(r.Candidates) != 0 {
		t.Errorf("an unpinned ref = (%+v, %v), want no candidates and no error", r.Candidates, err)
	}
	if got := len(host.Requests()); got != before {
		t.Errorf("a declined artwork role fetched something (%d new requests); want zero", got-before)
	}
}
