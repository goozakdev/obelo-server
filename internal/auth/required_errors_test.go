package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/auth"
	"github.com/goozakdev/obelo-server/internal/store"
)

// TestMissingInputIsASentinel: the api layer classifies these with errors.Is, so
// each must be a sentinel rather than text a handler has to match.
func TestMissingInputIsASentinel(t *testing.T) {
	svc, _, _ := newFixture(t)
	if _, err := svc.StartDeviceAuth(auth.DeviceInput{}, tvSourceIP); !errors.Is(err, auth.ErrDeviceClientIDRequired) {
		t.Errorf("StartDeviceAuth without clientId err = %v, want ErrDeviceClientIDRequired", err)
	}
	if _, err := svc.Login(context.Background(), "admin", "x", auth.DeviceInput{}, tvSourceIP); !errors.Is(err, auth.ErrDeviceClientIDRequired) {
		t.Errorf("Login without clientId err = %v, want ErrDeviceClientIDRequired", err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	fresh, err := auth.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Setup(context.Background(), fresh.ClaimToken(), "admin", ""); !errors.Is(err, auth.ErrCredentialsRequired) {
		t.Errorf("Setup without a password err = %v, want ErrCredentialsRequired", err)
	}
}
