// Package fanarttv is the Obelo Metadata provider for fanart.tv, as a plugin
// (ADR-0059, .scratch/bundled-plugins issue 07).
//
// It is a port and not a rewrite. Every endpoint, every query parameter, every
// JSON shape, the "likes" ranking and the HD-before-SD logo rule below came from
// internal/enrich/fanarttv.go, which this file replaces — the same requests go
// out and the same records come back, which is the whole claim the conversion
// makes. What changed is where the bytes come from: there is no net/http here,
// no client, no timeout and no socket. The provider holds a [pluginsdk.Host] and
// asks it, so the same code runs inside the WebAssembly sandbox and in a native
// `go test` against an in-memory host.
//
// # One instance, two kinds
//
// fanart.tv is the one shipped source that serves BOTH media kinds, and it is
// strictly id-keyed in each:
//
//   - Music (artist): the best "artistthumb" (poster), "artistbackground"
//     (background) and logo ("hdmusiclogo" preferred over "musiclogo"), keyed by
//     the MusicBrainz artist id the music chain resolved. No MBID, no lookup.
//   - Video (movie/show): the best poster + background, keyed by the TMDB or IMDb
//     id on a movie ref and by the TheTVDB id on a show ref. The roles are the
//     ones the video lead emits, so the host's fill-only merge takes only a role
//     the lead left empty.
//
// The manifest declares `kinds: [video, music]`, so the host's factory is called
// once per chain and each call returns a view over THIS ONE instance — one
// module, one linear memory, one cache pair, one pacer — and the host serializes
// every call into it. The Go provider was two instances sharing one throttle; a
// plugin is one instance, which is the same arrangement with the sharing made
// structural. The two caches stay NAMESPACED ("movie:"/"tv:" against the bare
// MBID) so a movie and an artist that happen to carry the same id string can
// never serve each other's images.
//
// # Where an error goes
//
// [pluginsdk.Unavailable] draws the line and this package does not restate it:
// a host refusal, a transport or deadline failure, or a 408/429/5xx is
// OutcomeUnavailable with the reason in Detail — never a Go error, because a Go
// error is a STRIKE and three disable the plugin, and an outage at the source
// must not take the provider off the server. A 404 stays what it always was:
// fanart.tv's answer for an unknown id, and so this provider's no-match. Every
// other non-2xx and every unreadable document stay Go errors, exactly as the Go
// provider returned them.
package fanarttv

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the name this provider stamps on every record and artwork candidate
// it returns. It is NOT the manifest id ("fanarttv"): it is the string the Go
// provider wrote, it is what the artwork picker shows beside an image and what
// the api suites assert, and changing it here would be a visible change smuggled
// in with a port whose whole claim is that there is none.
const Source = "fanart.tv"

// DefaultInterval is fanart.tv's own pacing: successive requests are spaced at
// least this far apart so backfilling a large library stays a polite trickle
// rather than a burst that gets this server throttled or banned.
//
// It is the Go provider's defaultFanartTVThrottle, unchanged. main.go hands it to
// [pluginsdk.PacedHost]; [pluginsdk.IntervalFrom] is what lets an operator's
// RateLimitMillis override it, which is exactly what the host-side throttle
// honoured before (ADR-0049, ADR-0059 decision 5).
const DefaultInterval = 250 * time.Millisecond

// Provider is the fanart.tv Metadata provider: a [pluginsdk.Host], and the two
// namespaced response caches the Go provider kept.
//
// The API key and the base URL are NOT fields. They are read from Host.Settings()
// per call, because that is where the host publishes them and because a secret is
// readable only while a call it belongs to is on the stack.
type Provider struct {
	host pluginsdk.Host

	// mu guards both caches. The host serializes calls into a guest instance, so
	// inside the sandbox this lock is never contended; it is here because a native
	// test is free to drive the provider from several goroutines at once, which is
	// how the "one instance serves both chains" claim is asserted at all.
	mu sync.Mutex
	// artists caches the parsed artist artwork by MBID. The zero value is a real
	// entry meaning "looked up, no images"; a failed fetch is never cached.
	artists map[string]artistImages
	// videos caches the best poster+background by "movie:<id>" / "tv:<id>" — the
	// namespace is what keeps a video result out of an artist's cache slot.
	videos map[string]videoArt
}

// The contract interfaces this provider fills. fanart.tv declares one optional
// capability (artwork-candidates) and implements none of the three optional
// interfaces; the SDK's dispatcher answers OutcomeUnavailable for those.
var _ pluginapi.MetadataProvider = (*Provider)(nil)

// New builds the provider on a Host. In the sandbox that Host is a
// [pluginsdk.PacedHost] over pluginsdk.Sandbox(); in a test it is one over
// sdktest.New(...). The provider cannot tell.
func New(h pluginsdk.Host) *Provider {
	return &Provider{
		host:    h,
		artists: map[string]artistImages{},
		videos:  map[string]videoArt{},
	}
}

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// --- Lookup ------------------------------------------------------------------

// Lookup dispatches on kind: an artist lookup returns the best artist image,
// background and logo; a movie/show lookup returns the best poster+background.
// Both are strictly id-keyed and artwork-only (fanart.tv has no text fields);
// anything else — and a record with no usable image — is OutcomeNoMatch.
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	s := p.host.Settings()

	switch ref.Kind {
	case "artist":
		return p.artistLookup(ctx, s, ref)
	case "movie", "show":
		return p.videoLookup(ctx, s, ref)
	default:
		return noMatch(), nil
	}
}

// artistLookup returns the best fanart.tv artist artwork — the best artistthumb
// (poster), artistbackground (background), and logo (hdmusiclogo preferred over
// musiclogo). Only a resolved MBID is served; a blank id (or a record carrying
// none of the three) is a no-match.
func (p *Provider) artistLookup(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (pluginapi.LookupResponse, error) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	if mbid == "" {
		return noMatch(), nil // strictly MBID-keyed: no id, no lookup
	}
	imgs, err := p.images(ctx, s, mbid)
	if err != nil {
		return lookupFailure(err)
	}
	rec := pluginapi.MetadataRecord{Matched: true, Source: Source}
	if u := bestImage(imgs.thumbs); u != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: u})
	}
	if u := bestImage(imgs.backgrounds); u != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "background", URL: u})
	}
	if u := imgs.bestLogo(); u != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "logo", URL: u})
	}
	if len(rec.Artwork) == 0 {
		return noMatch(), nil // record carried no usable image
	}
	return matched(rec), nil
}

// videoLookup returns the best fanart.tv poster+background for a movie/show, keyed
// by the external id the ref carries: a movie by its TMDB or IMDb id, a show by its
// TheTVDB id. With no usable id — or no available image — it is a no-match
// (strictly id-keyed, exactly like the artist path). It emits roles
// "poster"/"background" to match the video lead so the host's fill-only merge takes
// only a role the lead left empty.
func (p *Provider) videoLookup(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (pluginapi.LookupResponse, error) {
	var key, endpoint string
	switch ref.Kind {
	case "movie":
		// The movie endpoint accepts either a TMDB or an IMDb id; prefer TMDB.
		id := strings.TrimSpace(ref.TMDBID)
		if id == "" {
			id = strings.TrimSpace(ref.IMDBID)
		}
		if id == "" {
			return noMatch(), nil // strictly id-keyed: no id, no lookup
		}
		key = "movie:" + id
		endpoint = "/movies/" + url.PathEscape(id)
	case "show":
		id := strings.TrimSpace(ref.TheTVDBID)
		if id == "" {
			return noMatch(), nil // the tv endpoint is TheTVDB-id keyed
		}
		key = "tv:" + id
		endpoint = "/tv/" + url.PathEscape(id)
	default:
		return noMatch(), nil
	}

	art, err := p.bestVideoArt(ctx, s, key, endpoint)
	if err != nil {
		return lookupFailure(err)
	}
	if art.empty() {
		return noMatch(), nil
	}
	rec := pluginapi.MetadataRecord{Matched: true, Source: Source}
	if art.poster != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: art.poster})
	}
	if art.background != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "background", URL: art.background})
	}
	return matched(rec), nil
}

// --- ArtworkCandidates --------------------------------------------------------

// ArtworkCandidates lists the fanart.tv images the Edit-item picker offers for an
// artist role: the FULL set for that role — highest-"likes" first, so the grid
// leads with the "best" image the single-picture Lookup would auto-pick. The role
// selects the set: "background" → artistbackground[], "logo" → the logos
// (hdmusiclogo ahead of musiclogo), and "poster" (or anything else) →
// artistthumb[].
//
// fanart.tv is strictly MBID-keyed and owns no listable set for any other kind, so
// only "artist" is served; every other kind — INCLUDING the video kinds this same
// instance serves through Lookup — is OutcomeUnavailable, the picker's "not now",
// exactly as the Go provider's ErrSearchUnavailable was. A blank MBID, an unknown
// MBID (404) or a record with no image for the role is an empty matched list.
// fanart.tv reports no pixel dimensions, so Width/Height stay 0.
func (p *Provider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	ref := req.Ref
	if ref.Kind != "artist" {
		// fanart.tv owns no listable set for this kind. No call is made.
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	if mbid == "" {
		// Strictly MBID-keyed: no id, no candidates — and no call.
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	s := p.host.Settings()
	imgs, err := p.images(ctx, s, mbid) // the same cached fetch the Lookup uses
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	var urls []string
	switch req.Role {
	case "background":
		urls = rankedImages(imgs.backgrounds)
	case "logo":
		urls = imgs.logoURLs() // HD tier ahead of SD, each by likes
	default: // "poster" and anything unspecified → the artist photos
		urls = rankedImages(imgs.thumbs)
	}
	cands := make([]pluginapi.ArtworkCandidate, 0, len(urls))
	for _, u := range urls {
		cands = append(cands, pluginapi.ArtworkCandidate{URL: u, Source: Source})
	}
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// Search reports that fanart.tv is not a searchable authoritative source. Only the
// authoritative source per kind is ever searched for an Enrichment-override
// candidate list (ADR-0019): a fill-only supplement decorates a record already
// pinned by the authoritative id, so it has no candidate list to offer. The
// manifest declares no `search` capability, so the host never calls this; it
// answers the same "not now" the Go provider's ErrSearchUnavailable did in case
// anything ever does.
func (p *Provider) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// --- The records --------------------------------------------------------------

// image is one fanart.tv image entry. fanart.tv encodes "likes" as a JSON string
// (e.g. "12"), so it is parsed lazily.
type image struct {
	URL   string `json:"url"`
	Likes string `json:"likes"`
}

// artistImages is the fanart.tv artist artwork this provider needs: the thumb
// (poster), background, and logo image lists — each the raw, unsorted entries. The
// logos are split by tier (hdmusiclogo vs musiclogo) so the HD lettering can lead
// regardless of "likes". A zero value means "looked up, no images" (cached as a
// negative result).
type artistImages struct {
	thumbs      []image
	backgrounds []image
	hdLogos     []image
	sdLogos     []image
}

// logoURLs orders the logo candidates HD tier first, then the SD tier, each tier
// highest-"likes" first — so the crisp HD lettering always leads even when an SD
// logo has more likes.
func (a artistImages) logoURLs() []string {
	return append(rankedImages(a.hdLogos), rankedImages(a.sdLogos)...)
}

// bestLogo is the single logo the fill-only Lookup emits: the best HD logo when
// the record carries one, else the best SD logo, else "".
func (a artistImages) bestLogo() string {
	if u := bestImage(a.hdLogos); u != "" {
		return u
	}
	return bestImage(a.sdLogos)
}

// videoArt is the best poster + background fanart.tv carries for a movie/show. A
// zero value means "looked up, no usable image" (cached as a negative result and
// reported as a no-match by videoLookup).
type videoArt struct {
	poster     string
	background string
}

// empty reports whether the record carries neither a poster nor a background — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (a videoArt) empty() bool { return a.poster == "" && a.background == "" }

// --- The fetches ----------------------------------------------------------------

// images resolves the fanart.tv artist artwork for an MBID, returning a zero value
// when the artist has no images. It serves from the in-process response cache when
// possible (re-enrichment and the candidate grid share one fetch). A no-match is
// cached as the zero value; a failed fetch is not cached.
func (p *Provider) images(ctx context.Context, s pluginapi.Settings, mbid string) (artistImages, error) {
	if a, ok := p.cachedArtist(mbid); ok {
		return a, nil
	}
	a, found, err := p.fetchArtistImages(ctx, s, mbid)
	if err != nil {
		return artistImages{}, err
	}
	if !found {
		p.storeArtist(mbid, artistImages{})
		return artistImages{}, nil
	}
	p.storeArtist(mbid, a)
	return a, nil
}

// fetchArtistImages issues one fanart.tv artist request and returns the artist's
// thumb/background/logo image lists (the raw entries, unsorted), plus whether
// fanart.tv HAS a record: a 404 is its answer for an unknown MBID, and it is the
// normal "no record" outcome rather than a failure.
//
// IT DOES NOT PACE, and that is deliberate — as it was before this port, where the
// doc comment read "callers reserve a request slot first". The reservation has
// simply moved: the pacer is the plugin's own now (ADR-0059 decision 5) and lives
// in the [pluginsdk.PacedHost] main.go wraps the sandbox in, so every fetch out of
// this provider — this one included — is spaced by one interval without a single
// call site having to remember to ask. A throttle here as well would double the
// spacing.
func (p *Provider) fetchArtistImages(ctx context.Context, s pluginapi.Settings, mbid string) (artistImages, bool, error) {
	var out struct {
		ArtistThumb      []image `json:"artistthumb"`
		ArtistBackground []image `json:"artistbackground"`
		HDMusicLogo      []image `json:"hdmusiclogo"`
		MusicLogo        []image `json:"musiclogo"`
	}
	found, err := p.getJSON(ctx, s, "/music/"+url.PathEscape(mbid), &out)
	if err != nil || !found {
		return artistImages{}, false, err
	}
	return artistImages{
		thumbs:      out.ArtistThumb,
		backgrounds: out.ArtistBackground,
		hdLogos:     out.HDMusicLogo,
		sdLogos:     out.MusicLogo,
	}, true, nil
}

// bestVideoArt resolves an endpoint to the best poster+background, serving from the
// video response cache when possible (re-enrichment doesn't re-hit fanart.tv). A
// no-match is cached as the zero value; a failed fetch is not cached.
func (p *Provider) bestVideoArt(ctx context.Context, s pluginapi.Settings, key, endpoint string) (videoArt, error) {
	if a, ok := p.cachedVideo(key); ok {
		return a, nil
	}
	a, found, err := p.videoArt(ctx, s, endpoint)
	if err != nil {
		return videoArt{}, err
	}
	if !found {
		p.storeVideo(key, videoArt{})
		return videoArt{}, nil
	}
	p.storeVideo(key, a)
	return a, nil
}

// videoArt fetches a fanart.tv movie/tv record and returns the best
// poster+background. A 404 — fanart.tv's answer for an unknown id — is "no record".
// The movie and tv payloads use different field names for the same two roles, so
// both sets are decoded and coalesced.
func (p *Provider) videoArt(ctx context.Context, s pluginapi.Settings, endpoint string) (videoArt, bool, error) {
	var out struct {
		// Movie payload field names.
		MoviePoster     []image `json:"movieposter"`
		MovieBackground []image `json:"moviebackground"`
		// TV payload field names.
		TVPoster       []image `json:"tvposter"`
		ShowBackground []image `json:"showbackground"`
	}
	found, err := p.getJSON(ctx, s, endpoint, &out)
	if err != nil || !found {
		return videoArt{}, false, err
	}
	poster := bestImage(out.MoviePoster)
	if poster == "" {
		poster = bestImage(out.TVPoster)
	}
	background := bestImage(out.MovieBackground)
	if background == "" {
		background = bestImage(out.ShowBackground)
	}
	return videoArt{poster: poster, background: background}, true, nil
}

// --- The one way out ------------------------------------------------------------

// getJSON issues one GET against the operator's fanart.tv base URL with the API
// key on the query string, and decodes the JSON body into out.
//
// found is false for a 404 and ONLY a 404: that is fanart.tv's answer for an id it
// has never heard of, and the Go provider read it as ErrNoMatch. Every other
// non-2xx and every unreadable document come back as an error, which the caller
// hands to [pluginsdk.Unavailable] to decide whether it describes the SOURCE (and
// so must not count against the plugin) or OUR REQUEST.
func (p *Provider) getJSON(ctx context.Context, s pluginapi.Settings, path string, out any) (bool, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	err := pluginsdk.GetJSON(ctx, p.host, s.URL+path, q, out, pluginsdk.Header("Accept", "application/json"))
	if err != nil {
		var fe *pluginsdk.FetchError
		if errors.As(err, &fe) && fe.IsNotFound() {
			return false, nil // unknown id — the normal "no record" outcome
		}
		return false, err
	}
	return true, nil
}

// --- The "likes" ranking ---------------------------------------------------------

// bestImage picks the highest-"likes" entry, falling back to the first when likes
// are absent/tied; "" when there is no usable image.
func bestImage(imgs []image) string {
	best := ""
	bestLikes := -1
	for _, t := range imgs {
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

// rankedImages returns the non-empty URLs ordered by "likes" descending (a STABLE
// sort, so equal-likes entries keep fanart.tv's own order), so a picker grid leads
// with the same "best" image bestImage picks.
func rankedImages(imgs []image) []string {
	type ranked struct {
		url   string
		likes int
	}
	rs := make([]ranked, 0, len(imgs))
	for _, t := range imgs {
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

// --- The two namespaced caches ----------------------------------------------------

func (p *Provider) cachedArtist(mbid string) (artistImages, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.artists[mbid]
	return a, ok
}

func (p *Provider) storeArtist(mbid string, a artistImages) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.artists == nil {
		p.artists = map[string]artistImages{}
	}
	p.artists[mbid] = a
}

func (p *Provider) cachedVideo(key string) (videoArt, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.videos[key]
	return a, ok
}

func (p *Provider) storeVideo(key string, a videoArt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.videos == nil {
		p.videos = map[string]videoArt{}
	}
	p.videos[key] = a
}

// --- The answers -------------------------------------------------------------------

// lookupFailure turns a fetch failure into either the unavailable answer or the Go
// error, per the package comment.
//
// The CLASSIFICATION is [pluginsdk.Unavailable]'s and not this plugin's: every
// metadata provider needs exactly this rule and none of them should own a copy of
// it, least of all seven copies that could disagree about what a 503 means.
func lookupFailure(err error) (pluginapi.LookupResponse, error) {
	if detail, ok := pluginsdk.Unavailable(err); ok {
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
	}
	return pluginapi.LookupResponse{}, err
}

func noMatch() pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}
}

func matched(rec pluginapi.MetadataRecord) pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: rec}
}
