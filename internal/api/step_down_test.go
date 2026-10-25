package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The Step-down ladder on the wire (ADR-0071): every decision carries `stepDown` as
// an array — never null, never absent — and the handshake advertises it.

func TestPlaybackDecisionCarriesStepDownAsAnArray(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	list := scanFixtureLibrary(t, srv, token)
	duneID := findTitle(t, list, "Dune")

	var raw map[string]json.RawMessage
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+duneID+"/playback", token, mp4Profile(), &raw)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d; body: %s", status, body)
	}
	got, ok := raw["stepDown"]
	if !ok {
		t.Fatalf("decision has no stepDown field; body: %s", body)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(got)), "[") {
		t.Errorf("stepDown = %s, want a JSON array (an empty one when there is no rung)", got)
	}
	var rungs []struct {
		Current bool `json:"current"`
	}
	if err := json.Unmarshal(got, &rungs); err != nil {
		t.Fatalf("stepDown = %s: %v", got, err)
	}
	current := 0
	for _, r := range rungs {
		if r.Current {
			current++
		}
	}
	if current != 1 {
		t.Errorf("stepDown = %s, want exactly one current rung", got)
	}
}

func TestHandshakeAdvertisesStepDown(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	var got serverInfo
	if status, body := srv.GET("/api/v1/server", &got); status != http.StatusOK {
		t.Fatalf("handshake status = %d; body: %s", status, body)
	}
	if !got.Features["stepDown"] {
		t.Errorf("features.stepDown = %v, want true", got.Features["stepDown"])
	}
}
