package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The API half of ADR-0051: `?mode=recheck` and `{"mode":"recheck"}` both select
// the new mode, and an unrecognized mode still falls back to the default.
//
// This matters more than a mode string usually would. Before this endpoint gained
// the mode there was NO way for an operator to re-ask a settled non-answer short
// of ModeFull, and the web app had no client method for the endpoint at all — so
// the only lever the UI offered (a scan) provably changed nothing.

// enrichSummaryResp is the pass summary, including the ADR-0048 `retrying` count
// the Needs Fixing button reports back to the operator.
type enrichSummaryResp struct {
	LibraryID string `json:"libraryId"`
	Total     int    `json:"total"`
	Matched   int    `json:"matched"`
	Unmatched int    `json:"unmatched"`
	Failed    int    `json:"failed"`
	Disabled  int    `json:"disabled"`
	Retrying  int    `json:"retrying"`
}

// recheckHarness builds a scanned Movie Library whose every Title has SETTLED
// 'unmatched' — the population ADR-0051 exists for. The returned provider starts
// out answering "no such record" and is switched to a matching answer by the
// caller, which is the code-change-shaped event a recheck adopts.
func recheckHarness(t *testing.T) (*testharness.Server, *fakeProvider, string, string) {
	t.Helper()
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("X")}),
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// Settle everything as 'unmatched'. (Whether the post-scan auto-enrich got there
	// first is not this test's business — the assertion is on the resulting state.)
	enrichLib(t, srv, token, libID, "")
	if n := len(listEnrichmentAttention(t, srv, token, libID).Titles); n == 0 {
		t.Fatalf("no Titles settled unmatched; there is nothing for a recheck to re-ask")
	}
	return srv, prov, token, libID
}

// postEnrich starts a pass through the endpoint and returns its summary.
//
// The POST is a 202 now, not a 200 with the counts (ADR-0051's amendment: a pass
// is started, never awaited), so the wait moved in here — onto the pass's own
// settled signal — and the summary is read back from the status route. What each
// test asserts about a MODE is untouched by that; which is the point, because the
// mode's semantics are not what this change is about.
func postEnrich(t *testing.T, srv *testharness.Server, token, path string, body any) enrichSummaryResp {
	t.Helper()
	libID := libraryIDFromEnrichPath(t, path)
	srv.AwaitEnrichPass(libID)

	var ack enrichPassResp
	status, raw := srv.JSON(http.MethodPost, path, token, body, &ack)
	if status != http.StatusAccepted {
		t.Fatalf("POST %s = %d, want 202; body: %s", path, status, raw)
	}
	if !ack.Started {
		t.Fatalf("POST %s did not start a pass (one was already running); body: %s", path, raw)
	}
	srv.AwaitEnrichPass(libID)

	last := enrichStatus(t, srv, token, libID).LastPass
	if last == nil {
		t.Fatalf("POST %s: no finished pass reported afterwards", path)
	}
	return enrichSummaryResp{
		LibraryID: last.LibraryID, Total: last.Total, Matched: last.Matched,
		Unmatched: last.Unmatched, Failed: last.Failed,
		Disabled: last.Disabled, Retrying: last.Retrying,
	}
}

// libraryIDFromEnrichPath pulls the Library id out of an "…/libraries/{id}/enrich"
// path (with or without a ?mode= suffix), so postEnrich can await the pass it
// started without every caller repeating the id.
func libraryIDFromEnrichPath(t *testing.T, path string) string {
	t.Helper()
	_, rest, ok := strings.Cut(path, "/libraries/")
	if !ok {
		t.Fatalf("not a library enrich path: %q", path)
	}
	id, _, ok := strings.Cut(rest, "/enrich")
	if !ok {
		t.Fatalf("not a library enrich path: %q", path)
	}
	return id
}

// ?mode=recheck re-asks the settled rows an only-new pass cannot see.
func TestEnrichQueryModeRecheckReAsksTheSettledRows(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	settled := len(listEnrichmentAttention(t, srv, token, libID).Titles)

	// The only-new pass is the control: it is what a scan fires, and it is what left
	// the operator's queue at 722 rows with a blank reason.
	before := prov.calls()
	if res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich", nil); res.Total != 0 {
		t.Fatalf("the default pass visited %d Titles, want 0 — every Title here is settled "+
			"'unmatched', which ModeNew deliberately does not admit", res.Total)
	}
	if prov.calls() != before {
		t.Fatalf("the default pass made %d provider calls, want 0", prov.calls()-before)
	}

	// The improvement lands.
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }

	res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich?mode=recheck", nil)
	if res.Total != settled || res.Matched != settled {
		t.Fatalf("recheck summary %+v, want total=matched=%d — the settled non-answers are "+
			"exactly the population this mode is for", res, settled)
	}
	if n := len(listEnrichmentAttention(t, srv, token, libID).Titles); n != 0 {
		t.Fatalf("%d Titles still on the attention list after a successful recheck", n)
	}
}

// The same value in the JSON body, which is the other spelling the endpoint has
// always accepted for `full`.
func TestEnrichBodyModeRecheckSelectsTheSameMode(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	settled := len(listEnrichmentAttention(t, srv, token, libID).Titles)
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }

	res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich",
		map[string]any{"mode": "recheck"})
	if res.Total != settled || res.Matched != settled {
		t.Fatalf(`{"mode":"recheck"} summary %+v, want total=matched=%d`, res, settled)
	}
}

// An unrecognized mode keeps today's behaviour — the default pass — rather than
// 400-ing. The handler has always been best-effort about the mode (a malformed
// body leaves the default too), and an older or newer client naming a mode this
// build does not have should get the cheap, safe pass rather than an error.
func TestEnrichUnknownModeFallsBackToTheDefault(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }
	before := prov.calls()

	for _, mode := range []string{"?mode=deep", "?mode=", "?mode=RECHECKS"} {
		res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich"+mode, nil)
		if res.Total != 0 {
			t.Errorf("%q visited %d Titles, want 0 — an unknown mode must fall back to the "+
				"default, not to the widest thing available", mode, res.Total)
		}
	}
	if res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich",
		map[string]any{"mode": "deep"}); res.Total != 0 {
		t.Errorf(`{"mode":"deep"} visited %d Titles, want 0`, res.Total)
	}
	if prov.calls() != before {
		t.Errorf("an unknown mode made %d provider calls", prov.calls()-before)
	}
}

// Case is not significant, exactly as it is not for `full`.
func TestEnrichModeRecheckIsCaseInsensitive(t *testing.T) {
	srv, prov, token, libID := recheckHarness(t)
	settled := len(listEnrichmentAttention(t, srv, token, libID).Titles)
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }

	if res := postEnrich(t, srv, token, "/api/v1/libraries/"+libID+"/enrich?mode=Recheck",
		nil); res.Matched != settled {
		t.Fatalf("?mode=Recheck matched %d, want %d", res.Matched, settled)
	}
}
