package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleEventsNilBroker: Deps.Events is documented as optional, so GET
// /events with no Broker answers 503 instead of panicking.
func TestHandleEventsNilBroker(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handleEvents(nil)(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil-broker /events = %d, want 503", rec.Code)
	}
}

// TestRequireGetOrHead: GET and HEAD pass, everything else is a 405 whose Allow
// names both.
func TestRequireGetOrHead(t *testing.T) {
	t.Parallel()
	h := requireGetOrHead(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for method, want := range map[string]int{
		http.MethodGet: http.StatusNoContent, http.MethodHead: http.StatusNoContent,
		http.MethodPost: http.StatusMethodNotAllowed,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(method, "/x", nil))
		if rec.Code != want {
			t.Errorf("%s = %d, want %d", method, rec.Code, want)
		}
		if method == http.MethodPost && rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("Allow = %q, want \"GET, HEAD\"", rec.Header().Get("Allow"))
		}
	}
}
