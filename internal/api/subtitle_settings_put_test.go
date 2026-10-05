package api_test

import (
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// TestSubtitleProviderPutRejectsInvalidBaseURL: the subtitle-provider PUT applies
// the same absolute-http(s) baseURL check the metadata PUT does.
func TestSubtitleProviderPutRejectsInvalidBaseURL(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var env errorEnvelope
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/subtitle-providers", token,
		map[string]any{"providers": []map[string]any{{"slug": "opensubtitles", "baseURL": "not a url at all"}}}, &env)
	if status != http.StatusUnprocessableEntity || env.Error.Code != "PROVIDER_INVALID_BASE_URL" {
		t.Fatalf("PUT bad baseURL = %d/%s, want 422/PROVIDER_INVALID_BASE_URL; body: %s", status, env.Error.Code, raw)
	}
}

// TestSubtitleProviderPutRepeatedSlugFolds: two entries for one slug in a single
// PUT apply cumulatively - the second sees the first's key - instead of each being
// judged (and the last one winning) against the ORIGINAL row.
func TestSubtitleProviderPutRepeatedSlugFolds(t *testing.T) {
	t.Parallel()
	srv := testharness.New(t)
	token := adminToken(t, srv)

	var resp installedProvidersResp
	status, raw := srv.JSON(http.MethodPut, "/api/v1/settings/subtitle-providers", token,
		map[string]any{"providers": []map[string]any{
			{"slug": "opensubtitles", "apiKey": "k1"},
			{"slug": "opensubtitles", "enabled": true},
		}}, &resp)
	if status != http.StatusOK {
		t.Fatalf("PUT repeated slug = %d, want 200 (the second entry inherits the first's key); body: %s", status, raw)
	}
	if p := providerNamed(t, resp, "opensubtitles"); !p.Enabled || !p.HasKey {
		t.Errorf("opensubtitles = %+v, want enabled with the key from the first entry", p)
	}
}
