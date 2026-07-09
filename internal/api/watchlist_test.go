package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box HTTP tests for the Watchlist surface (watchlist 01): the per-User system
// Playlist, addressed by name. It is guaranteed to exist (lazily seeded on first
// touch), shows up as an ordinary owner-private Playlist, and — unlike an ordinary
// Playlist — refuses rename/delete. Append reuses the single-kind Playlist rule.

type watchlistDetailResp struct {
	ID          string               `json:"id"`
	System      string               `json:"system"`
	Kind        string               `json:"kind"`
	Name        string               `json:"name"`
	MemberCount int                  `json:"memberCount"`
	Members     []playlistMemberResp `json:"members"`
}

// TestWatchlistExistsAndImmutable: GET /watchlist lazily creates the caller's system
// Watchlist and returns it (system "watchlist", named "Watchlist"); it then appears
// in the ordinary /playlists list; a repeat GET is idempotent (same id); and the
// Watchlist refuses rename and delete with 422 SYSTEM_PLAYLIST.
func TestWatchlistExistsAndImmutable(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)

	// A brand-new caller (created after the back-fill migration) has no Watchlist
	// row yet; GET /watchlist seeds and returns it.
	var wl watchlistDetailResp
	if st, body := srv.AuthGET("/api/v1/watchlist", admin, &wl); st != http.StatusOK {
		t.Fatalf("GET /watchlist: status %d; body: %s", st, body)
	}
	if wl.ID == "" || wl.System != "watchlist" || wl.Name != "Watchlist" {
		t.Fatalf("watchlist = %+v, want a real id, system=watchlist, name=Watchlist", wl)
	}
	if wl.MemberCount != 0 || len(wl.Members) != 0 {
		t.Errorf("fresh watchlist has %d members, want 0", wl.MemberCount)
	}

	// It is a real owner-private Playlist: it now shows in GET /playlists.
	var list playlistsListResp
	srv.AuthGET("/api/v1/playlists", admin, &list)
	found := false
	for _, p := range list.Playlists {
		if p.ID == wl.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("watchlist %s not in /playlists list %+v", wl.ID, list.Playlists)
	}

	// Idempotent: a second GET returns the SAME Watchlist (no duplicate seeded).
	var wl2 watchlistDetailResp
	srv.AuthGET("/api/v1/watchlist", admin, &wl2)
	if wl2.ID != wl.ID {
		t.Errorf("second GET /watchlist id = %s, want same as first %s", wl2.ID, wl.ID)
	}

	// Rename is refused: the Watchlist is the system's, not the User's.
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/playlists/"+wl.ID, admin,
		map[string]any{"name": "Mine now"}, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "SYSTEM_PLAYLIST" {
		t.Errorf("rename watchlist = %d/%s, want 422 SYSTEM_PLAYLIST", st, env.Error.Code)
	}
	// Delete is refused for the same reason.
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodDelete, "/api/v1/playlists/"+wl.ID, admin, nil, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "SYSTEM_PLAYLIST" {
		t.Errorf("delete watchlist = %d/%s, want 422 SYSTEM_PLAYLIST", st, env.Error.Code)
	}
}

// TestWatchlistAppendSingleKind: POST /watchlist/items appends a Title (204) which
// then appears in GET /watchlist; a cross-kind append (an Episode after the movie
// fixed the kind) is a clean 422 KIND_MISMATCH; and DELETE /watchlist/items/{itemId}
// removes an entry.
func TestWatchlistAppendSingleKind(t *testing.T) {
	requireFixtures(t)
	srv, admin, movieID, episodeID := scanMovieAndEpisode(t)

	// Append a movie → 204.
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/watchlist/items", admin,
		map[string]any{"titleId": movieID}, &env); st != http.StatusNoContent {
		t.Fatalf("append movie to watchlist = %d/%s, want 204", st, env.Error.Code)
	}

	// It shows up in GET /watchlist, and the Watchlist is now typed "movie".
	var wl watchlistDetailResp
	srv.AuthGET("/api/v1/watchlist", admin, &wl)
	if len(wl.Members) != 1 || wl.Members[0].ID != movieID {
		t.Fatalf("watchlist members = %+v, want [%s]", wl.Members, movieID)
	}
	if wl.Kind != "movie" {
		t.Errorf("watchlist kind = %q, want movie", wl.Kind)
	}

	// A cross-kind append (Episode → "tv") is refused, kind unchanged.
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPost, "/api/v1/watchlist/items", admin,
		map[string]any{"titleId": episodeID}, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "KIND_MISMATCH" {
		t.Errorf("cross-kind append = %d/%s, want 422 KIND_MISMATCH", st, env.Error.Code)
	}

	// DELETE by item id removes the entry.
	itemID := wl.Members[0].ItemID
	if st, body := srv.JSON(http.MethodDelete, "/api/v1/watchlist/items/"+itemID, admin, nil, nil); st != http.StatusNoContent {
		t.Fatalf("delete watchlist item: status %d; body: %s", st, body)
	}
	srv.AuthGET("/api/v1/watchlist", admin, &wl)
	if len(wl.Members) != 0 {
		t.Errorf("after delete, watchlist has %d members, want 0", len(wl.Members))
	}
}
