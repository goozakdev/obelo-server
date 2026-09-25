package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Auto-skip (ADR-0065 §6): a per-User, server-side setting per Marker kind. It
// lives in the User's own settings at /me/marker-auto-skip, so it follows the
// User from client to client, and each session's Markers say which of them the
// viewer skips automatically.

type autoSkipJSON struct {
	Intro   bool `json:"intro"`
	Recap   bool `json:"recap"`
	Credits bool `json:"credits"`
	Preview bool `json:"preview"`
}

func getAutoSkip(t *testing.T, srv *testharness.Server, token string) autoSkipJSON {
	t.Helper()
	var out autoSkipJSON
	if status, body := srv.AuthGET("/api/v1/me/marker-auto-skip", token, &out); status != http.StatusOK {
		t.Fatalf("get auto-skip status = %d; body: %s", status, body)
	}
	return out
}

func putAutoSkip(t *testing.T, srv *testharness.Server, token string, in any, wantStatus int) autoSkipJSON {
	t.Helper()
	var out autoSkipJSON
	status, body := srv.JSON(http.MethodPut, "/api/v1/me/marker-auto-skip", token, in, &out)
	if status != wantStatus {
		t.Fatalf("put auto-skip status = %d, want %d; body: %s", status, wantStatus, body)
	}
	return out
}

// TestAutoSkipRoundTripsThroughTheUsersOwnSettings: off for every kind until the
// User turns one on; a PUT is the whole choice; another User neither sees nor
// changes it — there is no route that names somebody else.
func TestAutoSkipRoundTripsThroughTheUsersOwnSettings(t *testing.T) {
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	srv.CreateUser(admin, "member", "memberpass123", "member")
	member := srv.LoginAs("member", "memberpass123")

	if got := getAutoSkip(t, srv, admin); got != (autoSkipJSON{}) {
		t.Fatalf("fresh auto-skip = %+v, want every kind off", got)
	}
	want := autoSkipJSON{Intro: true}
	if got := putAutoSkip(t, srv, admin, want, http.StatusOK); got != want {
		t.Errorf("PUT answered %+v, want %+v", got, want)
	}
	if got := getAutoSkip(t, srv, admin); got != want {
		t.Errorf("round-tripped auto-skip = %+v, want %+v", got, want)
	}
	// A second sign-in of the same User — another client — reads the same setting.
	again := login(t, srv, "brandon", "hunter2hunter2", "iPad", "ios", "second-client").Token
	if got := getAutoSkip(t, srv, again); got != want {
		t.Errorf("another client of the same User reads %+v, want %+v", got, want)
	}

	if got := getAutoSkip(t, srv, member); got != (autoSkipJSON{}) {
		t.Errorf("another User reads %+v, want every kind off", got)
	}
	putAutoSkip(t, srv, member, autoSkipJSON{Credits: true}, http.StatusOK)
	if got := getAutoSkip(t, srv, admin); got != want {
		t.Errorf("after another User's PUT, auto-skip = %+v, want %+v unchanged", got, want)
	}

	putAutoSkip(t, srv, admin, map[string]any{"commercial": true}, http.StatusBadRequest)
	if status, _ := srv.AuthGET("/api/v1/me/marker-auto-skip", "", nil); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET status = %d, want 401", status)
	}
}

// TestMarkersSayWhichKindsTheViewerAutoSkips: the same File's Markers, read by
// two Users, carry each viewer's own choice — Intro auto-skipped for the User who
// turned it on and not for the one who did not, and Credits for neither.
func TestMarkersSayWhichKindsTheViewerAutoSkips(t *testing.T) {
	requireFixtures(t)
	srv := testharness.New(t)
	admin := adminToken(t, srv)
	duneID, _ := markerLibrary(t, srv, admin, 0.55)
	memberID := srv.CreateUser(admin, "member", "memberpass123", "member")
	var libs struct {
		Libraries []struct {
			ID string `json:"id"`
		} `json:"libraries"`
	}
	if status, body := srv.AuthGET("/api/v1/libraries", admin, &libs); status != http.StatusOK || len(libs.Libraries) != 1 {
		t.Fatalf("libraries status = %d; body: %s", status, body)
	}
	grantLibraries(t, srv, admin, memberID, libs.Libraries[0].ID)
	member := srv.LoginAs("member", "memberpass123")

	putAutoSkip(t, srv, admin, autoSkipJSON{Intro: true}, http.StatusOK)

	for _, tc := range []struct {
		who       string
		token     string
		wantIntro bool
	}{{"admin", admin, true}, {"member", member, false}} {
		dec := negotiateDune(t, srv, tc.token, duneID)
		var got struct {
			Markers []struct {
				Kind     string `json:"kind"`
				AutoSkip bool   `json:"autoSkip"`
			} `json:"markers"`
		}
		if status, body := srv.AuthGET("/api/v1/sessions/"+dec.SessionID+"/markers", tc.token, &got); status != http.StatusOK {
			t.Fatalf("%s markers status = %d; body: %s", tc.who, status, body)
		}
		if len(got.Markers) != 2 {
			t.Fatalf("%s markers = %+v, want an intro and a credits", tc.who, got.Markers)
		}
		for _, m := range got.Markers {
			want := m.Kind == "intro" && tc.wantIntro
			if m.AutoSkip != want {
				t.Errorf("%s: %s autoSkip = %v, want %v", tc.who, m.Kind, m.AutoSkip, want)
			}
		}
	}
}

// TestMarkersFlagTheCreditsThatAreTheWatchedPoint: the Credits Marker a /progress
// report crossing marks the Title watched (one starting at or past half the File)
// is flagged watchedPoint, so a player offers "Next episode" only for it; a
// Credits starting earlier, and every other kind, is not.
func TestMarkersFlagTheCreditsThatAreTheWatchedPoint(t *testing.T) {
	requireFixtures(t)
	for _, tc := range []struct {
		name        string
		creditsFrac float64
		want        bool
	}{{"past halfway", 0.55, true}, {"before halfway", 0.35, false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := testharness.New(t)
			token := adminToken(t, srv)
			duneID, _ := markerLibrary(t, srv, token, tc.creditsFrac)
			dec := negotiateDune(t, srv, token, duneID)
			var got struct {
				Markers []struct {
					Kind         string `json:"kind"`
					WatchedPoint bool   `json:"watchedPoint"`
				} `json:"markers"`
			}
			if status, body := srv.AuthGET("/api/v1/sessions/"+dec.SessionID+"/markers", token, &got); status != http.StatusOK {
				t.Fatalf("markers status = %d; body: %s", status, body)
			}
			if len(got.Markers) != 2 {
				t.Fatalf("markers = %+v, want an intro and a credits", got.Markers)
			}
			for _, m := range got.Markers {
				want := m.Kind == "credits" && tc.want
				if m.WatchedPoint != want {
					t.Errorf("%s watchedPoint = %v, want %v", m.Kind, m.WatchedPoint, want)
				}
			}
		})
	}
}
