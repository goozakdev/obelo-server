package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the mirror (.scratch/linked-servers issue 07, ADR-0056
// §1–§3): what a linked Library IS on the receiving Server.
//
// Two whole Servers, over real HTTP, for the reason issue 06's tests give: the
// claim under test is about the space between two machines. The strongest form of
// it is the round trip — export a scanned Library from one Server, pull it into
// another, and ask both the same browse questions — because that is the only
// assertion that fails when the sharer discloses a field the mirror cannot land.

// --- fixture ------------------------------------------------------------------

// linkedFixture is a sharer with scanned Libraries, a home Server that has
// pasted its invite, and the mapping between the two sides' Library ids.
type linkedFixture struct {
	sharer      *testharness.Server
	sharerAdmin string
	home        *testharness.Server
	homeAdmin   string
	remoteUser  string
	// src and mirror map a Library KIND to its id on the sharer and on the home
	// Server. The two ids are always different: the mirror mints its own local ids
	// and keys the sharer's onto them by remote_id (ADR-0056 §3).
	src    map[string]string
	mirror map[string]string
}

// linkFixtures scans one Library of each named kind on a sharer, grants them all
// to a `remote` User, and links a fresh home Server to it — which pulls.
func linkFixtures(t *testing.T, kinds ...string) *linkedFixture {
	t.Helper()
	requireFixtures(t)

	f := &linkedFixture{src: map[string]string{}, mirror: map[string]string{}}
	f.sharer = testharness.New(t)
	f.sharerAdmin = adminToken(t, f.sharer)

	var granted []string
	for _, kind := range kinds {
		var libID string
		switch kind {
		case "movie":
			libID = createMovieLibrary(t, f.sharer, f.sharerAdmin, fixtureRoot(t))
		case "tv":
			libID = createTVLibrary(t, f.sharer, f.sharerAdmin, tvRoot(t))
		default:
			libID = createMusicLibrary(t, f.sharer, f.sharerAdmin, musicRoot(t))
		}
		scanLib(t, f.sharer, f.sharerAdmin, libID, "")
		f.src[kind] = libID
		granted = append(granted, libID)
	}

	f.remoteUser = createRemoteUser(t, f.sharer, f.sharerAdmin, "The other household")
	grantLibraries(t, f.sharer, f.sharerAdmin, f.remoteUser, granted...)

	f.home = testharness.New(t)
	f.homeAdmin = adminToken(t, f.home)

	invite := mintInvite(t, f.sharer, f.sharerAdmin, f.remoteUser, f.sharer.URL("")).Invite
	var link linkResp
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &link); status != http.StatusCreated {
		t.Fatalf("POST /links = %d, want 201; body: %s", status, body)
	}
	if len(link.Libraries) != len(kinds) {
		t.Fatalf("the Link brought %d libraries, want %d: %+v", len(link.Libraries), len(kinds), link.Libraries)
	}
	for _, l := range link.Libraries {
		f.mirror[l.Kind] = l.ID
	}
	for _, kind := range kinds {
		if f.mirror[kind] == "" {
			t.Fatalf("no %s library came over the link: %+v", kind, link.Libraries)
		}
	}
	return f
}

// resync pastes a FRESH invite from the same sharer, which re-keys the Link in
// place and pulls again. It is issue 08's `POST /links/{id}/sync` said the only
// way this slice can say it — and it exercises the re-key path at the same time.
func (f *linkedFixture) resync(t *testing.T) {
	t.Helper()
	invite := mintInvite(t, f.sharer, f.sharerAdmin, f.remoteUser, f.sharer.URL("")).Invite
	var link linkResp
	if status, body := postLink(t, f.home, f.homeAdmin, invite, &link); status != http.StatusOK {
		t.Fatalf("re-linking = %d, want 200 (a re-key); body: %s", status, body)
	}
}

// --- comparing two catalogs that do not share ids -----------------------------

var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// mirrorDropped is `dropped` (the Export's deliberate omissions) plus the
// fields the mirror ADDS. A linked Title says linked/available/linkedServer and
// the sharer's own copy of it does not — that difference is the feature, not a
// mismatch.
var mirrorDropped = map[string]bool{"linked": true, "available": true, "linkedServer": true}

// normalizeAcrossServers strips what the two Servers cannot agree on and folds
// every id to a placeholder. The ids MUST differ — the mirror mints its own and
// keys the sharer's onto them (ADR-0056 §3) — so comparing them would assert the
// opposite of the design. Everything else, field for field, must match.
func normalizeAcrossServers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if dropped[k] || mirrorDropped[k] ||
				strings.HasSuffix(k, "Url") || strings.HasSuffix(k, "URL") {
				continue
			}
			out[k] = normalizeAcrossServers(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, normalizeAcrossServers(val))
		}
		return out
	case string:
		if uuidLike.MatchString(t) {
			return "<id>"
		}
		return t
	default:
		return v
	}
}

func browseDoc(t *testing.T, srv *testharness.Server, token, path string) any {
	t.Helper()
	var raw json.RawMessage
	status, body := srv.AuthGET(path, token, &raw)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body: %s", path, status, body)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return normalizeAcrossServers(v)
}

// walkBrowse returns every browse document under a Library, in traversal order:
// the grid, then each entry's detail (and, for TV/music, the whole hierarchy
// beneath it). Traversal order — not sorted paths — is what lets two Servers with
// different ids be compared position by position.
func walkBrowse(t *testing.T, srv *testharness.Server, token, kind, libID string) []any {
	t.Helper()
	grid := browseDoc(t, srv, token, "/api/v1/libraries/"+libID+"/titles?limit=100")
	docs := []any{grid}

	// The walk needs the real ids, which normalization has just erased, so it is
	// re-read raw. (The comparison and the walk want opposite things from the same
	// document, which is why they are two reads and not one.)
	var raw map[string]any
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/titles?limit=100", token, &raw); status != http.StatusOK {
		t.Fatalf("listing %s: %d; body: %s", libID, status, body)
	}

	switch kind {
	case "movie":
		for _, e := range rawList(raw, "titles") {
			docs = append(docs, browseDoc(t, srv, token, "/api/v1/titles/"+str(e, "id")))
		}
	case "tv":
		for _, s := range rawList(raw, "shows") {
			showID := str(s, "id")
			seasonsPath := "/api/v1/shows/" + showID + "/seasons"
			docs = append(docs, browseDoc(t, srv, token, seasonsPath))
			var seasons map[string]any
			srv.AuthGET(seasonsPath, token, &seasons)
			for _, se := range rawList(seasons, "seasons") {
				epPath := "/api/v1/seasons/" + str(se, "id") + "/episodes"
				docs = append(docs, browseDoc(t, srv, token, epPath))
				var eps map[string]any
				srv.AuthGET(epPath, token, &eps)
				for _, ep := range rawList(eps, "episodes") {
					docs = append(docs, browseDoc(t, srv, token, "/api/v1/titles/"+str(ep, "id")))
				}
			}
		}
	case "music":
		for _, a := range rawList(raw, "artists") {
			albumsPath := "/api/v1/artists/" + str(a, "id") + "/albums"
			docs = append(docs, browseDoc(t, srv, token, albumsPath))
			var albums map[string]any
			srv.AuthGET(albumsPath, token, &albums)
			for _, al := range rawList(albums, "albums") {
				trackPath := "/api/v1/albums/" + str(al, "id") + "/tracks"
				docs = append(docs, browseDoc(t, srv, token, trackPath))
				var tracks map[string]any
				srv.AuthGET(trackPath, token, &tracks)
				for _, tr := range rawList(tracks, "tracks") {
					docs = append(docs, browseDoc(t, srv, token, "/api/v1/titles/"+str(tr, "id")))
				}
			}
		}
	}
	return docs
}

func rawList(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if e, ok := v.(map[string]any); ok {
			out = append(out, e)
		}
	}
	return out
}

// --- the tests ----------------------------------------------------------------

// TestLinkedLibraryMirrorsTheSharersBrowse is the issue's first acceptance
// criterion, in its strongest form: after the full pull at link time, the home
// Server answers every browse question the sharer answers, identically — Watch
// state, artwork and the ids aside.
func TestLinkedLibraryMirrorsTheSharersBrowse(t *testing.T) {
	for _, kind := range []string{"movie", "tv", "music"} {
		t.Run(kind, func(t *testing.T) {
			f := linkFixtures(t, kind)

			want := walkBrowse(t, f.sharer, f.sharerAdmin, kind, f.src[kind])
			got := walkBrowse(t, f.home, f.homeAdmin, kind, f.mirror[kind])
			if len(want) == 0 {
				t.Fatal("the sharer's library browsed to nothing; the fixture is empty")
			}
			if len(got) != len(want) {
				t.Fatalf("the mirror answered %d browse documents, the sharer %d", len(got), len(want))
			}
			for i := range want {
				if reflect.DeepEqual(want[i], got[i]) {
					continue
				}
				a, _ := json.MarshalIndent(want[i], "", "  ")
				b, _ := json.MarshalIndent(got[i], "", "  ")
				t.Fatalf("browse document %d differs\n--- sharer ---\n%s\n--- mirror ---\n%s", i, a, b)
			}
		})
	}
}

// TestLinkedLibraryIsBadgedAsOne: the two fields a client badges with. `linked`
// and `available` appear on the Library and on the Title/Show/Artist summaries,
// and NEVER on a local one — a household that has linked nothing sees the wire it
// always saw.
func TestLinkedLibraryIsBadgedAsOne(t *testing.T) {
	f := linkFixtures(t, "movie")
	local := createMovieLibrary(t, f.home, f.homeAdmin, fixtureRoot(t))

	var libs struct {
		Libraries []struct {
			ID        string `json:"id"`
			Linked    bool   `json:"linked"`
			Available *bool  `json:"available"`
		} `json:"libraries"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries", f.homeAdmin, &libs); status != http.StatusOK {
		t.Fatalf("GET /libraries = %d; body: %s", status, body)
	}
	seen := 0
	for _, l := range libs.Libraries {
		switch l.ID {
		case f.mirror["movie"]:
			seen++
			if !l.Linked {
				t.Error("a mirrored Library did not report linked")
			}
			if l.Available == nil || !*l.Available {
				t.Errorf("a mirrored Library on a connected Link reported available = %v", l.Available)
			}
		case local:
			seen++
			if l.Linked || l.Available != nil {
				t.Errorf("a local Library carried the linked fields: linked=%v available=%v", l.Linked, l.Available)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("GET /libraries did not return both libraries: %+v", libs.Libraries)
	}

	var grid struct {
		Titles []struct {
			Linked    bool  `json:"linked"`
			Available *bool `json:"available"`
		} `json:"titles"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/titles", f.homeAdmin, &grid); status != http.StatusOK {
		t.Fatalf("browsing the mirror = %d; body: %s", status, body)
	}
	if len(grid.Titles) == 0 {
		t.Fatal("the mirrored Library browsed to no Titles")
	}
	for _, ti := range grid.Titles {
		if !ti.Linked || ti.Available == nil || !*ti.Available {
			t.Fatalf("a mirrored Title summary is not badged: linked=%v available=%v", ti.Linked, ti.Available)
		}
	}
}

// TestReapplyingTheSameExportIsIdempotent is the third acceptance criterion. A
// second pull of an unchanged Library must leave the SAME rows with the SAME
// local ids — not a second copy of the household's shelf, and not new ids that
// would orphan every Playlist entry and every resume position.
func TestReapplyingTheSameExportIsIdempotent(t *testing.T) {
	f := linkFixtures(t, "movie")
	before := walkBrowse(t, f.home, f.homeAdmin, "movie", f.mirror["movie"])
	beforeIDs := mirrorTitleIDs(t, f)

	f.resync(t)

	var links []linkResp
	if status, body := f.home.AuthGET("/api/v1/links", f.homeAdmin, &links); status != http.StatusOK {
		t.Fatalf("GET /links = %d; body: %s", status, body)
	}
	if len(links) != 1 || len(links[0].Libraries) != 1 {
		t.Fatalf("a second pull did not reuse the Link's shelf: %+v", links)
	}
	if links[0].Libraries[0].ID != f.mirror["movie"] {
		t.Fatalf("a second pull built a new Library %q; the first was %q",
			links[0].Libraries[0].ID, f.mirror["movie"])
	}

	if got := mirrorTitleIDs(t, f); !reflect.DeepEqual(got, beforeIDs) {
		t.Errorf("a second pull changed the local Title ids\nbefore: %v\nafter:  %v", beforeIDs, got)
	}
	after := walkBrowse(t, f.home, f.homeAdmin, "movie", f.mirror["movie"])
	if !reflect.DeepEqual(before, after) {
		t.Error("a second pull of an unchanged Library changed what browse answers")
	}
}

// TestRenameOnTheSharerUpsertsAndKeepsWatchState is ADR-0056 §3's promise, and
// the whole reason rows are matched by remote_id rather than by title: the friend
// corrects a film's name, and this household's resume position survives it.
func TestRenameOnTheSharerUpsertsAndKeepsWatchState(t *testing.T) {
	f := linkFixtures(t, "movie")

	// The two sides' ids differ, so the Title to follow is found by NAME once, at
	// the start — which is the last moment the two agree on anything but the name.
	srcTitle := firstTitleID(t, f.sharer, f.sharerAdmin, f.src["movie"])
	var srcDetail struct {
		Title string `json:"title"`
	}
	if status, body := f.sharer.AuthGET("/api/v1/titles/"+srcTitle, f.sharerAdmin, &srcDetail); status != http.StatusOK {
		t.Fatalf("reading the sharer's Title = %d; body: %s", status, body)
	}
	titleID := mirroredTitleNamed(t, f, srcDetail.Title)
	setWatchStateResume(t, f.home, f.homeAdmin, titleID, 123456)

	f.sharer.Exec(`UPDATE titles SET title = ?, sort_title = ? WHERE id = ?`,
		"A Completely Different Name", "a completely different name", srcTitle)

	f.resync(t)

	var detail struct {
		ID               string `json:"id"`
		Title            string `json:"title"`
		ResumePositionMs int64  `json:"resumePositionMs"`
	}
	if status, body := f.home.AuthGET("/api/v1/titles/"+titleID, f.homeAdmin, &detail); status != http.StatusOK {
		t.Fatalf("the renamed Title is gone from the mirror: %d; body: %s", status, body)
	}
	if detail.Title != "A Completely Different Name" {
		t.Errorf("the mirror still calls the Title %q", detail.Title)
	}
	if detail.ResumePositionMs != 123456 {
		t.Errorf("resume position after the rename = %d, want 123456", detail.ResumePositionMs)
	}
}

// mirroredTitleNamed finds the mirrored Title carrying a given display title.
func mirroredTitleNamed(t *testing.T, f *linkedFixture, name string) string {
	t.Helper()
	var raw map[string]any
	path := "/api/v1/libraries/" + f.mirror["movie"] + "/titles?limit=100"
	if status, body := f.home.AuthGET(path, f.homeAdmin, &raw); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", path, status, body)
	}
	for _, row := range rawList(raw, "titles") {
		if str(row, "title") == name {
			return str(row, "id")
		}
	}
	t.Fatalf("the mirror has no Title called %q", name)
	return ""
}

// TestTombstonesHideAMirroredTitle: a File the sharer marks Missing (ADR-0008)
// and the Title it hid arrive as tombstones, and the mirror hides them too — the
// same soft delete, not a row that vanishes with the Watch state on it.
func TestTombstonesHideAMirroredTitle(t *testing.T) {
	f := linkFixtures(t, "movie")

	before := mirrorTitleIDs(t, f)
	if len(before) < 2 {
		t.Skip("the movie fixture has fewer than two Titles")
	}

	srcTitle := firstTitleID(t, f.sharer, f.sharerAdmin, f.src["movie"])
	f.sharer.Exec(`UPDATE files SET present = 0
	                 WHERE edition_id IN (SELECT id FROM editions WHERE title_id = ?)`, srcTitle)
	f.sharer.SetTitleHidden(srcTitle, true)

	f.resync(t)

	after := mirrorTitleIDs(t, f)
	if len(after) != len(before)-1 {
		t.Fatalf("the mirror browses %d Titles after one was tombstoned, want %d",
			len(after), len(before)-1)
	}
}

// TestUnlinkTakesTheMirrorWithIt: unlinking is the only thing that deletes what
// came over a Link (ADR-0056 §6) — and it deletes all of it.
func TestUnlinkTakesTheMirrorWithIt(t *testing.T) {
	f := linkFixtures(t, "movie")
	mirrorLib := f.mirror["movie"]

	var links []linkResp
	f.home.AuthGET("/api/v1/links", f.homeAdmin, &links)
	if len(links) != 1 {
		t.Fatalf("want one Link, got %d", len(links))
	}

	if status, body := f.home.JSON(http.MethodDelete, "/api/v1/links/"+links[0].ID, f.homeAdmin, nil, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE /links = %d, want 204; body: %s", status, body)
	}

	var libs struct {
		Libraries []struct {
			ID string `json:"id"`
		} `json:"libraries"`
	}
	f.home.AuthGET("/api/v1/libraries", f.homeAdmin, &libs)
	for _, l := range libs.Libraries {
		if l.ID == mirrorLib {
			t.Fatal("unlinking left the mirrored Library behind")
		}
	}
	if status, _ := f.home.AuthGET("/api/v1/libraries/"+mirrorLib+"/titles", f.homeAdmin, nil); status != http.StatusNotFound {
		t.Errorf("browsing the removed mirror answered %d, want 404", status)
	}
}

// TestEveryWriterRefusesALinkedLibrary is the issue's table: every mutating route
// that can name a mirrored Library, each answering 409 LINKED_LIBRARY.
//
// It is one table on purpose. The guards are spread across four dispatchers, and
// the failure mode this catches is not "one guard is wrong" but "somebody added a
// leaf and did not think about mirrors" — which only a list of every route can
// see.
func TestEveryWriterRefusesALinkedLibrary(t *testing.T) {
	f := linkFixtures(t, "movie", "tv", "music")

	mirrored := writerRoutes(
		f.mirror["movie"],
		firstMirroredTitleID(t, f),
		firstMirroredID(t, f, "tv", "shows"),
		firstMirroredID(t, f, "music", "artists"),
		firstMirroredAlbumID(t, f, firstMirroredID(t, f, "music", "artists")),
	)
	for _, tc := range mirrored {
		t.Run(tc.name, func(t *testing.T) {
			var env errorEnvelope
			var status int
			var body []byte
			if tc.multipart {
				status, body = f.home.Multipart(http.MethodPost, tc.path, f.homeAdmin,
					"file", "poster.jpg", "image/jpeg", []byte("\xff\xd8\xff\xe0not-really"), &env)
			} else {
				status, body = f.home.JSON(tc.method, tc.path, f.homeAdmin, tc.body, &env)
			}
			if status != http.StatusConflict || env.Error.Code != "LINKED_LIBRARY" {
				t.Fatalf("%s %s = %d %q, want 409 LINKED_LIBRARY; body: %s",
					tc.method, tc.path, status, env.Error.Code, body)
			}
		})
	}

	// The control, and the half of this test that can actually rot: the SAME
	// routes against a LOCAL Library must not answer LINKED_LIBRARY. Without it a
	// guard bolted onto every route unconditionally would pass the table above
	// while breaking the server.
	local := localWriterFixture(t, f.home, f.homeAdmin)
	for _, tc := range local {
		t.Run("local/"+tc.name, func(t *testing.T) {
			var env errorEnvelope
			var status int
			if tc.multipart {
				status, _ = f.home.Multipart(http.MethodPost, tc.path, f.homeAdmin,
					"file", "poster.jpg", "image/jpeg", []byte("\xff\xd8\xff\xe0not-really"), &env)
			} else {
				status, _ = f.home.JSON(tc.method, tc.path, f.homeAdmin, tc.body, &env)
			}
			if status == http.StatusConflict && env.Error.Code == "LINKED_LIBRARY" {
				t.Fatalf("%s %s refused a LOCAL Library as a mirror", tc.method, tc.path)
			}
			if tc.wantLocalStatus != 0 && status != tc.wantLocalStatus {
				t.Fatalf("%s %s = %d %q, want %d", tc.method, tc.path, status, env.Error.Code, tc.wantLocalStatus)
			}
		})
	}
}

// writerRoute is one mutating route the guard has to cover. The bodies are
// deliberately minimal and often nonsense: the assertion is about WHICH refusal
// comes back, and the guard runs before any of them is read.
type writerRoute struct {
	name      string
	method    string
	path      string
	body      any
	multipart bool
	// wantLocalStatus, when set, is the exact status the LOCAL leg must answer
	// (rather than only "not 409 LINKED_LIBRARY") — for a route whose body is
	// well-formed enough to reach the handler's success path.
	wantLocalStatus int
}

func writerRoutes(movieLib, titleID, showID, artistID, albumID string) []writerRoute {
	return []writerRoute{
		{name: "library scan", method: http.MethodPost, path: "/api/v1/libraries/" + movieLib + "/scan", body: map[string]any{}},
		{name: "library enrich", method: http.MethodPost, path: "/api/v1/libraries/" + movieLib + "/enrich", body: map[string]any{}},
		{name: "library enrichment policy", method: http.MethodPut, path: "/api/v1/libraries/" + movieLib + "/enrichment-policy", body: map[string]any{"tmdbEnabled": true}},
		{name: "library fix-match", method: http.MethodPost, path: "/api/v1/libraries/" + movieLib + "/fix-match", body: map[string]any{"folderPath": "/x", "title": "X"}},
		{name: "library override delete", method: http.MethodDelete, path: "/api/v1/libraries/" + movieLib + "/overrides/whatever"},
		{name: "library delete", method: http.MethodDelete, path: "/api/v1/libraries/" + movieLib},
		{name: "library add root", method: http.MethodPatch, path: "/api/v1/libraries/" + movieLib, body: map[string]any{"addRootFolders": []string{"/tmp/obelo-nope"}}},

		{name: "title targeted scan", method: http.MethodPost, path: "/api/v1/titles/" + titleID + "/scan", body: map[string]any{}},
		{name: "title review", method: http.MethodPost, path: "/api/v1/titles/" + titleID + "/review", body: map[string]any{}},
		{name: "title metadata", method: http.MethodPut, path: "/api/v1/titles/" + titleID + "/metadata", body: map[string]any{"overview": "no"}},
		{name: "title lock release", method: http.MethodDelete, path: "/api/v1/titles/" + titleID + "/metadata/locks/overview"},
		// wantLocalStatus is 404, not 200: this table's own "library delete" case
		// above already deleted movieLib by the time this one runs (both share the
		// route slice), so titleID is gone too — a well-formed source only gets it
		// past the 400 the brief closed, not past a Library this same test removed.
		{name: "title enrichment override", method: http.MethodPut, path: "/api/v1/titles/" + titleID + "/enrichmentOverride", body: map[string]any{"externalId": "1", "source": "tmdb"}, wantLocalStatus: http.StatusNotFound},
		{name: "title identity correction", method: http.MethodPut, path: "/api/v1/titles/" + titleID + "/identityCorrection", body: map[string]any{"externalId": "1"}},
		{name: "title artwork pick", method: http.MethodPut, path: "/api/v1/titles/" + titleID + "/artwork", body: map[string]any{"role": "poster", "ref": "x"}},
		{name: "title artwork upload", method: http.MethodPost, path: "/api/v1/titles/" + titleID + "/artworkUpload?role=poster", multipart: true},
		{name: "title subtitle fetch", method: http.MethodPost, path: "/api/v1/titles/" + titleID + "/subtitles/fetch", body: map[string]any{"candidateId": "x"}},

		{name: "show targeted scan", method: http.MethodPost, path: "/api/v1/shows/" + showID + "/scan", body: map[string]any{}},
		{name: "show review", method: http.MethodPost, path: "/api/v1/shows/" + showID + "/review", body: map[string]any{}},
		{name: "show review episodes", method: http.MethodPost, path: "/api/v1/shows/" + showID + "/reviewEpisodes", body: map[string]any{}},
		{name: "show identity correction", method: http.MethodPut, path: "/api/v1/shows/" + showID + "/identityCorrection", body: map[string]any{"externalId": "1"}},
		{name: "show matcher apply", method: http.MethodPut, path: "/api/v1/shows/" + showID + "/matcher", body: map[string]any{"groups": []any{}}},
		{name: "show metadata", method: http.MethodPut, path: "/api/v1/shows/" + showID + "/metadata", body: map[string]any{"overview": "no"}},
		{name: "show enrichment override", method: http.MethodPut, path: "/api/v1/shows/" + showID + "/enrichmentOverride", body: map[string]any{"externalId": "1", "source": "tmdb"}, wantLocalStatus: http.StatusOK},
		{name: "show artwork pick", method: http.MethodPut, path: "/api/v1/shows/" + showID + "/artwork", body: map[string]any{"role": "poster", "ref": "x"}},
		{name: "show artwork upload", method: http.MethodPost, path: "/api/v1/shows/" + showID + "/artworkUpload?role=poster", multipart: true},
		{name: "show lock release", method: http.MethodDelete, path: "/api/v1/shows/" + showID + "/metadata/locks/overview"},

		{name: "artist targeted scan", method: http.MethodPost, path: "/api/v1/artists/" + artistID + "/scan", body: map[string]any{}},
		{name: "artist metadata", method: http.MethodPut, path: "/api/v1/artists/" + artistID + "/metadata", body: map[string]any{"overview": "no"}},
		{name: "artist enrichment override", method: http.MethodPut, path: "/api/v1/artists/" + artistID + "/enrichmentOverride", body: map[string]any{"externalId": "1", "source": "musicbrainz"}, wantLocalStatus: http.StatusOK},
		{name: "artist artwork pick", method: http.MethodPut, path: "/api/v1/artists/" + artistID + "/artwork", body: map[string]any{"role": "poster", "ref": "x"}},
		{name: "artist artwork upload", method: http.MethodPost, path: "/api/v1/artists/" + artistID + "/artworkUpload?role=poster", multipart: true},

		{name: "album targeted scan", method: http.MethodPost, path: "/api/v1/albums/" + albumID + "/scan", body: map[string]any{}},
		{name: "album metadata", method: http.MethodPut, path: "/api/v1/albums/" + albumID + "/metadata", body: map[string]any{"overview": "no"}},
		{name: "album enrichment override", method: http.MethodPut, path: "/api/v1/albums/" + albumID + "/enrichmentOverride", body: map[string]any{"externalId": "1", "source": "musicbrainz"}, wantLocalStatus: http.StatusOK},
		{name: "album artwork pick", method: http.MethodPut, path: "/api/v1/albums/" + albumID + "/artwork", body: map[string]any{"role": "poster", "ref": "x"}},
		{name: "album artwork upload", method: http.MethodPost, path: "/api/v1/albums/" + albumID + "/artworkUpload?role=poster", multipart: true},
	}
}

// localWriterFixture scans one Library of each kind LOCALLY on the given Server
// and returns the same route table addressed at those rows.
func localWriterFixture(t *testing.T, srv *testharness.Server, admin string) []writerRoute {
	t.Helper()
	movieLib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, movieLib, "")
	tvLib := createTVLibrary(t, srv, admin, tvRoot(t))
	scanLib(t, srv, admin, tvLib, "")
	musicLib := createMusicLibrary(t, srv, admin, musicRoot(t))
	scanLib(t, srv, admin, musicLib, "")

	titleID := firstTitleID(t, srv, admin, movieLib)
	showID := firstGridID(t, srv, admin, tvLib, "shows")
	artistID := firstGridID(t, srv, admin, musicLib, "artists")

	var albums map[string]any
	srv.AuthGET("/api/v1/artists/"+artistID+"/albums", admin, &albums)
	rows := rawList(albums, "albums")
	if len(rows) == 0 {
		t.Fatal("the local music fixture has no Albums")
	}
	return writerRoutes(movieLib, titleID, showID, artistID, str(rows[0], "id"))
}

func firstGridID(t *testing.T, srv *testharness.Server, token, libID, key string) string {
	t.Helper()
	var raw map[string]any
	path := "/api/v1/libraries/" + libID + "/titles?limit=100"
	if status, body := srv.AuthGET(path, token, &raw); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", path, status, body)
	}
	rows := rawList(raw, key)
	if len(rows) == 0 {
		t.Fatalf("%s has no %s", libID, key)
	}
	return str(rows[0], "id")
}

// TestRenamingALinkedLibraryIsStillAllowed: the one write that survives. What
// this household calls somebody else's shelf is this household's business
// (ADR-0056 §1), and the name must not be stomped by the next pull either.
func TestRenamingALinkedLibraryIsStillAllowed(t *testing.T) {
	f := linkFixtures(t, "movie")

	var out struct {
		Name   string `json:"name"`
		Linked bool   `json:"linked"`
	}
	status, body := f.home.JSON(http.MethodPatch, "/api/v1/libraries/"+f.mirror["movie"], f.homeAdmin,
		map[string]any{"name": "Dave's films"}, &out)
	if status != http.StatusOK {
		t.Fatalf("renaming a linked Library = %d, want 200; body: %s", status, body)
	}
	if out.Name != "Dave's films" || !out.Linked {
		t.Fatalf("renamed Library = %+v", out)
	}

	f.resync(t)

	var after struct {
		Name string `json:"name"`
	}
	f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"], f.homeAdmin, &after)
	if after.Name != "Dave's films" {
		t.Errorf("the next pull renamed the shelf back to %q", after.Name)
	}
}

// TestAttentionQueueSkipsALinkedLibrary: the Needs-Fixing queue is the sharer's,
// not this household's (ADR-0056 §1). Every one of its reads answers empty for a
// mirror — an offer to fix a file that is on another machine is an offer nobody
// here can take.
func TestAttentionQueueSkipsALinkedLibrary(t *testing.T) {
	f := linkFixtures(t, "movie")
	lib := f.mirror["movie"]

	// Flag something on the mirror the way an arriving export could, so the empty
	// answers below are a FILTER and not an empty database.
	f.home.Exec(`UPDATE titles SET needs_review = 1, enrichment_status = 'unmatched'
	              WHERE library_id = ?`, lib)

	for _, probe := range []struct {
		path string
		key  string
	}{
		{"/api/v1/libraries/" + lib + "/needs-review", "items"},
		{"/api/v1/libraries/" + lib + "/enrichment-attention", "titles"},
		{"/api/v1/libraries/" + lib + "/unmatched", "files"},
		{"/api/v1/libraries/" + lib + "/show-problems", "shows"},
		{"/api/v1/libraries/" + lib + "/overrides", "overrides"},
	} {
		var raw map[string]any
		status, body := f.home.AuthGET(probe.path, f.homeAdmin, &raw)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200; body: %s", probe.path, status, body)
		}
		if n := len(rawList(raw, probe.key)); n != 0 {
			t.Errorf("%s listed %d rows for a mirrored Library; the queue is the sharer's", probe.path, n)
		}
	}
}

// TestAMemberIsGrantedALinkedLibraryLikeAnyOther is ADR-0056 §2: a linked
// Library is a Library, so the grant dialog that already exists is the whole
// access story.
func TestAMemberIsGrantedALinkedLibraryLikeAnyOther(t *testing.T) {
	f := linkFixtures(t, "movie")
	memberID := f.home.CreateUser(f.homeAdmin, "kid", "hunter2hunter2", "member")
	memberTok := f.home.LoginAs("kid", "hunter2hunter2")

	// Ungranted: the Library is hidden entirely (404-not-403).
	if status, _ := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/titles", memberTok, nil); status != http.StatusNotFound {
		t.Fatalf("an ungranted mirrored Library answered %d, want 404", status)
	}

	grantLibraries(t, f.home, f.homeAdmin, memberID, f.mirror["movie"])

	var grid struct {
		Titles []struct {
			ID string `json:"id"`
		} `json:"titles"`
	}
	status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/titles", memberTok, &grid)
	if status != http.StatusOK {
		t.Fatalf("a granted mirrored Library answered %d, want 200; body: %s", status, body)
	}
	if len(grid.Titles) == 0 {
		t.Error("a Member granted the mirror sees nothing in it")
	}
}

// TestMirroredTitlesReachHomeSearchAndPlaylists is the rest of the first
// acceptance criterion — and the payoff of ADR-0056 §3's "a mirrored Title is a
// local row": three surfaces that were never told linking exists.
func TestMirroredTitlesReachHomeSearchAndPlaylists(t *testing.T) {
	f := linkFixtures(t, "movie")
	titleID := firstMirroredTitleID(t, f)

	var detail struct {
		Title string `json:"title"`
	}
	f.home.AuthGET("/api/v1/titles/"+titleID, f.homeAdmin, &detail)

	// Search finds it.
	var search struct {
		Movies []struct {
			ID string `json:"id"`
		} `json:"movies"`
	}
	q := "/api/v1/search?q=" + strings.ReplaceAll(detail.Title, " ", "+")
	if status, body := f.home.AuthGET(q, f.homeAdmin, &search); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", q, status, body)
	}
	if !mentionsID(search.Movies, titleID) {
		t.Errorf("search for %q did not find the mirrored Title", detail.Title)
	}

	// Continue Watching picks it up from a resume position written HERE
	// (ADR-0056 §5 — Watch state for a mirrored Title is written here, only).
	setWatchStateResume(t, f.home, f.homeAdmin, titleID, 60000)
	var home struct {
		ContinueWatching []struct {
			ID string `json:"id"`
		} `json:"continueWatching"`
	}
	if status, body := f.home.AuthGET("/api/v1/home", f.homeAdmin, &home); status != http.StatusOK {
		t.Fatalf("GET /home = %d; body: %s", status, body)
	}
	if !mentionsID(home.ContinueWatching, titleID) {
		t.Error("a mirrored Title with a resume position is not in Continue Watching")
	}

	// A Playlist holds it like any other Title.
	var pl struct {
		ID string `json:"id"`
	}
	if status, body := f.home.JSON(http.MethodPost, "/api/v1/playlists", f.homeAdmin,
		map[string]any{"name": "Friend's films"}, &pl); status != http.StatusCreated {
		t.Fatalf("creating a Playlist = %d; body: %s", status, body)
	}
	if status, body := f.home.JSON(http.MethodPost, "/api/v1/playlists/"+pl.ID+"/items", f.homeAdmin,
		map[string]any{"titleId": titleID}, nil); status != http.StatusNoContent &&
		status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("appending a mirrored Title to a Playlist = %d; body: %s", status, body)
	}
	var got struct {
		Members []struct {
			ID     string `json:"id"`
			Linked bool   `json:"linked"`
		} `json:"members"`
	}
	if status, body := f.home.AuthGET("/api/v1/playlists/"+pl.ID, f.homeAdmin, &got); status != http.StatusOK {
		t.Fatalf("reading the Playlist = %d; body: %s", status, body)
	}
	found := false
	for _, it := range got.Members {
		if it.ID == titleID {
			found = true
			if !it.Linked {
				t.Error("a mirrored Title in a Playlist is not badged linked")
			}
		}
	}
	if !found {
		t.Errorf("the Playlist lost the mirrored Title: %+v", got.Members)
	}
}

// TestTheMirrorIsNeverReShared: a Library that arrived over a Link is not
// exportable onward (ADR-0054 §4, ADR-0056 §7). The hook issue 05 left nil is
// wired now, and this is what it buys.
func TestTheMirrorIsNeverReShared(t *testing.T) {
	f := linkFixtures(t, "movie")
	// The peer is granted NOTHING: since issue 11 the grant itself is refused
	// (422 LINKED_GRANT — linked_grant_test.go), which is the earlier of the two
	// refusals. The export must still answer 404 on its own, which is what the
	// Admin leg below — all Libraries in scope — actually proves.
	peer := linkedServerToken(t, f.home, f.homeAdmin)

	if status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/export", peer, nil); status != http.StatusNotFound {
		t.Fatalf("exporting a mirrored Library = %d, want 404; body: %s", status, body)
	}
	// An Admin gets the same answer: this is not an access decision.
	if status, _ := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/export", f.homeAdmin, nil); status != http.StatusNotFound {
		t.Fatalf("an Admin exported a mirrored Library (%d); sharing does not travel", status)
	}
}

// --- small readers ------------------------------------------------------------

func mirrorTitleIDs(t *testing.T, f *linkedFixture) []string {
	t.Helper()
	var grid struct {
		Titles []struct {
			ID string `json:"id"`
		} `json:"titles"`
	}
	if status, body := f.home.AuthGET("/api/v1/libraries/"+f.mirror["movie"]+"/titles?limit=100",
		f.homeAdmin, &grid); status != http.StatusOK {
		t.Fatalf("browsing the mirror = %d; body: %s", status, body)
	}
	out := make([]string, 0, len(grid.Titles))
	for _, ti := range grid.Titles {
		out = append(out, ti.ID)
	}
	sort.Strings(out)
	return out
}

func firstMirroredTitleID(t *testing.T, f *linkedFixture) string {
	t.Helper()
	ids := mirrorTitleIDs(t, f)
	if len(ids) == 0 {
		t.Fatal("the mirrored Library has no Titles")
	}
	return ids[0]
}

func firstMirroredID(t *testing.T, f *linkedFixture, kind, key string) string {
	t.Helper()
	var raw map[string]any
	path := "/api/v1/libraries/" + f.mirror[kind] + "/titles?limit=100"
	if status, body := f.home.AuthGET(path, f.homeAdmin, &raw); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", path, status, body)
	}
	rows := rawList(raw, key)
	if len(rows) == 0 {
		t.Fatalf("the mirrored %s Library has no %s", kind, key)
	}
	return str(rows[0], "id")
}

func firstMirroredAlbumID(t *testing.T, f *linkedFixture, artistID string) string {
	t.Helper()
	var raw map[string]any
	path := "/api/v1/artists/" + artistID + "/albums"
	if status, body := f.home.AuthGET(path, f.homeAdmin, &raw); status != http.StatusOK {
		t.Fatalf("GET %s = %d; body: %s", path, status, body)
	}
	albums := rawList(raw, "albums")
	if len(albums) == 0 {
		t.Fatal("the mirrored Artist has no Albums")
	}
	return str(albums[0], "id")
}

func setWatchStateResume(t *testing.T, srv *testharness.Server, token, titleID string, ms int64) {
	t.Helper()
	srv.Exec(`INSERT INTO watch_state (user_id, title_id, resume_position_ms, watched)
	          SELECT id, ?, ?, 0 FROM users WHERE role = 'admin'`, titleID, ms)
}

// mentionsID reports whether any row in a decoded list carries the id. It works
// off the JSON rather than a field so one helper serves rows of different shapes.
func mentionsID[T any](rows []T, id string) bool {
	for _, r := range rows {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), fmt.Sprintf("%q", id)) {
			return true
		}
	}
	return false
}
