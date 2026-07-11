package enrich

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AniDBProvider is the anime-specialist Full video provider (ADR-0027): an
// enrichment-only source you can lead an anime Library with (its Authoritative
// provider) so anime titles are described by a source that understands them, with
// TMDB (and other enabled supplements) filling the gaps. It owns all AniDB
// HTTP-API/XML specifics behind the MetadataProvider seam (ADR-0006).
//
// It NEVER affects identity or Watch state (ADR-0002/0014): it only decorates, and
// its ExternalID is the resolved AniDB anime id, nothing more. AniDB ids are NOT
// naming-convention-derived, so this provider resolves BY a pinned anime id
// (TitleRef.AniDBID — set by a Fix-info Enrichment override); a name-based match
// against the AniDB titles dump is a deliberate future concern (PRD "Out of Scope"),
// so a lookup with no anime id is a normal ErrNoMatch the chain degrades over.
//
// The AniDB HTTP API is keyed by a registered CLIENT NAME (not a secret token); we
// carry it as the provider's Client credential (RequiresKey in the registry), so
// AniDB is selectable as an Authoritative provider only once it is configured.
type AniDBProvider struct {
	Client     string // registered AniDB HTTP-API client name (the "key")
	BaseURL    string // e.g. http://api.anidb.net:9001/httpapi
	Language   string // preferred title language (matches AniDB's xml:lang)
	UserAgent  string
	HTTPClient *http.Client

	// minInterval throttles the AniDB host: AniDB is strict about request rate, so
	// a large-library pass is spaced to a polite trickle to avoid a client ban.
	minInterval time.Duration

	mu    sync.Mutex
	last  time.Time              // reserved next-request slot (throttle)
	cache map[string]anidbResult // aid -> parsed record (zero value = looked up, no data)
}

// anidbImageBaseURL is the public AniDB cover-art host; a <picture> filename in the
// anime record is resolved against it into a poster URL.
const anidbImageBaseURL = "https://cdn.anidb.net/images/main"

// anidbClientVer is the HTTP-API client version we advertise (protover 1). AniDB
// requires client + clientver + protover on every request.
const anidbClientVer = "1"

// defaultAniDBThrottle spaces successive AniDB requests (AniDB bans bursty clients).
const defaultAniDBThrottle = 2 * time.Second

// anidbResult is the slice of an AniDB anime record this provider consumes. A zero
// value means "no usable data" (cached negative; reported as ErrNoMatch).
type anidbResult struct {
	title    string
	overview string
	year     int
	genres   []string
	poster   string // absolute cover-art URL, "" when none
}

func (r anidbResult) empty() bool {
	return r.title == "" && r.overview == "" && r.poster == "" && len(r.genres) == 0
}

// NewAniDBProvider builds a provider from config. An empty base URL falls back to
// the public AniDB HTTP API; a nil HTTP client gets a default with a sane timeout.
func NewAniDBProvider(client, baseURL, language string) *AniDBProvider {
	if baseURL == "" {
		baseURL = registryAniDBBaseURL
	}
	return &AniDBProvider{
		Client:      client,
		BaseURL:     baseURL,
		Language:    language,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 20 * time.Second},
		minInterval: defaultAniDBThrottle,
		cache:       map[string]anidbResult{},
	}
}

// Lookup resolves a video Title BY its pinned AniDB anime id and returns the
// enrichment-only descriptive fields. A non-video kind, or a ref with no AniDB id,
// is ErrNoMatch (the honest outcome until name-based matching exists) — so an
// AniDB-led chain leaves an unpinned Title unmatched rather than guessing, never
// touching identity. A record with none of the wanted fields is likewise ErrNoMatch.
func (p *AniDBProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "show", "season", "episode":
	default:
		return TitleMetadata{}, ErrNoMatch // AniDB serves the video kinds only
	}
	aid := strings.TrimSpace(ref.AniDBID)
	if aid == "" {
		return TitleMetadata{}, ErrNoMatch // AniDB ids are not naming-derived
	}
	r, err := p.result(ctx, aid)
	if err != nil {
		return TitleMetadata{}, err
	}
	if r.empty() {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{
		Matched:    true,
		Source:     "anidb",
		Name:       r.title,
		Year:       r.year,
		Overview:   r.overview,
		Genres:     r.genres,
		ExternalID: aid,
	}
	if r.poster != "" {
		meta.Artwork = []ArtworkRef{{Role: "poster", URL: r.poster}}
	}
	return meta, nil
}

// Search returns no candidates: AniDB's HTTP API offers no free-text title search
// (matching would need the offline titles dump — out of scope), so the Edit-item
// picker for an AniDB-led kind simply finds none rather than hanging. It is not an
// error; the caller falls back to a pasted-id override.
func (p *AniDBProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	return nil, nil
}

// ArtworkCandidates lists the anime's cover art (one poster) for the "poster" role
// when the ref carries a resolved anime id; every other role/kind is (nil, nil).
func (p *AniDBProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if role != "poster" || strings.TrimSpace(ref.AniDBID) == "" {
		return nil, nil
	}
	r, err := p.result(ctx, strings.TrimSpace(ref.AniDBID))
	if err != nil {
		return nil, err
	}
	if r.poster == "" {
		return nil, nil
	}
	return []ArtworkCandidate{{URL: r.poster, Source: "anidb"}}, nil
}

// result resolves an anime id to a parsed record, serving from the in-process cache
// (re-enrichment doesn't re-hit AniDB) and otherwise throttling before the request.
// A no-match is cached as the zero value; a transient error is not cached.
func (p *AniDBProvider) result(ctx context.Context, aid string) (anidbResult, error) {
	if r, ok := p.cached(aid); ok {
		return r, nil
	}
	if err := p.throttle(ctx); err != nil {
		return anidbResult{}, err
	}
	r, err := p.fetch(ctx, aid)
	switch {
	case err == ErrNoMatch:
		p.store(aid, anidbResult{})
		return anidbResult{}, nil
	case err != nil:
		return anidbResult{}, err
	}
	p.store(aid, r)
	return r, nil
}

// anidbAnime is the slice of the AniDB HTTP-API XML this provider parses. AniDB
// returns an <anime> root on success and an <error> root for an unknown aid or a
// banned/invalid client, so the struct pins NO root name (XMLName captures which it
// was) and RootText collects the root's character data — the error message when the
// root is <error>.
type anidbAnime struct {
	XMLName  xml.Name
	RootText string `xml:",chardata"`
	Titles   []struct {
		Lang string `xml:"lang,attr"`
		Type string `xml:"type,attr"`
		Text string `xml:",chardata"`
	} `xml:"titles>title"`
	Description string `xml:"description"`
	Picture     string `xml:"picture"`
	StartDate   string `xml:"startdate"`
	Tags        []struct {
		Name string `xml:"name"`
	} `xml:"tags>tag"`
}

// fetch issues one AniDB HTTP-API request for an anime id and parses the record.
// AniDB's <error> payload (unknown aid / banned client) is ErrNoMatch for an
// unknown-aid message and a real error otherwise; any non-2xx is a real error the
// chain logs and swallows.
func (p *AniDBProvider) fetch(ctx context.Context, aid string) (anidbResult, error) {
	q := url.Values{}
	q.Set("request", "anime")
	q.Set("client", p.Client)
	q.Set("clientver", anidbClientVer)
	q.Set("protover", "1")
	q.Set("aid", aid)
	reqURL := strings.TrimRight(p.BaseURL, "/") + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return anidbResult{}, fmt.Errorf("enrich: building anidb request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	resp, err := p.client().Do(req)
	if err != nil {
		return anidbResult{}, fmt.Errorf("enrich: anidb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return anidbResult{}, fmt.Errorf("enrich: anidb: status %d", resp.StatusCode)
	}
	var a anidbAnime
	if err := xml.NewDecoder(resp.Body).Decode(&a); err != nil {
		return anidbResult{}, fmt.Errorf("enrich: decoding anidb response: %w", err)
	}
	if a.XMLName.Local == "error" {
		msg := strings.TrimSpace(a.RootText)
		if strings.Contains(strings.ToLower(msg), "unknown") {
			return anidbResult{}, ErrNoMatch // no anime with that id
		}
		return anidbResult{}, fmt.Errorf("enrich: anidb: %s", msg)
	}
	return p.toResult(a), nil
}

// toResult normalizes a parsed anime record into the fields we consume, preferring
// the operator's language for the display title with sensible fallbacks (the
// English "main" title, then any title).
func (p *AniDBProvider) toResult(a anidbAnime) anidbResult {
	r := anidbResult{
		overview: strings.TrimSpace(a.Description),
	}
	// Title preference: the configured language's "main"/"official", then any "main",
	// then the first title present.
	lang := strings.ToLower(strings.SplitN(p.Language, "-", 2)[0])
	var mainTitle, anyTitle string
	for _, t := range a.Titles {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if anyTitle == "" {
			anyTitle = text
		}
		if t.Type == "main" && mainTitle == "" {
			mainTitle = text
		}
		if lang != "" && strings.EqualFold(t.Lang, lang) && (t.Type == "main" || t.Type == "official") {
			r.title = text
		}
	}
	if r.title == "" {
		if mainTitle != "" {
			r.title = mainTitle
		} else {
			r.title = anyTitle
		}
	}
	if len(a.StartDate) >= 4 {
		if y := parseYear(a.StartDate[:4]); y > 0 {
			r.year = y
		}
	}
	for _, t := range a.Tags {
		if name := strings.TrimSpace(t.Name); name != "" {
			r.genres = append(r.genres, name)
		}
	}
	if pic := strings.TrimSpace(a.Picture); pic != "" {
		r.poster = anidbImageBaseURL + "/" + pic
	}
	return r
}

// parseYear parses a 4-digit year, returning 0 when it is not a plausible year.
func parseYear(s string) int {
	y := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		y = y*10 + int(c-'0')
	}
	if y < 1900 || y > 2200 {
		return 0
	}
	return y
}

func (p *AniDBProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// throttle blocks until the next per-host request slot, reserving it so concurrent
// lookups queue rather than burst. Respects context cancellation.
func (p *AniDBProvider) throttle(ctx context.Context) error {
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

func (p *AniDBProvider) cached(aid string) (anidbResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.cache[aid]
	return r, ok
}

func (p *AniDBProvider) store(aid string, r anidbResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]anidbResult{}
	}
	p.cache[aid] = r
}
