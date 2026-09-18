// Package tmdb is the Obelo Metadata provider for The Movie Database, as a
// plugin (ADR-0059, .scratch/bundled-plugins issue 04).
//
// It is a port and not a rewrite. Every endpoint, every query parameter, every
// JSON shape and every mapping below came from internal/enrich/tmdb.go, which
// this file replaces — the same requests go out and the same records come back,
// which is the whole claim the conversion makes. What changed is where the bytes
// come from: there is no net/http here, no client, no timeout and no socket. The
// provider holds a [pluginsdk.Host] and asks it, so the same code runs inside the
// WebAssembly sandbox and in a native `go test` against an in-memory host.
//
// TMDB paces nothing. The Go provider had no throttle (ADR-0049 gave one to
// MusicBrainz and to nobody else), so this one has no [pluginsdk.Pacer] either —
// see main.go, which hands over a bare Sandbox rather than a PacedHost. An
// operator who sets a rate limit for TMDB on the providers screen is therefore
// setting a number this plugin does not read; that is the behaviour being
// preserved, and changing it is a separate issue rather than a ride-along.
//
// # Where an error goes
//
// Two different things can go wrong and they are answered differently, because
// the host does two different things with them:
//
//   - The HOST would not, or could not, make the request — a refusal, a transport
//     failure, or a fetch that ran out of the call's budget (ADR-0059 decision 6).
//     That is not TMDB's answer about this item at all, so it is
//     OutcomeUnavailable with the reason in Detail, NEVER OutcomeNoMatch. No
//     failure is counted against the plugin and the item takes ADR-0048's backoff.
//   - TMDB answered, and the answer was a non-2xx status or a document this code
//     cannot read. That is a Go error, exactly as the Go provider returned one, so
//     the host keeps treating it as the transport-or-plugin failure it is.
package tmdb

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Source is the slug this provider stamps on every record and artwork candidate
// it returns. It is the manifest id, and the host stores pins under it.
const Source = "tmdb"

// pageSize is TMDB's fixed search page size — the divisor that turns the picker's
// result offset into TMDB's 1-based page number.
const pageSize = 20

// Provider is the TMDB Metadata provider. It holds a [pluginsdk.Host] and nothing
// else: the API key, the language and both base URLs are read from
// Host.Settings() PER CALL, because that is where the host publishes them and
// because a secret is readable only while a call it belongs to is on the stack.
type Provider struct {
	host pluginsdk.Host
}

// The contract interfaces this provider fills. CapabilityEpisodeList is declared
// in the manifest and implemented here; the other two optional interfaces are
// not, and the SDK's dispatcher answers OutcomeUnavailable for them.
var (
	_ pluginapi.MetadataProvider = (*Provider)(nil)
	_ pluginapi.EpisodeLister    = (*Provider)(nil)
)

// New builds the provider on a Host. In the sandbox that Host is
// pluginsdk.Sandbox(); in a test it is sdktest.New(...). The provider cannot tell.
func New(h pluginsdk.Host) *Provider { return &Provider{host: h} }

// Host is the Host this provider was built on, so a test can assert what it did.
func (p *Provider) Host() pluginsdk.Host { return p.host }

// --- Lookup ------------------------------------------------------------------

// Lookup resolves ref to TMDB metadata, dispatching by kind. Movie/Show resolve
// the work (search by title+year unless an id is present); Season/Episode resolve
// the season/episode under the show id carried on the ref. With a TMDBID a record
// is fetched directly; otherwise a search takes the top result. A search with no
// results (or a missing show id for a season/episode) is OutcomeNoMatch. Music
// kinds are not TMDB's — they are OutcomeNoMatch too (the MusicBrainz plugin
// serves them).
func (p *Provider) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	ref := req.Ref
	s := p.host.Settings()

	switch ref.Kind {
	case "movie":
		id := ref.TMDBID
		if id == "" {
			found, err := p.searchMovie(ctx, s, ref.Title, ref.Year)
			if err != nil {
				return lookupFailure(err)
			}
			if found == "" {
				return noMatch(), nil
			}
			id = found
		}
		rec, err := p.movieDetails(ctx, s, id)
		if err != nil {
			return lookupFailure(err)
		}
		return matched(rec), nil
	case "show":
		id := ref.TMDBID
		if id == "" {
			found, err := p.searchTV(ctx, s, ref.Title, ref.Year)
			if err != nil {
				return lookupFailure(err)
			}
			if found == "" {
				return noMatch(), nil
			}
			id = found
		}
		rec, err := p.tvDetails(ctx, s, id)
		if err != nil {
			return lookupFailure(err)
		}
		return matched(rec), nil
	case "season":
		if ref.TMDBID == "" {
			return noMatch(), nil // no resolved show id → can't locate a season
		}
		rec, err := p.seasonDetails(ctx, s, ref.TMDBID, ref.SeasonNumber)
		if err != nil {
			return lookupFailure(err)
		}
		return matched(rec), nil
	case "episode":
		if ref.TMDBID == "" {
			return noMatch(), nil
		}
		rec, err := p.episodeDetails(ctx, s, ref.TMDBID, ref.SeasonNumber, ref.EpisodeNumber)
		if err != nil {
			return lookupFailure(err)
		}
		return matched(rec), nil
	default:
		return noMatch(), nil
	}
}

// --- Search ------------------------------------------------------------------

// Search returns TMDB candidates for a free-text query, dispatching by kind: a
// Movie query hits /search/movie; a TV query (show/season/episode) hits
// /search/tv — an Episode is corrected by re-pointing it at the right SHOW record
// (its season/episode numbers still locate the episode under it, exactly as the
// library pass threads the show id down). Each candidate carries the TMDB id to
// pin, the source title + year, a poster thumbnail, and the overview as the
// disambiguation hint. A blank query yields no candidates; an unsupported kind is
// OutcomeUnavailable, which is the Edit-item box's "this kind cannot be searched
// right now".
func (p *Provider) Search(ctx context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	s := p.host.Settings()

	var path string
	switch req.Kind {
	case "movie":
		path = "/search/movie"
	case "show", "season", "episode":
		path = "/search/tv"
	default:
		return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}

	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)
	q.Set("query", req.Query)
	// TMDB pages in fixed 20-result pages (no per-request limit), so translate an
	// offset into a 1-based page for the picker's "show more" — behavior is
	// unchanged for the default first page (Offset 0). Artist narrowing has no
	// TMDB analogue.
	if req.Offset > 0 {
		q.Set("page", strconv.Itoa(req.Offset/pageSize+1))
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
	if err := p.getJSON(ctx, s, path, q, &out); err != nil {
		if detail, ok := hostUnavailable(err); ok {
			return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.SearchResponse{}, err
	}
	cands := make([]pluginapi.SearchCandidate, 0, len(out.Results))
	for _, r := range out.Results {
		title := r.Title
		date := r.ReleaseDate
		if title == "" {
			title = r.Name // tv payload
		}
		if date == "" {
			date = r.FirstAirDate
		}
		c := pluginapi.SearchCandidate{
			ExternalID:     strconv.Itoa(r.ID),
			Title:          title,
			Year:           YearFromDate(date),
			Disambiguation: r.Overview,
			Kind:           req.Kind,
		}
		if r.PosterPath != "" {
			c.ThumbnailURL = s.URL2 + r.PosterPath
		}
		cands = append(cands, c)
	}
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// --- ArtworkCandidates --------------------------------------------------------

// ArtworkCandidates lists the images TMDB offers for a role on the record ref
// points at, so the Edit-item image picker can show them all (Fix label,
// ADR-0019). It dispatches by kind to the right /images endpoint and role: a
// Movie/Show poster → posters[], a background → backdrops[], a logo → logos[];
// an Episode's poster role is its still[] under the show id + season/episode
// numbers. A ref with no resolved TMDB id, or a record with no images for the
// role, yields no candidates (never a fatal error); an unsupported kind is
// OutcomeUnavailable, the picker's "not now". Read-only.
func (p *Provider) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	ref := req.Ref
	if ref.TMDBID == "" {
		// No resolved record → nothing to list, and no call made.
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
	}
	s := p.host.Settings()

	q := url.Values{}
	q.Set("api_key", s.Secret)
	// include_image_language widens the pool to language-less images too, so a
	// poster set isn't empty just because none is tagged for the UI language.
	q.Set("include_image_language", s.Language+",null")

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
		return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}

	var out struct {
		Posters   []image `json:"posters"`
		Backdrops []image `json:"backdrops"`
		Stills    []image `json:"stills"`
		Logos     []image `json:"logos"`
	}
	if err := p.getJSON(ctx, s, path, q, &out); err != nil {
		if detail, ok := hostUnavailable(err); ok {
			return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	// Map the requested role onto the source's image set: an Episode's poster role
	// is its still image (the title artwork endpoint only knows poster/background).
	var imgs []image
	switch req.Role {
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
	cands := make([]pluginapi.ArtworkCandidate, 0, len(imgs))
	for _, im := range imgs {
		// SVG logos are never offered: the pipeline stores raster images only.
		if im.FilePath == "" || isSVGImagePath(im.FilePath) {
			continue
		}
		cands = append(cands, pluginapi.ArtworkCandidate{
			URL:    s.URL2 + im.FilePath,
			Width:  im.Width,
			Height: im.Height,
			Source: Source,
		})
	}
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched, Candidates: cands}, nil
}

// --- EpisodeLister: picking WHICH provider episode decorates a file -----------

// SeriesSeasons lists a TMDB series' seasons so an Admin can choose the one their
// file actually belongs to. It exists because a provider's numbering and the
// numbering on disk can disagree — the run of episodes a provider moved into the
// next season being the common case — and enrichment otherwise looks the episode
// up by the on-disk numbers and fails forever.
func (p *Provider) SeriesSeasons(ctx context.Context, req pluginapi.SeriesSeasonsRequest) (pluginapi.SeriesSeasonsResponse, error) {
	s := p.host.Settings()
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)

	var out struct {
		Seasons []struct {
			SeasonNumber int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
	}
	if err := p.getJSON(ctx, s, "/tv/"+req.SeriesID, q, &out); err != nil {
		if detail, ok := hostUnavailable(err); ok {
			return pluginapi.SeriesSeasonsResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.SeriesSeasonsResponse{}, err
	}
	seasons := make([]pluginapi.SeasonSummary, 0, len(out.Seasons))
	for _, season := range out.Seasons {
		seasons = append(seasons, pluginapi.SeasonSummary{
			Season:       season.SeasonNumber,
			EpisodeCount: season.EpisodeCount,
		})
	}
	return pluginapi.SeriesSeasonsResponse{Outcome: pluginapi.OutcomeMatched, Seasons: seasons}, nil
}

// SeasonEpisodes lists one season's episodes in episode order, each with enough to
// recognize it on sight (name, air date, overview, still).
func (p *Provider) SeasonEpisodes(ctx context.Context, req pluginapi.SeasonEpisodesRequest) (pluginapi.SeasonEpisodesResponse, error) {
	s := p.host.Settings()
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)

	var out struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			SeasonNumber  int    `json:"season_number"`
			Name          string `json:"name"`
			Overview      string `json:"overview"`
			AirDate       string `json:"air_date"`
			StillPath     string `json:"still_path"`
		} `json:"episodes"`
	}
	path := "/tv/" + req.SeriesID + "/season/" + strconv.Itoa(req.Season)
	if err := p.getJSON(ctx, s, path, q, &out); err != nil {
		if detail, ok := hostUnavailable(err); ok {
			return pluginapi.SeasonEpisodesResponse{Outcome: pluginapi.OutcomeUnavailable, Detail: detail}, nil
		}
		return pluginapi.SeasonEpisodesResponse{}, err
	}
	eps := make([]pluginapi.EpisodeCandidate, 0, len(out.Episodes))
	for _, e := range out.Episodes {
		ep := pluginapi.EpisodeCandidate{
			Season:   e.SeasonNumber,
			Episode:  e.EpisodeNumber,
			Name:     e.Name,
			Overview: e.Overview,
			AirDate:  e.AirDate,
		}
		// TMDB reports the season on each episode, but a malformed payload could
		// omit it; fall back to the season we asked for so a pin is never written
		// against season 0 by accident.
		if ep.Season == 0 && req.Season != 0 {
			ep.Season = req.Season
		}
		if e.StillPath != "" {
			ep.StillURL = s.URL2 + e.StillPath
		}
		eps = append(eps, ep)
	}
	return pluginapi.SeasonEpisodesResponse{Outcome: pluginapi.OutcomeMatched, Episodes: eps}, nil
}

// --- The records --------------------------------------------------------------

// image is the subset of a TMDB /images entry the picker consumes.
type image struct {
	FilePath string `json:"file_path"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// named is TMDB's recurring {"name": …} object (a genre, a company, a network).
type named struct {
	Name string `json:"name"`
}

func (p *Provider) searchMovie(ctx context.Context, s pluginapi.Settings, title string, year int) (string, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)
	q.Set("query", title)
	if year > 0 {
		q.Set("year", strconv.Itoa(year))
	}
	var out struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, s, "/search/movie", q, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return strconv.Itoa(out.Results[0].ID), nil
}

func (p *Provider) searchTV(ctx context.Context, s pluginapi.Settings, title string, year int) (string, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)
	q.Set("query", title)
	if year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(year))
	}
	var out struct {
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if err := p.getJSON(ctx, s, "/search/tv", q, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", nil
	}
	return strconv.Itoa(out.Results[0].ID), nil
}

// movie is the subset of the TMDB movie-details payload we consume
// (append_to_response=credits,release_dates,images).
type movie struct {
	ID                  int     `json:"id"`
	Title               string  `json:"title"`
	Overview            string  `json:"overview"`
	Tagline             string  `json:"tagline"`
	ReleaseDate         string  `json:"release_date"`
	Runtime             int     `json:"runtime"`
	Genres              []named `json:"genres"`
	ProductionCompanies []named `json:"production_companies"`
	PosterPath          string  `json:"poster_path"`
	BackdropPath        string  `json:"backdrop_path"`
	Credits             struct {
		Cast []castMember `json:"cast"`
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
		Logos []image `json:"logos"`
	} `json:"images"`
}

// castMember is one entry of an appended `credits` block. A movie's and a show's
// are the same five fields and decode into the same normalized Credit.
type castMember struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Character   string `json:"character"`
	ProfilePath string `json:"profile_path"`
}

func (p *Provider) movieDetails(ctx context.Context, s pluginapi.Settings, id string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)
	q.Set("append_to_response", "credits,release_dates,images")
	// The appended images block is filtered by `language`; widen it to
	// language-less images too so a logo set isn't empty just because none is
	// tagged for the UI language (same widening ArtworkCandidates applies).
	q.Set("include_image_language", s.Language+",null")

	var m movie
	if err := p.getJSON(ctx, s, "/movie/"+id, q, &m); err != nil {
		return pluginapi.MetadataRecord{}, err
	}

	rec := pluginapi.MetadataRecord{
		Matched:        true,
		Name:           m.Title,
		Year:           YearFromDate(m.ReleaseDate),
		Overview:       m.Overview,
		Tagline:        m.Tagline,
		ReleaseDate:    m.ReleaseDate,
		RuntimeMinutes: m.Runtime,
		ContentRating:  usCertification(m),
		ExternalID:     strconv.Itoa(m.ID),
		Source:         Source,
	}
	if len(m.ProductionCompanies) > 0 {
		rec.Studio = m.ProductionCompanies[0].Name
	}
	for _, g := range m.Genres {
		rec.Genres = append(rec.Genres, g.Name)
	}
	rec.Cast = credits(m.Credits.Cast, s.URL2)
	if m.PosterPath != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: s.URL2 + m.PosterPath})
	}
	if m.BackdropPath != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "background", URL: s.URL2 + m.BackdropPath})
	}
	if logo := firstImagePath(m.Images.Logos); logo != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "logo", URL: s.URL2 + logo})
	}
	return rec, nil
}

// tv is the subset of the TMDB tv-details payload we consume
// (append_to_response=content_ratings,credits,images). The series-level `credits`
// block carries the show's main cast — the same id + profile_path per member a
// movie's credits do, decoded into the same normalized Credit shape.
type tv struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	FirstAirDate string  `json:"first_air_date"`
	Overview     string  `json:"overview"`
	Genres       []named `json:"genres"`
	Networks     []named `json:"networks"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Credits      struct {
		Cast []castMember `json:"cast"`
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
		Logos []image `json:"logos"`
	} `json:"images"`
}

func (p *Provider) tvDetails(ctx context.Context, s pluginapi.Settings, id string) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)
	q.Set("append_to_response", "content_ratings,credits,images")
	// Same language widening as movieDetails: appended images honor
	// include_image_language, without which a logo set could come back empty.
	q.Set("include_image_language", s.Language+",null")

	var m tv
	if err := p.getJSON(ctx, s, "/tv/"+id, q, &m); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	rec := pluginapi.MetadataRecord{
		Matched:       true,
		Name:          m.Name,
		Year:          YearFromDate(m.FirstAirDate),
		Overview:      m.Overview,
		ContentRating: usTVRating(m),
		ExternalID:    strconv.Itoa(m.ID),
		Source:        Source,
	}
	if len(m.Networks) > 0 {
		rec.Studio = m.Networks[0].Name // Studio carries the show's network
	}
	for _, g := range m.Genres {
		rec.Genres = append(rec.Genres, g.Name)
	}
	rec.Cast = credits(m.Credits.Cast, s.URL2)
	if m.PosterPath != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: s.URL2 + m.PosterPath})
	}
	if m.BackdropPath != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "background", URL: s.URL2 + m.BackdropPath})
	}
	if logo := firstImagePath(m.Images.Logos); logo != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "logo", URL: s.URL2 + logo})
	}
	return rec, nil
}

func (p *Provider) seasonDetails(ctx context.Context, s pluginapi.Settings, showID string, season int) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)

	var out struct {
		PosterPath string `json:"poster_path"`
		Overview   string `json:"overview"`
	}
	if err := p.getJSON(ctx, s, "/tv/"+showID+"/season/"+strconv.Itoa(season), q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	rec := pluginapi.MetadataRecord{Matched: true, Overview: out.Overview, Source: Source}
	if out.PosterPath != "" {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: s.URL2 + out.PosterPath})
	}
	return rec, nil
}

func (p *Provider) episodeDetails(ctx context.Context, s pluginapi.Settings, showID string, season, episode int) (pluginapi.MetadataRecord, error) {
	q := url.Values{}
	q.Set("api_key", s.Secret)
	q.Set("language", s.Language)

	var out struct {
		Name      string `json:"name"`
		Overview  string `json:"overview"`
		StillPath string `json:"still_path"`
	}
	path := "/tv/" + showID + "/season/" + strconv.Itoa(season) + "/episode/" + strconv.Itoa(episode)
	if err := p.getJSON(ctx, s, path, q, &out); err != nil {
		return pluginapi.MetadataRecord{}, err
	}
	rec := pluginapi.MetadataRecord{Matched: true, Name: out.Name, Overview: out.Overview, Source: Source}
	if out.StillPath != "" {
		// The episode still is its poster-role image (served by the title artwork
		// endpoint, which only knows poster/background roles).
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: "poster", URL: s.URL2 + out.StillPath})
	}
	return rec, nil
}

// credits normalizes an appended cast block. PersonRef is the source-namespaced
// stable person id that keys the person's headshot in the host's cache, so one
// actor's photo is stored once across every Title they appear in; ImageURL is
// built from the image base exactly like a poster URL.
func credits(cast []castMember, imageBase string) []pluginapi.Credit {
	if len(cast) == 0 {
		return nil
	}
	out := make([]pluginapi.Credit, 0, len(cast))
	for _, c := range cast {
		cr := pluginapi.Credit{Person: c.Name, Character: c.Character, Kind: "cast"}
		if c.ID != 0 {
			cr.PersonRef = Source + ":" + strconv.Itoa(c.ID)
		}
		if c.ProfilePath != "" {
			cr.ImageURL = imageBase + c.ProfilePath
		}
		out = append(out, cr)
	}
	return out
}

// usTVRating picks the US content rating from the content_ratings block, "" if
// absent. The Rating ceiling (CONTEXT.md) reads this when Members land.
func usTVRating(m tv) string {
	for _, r := range m.ContentRatings.Results {
		if r.Country == "US" && r.Rating != "" {
			return r.Rating
		}
	}
	return ""
}

// usCertification picks the US content rating from the release_dates block, "" if
// absent. The Rating ceiling (CONTEXT.md) reads this when Members land.
func usCertification(m movie) string {
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

// firstImagePath returns the first usable file_path in an appended images set —
// the default the details fetch auto-applies (TMDB orders images by rating, so
// the first is the one TMDB itself would show). SVG entries are skipped: the
// artwork pipeline is raster-only (ADR-0026), and TMDB logo sets mix SVG and PNG
// renditions of the same art.
func firstImagePath(imgs []image) string {
	for _, im := range imgs {
		if im.FilePath != "" && !isSVGImagePath(im.FilePath) {
			return im.FilePath
		}
	}
	return ""
}

// isSVGImagePath reports whether a TMDB image file_path is an SVG. TMDB serves
// logos as PNG or SVG (posters/backdrops are always raster); SVG is excluded from
// auto-picks and candidate lists because catalog artwork is raster-only
// (ADR-0026: a format that won't render everywhere never becomes catalog art —
// and an SVG cached under a raster extension serves with the wrong content-type
// and renders nowhere).
func isSVGImagePath(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".svg")
}

// YearFromDate extracts the 4-digit year from a TMDB date string ("YYYY-MM-DD",
// or just "YYYY"); 0 when absent/unparseable. Used to surface a release year for
// by-id identity resolution.
//
// Exported so a test can state the rule directly; the provider is the only caller.
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

// getJSON issues a GET against the operator's TMDB base URL and decodes the JSON
// body into out. A non-2xx is an error, exactly as it was for the Go provider.
func (p *Provider) getJSON(ctx context.Context, s pluginapi.Settings, path string, q url.Values, out any) error {
	return pluginsdk.GetJSON(ctx, p.host, s.URL+path, q, out)
}

// hostUnavailable reports whether a fetch failure is the HOST's rather than
// TMDB's, and returns the sentence to put in Detail when it is.
//
// The two that qualify are a REFUSAL (the allowlist, or an address this server
// will not talk to) and a fetch the host attempted and could not complete —
// which includes the one ADR-0059 decision 6 exists for: "the fetch did not
// finish before this call's deadline". Neither says anything about the item, so
// neither may become OutcomeNoMatch, and neither is worth a Go error that the
// host would count as a failure against this plugin.
//
// A non-2xx status and an unreadable document are NOT here. TMDB answered; that
// is the source's business and it travels as the Go error the port preserves.
func hostUnavailable(err error) (string, bool) {
	var fe *pluginsdk.FetchError
	if !errors.As(err, &fe) {
		return "", false
	}
	if fe.IsRefusal() || fe.Transport != "" {
		return fe.Error(), true
	}
	return "", false
}

// lookupFailure turns a fetch failure into either the unavailable answer or the
// Go error, per the package comment.
func lookupFailure(err error) (pluginapi.LookupResponse, error) {
	if detail, ok := hostUnavailable(err); ok {
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
