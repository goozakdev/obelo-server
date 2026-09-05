package access

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The per-User Playback ceiling's resolution and validation (ADR-0054 §2,
// .scratch/linked-servers issue 02). The ENFORCEMENT lives in playback; what
// this package owns is the settable vocabulary and putting the three numbers on
// the Scope every request already carries.

// TestResolveCarriesPlaybackCeiling: a Member's stored ceiling rides on the
// resolved Scope alongside the grants and the Rating ceiling, so requireScope
// needs no new call.
func TestResolveCarriesPlaybackCeiling(t *testing.T) {
	fs := newFake()
	fs.play["m"] = store.PlaybackCeiling{MaxResolution: "1080p", MaxBitrate: 8_000_000, MaxStreams: 2}
	sc, err := NewService(fs).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sc.MaxResolution != "1080p" || sc.MaxBitrate != 8_000_000 || sc.MaxStreams != 2 {
		t.Errorf("scope ceiling = %q/%d/%d, want 1080p/8000000/2",
			sc.MaxResolution, sc.MaxBitrate, sc.MaxStreams)
	}
}

// TestResolveUncappedIsZero: an unset ceiling resolves to the zero value in all
// three dimensions — uncapped, which is what every existing User must stay.
func TestResolveUncappedIsZero(t *testing.T) {
	sc, err := NewService(newFake()).Resolve("m")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sc.MaxResolution != "" || sc.MaxBitrate != 0 || sc.MaxStreams != 0 {
		t.Errorf("uncapped scope = %+v, want zero ceiling fields", sc)
	}
}

// TestResolveAdminCarriesNoCeiling: an Admin resolves all-access by role and is
// never capped, even with rows in the table (the endpoint refuses to write them,
// but the resolver must not depend on that).
func TestResolveAdminCarriesNoCeiling(t *testing.T) {
	fs := newFake()
	fs.play["a"] = store.PlaybackCeiling{MaxResolution: "720p", MaxStreams: 1}
	sc, err := NewService(fs).Resolve("a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sc.MaxResolution != "" || sc.MaxStreams != 0 {
		t.Errorf("admin scope = %+v, want an uncapped ceiling", sc)
	}
}

// TestSetPlaybackCeilingStoresAndCanonicalizes: a settable rung is stored (folded
// to its canonical spelling so the negotiator's clamp never has to), and the
// whole ceiling round-trips through the read accessor.
func TestSetPlaybackCeilingStoresAndCanonicalizes(t *testing.T) {
	fs := newFake()
	svc := NewService(fs)
	if err := svc.SetPlaybackCeiling("m", store.PlaybackCeiling{
		MaxResolution: " 1080P ", MaxBitrate: 8_000_000, MaxStreams: 2,
	}); err != nil {
		t.Fatalf("SetPlaybackCeiling: %v", err)
	}
	got, err := svc.PlaybackCeiling("m")
	if err != nil {
		t.Fatalf("PlaybackCeiling: %v", err)
	}
	if got.MaxResolution != "1080p" || got.MaxBitrate != 8_000_000 || got.MaxStreams != 2 {
		t.Errorf("stored ceiling = %+v, want 1080p/8000000/2", got)
	}
}

// TestSetPlaybackCeilingClears: the zero value is a REPLACE that clears every
// dimension — the ceiling is one setting with three knobs, not three settings.
func TestSetPlaybackCeilingClears(t *testing.T) {
	fs := newFake()
	svc := NewService(fs)
	if err := svc.SetPlaybackCeiling("m", store.PlaybackCeiling{MaxResolution: "720p", MaxStreams: 1}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.SetPlaybackCeiling("m", store.PlaybackCeiling{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ := svc.PlaybackCeiling("m")
	if got != (store.PlaybackCeiling{}) {
		t.Errorf("after clear = %+v, want the zero (uncapped) ceiling", got)
	}
}

// TestSetPlaybackCeilingRefusals: every guard on the write, each mapped to its
// own error so the api can answer with the right code.
func TestSetPlaybackCeilingRefusals(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		ceiling store.PlaybackCeiling
		want    error
	}{
		{"unknown user", "nobody", store.PlaybackCeiling{MaxStreams: 1}, ErrUserNotFound},
		{"an admin is uncapped by definition", "a", store.PlaybackCeiling{MaxResolution: "1080p"}, ErrAdminCeiling},
		{"a rung we do not offer", "m", store.PlaybackCeiling{MaxResolution: "1440p"}, ErrUnknownResolution},
		{"a raw height is not a rung", "m", store.PlaybackCeiling{MaxResolution: "1080"}, ErrUnknownResolution},
		{"negative bitrate", "m", store.PlaybackCeiling{MaxBitrate: -1}, ErrInvalidCeiling},
		{"negative streams", "m", store.PlaybackCeiling{MaxStreams: -1}, ErrInvalidCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFake()
			err := NewService(fs).SetPlaybackCeiling(tc.user, tc.ceiling)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// A refused write leaves nothing behind.
			if c, ok := fs.play[tc.user]; ok && c != (store.PlaybackCeiling{}) {
				t.Errorf("a refused write stored %+v", c)
			}
		})
	}
}

// TestSettableResolutions pins the vocabulary the endpoint validates against: the
// three rungs an operator picks from, case-folded, and nothing else.
func TestSettableResolutions(t *testing.T) {
	for _, ok := range []string{"720p", "1080p", "2160p", "2160P"} {
		if _, settable := canonicalResolution(ok); !settable {
			t.Errorf("canonicalResolution(%q) = not settable, want settable", ok)
		}
	}
	for _, bad := range []string{"", "4k", "480p", "1440p", "hd", "uhd", "nonsense"} {
		if _, settable := canonicalResolution(bad); settable {
			t.Errorf("canonicalResolution(%q) = settable, want refused", bad)
		}
	}
}
