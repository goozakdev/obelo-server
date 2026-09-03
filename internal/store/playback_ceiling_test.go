package store_test

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The Playback-ceiling columns added by migration 0059 (ADR-0054 §2): three
// nullable knobs beside rating_ceiling, whose UNSET state must read back as the
// zero value so every pre-existing User stays uncapped.

func TestPlaybackCeilingDefaultsToUncapped(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','x')`)

	got, err := db.PlaybackCeilingForUser("u1")
	if err != nil {
		t.Fatalf("PlaybackCeilingForUser: %v", err)
	}
	if got != (store.PlaybackCeiling{}) {
		t.Errorf("fresh User's ceiling = %+v, want the zero (uncapped) value", got)
	}
}

func TestPlaybackCeilingRoundTripsAndClears(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO users (id, username, role, password_hash) VALUES ('u1','u1','member','x')`)

	want := store.PlaybackCeiling{MaxResolution: "1080p", MaxBitrate: 8_000_000, MaxStreams: 2}
	if err := db.SetPlaybackCeiling("u1", want); err != nil {
		t.Fatalf("SetPlaybackCeiling: %v", err)
	}
	got, err := db.PlaybackCeilingForUser("u1")
	if err != nil {
		t.Fatalf("PlaybackCeilingForUser: %v", err)
	}
	if got != want {
		t.Errorf("round-tripped ceiling = %+v, want %+v", got, want)
	}

	// The write is a REPLACE of all three: the zero value clears every column back
	// to NULL, rather than leaving a stale cap behind.
	if err := db.SetPlaybackCeiling("u1", store.PlaybackCeiling{}); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	got, _ = db.PlaybackCeilingForUser("u1")
	if got != (store.PlaybackCeiling{}) {
		t.Errorf("after clear = %+v, want uncapped", got)
	}

	// A partial ceiling leaves the others uncapped (each column is independent).
	if err := db.SetPlaybackCeiling("u1", store.PlaybackCeiling{MaxStreams: 3}); err != nil {
		t.Fatalf("partial set: %v", err)
	}
	got, _ = db.PlaybackCeilingForUser("u1")
	if got != (store.PlaybackCeiling{MaxStreams: 3}) {
		t.Errorf("partial ceiling = %+v, want only MaxStreams 3", got)
	}
}

func TestPlaybackCeilingUnknownUser(t *testing.T) {
	db := openTemp(t)
	if _, err := db.PlaybackCeilingForUser("nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("read for an unknown User = %v, want ErrNotFound", err)
	}
	if err := db.SetPlaybackCeiling("nobody", store.PlaybackCeiling{MaxStreams: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("write for an unknown User = %v, want ErrNotFound", err)
	}
}
