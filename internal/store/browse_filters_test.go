package store_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Library browse filters: studio, network, decade and person, ANDed with each
// other and with genre.

var allAccess = store.AccessFilter{AllLibraries: true}

func seedBrowseTitle(t *testing.T, db *store.DB, id string, year int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year)
	                 VALUES (?, 'lib', 'movie', ?, ?, ?, ?)`, id, id, id, id, year)
}

func listTitleIDs(t *testing.T, db *store.DB, f store.BrowseFilter) []string {
	t.Helper()
	page, err := db.ListTitles("lib", store.SortByTitle, nil, 100, f, allAccess)
	if err != nil {
		t.Fatalf("ListTitles: %v", err)
	}
	return titleIDs(page.Titles)
}

func browseMovieDB(t *testing.T) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	return db
}

func TestStudioFilterMatchesAnyOfAMovieStudios(t *testing.T) {
	db := browseMovieDB(t)
	seedBrowseTitle(t, db, "a", 2000)
	seedBrowseTitle(t, db, "b", 2001)
	mustExec(t, db, `INSERT INTO title_studios (title_id, studio, ord) VALUES ('a','Legendary',0),('a','Warner',1),('b','A24',0)`)

	if got := listTitleIDs(t, db, store.BrowseFilter{Studio: "Warner"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("studio=Warner (second studio) = %v, want [a]", got)
	}
	if got := listTitleIDs(t, db, store.BrowseFilter{Studio: "A24"}); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("studio=A24 = %v, want [b]", got)
	}
}

func TestNetworkFilterOnAMovieLibraryMatchesNothing(t *testing.T) {
	db := browseMovieDB(t)
	seedBrowseTitle(t, db, "a", 2000)
	if got := listTitleIDs(t, db, store.BrowseFilter{Network: "HBO"}); len(got) != 0 {
		t.Errorf("network on movie library = %v, want empty", got)
	}
}

func TestDecadeFilterSpansTenYears(t *testing.T) {
	db := browseMovieDB(t)
	seedBrowseTitle(t, db, "y1979", 1979)
	seedBrowseTitle(t, db, "y1980", 1980)
	seedBrowseTitle(t, db, "y1989", 1989)
	seedBrowseTitle(t, db, "y1990", 1990)
	if got := listTitleIDs(t, db, store.BrowseFilter{Decade: 1980}); !reflect.DeepEqual(got, []string{"y1980", "y1989"}) {
		t.Errorf("decade=1980 = %v, want [y1980 y1989]", got)
	}
}

func TestGenreAndDecadeIntersect(t *testing.T) {
	db := browseMovieDB(t)
	seedBrowseTitle(t, db, "old-comedy", 1985)
	seedBrowseTitle(t, db, "new-comedy", 2015)
	seedBrowseTitle(t, db, "old-drama", 1986)
	mustExec(t, db, `INSERT INTO title_genres (title_id, genre) VALUES ('old-comedy','Comedy'),('new-comedy','Comedy'),('old-drama','Drama')`)
	got := listTitleIDs(t, db, store.BrowseFilter{Genre: "Comedy", Decade: 1980})
	if !reflect.DeepEqual(got, []string{"old-comedy"}) {
		t.Errorf("genre=Comedy+decade=1980 = %v, want [old-comedy]", got)
	}
}

func TestPersonFilterOnlyCountsTheTopFiveBilled(t *testing.T) {
	db := browseMovieDB(t)
	seedBrowseTitle(t, db, "fifth", 2000)
	seedBrowseTitle(t, db, "sixth", 2000)
	seedBrowseTitle(t, db, "crew", 2000)
	seedBrowseTitle(t, db, "noref", 2000)
	for _, id := range []string{"fifth", "sixth"} {
		for i := 0; i < 5; i++ {
			ref := fmt.Sprintf("other-%d", i)
			mustExec(t, db, `INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?, 'x', ?, 'cast', ?)`, id, ref, i)
		}
	}
	// Billed 5th (ord 4) vs 6th (ord 5), with gaps in ord handled by rank not value.
	mustExec(t, db, `UPDATE title_credits SET person_ref = 'p1' WHERE title_id = 'fifth' AND ord = 4`)
	mustExec(t, db, `INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES ('sixth', 'x', 'p1', 'cast', 5)`)
	mustExec(t, db, `INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES ('crew', 'x', 'p1', 'crew', 0)`)
	mustExec(t, db, `INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES ('noref', 'x', '', 'cast', 0)`)

	if got := listTitleIDs(t, db, store.BrowseFilter{Person: "p1"}); !reflect.DeepEqual(got, []string{"fifth"}) {
		t.Errorf("person=p1 = %v, want [fifth] (6th-billed and crew excluded)", got)
	}
	if got := listTitleIDs(t, db, store.BrowseFilter{Person: ""}); len(got) != 4 {
		t.Errorf("empty person = %v, want no filtering", got)
	}
}

func TestFilteredPaginationReturnsEveryMatchOnce(t *testing.T) {
	db := browseMovieDB(t)
	var want []string
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("t%02d", i)
		seedBrowseTitle(t, db, id, 1980+i)
		mustExec(t, db, `INSERT INTO title_studios (title_id, studio, ord) VALUES (?, 'S', 0)`, id)
		want = append(want, id)
	}
	seedBrowseTitle(t, db, "other", 1985)
	seedBrowseTitle(t, db, "later", 1995)

	var got []string
	var cursor *store.TitleCursor
	for {
		page, err := db.ListTitles("lib", store.SortByTitle, cursor, 3, store.BrowseFilter{Studio: "S", Decade: 1980}, allAccess)
		if err != nil {
			t.Fatalf("ListTitles: %v", err)
		}
		got = append(got, titleIDs(page.Titles)...)
		if !page.HasMore {
			break
		}
		last := page.Titles[len(page.Titles)-1]
		cursor = &store.TitleCursor{SortKey: last.SortTitle, ID: last.ID}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged = %v, want %v", got, want)
	}
}

func seedBrowseShow(t *testing.T, db *store.DB, id string, year int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO shows (id, library_id, title, identity_key, sort_title, year) VALUES (?, 'tv', ?, ?, ?, ?)`, id, id, id, id, year)
}

func showIDs(t *testing.T, db *store.DB, f store.BrowseFilter) []string {
	t.Helper()
	page, err := db.ListShows("tv", nil, 100, f, allAccess)
	if err != nil {
		t.Fatalf("ListShows: %v", err)
	}
	var out []string
	for _, s := range page.Shows {
		out = append(out, s.ID)
	}
	return out
}

func TestShowFilters(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	seedBrowseShow(t, db, "bear", 2022)
	seedBrowseShow(t, db, "wire", 2002)
	mustExec(t, db, `INSERT INTO entity_networks (entity_type, entity_id, network, ord) VALUES ('show','bear','FX',0),('show','bear','Hulu',1),('show','wire','HBO',0)`)
	mustExec(t, db, `INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('show','bear','Drama'),('show','wire','Drama')`)
	for i := 0; i < 6; i++ {
		mustExec(t, db, `INSERT INTO entity_credits (id, entity_type, entity_id, person_ref, person, kind, ord) VALUES (?, 'show', 'bear', ?, 'x', 'cast', ?)`,
			fmt.Sprintf("c%d", i), fmt.Sprintf("p%d", i), i)
	}
	// An Episode guest with the same ref must not make the person match a Show.
	mustExec(t, db, `INSERT INTO entity_credits (id, entity_type, entity_id, person_ref, person, kind, ord) VALUES ('g', 'episode', 'bear', 'p5', 'x', 'cast', 0)`)

	cases := []struct {
		name string
		f    store.BrowseFilter
		want []string
	}{
		{"second network", store.BrowseFilter{Network: "Hulu"}, []string{"bear"}},
		{"other network", store.BrowseFilter{Network: "HBO"}, []string{"wire"}},
		{"decade", store.BrowseFilter{Decade: 2000}, []string{"wire"}},
		{"genre and decade", store.BrowseFilter{Genre: "Drama", Decade: 2020}, []string{"bear"}},
		{"person billed 5th", store.BrowseFilter{Person: "p4"}, []string{"bear"}},
		{"person billed 6th", store.BrowseFilter{Person: "p5"}, nil},
		{"studio on tv library", store.BrowseFilter{Studio: "A24"}, nil},
	}
	for _, c := range cases {
		if got := showIDs(t, db, c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
