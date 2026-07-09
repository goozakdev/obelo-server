package api_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
)

// Cast-photos/01 black-box tests: scan + enrich a Movie library with a FAKE
// MetadataProvider (a movie cast carrying person refs + headshot URLs, and one
// member with no photo) and a FAKE ArtworkFetcher (image bytes, plus a URL that
// 404s), then assert the decorated cast + served headshots through the HTTP API —
// zero network. Prior art: enrich_test.go (the two seams) and cookie_test.go (the
// media-cookie artwork GET). Asserts every acceptance criterion in the issue.

// --- Fakes ------------------------------------------------------------------

// castFetcher returns deterministic bytes per URL ("PHOTO:"+url) so a served
// headshot can be matched back to the URL it came from, records every fetched URL
// for dedupe assertions, and fails a configured set of URLs (the non-fatal path).
type castFetcher struct {
	mu   sync.Mutex
	urls []string
	fail map[string]error
}

func (f *castFetcher) Fetch(_ context.Context, u string) ([]byte, string, error) {
	f.mu.Lock()
	f.urls = append(f.urls, u)
	err := f.fail[u]
	f.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	return []byte("PHOTO:" + u), "image/jpeg", nil
}

func (f *castFetcher) count(u string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.urls {
		if got == u {
			n++
		}
	}
	return n
}

// castMetaFor returns the faked movie metadata per fixture title. Dune credits
// Chalamet (with a headshot) and Zendaya (NO headshot); Blade Runner re-credits
// the SAME Chalamet ref (dedupe) plus Harrison Ford whose headshot download fails
// (non-fatal). Every other title gets a plain rich result.
func castMetaFor(title string) enrich.TitleMetadata {
	base := func(cast []enrich.Credit) enrich.TitleMetadata {
		m := richMeta()
		m.Cast = cast
		return m
	}
	switch title {
	case "Dune":
		return base([]enrich.Credit{
			{Person: "Timothée Chalamet", Character: "Paul Atreides", Kind: "cast",
				PersonRef: "tmdb:1", ImageURL: "https://img.example/chalamet.jpg"}, // shared w/ Blade Runner
			{Person: "Zendaya", Character: "Chani", Kind: "cast", PersonRef: "tmdb:2"}, // no headshot
			{Person: "Rebecca Ferguson", Character: "Lady Jessica", Kind: "cast",
				PersonRef: "tmdb:4", ImageURL: "https://img.example/ferguson.jpg"}, // Dune-exclusive
		})
	case "Blade Runner":
		return base([]enrich.Credit{
			{Person: "Timothée Chalamet", Character: "Cameo", Kind: "cast",
				PersonRef: "tmdb:1", ImageURL: "https://img.example/chalamet.jpg"}, // same ref → dedupe
			{Person: "Harrison Ford", Character: "Deckard", Kind: "cast",
				PersonRef: "tmdb:3", ImageURL: "https://img.example/ford.jpg"}, // download fails
		})
	default:
		return base(nil)
	}
}

// --- Test wire shapes -------------------------------------------------------

type castDetailResp struct {
	ID               string `json:"id"`
	IdentityKey      string `json:"identityKey"`
	Watched          bool   `json:"watched"`
	EnrichmentStatus string `json:"enrichmentStatus"`
	Cast             []struct {
		Person       string `json:"person"`
		Character    string `json:"character"`
		Kind         string `json:"kind"`
		PersonID     string `json:"personId"`
		PhotoVersion string `json:"photoVersion"`
	} `json:"cast"`
}

func getCastDetail(t *testing.T, srv *testharness.Server, token, titleID string) castDetailResp {
	t.Helper()
	var d castDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+titleID, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title = %d, want 200; body: %s", status, body)
	}
	return d
}

// personPhotoPath builds the cast-headshot route path for a person ref + role,
// mirroring the web personPhotoUrl helper (the ref is url-encoded).
func personPhotoPath(ref, role string) string {
	return "/api/v1/people/" + url.PathEscape(ref) + "/artwork/" + role
}

// findCredit returns the cast entry for a person (fatal if absent).
func findCredit(t *testing.T, d castDetailResp, person string) struct {
	Person       string `json:"person"`
	Character    string `json:"character"`
	Kind         string `json:"kind"`
	PersonID     string `json:"personId"`
	PhotoVersion string `json:"photoVersion"`
} {
	t.Helper()
	for _, c := range d.Cast {
		if c.Person == person {
			return c
		}
	}
	t.Fatalf("cast member %q not found in %+v", person, d.Cast)
	return d.Cast[0]
}

func castEnrichSetup(t *testing.T) (*testharness.Server, *castFetcher, string, string) {
	t.Helper()
	requireFixtures(t)
	prov := &fakeProvider{fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		return castMetaFor(ref.Title), nil
	}}
	fetch := &castFetcher{fail: map[string]error{
		"https://img.example/ford.jpg": enrich.ErrArtworkNotFound,
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(fetch),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, fetch, token, libID
}

// --- Tests ------------------------------------------------------------------

// TestCastPhotosCaptureAndServe: after enrichment, the movie detail cast carries a
// personId and its /people/{ref}/artwork/profile serves the cached bytes via the
// media cookie; a member with no headshot still appears (name + character) and its
// photo route 404s. Identity + watch state are untouched by the capture.
func TestCastPhotosCaptureAndServe(t *testing.T) {
	srv, _, token, libID := castEnrichSetup(t)

	id := titleIDByName(t, srv, token, libID, "Dune")

	// Identity + a watch state recorded BEFORE enrichment; capturing cast must not
	// disturb either (ADR-0002/0014).
	before := getCastDetail(t, srv, token, id)
	srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/watchState", token, map[string]any{"watched": true}, nil)

	enrichLib(t, srv, token, libID, "")

	d := getCastDetail(t, srv, token, id)
	if d.EnrichmentStatus != "matched" {
		t.Fatalf("status = %q, want matched", d.EnrichmentStatus)
	}
	if d.IdentityKey != before.IdentityKey || d.IdentityKey == "" {
		t.Errorf("identity changed by cast capture: before=%q after=%q", before.IdentityKey, d.IdentityKey)
	}
	if !d.Watched {
		t.Errorf("watch state lost across cast capture")
	}

	// Chalamet carries a personId + photo version and serves his headshot bytes.
	ch := findCredit(t, d, "Timothée Chalamet")
	if ch.PersonID != "tmdb:1" || ch.Character != "Paul Atreides" {
		t.Errorf("Chalamet credit = %+v, want personId tmdb:1 + character", ch)
	}
	if ch.PhotoVersion == "" {
		t.Errorf("Chalamet has a cached photo but no photoVersion cache-bust token: %+v", ch)
	}

	// Serve the headshot with ONLY the media cookie (browser <img> path).
	_, cookie := loginWithCookie(t, srv, "brandon", adminPassword, "web-cast")
	resp := cookieGET(t, srv, personPhotoPath("tmdb:1", "profile"), cookie, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("headshot via cookie = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("headshot content-type = %q, want image/*", ct)
	}
	status, body := authBytes(t, srv, token, personPhotoPath("tmdb:1", "profile"))
	if status != http.StatusOK || string(body) != "PHOTO:https://img.example/chalamet.jpg" {
		t.Errorf("headshot bytes = %q (status %d), want the fetched fake bytes", body, status)
	}

	// Zendaya has no headshot: present with name + character, no personId photo → 404.
	ze := findCredit(t, d, "Zendaya")
	if ze.PersonID != "tmdb:2" || ze.Character != "Chani" {
		t.Errorf("Zendaya credit = %+v, want personId tmdb:2 + character (name/character kept)", ze)
	}
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:2", "profile")); st != http.StatusNotFound {
		t.Errorf("photoless person headshot = %d, want 404", st)
	}
}

// TestCastPhotosDedupeAcrossMovies: the same person ref in two movies yields ONE
// download / one cached headshot, and both movies' cast reference it.
func TestCastPhotosDedupeAcrossMovies(t *testing.T) {
	srv, fetch, token, libID := castEnrichSetup(t)
	enrichLib(t, srv, token, libID, "")

	duneID := titleIDByName(t, srv, token, libID, "Dune")
	bladeID := titleIDByName(t, srv, token, libID, "Blade Runner")
	dune := getCastDetail(t, srv, token, duneID)
	blade := getCastDetail(t, srv, token, bladeID)

	if findCredit(t, dune, "Timothée Chalamet").PersonID != "tmdb:1" ||
		findCredit(t, blade, "Timothée Chalamet").PersonID != "tmdb:1" {
		t.Errorf("both movies should reference the same person ref tmdb:1")
	}
	// The shared headshot URL was fetched exactly once (cross-title dedupe).
	if n := fetch.count("https://img.example/chalamet.jpg"); n != 1 {
		t.Errorf("shared headshot fetched %d times, want 1 (dedupe)", n)
	}
	// It still serves for both movies (one cached row, both cast link it).
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:1", "profile")); st != http.StatusOK {
		t.Errorf("shared headshot serve = %d, want 200", st)
	}
}

// TestCastPhotosDownloadFailureNonFatal: a headshot whose download 404s leaves the
// movie enriched with the cast member's name + character intact and no photo; the
// pass does not fail.
func TestCastPhotosDownloadFailureNonFatal(t *testing.T) {
	srv, _, token, libID := castEnrichSetup(t)
	res := enrichLib(t, srv, token, libID, "")
	if res.Failed != 0 || res.Matched != res.Total {
		t.Fatalf("enrich result = %+v, want all matched (a headshot 404 must not fail the pass)", res)
	}

	bladeID := titleIDByName(t, srv, token, libID, "Blade Runner")
	blade := getCastDetail(t, srv, token, bladeID)
	ford := findCredit(t, blade, "Harrison Ford")
	if ford.Character != "Deckard" || ford.PersonID != "tmdb:3" {
		t.Errorf("Ford credit = %+v, want name/character intact despite failed headshot", ford)
	}
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:3", "profile")); st != http.StatusNotFound {
		t.Errorf("failed-download headshot = %d, want 404 (no photo)", st)
	}
}

// TestCastPhotosLockedCastNotOverwritten: an Admin-locked cast is neither
// overwritten NOR its headshots (re)fetched on enrichment.
func TestCastPhotosLockedCastNotOverwritten(t *testing.T) {
	srv, fetch, token, libID := castEnrichSetup(t)
	id := titleIDByName(t, srv, token, libID, "Dune")

	// Hand-edit (and thereby Lock) the cast to a single manual member with no photo,
	// BEFORE enrichment runs.
	if st, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+id+"/metadata", token,
		map[string]any{"cast": []map[string]any{{"person": "Manual Actor", "kind": "cast"}}}, nil); st != http.StatusOK {
		t.Fatalf("lock cast = %d; body: %s", st, body)
	}

	enrichLib(t, srv, token, libID, "full")

	d := getCastDetail(t, srv, token, id)
	if len(d.Cast) != 1 || d.Cast[0].Person != "Manual Actor" {
		t.Errorf("locked cast overwritten by enrichment: %+v", d.Cast)
	}
	// The locked cast's headshots were not fetched at all. Ferguson's headshot is
	// reachable ONLY through Dune's (now-locked) cast, so a zero count proves the
	// locked cast's photos were never refetched (Chalamet is excluded here because
	// Blade Runner's unlocked cast also credits him).
	if n := fetch.count("https://img.example/ferguson.jpg"); n != 0 {
		t.Errorf("locked cast headshot fetched %d times, want 0 (not refetched)", n)
	}
}

// TestCastPhotosAccessScoped: a person only reachable through a Library the viewer
// can't access is not served that viewer's headshot (404), while an Admin gets it.
func TestCastPhotosAccessScoped(t *testing.T) {
	srv, _, token, libID := castEnrichSetup(t)
	enrichLib(t, srv, token, libID, "")

	// Admin can serve the headshot.
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:1", "profile")); st != http.StatusOK {
		t.Fatalf("admin headshot serve = %d, want 200", st)
	}

	// A Member with NO grant to the crediting Library is hidden it as 404.
	memberID := srv.CreateUser(token, "kid", "memberpass123", "member")
	grantLibraries(t, srv, token, memberID) // clear: no libraries granted
	member := srv.LoginAs("kid", "memberpass123")
	if st, _ := authBytes(t, srv, member, personPhotoPath("tmdb:1", "profile")); st != http.StatusNotFound {
		t.Errorf("ungranted member headshot = %d, want 404 (access follows crediting titles)", st)
	}

	// Once granted, the same Member can serve it (access opens with the grant).
	grantLibraries(t, srv, token, memberID, libID)
	member = srv.LoginAs("kid", "memberpass123")
	if st, _ := authBytes(t, srv, member, personPhotoPath("tmdb:1", "profile")); st != http.StatusOK {
		t.Errorf("granted member headshot = %d, want 200", st)
	}
}

// TestCastPhotosUnknownPerson404: a photo request for a person no Title credits
// (unknown ref) 404s cleanly.
func TestCastPhotosUnknownPerson404(t *testing.T) {
	srv, _, token, libID := castEnrichSetup(t)
	enrichLib(t, srv, token, libID, "")
	if st, _ := authBytes(t, srv, token, personPhotoPath("tmdb:999999", "profile")); st != http.StatusNotFound {
		t.Errorf("unknown person headshot = %d, want 404", st)
	}
}
