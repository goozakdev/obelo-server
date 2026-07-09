package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

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
type HTTPArtworkFetcher struct {
	HTTPClient *http.Client
	// MaxBytes caps a downloaded image; 0 uses defaultMaxArtworkBytes.
	MaxBytes int64
}

const defaultMaxArtworkBytes = 16 << 20 // 16 MiB — generous for a poster/backdrop.

// Fetch downloads the image at url, returning its bytes and content-type.
func (f HTTPArtworkFetcher) Fetch(ctx context.Context, url string) ([]byte, string, error) {
	client := f.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	max := f.MaxBytes
	if max <= 0 {
		max = defaultMaxArtworkBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("enrich: building artwork request: %w", err)
	}
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
