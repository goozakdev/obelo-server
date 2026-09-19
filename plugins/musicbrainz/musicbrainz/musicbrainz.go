// Package musicbrainz is the Obelo Metadata provider for MusicBrainz — with the
// Cover Art Archive as its second host — as a plugin (ADR-0059,
// .scratch/bundled-plugins issue 06).
//
// It is a port and not a rewrite. Every endpoint, every query parameter, every
// `inc` list, every JSON shape and every mapping below came from
// internal/enrich/musicbrainz.go, which this package replaces: the same requests
// go out and the same records come back, which is the whole claim the conversion
// makes. What changed is where the bytes come from. There is no net/http here, no
// client, no timeout and no socket — the provider holds a [pluginsdk.Host] and
// asks it, so the same code runs inside the WebAssembly sandbox and in a native
// `go test` against an in-memory host.
//
// # The two hosts
//
// The MusicBrainz web service is Settings.URL; the COVER ART ARCHIVE is
// Settings.URL2, and it is the second host exactly as TMDB's image CDN is. That
// is the substantive change this port carries for an operator: the Cover Art
// Archive used to be a provider ROW of its own with no client behind it, and the
// server resolved that row into this source's second URL through a special case
// in three different files. Now the manifest declares
// `settings.defaultUrl2: https://coverartarchive.org` and the special cases are
// gone. Every place the Go provider built a cover URL or fetched the CAA manifest
// reads URL2 here, character for character.
//
// # Pacing
//
// MusicBrainz allows roughly one request a second and answers 503 when you
// exceed it. The Go provider paced itself with a PROCESS-WIDE limiter keyed by
// host, because a provider instance per Library meant three Libraries sent three
// requests a second while each believed it was well behaved (ADR-0049). A guest
// needs none of that machinery: one module, one linear memory, every call
// serialized into it by the host (ADR-0058 decision 7), and one
// [pluginsdk.PacedHost] wrapped around its Fetch in main.go. [DefaultInterval] is
// this plugin's own policy and Settings.RateLimitMillis is the operator's
// override.
//
// # The 503 ladder, and the budget it now runs inside
//
// The in-request retry is unchanged and is deliberately narrow: a refusal is
// retried HERE only when waiting a second or two can plausibly change the answer,
// which means only when MusicBrainz named a Retry-After or when its own headers
// say the refusal is about THIS server's usage. A global load shed is handed
// straight back to the pass, whose cross-pass backoff is measured in minutes
// (ADR-0048) and is the mechanism that actually recovers it.
//
// What is new is that a call now has a BUDGET (ADR-0059 decision 6). Every
// exported call bounds itself at [callBudget], and the ladder waits only while
// that budget leaves room for the request the wait is for. When it does not, the
// answer is `unavailable` — never a Go error, see below.
//
// # Where an error goes
//
// Two different things can go wrong and they are answered differently, because
// the host does two different things with them. [pluginsdk.Unavailable] draws the
// line and this package does not restate it:
//
//   - "MusicBrainz could not answer right now" — the host refused the fetch, the
//     fetch ran out of the call's budget, or the source answered 408, 429 or a
//     5xx the ladder gave up on. None of that is an answer about the ITEM, so it
//     is OutcomeUnavailable with the reason in Detail, NEVER OutcomeNoMatch and
//     never a Go error. The item takes ADR-0048's backoff and no failure is
//     counted against the plugin — which matters more than it looks: a Go error
//     IS a strike, and three consecutive ones disable the plugin, so one bad
//     afternoon at a source would otherwise take the whole provider off the
//     server.
//   - "MusicBrainz answered, and the answer is no use" — a 400, a 403, or a
//     document this code cannot read. Those describe OUR REQUEST, asking again
//     changes nothing, and they stay Go errors so the item is parked where an
//     operator will see it, exactly as the Go provider left it.
//
// A 404 is neither: it is the definitive "no such record" the Go provider mapped
// to ErrNoMatch, and it is OutcomeNoMatch here.
//
// # Nothing here judges a title
//
// ADR-0050's acceptance test left this source in plugin-system issue 01 and the
// port did not bring it back: a search hit comes back marked FromSearch and the
// HOST decides whether it is this record. The one title comparison that remains —
// in [Provider.artistIDFromAlbumSearch] — is not that rule and is explained where
// it stands: nothing it looks at becomes a record, and the candidate title never
// leaves the function.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the slug this provider stamps on every record it returns. It is the
// manifest id, and the host stores pins under it.
const Source = "musicbrainz"

// CoverArtSource is the slug stamped on an artwork candidate that came from the
// Cover Art Archive. It is NOT this plugin's id, and that is on purpose: it is
// the string the Go provider wrote, the host's artwork rows carry it, and
// changing it would rename every stored candidate's source.
const CoverArtSource = "coverartarchive"

// DefaultInterval is this plugin's own pacing policy: MusicBrainz allows about one
// request a second and answers 503 past it. main.go hands it to
// [pluginsdk.PacedHost], which honours Settings.RateLimitMillis over it when the
// operator set one (ADR-0049, ADR-0059 decision 5).
const DefaultInterval = time.Second

// defaultRetryBackoff is the base 503 delay when the response carries no
// Retry-After. One second matches the public host's ~1 req/sec policy, and is a
// floor a mirror inherits too: a mirror that answers 503 wants a pause even
// though it set no rate policy for the steady state.
//
// It is deliberately INDEPENDENT of the pacing interval. A 503 is the host
// telling us to back off, which it means whether or not we opted into
// client-side pacing. Deriving the delay from the interval (as this once did)
// meant an interval of 0 — the documented setting for a mirror with no rate
// policy — computed a delay of zero and burst all four attempts back-to-back,
// turning the one signal a struggling host can send into a hammering.
const defaultRetryBackoff = time.Second

// retryBackoffBase is [defaultRetryBackoff], in a variable for ONE reason: this
// package's own tests would otherwise spend real seconds proving that a ladder
// waits. Nothing else assigns it — the Go provider had the same seam as a struct
// field whose doc said "only a test sets it low", and this is that field with one
// fewer way to get it wrong from outside.
var retryBackoffBase = defaultRetryBackoff

// maxAttempts is how many times one request is made inside one lookup before the
// ladder gives up, unchanged from the Go provider.
const maxAttempts = 4

// callBudget is how long one call into this plugin may take, and it is the
// manifest's `callBudgetMillis` (90 s) less a margin.
//
// The margin is the point. The HOST enforces the budget by unwinding the guest,
// and an unwound call is a STRIKE against the plugin — so a plugin that sleeps up
// to its host's exact deadline is a plugin that gets disabled for being slow at a
// slow source. Bounding every call two seconds inside the host's own figure means
// this code always stops and ANSWERS (`unavailable`), which is what ADR-0059
// decision 6 asks for.
//
// WHY 90 SECONDS AND NOT THE HOST'S 30-SECOND DEFAULT. This source is paced at one
// request a second, and one call can legitimately make a dozen: an album SEARCH
// fetches a tracklist preview per candidate, and the cascade's album search asks
// for MusicBrainz's own default page of 25. Twenty-six paced requests is
// twenty-six seconds before a single byte of latency, so the default budget would
// have turned a search that works today into an `unavailable` — a behaviour change
// smuggled in with a port whose whole claim is that there is none. The manifest
// raises it instead; the host's cap is 120 s, so nothing is clamped.
const callBudget = 88 * time.Second

// releaseBrowseLimit caps the release browse behind fit-selection. MusicBrainz's
// browse default is 25 and its maximum is 100; a release-group with more than a
// hundred editions is a compilation nobody rips at home, and paging past the first
// hundred to find a better fit is not worth a second call at a rate-limited host.
const releaseBrowseLimit = 100

// maxRefusalBody is how much of a refusal's body is read to quote in the log.
// Enough for any provider's one-line JSON error, small enough that a
// misconfigured host serving an HTML page cannot flood the log.
const maxRefusalBody = 512

// errNoMatch is this package's internal "the source has no such record". It never
// crosses the contract — every call path turns it into OutcomeNoMatch — and it
// exists so the porting of the Go provider's `ErrNoMatch` control flow is
// line-for-line rather than reimagined.
var errNoMatch = errors.New("musicbrainz: no such record")

// errNoTracklist is the internal twin of errNoMatch for the tracklist calls: "this
// album has no tracklist", which the contract spells OutcomeNoMatch
// (see pluginapi.TracklistResponse for why the two are not collapsible).
var errNoTracklist = errors.New("musicbrainz: this album has no tracklist")

// Provider is the MusicBrainz Metadata provider. It holds a [pluginsdk.Host] and
// nothing else: both base URLs and the operator's pacing are read from
// Host.Settings() PER CALL, because that is where the host publishes them.
type Provider struct {
	host pluginsdk.Host
}

// The contract interfaces this provider fills. All four declared capabilities are
// implemented here; the SDK routes each export to the matching interface.
var (
	_ pluginapi.MetadataProvider  = (*Provider)(nil)
	_ pluginapi.AlbumTracklister  = (*Provider)(nil)
	_ pluginapi.ExternalRefParser = (*Provider)(nil)
)

// New builds the provider on a Host. In the sandbox that Host is
// pluginsdk.PacedHost(pluginsdk.Sandbox(), DefaultInterval); in a test it is
// sdktest.New(...). The provider cannot tell.
func New(h pluginsdk.Host) *Provider { return &Provider{host: h} }

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// bounded gives this call its budget. See [callBudget]: the host has one and
// enforces it by unwinding the guest, so the plugin keeps its own, slightly
// shorter, and answers rather than being killed.
//
// A context that ALREADY carries a deadline is left alone — that is a native test
// (or a future host that passes one through), and the caller's deadline is the
// truthful one. In the sandbox the context is context.Background(), so this is
// where a MusicBrainz call learns it is not allowed to take all afternoon.
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, callBudget)
}

// --- Lookup ------------------------------------------------------------------

// Lookup resolves ref to MusicBrainz metadata, dispatching by kind. Non-Music
// kinds are OutcomeNoMatch (the TMDB plugin serves the video kinds).
//
// A pinned id always wins over a name, on every kind: an id IS the identification
// (ADR-0049), and an applied Enrichment override has to survive a re-enrich
// (ADR-0019 durability). A pinned RELEASE on an album resolves to its parent
// release-group, because a release-group is what an album is (ADR-0038).
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	ref := req.Ref
	s := p.host.Settings()

	switch ref.Kind {
	case "artist":
		// A pinned artist MBID (an applied Enrichment override) resolves BY id so a
		// re-enrich or later pass looks up the exact artist the Admin picked instead
		// of re-searching by name.
		if id := ref.ID(pluginapi.NamespaceMusicBrainz); id != "" {
			return lookupResult(p.artistByID(ctx, s, id))
		}
		return lookupResult(p.artistDetails(ctx, s, ref.Title, ref.AlbumHints))
	case "album":
		// A pinned MusicBrainz *release* (one edition) resolves to its parent
		// release-group — the album we actually pin — so a pasted /release/ URL works.
		if id := strings.TrimSpace(ref.ReleaseMBID); id != "" {
			return lookupResult(p.releaseGroupForRelease(ctx, s, id))
		}
		// A pinned release-group MBID resolves BY id (the durable album override).
		if id := ref.ID(pluginapi.NamespaceMusicBrainz); id != "" {
			return lookupResult(p.releaseGroupByID(ctx, s, id))
		}
		return lookupResult(p.albumDetails(ctx, s, ref.Album, ref.Artist))
	case "track":
		// A pinned recording MBID (an applied Enrichment override) resolves BY id so
		// a re-enrich or later pass looks up the exact record the Admin picked instead
		// of re-searching by name (ADR-0019 durability). No id falls back to the
		// name+artist search.
		if id := ref.ID(pluginapi.NamespaceMusicBrainz); id != "" {
			return lookupResult(p.recordingByID(ctx, s, id))
		}
		return lookupResult(p.trackDetails(ctx, s, ref.Track, ref.Artist))
	default:
		return noMatch(), nil
	}
}

// --- Search ------------------------------------------------------------------

// Search returns MusicBrainz candidates for a free-text query, dispatching by
// kind: a Track (recording) search hits /recording; an Artist search /artist; an
// Album (release-group) search /release-group — the leaves + browse parents a
// Music Enrichment override corrects (ADR-0019). Each candidate carries the MBID
// to pin, a title/name, a disambiguation hint (the "wrong Nirvana" tell), and — for
// an album — its tracklist preview. A blank query yields no candidates; an
// unsupported kind is OutcomeUnavailable, which is the Edit-item box's "this kind
// cannot be searched right now".
func (p *Provider) Search(ctx context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	ctx, cancel := bounded(ctx)
	defer cancel()

	s := p.host.Settings()

	var (
		cands []pluginapi.SearchCandidate
		err   error
	)
	switch req.Kind {
	case "track":
		cands, err = p.searchRecordings(ctx, s, req)
	case "artist":
		cands, err = p.searchArtists(ctx, s, req)
	case "album":
		cands, err = p.searchReleaseGroups(ctx, s, req)
	default:
		return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	if err != nil {
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		// A 404 REACHES HERE AS A GO ERROR, deliberately, and that is what the Go
		// provider did too. A search index does not 404 for an unknown record — it
		// answers an empty list — so a 404 on this path means the REQUEST is wrong:
		// an operator's mirror whose base URL has the wrong prefix, where every
		// endpoint 404s. Answering OutcomeMatched with no candidates would render
		// that as "no results", which is a lie about the library and hides a
		// misconfiguration only the operator can fix.
		return pluginapi.SearchResponse{}, err
	}
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// musicQuery builds the MusicBrainz search query from the user's terms. It is the
// core fix for item-editing/search-improvements: the terms are Lucene-ESCAPED (so
// metacharacters in `AC/DC`, `!!!`, `"Heroes"` can't 4xx the parser) but NOT wrapped
// in a `field:"…"` exact-phrase. Phrase-quoting demanded every descriptor word be
// present and adjacent in the title field, so a query carrying a type word
// ("Soundtrack", "Deluxe Edition", "OST", "Disc 1") or a different word order matched
// zero records — e.g. `releasegroup:"Anastasia Soundtrack"` found nothing because the
// canonical release-group title is just "Anastasia" (an Album with secondary-type
// Soundtrack). Unscoped relevance-ranked terms let MusicBrainz score the right record
// to the top instead. Optional artist and release terms are AND-ed in as field-scoped
// clauses — the verified relevance-safe narrowing pattern — to focus a broad common
// title. A blank term adds no clause, so the common query is unchanged.
//
// `release` narrows a RECORDING search to the album the recording sits on
// (needs-fixing/06), and it is the difference between one answer and a page of noise:
// `Whisper Your Name AND artist:"Harry Connick"` returns nine recordings across
// soundtracks and promos, and the same query with `AND release:"She"` returns exactly
// the one on that album. Only the recording index has the field; the release-group
// and artist searches pass "" for it.
//
// The artist clause is ARTICLE-INSENSITIVE (ADR-0037's amendment). See artistClause;
// the `release` clause deliberately gets no such treatment, because an album's leading
// article is usually part of its title.
func musicQuery(terms, artist, release string) string {
	q := escapeLucene(terms)
	if a := strings.TrimSpace(artist); a != "" {
		q += ` AND artist:` + artistClause(a)
	}
	if r := strings.TrimSpace(release); r != "" {
		q += ` AND release:"` + escapeLucene(r) + `"`
	}
	return q
}

// artistClause builds the artist-narrowing clause article-insensitively, because
// ADR-0037 made a leading article irrelevant to how Obelo IDENTIFIES an Artist and
// the provider query never got the rule. An operator whose files say "The Eagles"
// got nothing from an album MusicBrainz credits to "Eagles":
//
//	release-group?query=Hell Freezes Over AND artist:"The Eagles"                → 0
//	release-group?query=Hell Freezes Over AND artist:("The Eagles" OR "Eagles")  → 3
//
// The mechanism is that `artist:"…"` is a Lucene PHRASE query over the analyzed
// artist-credit field, so its tokens must appear adjacent in the credit. A tagged
// article adds a token the credit does not have and the match is lost.
//
// Hence the alternatives: the name as the Admin typed it, OR the same name with a
// leading English article removed. Keeping the as-typed spelling is not redundant —
// a credit matching both alternatives scores higher, so "The Eagles" still outranks
// "Eagles" for an artist genuinely named with the article.
//
// THE OTHER DIRECTION IS DELIBERATELY ABSENT, against the letter of the amendment,
// because phrase matching already covers it and the evidence for it was misread.
// Verified live 2026-09-02:
//
//	Disintegration AND artist:"Cure"          → 4, top credited "The Cure"
//	Different Light AND artist:"The Bangles"  → 0   (MusicBrainz credits "Bangles")
//	Different Light AND artist:"Bangles"      → 2, the album
//
// A one-token phrase matches anywhere inside the credit, so `artist:"X"` already
// finds a credit spelled "The X"; the match set of `artist:"The X"` is a SUBSET of
// `artist:"X"`'s, and OR-ing it in cannot return one extra row. It would, however,
// rewrite the URL of every article-less artist in the library — which is why the
// alternatives collapsing to one emits today's plain single-phrase clause, byte for
// byte. Issue 15's Bangles case reads as the reverse direction but is not one: that
// release-group is credited "Bangles", and the OR query's two hits come entirely
// from the bare alternative, which today's clause already sends.
func artistClause(artist string) string {
	alts := []string{artist}
	if bare := withoutLeadingArticle(artist); bare != "" && bare != artist {
		alts = append(alts, bare)
	}
	if len(alts) == 1 {
		return `"` + escapeLucene(alts[0]) + `"`
	}
	quoted := make([]string, 0, len(alts))
	for _, a := range alts {
		quoted = append(quoted, `"`+escapeLucene(a)+`"`)
	}
	return "(" + strings.Join(quoted, " OR ") + ")"
}

// queryArticles are leading words dropped from an artist-narrowing alternative,
// longest first so a more specific prefix wins. Each carries its trailing space, so
// a bare article ("The") and words that merely begin with those letters ("Anthrax")
// are untouched. The list mirrors scanner.sortArticles, which serves identity keys
// on already-lower-cased text; this one matches case-insensitively because it runs
// on the name as the Admin typed it.
var queryArticles = []string{"the ", "an ", "a "}

// withoutLeadingArticle drops one leading English article from a display name,
// returning the name unchanged when it has none.
func withoutLeadingArticle(name string) string {
	for _, article := range queryArticles {
		if len(name) > len(article) && strings.EqualFold(name[:len(article)], article) {
			return strings.TrimSpace(name[len(article):])
		}
	}
	return name
}

// setPaging applies the picker's limit/offset to a search request so a broad
// common-title query can be paged ("show more") instead of only ever returning the
// source's first page. A zero limit/offset leaves the MusicBrainz default.
func setPaging(q url.Values, page pluginapi.Page) {
	if page.Limit > 0 {
		q.Set("limit", strconv.Itoa(page.Limit))
	}
	if page.Offset > 0 {
		q.Set("offset", strconv.Itoa(page.Offset))
	}
}

// typeLabel joins a release-group's primary + secondary types into a short badge
// ("Album · Soundtrack") — the disambiguation tell that separates same-titled hits.
func typeLabel(primary string, secondary []string) string {
	var parts []string
	if strings.TrimSpace(primary) != "" {
		parts = append(parts, primary)
	}
	for _, s := range secondary {
		if strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// searchRecordings serves the Track kind: recordings mapped to candidates carrying
// the recording MBID, title, an artist-credit + disambiguation hint, and a
// best-effort original year.
func (p *Provider) searchRecordings(ctx context.Context, s pluginapi.Settings, req pluginapi.SearchRequest) ([]pluginapi.SearchCandidate, error) {
	q := url.Values{}
	// Relevance-ranked terms (still Lucene-escaped so `AC/DC`, `"Heroes"`, `!!!` can't
	// 4xx the parser), NOT an exact-phrase recording:"…" (item-editing/search-
	// improvements). req.Artist and req.Release AND-narrow to a specific artist and
	// album when supplied — a recording title alone is rarely distinguishing.
	q.Set("query", musicQuery(req.Query, req.Artist, req.Release))
	setPaging(q, req.Page)
	q.Set("fmt", "json")
	var out struct {
		Recordings []struct {
			ID             string     `json:"id"`
			Title          string     `json:"title"`
			Disambiguation string     `json:"disambiguation"`
			FirstReleased  string     `json:"first-release-date"`
			ArtistCredit   []mbCredit `json:"artist-credit"`
			// A recording search hit rarely carries a top-level first-release-date, so
			// the disambiguating year is derived from its releases / release-groups. The
			// release-group title is also the album hint that helps tell same-named
			// recordings apart.
			Releases []struct {
				Date         string `json:"date"`
				ReleaseGroup struct {
					Title            string `json:"title"`
					FirstReleaseDate string `json:"first-release-date"`
				} `json:"release-group"`
			} `json:"releases"`
		} `json:"recordings"`
	}
	if err := p.getJSON(ctx, s, "/recording", q, &out); err != nil {
		return nil, err
	}
	cands := make([]pluginapi.SearchCandidate, 0, len(out.Recordings))
	for _, r := range out.Recordings {
		var hints []string
		// The FULL artist-credit (all collaborators), not just the first.
		if credit := creditString(r.ArtistCredit); credit != "" {
			hints = append(hints, credit)
		}
		if r.Disambiguation != "" {
			hints = append(hints, r.Disambiguation)
		}
		// Best-effort earliest (original) year across the recording's own first-release
		// date and each release's release-group / release date — the most useful year
		// for telling same-named recordings apart. Left 0 when truly absent.
		year := YearFromDate(r.FirstReleased)
		album := ""
		takeEarlier := func(date string) {
			if y := YearFromDate(date); y > 0 && (year == 0 || y < year) {
				year = y
			}
		}
		for _, rel := range r.Releases {
			takeEarlier(rel.ReleaseGroup.FirstReleaseDate)
			takeEarlier(rel.Date)
			if album == "" && rel.ReleaseGroup.Title != "" {
				album = rel.ReleaseGroup.Title
			}
		}
		if album != "" {
			hints = append(hints, "on "+album)
		}
		cands = append(cands, pluginapi.SearchCandidate{
			ExternalID:     r.ID,
			Title:          r.Title,
			Year:           year,
			Disambiguation: strings.Join(hints, " — "),
			Kind:           "track",
		})
	}
	return cands, nil
}

// searchArtists serves the Artist parent kind: MusicBrainz artist search mapped to
// candidates carrying the artist MBID, name, and a type/area/disambiguation hint.
func (p *Provider) searchArtists(ctx context.Context, s pluginapi.Settings, req pluginapi.SearchRequest) ([]pluginapi.SearchCandidate, error) {
	q := url.Values{}
	// Relevance-ranked, Lucene-escaped terms — not an exact artist:"…" phrase — so a
	// name typed with extra words or different order still scores the right artist to
	// the top (item-editing/search-improvements). Artist scoping is N/A here.
	q.Set("query", musicQuery(req.Query, "", ""))
	setPaging(q, req.Page)
	q.Set("fmt", "json")
	var out struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Type           string `json:"type"`
			Disambiguation string `json:"disambiguation"`
			Area           struct {
				Name string `json:"name"`
			} `json:"area"`
		} `json:"artists"`
	}
	if err := p.getJSON(ctx, s, "/artist", q, &out); err != nil {
		return nil, err
	}
	cands := make([]pluginapi.SearchCandidate, 0, len(out.Artists))
	for _, a := range out.Artists {
		// The type ("Group"/"Person") is the record-type badge; the free-text hint
		// carries the disambiguation comment + area/country (the "wrong Nirvana" tell).
		var hints []string
		if a.Disambiguation != "" {
			hints = append(hints, a.Disambiguation)
		}
		if a.Area.Name != "" {
			hints = append(hints, "from "+a.Area.Name)
		}
		cands = append(cands, pluginapi.SearchCandidate{
			ExternalID:     a.ID,
			Title:          a.Name,
			Disambiguation: strings.Join(hints, " — "),
			TypeLabel:      a.Type,
			Kind:           "artist",
		})
	}
	return cands, nil
}

// searchReleaseGroups serves the Album parent kind: release-group search mapped to
// candidates carrying the release-group MBID, title, year, a disambiguation hint,
// a Cover Art thumbnail — off Settings.URL2, the second host — and the tracklist
// preview the positional cascade consumes. A tracklist fetch that fails is
// non-fatal: the candidate is still offered without a preview (ADR-0001).
func (p *Provider) searchReleaseGroups(ctx context.Context, s pluginapi.Settings, req pluginapi.SearchRequest) ([]pluginapi.SearchCandidate, error) {
	q := url.Values{}
	// Relevance-ranked, Lucene-escaped terms — NOT an exact releasegroup:"…" phrase.
	// This is the headline fix (item-editing/search-improvements): the canonical
	// release-group title is often just the name ("Anastasia") with the descriptor a
	// secondary-TYPE (Soundtrack), so a phrase query carrying "Anastasia Soundtrack"
	// matched nothing. req.Artist AND-narrows to a specific artist when supplied;
	// req.Release is dropped, because a release-group search IS the album search —
	// there is no release axis left to narrow on.
	q.Set("query", musicQuery(req.Query, req.Artist, ""))
	setPaging(q, req.Page)
	q.Set("fmt", "json")
	var out struct {
		ReleaseGroups []struct {
			ID               string     `json:"id"`
			Title            string     `json:"title"`
			Disambiguation   string     `json:"disambiguation"`
			FirstReleaseDate string     `json:"first-release-date"`
			PrimaryType      string     `json:"primary-type"`
			SecondaryTypes   []string   `json:"secondary-types"`
			ArtistCredit     []mbCredit `json:"artist-credit"`
		} `json:"release-groups"`
	}
	if err := p.getJSON(ctx, s, "/release-group", q, &out); err != nil {
		return nil, err
	}
	cands := make([]pluginapi.SearchCandidate, 0, len(out.ReleaseGroups))
	for _, rg := range out.ReleaseGroups {
		var hints []string
		// The FULL artist-credit (all collaborators, e.g. "Ben Folds & Nick Hornby"),
		// not just the first — so a multi-artist album is recognizable in the picker.
		if credit := creditString(rg.ArtistCredit); credit != "" {
			hints = append(hints, credit)
		}
		if rg.Disambiguation != "" {
			hints = append(hints, rg.Disambiguation)
		}
		c := pluginapi.SearchCandidate{
			ExternalID:     rg.ID,
			Title:          rg.Title,
			Year:           YearFromDate(rg.FirstReleaseDate),
			ThumbnailURL:   s.URL2 + "/release-group/" + rg.ID + "/front-250",
			Disambiguation: strings.Join(hints, " — "),
			// "Album · Soundtrack" — the type badge that tells the Anastasia soundtrack
			// apart from the many other same-titled "Anastasia" release-groups.
			TypeLabel: typeLabel(rg.PrimaryType, rg.SecondaryTypes),
			Kind:      "album",
		}
		if tl, err := p.releaseGroupTracklist(ctx, s, rg.ID); err == nil {
			c.Tracklist = tl
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// releaseGroupTracklist fetches one release of a release-group and returns its
// ordered tracks (disc + position + title) as the album candidate's PREVIEW.
//
// It keeps taking whichever release MusicBrainz returns first, on purpose. This is
// the search-results path: a page of album candidates each showing a sample of what
// is on them, where roughly right is the whole requirement and paying two calls per
// candidate to find the exact edition would be absurd. The path that needs the
// right edition — one album, resolving its own tracks — is AlbumTracklist
// (ADR-0050), which is a different question and answers it separately.
func (p *Provider) releaseGroupTracklist(ctx context.Context, s pluginapi.Settings, rgID string) ([]pluginapi.TrackCandidate, error) {
	q := url.Values{}
	q.Set("release-group", rgID)
	q.Set("inc", "recordings")
	q.Set("limit", "1")
	q.Set("fmt", "json")
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := p.getJSON(ctx, s, "/release", q, &out); err != nil {
		return nil, err
	}
	if len(out.Releases) == 0 {
		return nil, nil
	}
	return mbTracklist(out.Releases[0].Media), nil
}

// --- AlbumTracklister: the tracklist of the release an album actually is ------

// AlbumTracklist returns the ordered tracks of the release THIS album is, not of
// whichever release the source happens to list first (ADR-0050).
//
// It costs one call for a tagged album and one for an untagged one; two only when
// the tagged release turns out to belong to somebody else. The order is:
//
//  1. No release-group id — the album is unresolved. OutcomeNoMatch, zero calls.
//  2. A release id — ONE /release/<id>?inc=recordings+release-groups, which answers
//     the tracklist and the parent release-group in the call the tracklist needed
//     anyway. The parent check is therefore free, and it is what stops a mis-tagged
//     file (or a stale pin) naming a stranger's release from renumbering the whole
//     album. A stranger's release (or a stale id that 404s) is discarded and falls
//     through to (3) — UNLESS a human chose it, see below.
//  3. Fit-selection — browse the release-group's releases WITH their recordings and
//     take the one whose track count equals the local album's, earliest date
//     breaking ties, earliest release when nothing fits. One call, because the
//     browse carries the tracklists too; picking by count and then fetching the
//     winner would have cost two.
//
// A refusal in (2) is NOT retried as (3): against the load shedding ADR-0049
// documented, a second request issued precisely during a failure is the wrong
// direction to push a struggling host, and the fit path would fail the same way.
//
// A CHOSEN release that does not apply stops here with "no tracklist" instead of
// falling through (ADR-0052). Falling through would answer with a fit tracklist
// that the caller could not tell apart from the human's edition, and would then
// license position-alone mapping against a release nobody asserted — exactly the
// stranger's-tracklist decoration the parent check exists to prevent. The caller
// re-asks without the pin, which is the same fall-through, made visible.
func (p *Provider) AlbumTracklist(ctx context.Context, req pluginapi.TracklistRequest) (pluginapi.TracklistResponse, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	s := p.host.Settings()

	rgID := strings.TrimSpace(req.ReleaseGroupID)
	if rgID == "" {
		return tracklistResult(nil, errNoTracklist)
	}
	if relID := strings.TrimSpace(req.ReleaseID); relID != "" {
		tl, err := p.taggedReleaseTracklist(ctx, s, relID, rgID)
		if err != nil {
			return tracklistResult(nil, err)
		}
		if len(tl) > 0 {
			return tracklistResult(tl, nil)
		}
		if req.ReleaseIDChosen {
			return tracklistResult(nil, errNoTracklist)
		}
	}
	return tracklistResult(p.bestFitTracklist(ctx, s, rgID, req.LocalTrackCount))
}

// taggedReleaseTracklist reads the named release — the one the FILES assert or the
// one an Admin chose — and returns its tracklist only if that release belongs to
// rgID. The check is the same either way: a human's pin is better evidence about
// WHICH edition, and no evidence at all that the edition is of this album. A
// release of some other release-group, a release with no tracks, and an unknown id
// (404 → errNoMatch) are all (nil, nil) — "not usable, fall through to
// fit-selection" — because none of them says anything about whether the album
// itself has a tracklist. A real failure is returned.
func (p *Provider) taggedReleaseTracklist(ctx context.Context, s pluginapi.Settings, relID, rgID string) ([]pluginapi.TrackCandidate, error) {
	q := url.Values{}
	// SPACE-separated, not "+"-separated: url.Values.Encode percent-encodes a literal
	// '+' to %2B (which MusicBrainz then reads as part of one nonsense inc name) and
	// encodes a space as '+', which is exactly the canonical
	// "inc=recordings+release-groups" the service documents.
	q.Set("inc", "recordings release-groups")
	q.Set("fmt", "json")
	var rel mbRelease
	if err := p.getJSON(ctx, s, "/release/"+url.PathEscape(relID), q, &rel); err != nil {
		if errors.Is(err, errNoMatch) {
			return nil, nil // stale or unknown release id — the album may still have one
		}
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(rel.ReleaseGroup.ID), rgID) {
		return nil, nil // a stranger's release: discard it rather than renumber the album
	}
	return mbTracklist(rel.Media), nil
}

// browseReleaseGroupReleases is THE listing of a release-group's editions, with
// their tracks: one `/release?release-group=…&inc=recordings&limit=100`.
//
// Both callers that need to know what editions exist go through here — fit
// selection (which reads the counts and takes one) and ReleaseGroupEditions (which
// shows the same counts to a human and lets them take one, ADR-0052). They are the
// automatic and the manual half of the SAME question, and asking it twice in two
// spellings is how the two would come to disagree about which editions there are.
func (p *Provider) browseReleaseGroupReleases(ctx context.Context, s pluginapi.Settings, rgID string) ([]mbRelease, error) {
	q := url.Values{}
	q.Set("release-group", rgID)
	q.Set("inc", "recordings")
	q.Set("limit", strconv.Itoa(releaseBrowseLimit))
	q.Set("fmt", "json")
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := p.getJSON(ctx, s, "/release", q, &out); err != nil {
		return nil, err
	}
	return out.Releases, nil
}

// ReleaseGroupEditions lists the release-group's editions for the Admin's picker
// (ADR-0052) — the AlbumTracklister half of the browse fit-selection already pays
// for. An unknown release-group (404 → errNoMatch) and a release-group with no
// releases are both an empty list: "there is nothing to choose from" is an answer
// the picker renders, not a failure it reports.
func (p *Provider) ReleaseGroupEditions(ctx context.Context, req pluginapi.ReleaseEditionsRequest) (pluginapi.ReleaseEditionsResponse, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()

	s := p.host.Settings()

	rgID := strings.TrimSpace(req.ReleaseGroupID)
	if rgID == "" {
		return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	rels, err := p.browseReleaseGroupReleases(ctx, s, rgID)
	if err != nil {
		if errors.Is(err, errNoMatch) {
			return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeMatched}, nil
		}
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ReleaseEditionsResponse{}, err
	}
	return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeMatched, Editions: mbEditions(rels)}, nil
}

// bestFitTracklist browses the release-group's releases and returns the tracklist of
// the one that fits the local album. inc=recordings makes the browse carry every
// candidate's tracks, so the count comparison and the answer come out of one call.
func (p *Provider) bestFitTracklist(ctx context.Context, s pluginapi.Settings, rgID string, localCount int) ([]pluginapi.TrackCandidate, error) {
	releases, err := p.browseReleaseGroupReleases(ctx, s, rgID)
	if err != nil {
		return nil, err
	}
	rel := pickReleaseByFit(releases, localCount)
	if rel == nil {
		return nil, errNoTracklist
	}
	tl := mbTracklist(rel.Media)
	if len(tl) == 0 {
		return nil, errNoTracklist
	}
	return tl, nil
}

// pickReleaseByFit chooses the release whose track count equals the local album's,
// earliest date breaking ties, and falls back to the earliest release when nothing
// fits (or when the local count is unknown). nil when there is nothing to choose.
//
// Count-fit is the cheapest rule that beats "whichever came back first": for an
// album with a deluxe edition, a remaster or a Japanese pressing, an arbitrary
// release is a wrong answer for every position after the first bonus track.
//
// The rule itself lives in pickEditionByFit, over the same ReleaseEdition view the
// Admin's picker is shown (ADR-0052). One implementation on purpose: the picker
// marks which edition is IN USE, and it would be marking the wrong row the moment
// a second copy of "which one fits" drifted from this one.
//
// NOTE that the HOST has its own copy of pickEditionByFit, over the same
// ReleaseEdition wire type, because it is what marks the in-use row in the picker
// (internal/enrich/album_editions.go). The two are the same rule on the same data
// arriving from opposite directions, and the host's edition list comes from THIS
// function's browse, so they agree by construction rather than by coincidence.
func pickReleaseByFit(releases []mbRelease, localCount int) *mbRelease {
	i := pickEditionByFit(mbEditions(releases), localCount)
	if i < 0 {
		return nil
	}
	return &releases[i]
}

// pickEditionByFit returns the INDEX of the edition that fits localCount best —
// equal track count first, then earliest date, then the id as a stable tiebreak —
// or -1 when there is nothing to choose. An edition with no tracks is never chosen:
// it cannot be anyone's tracklist.
//
// An undated release sorts after every dated one, and the id tiebreak is not
// meaningful — it is there so a release-group holding two same-dated editions
// resolves to the SAME one on every call rather than to whatever order the source
// returned this time.
func pickEditionByFit(eds []pluginapi.ReleaseEdition, localCount int) int {
	best := -1
	bestFits := false
	for i := range eds {
		e := eds[i]
		if e.TrackCount == 0 {
			continue
		}
		fits := localCount > 0 && e.TrackCount == localCount
		switch {
		case best < 0, fits && !bestFits:
		case bestFits && !fits:
			continue
		case !earlierEdition(e, eds[best]):
			continue
		}
		best, bestFits = i, fits
	}
	return best
}

// earlierEdition orders two editions: a dated one before an undated one, then by
// date, then by id.
func earlierEdition(a, b pluginapi.ReleaseEdition) bool {
	ad, bd := strings.TrimSpace(a.Date), strings.TrimSpace(b.Date)
	if (ad == "") != (bd == "") {
		return bd == ""
	}
	if ad != bd {
		return ad < bd
	}
	return a.ReleaseID < b.ReleaseID
}

// mbEditions projects the browse's releases onto the ReleaseEdition view, INDEX FOR
// INDEX — pickReleaseByFit maps the chosen index straight back onto the release it
// came from, so nothing may be dropped or reordered here.
func mbEditions(releases []mbRelease) []pluginapi.ReleaseEdition {
	out := make([]pluginapi.ReleaseEdition, 0, len(releases))
	for i := range releases {
		r := &releases[i]
		out = append(out, pluginapi.ReleaseEdition{
			ReleaseID:      r.ID,
			Date:           strings.TrimSpace(r.Date),
			Country:        strings.TrimSpace(r.Country),
			Format:         releaseFormat(r),
			TrackCount:     releaseTrackCount(r),
			Disambiguation: strings.TrimSpace(r.Disambiguation),
		})
	}
	return out
}

// releaseFormat summarizes what an edition is ON — "CD", "2×Vinyl", "CD + DVD" —
// from its media. A release with several discs of one format collapses to a count
// ("2×CD") because that is how a listener names it, and a mixed set keeps both
// names in order. Empty when the source reports no format for any medium, which is
// common for digital releases and is rendered as nothing rather than as "Unknown".
func releaseFormat(r *mbRelease) string {
	var names []string
	counts := map[string]int{}
	for _, m := range r.Media {
		f := strings.TrimSpace(m.Format)
		if f == "" {
			continue
		}
		if counts[f] == 0 {
			names = append(names, f)
		}
		counts[f]++
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if counts[n] > 1 {
			parts = append(parts, strconv.Itoa(counts[n])+"×"+n)
			continue
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, " + ")
}

// releaseTrackCount totals a release's tracks across every medium (disc), which is
// the number a local album's track count is compared against.
func releaseTrackCount(r *mbRelease) int {
	n := 0
	for _, m := range r.Media {
		n += len(m.Tracks)
	}
	return n
}

// mbRelease is one MusicBrainz release (one edition of a release-group) as both the
// candidate preview and AlbumTracklist read it. ReleaseGroup is populated only with
// inc=release-groups; Media/Tracks only with inc=recordings. Country and
// Disambiguation come back on the ordinary browse, so the edition picker costs no
// extra inc (ADR-0052).
type mbRelease struct {
	ID             string     `json:"id"`
	Date           string     `json:"date"`
	Country        string     `json:"country"`
	Disambiguation string     `json:"disambiguation"`
	Media          []mbMedium `json:"media"`
	ReleaseGroup   struct {
		ID string `json:"id"`
	} `json:"release-group"`
}

// mbMedium is one disc of a release; Position is the disc number, Format its medium
// ("CD", "Vinyl", "Digital Media") as the edition picker names it.
type mbMedium struct {
	Position int       `json:"position"`
	Format   string    `json:"format"`
	Tracks   []mbTrack `json:"tracks"`
}

// mbTrack is one track of a medium. Recording.ID is the MusicBrainz RECORDING id —
// the thing that resolves under /recording/ — as distinct from the track's own
// release-specific id, which does not (ADR-0049).
type mbTrack struct {
	Number    string `json:"number"`
	Position  int    `json:"position"`
	Title     string `json:"title"`
	Recording struct {
		ID string `json:"id"`
	} `json:"recording"`
}

// mbTracklist flattens a release's media into ordered TrackCandidates. A medium
// with no position is disc 1 (the single-disc release MusicBrainz numbers from 1
// anyway). An entry whose recording id is missing is KEPT, with an empty
// ExternalID: it still occupies its position, and a caller pairing leftovers needs
// to know the position is taken even though nothing can be pinned to it.
func mbTracklist(media []mbMedium) []pluginapi.TrackCandidate {
	var tl []pluginapi.TrackCandidate
	for _, m := range media {
		disc := m.Position
		if disc == 0 {
			disc = 1
		}
		for _, tr := range m.Tracks {
			tl = append(tl, pluginapi.TrackCandidate{
				Disc: disc, Position: tr.Position, Title: tr.Title, ExternalID: tr.Recording.ID,
			})
		}
	}
	return tl
}

// --- Lookups by id -----------------------------------------------------------

// releaseGroupForRelease resolves a MusicBrainz release (one edition — the entity a
// /release/ URL names) to its parent release-group and returns that release-group's
// decorated metadata, so a pasted release URL pins the album (release-group), matching
// how albums are identified. A 404 (stale/unknown release) flows out as errNoMatch via
// getJSON; a release with no parent group is likewise errNoMatch.
func (p *Provider) releaseGroupForRelease(ctx context.Context, s pluginapi.Settings, releaseID string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("inc", "release-groups")
	q.Set("fmt", "json")
	var rel struct {
		ReleaseGroup struct {
			ID string `json:"id"`
		} `json:"release-group"`
	}
	if err := p.getJSON(ctx, s, "/release/"+url.PathEscape(releaseID), q, &rel); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if strings.TrimSpace(rel.ReleaseGroup.ID) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	return p.releaseGroupByID(ctx, s, rel.ReleaseGroup.ID)
}

// artistByID fetches a single artist by MBID (the durable artist override path) and
// returns its decorative metadata (name, synthesized overview, genres). An unknown
// id is errNoMatch, like a name search with no hits.
func (p *Provider) artistByID(ctx context.Context, s pluginapi.Settings, mbid string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("inc", "tags")
	q.Set("fmt", "json")
	var a struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Type           string `json:"type"`
		Disambiguation string `json:"disambiguation"`
		Area           struct {
			Name string `json:"name"`
		} `json:"area"`
		Tags []mbTag `json:"tags"`
	}
	if err := p.getJSON(ctx, s, "/artist/"+url.PathEscape(mbid), q, &a); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	rec := pluginapi.MetadataRecord{Matched: true, Name: a.Name, ExternalID: a.ID, Source: Source}
	rec.Overview = artistOverview(a.Disambiguation, a.Type, a.Area.Name)
	rec.OverviewSynthesized = rec.Overview != ""
	rec.Genres = topTags(a.Tags)
	return rec, nil
}

// releaseGroupByID fetches a single release-group by MBID (the durable album
// override path) and returns its decorative metadata (genres, year, cover art).
func (p *Provider) releaseGroupByID(ctx context.Context, s pluginapi.Settings, mbid string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("inc", "tags")
	q.Set("fmt", "json")
	var rg struct {
		ID               string  `json:"id"`
		Title            string  `json:"title"`
		FirstReleaseDate string  `json:"first-release-date"`
		Tags             []mbTag `json:"tags"`
	}
	if err := p.getJSON(ctx, s, "/release-group/"+url.PathEscape(mbid), q, &rg); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if strings.TrimSpace(rg.ID) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	rec := pluginapi.MetadataRecord{Matched: true, Name: rg.Title, ExternalID: rg.ID, Source: Source}
	rec.Genres = topTags(rg.Tags)
	if len(rg.FirstReleaseDate) >= 4 {
		if y, err := strconv.Atoi(rg.FirstReleaseDate[:4]); err == nil && y > 0 {
			rec.ReleaseDate = rg.FirstReleaseDate
		}
	}
	rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{
		Role: "cover", URL: s.URL2 + "/release-group/" + rg.ID + "/front-500",
	})
	return rec, nil
}

// recordingByID fetches a single recording by its MBID and returns its canonical
// title (applied display-only, never identity — ADR-0002). An unknown id is
// errNoMatch, like a name search with no hits.
func (p *Provider) recordingByID(ctx context.Context, s pluginapi.Settings, mbid string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("fmt", "json")
	var out struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := p.getJSON(ctx, s, "/recording/"+url.PathEscape(mbid), q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if strings.TrimSpace(out.Title) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	return pluginapi.MetadataRecord{Matched: true, Name: out.Title, ExternalID: out.ID, Source: Source}, nil
}

// --- The artist tiers (ADR-0053) ---------------------------------------------

// artistDetails resolves an Artist that carries no id of its own — neither an
// Admin's Fix-info pin nor the MBID its files assert (both handled in Lookup, and
// both still ahead of everything here: ADR-0045/0046 and ADR-0049).
//
// It asks the artist's DISCOGRAPHY before it asks the artist's NAME (ADR-0053).
// The name search below is confidently wrong on a real, common shape, and no
// repair built out of the name reaches it: `artist:"The Eagles"` matches, exactly
// and at score 100, a 1958 British instrumental group, because the American band
// is named "Eagles". Article-insensitivity finds "Eagles" and leaves "The Eagles"
// winning; a name acceptance test accepts identical names. The two are told apart
// by what they recorded, and the caller is holding one of those albums.
//
// So: an album's release-group identifies the artist, and only if no hinted album
// can be identified does the name search run — unchanged, as the last resort for
// the artists corroboration has nothing to say about (a soundtrack filed under the
// film's name, an "Unknown Artist" pile).
//
// COST. Corroboration REPLACES the name search, it never joins it: exactly one
// identifying call is made either way, and when the album carries a tag
// release-group id that call is a LOOKUP rather than a search — zero traffic on the
// endpoint ADR-0049 measured shedding load globally. Resolving the artist id to its
// metadata then goes through artistByID, the same by-id path a pinned artist uses.
func (p *Provider) artistDetails(ctx context.Context, s pluginapi.Settings, name string, albums []pluginapi.AlbumHint) (pluginapi.MetadataRecord, error) {
	id, err := p.artistIDFromAlbum(ctx, s, albums)
	if err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if id != "" {
		return p.artistByID(ctx, s, id)
	}
	return p.artistByName(ctx, s, name)
}

// artistIDFromAlbum is ADR-0053's corroboration: it identifies ONE of the hinted
// albums and reads the artist credit off it. It returns ("", nil) — "nothing to
// corroborate with", the caller falls through to the name search — for every way
// the evidence can come up short: no hints, no usable hint, a release-group id that
// no longer resolves, a search that found nothing, or a top hit whose title is not
// this album's. A transport failure or a host refusal is returned as an ERROR
// instead, because the difference between "the discography says nothing" and
// "MusicBrainz was busy" is the difference between an answer and an outage, and
// silently answering by name during an outage is how the wrong Eagles gets stored.
//
// EXACTLY ONE CALL. The hints are ranked, not walked: the first that carries a
// release-group id wins, else the first that carries a title. Trying the second
// after the first declines would make an artist cost up to three requests to learn
// what one request already told us, and the ADR's cost claim is that corroboration
// replaces the name search one-for-one.
func (p *Provider) artistIDFromAlbum(ctx context.Context, s pluginapi.Settings, albums []pluginapi.AlbumHint) (string, error) {
	for _, h := range albums {
		// ADR-0049: an id is validated as a UUID before it is used. An unvalidated id
		// 404s, which here is indistinguishable from "this release-group is gone" and
		// would silently spend the one corroborating call on a typo.
		if IsUUID(strings.TrimSpace(h.ReleaseGroupMBID)) {
			return p.artistIDFromReleaseGroup(ctx, s, strings.TrimSpace(h.ReleaseGroupMBID))
		}
	}
	for _, h := range albums {
		if strings.TrimSpace(h.Title) != "" {
			return p.artistIDFromAlbumSearch(ctx, s, h.Title)
		}
	}
	return "", nil
}

// artistIDFromReleaseGroup reads the artist credit off a release-group the FILES
// name — one lookup, no search (ADR-0053, and the lookup-beats-search preference of
// ADR-0049 applied one level up). A stale or merged id 404s, which getJSON maps to
// errNoMatch; that is "this album could not corroborate", not "this artist does not
// exist", so it falls through to the name search rather than settling the Artist.
func (p *Provider) artistIDFromReleaseGroup(ctx context.Context, s pluginapi.Settings, rgID string) (string, error) {
	q := url.Values{}
	q.Set("inc", "artist-credits")
	q.Set("fmt", "json")
	var rg struct {
		ArtistCredit []mbCredit `json:"artist-credit"`
	}
	if err := p.getJSON(ctx, s, "/release-group/"+url.PathEscape(rgID), q, &rg); err != nil {
		if errors.Is(err, errNoMatch) {
			return "", nil
		}
		return "", err
	}
	return creditArtistID(rg.ArtistCredit), nil
}

// artistIDFromAlbumSearch searches release-groups for a local album title and reads
// the artist credit off the top hit — but only if that hit is actually this album.
//
// THE SEARCH IS UNNARROWED, AND THAT IS THE POINT. musicQuery is called with no
// artist and no release clause, so the wire query is the escaped album title and
// nothing else. AND-ing `artist:"<name>"` in is the natural-looking improvement,
// and it would silently undo this whole mechanism: the name is the thing being
// refused, and a query narrowed by it can only ever return an album by the artist
// the name already picked — which on the motivating library is a British
// instrumental group that never recorded Hell Freezes Over. There is a test that
// reads the query off the wire for exactly this reason.
//
// The top hit corroborates only when its title matches the local one under
// normalizeMatchTitle — the same comparison ADR-0050 put on the track search — and
// only the TOP hit is considered: scanning down a ranked list is how a search
// quietly becomes "find me anything plausible". A rejected hit corroborates nothing
// and the artist falls back to its name.
func (p *Provider) artistIDFromAlbumSearch(ctx context.Context, s pluginapi.Settings, album string) (string, error) {
	q := url.Values{}
	q.Set("query", musicQuery(album, "", ""))
	q.Set("fmt", "json")
	var out struct {
		ReleaseGroups []struct {
			ID           string     `json:"id"`
			Title        string     `json:"title"`
			ArtistCredit []mbCredit `json:"artist-credit"`
		} `json:"release-groups"`
	}
	if err := p.getJSON(ctx, s, "/release-group", q, &out); err != nil {
		if errors.Is(err, errNoMatch) {
			return "", nil
		}
		return "", err
	}
	if len(out.ReleaseGroups) == 0 {
		return "", nil
	}
	rg := out.ReleaseGroups[0]
	// THIS IS NOT THE ADR-0050 RECORD-ACCEPTANCE RULE, which left this source for
	// the host (ADR-0057) and did not come back with the port. Nothing here becomes
	// a record: the top hit is discarded either way, and all that is at stake is
	// whether the artist credit hanging off it is EVIDENCE. That question is
	// MusicBrainz's own — it is about which of its intermediate results this
	// provider will build its answer on, the candidate title never leaves this
	// function, and no contract call exposes the step for a host to judge. A hint
	// that fails corroborates nothing and the artist falls back to its name.
	want := normalizeMatchTitle(album)
	if want == "" || want != normalizeMatchTitle(rg.Title) {
		return "", nil
	}
	return creditArtistID(rg.ArtistCredit), nil
}

// artistByName is the last tier of ADR-0053's precedence and is unchanged from the
// behaviour that predates it: an exact-phrase name search, top hit taken. It is
// only reached when nothing in the artist's discography could identify it, which is
// where it was always the honest answer.
func (p *Provider) artistByName(ctx context.Context, s pluginapi.Settings, name string) (pluginapi.MetadataRecord, error) {
	if strings.TrimSpace(name) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	q := url.Values{}
	q.Set("query", `artist:"`+name+`"`)
	q.Set("fmt", "json")
	var out struct {
		Artists []struct {
			ID             string `json:"id"`
			Type           string `json:"type"`
			Disambiguation string `json:"disambiguation"`
			Area           struct {
				Name string `json:"name"`
			} `json:"area"`
			Tags []mbTag `json:"tags"`
		} `json:"artists"`
	}
	if err := p.getJSON(ctx, s, "/artist", q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if len(out.Artists) == 0 {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	a := out.Artists[0]
	rec := pluginapi.MetadataRecord{Matched: true, ExternalID: a.ID, Source: Source}
	rec.Overview = artistOverview(a.Disambiguation, a.Type, a.Area.Name)
	rec.OverviewSynthesized = rec.Overview != ""
	rec.Genres = topTags(a.Tags)
	return rec, nil
}

// artistOverview synthesizes the short artist blurb both artist paths write.
// MusicBrainz has no bio, so type + area + disambiguation is what keeps the Artist
// page from being bare (genres carry the real signal). The disambiguation comment
// wins over the type when there is one, exactly as it did. Both paths mark it
// OverviewSynthesized, which is what lets a Supplement's real biography replace it
// in the host's music chain (ADR-0061).
func artistOverview(disambiguation, typ, area string) string {
	var parts []string
	if disambiguation != "" {
		parts = append(parts, disambiguation)
	} else if typ != "" {
		parts = append(parts, typ)
	}
	if area != "" {
		parts = append(parts, "from "+area)
	}
	return strings.Join(parts, " ")
}

// --- Album and track by name --------------------------------------------------

// albumDetails resolves an Album the pass could not name by id: an exact-phrase
// release-group search, top hit taken, decorated with genres, year and the Cover
// Art Archive cover.
func (p *Provider) albumDetails(ctx context.Context, s pluginapi.Settings, album, artist string) (pluginapi.MetadataRecord, error) {
	if strings.TrimSpace(album) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	query := `releasegroup:"` + album + `"`
	if artist != "" {
		query += ` AND artist:"` + artist + `"`
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("fmt", "json")
	var out struct {
		ReleaseGroups []struct {
			ID               string  `json:"id"`
			FirstReleaseDate string  `json:"first-release-date"`
			Tags             []mbTag `json:"tags"`
		} `json:"release-groups"`
	}
	if err := p.getJSON(ctx, s, "/release-group", q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if len(out.ReleaseGroups) == 0 {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	rg := out.ReleaseGroups[0]
	rec := pluginapi.MetadataRecord{Matched: true, ExternalID: rg.ID, Source: Source}
	rec.Genres = topTags(rg.Tags)
	if len(rg.FirstReleaseDate) >= 4 {
		if y, err := strconv.Atoi(rg.FirstReleaseDate[:4]); err == nil && y > 0 {
			rec.ReleaseDate = rg.FirstReleaseDate
		}
	}
	// Album cover from the Cover Art Archive (the host's ArtworkFetcher downloads
	// it). Request the 500px derivative, not the full-resolution "/front" original:
	// originals routinely exceed the fetcher's size cap, and 500px is ample for an
	// album cover in the grid and on the detail page.
	rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{
		Role: "cover", URL: s.URL2 + "/release-group/" + rg.ID + "/front-500",
	})
	return rec, nil
}

// trackDetails is the LAST tier of ADR-0050's precedence (record → tag → album
// tracklist → search): nothing exact names this recording, so a text search is all
// that is left.
//
// THE QUERY IS THE PICKER'S. musicQuery gives relevance-ranked, Lucene-ESCAPED
// terms with the artist AND-narrowed as a field clause — not the exact-phrase
// `recording:"<title>"` this sent before, unescaped. Two things were wrong with
// that. It 4xx'd the Lucene parser on any title carrying a metacharacter (`AC/DC`,
// `"Heroes"`, `!!!`), surfacing as a fake provider failure. And an exact phrase
// misses on every punctuation MusicBrainz spells differently — the real case being
// a bracketed title tagged `( I Could Only ) Whisper Your Name` against the
// source's `(I Could Only) Whisper Your Name`, one of 170 bracketed titles among
// the 730 unmatched tracks that prompted this.
//
// THE ACCEPTANCE TEST IS WHAT MAKES THE SWAP PAYABLE, AND IT IS NOT APPLIED HERE.
// An exact phrase returning zero rows is HONESTLY empty; a relevance query
// essentially always returns something, so `Recordings[0]` applied blind would
// trade a queue row for a silent wrong overview — the confident-wrong-answer
// ADR-0049 ruled is the worse outcome. So the top hit comes back marked
// FromSearch, and the HOST accepts it only when its title matches the local
// track's, turning a failure into its own rejection reason. This source used to run
// that test and return that error; ADR-0057 moved the judgement to the host,
// because the rule has to hold for sources the core does not ship as well as for
// this one. What is decided is unchanged — only where.
//
// ONE REQUEST, ALWAYS. No looser second query when the first comes back empty or
// is rejected. ADR-0049 measured MusicBrainz shedding load globally on the search
// cluster, and a retry issued precisely during those failures pushes the wrong
// way; the album tier already removed most of the traffic that would have wanted
// one.
func (p *Provider) trackDetails(ctx context.Context, s pluginapi.Settings, track, artist string) (pluginapi.MetadataRecord, error) {
	if strings.TrimSpace(track) == "" {
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	q := url.Values{}
	q.Set("query", musicQuery(track, artist, ""))
	q.Set("fmt", "json")
	var out struct {
		Recordings []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"recordings"`
	}
	if err := p.getJSON(ctx, s, "/recording", q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	if len(out.Recordings) == 0 {
		// The source looked and has nothing. Plain no-match — a different failure,
		// and a different remedy, from a hit the host refused.
		return pluginapi.MetadataRecord{}, errNoMatch
	}
	r := out.Recordings[0]
	// The top hit, UNJUDGED, marked as the search hit it is. Whether it is actually
	// this song is the HOST's call (ADR-0057). Name carries the candidate's own
	// title because that is what the host judges against.
	//
	// MusicBrainz has no track synopsis; only the canonical title is offered. The
	// host applies it as a display title ONLY where the tag title was sparse, and
	// only for a record it accepted.
	return pluginapi.MetadataRecord{
		Matched: true, Name: r.Title, ExternalID: r.ID, Source: Source, FromSearch: true,
	}, nil
}

// --- ArtworkCandidates: the Cover Art Archive --------------------------------

// ArtworkCandidates lists the cover images the Cover Art Archive holds for an
// album (release-group), the Edit-item image picker's data for Music (Fix label,
// ADR-0019). Only the album kind has a listable image set (CAA is release-group
// keyed); an Artist/Track has none here, and a ref with no pinned MBID can't be
// listed, so those yield no candidates (never a failure). The "front" images are
// returned for any cover/poster role. Read-only.
//
// THE COVER ART ARCHIVE IS Settings.URL2 — a distinct host from the MusicBrainz web
// service, declared in this plugin's own network allowlist and defaulted by its own
// manifest. It is fetched directly rather than through getJSON, which prefixes
// Settings.URL, exactly as the Go provider fetched it off its own CoverArtURL
// field.
func (p *Provider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	ref := req.Ref
	mbid := ref.ID(pluginapi.NamespaceMusicBrainz)
	if ref.Kind != "album" || mbid == "" {
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	ctx, cancel := bounded(ctx)
	defer cancel()

	s := p.host.Settings()

	u := s.URL2 + "/release-group/" + url.PathEscape(mbid)
	var out struct {
		Images []struct {
			Image      string            `json:"image"`
			Front      bool              `json:"front"`
			Thumbnails map[string]string `json:"thumbnails"`
		} `json:"images"`
	}
	if err := pluginsdk.GetJSON(ctx, p.host, u, nil, &out, pluginsdk.Header("Accept", "application/json")); err != nil {
		// A 404 is the normal "this release-group has no cover art" outcome — no
		// images, and not a failure.
		var fe *pluginsdk.FetchError
		if errors.As(err, &fe) && fe.IsNotFound() {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
		}
		if detail, ok := pluginsdk.Unavailable(err); ok {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	cands := make([]pluginapi.ArtworkCandidate, 0, len(out.Images))
	for _, im := range out.Images {
		// Prefer the 500px derivative (the enrichment pass already caps cover fetches
		// at 500px — an original routinely exceeds the fetcher's size guard).
		u := im.Thumbnails["500"]
		if u == "" {
			u = im.Image
		}
		if u == "" {
			continue
		}
		cands = append(cands, pluginapi.ArtworkCandidate{URL: u, Source: CoverArtSource})
	}
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// --- ExternalRefParser --------------------------------------------------------

// ParseExternalRef reads a pasted MusicBrainz id-or-URL for a Music item — the
// ExternalRefParser half of this source (ADR-0057's external-ref capability). It
// is the one place this provider's URL vocabulary is interpreted, and it is a call
// rather than a pattern list because two of its four answers are logic, not shape:
//
//   - A /release/ URL on an ALBUM is not an album pin. It names one EDITION of the
//     release-group the album actually is (ADR-0038), so it resolves to the
//     release-group in ExternalID — which the Lookup does, from ReleaseMBID — and
//     rides back as ReleaseID so the human's edition survives the preview→apply
//     round trip instead of being dropped (ADR-0052).
//   - A recognized URL for an entity this server pins nothing by (a /work/, a
//     /label/) is OutcomeRefUnsupportedKind, not OutcomeRefInvalid, so the Admin is
//     told which kind of link to grab rather than "that's not a URL".
//
// The remaining two are the ordinary ones: a typed URL of the wrong kind for the
// item is OutcomeRefKindMismatch carrying both kinds, and anything else is
// unreadable. A non-Music kind is OutcomeUnavailable — this source has nothing to
// say about a TMDB paste, and saying so lets the host answer for the namespaces it
// keeps its own columns for.
//
// It makes NO request, so it takes no budget and needs no context of its own.
func (p *Provider) ParseExternalRef(_ context.Context, req pluginapi.ExternalRefRequest) (pluginapi.ExternalRefResponse, error) {
	switch req.Kind {
	case "artist", "album", "track":
	default:
		return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	if refKind, id, ok := ParseRef(req.Pasted); ok {
		if refKind != "" && refKind != req.Kind {
			return pluginapi.ExternalRefResponse{
				Outcome: pluginapi.OutcomeRefKindMismatch, GotKind: refKind, WantKind: req.Kind,
			}, nil
		}
		return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeMatched, ExternalID: id}, nil
	}
	if req.Kind == "album" {
		if relID, ok := ParseReleaseRef(req.Pasted); ok {
			return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeMatched, ReleaseID: relID}, nil
		}
	}
	if RefUnsupported(req.Pasted) {
		return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeRefUnsupportedKind}, nil
	}
	return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeRefInvalid}, nil
}

// --- Small shared shapes ------------------------------------------------------

type mbTag struct {
	Name string `json:"name"`
}

// topTags returns up to three highest-signal tag names as genres, preserving
// order (MusicBrainz returns them roughly by relevance).
func topTags(tags []mbTag) []string {
	var out []string
	for _, t := range tags {
		if t.Name == "" {
			continue
		}
		out = append(out, t.Name)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// mbCredit is one entry of a MusicBrainz artist-credit: an artist name plus the join
// phrase that links it to the next (" & ", " feat. ", ", ", …). The last entry's phrase
// is empty. See creditString.
//
// Artist is the credited artist ENTITY, and it is the whole product of ADR-0053's
// corroboration: an album identifies its artist by id, which is a fact about the
// discography rather than a guess about the name. It comes back on any response
// carrying artist credits — a release-group lookup with inc=artist-credits, and the
// release-group search, which includes them unasked.
type mbCredit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
	Artist     struct {
		ID string `json:"id"`
	} `json:"artist"`
}

// creditArtistID returns the MBID of the FIRST credited artist, empty when the
// credit names none. First, for the reason ADR-0049 takes the first value of a
// multi-valued tag: a collaboration credits several artists and the library files
// the album under one of them, so any other choice needs a rule this has no way to
// decide. A wrong pick here is a corroboration that fails to corroborate, and the
// artist falls back to the name search.
func creditArtistID(credits []mbCredit) string {
	for _, c := range credits {
		if id := strings.TrimSpace(c.Artist.ID); id != "" {
			return id
		}
	}
	return ""
}

// creditString joins an artist-credit into its full display string, preserving the
// provider's join phrases, so a collaboration reads as the whole credit ("Ben Folds &
// Nick Hornby") rather than only its first artist. Empty when there are no credits.
func creditString(credits []mbCredit) string {
	var b strings.Builder
	for _, c := range credits {
		b.WriteString(c.Name)
		b.WriteString(c.JoinPhrase)
	}
	return strings.TrimSpace(b.String())
}

// escapeLucene backslash-escapes the Lucene query metacharacters so a free-text
// search phrase can't be misparsed by the MusicBrainz query parser (which speaks
// Lucene). Applied to the user's terms in musicQuery — the terms are escaped but no
// longer phrase-wrapped, so metacharacters in `AC/DC` / `"Heroes"` / `!!!` still
// can't 4xx the parser (item-editing/search-improvements).
func escapeLucene(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\', '+', '-', '!', '(', ')', '{', '}', '[', ']', '^', '"', '~', '*', '?', ':', '/', '&', '|':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// YearFromDate extracts the 4-digit year from a MusicBrainz date string
// ("YYYY-MM-DD", or just "YYYY"); 0 when absent/unparseable.
//
// Exported so a test can state the rule directly. The HOST keeps its own copy
// (internal/enrich/externalid.go) because it parses provider dates of its own;
// this one is the plugin's, and a plugin sharing a helper with the server it is
// sandboxed from is exactly what the module split exists to prevent.
func YearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

// --- The one way out ----------------------------------------------------------

// getJSON issues a GET against the operator's MusicBrainz base URL, decodes the
// JSON body into out, and runs the in-request 503 ladder.
//
// The mapping of an answer to an error is the Go provider's, unchanged:
//
//   - 404 is a definitive "no such record" (a pasted id naming a different entity
//     type, a stale or merged MBID) — NOT a connectivity failure. It is errNoMatch,
//     so callers surface "no record found" rather than the alarming "source may be
//     unreachable".
//   - any other non-2xx is read for the host's own explanation (see [refusalFrom])
//     and either retried here or handed back.
//   - a body that will not parse is a Go error, and a Go error is a strike, which
//     is right: a source answering 200 with something this code cannot read is not
//     an outage, it is a disagreement an operator has to see.
func (p *Provider) getJSON(ctx context.Context, s pluginapi.Settings, path string, q url.Values, out any) error {
	u := pluginsdk.URLWithQuery(s.URL+path, q)
	for attempt := 1; ; attempt++ {
		resp, err := pluginsdk.Do(ctx, p.host, pluginapi.FetchRequest{
			URL:     u,
			Headers: []pluginapi.FetchHeader{pluginsdk.Header("Accept", "application/json")},
		})
		if err != nil {
			var fe *pluginsdk.FetchError
			if !errors.As(err, &fe) || fe.Status == 0 || fe.IsRefusal() {
				// The host refused, or never got an answer at all. Nothing was learned
				// about the item and nothing here can improve on it.
				return err
			}
			if fe.IsNotFound() {
				return errNoMatch
			}
			r := refusalFrom(resp)
			// Retry HERE, inside the one lookup, only when waiting a second or two can
			// plausibly change the answer — which means only when the refusal is about
			// OUR usage, or the host named a Retry-After.
			//
			// It used to retry any 503 four times. Against MusicBrainz's global search
			// shedding that is worse than useless: a shed lasts minutes, so all four
			// attempts fail, the pass is delayed ~6s per track, and three extra requests
			// are added to a host that is already dropping load. Failing fast hands the
			// item to the cross-pass backoff (ADR-0048), which is measured in minutes and
			// is the mechanism that actually recovers this.
			wait, retryHere := inRequestRetry(r, attempt, maxAttempts)
			if retryHere && budgetAllows(ctx, wait) {
				if err := sleepCtx(ctx, wait); err != nil {
					// The call's budget ran out mid-wait. That is an outage from the item's
					// point of view, not an answer, so it must reach the guest's caller as
					// a TRANSIENT fetch failure rather than as a bare context error.
					return &pluginsdk.FetchError{URL: u, Transport: err.Error()}
				}
				continue
			}
			// Giving up. The host's own explanation goes to the server log — it is the
			// one thing "status 503" cannot say, and an operator reading a bare 503
			// reasonably concludes they are blocked and starts throttling themselves,
			// which fixes nothing when MusicBrainz is shedding load globally. The ERROR
			// stays the SDK's FetchError, so pluginsdk.Unavailable classifies it and the
			// item takes ADR-0048's backoff.
			pluginsdk.Logf(p.host, pluginsdk.LevelWarn, "musicbrainz %s: status %d (%s)", path, r.Status, r)
			return err
		}
		if out != nil {
			if err := json.Unmarshal(resp.Body, out); err != nil {
				return &pluginsdk.FetchError{URL: u, Status: resp.Status, Decode: err}
			}
		}
		return nil
	}
}

// refusal is what MusicBrainz said when it turned a request away, in the terms it
// itself used. It exists because "status 503" is not a diagnosis: it is the same
// three digits whether the operator is rate-limited, blocked, or standing in a
// queue behind everyone else on the internet — and those have opposite remedies.
//
// MusicBrainz labels this precisely and the labels were being discarded:
//
//	x-ratelimit-zone: search-global    which bucket
//	x-ratelimit-who:  search-shed      WHOSE bucket — an IP when it is you,
//	                                   a shed name when it is everyone
//	x-ratelimit-limit / -remaining     how much of it is left
type refusal struct {
	Status     int
	Zone       string
	Who        string
	Limit      string
	Remaining  string
	RetryAfter string
	// Message is the host's own error text, trimmed.
	Message string
}

// refusalFrom captures a refusal from the response the SDK handed back alongside
// its error. pluginsdk.Do returns the response even when it returns an error,
// which is the whole reason this provider can keep the Go one's diagnosis.
func refusalFrom(resp pluginapi.FetchResponse) refusal {
	r := refusal{
		Status:     resp.Status,
		Zone:       header(resp, "X-RateLimit-Zone"),
		Who:        header(resp, "X-RateLimit-Who"),
		Limit:      header(resp, "X-RateLimit-Limit"),
		Remaining:  header(resp, "X-RateLimit-Remaining"),
		RetryAfter: header(resp, "Retry-After"),
	}
	body := resp.Body
	if len(body) > maxRefusalBody {
		body = body[:maxRefusalBody]
	}
	r.Message = strings.TrimSpace(collapseSpace(string(body)))
	return r
}

// header reads one response header case-insensitively. The contract carries
// headers as an ordered list of pairs rather than as a map, because a header may
// legitimately repeat; the first value wins here, as http.Header.Get does.
func header(resp pluginapi.FetchResponse, name string) string {
	for _, h := range resp.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// ourQuota reports whether the refusal is about THIS server's own usage — the one
// case where slowing down helps.
//
// The signal is x-ratelimit-who: when the limiter is counting an individual client
// it names that client (an address), and when it is shedding load across everyone
// it names the shed bucket ("search-shed"). An address contains a dot or a colon
// and a shed name does not, which is a crude test — so the host's own wording is
// consulted first, and an ABSENT who is treated as ours, because a refusal we
// cannot attribute is one worth slowing down for.
func (r refusal) ourQuota() bool {
	msg := strings.ToLower(r.Message)
	switch {
	case strings.Contains(msg, "exceeding the allowable rate limit"),
		strings.Contains(msg, "rate limit"):
		return true
	case strings.Contains(msg, "currently busy"), strings.Contains(msg, "try again later"):
		return false
	}
	if r.Who == "" {
		return true
	}
	return strings.ContainsAny(r.Who, ".:")
}

// String renders the refusal for a log line: the host's verdict first, then the
// counters, then the one sentence an operator needs — whether this is theirs.
func (r refusal) String() string {
	var b strings.Builder
	if r.Message != "" {
		fmt.Fprintf(&b, "%q", r.Message)
	}
	var facts []string
	if r.Zone != "" {
		facts = append(facts, "zone="+r.Zone)
	}
	if r.Who != "" {
		facts = append(facts, "who="+r.Who)
	}
	if r.Remaining != "" || r.Limit != "" {
		facts = append(facts, "quota="+r.Remaining+"/"+r.Limit)
	}
	if len(facts) > 0 {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(strings.Join(facts, " "))
	}
	if b.Len() > 0 {
		b.WriteString("; ")
	}
	if r.ourQuota() {
		b.WriteString("this is OUR usage — slowing down will help")
	} else {
		b.WriteString("the host is shedding load for everyone — not our rate limit, " +
			"and throttling further will not help")
	}
	return b.String()
}

// collapseSpace flattens whitespace runs so a multi-line HTML error page cannot
// span a dozen log lines.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// inRequestRetry decides whether to retry this refusal inside the current lookup,
// and how long to wait first.
//
// Two things are worth waiting out in-request, because both are short:
//
//   - our own rate limit — we went too fast, and a pause fixes it;
//   - an explicit Retry-After — the host named a duration, so honor it.
//
// Everything else (notably a global load shed) is handed straight back. The
// distinction is the host's, read from its response, not guessed from the status.
func inRequestRetry(r refusal, attempt, maxAttempts int) (time.Duration, bool) {
	if attempt >= maxAttempts || !pluginsdk.RetryableStatus(r.Status) {
		return 0, false
	}
	if after := retryAfter(r.RetryAfter, 0); after > 0 {
		return after, true
	}
	if r.ourQuota() {
		return time.Duration(attempt) * retryBackoffBase, true
	}
	return 0, false
}

// retryAfter reads a Retry-After header value (integer seconds), falling back to
// the given duration when it is absent or unparseable.
func retryAfter(v string, fallback time.Duration) time.Duration {
	if v = strings.TrimSpace(v); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
}

// budgetAllows reports whether waiting d and then making ONE more request still
// fits inside this call's budget (see [bounded]).
//
// This is the clause ADR-0059 decision 6 adds to the ladder. MusicBrainz can name
// a Retry-After of any size, and sleeping through one that outlives the call's
// deadline means the host unwinds the guest — which counts a failure against the
// plugin and disables it after three. So a wait the budget cannot pay for is not
// taken at all: the ladder stops, and the answer is `unavailable`, which is what
// an outage IS.
//
// retryHeadroom is what the request after the wait needs. It is deliberately
// generous relative to a fast round trip, because the cost of guessing low is the
// unwinding this exists to avoid, and the cost of guessing high is one retry not
// attempted at the very end of a budget that was about to expire anyway.
func budgetAllows(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Now().Add(d + retryHeadroom).Before(deadline)
}

// retryHeadroom is how much of the call budget one more request is assumed to
// need. See [budgetAllows].
const retryHeadroom = 5 * time.Second

// sleepCtx waits for d (no-op when d<=0), returning early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx.Done() == nil {
		// No cancellation channel — a sandbox context that carries only a deadline
		// cannot happen (bounded gives every call one), but a caller's plain
		// context.Background() can.
		time.Sleep(d)
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- Outcome mapping ----------------------------------------------------------

// lookupResult turns one internal (record, error) into the contract's answer. It
// is the one place this package's three error kinds meet the three outcomes, and
// the CLASSIFICATION of a failure is [pluginsdk.Unavailable]'s rather than this
// plugin's: every metadata provider needs exactly that rule and none of them
// should own a copy of it, least of all seven copies that could disagree about
// what a 503 means.
func lookupResult(rec pluginapi.MetadataRecord, err error) (pluginapi.LookupResponse, error) {
	switch {
	case err == nil:
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: rec}, nil
	case errors.Is(err, errNoMatch):
		return noMatch(), nil
	}
	if detail, ok := pluginsdk.Unavailable(err); ok {
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
	}
	return pluginapi.LookupResponse{}, err
}

// tracklistResult is lookupResult for the tracklist call. "This album has no
// tracklist" is OutcomeNoMatch and OutcomeMatched always carries at least one
// track (pluginapi.TracklistResponse).
//
// A 404 on the browse arrives as errNoMatch and is folded into the SAME answer,
// which is a decision and not a slip. The Go provider let it out as an error from
// AlbumTracklist while mapping it to an empty list in ReleaseGroupEditions — the
// same 404, on the same request, read two ways. OutcomeNoMatch is the reading that
// survives inspection: a release-group that does not exist holds no releases, which
// is exactly what the contract says this outcome means. The reading it replaces
// would have parked the album on a stale pin AND, under ADR-0059's strike rule,
// taken the plugin off the server after three of them.
func tracklistResult(tracks []pluginapi.TrackCandidate, err error) (pluginapi.TracklistResponse, error) {
	switch {
	case err == nil && len(tracks) > 0:
		return pluginapi.TracklistResponse{Outcome: pluginapi.OutcomeMatched, Tracks: tracks}, nil
	case err == nil, errors.Is(err, errNoTracklist), errors.Is(err, errNoMatch):
		return pluginapi.TracklistResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
	if detail, ok := pluginsdk.Unavailable(err); ok {
		return pluginapi.TracklistResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
	}
	return pluginapi.TracklistResponse{}, err
}

func noMatch() pluginapi.LookupResponse {
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}
}
