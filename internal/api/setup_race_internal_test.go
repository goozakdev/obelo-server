package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// setupRacedStore reports no Users, as the store does to a setup that counted
// before a racing one inserted.
type setupRacedStore struct{ *store.DB }

func (setupRacedStore) CountUsers() (int, error) { return 0, nil }

// TestASetupLosingARaceForTheUsernameIsABadRequest: a setup whose username a
// racing insert took first is the caller's 400, not a server failure.
func TestASetupLosingARaceForTheUsernameIsABadRequest(t *testing.T) {
	t.Parallel()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.CreateAdmin("first", "admin", "x"); err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(setupRacedStore{db})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	body := `{"claimToken":"` + svc.ClaimToken() + `","username":"Admin","password":"hunter2hunter2"}`
	rec := httptest.NewRecorder()
	handleSetup(svc)(rec, httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "BAD_REQUEST") {
		t.Fatalf("setup losing the race = %d %s, want 400 BAD_REQUEST", rec.Code, rec.Body.String())
	}
}
