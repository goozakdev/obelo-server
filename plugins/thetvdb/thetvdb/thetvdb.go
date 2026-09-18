// Package thetvdb is the Obelo Metadata provider for TheTVDB, as a plugin
// (ADR-0059, .scratch/bundled-plugins issue 05).
//
// It is a port and not a rewrite. The endpoints, the query parameters, the JSON
// shapes, the bearer-token login dance, the 401-refresh-once rule, the "N/A means
// empty" normalization, the response cache and the 250 ms pacing all came from
// internal/enrich/thetvdb.go, which this file replaces — the same requests go out
// and the same records come back, which is the whole claim the conversion makes.
// What changed is where the bytes come from: there is no net/http here, no
// client, no timeout and no socket. The provider holds a [pluginsdk.Host] and
// asks it, so the same code runs inside the WebAssembly sandbox and in a native
// `go test` against an in-memory host.
//
// TheTVDB is the video chain's fill-only supplement for the TV kinds
// (show/season/episode): it fills the gaps the authoritative source can leave —
// a canonical Name, an Overview, Genres and a still/poster. Every non-TV kind is
// OutcomeNoMatch without an outbound call, not even a login. It declares no
// capabilities, so the host never asks it to search or to list images. A Name it
// contributes is applied by the HOST as a display-only override; it is never
// identity (ADR-0002).
//
// # What it remembers
//
// The bearer token and the response cache are INSTANCE memory, and deliberately
// not kv. The Go provider minted a token on first use, reused it, refreshed it
// once on a 401, and lost the lot when the provider was rebuilt — which a
// settings save did. A guest instance has exactly that lifetime: recycle it and
// the next call mints a fresh token, which is what a fresh Go provider did. A
// token in kv would outlive the thing that earned it and would be a credential
// written to disk that nobody asked to store.
//
// # Where an error goes
//
// [pluginsdk.Unavailable] draws the line and this package does not restate it. A
// host refusal, a spent call budget, a transport failure or a 408/429/5xx is
// OutcomeUnavailable with the reason in Detail — never OutcomeNoMatch (a claim
// about the item) and never a Go error (a strike against the plugin, three of
// which disable it). A 404 is TheTVDB's "no record" answer and is OutcomeNoMatch,
// exactly as the Go provider's ErrNoMatch was. Everything else — a 403, a 401
// that survived a re-login, a document this code cannot read — stays a Go error,
// so the item is parked where an operator will see it.
//
// The 401 is the one status this package reads for itself, and it reads it as
// "the token is stale", not as a failure: the first one drops the cached token
// and retries the request once with a fresh login. Only a SECOND 401 is a Go
// error, because by then the apikey itself is what TheTVDB is rejecting.
package thetvdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
const Source = "thetvdb"

// DefaultThrottle spaces successive TheTVDB requests so a large-library backfill
// stays a polite trickle rather than a burst. It is the interval main.go hands to
// [pluginsdk.PacedHost], and it is the number the Go provider used.
const DefaultThrottle = 250 * time.Millisecond

// The three constants this provider would otherwise import net/http for.
//
// Spelling them here is not pedantry about a dependency: net/http drags the whole
// client, server, transport and TLS stack into the module, and MEASURED on this
// plugin it is 1.9 MB of wasm — about half a megabyte compressed, for three
// integers — in a module the server embeds in its own binary. A guest has no
// socket to open (ADR-0058 decision 5), so the package it would be importing is
// one it could never use, which is what the doc comment above already claims.
const (
	methodPost         = "POST"
	statusUnauthorized = 401
	statusNotFound     = 404
)

// errNoRecord is TheTVDB answering "I have no such record" — a 404, an empty
// series search, or an episode absent from a series. It never leaves this
// package: Lookup turns it into OutcomeNoMatch, and the cache remembers it as the
// zero result.
var errNoRecord = errors.New("thetvdb has no record for that reference")

// Provider is the TheTVDB Metadata provider. It holds a [pluginsdk.Host], the
// minted bearer token and the response cache; the apikey and the base URL are
// read from Host.Settings() PER CALL, because that is where the host publishes
// them and because a secret is readable only while a call it belongs to is on the
// stack.
type Provider struct {
	host pluginsdk.Host

	// mu guards the two pieces of instance memory. A guest instance is
	// single-threaded and the host serializes calls into it (ADR-0058 decision 7),
	// so this lock is never contended in the sandbox — it is here because the
	// native test host is free to drive the provider from several goroutines, and
	// because the Go provider this ports held the same lock.
	mu    sync.Mutex
	token string            // minted on first use, refreshed once on a 401
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

// result is the slice of a TheTVDB record this provider consumes: the fill-only
// fields. A zero value means "no usable data" (cached as a negative result and
// reported as OutcomeNoMatch by Lookup).
type result struct {
	name     string
	overview string
	genres   []string
	imageURL string
}

// empty reports whether the record carries none of the wanted fields — the
// fill-only supplement has nothing to contribute, so the chain treats it as a
// no-match.
func (r result) empty() bool {
	return r.name == "" && r.overview == "" && len(r.genres) == 0 && r.imageURL == ""
}

// --- Lookup ------------------------------------------------------------------

// Lookup serves the TV kinds (show/season/episode); anything else is
// OutcomeNoMatch with no outbound call. It resolves by a TheTVDB series id when
// present and otherwise by name, then fetches the record. A record with none of
// the wanted fields — or a TheTVDB not-found — is OutcomeNoMatch.
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	s := p.host.Settings()

	var (
		key   string
		fetch func(context.Context) (result, error)
	)
	switch ref.Kind {
	case "show":
		key = "show:" + seriesKey(ref)
		fetch = func(ctx context.Context) (result, error) { return p.showRecord(ctx, s, ref) }
	case "season":
		key = "season:" + seriesKey(ref) + "/" + strconv.Itoa(ref.SeasonNumber)
		fetch = func(ctx context.Context) (result, error) { return p.seasonRecord(ctx, s, ref) }
	case "episode":
		key = "episode:" + seriesKey(ref) + "/" + strconv.Itoa(ref.SeasonNumber) + "/" + strconv.Itoa(ref.EpisodeNumber)
		fetch = func(ctx context.Context) (result, error) { return p.episodeRecord(ctx, s, ref) }
	default:
		return noMatch(), nil
	}

	r, err := p.result(ctx, key, fetch)
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.LookupResponse{}, err
	}
	if r.empty() {
		return noMatch(), nil
	}
	rec := pluginapi.MetadataRecord{
		Matched:  true,
		Source:   Source,
		Name:     r.name,
		Overview: r.overview,
		Genres:   r.genres,
	}
	if r.imageURL != "" {
		// TheTVDB serves absolute artwork URLs; emit them as-is under the same
		// "poster" role the authoritative source uses for TV posters/stills (the
		// host's artwork merge fills a role that source left empty).
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: r.imageURL})
	}
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: rec}, nil
}

// --- the two calls this provider does not answer ------------------------------

// Search reports that TheTVDB is not a searchable authoritative source. The
// manifest declares no `search` capability, so the host never asks — this is the
// SDK-side half of the same statement.
func (p *Provider) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// ArtworkCandidates reports that TheTVDB owns no listable image set: it only
// fills a role the authoritative source left empty (ADR-0019). The manifest
// declares no `artwork-candidates` capability, so the host never asks.
func (p *Provider) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
}

// --- the records --------------------------------------------------------------

// seriesKey is the cache discriminator for the series a ref points at: its
// TheTVDB id when present, else the lowercased title. It keeps by-id and by-name
// lookups of the same show distinct in the cache.
func seriesKey(ref pluginapi.MediaRef) string {
	if id := strings.TrimSpace(ref.TheTVDBID); id != "" {
		return "id=" + id
	}
	return "name=" + strings.ToLower(strings.TrimSpace(ref.Title))
}

// result resolves a lookup key to a parsed TheTVDB record, serving from the
// instance response cache when possible (re-enrichment doesn't re-hit the host)
// and otherwise running the kind's fetch. A no-match is cached as the zero value;
// a failure is not cached.
func (p *Provider) result(ctx context.Context, key string, fetch func(context.Context) (result, error)) (result, error) {
	if r, ok := p.cached(key); ok {
		return r, nil
	}
	r, err := fetch(ctx)
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

// seriesID resolves the TheTVDB series id for a ref: the id it carries, otherwise
// the top hit of a series search by title. A search with no results is
// errNoRecord.
func (p *Provider) seriesID(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (string, error) {
	if id := strings.TrimSpace(ref.TheTVDBID); id != "" {
		return id, nil
	}
	title := strings.TrimSpace(ref.Title)
	if title == "" {
		return "", errNoRecord // nothing to resolve a series by
	}
	q := url.Values{}
	q.Set("query", title)
	q.Set("type", "series")
	var out struct {
		Data []struct {
			TVDBID string `json:"tvdb_id"`
		} `json:"data"`
	}
	if err := p.getJSON(ctx, s, "/search", q, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 || strings.TrimSpace(out.Data[0].TVDBID) == "" {
		return "", errNoRecord
	}
	return strings.TrimSpace(out.Data[0].TVDBID), nil
}

// series is the subset of a TheTVDB series record this provider consumes.
type series struct {
	Name     string `json:"name"`
	Overview string `json:"overview"`
	Image    string `json:"image"`
	Genres   []tag  `json:"genres"`
}

type tag struct {
	Name string `json:"name"`
}

// showRecord fetches the series record for a show ref
// (title/overview/genres/poster).
func (p *Provider) showRecord(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (result, error) {
	id, err := p.seriesID(ctx, s, ref)
	if err != nil {
		return result{}, err
	}
	var out struct {
		Data series `json:"data"`
	}
	if err := p.getJSON(ctx, s, "/series/"+url.PathEscape(id), nil, &out); err != nil {
		return result{}, err
	}
	return result{
		name:     field(out.Data.Name),
		overview: field(out.Data.Overview),
		genres:   genres(out.Data.Genres),
		imageURL: field(out.Data.Image),
	}, nil
}

// seasonRecord fetches the season record under a show (overview/poster).
func (p *Provider) seasonRecord(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (result, error) {
	id, err := p.seriesID(ctx, s, ref)
	if err != nil {
		return result{}, err
	}
	var out struct {
		Data struct {
			Overview string `json:"overview"`
			Image    string `json:"image"`
		} `json:"data"`
	}
	path := "/series/" + url.PathEscape(id) + "/seasons/" + strconv.Itoa(ref.SeasonNumber)
	if err := p.getJSON(ctx, s, path, nil, &out); err != nil {
		return result{}, err
	}
	return result{
		overview: field(out.Data.Overview),
		imageURL: field(out.Data.Image),
	}, nil
}

// episodeRecord resolves the series then finds the matching episode by season +
// number (name/overview/still). An episode absent from the series is errNoRecord.
func (p *Provider) episodeRecord(ctx context.Context, s pluginapi.Settings, ref pluginapi.MediaRef) (result, error) {
	id, err := p.seriesID(ctx, s, ref)
	if err != nil {
		return result{}, err
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
	if err := p.getJSON(ctx, s, path, nil, &out); err != nil {
		return result{}, err
	}
	for _, e := range out.Data.Episodes {
		if e.SeasonNumber == ref.SeasonNumber && e.Number == ref.EpisodeNumber {
			return result{
				name:     field(e.Name),
				overview: field(e.Overview),
				imageURL: field(e.Image),
			}, nil
		}
	}
	return result{}, errNoRecord // no episode with that season/number
}

// field normalizes a single TheTVDB text field: an absent value can come back as
// the literal "N/A" or empty, both of which must be treated as empty (never
// written over the authoritative source's value).
func field(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "N/A") {
		return ""
	}
	return v
}

// genres maps a TheTVDB genres block to a slice, dropping empty/"N/A" entries.
func genres(in []tag) []string {
	var out []string
	for _, g := range in {
		if n := field(g.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// --- the login dance ----------------------------------------------------------

// ensureToken returns a bearer token, minting one via login on first use. A
// concurrent double-login is harmless (both store an equivalent token).
func (p *Provider) ensureToken(ctx context.Context, s pluginapi.Settings) (string, error) {
	p.mu.Lock()
	tok := p.token
	p.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	tok, err := p.login(ctx, s)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.token = tok
	p.mu.Unlock()
	return tok, nil
}

// clearToken drops the cached token if it is still the one that just failed, so a
// concurrent successful re-login isn't thrown away.
func (p *Provider) clearToken(stale string) {
	p.mu.Lock()
	if p.token == stale {
		p.token = ""
	}
	p.mu.Unlock()
}

// login exchanges the apikey for a bearer token via TheTVDB's /login endpoint. A
// retryable status is a fetch error the caller answers unavailable to; a rejected
// key stays a Go error.
func (p *Provider) login(ctx context.Context, s pluginapi.Settings) (string, error) {
	body, err := json.Marshal(map[string]string{"apikey": s.Secret})
	if err != nil {
		return "", fmt.Errorf("encoding the thetvdb login: %w", err)
	}
	target := strings.TrimRight(s.URL, "/") + "/login"
	resp, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{
		Method: methodPost,
		URL:    target,
		Headers: []pluginapi.FetchHeader{
			pluginsdk.Header("Content-Type", "application/json"),
			pluginsdk.Header("Accept", "application/json"),
		},
		Body: body,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return "", &pluginsdk.FetchError{URL: target, Status: resp.Status, Decode: err}
	}
	if strings.TrimSpace(out.Data.Token) == "" {
		return "", fmt.Errorf("the thetvdb login returned no token")
	}
	return out.Data.Token, nil
}

// --- the one way out ----------------------------------------------------------

// getJSON issues one authed GET against the operator's TheTVDB base URL and
// decodes the JSON body into out.
//
// A 404 — TheTVDB's "no record" answer — is errNoRecord. A 401 clears the cached
// token and retries ONCE with a fresh login, which is the stale-token case and
// the only status this provider reads for itself. Everything else is left to
// [pluginsdk.Unavailable] at the call's edge: a refusal, a transport failure or a
// 408/429/5xx becomes OutcomeUnavailable, and a 403 or an unreadable document
// stays a Go error.
func (p *Provider) getJSON(ctx context.Context, s pluginapi.Settings, path string, q url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := p.ensureToken(ctx, s)
		if err != nil {
			return err
		}
		target := pluginsdk.URLWithQuery(strings.TrimRight(s.URL, "/")+path, q)
		resp, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{
			URL: target,
			Headers: []pluginapi.FetchHeader{
				pluginsdk.Header("Accept", "application/json"),
				pluginsdk.Header("Authorization", "Bearer "+tok),
			},
		})
		if err != nil {
			var fe *pluginsdk.FetchError
			if errors.As(err, &fe) && fe.Refused == "" && fe.Transport == "" {
				switch fe.Status {
				case statusUnauthorized:
					if attempt == 0 {
						p.clearToken(tok) // stale token — drop it and retry with a fresh login
						continue
					}
				case statusNotFound:
					return errNoRecord // unknown id — the normal "no record" outcome
				}
			}
			return err
		}
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return &pluginsdk.FetchError{URL: target, Status: resp.Status, Decode: err}
		}
		return nil
	}
	return fmt.Errorf("thetvdb %s: unauthorized after a token refresh", path)
}

func noMatch() pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}
}
