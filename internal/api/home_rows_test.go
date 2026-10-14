package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Each Discovery row's source + libraryIds must be a working issue-02 browse
// request (home-discovery-rows 04): the client builds "See all" from them alone.

type homeDiscoveryResp struct {
	Rows []struct {
		Kind      string `json:"kind"`
		MediaKind string `json:"mediaKind"`
		Total     int    `json:"total"`
		Items     []struct {
			ID string `json:"id"`
		} `json:"items"`
		Source *struct {
			Type       string   `json:"type"`
			Value      string   `json:"value"`
			LibraryIDs []string `json:"libraryIds"`
		} `json:"source"`
	} `json:"rows"`
}

func seedDiscoveryCatalog(srv *testharness.Server) {
	srv.Exec(`INSERT INTO libraries (id, name, kind) VALUES ('mA','Movies A','movie'),('mB','Movies B','movie'),('tv','Shows','tv')`)
	for i := 1; i <= 10; i++ {
		lib := "mA"
		if i%2 == 0 {
			lib = "mB"
		}
		id := fmt.Sprintf("m%02d", i)
		srv.Exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year) VALUES (?,?, 'movie',?,?,?,?)`,
			id, lib, id, id, id, 1980+i%10)
		srv.Exec(`INSERT INTO title_genres (title_id, genre) VALUES (?,'Sci-Fi & Fantasy')`, id)
		srv.Exec(`INSERT INTO title_studios (title_id, studio, ord) VALUES (?,'Studio Ghibli',0)`, id)
		srv.Exec(`INSERT INTO title_credits (title_id, person, person_ref, kind, ord) VALUES (?,'Tom Hanks','tmdb:31','cast',0)`, id)
	}
	for i := 1; i <= 10; i++ {
		id := fmt.Sprintf("s%02d", i)
		srv.Exec(`INSERT INTO shows (id, library_id, title, identity_key, sort_title, year) VALUES (?,'tv',?,?,?,?)`, id, id, id, id, 1990+i%10)
		srv.Exec(`INSERT INTO entity_genres (entity_type, entity_id, genre) VALUES ('show',?,'Drama')`, id)
		srv.Exec(`INSERT INTO entity_networks (entity_type, entity_id, network, ord) VALUES ('show',?,'HBO Max',0)`, id)
		srv.Exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES (?,?,1,?)`, id+"-s1", id, id+"|s01")
		srv.Exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, season_id, season_number, episode_number)
		          VALUES (?,'tv','episode','ep',?,'ep',?,1,1)`, id+"-e1", id+"e1", id+"-s1")
	}
}

func TestHomeDiscoveryRowSourceIsAValidBrowseRequest(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	seedDiscoveryCatalog(srv)

	var home homeDiscoveryResp
	if status, body := srv.AuthGET("/api/v1/home", token, &home); status != http.StatusOK {
		t.Fatalf("home = %d; body: %s", status, body)
	}
	checked := 0
	for _, row := range home.Rows {
		if row.Kind != "discovery" {
			continue
		}
		total := 0
		for _, lib := range row.Source.LibraryIDs {
			path := fmt.Sprintf("/api/v1/libraries/%s/titles?limit=100&filter[%s]=%s",
				lib, row.Source.Type, url.QueryEscape(row.Source.Value))
			var page struct {
				Titles []struct{} `json:"titles"`
				Shows  []struct{} `json:"shows"`
			}
			if status, body := srv.AuthGET(path, token, &page); status != http.StatusOK {
				t.Errorf("GET %s = %d; body: %s", path, status, body)
				continue
			}
			total += len(page.Titles) + len(page.Shows)
		}
		// The row says how many Titles matched; browsing every listed Library finds
		// exactly those (no kind of Library is left out, none invented).
		if total != row.Total {
			t.Errorf("%s=%s: browse found %d, row total %d", row.Source.Type, row.Source.Value, total, row.Total)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("no Discovery rows to check: %+v", home.Rows)
	}
}
