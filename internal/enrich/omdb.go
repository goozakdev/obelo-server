package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OMDbProvider is the first fill-only supplement in the video chain. It serves the
// Movie kind only: it fills the gaps TMDB can leave — a plot (Overview), a content
// rating (Rated), and genres — from the Open Movie Database. It owns all OMDb
// HTTP/JSON specifics behind the MetadataProvider seam (ADR-0006) and treats a
// no-match or network failure as a normal outcome the chain degrades over, never
// identity (ADR-0001/0002).
//
// It resolves by IMDb id (i=tt...) when the caller supplied one (the video chain
// feeds it the parsed IMDBID), otherwise by title+year (t=/y=). OMDb's
// {"Response":"False"} — or a record with none of the wanted fields — is a
// no-match; any non-2xx is a real error the chain logs and swallows. OMDb has no
// artwork host, so it returns only text fields; every other kind is ErrNoMatch
// (the chain routes non-movie video kinds straight to TMDB).
type OMDbProvider struct {
	APIKey     string
	BaseURL    string // e.g. https://www.omdbapi.com
	UserAgent  string
	HTTPClient *http.Client

	// minInterval is the per-host throttle: successive requests are spaced at
	// least this far apart so backfilling a large library doesn't get the client
	// throttled or banned by OMDb.
	minInterval time.Duration

	mu    sync.Mutex
	last  time.Time             // time the next request slot was reserved (throttle)
	cache map[string]omdbResult // lookup key -> parsed record (zero value = looked up, no data)
}

// omdbResult is the slice of an OMDb record this provider consumes: the three
// fill-only text fields. A zero value means "no usable data" (cached as a negative
// result and reported as ErrNoMatch by Lookup).
type omdbResult struct {
	overview      string
	contentRating string
	genres        []string
}

// empty reports whether the record carries none of the wanted fields — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (r omdbResult) empty() bool {
	return r.overview == "" && r.contentRating == "" && len(r.genres) == 0
}

// defaultOMDbThrottle spaces successive OMDb requests so a large-library backfill
// stays a polite trickle rather than a burst.
const defaultOMDbThrottle = 250 * time.Millisecond

// defaultOMDbBaseURL is the public OMDb endpoint used when the operator sets no
// override.
const defaultOMDbBaseURL = "https://www.omdbapi.com"

// NewOMDbProvider builds a provider from config. An empty base URL falls back to
// the public host; a nil HTTP client gets a default with a sane timeout (a slow
// lookup must not hang a pass).
func NewOMDbProvider(apiKey, baseURL string) *OMDbProvider {
	if baseURL == "" {
		baseURL = defaultOMDbBaseURL
	}
	return &OMDbProvider{
		APIKey:      apiKey,
		BaseURL:     baseURL,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		minInterval: defaultOMDbThrottle,
		cache:       map[string]omdbResult{},
	}
}

// Lookup serves the Movie kind (plot/rating/genres); anything else is ErrNoMatch
// (the chain routes other kinds straight to TMDB). It resolves by IMDb id when
// present and otherwise by title+year; a record with none of the wanted fields —
// or an OMDb no-match — is ErrNoMatch.
func (p *OMDbProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind != "movie" {
		return TitleMetadata{}, ErrNoMatch
	}
	imdb := strings.TrimSpace(ref.IMDBID)
	title := strings.TrimSpace(ref.Title)

	q := url.Values{}
	q.Set("apikey", p.APIKey)
	var key string
	switch {
	case imdb != "":
		key = "i:" + imdb
		q.Set("i", imdb)
	case title != "":
		key = "t:" + strings.ToLower(title)
		q.Set("t", title)
		if ref.Year > 0 {
			key += "/" + strconv.Itoa(ref.Year)
			q.Set("y", strconv.Itoa(ref.Year))
		}
	default:
		return TitleMetadata{}, ErrNoMatch // nothing to key a lookup by
	}

	r, err := p.result(ctx, key, q)
	if err != nil {
		return TitleMetadata{}, err
	}
	if r.empty() {
		return TitleMetadata{}, ErrNoMatch
	}
	return TitleMetadata{
		Matched:       true,
		Source:        "omdb",
		Overview:      r.overview,
		ContentRating: r.contentRating,
		Genres:        r.genres,
	}, nil
}

func (p *OMDbProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// result resolves a lookup key to a parsed OMDb record, returning a zero value
// when OMDb has no record. It serves from the in-process response cache when
// possible (re-enrichment doesn't re-hit the host) and otherwise throttles before
// issuing the request. A no-match is cached as the zero value; a transient error
// is not cached.
func (p *OMDbProvider) result(ctx context.Context, key string, q url.Values) (omdbResult, error) {
	if r, ok := p.cached(key); ok {
		return r, nil
	}
	if err := p.throttle(ctx); err != nil {
		return omdbResult{}, err
	}
	r, err := p.fetch(ctx, q)
	switch {
	case err == ErrNoMatch:
		p.store(key, omdbResult{})
		return omdbResult{}, nil
	case err != nil:
		return omdbResult{}, err
	}
	p.store(key, r)
	return r, nil
}

// fetch issues one OMDb request and parses the fill-only fields. OMDb's
// {"Response":"False"} — its "no record" answer — is ErrNoMatch; any non-2xx is a
// real error the chain logs and swallows. "N/A" field values are treated as empty.
func (p *OMDbProvider) fetch(ctx context.Context, q url.Values) (omdbResult, error) {
	reqURL := strings.TrimRight(p.BaseURL, "/") + "/?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return omdbResult{}, fmt.Errorf("enrich: building omdb request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return omdbResult{}, fmt.Errorf("enrich: omdb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return omdbResult{}, fmt.Errorf("enrich: omdb: status %d", resp.StatusCode)
	}
	var out struct {
		Response string `json:"Response"`
		Plot     string `json:"Plot"`
		Rated    string `json:"Rated"`
		Genre    string `json:"Genre"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return omdbResult{}, fmt.Errorf("enrich: decoding omdb response: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(out.Response), "true") {
		return omdbResult{}, ErrNoMatch // {"Response":"False"} — no record
	}
	return omdbResult{
		overview:      omdbField(out.Plot),
		contentRating: omdbField(out.Rated),
		genres:        omdbGenres(out.Genre),
	}, nil
}

// omdbField normalizes a single OMDb text field: OMDb encodes an absent value as
// the literal "N/A", which must be treated as empty (never written).
func omdbField(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "N/A") {
		return ""
	}
	return v
}

// omdbGenres splits OMDb's comma-separated Genre field into a slice, dropping
// empty and "N/A" entries.
func omdbGenres(genre string) []string {
	var out []string
	for _, g := range strings.Split(genre, ",") {
		if g = omdbField(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// throttle blocks until the next per-host request slot, reserving it so
// concurrent lookups queue rather than burst. It respects context cancellation.
func (p *OMDbProvider) throttle(ctx context.Context) error {
	if p.minInterval <= 0 {
		return nil
	}
	p.mu.Lock()
	now := time.Now()
	slot := p.last.Add(p.minInterval)
	if slot.Before(now) {
		slot = now
	}
	p.last = slot
	p.mu.Unlock()

	wait := time.Until(slot)
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *OMDbProvider) cached(key string) (omdbResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.cache[key]
	return r, ok
}

func (p *OMDbProvider) store(key string, r omdbResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]omdbResult{}
	}
	p.cache[key] = r
}
