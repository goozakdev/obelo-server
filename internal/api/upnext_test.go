package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box integration tests for issue tv-music/02: the TV-only Up Next Home
// row and the parent-context decoration on Continue Watching / Recently Added.
// All driven through the HTTP API against a scanned TV Library (the same `tv`
// fixtures issue 01 uses). The fixtures are mkv/h264/aac, which mp4Profile()
// direct-plays, so a real session + progress report exercises the auto Watched-
// threshold path; the manual PUT watchState path is exercised directly.

// --- wire shapes (Up Next + parent context additions) -----------------------

type upNextEpisodeCtx struct {
	ShowID        string `json:"showId"`
	ShowTitle     string `json:"showTitle"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	EpisodeLabel  string `json:"episodeLabel"`
}

type upNextHomeTitle struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	Title            string            `json:"title"`
	ResumePositionMs int64             `json:"resumePositionMs"`
	Episode          *upNextEpisodeCtx `json:"episode"`
}

type upNextHomeResp struct {
	ContinueWatching []upNextHomeTitle `json:"continueWatching"`
	UpNext           []upNextHomeTitle `json:"upNext"`
	RecentlyAdded    []upNextHomeTitle `json:"recentlyAdded"`
}

func getHome(t *testing.T, srv *testharness.Server, token string) upNextHomeResp {
	t.Helper()
	var home upNextHomeResp
	if status, body := srv.AuthGET("/api/v1/home", token, &home); status != http.StatusOK {
		t.Fatalf("home status = %d, want 200; body: %s", status, body)
	}
	return home
}

// upNextFor returns the single Up Next entry for the given Show id, or nil.
func upNextFor(home upNextHomeResp, showID string) *upNextHomeTitle {
	for i := range home.UpNext {
		if home.UpNext[i].Episode != nil && home.UpNext[i].Episode.ShowID == showID {
			return &home.UpNext[i]
		}
	}
	return nil
}

// negotiateEpisode negotiates direct play for an Episode (mkv/h264/aac fixture)
// and returns the decision (session id). Reuses mp4Profile, whose container list
// includes mkv and whose codec list includes h264.
func negotiateEpisode(t *testing.T, srv *testharness.Server, token, titleID string) decisionResp {
	t.Helper()
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+titleID+"/playback", token, mp4Profile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("episode playback status = %d, want 200; body: %s", status, body)
	}
	return dec
}

// bearSeason1Episodes scans the TV fixtures and returns The Bear's Season 1
// episodes (System=E01, Hands=E02) plus its Show id.
func bearSeason1Episodes(t *testing.T, srv *testharness.Server, token, libID string) (showID string, eps episodesListResp) {
	t.Helper()
	shows := listShows(t, srv, token, libID)
	showID = findShow(t, shows, "The Bear")
	seasons := showSeasons(t, srv, token, showID)
	var s1 seasonResp
	for _, s := range seasons.Seasons {
		if s.SeasonNumber == 1 {
			s1 = s
		}
	}
	if s1.ID == "" {
		t.Fatalf("The Bear Season 01 not found; seasons: %+v", seasons.Seasons)
	}
	eps = seasonEpisodes(t, srv, token, s1.ID)
	if len(eps.Episodes) != 2 {
		t.Fatalf("The Bear Season 01 episodes = %d, want 2", len(eps.Episodes))
	}
	return showID, eps
}

// TestUpNextStartsOnInProgress: a Show with an in-progress (mid-band) Episode is
// "started"; Up Next surfaces that same not-yet-watched Episode (E01) with its
// Show/episode parent context.
func TestUpNextStartsOnInProgress(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Before any watch state, the Show is NOT started → no Up Next.
	if got := upNextFor(getHome(t, srv, token), showID); got != nil {
		t.Fatalf("Up Next present before the Show is started: %+v", got)
	}

	// Report mid-band progress on E01 → it lands in Continue Watching and the Show
	// is now started. Up Next is the next UNWATCHED episode in order = E01 itself
	// (it is in progress, not watched).
	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)

	home := getHome(t, srv, token)
	un := upNextFor(home, showID)
	if un == nil {
		t.Fatalf("Up Next missing after starting the Show; upNext: %+v", home.UpNext)
	}
	if un.ID != e1.ID {
		t.Errorf("Up Next = %q, want E01 (%q); next unwatched in order", un.Title, e1.Title)
	}
	if un.Episode == nil || un.Episode.ShowTitle != "The Bear" || un.Episode.SeasonNumber != 1 || un.Episode.EpisodeNumber != 1 {
		t.Errorf("Up Next parent context = %+v, want The Bear S01E01", un.Episode)
	}
	_ = e2
}

// TestUpNextAdvancesOnThresholdCrossing: crossing the ~90% Watched threshold on
// E01 (the auto path) advances the Show's Up Next to E02.
func TestUpNextAdvancesOnThresholdCrossing(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	// Mid first so the Show is started and Up Next = E01, then cross 90%.
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)
	if un := upNextFor(getHome(t, srv, token), showID); un == nil || un.ID != e1.ID {
		t.Fatalf("Up Next before crossing = %+v, want E01", un)
	}

	out := postProgress(t, srv, token, dec.SessionID, dur-1, http.StatusOK) // ~100% ≥ 90%
	if !out.Watched {
		t.Fatalf("E01 watched = false after crossing 90%%, want true")
	}

	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next gone after watching E01; the Show still has an unwatched E02")
	}
	if un.ID != e2.ID {
		t.Errorf("Up Next = %q, want E02 (%q) after E01 crossed the threshold", un.Title, e2.Title)
	}
	if un.Episode == nil || un.Episode.EpisodeNumber != 2 {
		t.Errorf("advanced Up Next context = %+v, want S01E02", un.Episode)
	}
}

// TestUpNextManualToggleMovesConsistently: marking E01 watched manually advances
// Up Next to E02 (same as the auto path); marking it unwatched moves Up Next back
// to E01 — the manual path is consistent with the auto path.
func TestUpNextManualToggleMovesConsistently(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Manually mark E01 watched (no playback) → Show started, Up Next advances to E02.
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+e1.ID+"/watchState", token,
		map[string]any{"watched": true}, nil); status != http.StatusOK {
		t.Fatalf("watchState E01 status = %d; body: %s", status, body)
	}
	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil || un.ID != e2.ID {
		t.Fatalf("Up Next after manual-watch E01 = %+v, want E02 (%q)", un, e2.Title)
	}

	// Manually mark E02 watched too → both regular Season-1 episodes are watched.
	// The Bear also has a Specials S00E01, still unwatched. Up Next defers Specials
	// (Season 0) to the END of the progression, so with the regular run exhausted
	// Up Next now points at the Specials.
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+e2.ID+"/watchState", token,
		map[string]any{"watched": true}, nil); status != http.StatusOK {
		t.Fatalf("watchState E02 status = %d; body: %s", status, body)
	}
	un = upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next gone, but the Specials (S00E01) is still unwatched")
	}
	if un.ID == e1.ID || un.ID == e2.ID {
		t.Errorf("Up Next = %q, want the unwatched Specials (regular run exhausted)", un.Title)
	}
	if un.Episode == nil || un.Episode.SeasonNumber != 0 {
		t.Errorf("Up Next after watching the regular run = %+v, want a Season 0 (Specials) episode", un.Episode)
	}

	// Toggle E01 BACK to unwatched → the regular run has an unwatched episode again
	// (E01), and regular Seasons take precedence over Specials, so Up Next moves
	// back to E01 (consistent with the auto path's behavior).
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+e1.ID+"/watchState", token,
		map[string]any{"watched": false}, nil); status != http.StatusOK {
		t.Fatalf("watchState unwatch E01 status = %d; body: %s", status, body)
	}
	un = upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next gone after un-watching E01")
	}
	if un.ID != e1.ID {
		t.Errorf("Up Next = %q after un-watching E01, want E01 (regular run precedes Specials)", un.Title)
	}
}

// TestUpNextDropsWhenShowFullyWatched: when every Episode of a started Show is
// watched, the Show drops out of Up Next entirely (no next-to-watch episode).
func TestUpNextDropsWhenShowFullyWatched(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	// Double Show (2020) has exactly one Season with the multi-episode file
	// S01E05-E06 → two Episode Titles sharing one File. Marking one watched marks
	// both (sibling propagation), so the whole Show is watched in one toggle.
	shows := listShows(t, srv, token, libID)
	dblID := findShow(t, shows, "Double Show")
	seasons := showSeasons(t, srv, token, dblID)
	eps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	if len(eps.Episodes) != 2 {
		t.Fatalf("Double Show episodes = %d, want 2 (multi-episode file)", len(eps.Episodes))
	}

	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+eps.Episodes[0].ID+"/watchState", token,
		map[string]any{"watched": true}, nil); status != http.StatusOK {
		t.Fatalf("watchState status = %d; body: %s", status, body)
	}

	if un := upNextFor(getHome(t, srv, token), dblID); un != nil {
		t.Errorf("Up Next present for a fully-watched Show: %+v", un)
	}
}

// TestHomeParentContextOnContinueWatching: a Continue Watching entry for an
// Episode carries its Show / season / episode parent context (additive; the
// Movie path is covered by TestHomeRows and is unchanged here).
func TestHomeParentContextOnContinueWatching(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	_, eps := bearSeason1Episodes(t, srv, token, libID)
	e1 := eps.Episodes[0]

	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)

	home := getHome(t, srv, token)
	var cw *upNextHomeTitle
	for i := range home.ContinueWatching {
		if home.ContinueWatching[i].ID == e1.ID {
			cw = &home.ContinueWatching[i]
		}
	}
	if cw == nil {
		t.Fatalf("E01 not in Continue Watching; cw: %+v", home.ContinueWatching)
	}
	if cw.Episode == nil {
		t.Fatalf("Continue Watching Episode entry missing parent context")
	}
	if cw.Episode.ShowTitle != "The Bear" || cw.Episode.SeasonNumber != 1 || cw.Episode.EpisodeNumber != 1 {
		t.Errorf("Continue Watching context = %+v, want The Bear S01E01", cw.Episode)
	}

	// Recently Added also carries context for the Episode leaf.
	var ra *upNextHomeTitle
	for i := range home.RecentlyAdded {
		if home.RecentlyAdded[i].ID == e1.ID {
			ra = &home.RecentlyAdded[i]
		}
	}
	if ra == nil || ra.Episode == nil || ra.Episode.ShowTitle != "The Bear" {
		t.Errorf("Recently Added Episode entry missing/incorrect parent context: %+v", ra)
	}
}
