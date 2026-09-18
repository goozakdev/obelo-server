// Package theaudiodb is the Obelo Metadata provider for TheAudioDB, as a plugin
// (ADR-0059, .scratch/bundled-plugins issue 07).
//
// It is a port and not a rewrite. Every endpoint, every query parameter, every
// JSON shape, the language-suffixed field rule and both caches below came from
// internal/enrich/theaudiodb.go, which this file replaces — the same requests go
// out and the same records come back, which is the whole claim the conversion
// makes. What changed is where the bytes come from: there is no net/http here,
// no client, no timeout and no socket. The provider holds a [pluginsdk.Host] and
// asks it, so the same code runs inside the WebAssembly sandbox and in a native
// `go test` against an in-memory host.
//
// # What it covers
//
// TheAudioDB is the second, broader source in the Music chain. For an artist it
// covers the two gaps fanart.tv leaves: an artist image even when the lead
// produced no MBID — TheAudioDB also matches by NAME — and a real biography,
// which neither the lead (it synthesizes a stub) nor fanart.tv (image-only)
// offers. For a track it covers the lead's other documented gap, a synopsis:
// MusicBrainz returns only a canonical title, so a track's Overview is always
// empty without this source.
//
// # Where an error goes
//
// [pluginsdk.Unavailable] draws the line and this package does not restate it:
// a host refusal, a transport or deadline failure, or a 408/429/5xx is
// OutcomeUnavailable with the reason in Detail — never a Go error, because a Go
// error is a STRIKE and three disable the plugin, and an outage at the source
// must not take the provider off the server. A 404 stays what it always was:
// this provider's no-match. Every other non-2xx and every unreadable document
// stay Go errors, exactly as the Go provider returned them.
package theaudiodb

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the name this provider stamps on every record and artwork candidate
// it returns. It is the string the Go provider wrote — which here happens to
// equal the manifest id — and it is what the artwork picker shows beside an image.
const Source = "theaudiodb"

// DefaultInterval is TheAudioDB's own pacing: successive requests are spaced at
// least this far apart so backfilling a large library stays a polite trickle
// rather than a burst that gets this client throttled or banned.
//
// It is the Go provider's defaultTheAudioDBThrottle, unchanged. main.go hands it
// to [pluginsdk.PacedHost]; [pluginsdk.IntervalFrom] is what lets an operator's
// RateLimitMillis override it, which is exactly what the host-side throttle
// honoured before (ADR-0049, ADR-0059 decision 5).
const DefaultInterval = 250 * time.Millisecond

// Provider is the TheAudioDB Metadata provider: a [pluginsdk.Host], and the two
// response caches the Go provider kept.
//
// The API key, the base URL and the metadata language are NOT fields. They are
// read from Host.Settings() per call, because that is where the host publishes
// them and because a secret is readable only while a call it belongs to is on the
// stack.
type Provider struct {
	host pluginsdk.Host

	// mu guards both caches. The host serializes calls into a guest instance, so
	// inside the sandbox this lock is never contended; it is here because a native
	// test is free to drive the provider from several goroutines at once.
	mu sync.Mutex
	// artists caches parsed artist records by lookup key ("mb:<mbid>" /
	// "name:<lowercased name>"). The zero value is a real entry meaning "looked up,
	// no data"; a failed fetch is never cached.
	artists map[string]artist
	// tracks caches track synopses by lookup key ("track-mb:<mbid>" /
	// "track:<artist>/<title>"). "" means "looked up, no data".
	tracks map[string]string
}

// The contract interfaces this provider fills. TheAudioDB declares one optional
// capability (artwork-candidates) and implements none of the three optional
// interfaces; the SDK's dispatcher answers OutcomeUnavailable for those.
var _ pluginapi.MetadataProvider = (*Provider)(nil)

// New builds the provider on a Host. In the sandbox that Host is a
// [pluginsdk.PacedHost] over pluginsdk.Sandbox(); in a test it is one over
// sdktest.New(...). The provider cannot tell.
func New(h pluginsdk.Host) *Provider {
	return &Provider{
		host:    h,
		artists: map[string]artist{},
		tracks:  map[string]string{},
	}
}

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// --- Lookup ------------------------------------------------------------------

// Lookup serves the artist kind (image + biography) and the track kind
// (synopsis); anything else is a no-match (the chain routes other kinds straight
// to the lead).
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	s := p.host.Settings()

	switch ref.Kind {
	case "artist":
		return p.lookupArtist(ctx, s, ref)
	case "track":
		return p.lookupTrack(ctx, s, ref)
	default:
		return noMatch(), nil
	}
}

// lookupArtist returns the TheAudioDB artist image (poster), background, logo and
// biography (Overview), keyed by MBID when present and otherwise by name; an
// artist with none of the four is a no-match.
func (p *Provider) lookupArtist(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (pluginapi.LookupResponse, error) {
	key, reqURL, ok := artistRequest(s, ref)
	if !ok {
		return noMatch(), nil // nothing to key a lookup by
	}
	a, err := p.artist(ctx, s, key, reqURL)
	if err != nil {
		return lookupFailure(err)
	}
	if a.thumb == "" && len(a.backgrounds) == 0 && a.logo == "" && a.bio == "" {
		return noMatch(), nil
	}
	rec := pluginapi.MetadataRecord{Matched: true, Source: Source}
	if a.thumb != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: a.thumb})
	}
	if len(a.backgrounds) > 0 {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "background", URL: a.backgrounds[0]})
	}
	if a.logo != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "logo", URL: a.logo})
	}
	if a.bio != "" {
		rec.Overview = a.bio
	}
	return matched(rec), nil
}

// lookupTrack returns the TheAudioDB track synopsis as Overview, keyed by the
// recording MBID when present and otherwise by artist+name. No record, or a record
// with no description, is a no-match. No artwork is returned (out of scope).
func (p *Provider) lookupTrack(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (pluginapi.LookupResponse, error) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	track := strings.TrimSpace(ref.Track)
	artistName := strings.TrimSpace(ref.Artist)

	var key, reqURL string
	switch {
	case mbid != "":
		key = "track-mb:" + mbid
		reqURL = base(s) + "/track-mb.php?i=" + url.QueryEscape(mbid)
	case track != "":
		key = "track:" + strings.ToLower(artistName) + "/" + strings.ToLower(track)
		reqURL = base(s) + "/searchtrack.php?s=" + url.QueryEscape(artistName) + "&t=" + url.QueryEscape(track)
	default:
		return noMatch(), nil // nothing to key a lookup by
	}

	desc, err := p.track(ctx, s, key, reqURL)
	if err != nil {
		return lookupFailure(err)
	}
	if desc == "" {
		return noMatch(), nil
	}
	return matched(pluginapi.MetadataRecord{Matched: true, Source: Source, Overview: desc}), nil
}

// --- ArtworkCandidates --------------------------------------------------------

// ArtworkCandidates lists TheAudioDB's images for an artist role, backing the
// Edit-item picker as the fallback source behind fanart.tv. The role selects the
// set: "background" → the strArtistFanart* list, "logo" → the single
// strArtistLogo, "poster" (or anything else) → the single strArtistThumb — the
// music chain unions each with fanart.tv's set into the grid. Keyed by MBID when
// present and otherwise by NAME (so an un-MBID'd artist still gets images),
// reusing the same cached lookup as the enrichment pass.
//
// Only the "artist" kind is served (the track path carries no artwork); every
// other kind is OutcomeUnavailable, the picker's "not now", exactly as the Go
// provider's ErrSearchUnavailable was. An artist with nothing to key by, or with
// no image for the role, is an empty matched list.
func (p *Provider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	ref := req.Ref
	if ref.Kind != "artist" {
		// TheAudioDB owns no listable set for this kind. No call is made.
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	s := p.host.Settings()
	key, reqURL, ok := artistRequest(s, ref)
	if !ok {
		// Nothing to key a lookup by — no candidates, and no call.
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	a, err := p.artist(ctx, s, key, reqURL)
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	var urls []string
	switch req.Role {
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
	cands := make([]pluginapi.ArtworkCandidate, 0, len(urls))
	for _, u := range urls {
		cands = append(cands, pluginapi.ArtworkCandidate{URL: u, Source: Source})
	}
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// Search reports that TheAudioDB is not a searchable authoritative source. Only
// the authoritative source per kind is ever searched for an Enrichment-override
// candidate list (ADR-0019): a fill-only supplement decorates a record already
// pinned by the authoritative id, so it has no candidate list to offer. The
// manifest declares no `search` capability, so the host never calls this; it
// answers the same "not now" the Go provider's ErrSearchUnavailable did in case
// anything ever does.
func (p *Provider) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// --- The records --------------------------------------------------------------

// artist is the slice of a TheAudioDB artist record this provider needs: the
// thumbnail (poster), background(s), logo, and the chosen-language biography. A
// zero value means "no usable data" (cached as a negative result). backgrounds
// holds the non-empty strArtistFanart/2/3/4 in order (the first is the best-of the
// Lookup emits; the full list feeds the Background picker grid).
type artist struct {
	thumb       string
	backgrounds []string
	logo        string
	bio         string
}

// artistRequest builds the cache key + request URL for an artist lookup, keyed by
// MBID (artist-mb.php) when present and otherwise by NAME (search.php). ok is
// false when the ref carries neither — nothing to key a lookup by. Shared by the
// enrichment Lookup and the Artist Photo candidate list.
func artistRequest(s pluginapi.Settings, ref pluginapi.MediaRef) (key, reqURL string, ok bool) {
	mbid := strings.TrimSpace(ref.MusicbrainzID)
	name := strings.TrimSpace(ref.Title)
	if name == "" {
		name = strings.TrimSpace(ref.Artist)
	}
	switch {
	case mbid != "":
		return "mb:" + mbid, base(s) + "/artist-mb.php?i=" + url.QueryEscape(mbid), true
	case name != "":
		return "name:" + strings.ToLower(name), base(s) + "/search.php?s=" + url.QueryEscape(name), true
	default:
		return "", "", false
	}
}

// base is the operator's base URL with the API key appended as a path segment —
// TheAudioDB puts the key in the PATH rather than on the query string, which is
// why this provider builds whole URLs instead of handing a path and a query to the
// SDK's GetJSON.
func base(s pluginapi.Settings) string { return s.URL + "/" + url.PathEscape(s.Secret) }

// --- The fetches ----------------------------------------------------------------

// artist resolves a lookup key to a parsed artist record, returning a zero value
// when TheAudioDB has no record. It serves from the in-process response cache when
// possible (re-enrichment doesn't re-hit the host). A no-match is cached as the
// zero value; a failed fetch is not cached.
func (p *Provider) artist(ctx context.Context, s pluginapi.Settings, key, reqURL string) (artist, error) {
	if a, ok := p.cachedArtist(key); ok {
		return a, nil
	}
	a, found, err := p.fetchArtist(ctx, s, reqURL)
	if err != nil {
		return artist{}, err
	}
	if !found {
		p.storeArtist(key, artist{})
		return artist{}, nil
	}
	p.storeArtist(key, a)
	return a, nil
}

// track resolves a lookup key to a track synopsis, returning "" when TheAudioDB
// has no record or the record carries no description. Like artist it serves from
// the response cache and caches a no-match as "" (a failed fetch is not cached).
func (p *Provider) track(ctx context.Context, s pluginapi.Settings, key, reqURL string) (string, error) {
	if d, ok := p.cachedTrack(key); ok {
		return d, nil
	}
	d, found, err := p.fetchTrack(ctx, s, reqURL)
	if err != nil {
		return "", err
	}
	if !found {
		p.storeTrack(key, "")
		return "", nil
	}
	p.storeTrack(key, d)
	return d, nil
}

// fetchArtist issues one TheAudioDB artist request and parses the first artist's
// images + bio. An empty result set ({"artists":null}) — TheAudioDB's "no record"
// answer — is reported as found=false, exactly as a 404 is.
func (p *Provider) fetchArtist(ctx context.Context, s pluginapi.Settings, reqURL string) (artist, bool, error) {
	// The bio fields are language-suffixed (strBiographyEN/DE/...), so the record
	// is decoded loosely and the wanted fields are pulled by name.
	var out struct {
		Artists []map[string]any `json:"artists"`
	}
	found, err := p.getJSON(ctx, reqURL, &out)
	if err != nil || !found {
		return artist{}, false, err
	}
	if len(out.Artists) == 0 || out.Artists[0] == nil {
		return artist{}, false, nil
	}
	a := out.Artists[0]
	bio := mapString(a, biographyField(s.Language))
	if bio == "" {
		bio = mapString(a, "strBiographyEN") // English is the broadest fallback
	}
	var backgrounds []string
	for _, f := range []string{"strArtistFanart", "strArtistFanart2", "strArtistFanart3", "strArtistFanart4"} {
		if u := mapString(a, f); u != "" {
			backgrounds = append(backgrounds, u)
		}
	}
	return artist{
		thumb:       mapString(a, "strArtistThumb"),
		backgrounds: backgrounds,
		logo:        mapString(a, "strArtistLogo"),
		bio:         bio,
	}, true, nil
}

// fetchTrack issues one TheAudioDB track request and parses the first track's
// language-matched synopsis. An empty result set ({"track":null}) is found=false.
// No artwork is parsed.
func (p *Provider) fetchTrack(ctx context.Context, s pluginapi.Settings, reqURL string) (string, bool, error) {
	// The description fields are language-suffixed (strDescriptionEN/DE/...) like
	// the artist bio, so the record is decoded loosely and the field pulled by name.
	var out struct {
		Track []map[string]any `json:"track"`
	}
	found, err := p.getJSON(ctx, reqURL, &out)
	if err != nil || !found {
		return "", false, err
	}
	if len(out.Track) == 0 || out.Track[0] == nil {
		return "", false, nil
	}
	t := out.Track[0]
	desc := mapString(t, descriptionField(s.Language))
	if desc == "" {
		desc = mapString(t, "strDescriptionEN") // English is the broadest fallback
	}
	return desc, true, nil
}

// --- The one way out ------------------------------------------------------------

// getJSON issues one GET against a fully-built TheAudioDB URL and decodes the JSON
// body into out.
//
// found is false for a 404 and ONLY a 404 — the normal "no record" outcome the Go
// provider read as ErrNoMatch. Every other non-2xx and every unreadable document
// come back as an error, which the caller hands to [pluginsdk.Unavailable] to
// decide whether it describes the SOURCE (and so must not count against the
// plugin) or OUR REQUEST.
func (p *Provider) getJSON(ctx context.Context, reqURL string, out any) (bool, error) {
	err := pluginsdk.GetJSON(ctx, p.host, reqURL, nil, out, pluginsdk.Header("Accept", "application/json"))
	if err != nil {
		var fe *pluginsdk.FetchError
		if errors.As(err, &fe) && fe.IsNotFound() {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// --- The language-suffixed fields --------------------------------------------------

// biographyField maps a metadata language tag to TheAudioDB's language-suffixed
// biography field (e.g. "en-US" -> "strBiographyEN"); a blank tag falls back to
// the English field.
func biographyField(language string) string {
	if lang := languageSuffix(language); lang != "" {
		return "strBiography" + lang
	}
	return "strBiographyEN"
}

// descriptionField maps a metadata language tag to TheAudioDB's language-suffixed
// track description field (e.g. "en-US" -> "strDescriptionEN"); a blank tag falls
// back to the English field. Mirrors biographyField.
func descriptionField(language string) string {
	if lang := languageSuffix(language); lang != "" {
		return "strDescription" + lang
	}
	return "strDescriptionEN"
}

// languageSuffix is the upper-cased primary subtag of a language tag ("en-US" ->
// "EN"), or "" for a blank one.
func languageSuffix(language string) string {
	lang := language
	if i := strings.IndexAny(lang, "-_"); i >= 0 {
		lang = lang[:i]
	}
	return strings.ToUpper(strings.TrimSpace(lang))
}

// mapString returns m[key] when it is a non-empty string (TheAudioDB encodes
// absent fields as JSON null, which decodes to a nil interface, not a string).
func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// --- The two caches ----------------------------------------------------------------

func (p *Provider) cachedArtist(key string) (artist, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.artists[key]
	return a, ok
}

func (p *Provider) storeArtist(key string, a artist) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.artists == nil {
		p.artists = map[string]artist{}
	}
	p.artists[key] = a
}

func (p *Provider) cachedTrack(key string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.tracks[key]
	return d, ok
}

func (p *Provider) storeTrack(key, desc string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tracks == nil {
		p.tracks = map[string]string{}
	}
	p.tracks[key] = desc
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
