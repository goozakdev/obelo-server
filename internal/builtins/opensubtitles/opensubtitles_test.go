package opensubtitles

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// The OpenSubtitles Built-in's HTTP guards and its contract outcomes, against an
// httptest stub — no live network. The interesting one is the second step of the
// download: the `link` is a string out of the PROVIDER'S JSON, GET'd server-side
// with the bytes written to disk beside the operator's media, so where that link
// (or a redirect off it) points is a third party's choice, not the operator's.

const sampleSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello world\n"

// osStub is a minimal OpenSubtitles API: POST /download hands back a link on this
// same host, the link either serves the subtitle or redirects, and GET /subtitles
// answers with whatever candidate document the test set.
type osStub struct {
	*httptest.Server
	linkPath string // what POST /download points the caller at
	search   string // the /subtitles response body
	hits     []string
}

func newOSStub(t *testing.T, linkPath string) *osStub {
	t.Helper()
	s := &osStub{linkPath: linkPath, search: `{"data":[]}`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits = append(s.hits, r.URL.Path)
		switch r.URL.Path {
		case "/subtitles":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(s.search))
		case "/download":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"link":"` + s.Server.URL + s.linkPath + `","file_name":"movie.srt"}`))
		case "/files/movie.srt":
			_, _ = w.Write([]byte(sampleSRT))
		case "/files/redirect-inward.srt":
			// The link answers with a redirect — pointed back at this loopback host,
			// which is the hop that must be refused. A real hostile provider would
			// aim it at 127.0.0.1:<obelo>, a LAN NAS, or 169.254.169.254.
			http.Redirect(w, r, s.Server.URL+"/files/movie.srt", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func osProvider(s *osStub) *Provider {
	return NewProvider("test-key", s.Server.URL)
}

func downloadOne(p *Provider, c pluginapi.SubtitleCandidate) (pluginapi.SubtitleDownloadResponse, error) {
	return p.DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: c, MaxBytes: 8 << 20,
	})
}

// TestOpenSubtitlesDownloadFollowsItsLink is the control for the test below: the
// ordinary two-step download still works against a stub on loopback. Only redirect
// TARGETS are checked — the base URL itself is the operator's choice and may
// legitimately be a host on their own LAN (ADR-0001).
func TestOpenSubtitlesDownloadFollowsItsLink(t *testing.T) {
	s := newOSStub(t, "/files/movie.srt")

	resp, err := downloadOne(osProvider(s), pluginapi.SubtitleCandidate{ID: "42", Format: "srt"})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched {
		t.Fatalf("outcome = %q, want %q", resp.Outcome, pluginapi.OutcomeMatched)
	}
	if string(resp.Data) != sampleSRT || resp.Format != "srt" {
		t.Fatalf("got %q/%q, want the sample srt", resp.Data, resp.Format)
	}
}

// TestOpenSubtitlesDownloadRefusesInwardRedirect: the provider's link redirects to
// a loopback address and the download must fail rather than write whatever answers
// there into the subtitle cache.
func TestOpenSubtitlesDownloadRefusesInwardRedirect(t *testing.T) {
	s := newOSStub(t, "/files/redirect-inward.srt")

	resp, err := downloadOne(osProvider(s), pluginapi.SubtitleCandidate{ID: "42", Format: "srt"})
	if err == nil {
		t.Fatalf("a redirect onto loopback was followed; got %d bytes", len(resp.Data))
	}
	if !errors.Is(err, safefetch.ErrRedirectBlocked) {
		t.Fatalf("got %v, want it to wrap safefetch.ErrRedirectBlocked", err)
	}
	for _, h := range s.hits {
		if h == "/files/movie.srt" {
			t.Fatal("the redirect target was requested — the hop was followed, not refused")
		}
	}
}

// TestOpenSubtitlesDownloadHonorsTheCallersCap: the caller states the size cap and
// the Plugin refuses rather than returning more (ADR-0057 decision 2).
func TestOpenSubtitlesDownloadHonorsTheCallersCap(t *testing.T) {
	s := newOSStub(t, "/files/movie.srt")

	_, err := osProvider(s).DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{ID: "42", Format: "srt"},
		MaxBytes:  4,
	})
	if err == nil {
		t.Fatal("a subtitle larger than the caller's cap was returned")
	}
}

// TestOpenSubtitlesSearchOutcomes: a source with nothing for this release answers
// no-match — an outcome, not an error — and a hit answers matched, tagged with the
// narrowing that produced it (ADR-0021's hash-first match order).
func TestOpenSubtitlesSearchOutcomes(t *testing.T) {
	s := newOSStub(t, "/files/movie.srt")
	p := osProvider(s)
	ref := pluginapi.SubtitleRef{Title: "Dune", Year: 2021, MovieHash: "8e245d9679d31e12"}

	resp, err := p.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{Ref: ref, Language: "de"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeNoMatch || len(resp.Candidates) != 0 {
		t.Fatalf("empty source = (%q, %d candidates), want no-match and none", resp.Outcome, len(resp.Candidates))
	}

	s.search = `{"data":[{"attributes":{"language":"de","download_count":7,"release":"Dune.2021.1080p",` +
		`"files":[{"file_id":42,"file_name":"dune.srt"}]}}]}`
	resp, err = p.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{Ref: ref, Language: "de"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 1 {
		t.Fatalf("hit = (%q, %d candidates), want matched and one", resp.Outcome, len(resp.Candidates))
	}
	got := resp.Candidates[0]
	if got.ID != "42" || got.Language != "de" || got.Format != "srt" || got.MatchedBy != "moviehash" {
		t.Fatalf("candidate = %+v, want the file id, language, format and the moviehash narrowing", got)
	}

	// An unrecognized language is nothing to find, and costs no call.
	before := len(s.hits)
	resp, err = p.SearchSubtitles(context.Background(), pluginapi.SubtitleSearchRequest{Ref: ref, Language: "zzz"})
	if err != nil || resp.Outcome != pluginapi.OutcomeNoMatch {
		t.Fatalf("unknown language = (%q, %v), want no-match and no error", resp.Outcome, err)
	}
	if len(s.hits) != before {
		t.Fatal("an unrecognized language reached the network")
	}
}

// TestOpenSubtitlesClientDoesNotMutateAnInjectedClient: the policy is applied to a
// copy. The nil case used to hand back http.DefaultClient, and mutating that would
// have changed the redirect behaviour of every unrelated call in the process.
func TestOpenSubtitlesClientDoesNotMutateAnInjectedClient(t *testing.T) {
	injected := &http.Client{}
	p := &Provider{HTTPClient: injected}

	if got := p.client(); got.CheckRedirect == nil {
		t.Fatal("an injected client is used without the redirect policy")
	}
	if injected.CheckRedirect != nil {
		t.Error("client() mutated the injected client")
	}
	if (&Provider{}).client().CheckRedirect == nil {
		t.Error("the nil-client fallback carries no redirect policy")
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Error("http.DefaultClient was mutated")
	}
}

// TestNewBuildsFromSettings: the factory the registration carries reads the fixed
// Settings shape — Secret is the key, URL the effective base URL, and an empty URL
// falls back to the public host.
func TestNewBuildsFromSettings(t *testing.T) {
	p, err := New(pluginapi.Settings{Enabled: true, Secret: "k", URL: "https://mirror.test/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	prov, ok := p.(*Provider)
	if !ok {
		t.Fatalf("New returned %T, want *Provider", p)
	}
	if prov.APIKey != "k" || prov.BaseURL != "https://mirror.test/v1" {
		t.Fatalf("settings not carried: %+v", prov)
	}
	p, _ = New(pluginapi.Settings{Enabled: true, Secret: "k"})
	if prov := p.(*Provider); prov.BaseURL != DefaultBaseURL {
		t.Fatalf("base URL = %q, want the public host", prov.BaseURL)
	}
}
