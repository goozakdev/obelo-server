package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Black-box integration tests for issue 02: the nullable `resumePoint` on
// GET /shows/{id}/seasons — the Show detail page's next-episode block. It renders
// the SAME resume-point computation as Home's Up Next (ADR-0028) but KEEPS the
// in-progress-anchor case Home omits. Driven through the real handlers against the
// scanned TV fixtures: playback progress (auto Watched-threshold path) and the
// manual PUT watchState toggle, observed through the seasons response.

// --- wire shapes (resumePoint additions) ------------------------------------

type resumePointResp struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	SeasonID         string `json:"seasonId"`
	SeasonNumber     int    `json:"seasonNumber"`
	EpisodeNumber    int    `json:"episodeNumber"`
	EpisodeLabel     string `json:"episodeLabel"`
	Title            string `json:"title"`
	Overview         string `json:"overview"`
	ResumePositionMs int64  `json:"resumePositionMs"`
	Mode             string `json:"mode"`
}

type resumePointShowResp struct {
	ID                    string `json:"id"`
	UnwatchedEpisodeCount int    `json:"unwatchedEpisodeCount"`
}

type resumePointSeasonsResp struct {
	Show        resumePointShowResp `json:"show"`
	Seasons     []seasonResp        `json:"seasons"`
	ResumePoint *resumePointResp    `json:"resumePoint"`
}

// showSeasonsRP fetches GET /shows/{id}/seasons decoding the resumePoint block.
func showSeasonsRP(t *testing.T, srv *testharness.Server, token, showID string) resumePointSeasonsResp {
	t.Helper()
	var resp resumePointSeasonsResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seasons", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("seasons status = %d, want 200; body: %s", status, body)
	}
	return resp
}

// TestResumePointNotStarted: a Show the User has never touched has a null resume
// point and a positive unwatched count — the detail page shows the Show
// description + Play (from the first Episode), unchanged from today.
func TestResumePointNotStarted(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, _ := bearSeason1Episodes(t, srv, token, libID)

	resp := showSeasonsRP(t, srv, token, showID)
	if resp.ResumePoint != nil {
		t.Errorf("resumePoint present for a not-started Show: %+v", resp.ResumePoint)
	}
	// The null resume point is distinguished from fully-watched by a positive
	// unwatched count (The Bear: S01E01, S01E02, S00E01 = 3).
	if resp.Show.UnwatchedEpisodeCount == 0 {
		t.Errorf("unwatchedEpisodeCount = 0 for a not-started Show; want > 0 so the client shows Play")
	}
}

// TestResumePointInProgressSurfacesOnDetailNotHome: an in-progress (mid-band)
// Episode is the Show's anchor and its resume point — it surfaces on the DETAIL
// page as an in-progress block (Continue + Restart), even though Home's Up Next
// OMITS it (it belongs to Continue Watching). The two surfaces stay disjoint.
func TestResumePointInProgressSurfacesOnDetailNotHome(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1 := eps.Episodes[0]

	// Report mid-band progress on E01 → in-progress anchor.
	dur := titleDuration(t, srv, token, e1.ID)
	dec := negotiateEpisode(t, srv, token, e1.ID)
	postProgress(t, srv, token, dec.SessionID, dur/2, http.StatusOK)

	// Home's Up Next OMITS the in-progress anchor (disjoint from Continue Watching).
	if un := upNextFor(getHome(t, srv, token), showID); un != nil {
		t.Fatalf("Home Up Next lists the in-progress anchor E01: %+v; it belongs to Continue Watching only", un)
	}

	// The detail page KEEPS it: resumePoint is the in-progress E01, mode inProgress,
	// carrying the stored resume position Continue seeks to.
	rp := showSeasonsRP(t, srv, token, showID).ResumePoint
	if rp == nil {
		t.Fatalf("resumePoint missing on the detail page for an in-progress Show")
	}
	if rp.ID != e1.ID {
		t.Errorf("resumePoint id = %q, want the in-progress anchor E01 (%q)", rp.ID, e1.ID)
	}
	if rp.Mode != "inProgress" {
		t.Errorf("resumePoint mode = %q, want inProgress (Continue + Restart)", rp.Mode)
	}
	if rp.ResumePositionMs <= 0 {
		t.Errorf("resumePoint resumePositionMs = %d, want > 0 (where Continue resumes)", rp.ResumePositionMs)
	}
	if rp.SeasonID == "" {
		t.Errorf("resumePoint seasonId empty; the client needs it to build the show-from-here Queue")
	}
}

// TestResumePointNextAfterWatchedAnchor: playing E01 to the end (a watched anchor)
// advances the resume point to the next unwatched after it (E02) in "next" mode —
// the detail page shows the block + a single Play from 0.
func TestResumePointNextAfterWatchedAnchor(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	playEpisodeToEnd(t, srv, token, e1.ID)

	rp := showSeasonsRP(t, srv, token, showID).ResumePoint
	if rp == nil {
		t.Fatalf("resumePoint missing after playing E01; E02 is still unwatched")
	}
	if rp.ID != e2.ID {
		t.Errorf("resumePoint id = %q, want the next unwatched E02 (%q)", rp.ID, e2.ID)
	}
	if rp.Mode != "next" {
		t.Errorf("resumePoint mode = %q, want next (a single Play)", rp.Mode)
	}
	if rp.ResumePositionMs != 0 {
		t.Errorf("resumePoint resumePositionMs = %d, want 0 (a fresh next Episode plays from the start)", rp.ResumePositionMs)
	}
}

// TestResumePointCrossSeasonWalk: after the last regular Episode (S01E02) is
// played, the resume point walks across the Season boundary to the next unwatched
// in Show order — the deferred Specials (Season 0), a different Season than E02.
func TestResumePointCrossSeasonWalk(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e2 := eps.Episodes[1]

	playEpisodeToEnd(t, srv, token, e2.ID)

	rp := showSeasonsRP(t, srv, token, showID).ResumePoint
	if rp == nil {
		t.Fatalf("resumePoint missing after playing S01E02; the Specials is still unwatched")
	}
	// The walk crossed from Season 1 (E02) into the deferred Specials (Season 0) — a
	// different Season, and its own seasonId, so the client's from-here Queue walks
	// the boundary.
	if rp.SeasonNumber != 0 {
		t.Errorf("resumePoint after S01E02 = S%02dE%02d (%q), want the cross-season Specials (Season 0)",
			rp.SeasonNumber, rp.EpisodeNumber, rp.Title)
	}
	if rp.SeasonID == "" || rp.SeasonNumber == e2.SeasonNumber {
		t.Errorf("resumePoint stayed in E02's Season (S%02d); the walk must cross into the next Season", e2.SeasonNumber)
	}
	if rp.Mode != "next" {
		t.Errorf("resumePoint mode = %q, want next", rp.Mode)
	}
}

// TestResumePointFullyWatched: when every Episode of a started Show is watched,
// the resume point is null and the unwatched count is 0 — the detail page reverts
// to the Show description with NO Play (restarting a finished series is not a flow).
func TestResumePointFullyWatched(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	// Double Show (2020) is one Season with the multi-episode file S01E05-E06 → two
	// Episode Titles sharing one File, so marking one watched marks both (sibling
	// propagation) and the whole Show is watched in a single toggle.
	shows := listShows(t, srv, token, libID)
	dblID := findShow(t, shows, "Double Show")
	seasons := showSeasons(t, srv, token, dblID)
	dblEps := seasonEpisodes(t, srv, token, seasons.Seasons[0].ID)
	markWatched(t, srv, token, dblEps.Episodes[0].ID, true)

	resp := showSeasonsRP(t, srv, token, dblID)
	if resp.ResumePoint != nil {
		t.Errorf("resumePoint present for a fully-watched Show: %+v", resp.ResumePoint)
	}
	if resp.Show.UnwatchedEpisodeCount != 0 {
		t.Errorf("unwatchedEpisodeCount = %d for a fully-watched Show, want 0 (no Play)", resp.Show.UnwatchedEpisodeCount)
	}
}

// TestResumePointMarksOnlyFirstUnwatched: a Show touched only by a manual mark on
// a LATER Episode has no played_at (no anchor), so the resume point degenerates to
// first-unwatched — a marks-only Show still lands the viewer in the right place,
// and the mark never moves an anchor it doesn't have.
func TestResumePointMarksOnlyFirstUnwatched(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	e1, e2 := eps.Episodes[0], eps.Episodes[1]

	// Mark E02 watched manually (no playback anywhere) → no anchor → first-unwatched E01.
	markWatched(t, srv, token, e2.ID, true)

	rp := showSeasonsRP(t, srv, token, showID).ResumePoint
	if rp == nil {
		t.Fatalf("resumePoint missing for a marks-only Show; E01 is still unwatched")
	}
	if rp.ID != e1.ID {
		t.Errorf("resumePoint id = %q, want first-unwatched E01 (%q)", rp.ID, e1.ID)
	}
	if rp.Mode != "next" {
		t.Errorf("resumePoint mode = %q, want next (no in-progress anchor)", rp.Mode)
	}
}
