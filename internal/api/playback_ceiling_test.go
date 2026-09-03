package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the per-User Playback ceiling (ADR-0054 §2,
// .scratch/linked-servers issue 02): the Admin endpoint and its refusals, the
// promise that a ceiling never hides a Title, and the concurrent-stream cap.

// streamLimitResp is the 429 STREAM_LIMIT envelope: details { active, limit }.
// Unlike SERVER_BUSY it carries no suggestedMaxBitrate — a retry at a lower
// quality does not help, because the User's own cap is what refused.
type streamLimitResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Active int `json:"active"`
			Limit  int `json:"limit"`
		} `json:"details"`
	} `json:"error"`
}

// setPlaybackCeiling PUTs a User's whole ceiling, asserting a clean 204.
func setPlaybackCeiling(t *testing.T, srv *testharness.Server, adminTok, userID string, body map[string]any) {
	t.Helper()
	st, raw := srv.JSON(http.MethodPut, "/api/v1/users/"+userID+"/playbackCeiling", adminTok, body, nil)
	if st != http.StatusNoContent {
		t.Fatalf("set playback ceiling %v: status %d; body: %s", body, st, raw)
	}
}

// TestPlaybackCeilingManagement: the endpoint stores the whole ceiling, GET
// /users/{id} reflects it, every refusal has its own code, and an omitted
// dimension clears it (the body is a replace, not a patch).
func TestPlaybackCeilingManagement(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")

	setPlaybackCeiling(t, srv, admin, memberID, map[string]any{
		"maxResolution": "1080p", "maxBitrate": 8_000_000, "maxStreams": 2,
	})
	var ud userDetailResp
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if ud.MaxResolution != "1080p" || ud.MaxBitrate != 8_000_000 || ud.MaxStreams != 2 {
		t.Errorf("ceiling = %q/%d/%d, want 1080p/8000000/2", ud.MaxResolution, ud.MaxBitrate, ud.MaxStreams)
	}

	// An unsettable rung → 422 UNKNOWN_RESOLUTION.
	var env errorEnvelope
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+memberID+"/playbackCeiling", admin,
		map[string]any{"maxResolution": "1440p"}, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "UNKNOWN_RESOLUTION" {
		t.Errorf("unknown rung = %d/%s, want 422 UNKNOWN_RESOLUTION", st, env.Error.Code)
	}

	// A negative dimension → 400 (0 already means uncapped).
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+memberID+"/playbackCeiling", admin,
		map[string]any{"maxStreams": -1}, &env); st != http.StatusBadRequest {
		t.Errorf("negative maxStreams = %d/%s, want 400", st, env.Error.Code)
	}

	// A refused write left the stored ceiling untouched.
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if ud.MaxResolution != "1080p" || ud.MaxStreams != 2 {
		t.Errorf("after refusals ceiling = %+v, want the prior 1080p/2", ud)
	}

	// An Admin target → 422 ADMIN_CEILING (an Admin is uncapped by definition).
	var users usersListResp
	srv.AuthGET("/api/v1/users", admin, &users)
	var adminID string
	for _, u := range users.Users {
		if u.Role == "admin" {
			adminID = u.ID
		}
	}
	env = errorEnvelope{}
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/"+adminID+"/playbackCeiling", admin,
		map[string]any{"maxResolution": "720p"}, &env); st != http.StatusUnprocessableEntity ||
		env.Error.Code != "ADMIN_CEILING" {
		t.Errorf("ceiling on admin = %d/%s, want 422 ADMIN_CEILING", st, env.Error.Code)
	}

	// An unknown User → 404.
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/users/nope/playbackCeiling", admin,
		map[string]any{"maxStreams": 1}, nil); st != http.StatusNotFound {
		t.Errorf("unknown user = %d, want 404", st)
	}

	// The empty body is a REPLACE that clears every dimension.
	setPlaybackCeiling(t, srv, admin, memberID, map[string]any{})
	srv.AuthGET("/api/v1/users/"+memberID, admin, &ud)
	if ud.MaxResolution != "" || ud.MaxBitrate != 0 || ud.MaxStreams != 0 {
		t.Errorf("after clear ceiling = %+v, want uncapped", ud)
	}
}

// TestPlaybackCeilingIsAppliedToRemoteRole: the ceiling is settable on the role a
// linked Server holds, which is the case it exists for (ADR-0054 §2) — and on any
// other non-Admin role by the same code path.
func TestPlaybackCeilingIsAppliedToRemoteRole(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	remoteID := srv.CreateUser(admin, "brandons-server", "", "remote")

	setPlaybackCeiling(t, srv, admin, remoteID, map[string]any{
		"maxResolution": "1080p", "maxStreams": 2,
	})
	var ud userDetailResp
	srv.AuthGET("/api/v1/users/"+remoteID, admin, &ud)
	if ud.Role != "remote" || ud.MaxResolution != "1080p" || ud.MaxStreams != 2 {
		t.Errorf("remote ceiling = %+v, want role remote with 1080p/2", ud)
	}
}

// TestPlaybackCeilingNeverHidesATitle: a ceiling changes how a Title plays, never
// whether it exists (ADR-0054 §2). A 720p-capped Member browses exactly what the
// Admin does — no Title disappears — and playback still succeeds.
func TestPlaybackCeilingNeverHidesATitle(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	var all titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &all)

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	setPlaybackCeiling(t, srv, admin, memberID, map[string]any{"maxResolution": "720p", "maxBitrate": 2_000_000})
	member := srv.LoginAs("kid", "memberpass123")

	var capped titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", member, &capped)
	if len(capped.Titles) != len(all.Titles) {
		t.Fatalf("capped member saw %d titles, want all %d (a quality ceiling hides nothing)",
			len(capped.Titles), len(all.Titles))
	}

	// And the Title still plays — at whatever tier the ceiling implies, but it plays.
	duneID := findTitle(t, all, "Dune")
	var dec decisionResp
	if st, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", member,
		mp4Profile(), &dec); st != http.StatusOK {
		t.Fatalf("capped playback status = %d, want 200; body: %s", st, body)
	}
	if dec.SessionID == "" {
		t.Error("capped playback minted no session")
	}
}

// TestPlaybackStreamLimit: under maxStreams:2 a third concurrent negotiation
// answers 429 STREAM_LIMIT with the counts, and ending a session frees the slot.
// Every tier counts — these are all direct plays, which the transcode cap would
// never have metered.
func TestPlaybackStreamLimit(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	lib := createMovieLibrary(t, srv, admin, fixtureRoot(t))
	scanLib(t, srv, admin, lib, "")

	var all titlesListResp
	srv.AuthGET("/api/v1/libraries/"+lib+"/titles?limit=50", admin, &all)
	duneID := findTitle(t, all, "Dune")

	memberID := srv.CreateUser(admin, "kid", "memberpass123", "member")
	grantLibraries(t, srv, admin, memberID, lib)
	setPlaybackCeiling(t, srv, admin, memberID, map[string]any{"maxStreams": 2})
	member := srv.LoginAs("kid", "memberpass123")

	play := func() (int, decisionResp) {
		var dec decisionResp
		st, _ := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", member, mp4Profile(), &dec)
		return st, dec
	}

	st, first := play()
	if st != http.StatusOK {
		t.Fatalf("first playback = %d, want 200", st)
	}
	if st, _ = play(); st != http.StatusOK {
		t.Fatalf("second playback = %d, want 200", st)
	}

	// The third is refused, with the counts.
	var limit streamLimitResp
	st, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", member, mp4Profile(), &limit)
	if st != http.StatusTooManyRequests {
		t.Fatalf("third playback = %d, want 429; body: %s", st, raw)
	}
	if limit.Error.Code != "STREAM_LIMIT" {
		t.Errorf("error code = %q, want STREAM_LIMIT; body: %s", limit.Error.Code, raw)
	}
	if limit.Error.Details.Active != 2 || limit.Error.Details.Limit != 2 {
		t.Errorf("details = %d of %d, want 2 of 2; body: %s",
			limit.Error.Details.Active, limit.Error.Details.Limit, raw)
	}

	// The ADMIN, uncapped, is unaffected by the Member's cap.
	var adminDec decisionResp
	if s, b := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", admin,
		mp4Profile(), &adminDec); s != http.StatusOK {
		t.Fatalf("admin playback under the member's cap = %d, want 200; body: %s", s, b)
	}

	// Ending one of the Member's sessions frees the slot.
	if s, b := srv.JSON(http.MethodDelete, "/api/v1/sessions/"+first.SessionID, member, nil, nil); s != http.StatusNoContent {
		t.Fatalf("delete session = %d, want 204; body: %s", s, b)
	}
	if st, _ = play(); st != http.StatusOK {
		t.Fatalf("playback after freeing a slot = %d, want 200", st)
	}
}
