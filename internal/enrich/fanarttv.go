package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FanartTVProvider supplies the artwork MusicBrainz and TMDB document they lack.
// It serves TWO kinds, both strictly id-keyed and both artwork-ONLY (fanart.tv has
// no text fields):
//
//   - Music (artist): the best "artistthumb" (poster), "artistbackground"
//     (background), and logo ("hdmusiclogo" preferred over "musiclogo") as
//     ArtworkRefs — the same three roles the Show/Movie path uses, so mergeArtwork
//     fills only a role a prior source left empty. fanart.tv is strictly MBID-keyed
//     here, so it only serves the "artist" kind and only when the caller already
//     resolved a MusicBrainz artist id (the music chain feeds it via
//     ref.MusicbrainzID). Every other music kind — and a blank id — returns
//     ErrNoMatch.
//   - Video (movie/show): the best poster + background as ArtworkRefs with roles
//     "poster"/"background" (the same roles tmdb.go emits, so mergeArtwork fills only
//     a role TMDB left empty). fanart.tv's movie endpoint is keyed by TMDB or IMDb id
//     (ref.TMDBID/ref.IMDBID) and its tv endpoint by TheTVDB id (ref.TheTVDBID); with
//     no usable id it returns ErrNoMatch (strictly id-keyed, exactly like the artist
//     path is strictly MBID-keyed).
//
// It owns all fanart.tv HTTP/JSON specifics behind the MetadataProvider seam
// (ADR-0006), and like the other providers treats a no-match or network failure as a
// normal outcome the chain degrades over, never identity (ADR-0001/0002). The video
// path reuses the same per-host throttle and in-process response cache as the artist
// path (the cache key space is namespaced so video and artist lookups never collide).
type FanartTVProvider struct {
	APIKey     string
	BaseURL    string // e.g. https://webservice.fanart.tv/v3
	UserAgent  string // fanart.tv asks clients to identify themselves
	HTTPClient *http.Client

	// minInterval is the per-host throttle: successive requests are spaced at
	// least this far apart so backfilling a large library doesn't get the server
	// throttled or banned by fanart.tv.
	minInterval time.Duration

	mu    sync.Mutex
	last  time.Time               // time the next request slot was reserved (throttle)
	cache map[string]artistImages // artist cache: MBID -> parsed image lists (zero value = looked up, no images)

	// videoCache namespaces the video lookups so a movie/show artwork result never
	// collides with an artist result. Keyed by "movie:<id>" / "tv:<id>" -> the
	// encoded best poster+background ("" = looked up, no image).
	videoCache map[string]fanartVideoArt
}

// defaultFanartTVThrottle spaces successive fanart.tv requests so a large-library
// backfill stays a polite trickle rather than a burst.
const defaultFanartTVThrottle = 250 * time.Millisecond

// NewFanartTVProvider builds a provider from config. A nil HTTP client gets a
// default with a sane timeout (a slow lookup must not hang a pass).
func NewFanartTVProvider(apiKey, baseURL string) *FanartTVProvider {
	return &FanartTVProvider{
		APIKey:      apiKey,
		BaseURL:     baseURL,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		minInterval: defaultFanartTVThrottle,
		cache:       map[string]artistImages{},
		videoCache:  map[string]fanartVideoArt{},
	}
}

// Lookup dispatches on kind: an artist lookup returns the best artist image as an
// ArtworkRef{Role:"poster"}; a movie/show lookup returns the best poster+background.
// Both are strictly id-keyed and artwork-only; anything else (or no available image)
// is ErrNoMatch.
func (p *FanartTVProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "artist":
		return p.artistLookup(ctx, ref)
	case "movie", "show":
		return p.videoLookup(ctx, ref)
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

// artistLookup returns the best fanart.tv artist artwork as ArtworkRefs — the best
// artistthumb (poster), artistbackground (background), and logo (hdmusiclogo
// preferred over musiclogo). Only a resolved MBID is served; a blank id (or a
// record carrying none of the three) is ErrNoMatch.
func (p *FanartTVProvider) artistLookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	if mbid == "" {
		return TitleMetadata{}, ErrNoMatch // strictly MBID-keyed: no id, no lookup
	}
	imgs, err := p.images(ctx, mbid)
	if err != nil {
		return TitleMetadata{}, err
	}
	meta := TitleMetadata{Matched: true, Source: "fanart.tv"}
	if u := bestArtistThumb(imgs.thumbs); u != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: u})
	}
	if u := bestArtistThumb(imgs.backgrounds); u != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "background", URL: u})
	}
	if u := imgs.bestLogo(); u != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "logo", URL: u})
	}
	if len(meta.Artwork) == 0 {
		return TitleMetadata{}, ErrNoMatch // record carried no usable image
	}
	return meta, nil
}

func (p *FanartTVProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

// fanartImage is one fanart.tv image entry. fanart.tv encodes "likes" as a JSON
// string (e.g. "12"), so it is parsed lazily.
type fanartImage struct {
	URL   string `json:"url"`
	Likes string `json:"likes"`
}

// artistImages is the fanart.tv artist artwork this provider needs: the thumb
// (poster), background, and logo image lists — each the raw, unsorted entries. The
// logos are split by tier (hdmusiclogo vs musiclogo) so the HD lettering can lead
// regardless of "likes". A zero value means "looked up, no images" (cached as a
// negative result).
type artistImages struct {
	thumbs      []fanartImage
	backgrounds []fanartImage
	hdLogos     []fanartImage
	sdLogos     []fanartImage
}

// logoURLs orders the logo candidates HD tier first, then the SD tier, each tier
// highest-"likes" first — so the crisp HD lettering always leads even when an SD
// logo has more likes.
func (a artistImages) logoURLs() []string {
	return append(sortedArtistThumbs(a.hdLogos), sortedArtistThumbs(a.sdLogos)...)
}

// bestLogo is the single logo the fill-only Lookup emits: the best HD logo when the
// record carries one, else the best SD logo, else "".
func (a artistImages) bestLogo() string {
	if u := bestArtistThumb(a.hdLogos); u != "" {
		return u
	}
	return bestArtistThumb(a.sdLogos)
}

// images resolves the fanart.tv artist artwork for an MBID, returning a zero value
// when the artist has no images. It serves from the in-process response cache when
// possible (re-enrichment and the candidate grid share one fetch) and otherwise
// throttles before issuing the request. A no-match is cached as the zero value; a
// transient error is not cached.
func (p *FanartTVProvider) images(ctx context.Context, mbid string) (artistImages, error) {
	if a, ok := p.cached(mbid); ok {
		return a, nil
	}
	if err := p.throttle(ctx); err != nil {
		return artistImages{}, err
	}
	a, err := p.fetchArtistImages(ctx, mbid)
	switch {
	case err == ErrNoMatch:
		p.store(mbid, artistImages{})
		return artistImages{}, nil
	case err != nil:
		return artistImages{}, err
	}
	p.store(mbid, a)
	return a, nil
}

// fetchArtistImages issues one fanart.tv artist request and returns the artist's
// thumb/background/logo image lists (the raw entries, unsorted). It is the shared
// HTTP/parse layer behind both the Lookup (which collapses each role to its best
// image) and the per-role candidate lists (which surface them all). Logos coalesce
// hdmusiclogo ahead of musiclogo so the HD lettering leads. A 404 — fanart.tv's
// answer for an unknown MBID — is ErrNoMatch; any other non-2xx is a real error the
// chain logs and swallows. It does NOT throttle; callers reserve a request slot
// first (images / ArtworkCandidates).
func (p *FanartTVProvider) fetchArtistImages(ctx context.Context, mbid string) (artistImages, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	u := p.BaseURL + "/music/" + url.PathEscape(mbid)
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return artistImages{}, fmt.Errorf("enrich: building fanart.tv request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return artistImages{}, fmt.Errorf("enrich: fanart.tv request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return artistImages{}, ErrNoMatch // unknown MBID — the normal "no record" outcome
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return artistImages{}, fmt.Errorf("enrich: fanart.tv music/%s: status %d", mbid, resp.StatusCode)
	}
	var out struct {
		ArtistThumb      []fanartImage `json:"artistthumb"`
		ArtistBackground []fanartImage `json:"artistbackground"`
		HDMusicLogo      []fanartImage `json:"hdmusiclogo"`
		MusicLogo        []fanartImage `json:"musiclogo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return artistImages{}, fmt.Errorf("enrich: decoding fanart.tv response: %w", err)
	}
	return artistImages{
		thumbs:      out.ArtistThumb,
		backgrounds: out.ArtistBackground,
		hdLogos:     out.HDMusicLogo,
		sdLogos:     out.MusicLogo,
	}, nil
}

// ArtworkCandidates lists the fanart.tv images the Edit-item picker offers for an
// artist role (artwork-management/02): the FULL set for that role — highest-"likes"
// first, so the grid leads with the "best" image the single-picture Lookup would
// auto-pick — each surfaced as an ArtworkCandidate. The role selects the set:
// "background" → artistbackground[], "logo" → logos (hdmusiclogo ahead of
// musiclogo), and "poster" (or anything else) → artistthumb[]. fanart.tv is
// strictly MBID-keyed and owns no listable set for other kinds, so only the
// "artist" kind with a resolved MBID is served; every other kind (video candidates
// list through TMDB, not fanart.tv) reports ErrSearchUnavailable. A blank MBID, an
// unknown MBID (404), or a record with no image for the role is (nil, nil).
// fanart.tv reports no pixel dimensions, so Width/Height stay 0.
func (p *FanartTVProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if ref.Kind != "artist" {
		return nil, ErrSearchUnavailable // fanart.tv owns no listable set for this kind
	}
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	if mbid == "" {
		return nil, nil // strictly MBID-keyed: no id, no candidates
	}
	imgs, err := p.images(ctx, mbid) // shared cached fetch (throttles internally)
	if err != nil {
		return nil, err
	}
	var urls []string
	switch role {
	case "background":
		urls = sortedArtistThumbs(imgs.backgrounds)
	case "logo":
		urls = imgs.logoURLs() // HD tier ahead of SD, each by likes
	default: // "poster" and anything unspecified → the artist photos
		urls = sortedArtistThumbs(imgs.thumbs)
	}
	cands := make([]ArtworkCandidate, 0, len(urls))
	for _, u := range urls {
		cands = append(cands, ArtworkCandidate{URL: u, Source: "fanart.tv"})
	}
	return cands, nil
}

// bestArtistThumb picks the highest-"likes" artistthumb, falling back to the
// first when likes are absent/tied; "" when there is no usable image.
func bestArtistThumb(thumbs []fanartImage) string {
	best := ""
	bestLikes := -1
	for _, t := range thumbs {
		if t.URL == "" {
			continue
		}
		likes, _ := strconv.Atoi(strings.TrimSpace(t.Likes))
		if best == "" || likes > bestLikes {
			best, bestLikes = t.URL, likes
		}
	}
	return best
}

// sortedArtistThumbs returns the non-empty artistthumb URLs ordered by "likes"
// descending (a stable sort, so equal-likes entries keep fanart.tv's order), so
// the Artist Photo grid leads with the same "best" image bestArtistThumb picks.
func sortedArtistThumbs(thumbs []fanartImage) []string {
	type ranked struct {
		url   string
		likes int
	}
	rs := make([]ranked, 0, len(thumbs))
	for _, t := range thumbs {
		if t.URL == "" {
			continue
		}
		likes, _ := strconv.Atoi(strings.TrimSpace(t.Likes))
		rs = append(rs, ranked{url: t.URL, likes: likes})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].likes > rs[j].likes })
	urls := make([]string, len(rs))
	for i, r := range rs {
		urls[i] = r.url
	}
	return urls
}

// fanartVideoArt is the best poster + background fanart.tv carries for a movie/show.
// A zero value means "looked up, no usable image" (cached as a negative result and
// reported as ErrNoMatch by videoLookup).
type fanartVideoArt struct {
	poster     string
	background string
}

// empty reports whether the record carries neither a poster nor a background — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (a fanartVideoArt) empty() bool { return a.poster == "" && a.background == "" }

// videoLookup returns the best fanart.tv poster+background for a movie/show, keyed by
// the external id the ref carries: a movie by its TMDB or IMDb id, a show by its
// TheTVDB id. With no usable id — or no available image — it is ErrNoMatch (strictly
// id-keyed, exactly like the artist path). It emits roles "poster"/"background" to
// match tmdb.go so mergeArtwork fills only a role TMDB left empty.
func (p *FanartTVProvider) videoLookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	var key, endpoint string
	switch ref.Kind {
	case "movie":
		// The movie endpoint accepts either a TMDB or an IMDb id; prefer TMDB.
		id := strings.TrimSpace(ref.TMDBID)
		if id == "" {
			id = strings.TrimSpace(ref.IMDBID)
		}
		if id == "" {
			return TitleMetadata{}, ErrNoMatch // strictly id-keyed: no id, no lookup
		}
		key = "movie:" + id
		endpoint = "/movies/" + url.PathEscape(id)
	case "show":
		id := strings.TrimSpace(ref.TheTVDBID)
		if id == "" {
			return TitleMetadata{}, ErrNoMatch // the tv endpoint is TheTVDB-id keyed
		}
		key = "tv:" + id
		endpoint = "/tv/" + url.PathEscape(id)
	default:
		return TitleMetadata{}, ErrNoMatch
	}

	art, err := p.bestVideoArt(ctx, key, endpoint)
	if err != nil {
		return TitleMetadata{}, err
	}
	if art.empty() {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{Matched: true, Source: "fanart.tv"}
	if art.poster != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: art.poster})
	}
	if art.background != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "background", URL: art.background})
	}
	return meta, nil
}

// bestVideoArt resolves an endpoint to the best poster+background, serving from the
// video response cache when possible (re-enrichment doesn't re-hit fanart.tv) and
// otherwise throttling before issuing the request. A no-match is cached as the zero
// value; a transient error is not cached.
func (p *FanartTVProvider) bestVideoArt(ctx context.Context, key, endpoint string) (fanartVideoArt, error) {
	if a, ok := p.cachedVideo(key); ok {
		return a, nil
	}
	if err := p.throttle(ctx); err != nil {
		return fanartVideoArt{}, err
	}
	a, err := p.videoArt(ctx, endpoint)
	switch {
	case err == ErrNoMatch:
		p.storeVideo(key, fanartVideoArt{})
		return fanartVideoArt{}, nil
	case err != nil:
		return fanartVideoArt{}, err
	}
	p.storeVideo(key, a)
	return a, nil
}

// videoArt fetches a fanart.tv movie/tv record and returns the best poster+background.
// A 404 — fanart.tv's answer for an unknown id — is ErrNoMatch; any other non-2xx is a
// real error the chain logs and swallows. The movie and tv payloads use different
// field names for the same two roles, so both sets are decoded and coalesced.
func (p *FanartTVProvider) videoArt(ctx context.Context, endpoint string) (fanartVideoArt, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	u := p.BaseURL + endpoint
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fanartVideoArt{}, fmt.Errorf("enrich: building fanart.tv request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return fanartVideoArt{}, fmt.Errorf("enrich: fanart.tv request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fanartVideoArt{}, ErrNoMatch // unknown id — the normal "no record" outcome
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fanartVideoArt{}, fmt.Errorf("enrich: fanart.tv %s: status %d", endpoint, resp.StatusCode)
	}
	var out struct {
		// Movie payload field names.
		MoviePoster     []fanartImage `json:"movieposter"`
		MovieBackground []fanartImage `json:"moviebackground"`
		// TV payload field names.
		TVPoster       []fanartImage `json:"tvposter"`
		ShowBackground []fanartImage `json:"showbackground"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fanartVideoArt{}, fmt.Errorf("enrich: decoding fanart.tv response: %w", err)
	}
	poster := bestArtistThumb(out.MoviePoster)
	if poster == "" {
		poster = bestArtistThumb(out.TVPoster)
	}
	background := bestArtistThumb(out.MovieBackground)
	if background == "" {
		background = bestArtistThumb(out.ShowBackground)
	}
	return fanartVideoArt{poster: poster, background: background}, nil
}

func (p *FanartTVProvider) cachedVideo(key string) (fanartVideoArt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.videoCache[key]
	return a, ok
}

func (p *FanartTVProvider) storeVideo(key string, a fanartVideoArt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.videoCache == nil {
		p.videoCache = map[string]fanartVideoArt{}
	}
	p.videoCache[key] = a
}

// throttle blocks until the next per-host request slot, reserving it so
// concurrent lookups queue rather than burst. It respects context cancellation.
func (p *FanartTVProvider) throttle(ctx context.Context) error {
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

func (p *FanartTVProvider) cached(mbid string) (artistImages, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.cache[mbid]
	return a, ok
}

func (p *FanartTVProvider) store(mbid string, a artistImages) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]artistImages{}
	}
	p.cache[mbid] = a
}
