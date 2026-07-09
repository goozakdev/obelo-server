package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

	mu   sync.Mutex
	next time.Time // earliest instant the next request may start
}

const defaultMusicBrainzInterval = time.Second // MusicBrainz allows ~1 req/sec.

// NewMusicBrainzProvider builds a provider from config. A nil HTTP client gets a
// default with a sane timeout (a slow lookup must not hang a pass).
func NewMusicBrainzProvider(baseURL, coverArtURL, language string) *MusicBrainzProvider {
	return &MusicBrainzProvider{
		BaseURL:     baseURL,
		CoverArtURL: coverArtURL,
		Language:    language,
		UserAgent:   "juicebox/1.0 (self-hosted)",
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		MinInterval: defaultMusicBrainzInterval,
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
		return p.artistDetails(ctx, ref.Title)
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
// to the top instead. An optional artist term is AND-ed in as a field-scoped clause —
// the verified relevance-safe narrowing pattern — to focus a broad common title.
func musicQuery(terms, artist string) string {
	q := escapeLucene(terms)
	if a := strings.TrimSpace(artist); a != "" {
		q += ` AND artist:"` + escapeLucene(a) + `"`
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
	// improvements). opts.Artist AND-narrows to a specific artist when supplied.
	q.Set("query", musicQuery(query, opts.Artist))
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
	q.Set("query", musicQuery(query, ""))
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
	// matched nothing. opts.Artist AND-narrows to a specific artist when supplied.
	q.Set("query", musicQuery(query, opts.Artist))
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
// ordered tracks (disc + position + title) as the album candidate's preview.
func (p *MusicBrainzProvider) releaseGroupTracklist(ctx context.Context, rgID string) ([]TrackCandidate, error) {
	q := url.Values{}
	q.Set("release-group", rgID)
	q.Set("inc", "recordings")
	q.Set("limit", "1")
	q.Set("fmt", "json")
	var out struct {
		Releases []struct {
			Media []struct {
				Position int `json:"position"`
				Tracks   []struct {
					Number    string `json:"number"`
					Position  int    `json:"position"`
					Title     string `json:"title"`
					Recording struct {
						ID string `json:"id"`
					} `json:"recording"`
				} `json:"tracks"`
			} `json:"media"`
		} `json:"releases"`
	}
	if err := p.getJSON(ctx, "/release", q, &out); err != nil {
		return nil, err
	}
	if len(out.Releases) == 0 {
		return nil, nil
	}
	var tl []TrackCandidate
	for _, m := range out.Releases[0].Media {
		disc := m.Position
		if disc == 0 {
			disc = 1
		}
		for _, tr := range m.Tracks {
			// The recording MBID (inc=recordings) is the per-track durable pin the
			// slice-05 cascade writes so each mapped track survives a later pass.
			tl = append(tl, TrackCandidate{
				Disc: disc, Position: tr.Position, Title: tr.Title, ExternalID: tr.Recording.ID,
			})
		}
	}
	return tl, nil
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

func (p *MusicBrainzProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

type mbTag struct {
	Name string `json:"name"`
}

// mbCredit is one entry of a MusicBrainz artist-credit: an artist name plus the join
// phrase that links it to the next (" & ", " feat. ", ", ", …). The last entry's phrase
// is empty. See creditString.
type mbCredit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
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

func (p *MusicBrainzProvider) artistDetails(ctx context.Context, name string) (TitleMetadata, error) {
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

func (p *MusicBrainzProvider) trackDetails(ctx context.Context, track, artist string) (TitleMetadata, error) {
	if strings.TrimSpace(track) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	query := `recording:"` + track + `"`
	if artist != "" {
		query += ` AND artist:"` + artist + `"`
	}
	q := url.Values{}
	q.Set("query", query)
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
		return TitleMetadata{}, ErrNoMatch
	}
	r := out.Recordings[0]
	// MusicBrainz has no track synopsis; only the canonical title is offered. The
	// service applies it as a display title ONLY where the tag title was sparse.
	return TitleMetadata{Matched: true, Name: r.Title, ExternalID: r.ID, Source: "musicbrainz"}, nil
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
		return nil, fmt.Errorf("enrich: cover-art request: %w", err)
	}
	defer resp.Body.Close()
	// A 404 is the normal "this release-group has no cover art" outcome — no images.
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enrich: cover-art release-group: status %d", resp.StatusCode)
	}
	var out struct {
		Images []struct {
			Image      string            `json:"image"`
			Front      bool              `json:"front"`
			Thumbnails map[string]string `json:"thumbnails"`
		} `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("enrich: decoding cover-art response: %w", err)
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
			return fmt.Errorf("enrich: musicbrainz request: %w", err)
		}
		// 503 means we were throttled (or MusicBrainz is briefly unavailable): back
		// off and retry a few times rather than dropping the lookup.
		if resp.StatusCode == http.StatusServiceUnavailable && attempt < maxAttempts {
			resp.Body.Close()
			if err := sleepCtx(ctx, retryAfter(resp.Header, time.Duration(attempt)*p.MinInterval)); err != nil {
				return err
			}
			continue
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
			resp.Body.Close()
			return fmt.Errorf("enrich: musicbrainz %s: status %d", path, resp.StatusCode)
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("enrich: decoding musicbrainz response: %w", err)
		}
		return nil
	}
}

// throttle blocks until the provider's minimum inter-request interval has elapsed
// since the previous request, serializing callers so the whole process stays
// within MusicBrainz's ~1 req/sec policy. It reserves its slot under the lock and
// waits outside it, honoring context cancellation.
func (p *MusicBrainzProvider) throttle(ctx context.Context) error {
	if p.MinInterval <= 0 {
		return nil
	}
	p.mu.Lock()
	start := time.Now()
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(p.MinInterval)
	p.mu.Unlock()
	return sleepCtx(ctx, time.Until(start))
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
