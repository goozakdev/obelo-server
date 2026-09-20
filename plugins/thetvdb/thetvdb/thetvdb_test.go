package thetvdb

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// This provider's HTTP/parse layer, exercised against canned TheTVDB v4-shaped
// JSON served in memory by sdktest.Host — the port of
// internal/enrich/thetvdb_test.go, with every assertion intact. The httptest
// handler came across verbatim; what changed is that there is no server and no
// port, because the provider asks its Host rather than a net/http client. No live
// network is ever touched.

// apiBase is the operator's TheTVDB base URL as the host resolves it for a call.
// It carries no path, so an asserted request path reads exactly as it did when
// these tests ran against an httptest.Server.
const apiBase = "https://api4.thetvdb.com"

// settings is what the host publishes for one call.
func settings() pluginapi.Settings {
	return pluginapi.Settings{Enabled: true, Secret: "k", Language: "en-US", URL: apiBase}
}

// stub serves a TheTVDB-shaped API: a /login that mints a token and data
// endpoints that require the bearer token. It records the path of every DATA
// request (login excluded) so a test can assert the token flow, id-vs-name
// resolution, and the response cache. handlers is a per-path map; a missing path
// 404s (TheTVDB's "no record" answer).
type stub struct {
	handlers  map[string]http.HandlerFunc
	dataReqs  []string // data-endpoint paths hit (login excluded)
	logins    int
	lastToken string // last Authorization bearer seen on a data request

	// unauthorizeNext makes the next N data requests answer 401 whatever token
	// they carry, which is how an EXPIRED token is simulated.
	unauthorizeNext int
}

func provider(t *testing.T, s *stub) (*Provider, *sdktest.Host) {
	t.Helper()
	host := sdktest.New(
		sdktest.WithSettings(settings()),
		sdktest.WithHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/login" {
				s.logins++
				var body struct {
					APIKey string `json:"apikey"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"success","data":{"token":"tok-` + body.APIKey + `-` +
					itoa(s.logins) + `"}}`))
				return
			}
			// Data endpoints require the bearer token minted by /login.
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			s.dataReqs = append(s.dataReqs, r.URL.Path)
			s.lastToken = strings.TrimPrefix(auth, "Bearer ")
			if s.unauthorizeNext > 0 {
				s.unauthorizeNext--
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h, ok := s.handlers[r.URL.Path]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			h(w, r)
		}),
	)
	return New(host), host
}

// itoa avoids pulling strconv in for one digit.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "many"
}

func json200(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

// lookup runs one lookup and fails the test on a Go error, so each case below
// reads as it did when Lookup returned a record and an error.
func lookup(t *testing.T, p *Provider, ref pluginapi.MediaRef) pluginapi.LookupResponse {
	t.Helper()
	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{Ref: ref})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return resp
}

const seriesJSON = `{"status":"success","data":{
  "id":121361,
  "name":"Game of Thrones",
  "overview":"Seven noble families fight for control of Westeros.",
  "image":"https://artworks.thetvdb.com/series/got.jpg",
  "genres":[{"name":"Drama"},{"name":"Fantasy"}]
}}`

const episodesJSON = `{"status":"success","data":{"episodes":[
  {"seasonNumber":1,"number":4,"name":"Cripples, Bastards","overview":"Ep four.","image":"https://artworks.thetvdb.com/e4.jpg"},
  {"seasonNumber":1,"number":5,"name":"The Wolf and the Lion","overview":"Ned uncovers the truth.","image":"https://artworks.thetvdb.com/e5.jpg"}
]}}`

func TestTheTVDBShowByIDLoginThenReuse(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/series/121361": json200(seriesJSON),
		"/series/999":    json200(strings.Replace(seriesJSON, "121361", "999", 1)),
	}}
	p, _ := provider(t, s)

	got := lookup(t, p, pluginapi.MediaRef{Kind: "show", Title: "GoT", TheTVDBID: "121361"}).Record
	if got.Source != Source || !got.Matched {
		t.Errorf("source/matched = %q/%v, want thetvdb/true", got.Source, got.Matched)
	}
	if got.Name != "Game of Thrones" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Overview != "Seven noble families fight for control of Westeros." {
		t.Errorf("overview = %q", got.Overview)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Drama" || got.Genres[1] != "Fantasy" {
		t.Errorf("genres = %v, want [Drama Fantasy]", got.Genres)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].Role != "poster" ||
		got.Artwork[0].URL != "https://artworks.thetvdb.com/series/got.jpg" {
		t.Errorf("artwork = %+v, want a poster ref", got.Artwork)
	}
	// It resolved directly by id (no /search), and logged in exactly once.
	if len(s.dataReqs) != 1 || s.dataReqs[0] != "/series/121361" {
		t.Errorf("data reqs = %v, want a single /series/121361 lookup", s.dataReqs)
	}
	if s.logins != 1 || s.lastToken != "tok-k-1" {
		t.Errorf("logins=%d token=%q, want 1 login + minted token", s.logins, s.lastToken)
	}

	// A second, uncached lookup reuses the token — no second login.
	lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "999"})
	if s.logins != 1 {
		t.Errorf("logins = %d after a second lookup, want still 1 (token reused)", s.logins)
	}
}

func TestTheTVDBShowByNameSearchesFirst(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/search":        json200(`{"data":[{"tvdb_id":"121361","name":"Game of Thrones"}]}`),
		"/series/121361": json200(seriesJSON),
	}}
	p, _ := provider(t, s)

	got := lookup(t, p, pluginapi.MediaRef{Kind: "show", Title: "Game of Thrones"}).Record
	if got.Name != "Game of Thrones" {
		t.Errorf("name = %q", got.Name)
	}
	// By-name resolution is a /search then a /series fetch (two data calls).
	if len(s.dataReqs) != 2 || s.dataReqs[0] != "/search" || s.dataReqs[1] != "/series/121361" {
		t.Errorf("data reqs = %v, want /search then /series/121361", s.dataReqs)
	}
}

func TestTheTVDBEpisodeByIDResolvesBySeasonNumber(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/series/121361/episodes/default": json200(episodesJSON),
	}}
	p, _ := provider(t, s)

	got := lookup(t, p, pluginapi.MediaRef{
		Kind: "episode", TheTVDBID: "121361", SeasonNumber: 1, EpisodeNumber: 5,
	}).Record
	if got.Name != "The Wolf and the Lion" {
		t.Errorf("name = %q, want the S1E5 title", got.Name)
	}
	if got.Overview != "Ned uncovers the truth." {
		t.Errorf("overview = %q", got.Overview)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://artworks.thetvdb.com/e5.jpg" {
		t.Errorf("artwork = %+v, want the S1E5 still", got.Artwork)
	}
}

func TestTheTVDBEpisodeUnknownNumberIsNoMatch(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/series/121361/episodes/default": json200(episodesJSON),
	}}
	p, _ := provider(t, s)

	resp := lookup(t, p, pluginapi.MediaRef{
		Kind: "episode", TheTVDBID: "121361", SeasonNumber: 9, EpisodeNumber: 99,
	})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match (no such episode)", resp.Outcome)
	}
}

func TestTheTVDBTreatsNAAndEmptyAsEmpty(t *testing.T) {
	// A series that resolves but carries only "N/A"/empty fields contributes
	// nothing — the fill-only supplement reports it as a no-match.
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/series/1": json200(`{"data":{"name":"N/A","overview":"","image":"N/A","genres":[{"name":"N/A"}]}}`),
	}}
	p, _ := provider(t, s)

	resp := lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "1"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match (all fields N/A/empty)", resp.Outcome)
	}
}

func TestTheTVDBUnknownIDIsNoMatch(t *testing.T) {
	// The series path 404s (no handler registered) — TheTVDB's "no record" answer.
	s := &stub{handlers: map[string]http.HandlerFunc{}}
	p, _ := provider(t, s)

	resp := lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "404"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match for a 404", resp.Outcome)
	}
}

func TestTheTVDBSearchNoResultsIsNoMatch(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/search": json200(`{"data":[]}`),
	}}
	p, _ := provider(t, s)

	resp := lookup(t, p, pluginapi.MediaRef{Kind: "show", Title: "Nope"})
	if resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Errorf("outcome = %q, want no-match (search empty)", resp.Outcome)
	}
}

// A 500 was "a real (non-ErrNoMatch) error" for the Go provider, and the pass
// above read enrich.ErrTransient off it to retry the item. The same decision now
// travels as the OUTCOME: unavailable, with no Go error for the host to count as
// a strike. See failure_test.go for the whole table.
func TestTheTVDBNon2xxIsNotAMatch(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{
		"/series/1": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	}}
	p, _ := provider(t, s)

	resp, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", TheTVDBID: "1"},
	})
	if err != nil {
		t.Fatalf("a 500 became a Go error, which the host counts against the plugin: %v", err)
	}
	if resp.Outcome == pluginapi.OutcomeNoMatch || resp.Outcome == pluginapi.OutcomeMatched {
		t.Errorf("outcome = %q, want unavailable on a 500 (never a claim about the show)", resp.Outcome)
	}
}

func TestTheTVDBNonTVKindIsNoMatch(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{"/series/1": json200(seriesJSON)}}
	p, host := provider(t, s)

	for _, kind := range []string{"movie", "artist", "album", "track"} {
		resp := lookup(t, p, pluginapi.MediaRef{Kind: kind, TheTVDBID: "1", Title: "x"})
		if resp.Outcome != pluginapi.OutcomeNoMatch {
			t.Errorf("kind %q: outcome = %q, want no-match (TheTVDB serves TV only)", kind, resp.Outcome)
		}
	}
	// A non-TV kind must not touch the network at all — not even a login.
	if s.logins != 0 || len(s.dataReqs) != 0 || len(host.Requests()) != 0 {
		t.Errorf("non-TV kind hit the network (logins=%d reqs=%v fetches=%d); want zero",
			s.logins, s.dataReqs, len(host.Requests()))
	}
}

func TestTheTVDBCachesRepeatLookup(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{"/series/121361": json200(seriesJSON)}}
	p, _ := provider(t, s)
	ref := pluginapi.MediaRef{Kind: "show", TheTVDBID: "121361"}

	lookup(t, p, ref)
	lookup(t, p, ref)
	// The instance response cache means the repeat lookup does not re-hit the data
	// endpoint.
	if len(s.dataReqs) != 1 {
		t.Errorf("data endpoint hit %d times, want 1 (repeat served from cache)", len(s.dataReqs))
	}
}

// THE 401 RE-LOGIN, which is the one status this provider reads for itself.
//
// A stand-in that answers 401 ONCE and then 200 must cause exactly one re-login
// and one retried request, and the lookup must succeed. This is instance memory
// doing its job: the token minted on first use went stale, the provider dropped
// it, logged in again and asked again — all inside one call, with the caller
// seeing a record rather than a failure.
func TestTheTVDBA401RefreshesTheTokenAndRetriesOnce(t *testing.T) {
	s := &stub{
		handlers:        map[string]http.HandlerFunc{"/series/121361": json200(seriesJSON)},
		unauthorizeNext: 1, // the first data request is answered 401, whatever it carries
	}
	p, _ := provider(t, s)

	got := lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "121361"}).Record
	if got.Name != "Game of Thrones" {
		t.Fatalf("name = %q, want the record the retry fetched", got.Name)
	}
	// EXACTLY one re-login: the mint on first use, plus the one the 401 forced.
	if s.logins != 2 {
		t.Errorf("logins = %d, want 2 (one mint, one refresh forced by the 401)", s.logins)
	}
	// EXACTLY one retried request: the 401'd one and its retry, and nothing more.
	if len(s.dataReqs) != 2 || s.dataReqs[0] != "/series/121361" || s.dataReqs[1] != "/series/121361" {
		t.Errorf("data reqs = %v, want the same path twice", s.dataReqs)
	}
	// The retry carried the SECOND token, not the stale first one.
	if s.lastToken != "tok-k-2" {
		t.Errorf("retry token = %q, want the freshly minted tok-k-2", s.lastToken)
	}
}

// A 401 that SURVIVES the refresh is the apikey itself being rejected, and that
// is a Go error: asking again with the same key gets the same no, and an operator
// is the only one who can fix it. The provider must not loop.
func TestTheTVDBASecondUnauthorizedIsAGoError(t *testing.T) {
	s := &stub{
		handlers:        map[string]http.HandlerFunc{"/series/121361": json200(seriesJSON)},
		unauthorizeNext: 5, // every data request is 401
	}
	p, _ := provider(t, s)

	_, err := p.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "show", TheTVDBID: "121361"},
	})
	if err == nil {
		t.Fatal("a rejected apikey answered with no error; it must reach the operator")
	}
	if s.logins != 2 || len(s.dataReqs) != 2 {
		t.Errorf("logins=%d reqs=%d, want exactly one refresh and one retry before giving up",
			s.logins, len(s.dataReqs))
	}
}

// The secret and the base URL are read PER CALL, and the token minted from them
// is instance memory: a rotated key does not silently keep using the old token
// forever, because the next 401 mints one from whatever the host now publishes.
func TestTheTVDBReadsItsSettingsOnEveryCall(t *testing.T) {
	s := &stub{
		handlers:        map[string]http.HandlerFunc{"/series/1": json200(seriesJSON)},
		unauthorizeNext: 0,
	}
	p, host := provider(t, s)

	lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "1"})
	if s.lastToken != "tok-k-1" {
		t.Fatalf("token = %q, want one minted from the original key", s.lastToken)
	}

	rotated := settings()
	rotated.Secret = "rotated"
	host.SetSettings(rotated)
	s.unauthorizeNext = 1 // the old token is now stale
	s.handlers["/series/2"] = json200(strings.Replace(seriesJSON, "121361", "2", 1))
	lookup(t, p, pluginapi.MediaRef{Kind: "show", TheTVDBID: "2"})

	if s.lastToken != "tok-rotated-2" {
		t.Errorf("token = %q, want one minted from the rotated key", s.lastToken)
	}
}

// The manifest declares neither capability, so the host never asks — and when
// something does ask anyway, the answer is the host's own "not now" rather than a
// claim that this source has no candidates or no images.
func TestTheTVDBAnswersNeitherSearchNorArtwork(t *testing.T) {
	s := &stub{handlers: map[string]http.HandlerFunc{"/series/1": json200(seriesJSON)}}
	p, host := provider(t, s)
	ctx := context.Background()

	if resp, err := p.Search(ctx, pluginapi.SearchRequest{Kind: "show", Query: "GoT"}); err != nil ||
		resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("search = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if resp, err := p.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
		Ref: pluginapi.MediaRef{Kind: "show", TheTVDBID: "1"}, Role: "poster",
	}); err != nil || resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("artwork candidates = (%q, %v), want unavailable and no error", resp.Outcome, err)
	}
	if got := len(host.Requests()); got != 0 {
		t.Errorf("a declined call still fetched something (%d requests); want zero", got)
	}
}
