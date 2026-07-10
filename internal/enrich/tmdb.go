package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TMDBProvider is the production MetadataProvider for the Movie/TV kinds: it
// resolves a parsed identity against The Movie Database (TMDB) over HTTP and
// normalizes the response into a provider-agnostic TitleMetadata. All TMDB
// specifics — endpoints, JSON shapes, search-vs-id resolution, language — live
// here, behind the MetadataProvider seam, so the rest of the system never sees
// HTTP. Determinism/offline posture (ADR-0001): a no-match or a network failure
// is a normal outcome the service degrades over, never a fatal error.
//
// This is the secondary, lower seam: its HTTP/parse layer is unit-tested with an
// httptest server (tmdb_test.go); the project's black-box tests use a fake
// provider, never the live network.
type TMDBProvider struct {
	APIKey       string
	Language     string
	BaseURL      string // e.g. https://api.themoviedb.org/3
	ImageBaseURL string // e.g. https://image.tmdb.org/t/p/original
	HTTPClient   *http.Client
}

// NewTMDBProvider builds a provider from config. A nil HTTP client gets a default
// with a sane timeout (the real network is best-effort; a slow lookup must not
// hang a pass).
func NewTMDBProvider(apiKey, language, baseURL, imageBaseURL string) *TMDBProvider {
	return &TMDBProvider{
		APIKey:       apiKey,
		Language:     language,
		BaseURL:      baseURL,
		ImageBaseURL: imageBaseURL,
		HTTPClient:   &http.Client{Timeout: 15 * time.Second},
	}
}

// Lookup resolves ref to TMDB metadata, dispatching by kind. Movie/Show resolve
// the work (search by title+year unless an id is present); Season/Episode resolve
// the season/episode under the show id carried on the ref. With a TMDBID a record
// is fetched directly; otherwise a search takes the top result. A search with no
// results (or a missing show id for a season/episode) returns ErrNoMatch. Music
// kinds are not TMDB's — they return ErrNoMatch (the MusicBrainz provider serves
// them via the CompositeProvider).
func (p *TMDBProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie":
		id := ref.TMDBID
		if id == "" {
			found, err := p.searchMovie(ctx, ref.Title, ref.Year)
			if err != nil {
				return TitleMetadata{}, err
			}
			if found == "" {
				return TitleMetadata{}, ErrNoMatch
			}
			id = found
		}
		return p.movieDetails(ctx, id)
	case "show":
		id := ref.TMDBID
		if id == "" {
			found, err := p.searchTV(ctx, ref.Title, ref.Year)
			if err != nil {
				return TitleMetadata{}, err
			}
			if found == "" {
				return TitleMetadata{}, ErrNoMatch
			}
			id = found
		}
		return p.tvDetails(ctx, id)
	case "season":
		if ref.TMDBID == "" {
			return TitleMetadata{}, ErrNoMatch // no resolved show id → can't locate a season
		}
		return p.seasonDetails(ctx, ref.TMDBID, ref.SeasonNumber)
	case "episode":
		if ref.TMDBID == "" {
			return TitleMetadata{}, ErrNoMatch
		}
		return p.episodeDetails(ctx, ref.TMDBID, ref.SeasonNumber, ref.EpisodeNumber)
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

// tmdbPageSize is TMDB's fixed search page size — the divisor that turns the
// picker's result offset into TMDB's 1-based page number.
const tmdbPageSize = 20

// parseTMDBRef reads a pasted TMDB reference — a themoviedb.org URL (/movie/<id> or
// /tv/<id>, the id optionally carrying a "-slug" suffix; any scheme/subdomain,
// optional query/fragment) or a bare numeric id — into (urlKind, id) for the paste
// escape hatch (item-editing/search-improvements). urlKind is "movie" or "tv" for a
// typed URL (so the caller can validate movie↔movie / tv↔show|season|episode), or ""
// for a bare id (the caller assumes the item's kind). ok is false when s is neither a
// positive integer nor a recognized TMDB entity URL.
func parseTMDBRef(s string) (urlKind, id string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if isDigits(s) {
		return "", s, true
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	segs := strings.Split(s, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "movie" || segs[i] == "tv" {
			num := segs[i+1]
			if j := strings.IndexByte(num, '-'); j > 0 {
				num = num[:j] // strip the "-slug" TMDB appends to shareable URLs
			}
			if isDigits(num) {
				return segs[i], num, true
			}
		}
	}
	return "", "", false
}

// isDigits reports whether s is a non-empty run of ASCII digits (a TMDB id).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Search returns TMDB candidates for a free-text query, dispatching by kind: a
// Movie query hits /search/movie; a TV query (show/season/episode) hits
// /search/tv — an Episode is corrected by re-pointing it at the right SHOW record
// (its season/episode numbers still locate the episode under it, exactly as the
// library pass threads the show id down). Each candidate carries the TMDB id to
// pin, the source title + year, a poster thumbnail, and the overview as the
// disambiguation hint. A blank query or an unsupported kind yields no candidates.
func (p *TMDBProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	var path string
	switch kind {
	case "movie":
		path = "/search/movie"
	case "show", "season", "episode":
		path = "/search/tv"
	default:
		return nil, ErrSearchUnavailable
	}

	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)
	q.Set("query", query)
	// TMDB pages in fixed 20-result pages (no per-request limit), so translate an
	// offset into a 1-based page for the picker's "show more" — behavior is unchanged
	// for the default first page (Offset 0). Artist narrowing has no TMDB analogue.
	if opts.Offset > 0 {
		q.Set("page", strconv.Itoa(opts.Offset/tmdbPageSize+1))
	}
	var out struct {
		Results []struct {
			ID           int    `json:"id"`
			Title        string `json:"title"`          // movie
			Name         string `json:"name"`           // tv
			ReleaseDate  string `json:"release_date"`   // movie
			FirstAirDate string `json:"first_air_date"` // tv
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, path, q, &out); err != nil {
		return nil, err
	}
	cands := make([]Candidate, 0, len(out.Results))
	for _, r := range out.Results {
		title := r.Title
		date := r.ReleaseDate
		if title == "" {
			title = r.Name // tv payload
		}
		if date == "" {
			date = r.FirstAirDate
		}
		c := Candidate{
			ExternalID:     strconv.Itoa(r.ID),
			Title:          title,
			Year:           yearFromDate(date),
			Disambiguation: r.Overview,
			Kind:           kind,
		}
		if r.PosterPath != "" {
			c.ThumbnailURL = p.ImageBaseURL + r.PosterPath
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// ArtworkCandidates lists the images TMDB offers for a role on the record ref
// points at, so the Edit-item image picker can show them all (Fix label,
// ADR-0019). It dispatches by kind to the right /images endpoint and role: a
// Movie/Show poster → posters[], a background → backdrops[], a logo → logos[];
// an Episode's poster role is its still[] under the show id + season/episode
// numbers. A ref with no resolved TMDB id, an unsupported kind, or a record
// with no images for the role yields no candidates (never a fatal error).
// Read-only.
func (p *TMDBProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if ref.TMDBID == "" {
		return nil, nil // no resolved record → nothing to list
	}
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	// include_image_language widens the pool to language-less images too, so a
	// poster set isn't empty just because none is tagged for the UI language.
	q.Set("include_image_language", p.Language+",null")

	var path string
	switch ref.Kind {
	case "movie":
		path = "/movie/" + ref.TMDBID + "/images"
	case "show":
		path = "/tv/" + ref.TMDBID + "/images"
	case "season":
		path = "/tv/" + ref.TMDBID + "/season/" + strconv.Itoa(ref.SeasonNumber) + "/images"
	case "episode":
		path = "/tv/" + ref.TMDBID + "/season/" + strconv.Itoa(ref.SeasonNumber) +
			"/episode/" + strconv.Itoa(ref.EpisodeNumber) + "/images"
	default:
		return nil, ErrSearchUnavailable
	}

	var out struct {
		Posters   []tmdbImage `json:"posters"`
		Backdrops []tmdbImage `json:"backdrops"`
		Stills    []tmdbImage `json:"stills"`
		Logos     []tmdbImage `json:"logos"`
	}
	if err := p.getJSON(ctx, path, q, &out); err != nil {
		return nil, err
	}
	// Map the requested role onto the source's image set: an Episode's poster role
	// is its still image (the title artwork endpoint only knows poster/background).
	var imgs []tmdbImage
	switch role {
	case "background":
		imgs = out.Backdrops
	case "logo":
		imgs = out.Logos
	default: // "poster"
		if ref.Kind == "episode" {
			imgs = out.Stills
		} else {
			imgs = out.Posters
		}
	}
	cands := make([]ArtworkCandidate, 0, len(imgs))
	for _, im := range imgs {
		// SVG logos are never offered: the pipeline stores raster images only.
		if im.FilePath == "" || isSVGImagePath(im.FilePath) {
			continue
		}
		cands = append(cands, ArtworkCandidate{
			URL:    p.ImageBaseURL + im.FilePath,
			Width:  im.Width,
			Height: im.Height,
			Source: "tmdb",
		})
	}
	return cands, nil
}

// tmdbImage is the subset of a TMDB /images entry the picker consumes.
type tmdbImage struct {
	FilePath string `json:"file_path"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

func (p *TMDBProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

func (p *TMDBProvider) searchMovie(ctx context.Context, title string, year int) (string, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)
	q.Set("query", title)
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var out struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, "/search/movie", q, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return strconv.Itoa(out.Results[0].ID), nil
}

// tmdbMovie is the subset of the TMDB movie-details payload we consume
// (append_to_response=credits,release_dates,images).
type tmdbMovie struct {
	ID                  int     `json:"id"`
	Title               string  `json:"title"`
	Overview            string  `json:"overview"`
	Tagline             string  `json:"tagline"`
	ReleaseDate         string  `json:"release_date"`
	Runtime             int     `json:"runtime"`
	Genres              []tName `json:"genres"`
	ProductionCompanies []tName `json:"production_companies"`
	PosterPath          string  `json:"poster_path"`
	BackdropPath        string  `json:"backdrop_path"`
	Credits             struct {
		Cast []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			Character   string `json:"character"`
			ProfilePath string `json:"profile_path"`
		} `json:"cast"`
	} `json:"credits"`
	ReleaseDates struct {
		Results []struct {
			Country      string `json:"iso_3166_1"`
			ReleaseDates []struct {
				Certification string `json:"certification"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
	// Logos are only exposed via the appended images block (there is no top-level
	// logo_path the way poster_path/backdrop_path are).
	Images struct {
		Logos []tmdbImage `json:"logos"`
	} `json:"images"`
}

type tName struct {
	Name string `json:"name"`
}

func (p *TMDBProvider) movieDetails(ctx context.Context, id string) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)
	q.Set("append_to_response", "credits,release_dates,images")
	// The appended images block is filtered by `language`; widen it to
	// language-less images too so a logo set isn't empty just because none is
	// tagged for the UI language (same widening ArtworkCandidates applies).
	q.Set("include_image_language", p.Language+",null")

	var m tmdbMovie
	if err := p.getJSON(ctx, "/movie/"+id, q, &m); err != nil {
		return TitleMetadata{}, err
	}

	meta := TitleMetadata{
		Matched:        true,
		Name:           m.Title,
		Year:           yearFromDate(m.ReleaseDate),
		Overview:       m.Overview,
		Tagline:        m.Tagline,
		ReleaseDate:    m.ReleaseDate,
		RuntimeMinutes: m.Runtime,
		ContentRating:  usCertification(m),
		ExternalID:     strconv.Itoa(m.ID),
		Source:         "tmdb",
	}
	if len(m.ProductionCompanies) > 0 {
		meta.Studio = m.ProductionCompanies[0].Name
	}
	for _, g := range m.Genres {
		meta.Genres = append(meta.Genres, g.Name)
	}
	for _, c := range m.Credits.Cast {
		cr := Credit{Person: c.Name, Character: c.Character, Kind: "cast"}
		if c.ID != 0 {
			// Provider-namespaced person ref keys the headshot (dedupe across titles).
			cr.PersonRef = "tmdb:" + strconv.Itoa(c.ID)
		}
		if c.ProfilePath != "" {
			// Built from the image base exactly like a poster URL (mirrors the
			// *_path → ImageBaseURL construction above).
			cr.ImageURL = p.ImageBaseURL + c.ProfilePath
		}
		meta.Cast = append(meta.Cast, cr)
	}
	if m.PosterPath != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: p.ImageBaseURL + m.PosterPath})
	}
	if m.BackdropPath != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "background", URL: p.ImageBaseURL + m.BackdropPath})
	}
	if logo := firstImagePath(m.Images.Logos); logo != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "logo", URL: p.ImageBaseURL + logo})
	}
	return meta, nil
}

// firstImagePath returns the first usable file_path in an appended images set —
// the default the details fetch auto-applies (TMDB orders images by rating, so
// the first is the one TMDB itself would show). SVG entries are skipped: the
// artwork pipeline is raster-only (see isSVGImagePath), and TMDB logo sets mix
// SVG and PNG renditions of the same art.
func firstImagePath(imgs []tmdbImage) string {
	for _, im := range imgs {
		if im.FilePath != "" && !isSVGImagePath(im.FilePath) {
			return im.FilePath
		}
	}
	return ""
}

// isSVGImagePath reports whether a TMDB image file_path is an SVG. TMDB serves
// logos as PNG or SVG (posters/backdrops are always raster); SVG is excluded
// from auto-picks and candidate lists because catalog artwork is raster-only
// (ADR-0026: a format that won't render everywhere never becomes catalog art —
// and an SVG cached under a raster extension serves with the wrong content-type
// and renders nowhere).
func isSVGImagePath(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".svg")
}

func (p *TMDBProvider) searchTV(ctx context.Context, title string, year int) (string, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)
	q.Set("query", title)
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}
	var out struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, "/search/tv", q, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return strconv.Itoa(out.Results[0].ID), nil
}

// tmdbTV is the subset of the TMDB tv-details payload we consume
// (append_to_response=content_ratings,credits,images). The series-level `credits` block
// carries the show's main cast (cast-photos/02) — the same id + profile_path per
// member a movie's credits do, decoded into the same normalized Credit shape.
type tmdbTV struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	FirstAirDate string  `json:"first_air_date"`
	Overview     string  `json:"overview"`
	Genres       []tName `json:"genres"`
	Networks     []tName `json:"networks"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Credits      struct {
		Cast []struct {
			ID          int    `json:"id"`
			Name        string `json:"name"`
			Character   string `json:"character"`
			ProfilePath string `json:"profile_path"`
		} `json:"cast"`
	} `json:"credits"`
	ContentRatings struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
	// Logos are only exposed via the appended images block (there is no top-level
	// logo_path the way poster_path/backdrop_path are).
	Images struct {
		Logos []tmdbImage `json:"logos"`
	} `json:"images"`
}

func (p *TMDBProvider) tvDetails(ctx context.Context, id string) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)
	q.Set("append_to_response", "content_ratings,credits,images")
	// Same language widening as movieDetails: appended images honor
	// include_image_language, without which a logo set could come back empty.
	q.Set("include_image_language", p.Language+",null")

	var m tmdbTV
	if err := p.getJSON(ctx, "/tv/"+id, q, &m); err != nil {
		return TitleMetadata{}, err
	}
	meta := TitleMetadata{
		Matched:       true,
		Name:          m.Name,
		Year:          yearFromDate(m.FirstAirDate),
		Overview:      m.Overview,
		ContentRating: usTVRating(m),
		ExternalID:    strconv.Itoa(m.ID),
		Source:        "tmdb",
	}
	if len(m.Networks) > 0 {
		meta.Studio = m.Networks[0].Name // Studio carries the show's network
	}
	for _, g := range m.Genres {
		meta.Genres = append(meta.Genres, g.Name)
	}
	for _, c := range m.Credits.Cast {
		cr := Credit{Person: c.Name, Character: c.Character, Kind: "cast"}
		if c.ID != 0 {
			// Provider-namespaced person ref keys the headshot (dedupe across titles).
			cr.PersonRef = "tmdb:" + strconv.Itoa(c.ID)
		}
		if c.ProfilePath != "" {
			// Built from the image base exactly like a poster URL (mirrors the movie
			// credits decode above).
			cr.ImageURL = p.ImageBaseURL + c.ProfilePath
		}
		meta.Cast = append(meta.Cast, cr)
	}
	if m.PosterPath != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: p.ImageBaseURL + m.PosterPath})
	}
	if m.BackdropPath != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "background", URL: p.ImageBaseURL + m.BackdropPath})
	}
	if logo := firstImagePath(m.Images.Logos); logo != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "logo", URL: p.ImageBaseURL + logo})
	}
	return meta, nil
}

func (p *TMDBProvider) seasonDetails(ctx context.Context, showID string, season int) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)

	var out struct {
		PosterPath string `json:"poster_path"`
		Overview   string `json:"overview"`
	}
	if err := p.getJSON(ctx, "/tv/"+showID+"/season/"+strconv.Itoa(season), q, &out); err != nil {
		return TitleMetadata{}, err
	}
	meta := TitleMetadata{Matched: true, Overview: out.Overview, Source: "tmdb"}
	if out.PosterPath != "" {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: p.ImageBaseURL + out.PosterPath})
	}
	return meta, nil
}

func (p *TMDBProvider) episodeDetails(ctx context.Context, showID string, season, episode int) (TitleMetadata, error) {
	q := url.Values{}
	q.Set("api_key", p.APIKey)
	q.Set("language", p.Language)

	var out struct {
		Name      string `json:"name"`
		Overview  string `json:"overview"`
		StillPath string `json:"still_path"`
	}
	path := "/tv/" + showID + "/season/" + strconv.Itoa(season) + "/episode/" + strconv.Itoa(episode)
	if err := p.getJSON(ctx, path, q, &out); err != nil {
		return TitleMetadata{}, err
	}
	meta := TitleMetadata{Matched: true, Name: out.Name, Overview: out.Overview, Source: "tmdb"}
	if out.StillPath != "" {
		// The episode still is its poster-role image (served by the title artwork
		// endpoint, which only knows poster/background roles).
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: "poster", URL: p.ImageBaseURL + out.StillPath})
	}
	return meta, nil
}

// usTVRating picks the US content rating from the content_ratings block, "" if
// absent. The Rating ceiling (CONTEXT.md) reads this when Members land.
func usTVRating(m tmdbTV) string {
	for _, r := range m.ContentRatings.Results {
		if r.Country == "US" && r.Rating != "" {
			return r.Rating
		}
	}
	return ""
}

// usCertification picks the US content rating from the release_dates block, "" if
// absent. The Rating ceiling (CONTEXT.md) reads this when Members land.
func usCertification(m tmdbMovie) string {
	for _, r := range m.ReleaseDates.Results {
		if r.Country != "US" {
			continue
		}
		for _, rd := range r.ReleaseDates {
			if rd.Certification != "" {
				return rd.Certification
			}
		}
	}
	return ""
}

// yearFromDate extracts the 4-digit year from a TMDB date string ("YYYY-MM-DD",
// or just "YYYY"); 0 when absent/unparseable. Used to surface a release year for
// by-id identity resolution (Service.ResolveIdentity).
func yearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

// getJSON issues a GET against the TMDB base URL and decodes the JSON body into
// out. A non-2xx is an error (the service records the Title 'failed' and moves on).
func (p *TMDBProvider) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	u := p.BaseURL + path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("enrich: building tmdb request: %w", err)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("enrich: tmdb request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("enrich: tmdb %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("enrich: decoding tmdb response: %w", err)
	}
	return nil
}
