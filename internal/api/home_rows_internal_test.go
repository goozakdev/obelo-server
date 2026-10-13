package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/organize"
	"github.com/goozakdev/obelo-server/internal/store"
)

// GET /home `rows` (home-discovery-rows 04): the interleaved Home rows and My List.

var updateGolden = flag.Bool("update", false, "rewrite the testdata golden files")

type homeRowsFixture struct {
	t    *testing.T
	db   *store.DB
	deps Deps
}

func newHomeRowsFixture(t *testing.T) *homeRowsFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "home.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &homeRowsFixture{t: t, db: db}
	f.exec(`INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','h'),('u2','u2','member','h')`)
	f.exec(`INSERT INTO libraries (id, name, kind) VALUES ('mA','Movies','movie'),('tv','Shows','tv')`)
	f.deps = Deps{
		Catalog:  catalog.NewService(db, t.TempDir()),
		Organize: organize.NewService(db),
		// Wednesday 2026-10-14, local noon: any zone lands on the same Server-local day.
		Now: func() time.Time { return time.Date(2026, time.October, 14, 12, 0, 0, 0, time.Local) },
	}
	f.seed()
	return f
}

func (f *homeRowsFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

// seed: 12 Comedy/A24/Tom Hanks 1980s Movies (m11 rated R), 10 Drama/HBO 1990s
// Shows with one Episode each. Every timestamp is fixed so the payload is stable.
func (f *homeRowsFixture) seed() {
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("m%02d", i)
		rating := "PG"
		if i == 11 {
			rating = "R"
		}
		f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year, content_rating, added_at)
		        VALUES (?,'mA','movie',?,?,?,?,?,?)`,
			id, "Movie "+id, id, id, 1980+i%10, rating, fmt.Sprintf("2026-09-%02d 10:00:00", i))
		f.exec(`INSERT INTO title_genres (title_id, genre) VALUES (?,'Comedy')`, id)
		f.exec(`INSERT INTO title_studios (title_id, studio, ord) VALUES (?,'A24',0)`, id)
		f.exec(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?,'Tom Hanks','tmdb:31','cast',0)`, id)
	}
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("s%02d", i)
		f.exec(`INSERT INTO shows (id, library_id, title, identity_key, sort_title, year, added_at) VALUES (?,'tv',?,?,?,?,?)`,
			id, "Show "+id, id, id, 1990+i%10, fmt.Sprintf("2026-08-%02d 10:00:00", i))
		f.exec(`INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('show',?,'Drama')`, id)
		f.exec(`INSERT INTO entity_networks (entity_type, entity_id, network, ord) VALUES ('show',?,'HBO',0)`, id)
		f.exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES (?,?,1,?)`, id+"-s1", id, id+"|s01")
		f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number, added_at)
		        VALUES (?,'tv','episode','ep',?,'ep',?,1,1,'2026-01-01 00:00:00')`, id+"-e1", id+"e1", id+"-s1")
	}
}

// watchlist appends the titles to u1's Watchlist, in order.
func (f *homeRowsFixture) watchlist(user string, scope access.Scope, titleIDs ...string) {
	f.t.Helper()
	for _, id := range titleIDs {
		if _, err := f.deps.Organize.AppendToWatchlist(scope, user, id); err != nil {
			f.t.Fatalf("AppendToWatchlist %s: %v", id, err)
		}
	}
}

// home calls the handler as the User with the scope and returns the raw JSON.
func (f *homeRowsFixture) home(user string, scope access.Scope) []byte {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/home", nil)
	ctx := withIdentity(r.Context(), identity{User: store.User{ID: user}})
	r = r.WithContext(withScope(ctx, scope))
	w := httptest.NewRecorder()
	handleHome(f.deps)(w, r)
	if w.Code != http.StatusOK {
		f.t.Fatalf("home status = %d; body: %s", w.Code, w.Body)
	}
	return w.Body.Bytes()
}

type homeRowsView struct {
	Rows []struct {
		Kind      string `json:"kind"`
		Title     string `json:"title"`
		MediaKind string `json:"mediaKind"`
		Items     []struct {
			ID string `json:"id"`
		} `json:"items"`
		Source *struct {
			Type         string   `json:"type"`
			Value        string   `json:"value"`
			DisplayValue string   `json:"displayValue"`
			LibraryIDs   []string `json:"libraryIds"`
		} `json:"source"`
		Total *int `json:"total"`
	} `json:"rows"`
}

func (v homeRowsView) kinds() []string {
	out := []string{}
	for _, r := range v.Rows {
		out = append(out, r.Kind)
	}
	return out
}

func decodeRows(t *testing.T, body []byte) homeRowsView {
	t.Helper()
	var v homeRowsView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	return v
}

// wantPattern is the fixed Home order with empty rows dropped:
// Continue Watching, Up Next, Discovery[0], My List, Discovery[1], Recently Added,
// Discovery[2..].
func wantPattern(cw, un, ml, ra bool, discovery int) []string {
	var out []string
	add := func(on bool, k string) {
		if on {
			out = append(out, k)
		}
	}
	add(cw, "continueWatching")
	add(un, "upNext")
	if discovery > 0 {
		out = append(out, "discovery")
	}
	add(ml, "myList")
	if discovery > 1 {
		out = append(out, "discovery")
	}
	add(ra, "recentlyAdded")
	for i := 2; i < discovery; i++ {
		out = append(out, "discovery")
	}
	return out
}

func countKind(v homeRowsView, kind string) int {
	n := 0
	for _, r := range v.Rows {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func TestHomeRowsFollowTheInterleavePattern(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	all := access.AllAccess()
	f.watchlist("u1", all, "m05", "m02")
	f.exec(`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, updated_at) VALUES ('w1','u1','m07',60000,'2026-10-01 00:00:00')`)
	// An Episode watched makes its Show started, so the next Episode is Up Next.
	f.exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('s01-s2','s01',2,'s01|s02')`)
	f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number, added_at)
	        VALUES ('s01-e2','tv','episode','ep2','s01e2','ep2','s01-s2',2,1,'2026-01-02 00:00:00')`)
	f.exec(`INSERT INTO watch_state (id, user_id, title_id, watched, updated_at) VALUES ('w2','u1','s01-e1',1,'2026-10-02 00:00:00')`)

	v := decodeRows(t, f.home("u1", all))
	d := countKind(v, "discovery")
	if d < 2 {
		t.Fatalf("fixture yields %d Discovery rows, want >= 2 to exercise the pattern: %v", d, v.kinds())
	}
	if want := wantPattern(true, true, true, true, d); !reflect.DeepEqual(v.kinds(), want) {
		t.Errorf("row kinds = %v, want %v", v.kinds(), want)
	}
	for _, r := range v.Rows {
		if r.Kind == "myList" {
			if len(r.Items) != 2 || r.Items[0].ID != "m05" || r.Items[1].ID != "m02" {
				t.Errorf("My List items = %+v, want m05, m02 in Watchlist order", r.Items)
			}
		}
	}
}

func myListIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var got []string
	for _, r := range decodeRows(t, body).Rows {
		if r.Kind == "myList" {
			for _, it := range r.Items {
				got = append(got, it.ID)
			}
		}
	}
	return got
}

func TestHomeRowsMyListListsEachTitleOnce(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	all := access.AllAccess()
	f.watchlist("u1", all, "m05", "m02", "m05", "m07")
	if got, want := myListIDs(t, f.home("u1", all)), []string{"m05", "m02", "m07"}; !reflect.DeepEqual(got, want) {
		t.Errorf("My List = %v, want %v", got, want)
	}
}

func TestHomeRowsMyListCapAppliesAfterDedupe(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	all := access.AllAccess()
	// Ten more Movies make 22 distinct in the Library.
	for i := 13; i <= 22; i++ {
		id := fmt.Sprintf("m%02d", i)
		f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year, content_rating, added_at)
		        VALUES (?,'mA','movie',?,?,?,1985,'PG','2026-09-20 10:00:00')`, id, "Movie "+id, id, id)
	}
	// 25 entries, 20 distinct: m01..m05 twice, then m06..m20 once.
	var ids []string
	for i := 1; i <= 5; i++ {
		ids = append(ids, fmt.Sprintf("m%02d", i), fmt.Sprintf("m%02d", i))
	}
	for i := 6; i <= 20; i++ {
		ids = append(ids, fmt.Sprintf("m%02d", i))
	}
	f.watchlist("u1", all, ids...)
	got := myListIDs(t, f.home("u1", all))
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Errorf("My List repeats %s: %v", id, got)
		}
		seen[id] = true
	}
	if len(got) != homeRowLimit {
		t.Errorf("My List has %d Titles, want %d distinct: %v", len(got), homeRowLimit, got)
	}
}

func TestHomeRowsEmptyMyListCollapses(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	v := decodeRows(t, f.home("u2", access.AllAccess()))
	d := countKind(v, "discovery")
	if d < 2 {
		t.Fatalf("fixture yields %d Discovery rows, want >= 2: %v", d, v.kinds())
	}
	if countKind(v, "myList") != 0 {
		t.Errorf("an empty My List must be omitted: %v", v.kinds())
	}
	want := wantPattern(false, false, false, true, d)
	if !reflect.DeepEqual(v.kinds(), want) {
		t.Errorf("row kinds = %v, want %v", v.kinds(), want)
	}
	// Recently Added sits right after the second Discovery row.
	if v.kinds()[2] != "recentlyAdded" {
		t.Errorf("Recently Added is not directly after Discovery[1]: %v", v.kinds())
	}
}

func TestHomeRowsMyListNeverShowsAboveTheRatingCeiling(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	// m11 is rated R; it is on the Watchlist (added with full access) and a PG
	// ceiling now applies.
	f.watchlist("u1", access.AllAccess(), "m05", "m11", "m02")
	pg := access.Scope{AllLibraries: true, RatingCeiling: 2}
	v := decodeRows(t, f.home("u1", pg))
	var got []string
	for _, r := range v.Rows {
		for _, it := range r.Items {
			if it.ID == "m11" {
				t.Errorf("row %q carries m11, rated R above the PG ceiling", r.Kind)
			}
		}
		if r.Kind == "myList" {
			for _, it := range r.Items {
				got = append(got, it.ID)
			}
		}
	}
	if want := []string{"m05", "m02"}; !reflect.DeepEqual(got, want) {
		t.Errorf("My List = %v, want %v", got, want)
	}
}

func TestHomeRowsMyListEpisodeCarriesItsParentContext(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	all := access.AllAccess()
	f.watchlist("u1", all, "s01-e1") // a Playlist holds one media kind
	var v struct {
		Rows []struct {
			Kind  string `json:"kind"`
			Items []struct {
				ID      string `json:"id"`
				Episode *struct {
					ShowTitle     string `json:"showTitle"`
					SeasonNumber  int    `json:"seasonNumber"`
					EpisodeNumber int    `json:"episodeNumber"`
				} `json:"episode"`
			} `json:"items"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(f.home("u1", all), &v); err != nil {
		t.Fatal(err)
	}
	for _, r := range v.Rows {
		if r.Kind != "myList" {
			continue
		}
		if len(r.Items) != 1 || r.Items[0].ID != "s01-e1" {
			t.Fatalf("My List items = %+v, want s01-e1", r.Items)
		}
		ep := r.Items[0].Episode
		if ep == nil || ep.ShowTitle != "Show s01" || ep.SeasonNumber != 1 || ep.EpisodeNumber != 1 {
			t.Errorf("Episode context = %+v, want Show s01 S1E1", ep)
		}
		return
	}
	t.Fatal("no myList row")
}

func TestHomeRowsDiscoveryRowShape(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	v := decodeRows(t, f.home("u1", access.AllAccess()))
	if countKind(v, "discovery") == 0 {
		t.Fatalf("fixture yields no Discovery rows: %v", v.kinds())
	}
	for _, r := range v.Rows {
		if r.Kind != "discovery" {
			if r.Source != nil || r.Total != nil || r.MediaKind != "" {
				t.Errorf("%s row carries discovery-only fields: %+v", r.Kind, r)
			}
			continue
		}
		if r.Source == nil || r.Total == nil || r.Title == "" || len(r.Items) == 0 || len(r.Source.LibraryIDs) == 0 {
			t.Errorf("discovery row is missing source/total/title/items: %+v", r)
			continue
		}
		switch r.Source.Type {
		case "genre", "studio", "network", "decade", "person":
		default:
			t.Errorf("source.type = %q, want a filter[...] param name", r.Source.Type)
		}
		if r.MediaKind != "movie" && r.MediaKind != "tv" {
			t.Errorf("mediaKind = %q", r.MediaKind)
		}
	}
}

func TestHomeRowsTitles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		typ, value, display, kind, want string
	}{
		{"genre", "Comedy", "Comedy", "movie", "Comedy Movies"},
		{"genre", "Drama", "Drama", "tv", "Drama Shows"},
		{"studio", "A24", "A24", "movie", "From A24"},
		{"network", "HBO", "HBO", "tv", "On HBO"},
		{"decade", "1980", "1980", "movie", "80s Movies"},
		{"decade", "2010", "2010", "tv", "2010s Shows"},
		{"decade", "1900", "1900", "movie", "1900s Movies"},
		{"decade", "1910", "1910", "movie", "1910s Movies"},
		{"decade", "1920", "1920", "movie", "20s Movies"},
		{"decade", "1990", "1990", "tv", "90s Shows"},
		{"decade", "2000", "2000", "movie", "2000s Movies"},
		{"person", "tmdb:31", "Tom Hanks", "movie", "Starring Tom Hanks"},
	}
	for _, c := range cases {
		if got := discoveryRowTitle(c.typ, c.value, c.display, c.kind); got != c.want {
			t.Errorf("discoveryRowTitle(%s, %s, %s) = %q, want %q", c.typ, c.value, c.kind, got, c.want)
		}
	}
}

var itemIDPattern = regexp.MustCompile(`"itemId":"[0-9a-f-]{36}"`)

const homeRowsGolden = "testdata/home_rows.golden.json"

// TestHomeRowsGolden pins the whole /home payload, rows included, for a seeded
// store and a fixed clock. Regenerate with: go test ./internal/api -run HomeRowsGolden -update
func TestHomeRowsGolden(t *testing.T) {
	t.Parallel()
	f := newHomeRowsFixture(t)
	all := access.AllAccess()
	f.watchlist("u1", all, "m05", "m02")
	f.exec(`INSERT INTO watch_state (id, user_id, title_id, resume_position_ms, updated_at) VALUES ('w1','u1','m07',60000,'2026-10-01 00:00:00')`)

	// A Watchlist item id is a generated uuid; everything else is fixed.
	body := itemIDPattern.ReplaceAll(f.home("u1", all), []byte(`"itemId":"<uuid>"`))
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	if *updateGolden {
		if err := os.WriteFile(homeRowsGolden, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(homeRowsGolden)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Errorf("/home payload differs from %s (regenerate with -update if intended):\n%s", homeRowsGolden, pretty.String())
	}
}
