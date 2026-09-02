package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MusicBrainzProvider is the production MetadataProvider for the Music kinds
// (artist/album/track): it resolves a parsed identity against the MusicBrainz
// web service and points album artwork at the Cover Art Archive. Tag-derived
// identity stays authoritative (ADR-0002 as amended for Music) — this only
// decorates: artist genres/bio, album genres/year/cover, and a canonical track
// title the service applies only where the tag title was sparse. All HTTP +
// JSON shapes live here, behind the MetadataProvider seam.
//
// MusicBrainz has no artist images and no track synopses, so those fields come
// back empty (a documented limitation; fanart.tv/AudioDB would be a later seam).
type MusicBrainzProvider struct {
	BaseURL     string // e.g. https://musicbrainz.org/ws/2
	CoverArtURL string // e.g. https://coverartarchive.org
	Language    string
	UserAgent   string // MusicBrainz requires a descriptive UA
	HTTPClient  *http.Client
	// MinInterval throttles requests to respect the host's rate policy — the public
	// MusicBrainz allows ~1 req/sec (it answers 503 once you exceed it). It is
	// operator-configurable (config.MusicBrainzRateLimit) since a mirror may permit
	// more; zero disables throttling (a self-hosted mirror with no policy, or tests).
	MinInterval time.Duration
	// RetryBackoff is the base delay after a 503 that carried no usable
	// Retry-After: attempt N waits N x this, on top of whatever MinInterval the
	// throttle then imposes. Zero or negative uses defaultMusicBrainzRetryBackoff;
	// only a test sets it low.
	//
	// It is deliberately INDEPENDENT of MinInterval. A 503 is the host telling us
	// to back off, which it means whether or not we opted into client-side pacing.
	// Deriving the delay from MinInterval (as this once did) meant MinInterval=0 --
	// the documented setting for a mirror with no rate policy -- computed a delay
	// of zero and burst all four attempts back-to-back, turning the one signal a
	// struggling host can send into a hammering.
	RetryBackoff time.Duration
}

const defaultMusicBrainzInterval = time.Second // MusicBrainz allows ~1 req/sec.

// defaultMusicBrainzRetryBackoff is the base 503 delay when the response carries
// no Retry-After. One second matches the public host's ~1 req/sec policy, and is
// a floor a mirror inherits too: a mirror that answers 503 wants a pause even
// though it set no rate policy for the steady state.
const defaultMusicBrainzRetryBackoff = time.Second

// NewMusicBrainzProvider builds a provider from config. A nil HTTP client gets a
// default with a sane timeout (a slow lookup must not hang a pass).
func NewMusicBrainzProvider(baseURL, coverArtURL, language string) *MusicBrainzProvider {
	return &MusicBrainzProvider{
		BaseURL:      baseURL,
		CoverArtURL:  coverArtURL,
		Language:     language,
		UserAgent:    DefaultUserAgent,
		HTTPClient:   &http.Client{Timeout: 15 * time.Second},
		MinInterval:  defaultMusicBrainzInterval,
		RetryBackoff: defaultMusicBrainzRetryBackoff,
	}
}

// Lookup resolves ref to MusicBrainz metadata, dispatching by kind. Non-Music
// kinds return ErrNoMatch (the CompositeProvider routes video kinds to TMDB).
func (p *MusicBrainzProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "artist":
		// A pinned artist MBID (an applied Enrichment override) resolves BY id so a
		// re-enrich or later pass looks up the exact artist the Admin picked (ADR-0019
		// durability) instead of re-searching by name.
		if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
			return p.artistByID(ctx, id)
		}
		return p.artistDetails(ctx, ref.Title, ref.AlbumHints)
	case "album":
		// A pinned MusicBrainz *release* (one edition) resolves to its parent
		// release-group — the album we actually pin — so a pasted /release/ URL works.
		if id := strings.TrimSpace(ref.ReleaseMBID); id != "" {
			return p.releaseGroupForRelease(ctx, id)
		}
		// A pinned release-group MBID resolves BY id (the durable album override).
		if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
			return p.releaseGroupByID(ctx, id)
		}
		return p.albumDetails(ctx, ref.Album, ref.Artist)
	case "track":
		// A pinned recording MBID (an applied Enrichment override) resolves BY id so
		// a re-enrich or later pass looks up the exact record the Admin picked instead
		// of re-searching by name (ADR-0019 durability). No id falls back to the
		// name+artist search.
		if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
			return p.recordingByID(ctx, id)
		}
		return p.trackDetails(ctx, ref.Track, ref.Artist)
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

// Search returns MusicBrainz candidates for a free-text query, dispatching by
// kind: a Track (recording) search hits /recording; an Artist search /artist; an
// Album (release-group) search /release-group — the leaves + browse parents a
// Music Enrichment override corrects (ADR-0019). Each candidate carries the MBID
// to pin, a title/name, a disambiguation hint (the "wrong Nirvana" tell), and — for
// an album — its tracklist preview. A blank query or an unsupported kind yields no
// candidates / ErrSearchUnavailable.
func (p *MusicBrainzProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	switch kind {
	case "track":
		return p.searchRecordings(ctx, query, opts)
	case "artist":
		return p.searchArtists(ctx, query, opts)
	case "album":
		return p.searchReleaseGroups(ctx, query, opts)
	default:
		return nil, ErrSearchUnavailable
	}
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
func musicQuery(terms, artist, release string) string {
	q := escapeLucene(terms)
	if a := strings.TrimSpace(artist); a != "" {
		q += ` AND artist:"` + escapeLucene(a) + `"`
	}
	if r := strings.TrimSpace(release); r != "" {
		q += ` AND release:"` + escapeLucene(r) + `"`
	}
	return q
}

// setPaging applies the picker's limit/offset to a search request so a broad
// common-title query can be paged ("show more") instead of only ever returning the
// source's first page. A zero limit/offset leaves the MusicBrainz default.
func setPaging(q url.Values, opts SearchOptions) {
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
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

// searchRecordings serves the Track kind: recordings mapped to Candidates carrying
// the recording MBID, title, an artist-credit + disambiguation hint, and a
// best-effort original year.
func (p *MusicBrainzProvider) searchRecordings(ctx context.Context, query string, opts SearchOptions) ([]Candidate, error) {
	q := url.Values{}
	// Relevance-ranked terms (still Lucene-escaped so `AC/DC`, `"Heroes"`, `!!!` can't
	// 4xx the parser), NOT an exact-phrase recording:"…" (item-editing/search-
	// improvements). opts.Artist and opts.Release AND-narrow to a specific artist and
	// album when supplied — a recording title alone is rarely distinguishing.
	q.Set("query", musicQuery(query, opts.Artist, opts.Release))
	setPaging(q, opts)
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
	if err := p.getJSON(ctx, "/recording", q, &out); err != nil {
		return nil, err
	}
	cands := make([]Candidate, 0, len(out.Recordings))
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
		year := yearFromDate(r.FirstReleased)
		album := ""
		takeEarlier := func(date string) {
			if y := yearFromDate(date); y > 0 && (year == 0 || y < year) {
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
		cands = append(cands, Candidate{
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
// Candidates carrying the artist MBID, name, and a type/area/disambiguation hint.
func (p *MusicBrainzProvider) searchArtists(ctx context.Context, query string, opts SearchOptions) ([]Candidate, error) {
	q := url.Values{}
	// Relevance-ranked, Lucene-escaped terms — not an exact artist:"…" phrase — so a
	// name typed with extra words or different order still scores the right artist to
	// the top (item-editing/search-improvements). Artist scoping is N/A here.
	q.Set("query", musicQuery(query, "", ""))
	setPaging(q, opts)
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
	if err := p.getJSON(ctx, "/artist", q, &out); err != nil {
		return nil, err
	}
	cands := make([]Candidate, 0, len(out.Artists))
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
		cands = append(cands, Candidate{
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
// Candidates carrying the release-group MBID, title, year, a disambiguation hint,
// a Cover Art thumbnail, and the tracklist preview (fetched per candidate) the
// positional cascade (slice 05) will consume. A tracklist fetch that fails is
// non-fatal — the candidate is still offered without a preview (ADR-0001).
func (p *MusicBrainzProvider) searchReleaseGroups(ctx context.Context, query string, opts SearchOptions) ([]Candidate, error) {
	q := url.Values{}
	// Relevance-ranked, Lucene-escaped terms — NOT an exact releasegroup:"…" phrase.
	// This is the headline fix (item-editing/search-improvements): the canonical
	// release-group title is often just the name ("Anastasia") with the descriptor a
	// secondary-TYPE (Soundtrack), so a phrase query carrying "Anastasia Soundtrack"
	// matched nothing. opts.Artist AND-narrows to a specific artist when supplied;
	// opts.Release is dropped, because a release-group search IS the album search —
	// there is no release axis left to narrow on.
	q.Set("query", musicQuery(query, opts.Artist, ""))
	setPaging(q, opts)
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
	if err := p.getJSON(ctx, "/release-group", q, &out); err != nil {
		return nil, err
	}
	cands := make([]Candidate, 0, len(out.ReleaseGroups))
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
		c := Candidate{
			ExternalID:     rg.ID,
			Title:          rg.Title,
			Year:           yearFromDate(rg.FirstReleaseDate),
			ThumbnailURL:   p.CoverArtURL + "/release-group/" + rg.ID + "/front-250",
			Disambiguation: strings.Join(hints, " — "),
			// "Album · Soundtrack" — the type badge that tells the Anastasia soundtrack
			// apart from the many other same-titled "Anastasia" release-groups.
			TypeLabel: typeLabel(rg.PrimaryType, rg.SecondaryTypes),
			Kind:      "album",
		}
		if tl, err := p.releaseGroupTracklist(ctx, rg.ID); err == nil {
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
func (p *MusicBrainzProvider) releaseGroupTracklist(ctx context.Context, rgID string) ([]TrackCandidate, error) {
	q := url.Values{}
	q.Set("release-group", rgID)
	q.Set("inc", "recordings")
	q.Set("limit", "1")
	q.Set("fmt", "json")
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := p.getJSON(ctx, "/release", q, &out); err != nil {
		return nil, err
	}
	if len(out.Releases) == 0 {
		return nil, nil
	}
	return mbTracklist(out.Releases[0].Media), nil
}

// --- AlbumTracklister: the tracklist of the release an album actually is ------

// releaseBrowseLimit caps the release browse behind fit-selection. MusicBrainz's
// browse default is 25 and its maximum is 100; a release-group with more than a
// hundred editions is a compilation nobody rips at home, and paging past the first
// hundred to find a better fit is not worth a second call at a rate-limited host.
const releaseBrowseLimit = 100

// AlbumTracklist returns the ordered tracks of the release THIS album is, not of
// whichever release the source happens to list first (ADR-0050).
//
// It costs one call for a tagged album and one for an untagged one; two only when
// the tagged release turns out to belong to somebody else. The order is:
//
//  1. No release-group id — the album is unresolved. ErrNoTracklist, zero calls.
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
// A CHOSEN release that does not apply stops here with ErrNoTracklist instead of
// falling through (ADR-0052). Falling through would answer with a fit tracklist
// that the caller could not tell apart from the human's edition, and would then
// license position-alone mapping (issue 11) against a release nobody asserted —
// exactly the stranger's-tracklist decoration the parent check exists to prevent.
// The caller re-asks without the pin, which is the same fall-through, made visible.
func (p *MusicBrainzProvider) AlbumTracklist(ctx context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	rgID := strings.TrimSpace(req.ReleaseGroupID)
	if rgID == "" {
		return nil, ErrNoTracklist
	}
	if relID := strings.TrimSpace(req.ReleaseID); relID != "" {
		tl, err := p.taggedReleaseTracklist(ctx, relID, rgID)
		if err != nil {
			return nil, err
		}
		if len(tl) > 0 {
			return tl, nil
		}
		if req.ReleaseIDChosen {
			return nil, ErrNoTracklist
		}
	}
	return p.bestFitTracklist(ctx, rgID, req.LocalTrackCount)
}

// taggedReleaseTracklist reads the named release — the one the FILES assert or the
// one an Admin chose — and returns its tracklist only if that release belongs to
// rgID. The check is the same either way: a human's pin is better evidence about
// WHICH edition, and no evidence at all that the edition is of this album. A
// release of some other release-group, a
// release with no tracks, and an unknown id (404 → ErrNoMatch) are all (nil, nil) —
// "not usable, fall through to fit-selection" — because none of them says anything
// about whether the album itself has a tracklist. A real failure is returned.
func (p *MusicBrainzProvider) taggedReleaseTracklist(ctx context.Context, relID, rgID string) ([]TrackCandidate, error) {
	q := url.Values{}
	// SPACE-separated, not "+"-separated: url.Values.Encode percent-encodes a literal
	// '+' to %2B (which MusicBrainz then reads as part of one nonsense inc name) and
	// encodes a space as '+', which is exactly the canonical
	// "inc=recordings+release-groups" the service documents.
	q.Set("inc", "recordings release-groups")
	q.Set("fmt", "json")
	var rel mbRelease
	if err := p.getJSON(ctx, "/release/"+url.PathEscape(relID), q, &rel); err != nil {
		if errors.Is(err, ErrNoMatch) {
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
func (p *MusicBrainzProvider) browseReleaseGroupReleases(ctx context.Context, rgID string) ([]mbRelease, error) {
	q := url.Values{}
	q.Set("release-group", rgID)
	q.Set("inc", "recordings")
	q.Set("limit", strconv.Itoa(releaseBrowseLimit))
	q.Set("fmt", "json")
	var out struct {
		Releases []mbRelease `json:"releases"`
	}
	if err := p.getJSON(ctx, "/release", q, &out); err != nil {
		return nil, err
	}
	return out.Releases, nil
}

// ReleaseGroupEditions lists the release-group's editions for the Admin's picker
// (ADR-0052) — the AlbumEditionLister half of the browse fit-selection already
// pays for. An unknown release-group (404 → ErrNoMatch) and a release-group with no
// releases are both an empty list: "there is nothing to choose from" is an answer
// the picker renders, not a failure it reports.
func (p *MusicBrainzProvider) ReleaseGroupEditions(ctx context.Context, releaseGroupID string) ([]ReleaseEdition, error) {
	rgID := strings.TrimSpace(releaseGroupID)
	if rgID == "" {
		return nil, nil
	}
	rels, err := p.browseReleaseGroupReleases(ctx, rgID)
	if err != nil {
		if errors.Is(err, ErrNoMatch) {
			return nil, nil
		}
		return nil, err
	}
	return mbEditions(rels), nil
}

// bestFitTracklist browses the release-group's releases and returns the tracklist of
// the one that fits the local album. inc=recordings makes the browse carry every
// candidate's tracks, so the count comparison and the answer come out of one call.
func (p *MusicBrainzProvider) bestFitTracklist(ctx context.Context, rgID string, localCount int) ([]TrackCandidate, error) {
	releases, err := p.browseReleaseGroupReleases(ctx, rgID)
	if err != nil {
		return nil, err
	}
	rel := pickReleaseByFit(releases, localCount)
	if rel == nil {
		return nil, ErrNoTracklist
	}
	tl := mbTracklist(rel.Media)
	if len(tl) == 0 {
		return nil, ErrNoTracklist
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
func pickEditionByFit(eds []ReleaseEdition, localCount int) int {
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
func earlierEdition(a, b ReleaseEdition) bool {
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
func mbEditions(releases []mbRelease) []ReleaseEdition {
	out := make([]ReleaseEdition, 0, len(releases))
	for i := range releases {
		r := &releases[i]
		out = append(out, ReleaseEdition{
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
func mbTracklist(media []mbMedium) []TrackCandidate {
	var tl []TrackCandidate
	for _, m := range media {
		disc := m.Position
		if disc == 0 {
			disc = 1
		}
		for _, tr := range m.Tracks {
			tl = append(tl, TrackCandidate{
				Disc: disc, Position: tr.Position, Title: tr.Title, ExternalID: tr.Recording.ID,
			})
		}
	}
	return tl
}

// releaseGroupForRelease resolves a MusicBrainz release (one edition — the entity a
// /release/ URL names) to its parent release-group and returns that release-group's
// decorated metadata, so a pasted release URL pins the album (release-group), matching
// how albums are identified. A 404 (stale/unknown release) flows out as ErrNoMatch via
// getJSON; a release with no parent group is likewise ErrNoMatch.
func (p *MusicBrainzProvider) releaseGroupForRelease(ctx context.Context, releaseID string) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("inc", "release-groups")
	q.Set("fmt", "json")
	var rel struct {
		ReleaseGroup struct {
			ID string `json:"id"`
		} `json:"release-group"`
	}
	if err := p.getJSON(ctx, "/release/"+url.PathEscape(releaseID), q, &rel); err != nil {
		return TitleMetadata{}, err
	}
	if strings.TrimSpace(rel.ReleaseGroup.ID) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	return p.releaseGroupByID(ctx, rel.ReleaseGroup.ID)
}

// parseMusicBrainzReleaseRef returns the release MBID when s is a MusicBrainz /release/
// URL (a specific edition, not itself an album pin — the caller resolves it to its
// parent release-group). Typed URLs only: a bare UUID is ambiguous (any entity), so it
// stays trusted for the item's own kind rather than being guessed as a release.
func parseMusicBrainzReleaseRef(s string) (id string, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	segs := strings.Split(s, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "release" && isUUID(segs[i+1]) {
			return strings.ToLower(segs[i+1]), true
		}
	}
	return "", false
}

// artistByID fetches a single artist by MBID (the durable artist override path) and
// returns its decorative metadata (name, synthesized overview, genres). An unknown
// id is ErrNoMatch, like a name search with no hits.
func (p *MusicBrainzProvider) artistByID(ctx context.Context, mbid string) (TitleMetadata, error) {
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
	if err := p.getJSON(ctx, "/artist/"+url.PathEscape(mbid), q, &a); err != nil {
		return TitleMetadata{}, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{Matched: true, Name: a.Name, ExternalID: a.ID, Source: "musicbrainz"}
	var parts []string
	if a.Disambiguation != "" {
		parts = append(parts, a.Disambiguation)
	} else if a.Type != "" {
		parts = append(parts, a.Type)
	}
	if a.Area.Name != "" {
		parts = append(parts, "from "+a.Area.Name)
	}
	meta.Overview = strings.Join(parts, " ")
	meta.Genres = topTags(a.Tags)
	return meta, nil
}

// releaseGroupByID fetches a single release-group by MBID (the durable album
// override path) and returns its decorative metadata (genres, year, cover art).
func (p *MusicBrainzProvider) releaseGroupByID(ctx context.Context, mbid string) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("inc", "tags")
	q.Set("fmt", "json")
	var rg struct {
		ID               string  `json:"id"`
		Title            string  `json:"title"`
		FirstReleaseDate string  `json:"first-release-date"`
		Tags             []mbTag `json:"tags"`
	}
	if err := p.getJSON(ctx, "/release-group/"+url.PathEscape(mbid), q, &rg); err != nil {
		return TitleMetadata{}, err
	}
	if strings.TrimSpace(rg.ID) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	meta := TitleMetadata{Matched: true, Name: rg.Title, ExternalID: rg.ID, Source: "musicbrainz"}
	meta.Genres = topTags(rg.Tags)
	if len(rg.FirstReleaseDate) >= 4 {
		if y, err := strconv.Atoi(rg.FirstReleaseDate[:4]); err == nil && y > 0 {
			meta.ReleaseDate = rg.FirstReleaseDate
		}
	}
	meta.Artwork = append(meta.Artwork, ArtworkRef{
		Role: "cover", URL: p.CoverArtURL + "/release-group/" + rg.ID + "/front-500",
	})
	return meta, nil
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

// mbEntityKind maps a MusicBrainz URL entity segment to our search/lookup kind, so
// a pasted typed URL can be validated against the item being corrected.
var mbEntityKind = map[string]string{
	"release-group": "album",
	"artist":        "artist",
	"recording":     "track",
}

// mbUnsupportedEntity are real MusicBrainz URL entity segments we can't use as an
// Enrichment override — an album is a release-group (not a work or a specific
// release), a track is a recording, etc. Recognized only so the paste box can say
// "wrong kind of record" instead of "unreadable". (`release` and `work` are the ones
// users hit most: a release is one edition of a release-group; a work is the abstract
// composition — neither identifies the album/artist/track we pin.)
var mbUnsupportedEntity = map[string]bool{
	"release": true, "work": true, "label": true, "area": true, "place": true,
	"event": true, "series": true, "instrument": true, "genre": true, "url": true,
	"editor": true, "collection": true,
}

// MusicBrainzRefUnsupported reports whether s is a MusicBrainz URL naming a real but
// unsupported entity type (work/release/label/…). Lets a caller distinguish "a valid
// MusicBrainz link, wrong entity kind" from "not a MusicBrainz reference at all".
func MusicBrainzRefUnsupported(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	segs := strings.Split(s, "/")
	for i := 0; i+1 < len(segs); i++ {
		if mbUnsupportedEntity[segs[i]] && isUUID(segs[i+1]) {
			return true
		}
	}
	return false
}

// ParseMusicBrainzRef reads a pasted MusicBrainz reference — a full URL
// (https://musicbrainz.org/release-group/<uuid>, /artist/<uuid>, /recording/<uuid>;
// any scheme/subdomain, optional slug/query/fragment) or a bare MBID (UUID) — into
// (kind, id) for the paste-a-MusicBrainz-ID/URL escape hatch (item-editing/search-
// improvements). For a typed URL kind is the matching item kind ("album"/"artist"/
// "track") so the caller can reject an id of the wrong kind; a bare UUID returns an
// empty kind (the caller assumes the item's own kind). ok is false when s is neither
// a UUID nor a recognized entity URL — the handler surfaces that as "unreadable".
func ParseMusicBrainzRef(s string) (kind, id string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if isUUID(s) {
		return "", strings.ToLower(s), true
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	segs := strings.Split(s, "/")
	for i := 0; i+1 < len(segs); i++ {
		if k, okk := mbEntityKind[segs[i]]; okk && isUUID(segs[i+1]) {
			return k, strings.ToLower(segs[i+1]), true
		}
	}
	return "", "", false
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID (a MusicBrainz MBID).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// recordingByID fetches a single recording by its MBID and returns its canonical
// title (applied display-only, never identity — ADR-0002). An unknown id is
// ErrNoMatch, like a name search with no hits.
func (p *MusicBrainzProvider) recordingByID(ctx context.Context, mbid string) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("fmt", "json")
	var out struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := p.getJSON(ctx, "/recording/"+url.PathEscape(mbid), q, &out); err != nil {
		return TitleMetadata{}, err
	}
	if strings.TrimSpace(out.Title) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	return TitleMetadata{Matched: true, Name: out.Title, ExternalID: out.ID, Source: "musicbrainz"}, nil
}

// client returns this provider's HTTP client, under the shared redirect policy —
// see providerClient in fetcher.go for why the JSON clients get it too.
func (p *MusicBrainzProvider) client() *http.Client {
	return providerClient(p.HTTPClient)
}

type mbTag struct {
	Name string `json:"name"`
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
func (p *MusicBrainzProvider) artistDetails(ctx context.Context, name string, albums []AlbumHint) (TitleMetadata, error) {
	id, err := p.artistIDFromAlbum(ctx, albums)
	if err != nil {
		return TitleMetadata{}, err
	}
	if id != "" {
		return p.artistByID(ctx, id)
	}
	return p.artistByName(ctx, name)
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
func (p *MusicBrainzProvider) artistIDFromAlbum(ctx context.Context, albums []AlbumHint) (string, error) {
	for _, h := range albums {
		// ADR-0049: an id is validated as a UUID before it is used. An unvalidated id
		// 404s, which here is indistinguishable from "this release-group is gone" and
		// would silently spend the one corroborating call on a typo.
		if isUUID(strings.TrimSpace(h.ReleaseGroupMBID)) {
			return p.artistIDFromReleaseGroup(ctx, strings.TrimSpace(h.ReleaseGroupMBID))
		}
	}
	for _, h := range albums {
		if strings.TrimSpace(h.Title) != "" {
			return p.artistIDFromAlbumSearch(ctx, h.Title)
		}
	}
	return "", nil
}

// artistIDFromReleaseGroup reads the artist credit off a release-group the FILES
// name — one lookup, no search (ADR-0053, and the lookup-beats-search preference of
// ADR-0049 applied one level up). A stale or merged id 404s, which getJSON maps to
// ErrNoMatch; that is "this album could not corroborate", not "this artist does not
// exist", so it falls through to the name search rather than settling the Artist.
func (p *MusicBrainzProvider) artistIDFromReleaseGroup(ctx context.Context, rgID string) (string, error) {
	q := url.Values{}
	q.Set("inc", "artist-credits")
	q.Set("fmt", "json")
	var rg struct {
		ArtistCredit []mbCredit `json:"artist-credit"`
	}
	if err := p.getJSON(ctx, "/release-group/"+url.PathEscape(rgID), q, &rg); err != nil {
		if errors.Is(err, ErrNoMatch) {
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
// The top hit is accepted only when its title matches the local one under
// normalizeMatchTitle — issue 03's matching normalizer, the same acceptance test
// ADR-0050 put on the track search — and only the TOP hit is considered, for the
// reason issue 05 gives: scanning down a ranked list is how a search quietly
// becomes "find me anything plausible". A rejected hit corroborates nothing and the
// artist falls back to its name.
func (p *MusicBrainzProvider) artistIDFromAlbumSearch(ctx context.Context, album string) (string, error) {
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
	if err := p.getJSON(ctx, "/release-group", q, &out); err != nil {
		if errors.Is(err, ErrNoMatch) {
			return "", nil
		}
		return "", err
	}
	if len(out.ReleaseGroups) == 0 {
		return "", nil
	}
	rg := out.ReleaseGroups[0]
	if !acceptsTitle(album, rg.Title) {
		return "", nil
	}
	return creditArtistID(rg.ArtistCredit), nil
}

// artistByName is the last tier of ADR-0053's precedence and is unchanged from the
// behaviour that predates it: an exact-phrase name search, top hit taken. It is
// only reached when nothing in the artist's discography could identify it, which is
// where it was always the honest answer.
func (p *MusicBrainzProvider) artistByName(ctx context.Context, name string) (TitleMetadata, error) {
	if strings.TrimSpace(name) == "" {
		return TitleMetadata{}, ErrNoMatch
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
	if err := p.getJSON(ctx, "/artist", q, &out); err != nil {
		return TitleMetadata{}, err
	}
	if len(out.Artists) == 0 {
		return TitleMetadata{}, ErrNoMatch
	}
	a := out.Artists[0]
	meta := TitleMetadata{Matched: true, ExternalID: a.ID, Source: "musicbrainz"}
	// MusicBrainz has no bio; synthesize a short overview from type + area +
	// disambiguation so the Artist page isn't bare (genres carry the real signal).
	var parts []string
	if a.Disambiguation != "" {
		parts = append(parts, a.Disambiguation)
	} else if a.Type != "" {
		parts = append(parts, a.Type)
	}
	if a.Area.Name != "" {
		parts = append(parts, "from "+a.Area.Name)
	}
	meta.Overview = strings.Join(parts, " ")
	meta.Genres = topTags(a.Tags)
	return meta, nil
}

func (p *MusicBrainzProvider) albumDetails(ctx context.Context, album, artist string) (TitleMetadata, error) {
	if strings.TrimSpace(album) == "" {
		return TitleMetadata{}, ErrNoMatch
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
	if err := p.getJSON(ctx, "/release-group", q, &out); err != nil {
		return TitleMetadata{}, err
	}
	if len(out.ReleaseGroups) == 0 {
		return TitleMetadata{}, ErrNoMatch
	}
	rg := out.ReleaseGroups[0]
	meta := TitleMetadata{Matched: true, ExternalID: rg.ID, Source: "musicbrainz"}
	meta.Genres = topTags(rg.Tags)
	if len(rg.FirstReleaseDate) >= 4 {
		if y, err := strconv.Atoi(rg.FirstReleaseDate[:4]); err == nil && y > 0 {
			meta.ReleaseDate = rg.FirstReleaseDate
		}
	}
	// Album cover from the Cover Art Archive (the ArtworkFetcher downloads it).
	// Request the 500px derivative, not the full-resolution "/front" original:
	// originals routinely exceed the fetcher's size cap, and 500px is ample for an
	// album cover in the grid and on the detail page.
	meta.Artwork = append(meta.Artwork, ArtworkRef{
		Role: "cover", URL: p.CoverArtURL + "/release-group/" + rg.ID + "/front-500",
	})
	return meta, nil
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
// the 730 unmatched tracks that prompted this. The interactive picker beside this
// had already moved off that shape, so the automatic matcher was strictly worse
// than the manual one it hands its failures to.
//
// THE ACCEPTANCE TEST IS WHAT MAKES THE SWAP PAYABLE. An exact phrase returning
// zero rows is HONESTLY empty; a relevance query essentially always returns
// something, so `Recordings[0]` applied blind would trade a queue row for a silent
// wrong overview — the confident-wrong-answer ADR-0049 ruled is the worse outcome.
// The top hit is therefore accepted only when its title matches the local track's
// under normalizeMatchTitle, the same matching normalizer the album tracklist rule
// uses (and deliberately NOT scanner.normalizeTitle, which serves identity keys).
// A rejected hit is ErrMatchRejected, which wraps ErrNoMatch: the row that results
// is exactly as honest as today's.
//
// ONE REQUEST, ALWAYS. No looser second query when the first comes back empty or
// is rejected. ADR-0049 measured MusicBrainz shedding load globally on the search
// cluster, and a retry issued precisely during those failures pushes the wrong
// way; the album tier already removed most of the traffic that would have wanted
// one.
func (p *MusicBrainzProvider) trackDetails(ctx context.Context, track, artist string) (TitleMetadata, error) {
	if strings.TrimSpace(track) == "" {
		return TitleMetadata{}, ErrNoMatch
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
	if err := p.getJSON(ctx, "/recording", q, &out); err != nil {
		return TitleMetadata{}, err
	}
	if len(out.Recordings) == 0 {
		// The source looked and has nothing. Plain ErrNoMatch — a different failure,
		// and a different remedy, from a hit we refused.
		return TitleMetadata{}, ErrNoMatch
	}
	r := out.Recordings[0]
	if !acceptsTitle(track, r.Title) {
		return TitleMetadata{}, ErrMatchRejected
	}
	// MusicBrainz has no track synopsis; only the canonical title is offered. The
	// service applies it as a display title ONLY where the tag title was sparse.
	return TitleMetadata{Matched: true, Name: r.Title, ExternalID: r.ID, Source: "musicbrainz"}, nil
}

// acceptsTitle is the acceptance test a search hit must pass before it becomes a
// Track's record: the candidate's title and the local one must be the same title
// under normalizeMatchTitle (case, diacritics, punctuation, bracket padding and the
// trailing decorations taggers and MusicBrainz disagree about all folded).
//
// A local title that normalizes to nothing — one that is punctuation only — never
// accepts. It would otherwise be equal to every other degenerate title the source
// holds, which is the same coin flip mapTracks refuses in rules 1 and 2.
func acceptsTitle(local, candidate string) bool {
	want := normalizeMatchTitle(local)
	return want != "" && want == normalizeMatchTitle(candidate)
}

// ArtworkCandidates lists the cover images the Cover Art Archive holds for an
// album (release-group), the Edit-item image picker's data for Music (Fix label,
// ADR-0019). Only the album kind has a listable image set (CAA is release-group
// keyed); an Artist/Track has none here, and a ref with no pinned MBID can't be
// listed, so those yield no candidates (never a fatal error). The "front" images
// are returned for any cover/poster role. Read-only.
func (p *MusicBrainzProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if ref.Kind != "album" || strings.TrimSpace(ref.MusicbrainzID) == "" {
		return nil, nil
	}
	// The Cover Art Archive is a distinct host from the MusicBrainz web service, so
	// it is fetched directly off CoverArtURL rather than via getJSON (which prefixes
	// BaseURL). Its release-group endpoint answers a JSON manifest of the images.
	u := p.CoverArtURL + "/release-group/" + url.PathEscape(ref.MusicbrainzID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("enrich: building cover-art request: %w", err)
	}
	req.Header.Set("User-Agent", p.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, requestError("cover-art", err)
	}
	defer resp.Body.Close()
	// A 404 is the normal "this release-group has no cover art" outcome — no images.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError("cover-art", "release-group", resp.StatusCode)
	}
	var out struct {
		Images []struct {
			Image      string            `json:"image"`
			Front      bool              `json:"front"`
			Thumbnails map[string]string `json:"thumbnails"`
		} `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, decodeError("cover-art", err)
	}
	cands := make([]ArtworkCandidate, 0, len(out.Images))
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
		cands = append(cands, ArtworkCandidate{URL: u, Source: "coverartarchive"})
	}
	return cands, nil
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

func (p *MusicBrainzProvider) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	u := p.BaseURL + path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		if err := p.throttle(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return fmt.Errorf("enrich: building musicbrainz request: %w", err)
		}
		req.Header.Set("User-Agent", p.UserAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := p.client().Do(req)
		if err != nil {
			return requestError("musicbrainz", err)
		}
		// A 404 is a definitive "no such record" (e.g. a pasted id that names a
		// different entity type, or a stale/merged MBID) — NOT a connectivity
		// failure. Map it to ErrNoMatch so callers surface "no record found" rather
		// than the alarming "source may be unreachable" (paste-id escape hatch).
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ErrNoMatch
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			refusal := readRefusal(resp)
			resp.Body.Close()
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
			wait, retryHere := p.inRequestRetry(refusal, attempt, maxAttempts)
			if retryHere {
				if err := sleepCtx(ctx, wait); err != nil {
					return err
				}
				continue
			}
			return refusalError("musicbrainz", path, refusal)
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return decodeError("musicbrainz", err)
		}
		return nil
	}
}

// throttle blocks until this HOST's minimum inter-request interval has elapsed,
// so the whole process — every Library's provider instance, and the global one —
// stays within MusicBrainz's ~1 req/sec policy together (ADR-0049). The limiter is
// keyed by host rather than held on the provider, because that is what the host
// counts; see hostthrottle.go for what the per-instance version got wrong.
func (p *MusicBrainzProvider) throttle(ctx context.Context) error {
	if p.MinInterval <= 0 {
		return nil
	}
	return throttleFor(p.BaseURL, p.MinInterval).wait(ctx)
}

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
func (p *MusicBrainzProvider) inRequestRetry(r ProviderRefusal, attempt, maxAttempts int) (time.Duration, bool) {
	if attempt >= maxAttempts || !retryableStatus(r.Status) {
		return 0, false
	}
	if after := retryAfter(r.Header, 0); after > 0 {
		return after, true
	}
	if r.ourQuota() {
		return time.Duration(attempt) * p.retryBackoff(), true
	}
	return 0, false
}

// retryBackoff is the base 503 delay, never zero: a provider built as a bare
// struct literal, or one whose operator disabled throttling, still backs off.
func (p *MusicBrainzProvider) retryBackoff() time.Duration {
	if p.RetryBackoff > 0 {
		return p.RetryBackoff
	}
	return defaultMusicBrainzRetryBackoff
}

// sleepCtx waits for d (no-op when d<=0), returning early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
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

// retryAfter reads a Retry-After header (integer seconds), falling back to the
// given duration when it is absent or unparseable.
func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return fallback
}
