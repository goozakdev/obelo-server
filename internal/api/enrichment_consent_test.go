package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
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
		testharness.WithEnrichmentKey("test-key"),   // TMDB configured → video WOULD be enabled…
		testharness.WithoutEnrichmentConsent(),       // …but consent is undecided, so it is gated off
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
