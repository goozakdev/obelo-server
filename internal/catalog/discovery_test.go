package catalog_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Discovery row engine: per-User, per-day synthesized Home rows.

type discFix struct {
	t  *testing.T
	db *store.DB
	tx *sql.Tx
}

func newDiscFix(t *testing.T) *discFix {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "disc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &discFix{t: t, db: db}
	f.exec(`INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','h'),('u2','u2','member','h')`)
	f.exec(`INSERT INTO libraries (id, name, kind) VALUES ('mA','A','movie'),('mB','B','movie'),('tv','TV','tv'),('mu','Music','music')`)
	return f
}

func (f *discFix) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

type movie struct {
	id, lib, rating string
	year            int
	genres, studios []string
	cast            []string // person refs in billing order
}

func (f *discFix) movie(m movie) {
	f.t.Helper()
	if m.lib == "" {
		m.lib = "mA"
	}
	f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year, content_rating) VALUES (?,?,'movie',?,?,?,?,?)`,
		m.id, m.lib, m.id, m.id, m.id, m.year, m.rating)
	for _, g := range m.genres {
		f.exec(`INSERT INTO title_genres (title_id, genre) VALUES (?,?)`, m.id, g)
	}
	for i, s := range m.studios {
		f.exec(`INSERT INTO title_studios (title_id, studio, ord) VALUES (?,?,?)`, m.id, s, i)
	}
	for i, ref := range m.cast {
		f.exec(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?,?,?,'cast',?)`, m.id, "Name "+ref, ref, i)
	}
}

type show struct {
	id, rating string
	year       int
	genres     []string
	networks   []string
	cast       []string
	episodes   int
}

func (f *discFix) show(s show) {
	f.t.Helper()
	f.exec(`INSERT INTO shows (id, library_id, title, identity_key, sort_title, year) VALUES (?,'tv',?,?,?,?)`, s.id, s.id, s.id, s.id, s.year)
	if s.rating != "" {
		f.exec(`INSERT INTO entity_enrichment (entity_type, entity_id, content_rating) VALUES ('show',?,?)`, s.id, s.rating)
	}
	for _, g := range s.genres {
		f.exec(`INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('show',?,?)`, s.id, g)
	}
	for i, n := range s.networks {
		f.exec(`INSERT INTO entity_networks (entity_type, entity_id, network, ord) VALUES ('show',?,?,?)`, s.id, n, i)
	}
	for i, ref := range s.cast {
		f.exec(`INSERT INTO entity_credits (id, entity_type, entity_id, person_ref, person, kind, ord) VALUES (?,'show',?,?,?,'cast',?)`,
			fmt.Sprintf("%s-c%d", s.id, i), s.id, ref, "Name "+ref, i)
	}
	if s.episodes == 0 {
		s.episodes = 2
	}
	f.exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES (?,?,1,?)`, s.id+"-s1", s.id, s.id+"|s01")
	for e := 1; e <= s.episodes; e++ {
		f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number)
		        VALUES (?,'tv','episode',?,?,?,?,1,?)`, fmt.Sprintf("%s-e%d", s.id, e), "ep", s.id+fmt.Sprint(e), "ep", s.id+"-s1", e)
	}
}

func (f *discFix) watch(user string, titleIDs ...string) {
	f.t.Helper()
	for _, id := range titleIDs {
		f.exec(`INSERT INTO watch_state (id, user_id, title_id, watched) VALUES (?,?,?,1)`, user+id, user, id)
	}
}

// bigCatalog is a mixed catalog with many qualifying candidates across all five
// Row sources, so selection (not qualification) decides what a day shows.
func (f *discFix) bigCatalog() {
	genres := []string{"Comedy", "Drama", "Action", "Horror"}
	studios := []string{"A24", "Pixar", "Neon", "Focus", "Annapurna"}
	for i := 0; i < 60; i++ {
		f.movie(movie{
			id: fmt.Sprintf("m%02d", i), year: 1970 + 10*((i/3)%5) + i%10,
			genres: []string{genres[i%4]}, studios: []string{studios[i%5]},
			cast: []string{fmt.Sprintf("act%d", i%3)},
		})
	}
	for i := 0; i < 24; i++ {
		f.show(show{
			id: fmt.Sprintf("s%02d", i), year: 1980 + 10*(i/8) + i%10,
			genres: []string{genres[i%2]}, networks: []string{[]string{"HBO", "FX", "AMC"}[i%3]},
			cast: []string{fmt.Sprintf("tvact%d", i%2)},
		})
	}
}

func (f *discFix) rows(user string, scope access.Scope, day time.Time) []catalog.DiscoveryRow {
	f.t.Helper()
	rows, err := catalog.NewService(f.db, f.t.TempDir()).DiscoveryRows(scope, user, day, 20)
	if err != nil {
		f.t.Fatalf("DiscoveryRows: %v", err)
	}
	return rows
}

func day(d int) time.Time { return time.Date(2026, time.October, d, 9, 30, 0, 0, time.Local) }

func ids(r catalog.DiscoveryRow) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, it.ID)
	}
	return out
}

func TestDiscoverySameUserSameDayIsIdenticalAndNextDayDiffers(t *testing.T) {
	f := newDiscFix(t)
	f.bigCatalog()
	a := f.rows("u1", access.AllAccess(), day(13))
	if len(a) == 0 {
		t.Fatal("no rows for a catalog with dozens of qualifying candidates")
	}
	// Same date, later in the day, and a repeat call.
	b := f.rows("u1", access.AllAccess(), day(13).Add(10*time.Hour))
	if !reflect.DeepEqual(a, b) {
		t.Error("same user + same date produced different rows")
	}
	if c := f.rows("u1", access.AllAccess(), day(14)); reflect.DeepEqual(a, c) {
		t.Error("next date produced the identical selection and order")
	}
}

func TestDiscoveryNeverMoreThanEightRowsAndNoAdjacentSource(t *testing.T) {
	f := newDiscFix(t)
	f.bigCatalog()
	sawEight := false
	for d := 1; d <= 28; d++ {
		rows := f.rows("u1", access.AllAccess(), day(d))
		if len(rows) > 8 {
			t.Fatalf("day %d: %d rows, want <= 8", d, len(rows))
		}
		sawEight = sawEight || len(rows) == 8
		for i := 1; i < len(rows); i++ {
			if rows[i].Source == rows[i-1].Source {
				t.Fatalf("day %d: rows %d and %d share source %s", d, i-1, i, rows[i].Source)
			}
		}
	}
	if !sawEight {
		t.Error("never saw a full 8 rows with 27 qualifying candidates")
	}
}

func TestDiscoveryDropsRowsRatherThanBreakAdjacency(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 8; i++ {
		for _, g := range []string{"Comedy", "Drama", "Action"} {
			f.movie(movie{id: fmt.Sprintf("%s%d", g, i), genres: []string{g}})
		}
	}
	rows := f.rows("u1", access.AllAccess(), day(13))
	if len(rows) != 1 {
		t.Fatalf("three genre candidates and nothing else: %d rows, want 1", len(rows))
	}

	// One non-genre candidate allows G S G, but not a fourth row.
	for i := 0; i < 8; i++ {
		f.exec(`INSERT INTO title_studios (title_id, studio) VALUES (?, 'A24')`, fmt.Sprintf("Comedy%d", i))
	}
	for d := 1; d <= 10; d++ {
		rows = f.rows("u1", access.AllAccess(), day(d))
		var srcs []catalog.DiscoverySource
		for _, r := range rows {
			srcs = append(srcs, r.Source)
		}
		if len(rows) != 3 || srcs[0] != catalog.SourceGenre && srcs[1] != catalog.SourceGenre {
			t.Fatalf("day %d: sources %v, want 3 rows with no adjacent repeat", d, srcs)
		}
		if srcs[1] == srcs[0] || srcs[2] == srcs[1] {
			t.Fatalf("day %d: adjacent repeat in %v", d, srcs)
		}
	}
}

func TestDiscoveryNeedsEightVisibleTitlesPerUser(t *testing.T) {
	f := newDiscFix(t)
	// Rare (a genre) is split across two Libraries; Big (a studio) lives in mA. Different
	// Row sources, so adjacency does not hide one of them.
	for i := 0; i < 4; i++ {
		f.movie(movie{id: fmt.Sprintf("rareA%d", i), lib: "mA", genres: []string{"Rare"}})
		f.movie(movie{id: fmt.Sprintf("rareB%d", i), lib: "mB", genres: []string{"Rare"}})
	}
	for i := 0; i < 8; i++ {
		f.movie(movie{id: fmt.Sprintf("big%d", i), lib: "mA", studios: []string{"Big"}})
	}
	onlyA := access.Scope{LibraryIDs: []string{"mA"}}

	studios := func(rows []catalog.DiscoveryRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Value)
		}
		sort.Strings(out)
		return out
	}
	if got := studios(f.rows("u1", access.AllAccess(), day(13))); !reflect.DeepEqual(got, []string{"Big", "Rare"}) {
		t.Errorf("admin rows = %v, want [Big Rare]", got)
	}
	restricted := f.rows("u2", onlyA, day(13))
	if got := studios(restricted); !reflect.DeepEqual(got, []string{"Big"}) {
		t.Errorf("restricted rows = %v, want [Big] (Rare has only 4 visible)", got)
	}
	for _, r := range restricted {
		for _, it := range r.Items {
			if it.LibraryID != "mA" {
				t.Errorf("restricted user sees %s from library %s", it.ID, it.LibraryID)
			}
		}
	}
}

func TestDiscoveryHonoursTheRatingCeilingForMoviesAndShows(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 8; i++ {
		rating := ""
		if i < 3 {
			rating = "R"
		}
		f.movie(movie{id: fmt.Sprintf("mv%d", i), rating: rating, studios: []string{"S"}})
		r := ""
		if i < 3 {
			r = "TV-MA"
		}
		f.show(show{id: fmt.Sprintf("sh%d", i), rating: r, networks: []string{"N"}})
	}
	if rows := f.rows("u1", access.AllAccess(), day(13)); len(rows) != 2 {
		t.Fatalf("uncapped: %d rows, want 2", len(rows))
	}
	// 3 of 8 above a PG-13 ceiling leaves 5 visible: neither row qualifies.
	if rows := f.rows("u1", access.Scope{AllLibraries: true, RatingCeiling: 3}, day(13)); len(rows) != 0 {
		t.Errorf("capped: %d rows, want 0 (only 5 visible each)", len(rows))
	}
}

func TestDiscoveryExcludesHiddenTitles(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 8; i++ {
		f.movie(movie{id: fmt.Sprintf("mv%d", i), studios: []string{"S"}})
		f.show(show{id: fmt.Sprintf("sh%d", i), networks: []string{"N"}})
	}
	if rows := f.rows("u1", access.AllAccess(), day(13)); len(rows) != 2 {
		t.Fatalf("before hiding: %d rows, want 2", len(rows))
	}
	f.exec(`UPDATE titles SET hidden = 1 WHERE id = 'mv0'`)
	f.exec(`UPDATE shows SET hidden = 1 WHERE id = 'sh0'`)
	if rows := f.rows("u1", access.AllAccess(), day(13)); len(rows) != 0 {
		t.Errorf("%d rows, want 0 once a Missing Title/Show leaves 7 visible", len(rows))
	}
}

func TestDiscoveryUnwatchedPrecedeWatched(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 12; i++ {
		f.movie(movie{id: fmt.Sprintf("mv%02d", i), genres: []string{"Comedy"}})
	}
	f.watch("u1", "mv00", "mv03", "mv04", "mv07", "mv09")
	for d := 1; d <= 14; d++ {
		rows := f.rows("u1", access.AllAccess(), day(d))
		if len(rows) != 1 {
			t.Fatalf("%d rows, want 1", len(rows))
		}
		seenWatched := false
		for _, it := range rows[0].Items {
			if it.Watched {
				seenWatched = true
			} else if seenWatched {
				t.Fatalf("day %d: unwatched %s after a watched Title: %v", d, it.ID, ids(rows[0]))
			}
		}
		if !seenWatched {
			t.Fatal("no watched Title in the row")
		}
	}
	// Another User's watches do not reorder this User's row.
	for _, it := range f.rows("u2", access.AllAccess(), day(13))[0].Items {
		if it.Watched {
			t.Errorf("u2 has watched nothing, but %s is marked watched", it.ID)
		}
	}
}

func TestDiscoveryShowIsWatchedOnlyWhenEveryEpisodeIs(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 9; i++ {
		f.show(show{id: fmt.Sprintf("sh%d", i), genres: []string{"Comedy"}})
	}
	f.watch("u1", "sh0-e1", "sh0-e2", "sh1-e1")
	rows := f.rows("u1", access.AllAccess(), day(13))
	if len(rows) != 1 || rows[0].Kind != "tv" {
		t.Fatalf("rows = %+v, want one tv row", rows)
	}
	watched := map[string]bool{}
	for _, it := range rows[0].Items {
		watched[it.ID] = it.Watched
	}
	if !watched["sh0"] || watched["sh1"] || watched["sh2"] {
		t.Errorf("watched = %v, want only sh0 (sh1 has one of two Episodes watched)", watched)
	}
	if last := rows[0].Items[len(rows[0].Items)-1]; last.ID != "sh0" {
		t.Errorf("last = %s, want watched sh0 last", last.ID)
	}
}

func TestDiscoveryNeverListsMusicOrEpisodes(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 10; i++ {
		f.exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year) VALUES (?, 'mu', 'track', 't', ?, 't', 1985)`, fmt.Sprintf("tr%d", i), fmt.Sprintf("tr%d", i))
		f.exec(`INSERT INTO title_genres (title_id, genre) VALUES (?, 'Rock')`, fmt.Sprintf("tr%d", i))
		f.exec(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?, 'x', 'bandref', 'cast', 0)`, fmt.Sprintf("tr%d", i))
	}
	for i := 0; i < 10; i++ {
		f.show(show{id: fmt.Sprintf("sh%d", i), year: 1985})
		// Episode-level metadata (title_genres on an Episode, guest credits) must not leak.
		f.exec(`INSERT INTO title_genres (title_id, genre) VALUES (?, 'Sitcom')`, fmt.Sprintf("sh%d-e1", i))
		f.exec(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?, 'g', 'guestref', 'cast', 0)`, fmt.Sprintf("sh%d-e1", i))
	}
	// The ten Shows share a decade, so exactly one TV decade row is legitimate.
	rows := f.rows("u1", access.AllAccess(), day(13))
	if len(rows) != 1 || rows[0].Source != catalog.SourceDecade || rows[0].Value != "1980" || rows[0].Kind != "tv" {
		t.Fatalf("rows = %+v, want only the 1980 TV decade row", rows)
	}
	for _, it := range rows[0].Items {
		if it.Kind != "show" {
			t.Errorf("item %s is kind %q, want show", it.ID, it.Kind)
		}
	}
}

func TestDiscoveryActorBilledSixthNeverGetsARow(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 12; i++ {
		f.movie(movie{id: fmt.Sprintf("late%02d", i), cast: []string{
			fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i), fmt.Sprintf("c%d", i), fmt.Sprintf("d%d", i), fmt.Sprintf("e%d", i), "sixth"}})
	}
	if rows := f.rows("u1", access.AllAccess(), day(13)); len(rows) != 0 {
		t.Errorf("actor billed 6th everywhere got rows: %+v", rows)
	}
	for i := 0; i < 12; i++ {
		f.movie(movie{id: fmt.Sprintf("fifth%02d", i), cast: []string{
			fmt.Sprintf("a%d", i), fmt.Sprintf("b%d", i), fmt.Sprintf("c%d", i), fmt.Sprintf("d%d", i), "fifth"}})
	}
	rows := f.rows("u1", access.AllAccess(), day(13))
	if len(rows) != 1 || rows[0].Source != catalog.SourceActor || rows[0].Value != "fifth" || rows[0].DisplayValue != "Name fifth" || rows[0].Kind != "movie" {
		t.Errorf("rows = %+v, want one Actor row for the 5th-billed ref with its display name", rows)
	}
}

func TestDiscoveryRowDescriptorAndLimit(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 12; i++ {
		lib := "mA"
		if i%2 == 1 {
			lib = "mB"
		}
		f.movie(movie{id: fmt.Sprintf("mv%02d", i), lib: lib, studios: []string{"A24"}})
	}
	rows, err := catalog.NewService(f.db, t.TempDir()).DiscoveryRows(access.AllAccess(), "u1", day(13), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	r := rows[0]
	if r.Source != catalog.SourceStudio || r.Value != "A24" || r.DisplayValue != "A24" || r.Kind != "movie" ||
		!reflect.DeepEqual(r.LibraryIDs, []string{"mA", "mB"}) || r.Total != 12 || len(r.Items) != 10 {
		t.Errorf("descriptor = %+v", r)
	}
}

// seed10k fills a store with ~10k Titles: 5,000 Movies, 400 Shows x 12 Episodes.
func seed10k(tb testing.TB) *store.DB {
	db, err := store.Open(filepath.Join(tb.TempDir(), "big.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		tb.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	x := func(q string, a ...any) {
		if _, err := tx.Exec(q, a...); err != nil {
			tb.Fatalf("%q: %v", q, err)
		}
	}
	x(`INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','h')`)
	x(`INSERT INTO libraries (id, name, kind) VALUES ('mA','A','movie'),('tv','TV','tv')`)
	genres := []string{"Comedy", "Drama", "Action", "Horror", "SciFi", "Romance", "Thriller", "Family"}
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("m%05d", i)
		x(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year) VALUES (?,'mA','movie',?,?,?,?)`, id, id, id, id, 1950+i%75)
		x(`INSERT INTO title_genres (title_id, genre) VALUES (?,?)`, id, genres[i%8])
		x(`INSERT INTO title_studios (title_id, studio, ord) VALUES (?,?,0)`, id, fmt.Sprintf("studio%d", i%120))
		for c := 0; c < 8; c++ {
			x(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?,?,?,'cast',?)`, id, "p", fmt.Sprintf("ref%d", (i*7+c*131)%900), c)
		}
		if i%3 == 0 {
			x(`INSERT INTO watch_state (id, user_id, title_id, watched) VALUES (?,'u1',?,1)`, "w"+id, id)
		}
	}
	for i := 0; i < 400; i++ {
		sid := fmt.Sprintf("s%04d", i)
		x(`INSERT INTO shows (id, library_id, title, identity_key, sort_title, year) VALUES (?,'tv',?,?,?,?)`, sid, sid, sid, sid, 1960+i%60)
		x(`INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('show',?,?)`, sid, genres[i%8])
		x(`INSERT INTO entity_networks (entity_type, entity_id, network, ord) VALUES ('show',?,?,0)`, sid, fmt.Sprintf("net%d", i%20))
		for c := 0; c < 8; c++ {
			x(`INSERT INTO entity_credits (id, entity_type, entity_id, person_ref, person, kind, ord) VALUES (?,'show',?,?,'p','cast',?)`,
				fmt.Sprintf("%s-c%d", sid, c), sid, fmt.Sprintf("tref%d", (i*5+c*37)%150), c)
		}
		x(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES (?,?,1,?)`, sid+"s", sid, sid+"|1")
		for e := 0; e < 12; e++ {
			eid := fmt.Sprintf("%se%d", sid, e)
			x(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number) VALUES (?,'tv','episode','e',?,'e',?,1,?)`, eid, eid, sid+"s", e)
			if i%4 == 0 {
				x(`INSERT INTO watch_state (id, user_id, title_id, watched) VALUES (?,'u1',?,1)`, "w"+eid, eid)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return db
}

func BenchmarkDiscoveryRows10k(b *testing.B) {
	svc := catalog.NewService(seed10k(b), b.TempDir())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.DiscoveryRows(access.AllAccess(), "u1", day(1+i%28), 20); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDiscoveryRowsStayCheapOnA10kStore(t *testing.T) {
	svc := catalog.NewService(seed10k(t), t.TempDir())
	var worst time.Duration
	for d := 1; d <= 5; d++ {
		start := time.Now()
		rows, err := svc.DiscoveryRows(access.AllAccess(), "u1", day(d), 20)
		if err != nil {
			t.Fatal(err)
		}
		if el := time.Since(start); el > worst {
			worst = el
		}
		if len(rows) == 0 {
			t.Fatal("no rows on the 10k store")
		}
	}
	t.Logf("DiscoveryRows on 5,000 Movies + 400 Shows (4,800 Episodes), worst of 5 calls: %v", worst)
	if worst > 500*time.Millisecond {
		t.Errorf("worst call %v exceeds the (loaded-CI-tolerant) 500ms bound; the target is 150ms", worst)
	}
}

// The Row-source cap in selection must keep a full 8 rows when arrangement would
// otherwise have to trim a genre-heavy pick.
func TestDiscoveryFillsEightRowsFromGenreHeavyMix(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 80; i++ {
		f.movie(movie{id: fmt.Sprintf("m%02d", i), year: 1950 + 10*(i%4) + 1, genres: []string{fmt.Sprintf("G%d", i%10)}})
	}
	for d := 1; d <= 28; d++ {
		if rows := f.rows("u1", access.AllAccess(), day(d)); len(rows) != 8 {
			t.Fatalf("day %d: %d rows, want 8 (10 genre + 4 decade candidates allow 4+4)", d, len(rows))
		}
	}
}

// The seed date is the Server-local day whatever zone the caller's time carries.
func TestDiscoveryUsesServerLocalDayForAnyCallerZone(t *testing.T) {
	prev := time.Local
	time.Local = time.FixedZone("test+13", 13*3600)
	t.Cleanup(func() { time.Local = prev })

	f := newDiscFix(t)
	f.bigCatalog()
	// 05:00 on the 14th Server-local is 16:00 on the 13th UTC: the dates differ.
	local := time.Date(2026, time.October, 14, 5, 0, 0, 0, time.Local)
	a := f.rows("u1", access.AllAccess(), local)
	b := f.rows("u1", access.AllAccess(), local.UTC())
	if !reflect.DeepEqual(a, b) {
		t.Error("the same instant in Local and UTC produced different rows")
	}
}

// An actor row's label is the lexically smallest name seen for the ref, not
// whichever Title the store happened to return first.
func TestDiscoveryActorDisplayNameIsSmallestNameSeen(t *testing.T) {
	f := newDiscFix(t)
	for i := 0; i < 8; i++ {
		f.movie(movie{id: fmt.Sprintf("a%d", i), cast: []string{"p1"}})
	}
	f.exec(`UPDATE title_credits SET person = 'Zed' WHERE title_id = 'a0'`)
	rows := f.rows("u1", access.AllAccess(), day(13))
	if len(rows) != 1 || rows[0].Source != catalog.SourceActor {
		t.Fatalf("rows = %+v, want one actor row", rows)
	}
	if got := rows[0].DisplayValue; got != "Name p1" {
		t.Errorf("DisplayValue = %q, want %q", got, "Name p1")
	}
}
