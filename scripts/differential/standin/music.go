package main

// Stand-in for the four MUSIC sources — MusicBrainz, the Cover Art Archive,
// fanart.tv and TheAudioDB — serving the one fixture identity both builds of the
// differential run are pointed at:
//
//	artist          Radiohead                      a74b1b7f-71a5-4011-9441-d0b5e4122711
//	album (rg)      OK Computer, 1997-05-21, Album b1392450-e666-3926-a536-22c65f834433
//	release (CD)    OK Computer, 1997-05-21, GB    0b6b4ba0-d36f-47bd-b4ea-6a5b91842d29
//	  1 Airbag                       284000 ms     d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0001
//	  2 Paranoid Android             383000 ms     d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0002
//	  3 Subterranean Homesick Alien  267000 ms     d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0003
//
// fanart.tv is additionally asked about the two VIDEO fixtures, because the one
// fanart.tv instance serves both chains: the movie "Back to the Future" (TMDB
// 105, IMDb tt0088763) and the show "Breaking Bad" (TheTVDB 81189).
//
// It exists to force BOTH implementations down the same wire path:
//
//   - the plugin providers, plugins/musicbrainz/musicbrainz/musicbrainz.go,
//     plugins/fanarttv/fanarttv/fanarttv.go and
//     plugins/theaudiodb/theaudiodb/theaudiodb.go, and
//   - the old Go providers, internal/enrich/musicbrainz.go (which carried the
//     Cover Art Archive as its CoverArtURL), internal/enrich/fanarttv.go and
//     internal/enrich/theaudiodb.go.
//
// Each pair issues byte-for-byte identical requests — same endpoints, same query
// parameters, same `inc` lists, same key-in-the-path for TheAudioDB — so one
// stand-in satisfies both and any difference in the records they produce is a
// difference in THEM, not in what they were served.
//
// Every body below is a raw string literal and nothing in here consults the
// clock, a random source or a map iteration order, so the same request always
// produces the same bytes. Artwork URLs are absolute in all four real APIs, so
// they are emitted as %%BASE%%/img/<name>.jpg and writeRawJSON rewrites the token
// to the stand-in's own origin, pointing them back at its image mount.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// --- the fixture identity ----------------------------------------------------

const (
	mbArtistID       = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	mbReleaseGroupID = "b1392450-e666-3926-a536-22c65f834433"
	mbReleaseID      = "0b6b4ba0-d36f-47bd-b4ea-6a5b91842d29"

	mbRecAirbagID       = "d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0001"
	mbRecParanoidID     = "d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0002"
	mbRecSubterraneanID = "d8c4b2a6-1f51-4b3e-9e0c-8b0d8c2f0003"
)

// mbNotFoundJSON is MusicBrainz's own 404 document, which both implementations
// read as "no such record" off the STATUS alone (getJSON → errNoMatch /
// ErrNoMatch). The body is here so a human reading a capture sees what the real
// service would have said.
const mbNotFoundJSON = `{"error":"Not Found","help":"For usage, please see: https://musicbrainz.org/development/mmd"}`

// mbCreditJSON is the fixture's artist-credit, the shape mbCredit decodes: a
// name, an empty join phrase (it is the last and only entry) and the credited
// artist ENTITY, whose id is what ADR-0053's corroboration reads off an album.
const mbCreditJSON = `[{"name":"Radiohead","joinphrase":"","artist":{"id":"` + mbArtistID + `","name":"Radiohead","sort-name":"Radiohead","disambiguation":""}}]`

// mbArtistTagsJSON and mbAlbumTagsJSON are the tag lists topTags() turns into
// the record's Genres — capped at three there, so three is what is served.
const (
	mbArtistTagsJSON = `[{"count":12,"name":"rock"},{"count":9,"name":"alternative rock"},{"count":6,"name":"art rock"}]`
	mbAlbumTagsJSON  = `[{"count":8,"name":"alternative rock"},{"count":5,"name":"art rock"},{"count":3,"name":"experimental rock"}]`
)

// mbAreaJSON is the artist's area, which artistOverview() renders as
// "from United Kingdom".
const mbAreaJSON = `{"id":"8a754a16-0027-3a29-b6d7-2b40ea0481ed","type":"Country","name":"United Kingdom","sort-name":"United Kingdom"}`

// --- MusicBrainz -------------------------------------------------------------

// musicbrainzHandler serves the stand-in MusicBrainz ws/2 API. r.URL.Path has
// already had the mount prefix stripped by main.go, so it begins at "/artist",
// "/release-group", "/release/<mbid>", "/recording", etc.
//
// Both implementations reach exactly eleven request shapes, and every one of
// them is answered below. `fmt=json` is deliberately NOT enforced: the counters
// in main.go already record the full query string, so a build that dropped it
// would show up as a diff rather than as a hole in the stand-in.
func musicbrainzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		unmatched(w, r, "musicbrainz") // ws/2 is read-only to both providers
		return
	}
	path := "/" + strings.Trim(r.URL.Path, "/")
	q := r.URL.Query()

	switch {
	// --- GET /artist?query=… -------------------------------------------------
	//
	// searchArtists() (musicbrainz.go:524, the picker's Artist kind) sends
	// musicQuery(terms,"","") + limit/offset; artistByName() (musicbrainz.go:1244,
	// ADR-0053's last tier) sends the exact phrase artist:"<name>" and reads
	// Tags off the SEARCH hit — which is why the hit below carries tags.
	case path == "/artist" && q.Has("query"):
		if mbPagedPast(q) || !mbMatchesFixture(q.Get("query")) {
			writeRawJSON(w, http.StatusOK, `{"count":0,"offset":0,"artists":[]}`)
			return
		}
		writeRawJSON(w, http.StatusOK, `{"count":1,"offset":0,"artists":[`+mbArtistHitJSON+`]}`)

	// --- GET /artist/<mbid>?inc=tags -----------------------------------------
	//
	// artistByID() (musicbrainz.go:1023): the durable artist-override path, and
	// the path artistDetails() funnels into once corroboration has an id. Reads
	// name, type, disambiguation, area.name and tags.
	case strings.HasPrefix(path, "/artist/"):
		if id, ok := mbSegment(path, "/artist/"); !ok || id != mbArtistID {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, mbArtistLookupJSON(q.Get("inc")))

	// --- GET /release-group?query=… ------------------------------------------
	//
	// Three callers, one shape: searchReleaseGroups() (musicbrainz.go:573, the
	// picker's Album kind, which also fetches a tracklist preview per candidate),
	// artistIDFromAlbumSearch() (musicbrainz.go:1204, the UNNARROWED corroborating
	// search) and albumDetails() (musicbrainz.go:1297, the exact-phrase
	// releasegroup:"…" lookup). All three read the hit below; two of them read its
	// tags, one reads its artist-credit.
	case path == "/release-group" && q.Has("query"):
		if mbPagedPast(q) || !mbMatchesFixture(q.Get("query")) {
			writeRawJSON(w, http.StatusOK, `{"count":0,"offset":0,"release-groups":[]}`)
			return
		}
		writeRawJSON(w, http.StatusOK, `{"count":1,"offset":0,"release-groups":[`+mbReleaseGroupHitJSON+`]}`)

	// --- GET /release-group/<mbid>?inc=tags | ?inc=artist-credits ------------
	//
	// releaseGroupByID() (musicbrainz.go:1051) sends inc=tags — the durable album
	// override, and the tail of releaseGroupForRelease(). artistIDFromReleaseGroup()
	// (musicbrainz.go:1171) sends inc=artist-credits — ADR-0053's one lookup, no
	// search. The `inc` gating below is what makes those two distinguishable in a
	// capture.
	case strings.HasPrefix(path, "/release-group/"):
		if id, ok := mbSegment(path, "/release-group/"); !ok || id != mbReleaseGroupID {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, mbReleaseGroupLookupJSON(q.Get("inc")))

	// --- GET /release?release-group=<mbid>&inc=recordings&limit=1|100 --------
	//
	// releaseGroupTracklist() (musicbrainz.go:638) browses with limit=1 for the
	// candidate PREVIEW; browseReleaseGroupReleases() (musicbrainz.go:749) browses
	// with limit=100 for fit-selection and for ReleaseGroupEditions (ADR-0052).
	// Both read the same mbRelease, so both get the same one edition.
	case path == "/release" && q.Has("release-group"):
		if q.Get("release-group") != mbReleaseGroupID {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK,
			`{"release-count":1,"release-offset":0,"releases":[`+mbReleaseJSON(`recordings`)+`]}`)

	// --- GET /release?query=… ------------------------------------------------
	//
	// Neither implementation searches the release index today (an album IS a
	// release-group, ADR-0038). It is served so that a build which started to
	// would be caught by the DIFF rather than by an unmatched request.
	case path == "/release" && q.Has("query"):
		if mbPagedPast(q) || !mbMatchesFixture(q.Get("query")) {
			writeRawJSON(w, http.StatusOK, `{"count":0,"offset":0,"releases":[]}`)
			return
		}
		writeRawJSON(w, http.StatusOK,
			`{"count":1,"offset":0,"releases":[`+mbReleaseJSON(`recordings release-groups`)+`]}`)

	// --- GET /release/<mbid>?inc=recordings release-groups | ?inc=release-groups
	//
	// taggedReleaseTracklist() (musicbrainz.go:720) asks for both — the tracklist
	// and the parent check in the call the tracklist needed anyway.
	// releaseGroupForRelease() (musicbrainz.go:1002) asks only for the parent, to
	// turn a pasted /release/ URL into the album it is an edition of.
	case strings.HasPrefix(path, "/release/"):
		if id, ok := mbSegment(path, "/release/"); !ok || id != mbReleaseID {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, mbReleaseJSON(q.Get("inc")))

	// --- GET /recording?query=… ----------------------------------------------
	//
	// searchRecordings() (musicbrainz.go:449, the picker's Track kind) and
	// trackDetails() (musicbrainz.go:1369, ADR-0050's last tier, whose top hit the
	// HOST then accepts or rejects by title). Both take Recordings[0], so a query
	// naming one of the three fixture tracks puts THAT recording first — otherwise
	// a track lookup would be rejected by the host for a reason that says nothing
	// about either build.
	case path == "/recording" && q.Has("query"):
		hits := mbRecordingHits(q.Get("query"))
		if mbPagedPast(q) || len(hits) == 0 {
			writeRawJSON(w, http.StatusOK, `{"count":0,"offset":0,"recordings":[]}`)
			return
		}
		writeRawJSON(w, http.StatusOK,
			`{"count":`+strconv.Itoa(len(hits))+`,"offset":0,"recordings":[`+strings.Join(hits, ",")+`]}`)

	// --- GET /recording/<mbid>?fmt=json --------------------------------------
	//
	// recordingByID() (musicbrainz.go:1083): the durable track override. It sends
	// NO inc at all and reads only id + title.
	case strings.HasPrefix(path, "/recording/"):
		id, ok := mbSegment(path, "/recording/")
		if !ok {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		rec := mbRecordingByID(id)
		if rec == nil {
			writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, rec.lookup)

	default:
		unmatched(w, r, "musicbrainz")
	}
}

// mbMatchesFixture is the stand-in's query-matching rule, and it is deliberately
// LOOSE. Both implementations build Lucene-ish queries whose exact spelling is
// the thing under test — `releasegroup:"OK Computer" AND artist:"Radiohead"`,
// `OK Computer AND artist:("The Radiohead" OR "Radiohead")`, the bare escaped
// album title, the exact phrase `artist:"Radiohead"` — and a stand-in that
// parsed them would be re-implementing the query builder it is meant to observe.
// So: the already-URL-decoded query matches the fixture when it MENTIONS the
// artist or the album, case-insensitively, and nothing else is inspected. The
// exact query string is still compared between the two builds, by the counters
// in main.go.
func mbMatchesFixture(query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(q, "radiohead") || strings.Contains(q, "ok computer")
}

// mbPagedPast reports whether the request asked for a page after the first, in
// which case the one-hit fixture index is exhausted. setPaging()
// (musicbrainz.go:422) is what puts offset on the wire, for the picker's "show
// more"; answering an empty page is how that loop terminates.
func mbPagedPast(q url.Values) bool {
	n, err := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	return err == nil && n > 0
}

// mbSegment returns the one path segment after prefix, and reports whether the
// path is exactly prefix+segment. A deeper path is not a shape either
// implementation produces, so it is left to the caller's 404.
func mbSegment(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// mbIncHas reports whether an `inc` value names part. Both implementations send
// the canonical SPACE-separated list ("recordings release-groups"), which
// url.Values decodes back from the '+' on the wire; a literal '+' separator is
// accepted too so a hand-made capture replays.
func mbIncHas(inc, part string) bool {
	for _, f := range strings.Fields(strings.ReplaceAll(inc, "+", " ")) {
		if f == part {
			return true
		}
	}
	return false
}

// mbArtistHitJSON is one /artist search hit. `score` is MusicBrainz's own
// relevance figure; NEITHER implementation decodes it (both take Artists[0]
// unjudged), and it is carried only so the document is the shape the real
// service returns.
const mbArtistHitJSON = `{"id":"` + mbArtistID + `","type":"Group","score":100,"name":"Radiohead","sort-name":"Radiohead","country":"GB","disambiguation":"","area":` + mbAreaJSON + `,"tags":` + mbArtistTagsJSON + `}`

// mbArtistLookupJSON is GET /artist/<mbid>. The tags are gated on inc=tags, which
// is what artistByID() sends, so a capture shows whether the inc survived.
func mbArtistLookupJSON(inc string) string {
	b := `{"id":"` + mbArtistID + `","name":"Radiohead","sort-name":"Radiohead","type":"Group","country":"GB","disambiguation":"","area":` + mbAreaJSON
	if mbIncHas(inc, "tags") {
		b += `,"tags":` + mbArtistTagsJSON
	}
	return b + `}`
}

// mbReleaseGroupHitJSON is one /release-group search hit. It carries the
// artist-credit (the release-group index includes credits unasked, which is what
// artistIDFromAlbumSearch relies on) AND the tags (albumDetails reads Genres off
// the search hit rather than off a lookup).
const mbReleaseGroupHitJSON = `{"id":"` + mbReleaseGroupID + `","score":100,"count":1,"title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21","disambiguation":"","artist-credit":` + mbCreditJSON + `,"tags":` + mbAlbumTagsJSON + `}`

// mbReleaseGroupLookupJSON is GET /release-group/<mbid>, with the two `inc`
// lists the two callers send gating the two optional members.
func mbReleaseGroupLookupJSON(inc string) string {
	b := `{"id":"` + mbReleaseGroupID + `","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21","disambiguation":""`
	if mbIncHas(inc, "artist-credits") {
		b += `,"artist-credit":` + mbCreditJSON
	}
	if mbIncHas(inc, "tags") {
		b += `,"tags":` + mbAlbumTagsJSON
	}
	return b + `}`
}

// mbMediaJSON is the release's one CD, with the three tracks in position order.
// mbTracklist() (musicbrainz.go:979) reads medium.position, track.position,
// track.title and track.recording.id; releaseFormat() (musicbrainz.go:904) reads
// medium.format, and releaseTrackCount() counts len(tracks) — the number
// fit-selection compares against the local album's.
const mbMediaJSON = `[{"position":1,"format":"CD","track-count":3,"tracks":[` +
	`{"id":"e2a3c0d1-0000-4000-8000-000000000001","position":1,"number":"1","title":"Airbag","length":284000,"recording":{"id":"` + mbRecAirbagID + `","title":"Airbag","length":284000,"video":false,"disambiguation":""}},` +
	`{"id":"e2a3c0d1-0000-4000-8000-000000000002","position":2,"number":"2","title":"Paranoid Android","length":383000,"recording":{"id":"` + mbRecParanoidID + `","title":"Paranoid Android","length":383000,"video":false,"disambiguation":""}},` +
	`{"id":"e2a3c0d1-0000-4000-8000-000000000003","position":3,"number":"3","title":"Subterranean Homesick Alien","length":267000,"recording":{"id":"` + mbRecSubterraneanID + `","title":"Subterranean Homesick Alien","length":267000,"video":false,"disambiguation":""}}` +
	`]}]`

// mbReleaseJSON is the one edition, gated by `inc` exactly as the real service
// gates it: `media` only with inc=recordings, `release-group` only with
// inc=release-groups. The gating is load-bearing for taggedReleaseTracklist(),
// whose parent check compares release-group.id against the album's rgID — served
// unasked, the check would pass on a request that never asked for it.
func mbReleaseJSON(inc string) string {
	b := `{"id":"` + mbReleaseID + `","title":"OK Computer","status":"Official","date":"1997-05-21","country":"GB","barcode":"724385522925","disambiguation":"","artist-credit":` + mbCreditJSON
	if mbIncHas(inc, "recordings") {
		b += `,"media":` + mbMediaJSON
	}
	if mbIncHas(inc, "release-groups") {
		b += `,"release-group":{"id":"` + mbReleaseGroupID + `","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21"}`
	}
	return b + `}`
}

// mbRecordingFixture is one fixture recording: the MBID both implementations
// pin, the title they search by, the object it contributes to a /recording
// search, and the body GET /recording/<mbid> answers with.
type mbRecordingFixture struct {
	id     string
	title  string
	hit    string
	lookup string
}

// mbRecordings is the three tracks, IN TRACK ORDER — the order a search with no
// title in it returns them in, so Recordings[0] is always the same recording.
var mbRecordings = []mbRecordingFixture{
	{
		id:     mbRecAirbagID,
		title:  "Airbag",
		hit:    mbRecordingHitJSON(mbRecAirbagID, "Airbag", 284000),
		lookup: mbRecordingLookupJSON(mbRecAirbagID, "Airbag", 284000),
	},
	{
		id:     mbRecParanoidID,
		title:  "Paranoid Android",
		hit:    mbRecordingHitJSON(mbRecParanoidID, "Paranoid Android", 383000),
		lookup: mbRecordingLookupJSON(mbRecParanoidID, "Paranoid Android", 383000),
	},
	{
		id:     mbRecSubterraneanID,
		title:  "Subterranean Homesick Alien",
		hit:    mbRecordingHitJSON(mbRecSubterraneanID, "Subterranean Homesick Alien", 267000),
		lookup: mbRecordingLookupJSON(mbRecSubterraneanID, "Subterranean Homesick Alien", 267000),
	},
}

// mbRecordingHitJSON builds one /recording search hit. searchRecordings() reads
// the artist-credit, the disambiguation, first-release-date and each release's
// date + release-group title/first-release-date (its "on <album>" hint and its
// earliest-year rule); trackDetails() reads only id + title.
func mbRecordingHitJSON(id, title string, length int) string {
	return `{"id":"` + id + `","score":100,"title":"` + title + `","length":` + strconv.Itoa(length) +
		`,"video":null,"disambiguation":"","artist-credit":` + mbCreditJSON +
		`,"first-release-date":"1997-05-21","releases":[{"id":"` + mbReleaseID +
		`","title":"OK Computer","status":"Official","date":"1997-05-21","country":"GB","release-group":{"id":"` +
		mbReleaseGroupID + `","title":"OK Computer","primary-type":"Album","first-release-date":"1997-05-21"}}]}`
}

// mbRecordingLookupJSON builds GET /recording/<mbid>, which is sent with fmt=json
// and no inc at all.
func mbRecordingLookupJSON(id, title string, length int) string {
	return `{"id":"` + id + `","title":"` + title + `","length":` + strconv.Itoa(length) +
		`,"video":false,"disambiguation":"","first-release-date":"1997-05-21"}`
}

// mbRecordingHits ranks the fixture recordings for a /recording search.
//
// A query that NAMES one of the three tracks returns that track alone, because
// both implementations take Recordings[0] and the host then accepts it only if
// its title matches the local track's (ADR-0057). Returning the tracklist in
// track order for a query about "Paranoid Android" would have every track but
// the first rejected — a no-match that says nothing about either build. A query
// that mentions only the artist or the album returns all three in track order;
// anything else returns nothing.
func mbRecordingHits(query string) []string {
	q := strings.ToLower(query)
	var named []string
	for _, rec := range mbRecordings {
		if strings.Contains(q, strings.ToLower(rec.title)) {
			named = append(named, rec.hit)
		}
	}
	if len(named) > 0 {
		return named
	}
	if !mbMatchesFixture(query) {
		return nil
	}
	all := make([]string, 0, len(mbRecordings))
	for _, rec := range mbRecordings {
		all = append(all, rec.hit)
	}
	return all
}

// mbRecordingByID finds a fixture recording by MBID; nil for an unknown one,
// which is the 404 both implementations read as errNoMatch/ErrNoMatch.
func mbRecordingByID(id string) *mbRecordingFixture {
	for i := range mbRecordings {
		if mbRecordings[i].id == id {
			return &mbRecordings[i]
		}
	}
	return nil
}

// --- the Cover Art Archive ---------------------------------------------------

// coverartHandler serves the stand-in Cover Art Archive. Path begins at
// "/release-group/<mbid>" or "/release-group/<mbid>/front-500".
//
// The CAA is the MusicBrainz provider's SECOND host: Settings.URL2 in the plugin
// (musicbrainz.go:1427 ArtworkCandidates, and the two cover URLs built at
// :614 and :1075/:1334), and CoverArtURL in the Go provider
// (internal/enrich/musicbrainz.go:1436). It is mounted on the same listener as
// everything else because the plugin host only permits a guest to fetch a host
// the operator configured — see the package comment in main.go.
func coverartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		unmatched(w, r, "coverart")
		return
	}
	path := "/" + strings.Trim(r.URL.Path, "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "release-group" {
		unmatched(w, r, "coverart")
		return
	}
	if parts[1] != mbReleaseGroupID {
		// The CAA's answer for a release-group it holds no art for. Both
		// implementations read it as "no cover art", not as a failure.
		writeRawJSON(w, http.StatusNotFound, mbNotFoundJSON)
		return
	}

	switch len(parts) {
	// --- GET /release-group/<mbid> -------------------------------------------
	//
	// The JSON index. ArtworkCandidates() reads images[].image, .front and
	// .thumbnails, preferring thumbnails["500"] over the full-resolution original.
	case 2:
		writeRawJSON(w, http.StatusOK, caaIndexJSON)

	// --- GET /release-group/<mbid>/front-250 | /front-500 --------------------
	//
	// These are IMAGE urls, not JSON: /front-250 is the candidate thumbnail
	// searchReleaseGroups() hands the picker, /front-500 is the cover
	// releaseGroupByID()/albumDetails() hand the host's ArtworkFetcher, which
	// really downloads it.
	case 3:
		if !caaFrontSegment(parts[2]) {
			unmatched(w, r, "coverart")
			return
		}
		writeStandinImage(w)

	default:
		unmatched(w, r, "coverart")
	}
}

// caaFrontSegment reports whether a segment is one of the CAA's "front"
// derivatives. Only front-250 and front-500 are on either implementation's wire
// path; the other two are accepted so that a size change shows up as a DIFF
// rather than as a hole in the stand-in.
func caaFrontSegment(seg string) bool {
	switch seg {
	case "front", "front-250", "front-500", "front-1200":
		return true
	}
	return false
}

// caaIndexJSON is the Cover Art Archive's release-group index: the "front" image
// plus a back cover that must NOT be chosen for a cover role.
const caaIndexJSON = `{"images":[` +
	`{"id":"10000000001","image":"%%BASE%%/img/okc_front.jpg","front":true,"back":false,"approved":true,"edit":1000001,"comment":"","types":["Front"],` +
	`"thumbnails":{"250":"%%BASE%%/img/okc_front_250.jpg","500":"%%BASE%%/img/okc_front_500.jpg","1200":"%%BASE%%/img/okc_front_1200.jpg","small":"%%BASE%%/img/okc_front_250.jpg","large":"%%BASE%%/img/okc_front_500.jpg"}},` +
	`{"id":"10000000002","image":"%%BASE%%/img/okc_back.jpg","front":false,"back":true,"approved":true,"edit":1000002,"comment":"","types":["Back"],` +
	`"thumbnails":{"250":"%%BASE%%/img/okc_back_250.jpg","500":"%%BASE%%/img/okc_back_500.jpg","1200":"%%BASE%%/img/okc_back_1200.jpg","small":"%%BASE%%/img/okc_back_250.jpg","large":"%%BASE%%/img/okc_back_500.jpg"}}` +
	`],"release":"%%BASE%%/musicbrainz/release/` + mbReleaseID + `"}`

// --- fanart.tv ---------------------------------------------------------------

// fanarttvHandler serves the stand-in fanart.tv v3 API. Path begins at
// "/music/<mbid>", "/movies/<id>", "/tv/<id>".
//
// One handler for both chains, because one fanart.tv INSTANCE serves both:
// fetchArtistImages() (fanarttv.go:361) hits /music/<mbid>, and videoArt()
// (fanarttv.go:403) hits the endpoint videoLookup() built at :187/:194 — a movie
// by TMDB id, falling back to IMDb, a show by TheTVDB id. The Go provider is
// identical (internal/enrich/fanarttv.go:199, :348, :355).
func fanarttvHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		unmatched(w, r, "fanarttv")
		return
	}
	path := "/" + strings.Trim(r.URL.Path, "/")

	// --- the key is on the QUERY string --------------------------------------
	//
	// getJSON() (fanarttv.go:437) sets api_key=<Settings.Secret> on every request.
	// A blank key is fanart.tv's 401, which pluginsdk.Unavailable does NOT
	// classify as an outage — so answering it here is what proves the key reached
	// the guest at all.
	if strings.TrimSpace(r.URL.Query().Get("api_key")) == "" {
		writeRawJSON(w, http.StatusUnauthorized, fanUnauthorizedJSON)
		return
	}

	switch {
	// --- GET /music/<mbid>?api_key=… -----------------------------------------
	//
	// The artist artwork. fetchArtistImages() decodes artistthumb,
	// artistbackground, hdmusiclogo and musiclogo; every other member of the real
	// document (name, mbid_id, albums) is ignored by both implementations and is
	// here so the payload is the shape fanart.tv actually returns.
	case strings.HasPrefix(path, "/music/"):
		if id, ok := mbSegment(path, "/music/"); !ok || id != mbArtistID {
			writeRawJSON(w, http.StatusNotFound, fanNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, fanMusicJSON)

	// --- GET /movies/105 | /movies/tt0088763 ---------------------------------
	//
	// The movie endpoint accepts either id, and videoLookup() prefers TMDB — so
	// both are served, with the same body, and the COUNTERS say which was asked
	// for.
	case strings.HasPrefix(path, "/movies/"):
		id, ok := mbSegment(path, "/movies/")
		if !ok || (id != "105" && id != "tt0088763") {
			writeRawJSON(w, http.StatusNotFound, fanNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, fanMovieJSON)

	// --- GET /tv/81189 -------------------------------------------------------
	//
	// The tv endpoint is TheTVDB-id keyed. videoArt() reads tvposter and
	// showbackground off it.
	case strings.HasPrefix(path, "/tv/"):
		if id, ok := mbSegment(path, "/tv/"); !ok || id != "81189" {
			writeRawJSON(w, http.StatusNotFound, fanNotFoundJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, fanTVJSON)

	default:
		unmatched(w, r, "fanarttv")
	}
}

// fanUnauthorizedJSON and fanNotFoundJSON are fanart.tv's own refusal documents.
// Both implementations read only the STATUS — 404 is the "no record" outcome,
// everything else is a failure — so the bodies exist for a human reading a
// capture.
const (
	fanUnauthorizedJSON = `{"status":"error","error message":"Invalid project key or no project key was given."}`
	fanNotFoundJSON     = `{"status":"error","error message":"Not found"}`
)

// fanMusicJSON is the artist artwork. The "likes" are DELIBERATELY out of order
// and encoded as strings, as fanart.tv encodes them, so bestImage()
// (fanarttv.go:455) and rankedImages() (:473) have a ranking to actually do: the
// best thumb is _2 (likes 31), the best background _1 (likes 44), and the logo is
// the HD one even though the SD logo has more likes.
const fanMusicJSON = `{"name":"Radiohead","mbid_id":"` + mbArtistID + `",` +
	`"artistthumb":[` +
	`{"id":"70131","url":"%%BASE%%/img/fanart_artistthumb_1.jpg","likes":"7"},` +
	`{"id":"70132","url":"%%BASE%%/img/fanart_artistthumb_2.jpg","likes":"31"}],` +
	`"artistbackground":[` +
	`{"id":"70141","url":"%%BASE%%/img/fanart_artistbackground_1.jpg","likes":"44"},` +
	`{"id":"70142","url":"%%BASE%%/img/fanart_artistbackground_2.jpg","likes":"12"}],` +
	`"musiclogo":[{"id":"70151","url":"%%BASE%%/img/fanart_musiclogo_1.png","likes":"58"}],` +
	`"hdmusiclogo":[{"id":"70161","url":"%%BASE%%/img/fanart_hdmusiclogo_1.png","likes":"9"}],` +
	`"albums":{"` + mbReleaseGroupID + `":{` +
	`"albumcover":[{"id":"70171","url":"%%BASE%%/img/fanart_albumcover_1.jpg","likes":"6"}],` +
	`"cdart":[{"id":"70181","url":"%%BASE%%/img/fanart_cdart_1.png","likes":"2","disc":"1","size":"1000"}]}}}`

// fanMovieJSON is the movie artwork. videoArt() reads movieposter and
// moviebackground; the other members are what the real document carries.
const fanMovieJSON = `{"name":"Back to the Future","tmdb_id":"105","imdb_id":"tt0088763",` +
	`"movieposter":[` +
	`{"id":"80101","url":"%%BASE%%/img/fanart_movieposter_1.jpg","lang":"en","likes":"5"},` +
	`{"id":"80102","url":"%%BASE%%/img/fanart_movieposter_2.jpg","lang":"en","likes":"23"}],` +
	`"moviebackground":[` +
	`{"id":"80111","url":"%%BASE%%/img/fanart_moviebackground_1.jpg","lang":"","likes":"17"},` +
	`{"id":"80112","url":"%%BASE%%/img/fanart_moviebackground_2.jpg","lang":"","likes":"4"}],` +
	`"hdmovielogo":[{"id":"80121","url":"%%BASE%%/img/fanart_hdmovielogo_1.png","lang":"en","likes":"11"}],` +
	`"moviethumb":[{"id":"80131","url":"%%BASE%%/img/fanart_moviethumb_1.jpg","lang":"en","likes":"3"}],` +
	`"moviebanner":[{"id":"80141","url":"%%BASE%%/img/fanart_moviebanner_1.jpg","lang":"en","likes":"2"}],` +
	`"moviedisc":[{"id":"80151","url":"%%BASE%%/img/fanart_moviedisc_1.png","lang":"en","likes":"1","disc":"1","disc_type":"bluray"}]}`

// fanTVJSON is the show artwork. videoArt() reads tvposter and showbackground;
// seasonposter and the logos are the rest of the real document.
const fanTVJSON = `{"name":"Breaking Bad","thetvdb_id":"81189",` +
	`"tvposter":[` +
	`{"id":"90101","url":"%%BASE%%/img/fanart_tvposter_1.jpg","lang":"en","likes":"8"},` +
	`{"id":"90102","url":"%%BASE%%/img/fanart_tvposter_2.jpg","lang":"en","likes":"29"}],` +
	`"showbackground":[` +
	`{"id":"90111","url":"%%BASE%%/img/fanart_showbackground_1.jpg","lang":"","likes":"21","season":"all"},` +
	`{"id":"90112","url":"%%BASE%%/img/fanart_showbackground_2.jpg","lang":"","likes":"6","season":"1"}],` +
	`"hdtvlogo":[{"id":"90121","url":"%%BASE%%/img/fanart_hdtvlogo_1.png","lang":"en","likes":"14"}],` +
	`"clearlogo":[{"id":"90131","url":"%%BASE%%/img/fanart_clearlogo_1.png","lang":"en","likes":"5"}],` +
	`"seasonposter":[{"id":"90141","url":"%%BASE%%/img/fanart_seasonposter_1.jpg","lang":"en","likes":"2","season":"1"}],` +
	`"tvthumb":[{"id":"90151","url":"%%BASE%%/img/fanart_tvthumb_1.jpg","lang":"en","likes":"4"}],` +
	`"tvbanner":[{"id":"90161","url":"%%BASE%%/img/fanart_tvbanner_1.jpg","lang":"en","likes":"1"}]}`

// --- TheAudioDB --------------------------------------------------------------

// theaudiodbHandler serves the stand-in TheAudioDB API. NOTE: TheAudioDB puts the
// API key IN THE PATH ("<base>/<key>/search.php?s=…"), so main.go passes the path
// with the mount prefix stripped but the key segment still present; it is
// stripped and ignored here.
//
// That is base() in the plugin (theaudiodb.go:285) and the three inline
// p.BaseURL+"/"+url.PathEscape(p.APIKey)+… in the Go provider
// (internal/enrich/theaudiodb.go:182, :185, :203, :207). Because the key is a
// path segment it never reaches redactedQuery, so it is also why the counters
// key for this provider carries the key: the two builds are configured with the
// same one, so it still compares equal.
func theaudiodbHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		unmatched(w, r, "theaudiodb")
		return
	}
	path := "/" + strings.Trim(r.URL.Path, "/")
	rest, ok := adbStripKey(path)
	if !ok {
		unmatched(w, r, "theaudiodb")
		return
	}
	q := r.URL.Query()

	switch rest {
	// --- GET /<key>/search.php?s=<name> --------------------------------------
	//
	// artistRequest() (theaudiodb.go:265) when the ref carries no MBID — the gap
	// this source covers for fanart.tv, which is strictly MBID-keyed.
	case "/search.php":
		if !adbIsRadiohead(q.Get("s")) {
			writeRawJSON(w, http.StatusOK, adbNoArtistJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, adbArtistJSON)

	// --- GET /<key>/artist-mb.php?i=<mbid> -----------------------------------
	//
	// artistRequest() when the ref carries the MBID the music lead resolved.
	case "/artist-mb.php":
		if strings.TrimSpace(q.Get("i")) != mbArtistID {
			writeRawJSON(w, http.StatusOK, adbNoArtistJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, adbArtistJSON)

	// --- GET /<key>/searchtrack.php?s=<artist>&t=<track> ---------------------
	//
	// lookupTrack() (theaudiodb.go:168) when the track carries no recording MBID.
	case "/searchtrack.php":
		if s := strings.TrimSpace(q.Get("s")); s != "" && !adbIsRadiohead(s) {
			writeRawJSON(w, http.StatusOK, adbNoTrackJSON)
			return
		}
		body, ok := adbTrackByTitle(q.Get("t"))
		if !ok {
			writeRawJSON(w, http.StatusOK, adbNoTrackJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, body)

	// --- GET /<key>/track-mb.php?i=<mbid> ------------------------------------
	//
	// lookupTrack() (theaudiodb.go:165) when it does. The synopsis this returns is
	// the ONLY source of a track Overview in the whole music chain — MusicBrainz
	// offers a canonical title and nothing else.
	case "/track-mb.php":
		body, ok := adbTrackByMBID(strings.TrimSpace(q.Get("i")))
		if !ok {
			writeRawJSON(w, http.StatusOK, adbNoTrackJSON)
			return
		}
		writeRawJSON(w, http.StatusOK, body)

	default:
		unmatched(w, r, "theaudiodb")
	}
}

// adbStripKey drops the API-key path segment, returning the endpoint path that
// follows it. ok is false for a path with no endpoint after the key, which is not
// a shape either implementation produces.
func adbStripKey(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	i := strings.Index(trimmed, "/")
	if i < 0 {
		return "", false
	}
	rest := trimmed[i:]
	if rest == "/" {
		return "", false
	}
	return rest, true
}

// adbIsRadiohead matches the artist-name key loosely, for the same reason
// mbMatchesFixture does: the name on the wire is whatever the local library
// tagged, and a stand-in that demanded an exact spelling would be asserting about
// the SCANNER rather than about either provider.
func adbIsRadiohead(name string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "radiohead")
}

// adbNoArtistJSON and adbNoTrackJSON are TheAudioDB's "no record" answers. It
// answers 200 with a null result set rather than 404, and BOTH implementations
// handle that explicitly (fetchArtist at theaudiodb.go:341, fetchTrack at :376,
// each reporting found=false for an empty set exactly as for a 404).
const (
	adbNoArtistJSON = `{"artists":null}`
	adbNoTrackJSON  = `{"track":null}`
)

// adbArtistJSON is the artist record. fetchArtist() reads strArtistThumb,
// strArtistFanart/2/3/4 (in that order — the first is the background the Lookup
// emits, the whole list feeds the picker grid), strArtistLogo and the
// language-suffixed biography, falling back to strBiographyEN. Only the EN
// biography is served, which is what every non-English configuration falls back
// to.
const adbArtistJSON = `{"artists":[{` +
	`"idArtist":"111239","strArtist":"Radiohead","strArtistAlternate":"",` +
	`"strMusicBrainzID":"` + mbArtistID + `",` +
	`"intFormedYear":"1985","strCountry":"England","strGenre":"Alternative Rock","strStyle":"Rock",` +
	`"strArtistThumb":"%%BASE%%/img/adb_artist_thumb.jpg",` +
	`"strArtistLogo":"%%BASE%%/img/adb_artist_logo.png",` +
	`"strArtistClearart":"%%BASE%%/img/adb_artist_clearart.png",` +
	`"strArtistBanner":"%%BASE%%/img/adb_artist_banner.jpg",` +
	`"strArtistFanart":"%%BASE%%/img/adb_artist_fanart1.jpg",` +
	`"strArtistFanart2":"%%BASE%%/img/adb_artist_fanart2.jpg",` +
	`"strArtistFanart3":"%%BASE%%/img/adb_artist_fanart3.jpg",` +
	`"strArtistFanart4":null,` +
	`"strBiographyEN":"Radiohead are an English rock band formed in Abingdon, Oxfordshire, in 1985. This biography is stand-in fixture text for the differential run.",` +
	`"strBiographyDE":null,"strBiographyFR":null` +
	`}]}`

// adbTrackJSON builds one track record. fetchTrack() reads only the
// language-suffixed description, falling back to strDescriptionEN; the rest is
// the shape the real document carries.
func adbTrackJSON(idTrack, mbid, title string, duration int) string {
	return `{"track":[{` +
		`"idTrack":"` + idTrack + `","idAlbum":"2110101","idArtist":"111239",` +
		`"strTrack":"` + title + `","strArtist":"Radiohead","strAlbum":"OK Computer",` +
		`"intDuration":"` + strconv.Itoa(duration) + `","strGenre":"Alternative Rock",` +
		`"strMusicBrainzID":"` + mbid + `",` +
		`"strTrackThumb":"%%BASE%%/img/adb_track_` + idTrack + `.jpg",` +
		`"strDescriptionEN":"Stand-in synopsis for ` + title + `, track on OK Computer by Radiohead.",` +
		`"strDescriptionDE":null` +
		`}]}`
}

// adbTracks is the three tracks keyed both ways TheAudioDB is asked for them: by
// recording MBID (track-mb.php) and by title (searchtrack.php). A slice rather
// than a map, so nothing here can depend on iteration order.
var adbTracks = []struct {
	mbid  string
	title string
	body  string
}{
	{mbRecAirbagID, "Airbag", adbTrackJSON("32001", mbRecAirbagID, "Airbag", 284000)},
	{mbRecParanoidID, "Paranoid Android", adbTrackJSON("32002", mbRecParanoidID, "Paranoid Android", 383000)},
	{mbRecSubterraneanID, "Subterranean Homesick Alien", adbTrackJSON("32003", mbRecSubterraneanID, "Subterranean Homesick Alien", 267000)},
}

// adbTrackByMBID answers track-mb.php.
func adbTrackByMBID(mbid string) (string, bool) {
	if mbid == "" {
		return "", false
	}
	for _, t := range adbTracks {
		if t.mbid == mbid {
			return t.body, true
		}
	}
	return "", false
}

// adbTrackByTitle answers searchtrack.php, matching the title case-insensitively
// (TheAudioDB's own search is case- and punctuation-tolerant, and the title on
// the wire is whatever the local file was tagged with).
func adbTrackByTitle(title string) (string, bool) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", false
	}
	for _, fixture := range adbTracks {
		if strings.EqualFold(fixture.title, t) {
			return fixture.body, true
		}
	}
	return "", false
}
