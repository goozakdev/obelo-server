// TMDB and OMDb stand-ins for the differential run.
//
// Both handlers answer the EXACT requests the two implementations under
// comparison make — the plugins in plugins/tmdb/tmdb and plugins/omdb/omdb, and
// the Go providers they replaced (internal/enrich/tmdb.go, internal/enrich/omdb.go)
// — from fixed, canned JSON. Nothing here reads a clock, a map or a random
// source: the same request always produces byte-identical bytes, which is the
// only reason a differential run can attribute a difference to the code rather
// than to the stand-in.
//
// The fixtures are two works and nothing else:
//
//	movie  "Back to the Future" (1985)  TMDB 105   IMDb tt0088763
//	show   "Breaking Bad"       (2008)  TMDB 1396  IMDb tt0903747, S1E1 "Pilot"
//
// A KNOWN-SHAPED request about anything else gets the source's own empty/not-found
// answer (an empty results page, TMDB's 404 error document, OMDb's
// {"Response":"False"}) — never unmatched, because "no such movie" is a real answer
// both implementations must handle identically. Only a request whose SHAPE is
// unrecognized reaches unmatched(), where it is logged for a human to look at.
package main

import (
	"net/http"
	"strconv"
	"strings"
)

// The fixture identities. Every canned body below is about one of these two.
const (
	tmdbMovieID  = "105"
	tmdbShowID   = "1396"
	tmdbSeasonNo = 1
	tmdbEpisode  = 1

	tmdbMovieTitle = "Back to the Future"
	tmdbShowTitle  = "Breaking Bad"

	omdbMovieIMDBID = "tt0088763"
)

// --- TMDB ---------------------------------------------------------------------

// tmdbHandler serves the stand-in TMDB API. r.URL.Path has already had the mount
// prefix stripped by main.go, so it begins at "/search/movie", "/movie/105", etc.
//
// The route table is the union of every path the two implementations build:
//
//	Provider.Search / TMDBProvider.Search          -> /search/movie, /search/tv
//	searchMovie / searchTV                         -> /search/movie, /search/tv
//	movieDetails                                   -> /movie/{id}
//	tvDetails, SeriesSeasons                       -> /tv/{id}
//	seasonDetails, SeasonEpisodes                  -> /tv/{id}/season/{n}
//	episodeDetails                                 -> /tv/{id}/season/{n}/episode/{m}
//	ArtworkCandidates                              -> any of the above + "/images"
func tmdbHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		unmatched(w, r, "tmdb")
		return
	}
	segs := tmdbSegments(r.URL.Path)

	// Provider.Search (tmdb.go, Search) and searchMovie/searchTV: TMDB's search
	// endpoints. The title must match the fixture; every other query is a real,
	// well-formed "no results" page, which is what drives the OutcomeNoMatch arm.
	if len(segs) == 2 && segs[0] == "search" {
		query := strings.TrimSpace(r.URL.Query().Get("query"))
		switch segs[1] {
		case "movie":
			if strings.EqualFold(query, tmdbMovieTitle) {
				writeRawJSON(w, http.StatusOK, tmdbMovieSearchJSON)
				return
			}
			writeRawJSON(w, http.StatusOK, tmdbEmptySearchJSON)
			return
		case "tv":
			if strings.EqualFold(query, tmdbShowTitle) {
				writeRawJSON(w, http.StatusOK, tmdbTVSearchJSON)
				return
			}
			writeRawJSON(w, http.StatusOK, tmdbEmptySearchJSON)
			return
		}
		unmatched(w, r, "tmdb")
		return
	}

	switch {
	// movieDetails (tmdb.go, movieDetails; old tmdb.go, movieDetails) — the
	// append_to_response=credits,release_dates,images blocks are all embedded.
	case len(segs) == 2 && segs[0] == "movie":
		if !tmdbIsID(segs[1]) {
			break
		}
		if segs[1] != tmdbMovieID {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbMovieDetailsJSON)
		return

	// ArtworkCandidates, kind "movie" (tmdb.go, ArtworkCandidates).
	case len(segs) == 3 && segs[0] == "movie" && segs[2] == "images":
		if !tmdbIsID(segs[1]) {
			break
		}
		if segs[1] != tmdbMovieID {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbMovieImagesJSON)
		return

	// tvDetails (tmdb.go, tvDetails) AND SeriesSeasons (tmdb.go, SeriesSeasons):
	// the same path, so the body carries both the details fields and the
	// "seasons" array SeriesSeasons reads.
	case len(segs) == 2 && segs[0] == "tv":
		if !tmdbIsID(segs[1]) {
			break
		}
		if segs[1] != tmdbShowID {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbTVDetailsJSON)
		return

	// ArtworkCandidates, kind "show".
	case len(segs) == 3 && segs[0] == "tv" && segs[2] == "images":
		if !tmdbIsID(segs[1]) {
			break
		}
		if segs[1] != tmdbShowID {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbTVImagesJSON)
		return

	// seasonDetails (tmdb.go, seasonDetails) AND SeasonEpisodes (tmdb.go,
	// SeasonEpisodes): again one path, so the season object carries its own
	// poster_path/overview and the full "episodes" array.
	case len(segs) == 4 && segs[0] == "tv" && segs[2] == "season":
		if !tmdbIsID(segs[1]) || !tmdbIsNumber(segs[3]) {
			break
		}
		if !tmdbIsFixtureSeason(segs[1], segs[3]) {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbSeasonJSON)
		return

	// ArtworkCandidates, kind "season".
	case len(segs) == 5 && segs[0] == "tv" && segs[2] == "season" && segs[4] == "images":
		if !tmdbIsID(segs[1]) || !tmdbIsNumber(segs[3]) {
			break
		}
		if !tmdbIsFixtureSeason(segs[1], segs[3]) {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbSeasonImagesJSON)
		return

	// episodeDetails (tmdb.go, episodeDetails).
	case len(segs) == 6 && segs[0] == "tv" && segs[2] == "season" && segs[4] == "episode":
		if !tmdbIsID(segs[1]) || !tmdbIsNumber(segs[3]) || !tmdbIsNumber(segs[5]) {
			break
		}
		if !tmdbIsFixtureEpisode(segs[1], segs[3], segs[5]) {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbEpisodeJSON)
		return

	// ArtworkCandidates, kind "episode" — the poster role reads stills[].
	case len(segs) == 7 && segs[0] == "tv" && segs[2] == "season" && segs[4] == "episode" && segs[6] == "images":
		if !tmdbIsID(segs[1]) || !tmdbIsNumber(segs[3]) || !tmdbIsNumber(segs[5]) {
			break
		}
		if !tmdbIsFixtureEpisode(segs[1], segs[3], segs[5]) {
			tmdbNotFound(w)
			return
		}
		writeRawJSON(w, http.StatusOK, tmdbEpisodeImagesJSON)
		return
	}

	unmatched(w, r, "tmdb")
}

// tmdbSegments splits a request path into its non-empty segments.
func tmdbSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// tmdbIsNumber reports whether s is a non-negative decimal integer — a TMDB id, a
// season number or an episode number. A segment that is not one means the path
// shape is not TMDB's, so the caller routes it to unmatched rather than to a 404.
func tmdbIsNumber(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

// tmdbIsID is tmdbIsNumber under the name the id positions read better with.
func tmdbIsID(s string) bool { return tmdbIsNumber(s) }

func tmdbIsFixtureSeason(showID, season string) bool {
	return showID == tmdbShowID && season == strconv.Itoa(tmdbSeasonNo)
}

func tmdbIsFixtureEpisode(showID, season, episode string) bool {
	return tmdbIsFixtureSeason(showID, season) && episode == strconv.Itoa(tmdbEpisode)
}

// tmdbNotFound is TMDB's own 404 document for a well-formed request about a
// record it does not have. Both implementations treat a 404 as "TMDB answered,
// and the answer is no use" (a Go error / a parked item), so this body is the
// fixture for that whole arm.
func tmdbNotFound(w http.ResponseWriter) {
	writeRawJSON(w, http.StatusNotFound, tmdbNotFoundJSON)
}

const tmdbNotFoundJSON = `{
  "success": false,
  "status_code": 34,
  "status_message": "The resource you requested could not be found."
}`

const tmdbEmptySearchJSON = `{
  "page": 1,
  "results": [],
  "total_pages": 1,
  "total_results": 0
}`

// tmdbMovieSearchJSON — one result, read by Search (id/title/release_date/
// overview/poster_path) and by searchMovie (results[0].id only).
const tmdbMovieSearchJSON = `{
  "page": 1,
  "results": [
    {
      "id": 105,
      "title": "Back to the Future",
      "original_title": "Back to the Future",
      "release_date": "1985-07-03",
      "overview": "Eighties teenager Marty McFly is accidentally sent back in time to 1955, where he must make sure his high-school-aged parents fall in love so he can get back to the future.",
      "poster_path": "/btf_poster.jpg",
      "backdrop_path": "/btf_backdrop.jpg",
      "adult": false,
      "popularity": 42.5,
      "vote_average": 8.3,
      "vote_count": 19850
    }
  ],
  "total_pages": 1,
  "total_results": 1
}`

// tmdbTVSearchJSON — one result, read by Search (id/name/first_air_date/
// overview/poster_path) and by searchTV (results[0].id only).
const tmdbTVSearchJSON = `{
  "page": 1,
  "results": [
    {
      "id": 1396,
      "name": "Breaking Bad",
      "original_name": "Breaking Bad",
      "first_air_date": "2008-01-20",
      "overview": "A high school chemistry teacher diagnosed with terminal cancer turns to manufacturing methamphetamine to secure his family's future.",
      "poster_path": "/bb_poster.jpg",
      "backdrop_path": "/bb_backdrop.jpg",
      "popularity": 96.1,
      "vote_average": 8.9,
      "vote_count": 12040
    }
  ],
  "total_pages": 1,
  "total_results": 1
}`

// tmdbMovieDetailsJSON is GET /movie/105 with
// append_to_response=credits,release_dates,images — every appended block
// movieDetails reads:
//
//	credits.cast[]            -> Credit{Person,Character,PersonRef,ImageURL}
//	release_dates.results[]   -> usCertification picks the US certification ("PG";
//	                             the GB entry is here so the US pick is a real pick)
//	images.logos[]            -> firstImagePath -> the logo ArtworkRef
//
// imdb_id/external_ids are not read by either implementation; they are present
// because the real payload carries them and a downstream chain may key off them.
const tmdbMovieDetailsJSON = `{
  "id": 105,
  "imdb_id": "tt0088763",
  "title": "Back to the Future",
  "original_title": "Back to the Future",
  "original_language": "en",
  "overview": "Eighties teenager Marty McFly is accidentally sent back in time to 1955, where he must make sure his high-school-aged parents fall in love so he can get back to the future.",
  "tagline": "He was never in time for his classes. He wasn't in time for his dinner. Then one day, he wasn't in his time at all.",
  "release_date": "1985-07-03",
  "runtime": 116,
  "status": "Released",
  "adult": false,
  "budget": 19000000,
  "revenue": 381109762,
  "popularity": 42.5,
  "vote_average": 8.3,
  "vote_count": 19850,
  "homepage": "",
  "genres": [
    {"id": 12, "name": "Adventure"},
    {"id": 35, "name": "Comedy"},
    {"id": 878, "name": "Science Fiction"}
  ],
  "production_companies": [
    {"id": 33, "name": "Universal Pictures", "origin_country": "US"},
    {"id": 56, "name": "Amblin Entertainment", "origin_country": "US"}
  ],
  "production_countries": [{"iso_3166_1": "US", "name": "United States of America"}],
  "spoken_languages": [{"iso_639_1": "en", "name": "English"}],
  "poster_path": "/btf_poster.jpg",
  "backdrop_path": "/btf_backdrop.jpg",
  "external_ids": {
    "imdb_id": "tt0088763",
    "wikidata_id": "Q91540",
    "facebook_id": null,
    "instagram_id": null,
    "twitter_id": null
  },
  "credits": {
    "cast": [
      {
        "id": 1101,
        "name": "Michael J. Fox",
        "original_name": "Michael J. Fox",
        "character": "Marty McFly",
        "credit_id": "btf-cast-1101",
        "profile_path": "/btf_profile_fox.jpg",
        "order": 0,
        "known_for_department": "Acting"
      },
      {
        "id": 1102,
        "name": "Christopher Lloyd",
        "original_name": "Christopher Lloyd",
        "character": "Dr. Emmett Brown",
        "credit_id": "btf-cast-1102",
        "profile_path": "/btf_profile_lloyd.jpg",
        "order": 1,
        "known_for_department": "Acting"
      },
      {
        "id": 1103,
        "name": "Lea Thompson",
        "original_name": "Lea Thompson",
        "character": "Lorraine Baines",
        "credit_id": "btf-cast-1103",
        "profile_path": "/btf_profile_thompson.jpg",
        "order": 2,
        "known_for_department": "Acting"
      },
      {
        "id": 1104,
        "name": "Crispin Glover",
        "original_name": "Crispin Glover",
        "character": "George McFly",
        "credit_id": "btf-cast-1104",
        "profile_path": null,
        "order": 3,
        "known_for_department": "Acting"
      }
    ],
    "crew": [
      {
        "id": 1201,
        "name": "Robert Zemeckis",
        "job": "Director",
        "department": "Directing",
        "credit_id": "btf-crew-1201",
        "profile_path": "/btf_profile_zemeckis.jpg"
      }
    ]
  },
  "release_dates": {
    "results": [
      {
        "iso_3166_1": "GB",
        "release_dates": [
          {"certification": "12A", "iso_639_1": "", "release_date": "1985-12-04T00:00:00.000Z", "type": 3, "note": ""}
        ]
      },
      {
        "iso_3166_1": "US",
        "release_dates": [
          {"certification": "PG", "iso_639_1": "", "release_date": "1985-07-03T00:00:00.000Z", "type": 3, "note": ""}
        ]
      }
    ]
  },
  "images": {
    "posters": [
      {"file_path": "/btf_poster.jpg", "width": 2000, "height": 3000, "aspect_ratio": 0.667, "iso_639_1": "en", "vote_average": 5.5, "vote_count": 12},
      {"file_path": "/btf_poster_alt.jpg", "width": 1000, "height": 1500, "aspect_ratio": 0.667, "iso_639_1": null, "vote_average": 5.1, "vote_count": 4}
    ],
    "backdrops": [
      {"file_path": "/btf_backdrop.jpg", "width": 3840, "height": 2160, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.4, "vote_count": 9}
    ],
    "logos": [
      {"file_path": "/btf_logo.jpg", "width": 1600, "height": 620, "aspect_ratio": 2.581, "iso_639_1": "en", "vote_average": 5.3, "vote_count": 6}
    ]
  }
}`

// tmdbMovieImagesJSON is GET /movie/105/images — the standalone set
// ArtworkCandidates lists for the poster/background/logo roles.
const tmdbMovieImagesJSON = `{
  "id": 105,
  "posters": [
    {"file_path": "/btf_poster.jpg", "width": 2000, "height": 3000, "aspect_ratio": 0.667, "iso_639_1": "en", "vote_average": 5.5, "vote_count": 12},
    {"file_path": "/btf_poster_alt.jpg", "width": 1000, "height": 1500, "aspect_ratio": 0.667, "iso_639_1": null, "vote_average": 5.1, "vote_count": 4}
  ],
  "backdrops": [
    {"file_path": "/btf_backdrop.jpg", "width": 3840, "height": 2160, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.4, "vote_count": 9},
    {"file_path": "/btf_backdrop_alt.jpg", "width": 1920, "height": 1080, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.0, "vote_count": 3}
  ],
  "logos": [
    {"file_path": "/btf_logo.jpg", "width": 1600, "height": 620, "aspect_ratio": 2.581, "iso_639_1": "en", "vote_average": 5.3, "vote_count": 6}
  ]
}`

// tmdbTVDetailsJSON is GET /tv/1396 with
// append_to_response=content_ratings,credits,images. It serves two callers:
//
//	tvDetails      -> name, first_air_date, overview, genres, networks,
//	                  poster_path, backdrop_path, credits.cast[],
//	                  content_ratings.results[] (US "TV-MA"), images.logos[]
//	SeriesSeasons  -> seasons[].season_number + seasons[].episode_count
//
// Only season 1 is listed, because season 1 is the only season this stand-in can
// answer a details request for — a listed season that 404s would be a fixture
// bug, not a difference between implementations.
const tmdbTVDetailsJSON = `{
  "id": 1396,
  "name": "Breaking Bad",
  "original_name": "Breaking Bad",
  "original_language": "en",
  "first_air_date": "2008-01-20",
  "last_air_date": "2013-09-29",
  "overview": "A high school chemistry teacher diagnosed with terminal cancer turns to manufacturing methamphetamine to secure his family's future.",
  "tagline": "Change the equation.",
  "status": "Ended",
  "type": "Scripted",
  "in_production": false,
  "number_of_seasons": 1,
  "number_of_episodes": 7,
  "episode_run_time": [47],
  "popularity": 96.1,
  "vote_average": 8.9,
  "vote_count": 12040,
  "homepage": "",
  "poster_path": "/bb_poster.jpg",
  "backdrop_path": "/bb_backdrop.jpg",
  "genres": [
    {"id": 18, "name": "Drama"},
    {"id": 80, "name": "Crime"}
  ],
  "networks": [
    {"id": 174, "name": "AMC", "origin_country": "US"}
  ],
  "production_companies": [
    {"id": 11073, "name": "Sony Pictures Television", "origin_country": "US"}
  ],
  "origin_country": ["US"],
  "seasons": [
    {
      "id": 3572,
      "name": "Season 1",
      "season_number": 1,
      "episode_count": 7,
      "air_date": "2008-01-20",
      "overview": "Walter White, a struggling chemistry teacher, turns to cooking methamphetamine after a terminal diagnosis.",
      "poster_path": "/bb_season1_poster.jpg"
    }
  ],
  "external_ids": {
    "imdb_id": "tt0903747",
    "tvdb_id": 81189,
    "wikidata_id": "Q1079",
    "facebook_id": null,
    "instagram_id": null,
    "twitter_id": null
  },
  "content_ratings": {
    "results": [
      {"iso_3166_1": "GB", "rating": "18", "descriptors": []},
      {"iso_3166_1": "US", "rating": "TV-MA", "descriptors": []}
    ]
  },
  "credits": {
    "cast": [
      {
        "id": 2101,
        "name": "Bryan Cranston",
        "original_name": "Bryan Cranston",
        "character": "Walter White",
        "credit_id": "bb-cast-2101",
        "profile_path": "/bb_profile_cranston.jpg",
        "order": 0,
        "known_for_department": "Acting"
      },
      {
        "id": 2102,
        "name": "Aaron Paul",
        "original_name": "Aaron Paul",
        "character": "Jesse Pinkman",
        "credit_id": "bb-cast-2102",
        "profile_path": "/bb_profile_paul.jpg",
        "order": 1,
        "known_for_department": "Acting"
      },
      {
        "id": 2103,
        "name": "Anna Gunn",
        "original_name": "Anna Gunn",
        "character": "Skyler White",
        "credit_id": "bb-cast-2103",
        "profile_path": null,
        "order": 2,
        "known_for_department": "Acting"
      }
    ],
    "crew": [
      {
        "id": 2201,
        "name": "Vince Gilligan",
        "job": "Creator",
        "department": "Writing",
        "credit_id": "bb-crew-2201",
        "profile_path": "/bb_profile_gilligan.jpg"
      }
    ]
  },
  "images": {
    "posters": [
      {"file_path": "/bb_poster.jpg", "width": 2000, "height": 3000, "aspect_ratio": 0.667, "iso_639_1": "en", "vote_average": 5.6, "vote_count": 14}
    ],
    "backdrops": [
      {"file_path": "/bb_backdrop.jpg", "width": 3840, "height": 2160, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.2, "vote_count": 7}
    ],
    "logos": [
      {"file_path": "/bb_logo.jpg", "width": 1600, "height": 620, "aspect_ratio": 2.581, "iso_639_1": "en", "vote_average": 5.1, "vote_count": 5}
    ]
  }
}`

// tmdbTVImagesJSON is GET /tv/1396/images — ArtworkCandidates, kind "show".
const tmdbTVImagesJSON = `{
  "id": 1396,
  "posters": [
    {"file_path": "/bb_poster.jpg", "width": 2000, "height": 3000, "aspect_ratio": 0.667, "iso_639_1": "en", "vote_average": 5.6, "vote_count": 14},
    {"file_path": "/bb_poster_alt.jpg", "width": 1000, "height": 1500, "aspect_ratio": 0.667, "iso_639_1": null, "vote_average": 5.0, "vote_count": 2}
  ],
  "backdrops": [
    {"file_path": "/bb_backdrop.jpg", "width": 3840, "height": 2160, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.2, "vote_count": 7}
  ],
  "logos": [
    {"file_path": "/bb_logo.jpg", "width": 1600, "height": 620, "aspect_ratio": 2.581, "iso_639_1": "en", "vote_average": 5.1, "vote_count": 5}
  ]
}`

// tmdbSeasonJSON is GET /tv/1396/season/1. Two callers again:
//
//	seasonDetails   -> poster_path + overview
//	SeasonEpisodes  -> episodes[].{episode_number,season_number,name,overview,
//	                   air_date,still_path}
const tmdbSeasonJSON = `{
  "_id": "bb-season-1",
  "id": 3572,
  "name": "Season 1",
  "season_number": 1,
  "air_date": "2008-01-20",
  "overview": "Walter White, a struggling chemistry teacher, turns to cooking methamphetamine after a terminal diagnosis.",
  "poster_path": "/bb_season1_poster.jpg",
  "vote_average": 8.2,
  "episodes": [
    {
      "id": 62085,
      "name": "Pilot",
      "overview": "Diagnosed with terminal lung cancer, chemistry teacher Walter White teams with a former student to cook methamphetamine.",
      "air_date": "2008-01-20",
      "episode_number": 1,
      "season_number": 1,
      "episode_type": "standard",
      "production_code": "BB-101",
      "runtime": 58,
      "show_id": 1396,
      "still_path": "/bb_s01e01_still.jpg",
      "vote_average": 8.2,
      "vote_count": 320,
      "crew": [],
      "guest_stars": []
    }
  ]
}`

// tmdbSeasonImagesJSON is GET /tv/1396/season/1/images — ArtworkCandidates,
// kind "season" (TMDB offers posters only for a season).
const tmdbSeasonImagesJSON = `{
  "id": 3572,
  "posters": [
    {"file_path": "/bb_season1_poster.jpg", "width": 2000, "height": 3000, "aspect_ratio": 0.667, "iso_639_1": "en", "vote_average": 5.3, "vote_count": 6},
    {"file_path": "/bb_season1_poster_alt.jpg", "width": 1000, "height": 1500, "aspect_ratio": 0.667, "iso_639_1": null, "vote_average": 5.0, "vote_count": 2}
  ]
}`

// tmdbEpisodeJSON is GET /tv/1396/season/1/episode/1 — episodeDetails reads
// name, overview and still_path (the still is the episode's poster-role image).
const tmdbEpisodeJSON = `{
  "id": 62085,
  "name": "Pilot",
  "overview": "Diagnosed with terminal lung cancer, chemistry teacher Walter White teams with a former student to cook methamphetamine.",
  "air_date": "2008-01-20",
  "episode_number": 1,
  "season_number": 1,
  "episode_type": "standard",
  "production_code": "BB-101",
  "runtime": 58,
  "show_id": 1396,
  "still_path": "/bb_s01e01_still.jpg",
  "vote_average": 8.2,
  "vote_count": 320,
  "crew": [],
  "guest_stars": []
}`

// tmdbEpisodeImagesJSON is GET /tv/1396/season/1/episode/1/images —
// ArtworkCandidates, kind "episode": the poster role reads stills[].
const tmdbEpisodeImagesJSON = `{
  "id": 62085,
  "stills": [
    {"file_path": "/bb_s01e01_still.jpg", "width": 1920, "height": 1080, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.4, "vote_count": 8},
    {"file_path": "/bb_s01e01_still_alt.jpg", "width": 1280, "height": 720, "aspect_ratio": 1.778, "iso_639_1": null, "vote_average": 5.0, "vote_count": 3}
  ]
}`

// --- OMDb ---------------------------------------------------------------------

// omdbHandler serves the stand-in OMDb API. The real OMDb serves everything from
// "/" with query parameters, so r.URL.Path will normally be "/".
//
// The only request either implementation makes is the one built in
// Provider.fetch (plugins/omdb/omdb/omdb.go, fetch) and OMDbProvider.fetch
// (internal/enrich/omdb.go, fetch): GET base + "/?" + {apikey, and then either
// i=<imdb id> or t=<title>[&y=<year>]}. Only Response/Plot/Rated/Genre are read
// back, but the whole record is served because that is what OMDb returns and a
// difference in parsing would otherwise be invisible.
func omdbHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		unmatched(w, r, "omdb")
		return
	}
	// OMDb is entirely query-driven; a path other than the root is not a shape
	// either implementation produces, so it is worth a human's attention.
	if p := strings.Trim(r.URL.Path, "/"); p != "" {
		unmatched(w, r, "omdb")
		return
	}

	q := r.URL.Query()
	// The real OMDb rejects a keyless request with 401 before looking at anything
	// else. Both implementations classify a 401 as "our request is wrong" — a Go
	// error and a parked item, never a retry — so this arm is a fixture for that.
	if strings.TrimSpace(q.Get("apikey")) == "" {
		writeRawJSON(w, http.StatusUnauthorized, omdbNoKeyJSON)
		return
	}

	imdbID := strings.TrimSpace(q.Get("i"))
	title := strings.TrimSpace(q.Get("t"))
	if imdbID == omdbMovieIMDBID || strings.EqualFold(title, tmdbMovieTitle) {
		writeRawJSON(w, http.StatusOK, omdbMovieJSON)
		return
	}
	// OMDb answers "no such record" with HTTP 200 and Response:"False". Both
	// implementations turn that into OutcomeNoMatch and cache it as the zero
	// result — it is an answer, not a failure, so it is never a 404 here.
	writeRawJSON(w, http.StatusOK, omdbNotFoundJSON)
}

const omdbNoKeyJSON = `{"Response":"False","Error":"No API key provided."}`

const omdbNotFoundJSON = `{"Response":"False","Error":"Movie not found!"}`

// omdbMovieJSON is the full record for the movie fixture. The fill-only fields
// the providers consume are Plot (-> Overview), Rated (-> ContentRating) and
// Genre (-> the comma-split Genres slice).
const omdbMovieJSON = `{
  "Title": "Back to the Future",
  "Year": "1985",
  "Rated": "PG",
  "Released": "03 Jul 1985",
  "Runtime": "116 min",
  "Genre": "Adventure, Comedy, Sci-Fi",
  "Director": "Robert Zemeckis",
  "Writer": "Robert Zemeckis, Bob Gale",
  "Actors": "Michael J. Fox, Christopher Lloyd, Lea Thompson",
  "Plot": "Marty McFly, a teenager from 1985, is sent thirty years into the past by his friend Doc Brown's time machine and has to make his parents fall in love before he can return.",
  "Language": "English",
  "Country": "United States",
  "Awards": "Won 1 Oscar. 20 wins & 25 nominations total",
  "Poster": "%%BASE%%/img/omdb_tt0088763_poster.jpg",
  "Ratings": [
    {"Source": "Internet Movie Database", "Value": "8.5/10"},
    {"Source": "Rotten Tomatoes", "Value": "93%"},
    {"Source": "Metacritic", "Value": "87/100"}
  ],
  "Metascore": "87",
  "imdbRating": "8.5",
  "imdbVotes": "1,280,000",
  "imdbID": "tt0088763",
  "Type": "movie",
  "DVD": "N/A",
  "BoxOffice": "$212,836,762",
  "Production": "N/A",
  "Website": "N/A",
  "Response": "True"
}`
