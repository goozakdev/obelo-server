package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// enrichmentConsentView decodes GET/PUT /settings/enrichment-consent (ADR-0032).
type enrichmentConsentView struct {
	State     string `json:"state"`
	GrantedAt string `json:"grantedAt"`
}

const consentPath = "/api/v1/settings/enrichment-consent"

func getConsent(t *testing.T, srv *testharness.Server, token string) enrichmentConsentView {
	t.Helper()
	var v enrichmentConsentView
	status, body := srv.AuthGET(consentPath, token, &v)
	if status != http.StatusOK {
		t.Fatalf("GET consent = %d, want 200; body: %s", status, body)
	}
	return v
}

func putConsent(t *testing.T, srv *testharness.Server, token string, granted bool) enrichmentConsentView {
	t.Helper()
	var v enrichmentConsentView
	status, body := srv.JSON(http.MethodPut, consentPath, token, map[string]any{"granted": granted}, &v)
	if status != http.StatusOK {
		t.Fatalf("PUT consent(%v) = %d, want 200; body: %s", granted, status, body)
	}
	return v
}

// TestEnrichmentConsentGate is the core acceptance test for issue 01 (ADR-0032):
// a fresh install with a provider CONFIGURED but consent UNDECIDED makes zero
// outbound provider calls; granting consent opens the gate so a pass enriches;
// revoking it closes the gate again. It drives the real Manager Reload path
// (WithProviderBuilder) so the gate under test is the production one.
func TestEnrichmentConsentGate(t *testing.T) {
	requireFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)),
		testharness.WithEnrichmentKey("test-key"), // TMDB configured → video WOULD be enabled…
		testharness.WithoutEnrichmentConsent(),    // …but consent is undecided, so it is gated off
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)

	// A fresh install reports consent unset — the SPA shows the first-run prompt.
	if v := getConsent(t, srv, token); v.State != "unset" {
		t.Fatalf("fresh consent state = %q, want unset", v.State)
	}

	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")

	// With consent undecided, a manual enrich pass makes ZERO provider calls and
	// records every Title 'disabled' — no surprise outbound calls before consent.
	res := enrichLib(t, srv, token, libID, "")
	if prov.calls() != 0 {
		t.Fatalf("provider called %d times before consent, want 0", prov.calls())
	}
	if res.Matched != 0 || res.Disabled == 0 {
		t.Fatalf("pre-consent pass result = %+v, want 0 matched + some disabled", res)
	}
	duneID := titleIDByName(t, srv, token, libID, "Dune")
	if d := getEnrichedDetail(t, srv, token, duneID); d.EnrichmentStatus != "disabled" {
		t.Fatalf("pre-consent Dune status = %q, want disabled", d.EnrichmentStatus)
	}

	// Grant consent. The PUT reports granted and re-gates the running provider.
	if v := putConsent(t, srv, token, true); v.State != "granted" || v.GrantedAt == "" {
		t.Fatalf("after grant view = %+v, want granted with a timestamp", v)
	}
	if v := getConsent(t, srv, token); v.State != "granted" {
		t.Fatalf("consent state after grant = %q, want granted", v.State)
	}

	// Now the SAME configuration enriches: a pass consults the provider and matches.
	res = enrichLib(t, srv, token, libID, "full")
	if prov.calls() == 0 {
		t.Fatalf("provider not called after consent granted, want > 0")
	}
	if res.Matched == 0 {
		t.Fatalf("post-consent pass result = %+v, want some matched", res)
	}
	if d := getEnrichedDetail(t, srv, token, duneID); d.EnrichmentStatus != "matched" {
		t.Fatalf("post-consent Dune status = %q, want matched", d.EnrichmentStatus)
	}

	// Revoke consent: the gate closes again — a further pass makes no NEW calls.
	if v := putConsent(t, srv, token, false); v.State != "declined" {
		t.Fatalf("after revoke view = %+v, want declined", v)
	}
	callsBefore := prov.calls()
	res = enrichLib(t, srv, token, libID, "full")
	if prov.calls() != callsBefore {
		t.Fatalf("provider called %d times after revoke, want 0 new", prov.calls()-callsBefore)
	}
	if res.Disabled == 0 {
		t.Fatalf("post-revoke pass result = %+v, want some disabled", res)
	}
}

// TestEnrichmentConsentGatesTheDisplayPaths is the acceptance test for issue 05:
// the settings GET and the per-Library policy GET — the two reads that exist ONLY
// to tell the admin UI what is enabled — report enrichment OFF whenever consent is
// not granted, for a server with a provider key configured. It walks all three
// consent states in ONE running server, so the flip is proven live: unanswered →
// declined → granted, no restart, no reconstruction.
//
// The combination that matters (and that nothing covered before) is configured AND
// not-granted: with no key at all the response would read off for an unrelated
// reason, which is why the harness supplies a TMDB key throughout and the test
// asserts configuredEnablement stays ON in every state.
func TestEnrichmentConsentGatesTheDisplayPaths(t *testing.T) {
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }}
	srv := testharness.New(t,
		testharness.WithProviderBuilder(countingBuilder(prov)), // the real Manager path
		testharness.WithEnrichmentKey("test-key"),              // TMDB configured → video WOULD be on
		testharness.WithoutEnrichmentConsent(),                 // …and consent has never been answered
	)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, t.TempDir())

	// assertOff checks both display reads at once: what the server will DO is off,
	// what is CONFIGURED is unchanged, and the consent decision explains the gap.
	assertOff := func(wantState string) {
		t.Helper()
		pv := getProviders(t, srv, token)
		if pv.Enablement.Video || pv.Enablement.Music {
			t.Errorf("consent %s: settings enablement = %+v, want off (the server makes no calls)", wantState, pv.Enablement)
		}
		if !pv.ConfiguredEnablement.Video {
			t.Errorf("consent %s: configuredEnablement = %+v, want video on (a key IS on file)", wantState, pv.ConfiguredEnablement)
		}
		if pv.ConsentState != wantState {
			t.Errorf("consent %s: settings consentState = %q", wantState, pv.ConsentState)
		}
		lv := getPolicy(t, srv, token, libID)
		if lv.Effective.Video || lv.Effective.Music {
			t.Errorf("consent %s: policy effective = %+v, want off for a Library whose policy would enable it", wantState, lv.Effective)
		}
		if !lv.Configured.Video {
			t.Errorf("consent %s: policy configured = %+v, want video on (the policy inherits an enabled global)", wantState, lv.Configured)
		}
		if lv.InheritedEnrichEnabled {
			t.Errorf("consent %s: inheritedEnrichEnabled = true, want false — \"Inherit (currently On)\" would promise calls consent forbids", wantState)
		}
		if lv.ConsentState != wantState {
			t.Errorf("consent %s: policy consentState = %q", wantState, lv.ConsentState)
		}
	}

	// 1. Never answered. A fresh install is NOT a grant.
	assertOff("unset")

	// 2. Explicitly declined — a different decision, the same reported enablement.
	putConsent(t, srv, token, false)
	assertOff("declined")

	// 3. Granted: both reads flip on with no restart, on the same server object.
	putConsent(t, srv, token, true)
	pv := getProviders(t, srv, token)
	if !pv.Enablement.Video || pv.ConsentState != "granted" {
		t.Errorf("after grant: settings view enablement = %+v state = %q, want video on + granted", pv.Enablement, pv.ConsentState)
	}
	lv := getPolicy(t, srv, token, libID)
	if !lv.Effective.Video || !lv.InheritedEnrichEnabled || lv.ConsentState != "granted" {
		t.Errorf("after grant: policy effective = %+v inherited = %v state = %q, want video on + inherited on + granted",
			lv.Effective, lv.InheritedEnrichEnabled, lv.ConsentState)
	}

	// 4. And back off again: revoking re-closes the gate on the display paths too,
	//    so the screen tracks the decision in both directions.
	putConsent(t, srv, token, false)
	assertOff("declined")

	// Nothing in this test should have contacted a provider: it only reads settings.
	if prov.calls() != 0 {
		t.Errorf("provider called %d times by settings/policy reads, want 0", prov.calls())
	}
}

// TestEnrichmentConsentRequiresGrantedField rejects a PUT that omits the decision,
// so a malformed client can never silently flip consent.
func TestEnrichmentConsentRequiresGrantedField(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	var v enrichmentConsentView
	status, _ := srv.JSON(http.MethodPut, consentPath, token, map[string]any{}, &v)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("PUT consent with no granted field = %d, want 422", status)
	}
}

// TestEnrichmentConsentAdminOnly confirms the consent endpoints are Admin-gated
// like the rest of the /settings subtree.
func TestEnrichmentConsentAdminOnly(t *testing.T) {
	srv := testharness.New(t)
	adminToken(t, srv) // claim the first admin so /settings is auth-gated, not setup-gated
	srv.CreateMember("bob", "pw12345678")
	member := srv.LoginAs("bob", "pw12345678")
	var v enrichmentConsentView
	if status, _ := srv.AuthGET(consentPath, member, &v); status != http.StatusForbidden {
		t.Fatalf("GET consent as member = %d, want 403", status)
	}
	if status, _ := srv.JSON(http.MethodPut, consentPath, member, map[string]any{"granted": true}, &v); status != http.StatusForbidden {
		t.Fatalf("PUT consent as member = %d, want 403", status)
	}
}
