package subfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// The OpenSubtitles provider's HTTP guards, against an httptest stub — no live
// network. The interesting one is the second step of the download: the `link` is a
// string out of the PROVIDER'S JSON, GET'd server-side with the bytes written to
// disk beside the operator's media, so where that link (or a redirect off it) points
// is a third party's choice, not the operator's.

// osStub is a minimal OpenSubtitles API: POST /download hands back a link on this
// same host, and the link either serves the subtitle or redirects.
type osStub struct {
	*httptest.Server
	linkPath string // what POST /download points the caller at
	hits     []string
}

func newOSStub(t *testing.T, linkPath string) *osStub {
	t.Helper()
	s := &osStub{linkPath: linkPath}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits = append(s.hits, r.URL.Path)
		switch r.URL.Path {
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

func osProvider(s *osStub) *OpenSubtitlesProvider {
	p := NewOpenSubtitlesProvider("test-key", s.Server.URL)
	return p
}

// TestOpenSubtitlesDownloadFollowsItsLink is the control for the test below: the
// ordinary two-step download still works against a stub on loopback. Only redirect
// TARGETS are checked — the base URL itself is the operator's choice and may
// legitimately be a host on their own LAN (ADR-0001).
func TestOpenSubtitlesDownloadFollowsItsLink(t *testing.T) {
	s := newOSStub(t, "/files/movie.srt")

	data, format, err := osProvider(s).Download(context.Background(), Candidate{ID: "42", Format: "srt"})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(data) != sampleSRT || format != "srt" {
		t.Fatalf("got %q/%q, want the sample srt", data, format)
	}
}

// TestOpenSubtitlesDownloadRefusesInwardRedirect: the provider's link redirects to
// a loopback address and the download must fail rather than write whatever answers
// there into the subtitle cache.
func TestOpenSubtitlesDownloadRefusesInwardRedirect(t *testing.T) {
	s := newOSStub(t, "/files/redirect-inward.srt")

	data, _, err := osProvider(s).Download(context.Background(), Candidate{ID: "42", Format: "srt"})
	if err == nil {
		t.Fatalf("a redirect onto loopback was followed; got %d bytes", len(data))
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

// TestOpenSubtitlesClientDoesNotMutateAnInjectedClient: the policy is applied to a
// copy. The nil case used to hand back http.DefaultClient, and mutating that would
// have changed the redirect behaviour of every unrelated call in the process.
func TestOpenSubtitlesClientDoesNotMutateAnInjectedClient(t *testing.T) {
	injected := &http.Client{}
	p := &OpenSubtitlesProvider{HTTPClient: injected}

	if got := p.client(); got.CheckRedirect == nil {
		t.Fatal("an injected client is used without the redirect policy")
	}
	if injected.CheckRedirect != nil {
		t.Error("client() mutated the injected client")
	}
	if (&OpenSubtitlesProvider{}).client().CheckRedirect == nil {
		t.Error("the nil-client fallback carries no redirect policy")
	}
	if http.DefaultClient.CheckRedirect != nil {
		t.Error("http.DefaultClient was mutated")
	}
}
