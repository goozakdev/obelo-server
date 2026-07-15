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
	DurationMs       int64             `json:"durationMs"`
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

// TestUpNextExcludesInProgressAnchor: an in-progress (mid-band) Episode is the
// Show's anchor AND its resume point, but Home's Up Next EXCLUDES it — an
// in-progress anchor belongs to Continue Watching, so the two Home rows stay
// disjoint and never list the same Episode (ADR-0028).
func TestUpNextExcludesInProgressAnchor(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1 := eps.Episodes[0]

	// Before any watch state, the Show is NOT started → no Up Next.
	if got := upNextFor(getHome(t, srv, token), showID); got != nil {
		t.Fatalf("Up Next present before the Show is started: %+v", got)
	}

	// Report mid-band progress on E01 → it is the in-progress anchor. It lands in
	// Continue Watching and is kept OUT of Home's Up Next (disjointness).
	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)

	home := getHome(t, srv, token)
	if un := upNextFor(home, showID); un != nil {
		t.Errorf("Up Next lists the in-progress anchor E01 (%+v); it belongs to Continue Watching only", un)
	}
	var inCW bool
	for i := range home.ContinueWatching {
		if home.ContinueWatching[i].ID == e1.ID {
			inCW = true
		}
	}
	if !inCW {
		t.Errorf("in-progress E01 missing from Continue Watching; cw: %+v", home.ContinueWatching)
	}
}

// TestUpNextAdvancesOnThresholdCrossing: crossing the ~90% Watched threshold on
// E01 (the auto playback path) makes E01 the watched anchor, so the resume point
// advances to the next unwatched after it = E02.
func TestUpNextAdvancesOnThresholdCrossing(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	// Mid first: E01 is the in-progress anchor, so it sits in Continue Watching and
	// is EXCLUDED from Home's Up Next (ADR-0028 disjointness) until it crosses 90%.
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)
	if un := upNextFor(getHome(t, srv, token), showID); un != nil {
		t.Fatalf("Up Next before crossing = %+v, want none (E01 is the in-progress anchor → Continue Watching)", un)
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

// TestHomeContinueWatchingCarriesDuration: a Continue Watching entry carries the
// durationMs that, with resumePositionMs, draws the card's progress bar — the
// same pairing the Show detail's resumePoint already carried. The value is the
// Title's real playable duration, so the fraction is computable from /home alone
// with no extra round-trip. Up Next / Recently Added draw no bar and omit it.
func TestHomeContinueWatchingCarriesDuration(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1 := eps.Episodes[0]

	// Mid-band progress on E01 makes it the in-progress anchor → Continue Watching.
	// (Up Next stays empty here by design: the in-progress anchor is excluded from
	// it, so the two rows are disjoint — TestUpNextExcludesInProgressAnchor.)
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
	if cw.DurationMs != dur {
		t.Errorf("Continue Watching durationMs = %d, want %d (the Title's playable duration)", cw.DurationMs, dur)
	}
	// The point of the field: the progress fraction is computable, and lands in the
	// 2–90% band the row is defined by.
	frac := float64(cw.ResumePositionMs) / float64(cw.DurationMs)
	if frac <= 0.02 || frac >= 0.90 {
		t.Errorf("progress fraction = %.3f (resume %d / duration %d), want inside the 2–90%% band",
			frac, cw.ResumePositionMs, cw.DurationMs)
	}

	// Recently Added lists the same Episode, draws no bar, and omits the duration.
	for i := range home.RecentlyAdded {
		if home.RecentlyAdded[i].DurationMs != 0 {
			t.Errorf("Recently Added durationMs = %d, want 0 (omitted — no progress bar on that row)",
				home.RecentlyAdded[i].DurationMs)
		}
	}

	// Finishing E01 advances Up Next to E02. That row carries no resume and draws
	// no bar, so it omits the duration too.
	playEpisodeToEnd(t, srv, token, e1.ID)
	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next missing after finishing E01")
	}
	if un.DurationMs != 0 {
		t.Errorf("Up Next durationMs = %d, want 0 (omitted — no progress bar on that row)", un.DurationMs)
	}
}

// playEpisodeToEnd plays an Episode past the ~90% Watched threshold through the
// real playback path (negotiate → progress), so it becomes a watched anchor
// carrying played_at — the recency the resume point reads.
func playEpisodeToEnd(t *testing.T, srv *testharness.Server, token, titleID string) {
	t.Helper()
	dur := titleDuration(t, srv, token, titleID)
	dec := negotiateEpisode(t, srv, token, titleID)
	out := postProgress(t, srv, token, dec.SessionID, dur-1, http.StatusOK)
	if !out.Watched {
		t.Fatalf("episode %s watched = false after playing to the end", titleID)
	}
}

// markWatched drives the MANUAL PUT /titles/{id}/watchState toggle (no playback,
// so it never stamps played_at / moves the anchor).
func markWatched(t *testing.T, srv *testharness.Server, token, titleID string, watched bool) {
	t.Helper()
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+titleID+"/watchState", token,
		map[string]any{"watched": watched}, nil); status != http.StatusOK {
		t.Fatalf("watchState %s (watched=%v) status = %d; body: %s", titleID, watched, status, body)
	}
}

// TestUpNextResumesForwardWithWrap: playing E02 without ever playing E01 anchors
// the resume point on E02, so Up Next offers the next unwatched AFTER it (the
// deferred Specials) rather than nagging the skipped E01 — which resurfaces
// exactly once, at the wrap, after the Specials is played too.
func TestUpNextResumesForwardWithWrap(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Play E02 to completion; E01 is never played (a deliberate skip).
	playEpisodeToEnd(t, srv, token, e2.ID)
	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next missing after playing E02; the Specials is still unwatched")
	}
	if un.ID == e1.ID {
		t.Fatalf("Up Next = the skipped E01 (%q); a skip must not be nagged before the wrap", e1.Title)
	}
	if un.Episode == nil || un.Episode.SeasonNumber != 0 {
		t.Errorf("Up Next after E02 = %+v, want the deferred Specials (Season 0)", un.Episode)
	}

	// Play the Specials too → the anchor moves to it (most recently played). It is
	// last in Show order, so the resume point WRAPS to the first unwatched = E01.
	playEpisodeToEnd(t, srv, token, un.ID)
	un = upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next missing after the wrap; the skipped E01 is still unwatched")
	}
	if un.ID != e1.ID {
		t.Errorf("Up Next after wrap = %q, want the once-skipped E01 (%q)", un.Title, e1.Title)
	}
}

// TestUpNextAnchorFollowsPlaybackJumpAhead: starting E01 then jumping ahead to
// play E02 moves the anchor to the most-recently-PLAYED Episode (E02); the resume
// point advances past the mid-progress E01 (which stays in Continue Watching),
// keeping the two Home rows disjoint.
func TestUpNextAnchorFollowsPlaybackJumpAhead(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Start E01 mid-band (in-progress anchor → Continue Watching, not Up Next).
	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)
	if un := upNextFor(getHome(t, srv, token), showID); un != nil {
		t.Fatalf("Up Next lists in-progress E01: %+v", un)
	}

	// Jump ahead: play E02 to completion. The anchor follows to E02 (watched); the
	// resume point is the next unwatched after it (the Specials), NOT the earlier E01.
	playEpisodeToEnd(t, srv, token, e2.ID)
	home := getHome(t, srv, token)
	un := upNextFor(home, showID)
	if un == nil {
		t.Fatalf("Up Next missing after jumping ahead to E02")
	}
	if un.ID == e1.ID {
		t.Errorf("Up Next = E01 (%q); the anchor should have followed playback to E02", e1.Title)
	}
	if un.Episode == nil || un.Episode.SeasonNumber != 0 {
		t.Errorf("Up Next after the jump-ahead = %+v, want the deferred Specials", un.Episode)
	}
	// E01 stays mid-progress in Continue Watching — disjoint from Up Next.
	var inCW bool
	for i := range home.ContinueWatching {
		if home.ContinueWatching[i].ID == e1.ID {
			inCW = true
		}
	}
	if !inCW {
		t.Errorf("E01 dropped from Continue Watching after jumping ahead; cw: %+v", home.ContinueWatching)
	}
}

// TestUpNextManualMarkAdvancesWithoutMovingAnchor: after playing E01 (anchor E01,
// resume point E02), MANUALLY marking E02 watched does not move the anchor but does
// remove E02 from the unwatched set, so the resume point advances past it to the
// Specials ("doesn't move the anchor" is not "changes nothing", ADR-0028).
func TestUpNextManualMarkAdvancesWithoutMovingAnchor(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	playEpisodeToEnd(t, srv, token, e1.ID)
	if un := upNextFor(getHome(t, srv, token), showID); un == nil || un.ID != e2.ID {
		t.Fatalf("Up Next after playing E01 = %+v, want E02", un)
	}

	markWatched(t, srv, token, e2.ID, true)
	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil {
		t.Fatalf("Up Next gone after marking E02 watched; the Specials is still unwatched")
	}
	if un.ID == e2.ID {
		t.Errorf("Up Next still = E02 after marking it watched; a mark must advance the next past it")
	}
	if un.Episode == nil || un.Episode.SeasonNumber != 0 {
		t.Errorf("Up Next after marking E02 = %+v, want the deferred Specials", un.Episode)
	}
}

// TestUpNextMarksOnlyYieldsFirstUnwatched: a Show touched only by manual marks has
// no played_at, so it has no anchor and degenerates to first-unwatched — marking a
// LATER Episode watched never moves the resume point forward.
func TestUpNextMarksOnlyYieldsFirstUnwatched(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Mark E02 watched manually (no playback anywhere) → no anchor → first-unwatched.
	markWatched(t, srv, token, e2.ID, true)
	un := upNextFor(getHome(t, srv, token), showID)
	if un == nil || un.ID != e1.ID {
		t.Fatalf("Up Next for a marks-only Show = %+v, want first-unwatched E01 (%q)", un, e1.Title)
	}
}
