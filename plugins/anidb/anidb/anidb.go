// Package anidb is the Obelo Metadata provider for AniDB, as a plugin (ADR-0059,
// .scratch/bundled-plugins issue 05).
//
// It is a port and not a rewrite. The request, the query parameters, the XML
// shape, the title-preference rule, the start-date year parse, the tag-to-genre
// mapping, the cover-art URL, the negative cache and the two-second pacing all
// came from internal/enrich/anidb.go, which this file replaces — the same request
// goes out and the same record comes back, which is the whole claim the
// conversion makes. What changed is where the bytes come from: there is no
// net/http here, no client, no timeout and no socket. The provider holds a
// [pluginsdk.Host] and asks it, so the same code runs inside the WebAssembly
// sandbox and in a native `go test` against an in-memory host.
//
// AniDB is the anime-specialist Full video provider (ADR-0027): an
// enrichment-only source a Library can LEAD with, so anime titles are described
// by a source that understands them. It NEVER affects identity or Watch state
// (ADR-0002/0014) — its ExternalID is the resolved AniDB anime id and nothing
// more. AniDB ids are not naming-convention-derived, so this provider resolves BY
// a pinned anime id (MediaRef.AniDBID, set by a Fix-info Enrichment override); a
// name-based match against the offline titles dump is a deliberate future
// concern, so a lookup with no anime id is a normal OutcomeNoMatch.
//
// The AniDB HTTP API is keyed by a registered CLIENT NAME rather than by a secret
// token; the manifest declares `requiresSecret` and the client name arrives as
// Settings.Secret, which is what made AniDB selectable only once configured
// before this port and is what does so now.
//
// # encoding/xml under wasip1
//
// This is the one bundled provider that parses XML, and it does it with the
// standard library's encoding/xml under GOOS=wasip1 GOARCH=wasm with stock Go —
// reflection, struct tags, `titles>title` paths and all. That is the fact
// ADR-0059 decision 10 rests on: TinyGo was rejected partly because its
// reflection limits would be met here mid-port, and stock Go has no such limit.
//
// # The base URL is plain http on an explicit port
//
// `http://api.anidb.net:9001/httpapi` is what AniDB publishes, and it is a
// legitimate manifest host rather than anything private. The host's fetch check
// accepts it as written: it admits `http` alongside `https`, and both the
// allowlist and the operator-URL comparison match on the HOSTNAME with the port
// stripped, so `api.anidb.net` in `network.hosts` covers `:9001`.
//
// # Where an error goes
//
// [pluginsdk.Unavailable] draws the line and this package does not restate it. A
// host refusal, a spent call budget, a transport failure or a 408/429/5xx is
// OutcomeUnavailable with the reason in Detail — never OutcomeNoMatch (a claim
// about the item) and never a Go error (a strike against the plugin, three of
// which disable it). AniDB's own <error>…unknown…</error> payload IS a claim
// about the item, so it is OutcomeNoMatch and is cached as one; any other <error>
// body — a banned or invalid client — stays a Go error, as do a 401, a 403 and a
// document this code cannot read, so the item is parked where an operator will
// see it.
package anidb

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the slug this provider stamps on every record and artwork candidate
// it returns. It is the manifest id.
const Source = "anidb"

// ImageBaseURL is the public AniDB cover-art host; a <picture> filename in the
// anime record is resolved against it into a poster URL. The HOST downloads that
// URL through its own guarded fetcher, so this is a string this plugin builds and
// never a host it fetches from — which is why it is not in the manifest's
// allowlist.
const ImageBaseURL = "https://cdn.anidb.net/images/main"

// ClientVer is the HTTP-API client version this plugin advertises (protover 1).
// AniDB requires client + clientver + protover on every request.
const ClientVer = "1"

// DefaultThrottle spaces successive AniDB requests: AniDB bans bursty clients, so
// a large-library pass is a polite trickle. It is the interval main.go hands to
// [pluginsdk.PacedHost], and it is the number the Go provider used.
const DefaultThrottle = 2 * time.Second

// errNoRecord is AniDB answering "I have no such anime" — its
// <error>…unknown…</error> payload. It never leaves this package: the callers
// turn it into OutcomeNoMatch, and the cache remembers it as the zero result.
var errNoRecord = errors.New("anidb has no record for that anime id")

// Provider is the AniDB Metadata provider. It holds a [pluginsdk.Host] and the
// response cache; the client name, the base URL and the metadata language are
// read from Host.Settings() PER CALL, because that is where the host publishes
// them and because a credential is readable only while a call it belongs to is on
// the stack.
type Provider struct {
	host pluginsdk.Host

	// mu guards the cache. A guest instance is single-threaded and the host
	// serializes every call into it (ADR-0058 decision 7), so this lock is never
	// contended in the sandbox — it is here because the native test host is free to
	// drive the provider from several goroutines, and because the Go provider this
	// ports held the same lock.
	mu    sync.Mutex
	cache map[string]result // aid -> parsed record (zero value = looked up, no data)
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

// result is the slice of an AniDB anime record this provider consumes. A zero
// value means "no usable data" (cached negative; reported as OutcomeNoMatch).
type result struct {
	title    string
	overview string
	year     int
	genres   []string
	poster   string // absolute cover-art URL, "" when none
}

func (r result) empty() bool {
	return r.title == "" && r.overview == "" && r.poster == "" && len(r.genres) == 0
}

// --- Lookup ------------------------------------------------------------------

// Lookup resolves a video Title BY its pinned AniDB anime id and returns the
// enrichment-only descriptive fields. A non-video kind, or a ref with no AniDB
// id, is OutcomeNoMatch without an outbound call (the honest outcome until
// name-based matching exists) — so an AniDB-led chain leaves an unpinned Title
// unmatched rather than guessing, never touching identity. A record with none of
// the wanted fields is likewise OutcomeNoMatch.
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	switch ref.Kind {
	case "movie", "show", "season", "episode":
	default:
		return noMatch(), nil // AniDB serves the video kinds only
	}
	aid := strings.TrimSpace(ref.AniDBID)
	if aid == "" {
		return noMatch(), nil // AniDB ids are not naming-derived
	}
	s := p.host.Settings()

	r, err := p.result(ctx, s, aid)
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
		Matched:    true,
		Source:     Source,
		Name:       r.title,
		Year:       r.year,
		Overview:   r.overview,
		Genres:     r.genres,
		ExternalID: aid,
	}
	if r.poster != "" {
		rec.Artwork = []pluginapi.ArtworkRef{{Role: "poster", URL: r.poster}}
	}
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: rec}, nil
}

// --- Search ------------------------------------------------------------------

// Search returns no candidates: AniDB's HTTP API offers no free-text title search
// (matching would need the offline titles dump — out of scope), so the Edit-item
// picker for an AniDB-led kind simply finds none rather than hanging.
//
// The manifest DECLARES `search` even though there is nothing to search, and that
// is deliberate: the declaration says "ask me and I will answer", and this empty
// candidate list renders as "no results", where an undeclared capability renders
// as "search unavailable". Those are different sentences, and an anime Library
// leading with AniDB sees the right one.
func (p *Provider) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

// --- ArtworkCandidates --------------------------------------------------------

// ArtworkCandidates lists the anime's cover art (one poster) for the "poster"
// role when the ref carries a resolved anime id; every other role/kind yields no
// candidates and makes no call.
func (p *Provider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	aid := strings.TrimSpace(req.Ref.AniDBID)
	if req.Role != "poster" || aid == "" {
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	s := p.host.Settings()

	r, err := p.result(ctx, s, aid)
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	if r.poster == "" {
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	return pluginapi.ArtworkCandidatesResponse{
		Outcome:    pluginapi.OutcomeMatched,
		Candidates: []pluginapi.ArtworkCandidate{{URL: r.poster, Source: Source}},
	}, nil
}

// --- the records --------------------------------------------------------------

// result resolves an anime id to a parsed record, serving from the instance cache
// (re-enrichment doesn't re-hit AniDB) and otherwise fetching. A no-match is
// cached as the zero value; a failure is not cached.
func (p *Provider) result(ctx context.Context, s pluginapi.Settings, aid string) (result, error) {
	if r, ok := p.cached(aid); ok {
		return r, nil
	}
	r, err := p.fetch(ctx, s, aid)
	switch {
	case errors.Is(err, errNoRecord):
		p.store(aid, result{})
		return result{}, nil
	case err != nil:
		return result{}, err
	}
	p.store(aid, r)
	return r, nil
}

func (p *Provider) cached(aid string) (result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.cache[aid]
	return r, ok
}

func (p *Provider) store(aid string, r result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = map[string]result{}
	}
	p.cache[aid] = r
}

// anime is the slice of the AniDB HTTP-API XML this provider parses. AniDB
// returns an <anime> root on success and an <error> root for an unknown aid or a
// banned/invalid client, so the struct pins NO root name (XMLName captures which
// it was) and RootText collects the root's character data — the error message when
// the root is <error>.
type anime struct {
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
// AniDB's <error> payload is errNoRecord for an unknown-aid message and a Go
// error otherwise (a banned or invalid client is the operator's to fix).
func (p *Provider) fetch(ctx context.Context, s pluginapi.Settings, aid string) (result, error) {
	q := url.Values{}
	q.Set("request", "anime")
	q.Set("client", s.Secret)
	q.Set("clientver", ClientVer)
	q.Set("protover", "1")
	q.Set("aid", aid)
	// The Go provider spelled this base + "?" + query with no path appended, and a
	// trailing slash on the base was trimmed. Keep both, so an operator's mirror URL
	// reaches the same path it always did.
	target := pluginsdk.URLWithQuery(strings.TrimRight(s.URL, "/"), q)

	resp, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{URL: target})
	if err != nil {
		return result{}, err
	}
	var a anime
	if err := xml.Unmarshal(resp.Body, &a); err != nil {
		return result{}, &pluginsdk.FetchError{URL: target, Status: resp.Status, Decode: err}
	}
	if a.XMLName.Local == "error" {
		msg := strings.TrimSpace(a.RootText)
		if strings.Contains(strings.ToLower(msg), "unknown") {
			return result{}, errNoRecord // no anime with that id
		}
		return result{}, fmt.Errorf("anidb: %s", msg)
	}
	return toResult(a, s.Language), nil
}

// toResult normalizes a parsed anime record into the fields we consume,
// preferring the operator's language for the display title with sensible
// fallbacks (the English "main" title, then any title).
func toResult(a anime, language string) result {
	r := result{
		overview: strings.TrimSpace(a.Description),
	}
	// Title preference: the configured language's "main"/"official", then any
	// "main", then the first title present.
	lang := strings.ToLower(strings.SplitN(language, "-", 2)[0])
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
		if y := ParseYear(a.StartDate[:4]); y > 0 {
			r.year = y
		}
	}
	for _, t := range a.Tags {
		if name := strings.TrimSpace(t.Name); name != "" {
			r.genres = append(r.genres, name)
		}
	}
	if pic := strings.TrimSpace(a.Picture); pic != "" {
		r.poster = ImageBaseURL + "/" + pic
	}
	return r
}

// ParseYear parses a 4-digit year, returning 0 when it is not a plausible year.
//
// Exported so a test can state the rule directly; the provider is the only
// caller.
func ParseYear(s string) int {
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

func noMatch() pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}
}
