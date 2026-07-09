package enrich

import (
	"bytes"
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

// TheTVDBProvider is the fill-only supplement in the video chain for the TV kinds
// (show/season/episode): it fills the gaps TMDB can leave — an episode's or show's
// canonical Name, an Overview, Genres, and a still/artwork image — from TheTVDB. It
// owns all TheTVDB HTTP/JSON specifics behind the MetadataProvider seam (ADR-0006),
// including TheTVDB's bearer-token login dance, and treats a no-match or network
// failure as a normal outcome the chain degrades over, never identity
// (ADR-0001/0002). An episode Name it contributes is applied by the chain as a
// display-only override, exactly the rule the music chain uses for a sparse track —
// it is NEVER identity and never re-keys watch state.
//
// It resolves by a TheTVDB series id when the caller supplied one (ref.TheTVDBID),
// otherwise by name (a series search whose top hit gives the id), then fetches the
// series/season/episode record. A record with none of the wanted fields — or a
// TheTVDB not-found — is ErrNoMatch; any other non-2xx is a real error the chain
// logs and swallows. Every non-TV kind is ErrNoMatch (the chain routes movies to
// OMDb and everything else straight to TMDB).
//
// TheTVDB's auth is an internal detail: the first outbound call logs in with the
// apikey to mint a bearer token, which is then reused for subsequent calls (and
// refreshed once on a 401). This mirrors the low-seam shape of omdb.go/fanarttv.go
// (per-host throttle + in-process response cache), guarded by a single mutex.
type TheTVDBProvider struct {
	APIKey     string
	BaseURL    string // e.g. https://api4.thetvdb.com/v4
	UserAgent  string
	HTTPClient *http.Client

	// minInterval is the per-host throttle: successive requests are spaced at least
	// this far apart so backfilling a large library doesn't get the client throttled
	// or banned by TheTVDB.
	minInterval time.Duration

	mu    sync.Mutex
	last  time.Time                // time the next request slot was reserved (throttle)
	token string                   // cached bearer token (minted on first use, refreshed on 401)
	cache map[string]thetvdbResult // lookup key -> parsed record (zero value = looked up, no data)
}

// thetvdbResult is the slice of a TheTVDB record this provider consumes: the
// fill-only fields. A zero value means "no usable data" (cached as a negative
// result and reported as ErrNoMatch by Lookup).
type thetvdbResult struct {
	name     string
	overview string
	genres   []string
	imageURL string
}

// empty reports whether the record carries none of the wanted fields — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (r thetvdbResult) empty() bool {
	return r.name == "" && r.overview == "" && len(r.genres) == 0 && r.imageURL == ""
}

// defaultTheTVDBThrottle spaces successive TheTVDB requests so a large-library
// backfill stays a polite trickle rather than a burst.
const defaultTheTVDBThrottle = 250 * time.Millisecond

// defaultTheTVDBBaseURL is TheTVDB's current v4 API host used when the operator
// sets no override.
const defaultTheTVDBBaseURL = "https://api4.thetvdb.com/v4"

// NewTheTVDBProvider builds a provider from config. An empty base URL falls back to
// the public v4 host; a nil HTTP client gets a default with a sane timeout (a slow
// lookup must not hang a pass).
func NewTheTVDBProvider(apiKey, baseURL string) *TheTVDBProvider {
	if baseURL == "" {
		baseURL = defaultTheTVDBBaseURL
	}
	return &TheTVDBProvider{
		APIKey:      apiKey,
		BaseURL:     baseURL,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		minInterval: defaultTheTVDBThrottle,
		cache:       map[string]thetvdbResult{},
	}
}

// Lookup serves the TV kinds (show/season/episode); anything else is ErrNoMatch (no
// outbound call). It resolves by a TheTVDB series id when present and otherwise by
// name, then fetches the record. A record with none of the wanted fields — or a
// TheTVDB not-found — is ErrNoMatch.
func (p *TheTVDBProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	var (
		key   string
		fetch func(context.Context) (thetvdbResult, error)
	)
	switch ref.Kind {
	case "show":
		key = "show:" + p.seriesKey(ref)
		fetch = func(ctx context.Context) (thetvdbResult, error) { return p.showRecord(ctx, ref) }
	case "season":
		key = "season:" + p.seriesKey(ref) + "/" + strconv.Itoa(ref.SeasonNumber)
		fetch = func(ctx context.Context) (thetvdbResult, error) { return p.seasonRecord(ctx, ref) }
	case "episode":
		key = "episode:" + p.seriesKey(ref) + "/" + strconv.Itoa(ref.SeasonNumber) + "/" + strconv.Itoa(ref.EpisodeNumber)
		fetch = func(ctx context.Context) (thetvdbResult, error) { return p.episodeRecord(ctx, ref) }
	default:
		return TitleMetadata{}, ErrNoMatch
	}

	r, err := p.result(ctx, key, fetch)
	if err != nil {
		return TitleMetadata{}, err
	}
	if r.empty() {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{
		Matched:  true,
		Source:   "thetvdb",
		Name:     r.name,
		Overview: r.overview,
		Genres:   r.genres,
	}
	if r.imageURL != "" {
		// TheTVDB serves absolute artwork URLs; emit them as-is under the same
		// "poster" role TMDB uses for TV posters/stills (mergeArtwork fills a role
		// TMDB left empty).
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: r.imageURL})
	}
	return meta, nil
}

// seriesKey is the cache discriminator for the series a ref points at: its TheTVDB
// id when present, else the lowercased title. It keeps by-id and by-name lookups of
// the same show distinct in the cache.
func (p *TheTVDBProvider) seriesKey(ref TitleRef) string {
	if id := strings.TrimSpace(ref.TheTVDBID); id != "" {
		return "id=" + id
	}
	return "name=" + strings.ToLower(strings.TrimSpace(ref.Title))
}

func (p *TheTVDBProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// result resolves a lookup key to a parsed TheTVDB record, serving from the
// in-process response cache when possible (re-enrichment doesn't re-hit the host)
// and otherwise running the kind's fetch. A no-match is cached as the zero value; a
// transient error is not cached.
func (p *TheTVDBProvider) result(ctx context.Context, key string, fetch func(context.Context) (thetvdbResult, error)) (thetvdbResult, error) {
	if r, ok := p.cached(key); ok {
		return r, nil
	}
	r, err := fetch(ctx)
	switch {
	case err == ErrNoMatch:
		p.store(key, thetvdbResult{})
		return thetvdbResult{}, nil
	case err != nil:
		return thetvdbResult{}, err
	}
	p.store(key, r)
	return r, nil
}

// seriesID resolves the TheTVDB series id for a ref: the id it carries, otherwise
// the top hit of a series search by title. A search with no results is ErrNoMatch.
func (p *TheTVDBProvider) seriesID(ctx context.Context, ref TitleRef) (string, error) {
	if id := strings.TrimSpace(ref.TheTVDBID); id != "" {
		return id, nil
	}
	title := strings.TrimSpace(ref.Title)
	if title == "" {
		return "", ErrNoMatch // nothing to resolve a series by
	}
	q := url.Values{}
	q.Set("query", title)
	q.Set("type", "series")
	var out struct {
		Data []struct {
			TVDBID string `json:"tvdb_id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, "/search", q, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 || strings.TrimSpace(out.Data[0].TVDBID) == "" {
		return "", ErrNoMatch
	}
	return strings.TrimSpace(out.Data[0].TVDBID), nil
}

// thetvdbSeries is the subset of a TheTVDB series record this provider consumes.
type thetvdbSeries struct {
	Name     string    `json:"name"`
	Overview string    `json:"overview"`
	Image    string    `json:"image"`
	Genres   []tvdbTag `json:"genres"`
}

type tvdbTag struct {
	Name string `json:"name"`
}

// showRecord fetches the series record for a show ref (title/overview/genres/poster).
func (p *TheTVDBProvider) showRecord(ctx context.Context, ref TitleRef) (thetvdbResult, error) {
	id, err := p.seriesID(ctx, ref)
	if err != nil {
		return thetvdbResult{}, err
	}
	var out struct {
		Data thetvdbSeries `json:"data"`
	}
	if err := p.getJSON(ctx, "/series/"+url.PathEscape(id), nil, &out); err != nil {
		return thetvdbResult{}, err
	}
	return thetvdbResult{
		name:     tvdbField(out.Data.Name),
		overview: tvdbField(out.Data.Overview),
		genres:   tvdbGenres(out.Data.Genres),
		imageURL: tvdbField(out.Data.Image),
	}, nil
}

// seasonRecord fetches the season record under a show (overview/poster).
func (p *TheTVDBProvider) seasonRecord(ctx context.Context, ref TitleRef) (thetvdbResult, error) {
	id, err := p.seriesID(ctx, ref)
	if err != nil {
		return thetvdbResult{}, err
	}
	var out struct {
		Data struct {
			Overview string `json:"overview"`
			Image    string `json:"image"`
		} `json:"data"`
	}
	path := "/series/" + url.PathEscape(id) + "/seasons/" + strconv.Itoa(ref.SeasonNumber)
	if err := p.getJSON(ctx, path, nil, &out); err != nil {
		return thetvdbResult{}, err
	}
	return thetvdbResult{
		overview: tvdbField(out.Data.Overview),
		imageURL: tvdbField(out.Data.Image),
	}, nil
}

// episodeRecord resolves the series then finds the matching episode by season +
// number (name/overview/still). An episode absent from the series is ErrNoMatch.
func (p *TheTVDBProvider) episodeRecord(ctx context.Context, ref TitleRef) (thetvdbResult, error) {
	id, err := p.seriesID(ctx, ref)
	if err != nil {
		return thetvdbResult{}, err
	}
	var out struct {
		Data struct {
			Episodes []struct {
				SeasonNumber int    `json:"seasonNumber"`
				Number       int    `json:"number"`
				Name         string `json:"name"`
				Overview     string `json:"overview"`
				Image        string `json:"image"`
			} `json:"episodes"`
		} `json:"data"`
	}
	path := "/series/" + url.PathEscape(id) + "/episodes/default"
	if err := p.getJSON(ctx, path, nil, &out); err != nil {
		return thetvdbResult{}, err
	}
	for _, e := range out.Data.Episodes {
		if e.SeasonNumber == ref.SeasonNumber && e.Number == ref.EpisodeNumber {
			return thetvdbResult{
				name:     tvdbField(e.Name),
				overview: tvdbField(e.Overview),
				imageURL: tvdbField(e.Image),
			}, nil
		}
	}
	return thetvdbResult{}, ErrNoMatch // no episode with that season/number
}

// tvdbField normalizes a single TheTVDB text field: an absent value can come back
// as the literal "N/A" or empty, both of which must be treated as empty (never
// written over a TMDB value).
func tvdbField(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "N/A") {
		return ""
	}
	return v
}

// tvdbGenres maps a TheTVDB genres block to a slice, dropping empty/"N/A" entries.
func tvdbGenres(genres []tvdbTag) []string {
	var out []string
	for _, g := range genres {
		if n := tvdbField(g.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ensureToken returns a bearer token, minting one via login on first use. A concurrent
// double-login is harmless (both store an equivalent token).
func (p *TheTVDBProvider) ensureToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	tok := p.token
	p.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	tok, err := p.login(ctx)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.token = tok
	p.mu.Unlock()
	return tok, nil
}

// login exchanges the apikey for a bearer token via TheTVDB's /login endpoint. A
// non-2xx (bad key, host down) is a real error the chain logs and swallows.
func (p *TheTVDBProvider) login(ctx context.Context) (string, error) {
	if err := p.throttle(ctx); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"apikey": p.APIKey})
	if err != nil {
		return "", fmt.Errorf("enrich: encoding thetvdb login: %w", err)
	}
	reqURL := strings.TrimRight(p.BaseURL, "/") + "/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("enrich: building thetvdb login request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("enrich: thetvdb login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("enrich: thetvdb login: status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("enrich: decoding thetvdb login: %w", err)
	}
	if strings.TrimSpace(out.Data.Token) == "" {
		return "", fmt.Errorf("enrich: thetvdb login returned no token")
	}
	return out.Data.Token, nil
}

// getJSON issues one authed GET against the TheTVDB base URL and decodes the JSON
// body into out. A 404 — TheTVDB's "no record" answer — is ErrNoMatch; a 401 clears
// the cached token and retries once (a stale/expired token); any other non-2xx is a
// real error the chain logs and swallows.
func (p *TheTVDBProvider) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := p.ensureToken(ctx)
		if err != nil {
			return err
		}
		if err := p.throttle(ctx); err != nil {
			return err
		}
		u := strings.TrimRight(p.BaseURL, "/") + path
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return fmt.Errorf("enrich: building thetvdb request: %w", err)
		}
		req.Header.Set("User-Agent", p.UserAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := p.client().Do(req)
		if err != nil {
			return fmt.Errorf("enrich: thetvdb request: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			p.clearToken(tok) // stale token — drop it and retry with a fresh login
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ErrNoMatch // unknown id — the normal "no record" outcome
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return fmt.Errorf("enrich: thetvdb %s: status %d", path, resp.StatusCode)
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("enrich: decoding thetvdb response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("enrich: thetvdb %s: unauthorized after token refresh", path)
}

// clearToken drops the cached token if it is still the one that just failed, so a
// concurrent successful re-login isn't thrown away.
func (p *TheTVDBProvider) clearToken(stale string) {
	p.mu.Lock()
	if p.token == stale {
		p.token = ""
	}
	p.mu.Unlock()
}

// throttle blocks until the next per-host request slot, reserving it so concurrent
// lookups queue rather than burst. It respects context cancellation.
func (p *TheTVDBProvider) throttle(ctx context.Context) error {
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

func (p *TheTVDBProvider) cached(key string) (thetvdbResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.cache[key]
	return r, ok
}

func (p *TheTVDBProvider) store(key string, r thetvdbResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]thetvdbResult{}
	}
	p.cache[key] = r
}
