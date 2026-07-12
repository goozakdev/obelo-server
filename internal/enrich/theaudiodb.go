package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TheAudioDBProvider is the second, broader source in the Music chain. For an
// artist it covers the two gaps fanart.tv leaves: (1) an artist image even when
// MusicBrainz produced no MBID — TheAudioDB also matches by NAME — and (2) a real
// biography, which neither MusicBrainz (it synthesizes a stub) nor fanart.tv
// (image-only) offers. For a track it covers MusicBrainz's other documented gap, a
// synopsis: MusicBrainz returns only a canonical title, so a track's Overview is
// always empty without this source. It owns all TheAudioDB HTTP/JSON specifics
// behind the MetadataProvider seam (ADR-0006) and treats a no-match or network
// failure as a normal outcome the chain degrades over, never identity
// (ADR-0001/0002).
//
// It resolves an artist by MBID when the caller supplied one (artist-mb.php) and
// otherwise by name (search.php), parsing strArtistThumb into ArtworkRef{Role:
// "poster"}, strArtistFanart* into "background", and strArtistLogo into "logo" (the
// fallback source behind fanart.tv for the same three artist roles) and the
// language-matched strBiography* into Overview.
// It resolves a track by recording MBID (track-mb.php) and otherwise by
// artist+name (searchtrack.php), parsing the language-matched strDescription* into
// Overview (the synopsis only — no track artwork).
type TheAudioDBProvider struct {
	APIKey     string
	BaseURL    string // e.g. https://www.theaudiodb.com/api/v1/json
	Language   string // selects the strBiography* field (e.g. en-US -> strBiographyEN)
	UserAgent  string // TheAudioDB asks clients to identify themselves
	HTTPClient *http.Client

	// minInterval is the per-host throttle: successive requests are spaced at
	// least this far apart so backfilling a large library doesn't get the client
	// throttled or banned by TheAudioDB.
	minInterval time.Duration

	mu         sync.Mutex
	last       time.Time                // time the next request slot was reserved (throttle)
	cache      map[string]audiodbArtist // artist lookup key -> parsed artist (zero value = looked up, no data)
	trackCache map[string]string        // track lookup key -> synopsis ("" = looked up, no data)
}

// audiodbArtist is the slice of a TheAudioDB artist record this provider needs:
// the thumbnail (poster), background(s), logo, and the chosen-language biography. A
// zero value means "no usable data" (cached as a negative result). backgrounds
// holds the non-empty strArtistFanart/2/3/4 in order (the first is the best-of the
// Lookup emits; the full list feeds the Background picker grid).
type audiodbArtist struct {
	thumb       string
	backgrounds []string
	logo        string
	bio         string
}

// defaultTheAudioDBThrottle spaces successive TheAudioDB requests so a large-
// library backfill stays a polite trickle rather than a burst.
const defaultTheAudioDBThrottle = 250 * time.Millisecond

// NewTheAudioDBProvider builds a provider from config. A nil HTTP client gets a
// default with a sane timeout (a slow lookup must not hang a pass).
func NewTheAudioDBProvider(apiKey, baseURL, language string) *TheAudioDBProvider {
	return &TheAudioDBProvider{
		APIKey:      apiKey,
		BaseURL:     baseURL,
		Language:    language,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		minInterval: defaultTheAudioDBThrottle,
		cache:       map[string]audiodbArtist{},
		trackCache:  map[string]string{},
	}
}

// Lookup serves the artist kind (image + biography) and the track kind (synopsis);
// anything else is ErrNoMatch (the chain routes other kinds straight to
// MusicBrainz).
func (p *TheAudioDBProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "artist":
		return p.lookupArtist(ctx, ref)
	case "track":
		return p.lookupTrack(ctx, ref)
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

// lookupArtist returns the TheAudioDB artist image (poster) and biography
// (Overview), keyed by MBID when present and otherwise by name; an artist with
// neither image nor bio is ErrNoMatch.
func (p *TheAudioDBProvider) lookupArtist(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	key, reqURL, ok := p.artistRequest(ref)
	if !ok {
		return TitleMetadata{}, ErrNoMatch // nothing to key a lookup by
	}
	a, err := p.artist(ctx, key, reqURL)
	if err != nil {
		return TitleMetadata{}, err
	}
	if a.thumb == "" && len(a.backgrounds) == 0 && a.logo == "" && a.bio == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{Matched: true, Source: "theaudiodb"}
	if a.thumb != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: a.thumb})
	}
	if len(a.backgrounds) > 0 {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "background", URL: a.backgrounds[0]})
	}
	if a.logo != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "logo", URL: a.logo})
	}
	if a.bio != "" {
		meta.Overview = a.bio
	}
	return meta, nil
}

// ArtworkCandidates lists TheAudioDB's images for an artist role, backing the
// Edit-item picker (artwork-management/02) as the fallback source behind fanart.tv.
// The role selects the set: "background" → the strArtistFanart* list, "logo" → the
// single strArtistLogo, "poster" (or anything else) → the single strArtistThumb —
// the music chain unions each with fanart.tv's set into the grid. Keyed by MBID
// when present and otherwise by NAME (so an un-MBID'd artist still gets images),
// reusing the same cached lookup as the enrichment pass. Only the "artist" kind is
// served (the track path carries no artwork); every other kind reports
// ErrSearchUnavailable. An artist with nothing to key by, or with no image for the
// role, is (nil, nil).
func (p *TheAudioDBProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if ref.Kind != "artist" {
		return nil, ErrSearchUnavailable // TheAudioDB owns no listable set for this kind
	}
	key, reqURL, ok := p.artistRequest(ref)
	if !ok {
		return nil, nil // nothing to key a lookup by
	}
	a, err := p.artist(ctx, key, reqURL)
	if err != nil {
		return nil, err
	}
	var urls []string
	switch role {
	case "background":
		urls = a.backgrounds
	case "logo":
		if a.logo != "" {
			urls = []string{a.logo}
		}
	default: // "poster" and anything unspecified → the artist photo
		if a.thumb != "" {
			urls = []string{a.thumb}
		}
	}
	cands := make([]ArtworkCandidate, 0, len(urls))
	for _, u := range urls {
		cands = append(cands, ArtworkCandidate{URL: u, Source: "theaudiodb"})
	}
	return cands, nil
}

// artistRequest builds the cache key + request URL for an artist lookup, keyed by
// MBID (artist-mb.php) when present and otherwise by NAME (search.php). ok is
// false when the ref carries neither — nothing to key a lookup by. Shared by the
// enrichment Lookup and the Artist Photo candidate list.
func (p *TheAudioDBProvider) artistRequest(ref TitleRef) (key, reqURL string, ok bool) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	name := strings.TrimSpace(ref.Title)
	if name == "" {
		name = strings.TrimSpace(ref.Artist)
	}
	switch {
	case mbid != "":
		return "mb:" + mbid,
			p.BaseURL + "/" + url.PathEscape(p.APIKey) + "/artist-mb.php?i=" + url.QueryEscape(mbid), true
	case name != "":
		return "name:" + strings.ToLower(name),
			p.BaseURL + "/" + url.PathEscape(p.APIKey) + "/search.php?s=" + url.QueryEscape(name), true
	default:
		return "", "", false
	}
}

// lookupTrack returns the TheAudioDB track synopsis as Overview, keyed by the
// recording MBID when present and otherwise by artist+name. No record or a record
// with no description is ErrNoMatch. No artwork is returned (out of scope).
func (p *TheAudioDBProvider) lookupTrack(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	track := strings.TrimSpace(ref.Track)
	artist := strings.TrimSpace(ref.Artist)

	var key, reqURL string
	switch {
	case mbid != "":
		key = "track-mb:" + mbid
		reqURL = p.BaseURL + "/" + url.PathEscape(p.APIKey) + "/track-mb.php?i=" + url.QueryEscape(mbid)
	case track != "":
		key = "track:" + strings.ToLower(artist) + "/" + strings.ToLower(track)
		reqURL = p.BaseURL + "/" + url.PathEscape(p.APIKey) +
			"/searchtrack.php?s=" + url.QueryEscape(artist) + "&t=" + url.QueryEscape(track)
	default:
		return TitleMetadata{}, ErrNoMatch // nothing to key a lookup by
	}

	desc, err := p.track(ctx, key, reqURL)
	if err != nil {
		return TitleMetadata{}, err
	}
	if desc == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	return TitleMetadata{Matched: true, Source: "theaudiodb", Overview: desc}, nil
}

func (p *TheAudioDBProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// artist resolves a lookup key to a parsed artist record, returning a zero value
// when TheAudioDB has no record. It serves from the in-process response cache
// when possible (re-enrichment doesn't re-hit the host) and otherwise throttles
// before issuing the request. A no-match is cached as the zero value; a transient
// error is not cached.
func (p *TheAudioDBProvider) artist(ctx context.Context, key, reqURL string) (audiodbArtist, error) {
	if a, ok := p.cached(key); ok {
		return a, nil
	}
	if err := p.throttle(ctx); err != nil {
		return audiodbArtist{}, err
	}
	a, err := p.fetch(ctx, reqURL)
	switch {
	case err == ErrNoMatch:
		p.store(key, audiodbArtist{})
		return audiodbArtist{}, nil
	case err != nil:
		return audiodbArtist{}, err
	}
	p.store(key, a)
	return a, nil
}

// track resolves a lookup key to a track synopsis, returning "" when TheAudioDB
// has no record or the record carries no description. Like artist it serves from
// the response cache, otherwise throttles before the request, and caches a
// no-match as "" (a transient error is not cached).
func (p *TheAudioDBProvider) track(ctx context.Context, key, reqURL string) (string, error) {
	if d, ok := p.cachedTrack(key); ok {
		return d, nil
	}
	if err := p.throttle(ctx); err != nil {
		return "", err
	}
	d, err := p.fetchTrack(ctx, reqURL)
	switch {
	case err == ErrNoMatch:
		p.storeTrack(key, "")
		return "", nil
	case err != nil:
		return "", err
	}
	p.storeTrack(key, d)
	return d, nil
}

// fetch issues one TheAudioDB artist request and parses the first artist's thumb +
// bio. An empty result set ({"artists":null}) — TheAudioDB's "no record" answer —
// is ErrNoMatch; any non-2xx is a real error the chain logs and swallows.
func (p *TheAudioDBProvider) fetch(ctx context.Context, reqURL string) (audiodbArtist, error) {
	// The bio fields are language-suffixed (strBiographyEN/DE/...), so the record
	// is decoded loosely and the wanted fields are pulled by name.
	var out struct {
		Artists []map[string]any `json:"artists"`
	}
	if err := p.get(ctx, reqURL, &out); err != nil {
		return audiodbArtist{}, err
	}
	if len(out.Artists) == 0 || out.Artists[0] == nil {
		return audiodbArtist{}, ErrNoMatch
	}
	a := out.Artists[0]
	bio := mapString(a, biographyField(p.Language))
	if bio == "" {
		bio = mapString(a, "strBiographyEN") // English is the broadest fallback
	}
	var backgrounds []string
	for _, f := range []string{"strArtistFanart", "strArtistFanart2", "strArtistFanart3", "strArtistFanart4"} {
		if u := mapString(a, f); u != "" {
			backgrounds = append(backgrounds, u)
		}
	}
	return audiodbArtist{
		thumb:       mapString(a, "strArtistThumb"),
		backgrounds: backgrounds,
		logo:        mapString(a, "strArtistLogo"),
		bio:         bio,
	}, nil
}

// fetchTrack issues one TheAudioDB track request and parses the first track's
// language-matched synopsis. An empty result set ({"track":null}) is ErrNoMatch;
// any non-2xx is a real error the chain logs and swallows. No artwork is parsed.
func (p *TheAudioDBProvider) fetchTrack(ctx context.Context, reqURL string) (string, error) {
	// The description fields are language-suffixed (strDescriptionEN/DE/...) like the
	// artist bio, so the record is decoded loosely and the wanted field pulled by name.
	var out struct {
		Track []map[string]any `json:"track"`
	}
	if err := p.get(ctx, reqURL, &out); err != nil {
		return "", err
	}
	if len(out.Track) == 0 || out.Track[0] == nil {
		return "", ErrNoMatch
	}
	t := out.Track[0]
	desc := mapString(t, descriptionField(p.Language))
	if desc == "" {
		desc = mapString(t, "strDescriptionEN") // English is the broadest fallback
	}
	return desc, nil
}

// get issues one TheAudioDB GET and decodes the JSON body into out. A 404 is the
// normal "no record" outcome (ErrNoMatch); any other non-2xx is a real error.
func (p *TheAudioDBProvider) get(ctx context.Context, reqURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("enrich: building theaudiodb request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("enrich: theaudiodb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoMatch
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("enrich: theaudiodb: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("enrich: decoding theaudiodb response: %w", err)
	}
	return nil
}

// biographyField maps a metadata language tag to TheAudioDB's language-suffixed
// biography field (e.g. "en-US" -> "strBiographyEN"); a blank tag falls back to
// the English field.
func biographyField(language string) string {
	lang := language
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	lang = strings.ToUpper(strings.TrimSpace(lang))
	if lang == "" {
		return "strBiographyEN"
	}
	return "strBiography" + lang
}

// descriptionField maps a metadata language tag to TheAudioDB's language-suffixed
// track description field (e.g. "en-US" -> "strDescriptionEN"); a blank tag falls
// back to the English field. Mirrors biographyField.
func descriptionField(language string) string {
	lang := language
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	lang = strings.ToUpper(strings.TrimSpace(lang))
	if lang == "" {
		return "strDescriptionEN"
	}
	return "strDescription" + lang
}

// mapString returns m[key] when it is a non-empty string (TheAudioDB encodes
// absent fields as JSON null, which decodes to a nil interface, not a string).
func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// throttle blocks until the next per-host request slot, reserving it so
// concurrent lookups queue rather than burst. It respects context cancellation.
func (p *TheAudioDBProvider) throttle(ctx context.Context) error {
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

func (p *TheAudioDBProvider) cached(key string) (audiodbArtist, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.cache[key]
	return a, ok
}

func (p *TheAudioDBProvider) store(key string, a audiodbArtist) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]audiodbArtist{}
	}
	p.cache[key] = a
}

func (p *TheAudioDBProvider) cachedTrack(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.trackCache[key]
	return d, ok
}

func (p *TheAudioDBProvider) storeTrack(key, desc string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trackCache == nil {
		p.trackCache = map[string]string{}
	}
	p.trackCache[key] = desc
}
