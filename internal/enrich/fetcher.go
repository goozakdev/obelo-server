package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
)

// providerClient is the one place the JSON metadata clients (tmdb.go, musicbrainz.go,
// fanarttv.go, theaudiodb.go, anidb.go, omdb.go, thetvdb.go) turn their injectable
// HTTPClient field into the client they actually use, so all seven get the shared
// redirect policy and none of them can quietly opt out.
//
// Those clients only PARSE what comes back, so a hop inward leaks no body — the
// reason they are covered anyway is that "connected" and "did not connect" are
// distinguishable by timing and by which error surfaces, which is enough to map an
// internal network blindly from a provider's Location header. It costs nothing:
// only redirect TARGETS are checked, so an operator's own mirror is still a
// perfectly good base URL (ADR-0001), and none of these APIs redirects off-host in
// normal operation.
//
// Guard copies rather than mutating, which matters here because the nil case used
// to be http.DefaultClient — a process-global whose redirect behaviour is not ours
// to change.
func providerClient(c *http.Client) *http.Client { return safefetch.Guard(c) }

// ErrArtworkNotFound is the benign "the source has no image at this URL" outcome
// (an HTTP 404) — e.g. a Cover Art Archive release-group with no cover. It is
// distinct from a real fetch failure so callers can skip it quietly instead of
// logging it as an error (graceful degradation, ADR-0001).
var ErrArtworkNotFound = errors.New("enrich: artwork not found")

// HTTPArtworkFetcher is the production ArtworkFetcher: a guarded HTTP GET for an
// image URL the provider returned. It bounds the response size and verifies the
// content-type is an image, so a redirect to an HTML error page or an oversized
// body can't poison the artwork cache. A failure is non-fatal upstream (the
// metadata still applies; only the image is skipped).
//
// The URL is a THIRD PARTY'S choice — it comes out of a provider's JSON, or out of
// an admin's PUT /titles/{id}/artwork — and so is any redirect off it, which is why
// every fetch here runs under safefetch's redirect policy: a hop onto 127.0.0.1, an
// RFC1918 address or 169.254.169.254 would otherwise put the bytes of a LAN admin
// panel or a cloud metadata endpoint into the artwork cache, and from there into an
// admin's browser.
//
// HTTPClient stays injectable (the tests point it at httptest servers), but it is
// NOT where the policy lives: Fetch applies safefetch.Guard to whatever client it
// was handed. That is the difference between a control and a control-shaped
// comment — app.go constructs this with a nil client today, and a future line
// assigning a bare &http.Client{} must not be able to disarm it silently. Only
// redirect TARGETS are checked, so a stub server on loopback is still fetchable,
// which is what keeps that injection useful.
type HTTPArtworkFetcher struct {
	HTTPClient *http.Client
	// MaxBytes caps a downloaded image; 0 uses defaultMaxArtworkBytes.
	MaxBytes int64
	// UserAgent identifies Obelo on the image download itself; empty uses
	// DefaultUserAgent. The bytes behind a Cover Art Archive cover come from here,
	// not from the MusicBrainz provider's own request, so leaving this unset sent
	// every cover fetch out as Go's "Go-http-client/1.1" — precisely the anonymous
	// agent MusicBrainz throttles hardest — while the manifest request beside it was
	// properly identified.
	UserAgent string
}

const defaultMaxArtworkBytes = 16 << 20 // 16 MiB — generous for a poster/backdrop.

// Fetch downloads the image at url, returning its bytes and content-type.
func (f HTTPArtworkFetcher) Fetch(ctx context.Context, url string) ([]byte, string, error) {
	client := f.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Guard, not assume: this is applied per fetch, to the injected client as much
	// as to the default one, and it copies rather than mutating the caller's.
	client = safefetch.Guard(client)
	max := f.MaxBytes
	if max <= 0 {
		max = defaultMaxArtworkBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("enrich: building artwork request: %w", err)
	}
	ua := f.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("enrich: artwork request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("enrich: artwork %s: %w", url, ErrArtworkNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("enrich: artwork %s: status %d", url, resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("enrich: artwork %s: non-image content-type %q", url, ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", fmt.Errorf("enrich: reading artwork body: %w", err)
	}
	if int64(len(data)) > max {
		return nil, "", fmt.Errorf("enrich: artwork %s exceeds %d bytes", url, max)
	}
	return data, ct, nil
}
