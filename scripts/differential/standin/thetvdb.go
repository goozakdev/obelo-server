package main

// Stand-in for TheTVDB's v4 API, serving the one fixture identity both builds of
// the differential run are pointed at: "Breaking Bad", series 81189, first aired
// 2008-01-20, IMDb tt0903747, with season 1 episode 1 "Pilot".
//
// It exists to force BOTH implementations down the same wire path:
//
//   - the plugin provider, plugins/thetvdb/thetvdb/thetvdb.go, and
//   - the old Go provider, internal/enrich/thetvdb.go (TheTVDBProvider).
//
// The two issue byte-for-byte identical requests — same endpoints, same query
// parameters, same bearer-token login dance — so one stand-in satisfies both and
// any difference in the records they produce is a difference in THEM, not in what
// they were served.
//
// Every body below is a raw string literal and nothing in here consults the clock,
// a random source or a map iteration order, so the same request always produces
// the same bytes. Artwork URLs are absolute in the real API, so they are emitted
// as %%BASE%%/img/<name>.jpg and writeRawJSON rewrites the token to the stand-in's
// own origin, pointing them back at its image mount.

import (
	"encoding/json"
	"net/http"
	"strings"
)

// thetvdbToken is the bearer token POST /login hands out. Both providers mint a
// token on first use and send it as "Authorization: Bearer <token>" on every data
// request (thetvdb.go ensureToken/getJSON; internal/enrich/thetvdb.go the same),
// so a data request that does not carry exactly this token is answered 401 — which
// is how the stand-in guarantees neither build can skip the login dance.
const thetvdbToken = "standin-token"

// thetvdbSeriesID is the one series this stand-in knows about.
const thetvdbSeriesID = "81189"

// thetvdbHandler serves the stand-in TheTVDB v4 API. r.URL.Path has already had
// the mount prefix stripped by main.go, so it begins at "/login", "/search",
// "/series/81189", etc.
func thetvdbHandler(w http.ResponseWriter, r *http.Request) {
	path := "/" + strings.Trim(r.URL.Path, "/")

	// --- the login dance -----------------------------------------------------
	//
	// plugins/thetvdb/thetvdb/thetvdb.go login(): POSTs {"apikey": s.Secret} as
	// application/json to <base>/login and reads data.token back.
	// internal/enrich/thetvdb.go login(): identical body, identical read.
	if path == "/login" {
		if r.Method != http.MethodPost {
			unmatched(w, r, "thetvdb") // a GET /login is not a shape either build produces
			return
		}
		thetvdbLogin(w, r)
		return
	}

	// --- every other endpoint is authed --------------------------------------
	//
	// getJSON() in both implementations sets "Authorization: Bearer <tok>" on
	// every data request. A 401 here is read by both as "the token is stale":
	// the plugin drops the cached token and retries once (statusUnauthorized in
	// getJSON), the Go provider does the same. Answering 401 to a missing or
	// wrong token is what forces the login above to have happened.
	if !thetvdbAuthorized(r) {
		writeRawJSON(w, http.StatusUnauthorized, thetvdbUnauthorizedJSON)
		return
	}

	if r.Method != http.MethodGet {
		unmatched(w, r, "thetvdb") // both providers only ever GET a data endpoint
		return
	}

	switch {
	// --- GET /search?query=…&type=series -------------------------------------
	//
	// seriesID() in both implementations: url.Values{query: ref.Title,
	// type: "series"} against "/search", reading only data[0].tvdb_id. An empty
	// data array is the "no such series" answer both turn into a no-match.
	case path == "/search":
		thetvdbSearch(w, r)
		return

	// --- GET /series/{id} ----------------------------------------------------
	//
	// showRecord(): "/series/"+id, reading data.name, data.overview, data.image
	// and data.genres[].name.
	case path == "/series/"+thetvdbSeriesID:
		writeRawJSON(w, http.StatusOK, thetvdbSeriesJSON)
		return

	// --- GET /series/{id}/seasons/{n} ----------------------------------------
	//
	// seasonRecord(): "/series/"+id+"/seasons/"+n, reading data.overview and
	// data.image only.
	case path == "/series/"+thetvdbSeriesID+"/seasons/1":
		writeRawJSON(w, http.StatusOK, thetvdbSeason1JSON)
		return

	// --- GET /series/{id}/episodes/default -----------------------------------
	//
	// episodeRecord(): "/series/"+id+"/episodes/default", walking
	// data.episodes[] for a seasonNumber/number pair. Neither implementation
	// sends a page parameter nor reads "links", but the real API paginates, so
	// the body carries a links block with "next": null and any page past the
	// first is served empty — a build that ever learns to page cannot loop.
	case path == "/series/"+thetvdbSeriesID+"/episodes/default":
		if p := strings.TrimSpace(r.URL.Query().Get("page")); p != "" && p != "0" {
			writeRawJSON(w, http.StatusOK, thetvdbEpisodesEmptyPageJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, thetvdbEpisodesJSON)
		return
	}

	// --- a known shape about an unknown id -----------------------------------
	//
	// /series/{other}, /series/{other}/seasons/{n}, /series/{id}/seasons/{other}
	// and /series/{other}/episodes/default are all well-formed requests about
	// something this stand-in has no record of. TheTVDB answers those 404, and
	// both implementations read a 404 as their no-record outcome (errNoRecord /
	// ErrNoMatch) rather than as a failure — so they must be served, not logged
	// as unrecognized.
	if thetvdbIsSeriesShape(path) {
		writeRawJSON(w, http.StatusNotFound, thetvdbNotFoundJSON)
		return
	}

	unmatched(w, r, "thetvdb")
}

// thetvdbAuthorized reports whether the request carries the exact bearer token
// POST /login handed out.
func thetvdbAuthorized(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == thetvdbToken
}

// thetvdbLogin answers POST /login. The request body is {"apikey": "..."} (the
// real API also accepts an optional "pin" for a subscriber key; neither
// implementation sends one, so it is read and ignored). A blank or absent apikey
// is rejected the way the real API rejects it — 401 in TheTVDB's failure shape —
// which is the case internal/enrich's statusError and the plugin's login error
// path both turn into an operator-visible error.
func thetvdbLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"apikey"`
		PIN    string `json:"pin"`
	}
	// A body that will not decode is the same thing as one with no apikey in it.
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.APIKey) == "" {
		writeRawJSON(w, http.StatusUnauthorized, thetvdbUnauthorizedJSON)
		return
	}
	writeRawJSON(w, http.StatusOK, thetvdbLoginJSON)
}

// thetvdbSearch answers GET /search. Only the fixture title resolves; any other
// query is a well-formed search with nothing in it, which both implementations
// read as a no-match (len(out.Data) == 0).
func thetvdbSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	if q == "" {
		q = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	}
	if q == "breaking bad" {
		writeRawJSON(w, http.StatusOK, thetvdbSearchHitJSON)
		return
	}
	writeRawJSON(w, http.StatusOK, thetvdbSearchEmptyJSON)
}

// thetvdbIsSeriesShape reports whether a path is one of the series-rooted shapes
// this stand-in recognizes but has no record for — the ones that earn a 404
// rather than an unmatched log.
func thetvdbIsSeriesShape(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 0 || parts[0] != "series" || len(parts) < 2 || parts[1] == "" {
		return false
	}
	switch len(parts) {
	case 2: // /series/{id}
		return true
	case 4: // /series/{id}/seasons/{n} or /series/{id}/episodes/{order}
		return parts[2] == "seasons" || parts[2] == "episodes"
	default:
		return false
	}
}

// --- the canned bodies --------------------------------------------------------

// thetvdbLoginJSON is the shape both implementations decode: data.token.
const thetvdbLoginJSON = `{
  "status": "success",
  "data": {
    "token": "standin-token"
  }
}`

// thetvdbUnauthorizedJSON is TheTVDB's 401 failure shape.
const thetvdbUnauthorizedJSON = `{
  "status": "failure",
  "message": "Unauthorized",
  "data": null
}`

// thetvdbNotFoundJSON is TheTVDB's 404 failure shape — its "no record" answer.
const thetvdbNotFoundJSON = `{
  "status": "failure",
  "message": "Not Found",
  "data": null
}`

// thetvdbSearchHitJSON is one series hit for the fixture. Both implementations
// read only data[0].tvdb_id; the rest is here because the real API sends it and a
// stand-in that trims a response to what today's parser happens to read hides a
// parser that starts reading more.
const thetvdbSearchHitJSON = `{
  "status": "success",
  "data": [
    {
      "objectID": "series-81189",
      "id": "series-81189",
      "tvdb_id": "81189",
      "name": "Breaking Bad",
      "slug": "breaking-bad",
      "overview": "A high school chemistry teacher diagnosed with inoperable lung cancer turns to manufacturing and selling methamphetamine to secure his family's future.",
      "first_air_time": "2008-01-20",
      "year": "2008",
      "image_url": "%%BASE%%/img/breaking-bad-poster.jpg",
      "thumbnail": "%%BASE%%/img/breaking-bad-poster.jpg",
      "country": "usa",
      "primary_language": "eng",
      "primary_type": "series",
      "status": "Ended",
      "type": "series",
      "network": "AMC",
      "genres": ["Drama", "Crime", "Thriller"],
      "remote_ids": [
        {"id": "tt0903747", "type": 2, "sourceName": "IMDB"}
      ]
    }
  ],
  "links": {
    "prev": null,
    "self": "%%BASE%%/search?query=Breaking+Bad&type=series",
    "next": null,
    "total_items": 1,
    "page_size": 25
  }
}`

// thetvdbSearchEmptyJSON is a search that matched nothing.
const thetvdbSearchEmptyJSON = `{
  "status": "success",
  "data": [],
  "links": {
    "prev": null,
    "self": "%%BASE%%/search",
    "next": null,
    "total_items": 0,
    "page_size": 25
  }
}`

// thetvdbSeriesJSON is the series record. showRecord() consumes data.name,
// data.overview, data.image and data.genres[].name.
const thetvdbSeriesJSON = `{
  "status": "success",
  "data": {
    "id": 81189,
    "name": "Breaking Bad",
    "slug": "breaking-bad",
    "image": "%%BASE%%/img/breaking-bad-poster.jpg",
    "overview": "A high school chemistry teacher diagnosed with inoperable lung cancer turns to manufacturing and selling methamphetamine to secure his family's future.",
    "firstAired": "2008-01-20",
    "lastAired": "2013-09-29",
    "nextAired": "",
    "year": "2008",
    "score": 9876,
    "averageRuntime": 47,
    "originalCountry": "usa",
    "originalLanguage": "eng",
    "defaultSeasonType": 1,
    "isOrderRandomized": false,
    "lastUpdated": "2020-01-01 00:00:00",
    "status": {
      "id": 2,
      "name": "Ended",
      "recordType": "series",
      "keepUpdated": false
    },
    "genres": [
      {"id": 21, "name": "Drama", "slug": "drama"},
      {"id": 31, "name": "Crime", "slug": "crime"},
      {"id": 43, "name": "Thriller", "slug": "thriller"}
    ],
    "remoteIds": [
      {"id": "tt0903747", "type": 2, "sourceName": "IMDB"}
    ],
    "originalNetwork": {
      "id": 174,
      "name": "AMC",
      "slug": "amc",
      "country": "usa"
    }
  }
}`

// thetvdbSeason1JSON is the season record. seasonRecord() consumes only
// data.overview and data.image.
const thetvdbSeason1JSON = `{
  "status": "success",
  "data": {
    "id": 27272,
    "seriesId": 81189,
    "number": 1,
    "name": "Season 1",
    "overview": "Walter White's first cook, his first partner, and the first of the lines he crosses.",
    "image": "%%BASE%%/img/breaking-bad-s01-poster.jpg",
    "imageType": 7,
    "companies": {
      "studio": [],
      "network": [],
      "production": [],
      "distributor": [],
      "special_effects": []
    },
    "type": {
      "id": 1,
      "name": "Aired Order",
      "type": "official",
      "alternateName": null
    },
    "year": "2008",
    "lastUpdated": "2020-01-01 00:00:00"
  }
}`

// thetvdbEpisodesJSON is the default-order episode list. episodeRecord() walks
// data.episodes[] for a matching seasonNumber/number pair and reads name,
// overview and image off the hit. The links block ends the list ("next": null)
// so a paging caller stops here.
const thetvdbEpisodesJSON = `{
  "status": "success",
  "data": {
    "series": {
      "id": 81189,
      "name": "Breaking Bad",
      "slug": "breaking-bad",
      "image": "%%BASE%%/img/breaking-bad-poster.jpg",
      "firstAired": "2008-01-20",
      "lastAired": "2013-09-29",
      "year": "2008"
    },
    "episodes": [
      {
        "id": 349232,
        "seriesId": 81189,
        "seasonNumber": 1,
        "number": 1,
        "name": "Pilot",
        "overview": "Diagnosed with terminal lung cancer, chemistry teacher Walter White teams with a former student to cook and sell crystal meth.",
        "aired": "2008-01-20",
        "runtime": 58,
        "image": "%%BASE%%/img/breaking-bad-s01e01.jpg",
        "imageType": 12,
        "isMovie": 0,
        "finaleType": null,
        "lastUpdated": "2020-01-01 00:00:00"
      },
      {
        "id": 349233,
        "seriesId": 81189,
        "seasonNumber": 1,
        "number": 2,
        "name": "Cat's in the Bag...",
        "overview": "Walt and Jesse are left with a mess to clean up after their first cook goes wrong.",
        "aired": "2008-01-27",
        "runtime": 48,
        "image": "%%BASE%%/img/breaking-bad-s01e02.jpg",
        "imageType": 12,
        "isMovie": 0,
        "finaleType": null,
        "lastUpdated": "2020-01-01 00:00:00"
      }
    ]
  },
  "links": {
    "prev": null,
    "self": "%%BASE%%/series/81189/episodes/default?page=0",
    "next": null,
    "total_items": 2,
    "page_size": 500
  }
}`

// thetvdbEpisodesEmptyPageJSON answers any page past the first: the same shape
// with nothing in it, so a caller that pages cannot loop.
const thetvdbEpisodesEmptyPageJSON = `{
  "status": "success",
  "data": {
    "series": {
      "id": 81189,
      "name": "Breaking Bad",
      "slug": "breaking-bad",
      "image": "%%BASE%%/img/breaking-bad-poster.jpg",
      "firstAired": "2008-01-20",
      "lastAired": "2013-09-29",
      "year": "2008"
    },
    "episodes": []
  },
  "links": {
    "prev": null,
    "self": "%%BASE%%/series/81189/episodes/default",
    "next": null,
    "total_items": 2,
    "page_size": 500
  }
}`
