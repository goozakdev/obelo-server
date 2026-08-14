package tailnet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/marioquake/obelo-server/internal/store"
	"github.com/marioquake/obelo-server/internal/tailnet"
)

// The first-boot seed and the source-of-truth handoff (ADR-0043). The black-box
// half — an actual second boot of an actual server — lives in
// internal/api/tailnet_test.go; this pins the rule itself, including the failure
// mode that makes the rule worth having (an env var quietly overwriting a saved
// setting on every restart).

// seedStore is an in-memory SeedStore: one settings row, or none.
type seedStore struct {
	row    *store.TailnetSettingsUpsert
	writes int
	err    error
}

func (s *seedStore) TailnetSettingsEmpty() (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.row == nil, nil
}

func (s *seedStore) SetTailnetSettings(u store.TailnetSettingsUpsert) error {
	s.writes++
	s.row = &u
	return nil
}

// TestSeedIfEmptySeedsOnceThenIgnoresConfig: the env seeds a fresh install, and
// a later boot — with DIFFERENT env values, over settings the operator has since
// changed in the UI — writes nothing at all.
func TestSeedIfEmptySeedsOnceThenIgnoresConfig(t *testing.T) {
	s := &seedStore{}

	seeded, err := tailnet.SeedIfEmpty(s, tailnet.SeedInput{
		Enabled:      true,
		Hostname:     "from-env",
		ControlURL:   "https://headscale.example.com",
		HTTPSEnabled: true,
		ServerName:   "Living Room",
	})
	if err != nil || !seeded {
		t.Fatalf("first boot: seeded = %v, err = %v; want true, nil", seeded, err)
	}
	if got := *s.row; got.Hostname != "from-env" || !got.Enabled || !got.HTTPSEnabled ||
		got.ControlURL != "https://headscale.example.com" {
		t.Fatalf("seeded row = %+v, want the env values", got)
	}

	// The operator changes their mind in the UI.
	if err := s.SetTailnetSettings(store.TailnetSettingsUpsert{Hostname: "chosen-in-ui"}); err != nil {
		t.Fatalf("ui save: %v", err)
	}
	writesBefore := s.writes

	// A later boot, with the env still set (and now saying something else): the DB
	// is authoritative and the environment is ignored ENTIRELY — not merged, not
	// consulted for the fields the UI did not touch.
	seeded, err = tailnet.SeedIfEmpty(s, tailnet.SeedInput{
		Enabled:    true,
		Hostname:   "second-boot-env",
		ControlURL: "https://elsewhere.example.com",
		ServerName: "Living Room",
	})
	if err != nil || seeded {
		t.Fatalf("second boot: seeded = %v, err = %v; want false, nil", seeded, err)
	}
	if s.writes != writesBefore {
		t.Errorf("second boot wrote %d time(s); the DB is authoritative and must not be touched", s.writes-writesBefore)
	}
	if got := *s.row; got.Hostname != "chosen-in-ui" || got.Enabled || got.ControlURL != "" {
		t.Errorf("row after second boot = %+v, want the UI's values untouched", got)
	}
}

// TestSeedIfEmptyPropagatesReadFailure: a store that cannot answer "have these
// settings ever been written" must not be guessed at — seeding on a false
// negative would overwrite an operator's settings from stale env vars.
func TestSeedIfEmptyPropagatesReadFailure(t *testing.T) {
	boom := errors.New("disk on fire")
	s := &seedStore{err: boom}
	if _, err := tailnet.SeedIfEmpty(s, tailnet.SeedInput{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if s.writes != 0 {
		t.Errorf("wrote %d row(s) despite an unreadable store", s.writes)
	}
}

// TestSeedHostname pins the derivation: the operator's name wins, else the
// Server's display name, each sanitized to a legal DNS label by discovery's
// sanitizer (the SAME one that makes the .local name), falling back to "obelo".
func TestSeedHostname(t *testing.T) {
	cases := []struct {
		name       string
		hostname   string
		serverName string
		want       string
	}{
		{"explicit wins", "attic", "Living Room", "attic"},
		{"falls back to the server name", "", "Living Room", "Living-Room"},
		{"sanitizes the explicit name", "Brandon's Box!", "ignored", "Brandon-s-Box"},
		{"nothing usable anywhere", "", "", "obelo"},
		{"a name that sanitizes to nothing", "...", "!!!", "obelo"},
		{"keeps only the first label", "nuc.lan", "", "nuc"},
		{"caps at the 63-octet label limit", strings.Repeat("a", 80), "", strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailnet.SeedHostname(tc.hostname, tc.serverName); got != tc.want {
				t.Errorf("SeedHostname(%q, %q) = %q, want %q", tc.hostname, tc.serverName, got, tc.want)
			}
		})
	}
}
