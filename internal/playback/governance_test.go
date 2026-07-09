package playback

import (
	"errors"
	"testing"
	"time"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the transcode-governance accounting (ADR-0009): only the
// transcode tier consumes a cap slot, the cap rejects (never queues) at the
// limit, and ending/reaping a transcode frees its slot so a previously-rejected
// transcode can then succeed. These exercise the Manager directly (no ffmpeg);
// the real-ffmpeg end-to-end governance check lives in the api integration test.

func transcodeDecision(id string) Decision {
	return Decision{
		Tier:             TierTranscode,
		Edition:          store.Edition{ID: "e-" + id},
		File:             store.File{ID: "f-" + id, Path: "/movies/" + id + ".mkv", Bitrate: 8_000_000},
		EstimatedBitrate: 8_000_000,
	}
}

// TestTranscodeCapRejectsAtLimit: with a cap of 1, the first transcode takes the
// slot and the second is rejected with ErrTranscodeCapFull WITHOUT creating a
// session — reject-don't-queue.
func TestTranscodeCapRejectsAtLimit(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	s1, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatalf("first transcode: unexpected err %v", err)
	}
	if s1.ID == "" {
		t.Fatal("first transcode created no session")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after 1, want 1", got)
	}

	s2, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b"))
	if !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("second transcode err = %v, want ErrTranscodeCapFull", err)
	}
	if s2.ID != "" {
		t.Error("rejected transcode still created a session")
	}
	// The rejection must not have leaked a slot or a map entry.
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after a rejection, want 1 (no leak)", got)
	}
	if got := m.Count(); got != 1 {
		t.Errorf("session count = %d after a rejection, want 1", got)
	}
}

// TestDirectPlayAndRemuxAreUnmetered: neither direct play nor remux consumes a
// slot or hits the cap, even when the cap is 1 and exhausted by a transcode.
func TestDirectPlayAndRemuxAreUnmetered(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	// Fill the single transcode slot.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}

	// Direct play and remux still succeed at the cap and never increment the count.
	dp := Decision{Tier: TierDirectPlay, Edition: store.Edition{ID: "e2"}, File: store.File{ID: "f2", Path: "/m/dp.mp4"}}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, dp); err != nil {
		t.Errorf("direct play rejected at transcode cap: %v", err)
	}
	rm := Decision{Tier: TierDirectStream, Edition: store.Edition{ID: "e3"}, File: store.File{ID: "f3", Path: "/m/rm.mkv"}}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u3"}, rm); err != nil {
		t.Errorf("remux rejected at transcode cap: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d, want 1 (only the transcode counts)", got)
	}
	if got := m.Count(); got != 3 {
		t.Errorf("session count = %d, want 3 (transcode + directPlay + remux)", got)
	}
}

// TestEndFreesTranscodeSlot: ending the transcode that holds the only slot lets
// a previously-rejected transcode through.
func TestEndFreesTranscodeSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	s1, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatalf("first transcode: %v", err)
	}
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("second transcode err = %v, want ErrTranscodeCapFull", err)
	}

	// Free the slot.
	if !m.End(s1.ID) {
		t.Fatal("End returned false for the live transcode")
	}
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after End, want 0 (slot freed)", got)
	}

	// A new transcode now succeeds.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u3"}, transcodeDecision("c")); err != nil {
		t.Errorf("transcode after freed slot rejected: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d, want 1", got)
	}
}

// TestReapFreesTranscodeSlot: reaping an idle transcode frees its slot exactly
// as a clean End would, so an abandoned transcode never permanently holds the cap.
func TestReapFreesTranscodeSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)

	base := time.Now()
	m.SetNow(func() time.Time { return base })
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}

	// Advance the clock past the idle window and reap.
	m.SetNow(func() time.Time { return base.Add(time.Hour) })
	if n := m.Reap(time.Minute); n != 1 {
		t.Fatalf("Reap swept %d, want 1", n)
	}
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after reap, want 0", got)
	}
	// A new transcode now fits.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2"}, transcodeDecision("b")); err != nil {
		t.Errorf("transcode after reap rejected: %v", err)
	}
}

// TestEndingDirectPlayDoesNotTouchSlot: ending a direct-play session never
// decrements the transcode counter (it never incremented it).
func TestEndingDirectPlayDoesNotTouchSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(2)

	if _, err := m.CreateGoverned(CreateInput{UserID: "u1"}, transcodeDecision("a")); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	dp := Decision{Tier: TierDirectPlay, Edition: store.Edition{ID: "e2"}, File: store.File{ID: "f2", Path: "/m/dp.mp4"}}
	dpSess, err := m.CreateGoverned(CreateInput{UserID: "u2"}, dp)
	if err != nil {
		t.Fatalf("direct play: %v", err)
	}

	if !m.End(dpSess.ID) {
		t.Fatal("End(directPlay) returned false")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after ending a direct-play session, want 1 (unchanged)", got)
	}
}

// TestUnlimitedCapNeverRejects: a cap of 0 means unlimited — transcodes are
// still metered (for observability) but never rejected.
func TestUnlimitedCapNeverRejects(t *testing.T) {
	m := NewManager() // cap defaults to 0 (unlimited)
	for i := 0; i < 5; i++ {
		if _, err := m.CreateGoverned(CreateInput{UserID: "u"}, transcodeDecision(string(rune('a'+i)))); err != nil {
			t.Fatalf("transcode %d rejected under unlimited cap: %v", i, err)
		}
	}
	if got := m.ActiveTranscodes(); got != 5 {
		t.Errorf("activeTranscodes = %d, want 5 (metered even when unlimited)", got)
	}
}

// TestSuggestBusyBitrate exercises the pure suggestedMaxBitrate heuristic
// (ADR-0009 "suggested lower bitrate"): half the estimate, floored, always a
// real step down, with sensible fallbacks when inputs are missing.
func TestSuggestBusyBitrate(t *testing.T) {
	tests := []struct {
		name      string
		estimated int64
		requested int64
		want      int64
	}{
		{"half of estimate", 8_000_000, 0, 4_000_000},
		{"falls back to requested when no estimate", 0, 6_000_000, 3_000_000},
		{"floor when estimate is low", 1_000_000, 0, busyBitrateFloor},
		{"floor when nothing known", 0, 0, busyBitrateFloor},
		{"prefers estimate over requested", 10_000_000, 2_000_000, 5_000_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := suggestBusyBitrate(tc.estimated, tc.requested)
			if got != tc.want {
				t.Errorf("suggestBusyBitrate(%d, %d) = %d, want %d", tc.estimated, tc.requested, got, tc.want)
			}
			// Invariant: the suggestion must be a genuine step down from the base it
			// derived from (or the floor), so the client does not loop on busy.
			base := tc.estimated
			if base <= 0 {
				base = tc.requested
			}
			if base > 0 && got >= base && got != busyBitrateFloor {
				t.Errorf("suggestion %d is not below base %d", got, base)
			}
		})
	}
}
