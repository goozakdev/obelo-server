package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
)

// The API half of ADR-0062: `?mode=missing` and `{"mode":"missing"}` both start a
// pass in mode "missing", and an unrecognized mode's 400 names "missing" among the
// values it accepts.

// {"mode":"missing"} starts (202) and reports back a pass in mode "missing".
func TestEnrichBodyModeMissingSelectsTheMissingMode(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }

	srv.AwaitEnrichPass(libID)
	var ack enrichPassResp
	status, raw := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/enrich", token,
		map[string]any{"mode": "missing"}, &ack)
	if status != http.StatusAccepted {
		t.Fatalf(`POST enrich {"mode":"missing"} = %d, want 202; body: %s`, status, raw)
	}
	if !ack.Started {
		t.Fatalf("the pass did not start (one was already running); body: %s", raw)
	}
	if ack.Mode != "missing" {
		t.Fatalf("ack named mode %q, want %q; body: %s", ack.Mode, "missing", raw)
	}
	srv.AwaitEnrichPass(libID)
}

// The same value in the query string, the other spelling the endpoint accepts.
func TestEnrichQueryModeMissingSelectsTheMissingMode(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }

	res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich?mode=missing", nil)
	// recheckHarness's fixture is entirely settled 'unmatched', which ModeMissing
	// admits exactly as ModeRecheck does — same population here, different mode name.
	settled := res.Total
	if settled == 0 {
		t.Fatalf("?mode=missing visited 0 Titles — nothing for this test to have exercised")
	}
	if res.Matched != settled {
		t.Fatalf("?mode=missing matched %d of %d", res.Matched, settled)
	}
}

// An unrecognized mode's 400 names "missing" among the accepted values (D029):
// the message is the only place a caller not reading source code learns the
// current vocabulary.
func TestEnrichUnknownModeMessageNamesMissing(t *testing.T) {
	srv, _, token, libID := recheckHarness(t)

	status, body := srv.JSON(http.MethodPost, "/api/v1/libraries/"+libID+"/enrich?mode=deep", token, nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("POST enrich?mode=deep = %d, want 400; body: %s", status, body)
	}
	if !strings.Contains(string(body), `missing`) {
		t.Fatalf("400 body %s does not name \"missing\" among the accepted values", body)
	}
}
