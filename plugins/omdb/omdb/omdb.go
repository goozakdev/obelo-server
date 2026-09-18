// Package omdb is the Obelo Metadata provider for the Open Movie Database, as a
// plugin (ADR-0059, .scratch/bundled-plugins issue 05).
//
// It is a port and not a rewrite. The request, the query parameters, the JSON
// shape, the "N/A means empty" rule, the comma-split genre list, the negative
// cache and the 250 ms pacing all came from internal/enrich/omdb.go, which this
// file replaces — the same request goes out and the same record comes back, which
// is the whole claim the conversion makes. What changed is where the bytes come
// from: there is no net/http here, no client, no timeout and no socket. The
// provider holds a [pluginsdk.Host] and asks it, so the same code runs inside the
// WebAssembly sandbox and in a native `go test` against an in-memory host.
//
// OMDb is the video chain's first fill-only supplement and serves the MOVIE kind
// only: it fills the gaps the authoritative source can leave — a plot (Overview),
// a content rating (Rated) and genres. Every other kind is OutcomeNoMatch without
// a request, because the chain routes non-movie video kinds past it. It declares
// no capabilities, so the host never asks it to search or to list images.
//
// # What it remembers
//
// The negative cache is INSTANCE memory and deliberately not kv. It exists so a
// re-enrichment pass does not re-hit OMDb for a movie it already asked about, and
// it lives exactly as long as the guest instance does — which is what the Go
// provider's in-process map did, and what recycling that provider did to it.
//
// # Where an error goes
//
// [pluginsdk.Unavailable] draws the line and this package does not restate it:
//
//   - "OMDb could not answer right now" — the host refused the fetch, the fetch
//     ran out of the call's budget (ADR-0059 decision 6), or OMDb answered 408,
//     429 or a 5xx. None of that is an answer about the ITEM, so it is
//     OutcomeUnavailable with the reason in Detail, NEVER OutcomeNoMatch and never
//     a Go error. A Go error IS a strike, and three consecutive ones disable the
//     plugin, so a source having a bad afternoon would otherwise take the whole
//     provider down.
//   - "OMDb answered, and the answer is no use" — a 401, a 403, or a document this
//     code cannot read. Those describe OUR REQUEST, and an operator is the only
//     one who can fix a rejected key. They stay Go errors, so the item is parked
//     where they will see it, exactly as the Go provider left it.
//
// OMDb's own {"Response":"False"} is neither: it is the source saying it has no
// such record, which is OutcomeNoMatch, and it is cached as one.
package omdb

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the slug this provider stamps on every record it returns. It is the
// manifest id.
const Source = "omdb"

// DefaultThrottle spaces successive OMDb requests so a large-library backfill
// stays a polite trickle rather than a burst. It is the interval main.go hands to
// [pluginsdk.PacedHost], and it is the number the Go provider used.
const DefaultThrottle = 250 * time.Millisecond

// errNoRecord is OMDb answering "I have no such movie" — its
// {"Response":"False"} payload. It never leaves this package: Lookup turns it
// into OutcomeNoMatch, and the cache remembers it as the zero result.
var errNoRecord = errors.New("omdb has no record for that reference")

// Provider is the OMDb Metadata provider. It holds a [pluginsdk.Host] and the
// response cache: the API key and the base URL are read from Host.Settings() PER
// CALL, because that is where the host publishes them and because a secret is
// readable only while a call it belongs to is on the stack.
type Provider struct {
	host pluginsdk.Host

	// mu guards the cache. A guest instance is single-threaded and the host
	// serializes every call into it (ADR-0058 decision 7), so this lock is never
	// contended in the sandbox — it is here because the native test host is free to
	// drive the provider from several goroutines, and because the Go provider this
	// ports held the same lock.
	mu    sync.Mutex
	cache map[string]result // lookup key -> parsed record (zero value = looked up, no data)
}

var _ pluginapi.MetadataProvider = (*Provider)(nil)

// New builds the provider on a Host. In the sandbox that Host is a
// [pluginsdk.PacedHost] around pluginsdk.Sandbox(); in a test it is
// sdktest.New(...). The provider cannot tell.
func New(h pluginsdk.Host) *Provider {
	return &Provider{host: h, cache: map[string]result{}}
}

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// result is the slice of an OMDb record this provider consumes: the three
// fill-only text fields. A zero value means "no usable data" (cached as a
// negative result and reported as OutcomeNoMatch by Lookup).
type result struct {
	overview      string
	contentRating string
	genres        []string
}

// empty reports whether the record carries none of the wanted fields — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (r result) empty() bool {
	return r.overview == "" && r.contentRating == "" && len(r.genres) == 0
}

// --- Lookup ------------------------------------------------------------------

// Lookup serves the Movie kind (plot/rating/genres); anything else is
// OutcomeNoMatch with no outbound call (the chain routes other kinds straight to
// the authoritative source). It resolves by IMDb id when present and otherwise by
// title+year; a record with none of the wanted fields — or an OMDb no-match — is
// OutcomeNoMatch.
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	if ref.Kind != "movie" {
		return noMatch(), nil
	}
	s := p.host.Settings()

	imdb := strings.TrimSpace(ref.IMDBID)
	title := strings.TrimSpace(ref.Title)

	q := url.Values{}
	q.Set("apikey", s.Secret)
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
		return noMatch(), nil // nothing to key a lookup by
	}

	r, err := p.result(ctx, s, key, q)
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.LookupResponse{}, err
	}
	if r.empty() {
		return noMatch(), nil
	}
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: pluginapi.MetadataRecord{
		Matched:       true,
		Source:        Source,
		Overview:      r.overview,
		ContentRating: r.contentRating,
		Genres:        r.genres,
	}}, nil
}

// --- the two calls this provider does not answer ------------------------------

// Search reports that OMDb is not a searchable authoritative source. The manifest
// declares no `search` capability, so the host never asks — this is the SDK-side
// half of the same statement, and it is the OutcomeUnavailable the host produces
// for an undeclared capability anyway.
func (p *Provider) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// ArtworkCandidates reports that OMDb owns no listable image set: it is a
// fill-only text supplement with no artwork host at all (ADR-0019). The manifest
// declares no `artwork-candidates` capability, so the host never asks.
func (p *Provider) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// --- the records --------------------------------------------------------------

// result resolves a lookup key to a parsed OMDb record, returning a zero value
// when OMDb has no record. It serves from the instance response cache when
// possible (re-enrichment doesn't re-hit the host) and otherwise fetches. A
// no-match is cached as the zero value; a failure is not cached.
func (p *Provider) result(ctx context.Context, s pluginapi.Settings, key string, q url.Values) (result, error) {
	if r, ok := p.cached(key); ok {
		return r, nil
	}
	r, err := p.fetch(ctx, s, q)
	switch {
	case errors.Is(err, errNoRecord):
		p.store(key, result{})
		return result{}, nil
	case err != nil:
		return result{}, err
	}
	p.store(key, r)
	return r, nil
}

func (p *Provider) cached(key string) (result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.cache[key]
	return r, ok
}

func (p *Provider) store(key string, r result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]result{}
	}
	p.cache[key] = r
}

// fetch issues one OMDb request and parses the fill-only fields. OMDb's
// {"Response":"False"} — its "no record" answer — is errNoRecord; "N/A" field
// values are treated as empty.
func (p *Provider) fetch(ctx context.Context, s pluginapi.Settings, q url.Values) (result, error) {
	var out struct {
		Response string `json:"Response"`
		Plot     string `json:"Plot"`
		Rated    string `json:"Rated"`
		Genre    string `json:"Genre"`
	}
	// The Go provider spelled this base + "/?" + query, and a base with a trailing
	// slash collapsed to one. Keep both, so an operator's mirror URL reaches the
	// same path it always did.
	target := strings.TrimRight(s.URL, "/") + "/"
	if err := pluginsdk.GetJSON(ctx, p.host, target, q, &out, pluginsdk.Header("Accept", "application/json")); err != nil {
		return result{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(out.Response), "true") {
		return result{}, errNoRecord // {"Response":"False"} — no record
	}
	return result{
		overview:      field(out.Plot),
		contentRating: field(out.Rated),
		genres:        genres(out.Genre),
	}, nil
}

// field normalizes a single OMDb text field: OMDb encodes an absent value as the
// literal "N/A", which must be treated as empty (never written).
func field(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "N/A") {
		return ""
	}
	return v
}

// genres splits OMDb's comma-separated Genre field into a slice, dropping empty
// and "N/A" entries.
func genres(genre string) []string {
	var out []string
	for _, g := range strings.Split(genre, ",") {
		if g = field(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func noMatch() pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}
}
