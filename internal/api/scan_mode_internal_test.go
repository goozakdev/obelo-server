package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/scanner"
)

// TestScanModeFromRequest: the mode is honoured from the query or a JSON body,
// including a chunked body (ContentLength -1), which used to be ignored.
func TestScanModeFromRequest(t *testing.T) {
	t.Parallel()
	chunked := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", io.NopCloser(strings.NewReader(body)))
		r.ContentLength = -1
		return r
	}
	sized := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"mode":"full"}`))
	cases := []struct {
		name string
		req  *http.Request
		want scanner.Mode
	}{
		{"query", httptest.NewRequest(http.MethodPost, "/x?mode=full", nil), scanner.ModeFull},
		{"sized body", sized, scanner.ModeFull},
		{"chunked body", chunked(`{"mode":"full"}`), scanner.ModeFull},
		{"chunked malformed", chunked(`{`), scanner.ModeIncremental},
		{"no body", httptest.NewRequest(http.MethodPost, "/x", nil), scanner.ModeIncremental},
	}
	for _, c := range cases {
		if got := scanModeFromRequest(httptest.NewRecorder(), c.req); got != c.want {
			t.Errorf("%s: mode = %v, want %v", c.name, got, c.want)
		}
	}
}
