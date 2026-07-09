package api_test

import (
	"net/http"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// serverInfo mirrors the camelCase handshake response so the test asserts the
// wire shape clients actually see, not internal types.
type serverInfo struct {
	Version           string          `json:"version"`
	SupportedVersions []int           `json:"supportedVersions"`
	Features          map[string]bool `json:"features"`
	SetupRequired     bool            `json:"setupRequired"`
}

type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// TestServerHandshake is the smoke test: a fresh server boots and GET
// /api/v1/server returns version, supported API versions, a feature-flags map,
// and setupRequired=true on the empty DB.
func TestServerHandshake(t *testing.T) {
	srv := testharness.New(t)

	var got serverInfo
	status, body := srv.GET("/api/v1/server", &got)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}
	if got.Version == "" {
		t.Errorf("version is empty; body: %s", body)
	}
	if len(got.SupportedVersions) == 0 {
		t.Errorf("supportedVersions is empty; body: %s", body)
	}
	wantV1 := false
	for _, v := range got.SupportedVersions {
		if v == 1 {
			wantV1 = true
		}
	}
	if !wantV1 {
		t.Errorf("supportedVersions = %v, want it to include 1", got.SupportedVersions)
	}
	if got.Features == nil {
		t.Errorf("features map is nil; body: %s", body)
	}
	if !got.SetupRequired {
		t.Errorf("setupRequired = false on a fresh DB, want true; body: %s", body)
	}
}

// TestErrorEnvelope verifies that error responses use the standard envelope
// with correct status codes and machine-readable codes.
func TestErrorEnvelope(t *testing.T) {
	srv := testharness.New(t)

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown path under prefix",
			method:     http.MethodGet,
			path:       "/api/v1/does-not-exist",
			wantStatus: http.StatusNotFound,
			wantCode:   "NOT_FOUND",
		},
		// A path OUTSIDE /api/v1 is no longer the API's concern: the top-level
		// webui composition (ADR-0012) serves the embedded SPA there with an
		// index.html fallback, so it is not an error envelope. That behavior is
		// asserted in internal/webui/webui_test.go. The API still owns unknown
		// paths UNDER the prefix (the case above).
		{
			name:       "wrong method on handshake",
			method:     http.MethodPost,
			path:       "/api/v1/server",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "", // net/http supplies 405; body code not asserted here
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.Do(tc.method, tc.path, nil)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantCode == "" {
				return
			}
			var env errorEnvelope
			status, body := srv.GET(tc.path, &env)
			_ = status
			if env.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q; body: %s", env.Error.Code, tc.wantCode, body)
			}
			if env.Error.Message == "" {
				t.Errorf("error message is empty; body: %s", body)
			}
		})
	}
}
