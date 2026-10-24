package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Tests for replacesSessionId (ADR-0071 §5): a negotiation may name the caller's own
// session, which the stream-limit and transcode-cap checks then count as already
// gone — atomically, so a refused swap leaves the old session intact.

func replacingInput(user, replaces string) CreateInput {
	return CreateInput{UserID: user, DeviceID: "d1", ReplacesSessionID: replaces}
}

func TestReplacesSessionSwapsTheTranscodeSlot(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)
	s1, err := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatalf("first transcode: %v", err)
	}
	s2, err := m.CreateGoverned(replacingInput("u1", s1.ID), transcodeDecision("b"))
	if err != nil {
		t.Fatalf("replacing transcode refused at a cap its own predecessor fills: %v", err)
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d right after the swap, want 1", got)
	}
	if !m.End(s1.ID) {
		t.Fatal("the replaced session was gone before its DELETE")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after the old session's DELETE, want 1 (never 2, never 0)", got)
	}
	m.End(s2.ID)
	if got := m.ActiveTranscodes(); got != 0 {
		t.Errorf("activeTranscodes = %d after both ended, want 0 (never negative)", got)
	}
}

func TestWithoutReplacesSessionTheCapStillRefuses(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateGoverned(replacingInput("u1", ""), transcodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("err = %v, want ErrTranscodeCapFull", err)
	}
}

func TestReplacesSessionSwapsTheStreamLimit(t *testing.T) {
	m := NewManager()
	s1, err := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1", MaxStreams: 1}, Decision{Tier: TierDirectPlay})
	if err != nil {
		t.Fatal(err)
	}
	in := CreateInput{UserID: "u1", DeviceID: "d1", MaxStreams: 1}
	if _, err := m.CreateGoverned(in, Decision{Tier: TierDirectPlay}); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("without replaces: err = %v, want ErrStreamLimit", err)
	}
	if err := m.CheckStreamLimit("u1", 1); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("pre-check without replaces: err = %v, want ErrStreamLimit", err)
	}
	if err := m.CheckStreamLimitReplacing("u1", "d1", 1, s1.ID); err != nil {
		t.Errorf("pre-check naming the caller's own session refused: %v", err)
	}
	in.ReplacesSessionID = s1.ID
	if _, err := m.CreateGoverned(in, Decision{Tier: TierDirectPlay}); err != nil {
		t.Fatalf("with replaces: %v", err)
	}
	// The old one stays counted-out for the stream limit while it is in grace.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1", MaxStreams: 1}, Decision{Tier: TierDirectPlay}); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("a third stream got past maxStreams=1: err = %v", err)
	}
}

func TestReplacesAnotherUsersSessionIsTreatedAsAbsent(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(1)
	theirs, err := m.CreateGoverned(CreateInput{UserID: "u2", DeviceID: "d1"}, transcodeDecision("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateGoverned(replacingInput("u1", theirs.ID), transcodeDecision("b")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("err = %v, want ErrTranscodeCapFull (the id must not free another User's slot)", err)
	}
	if _, ok := m.Get(theirs.ID); !ok {
		t.Error("the other User's session was ended")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d, want 1", got)
	}
	// Same User, other Device: also not the caller's own session.
	own := replacingInput("u2", theirs.ID)
	own.DeviceID = "d2"
	if _, err := m.CreateGoverned(own, transcodeDecision("c")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Errorf("another Device's session was replaced: err = %v", err)
	}
	// An unknown id is absent, not an error.
	if _, err := m.CreateGoverned(replacingInput("u1", "nope"), Decision{Tier: TierDirectPlay}); err != nil {
		t.Errorf("unknown replacesSessionId errored: %v", err)
	}
}

func TestRefusedReplacementLeavesTheOldSessionIntact(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(2)
	s1, _ := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a"))
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2", DeviceID: "d1"}, transcodeDecision("b")); err != nil {
		t.Fatal(err)
	}
	// Cap 2 is full by s1 + another's. Replacing s1 with a DIRECT-PLAY-free
	// transcode frees s1's slot, so shrink the cap to make it genuinely full.
	m.SetTranscodeCap(1)
	if _, err := m.CreateGoverned(replacingInput("u1", s1.ID), transcodeDecision("c")); !errors.Is(err, ErrTranscodeCapFull) {
		t.Fatalf("err = %v, want ErrTranscodeCapFull", err)
	}
	got, ok := m.Get(s1.ID)
	if !ok {
		t.Fatal("refused replacement ended the old session")
	}
	if got.replaced || got.slotFreed {
		t.Error("refused replacement marked the old session replaced")
	}
	if n := m.ActiveTranscodes(); n != 2 {
		t.Errorf("activeTranscodes = %d, want 2 (s1 still counted)", n)
	}
	// And it still counts for the stream limit.
	if _, err := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1", MaxStreams: 1}, Decision{Tier: TierDirectPlay}); !errors.Is(err, ErrStreamLimit) {
		t.Errorf("s1 stopped counting after a refused swap: err = %v", err)
	}
}

func TestReplacedSessionEndsAfterGrace(t *testing.T) {
	m := NewManager()
	m.SetReplaceGrace(30 * time.Millisecond)
	m.SetTranscodeCap(1)
	s1, _ := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a"))
	s2, err := m.CreateGoverned(replacingInput("u1", s1.ID), transcodeDecision("b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(s1.ID); !ok {
		t.Fatal("the replaced session did not survive its grace period")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := m.Get(s1.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replaced session was never ended after the grace period")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := m.Get(s2.ID); !ok {
		t.Error("the replacement was ended with it")
	}
	if got := m.ActiveTranscodes(); got != 1 {
		t.Errorf("activeTranscodes = %d after grace expiry, want 1", got)
	}
}

func TestDeleteDuringGraceEndsOnceWithoutDoubleRelease(t *testing.T) {
	m := NewManager()
	m.SetTranscodeCap(2)
	s1, _ := m.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a"))
	if _, err := m.CreateGoverned(CreateInput{UserID: "u2", DeviceID: "d1"}, transcodeDecision("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateGoverned(replacingInput("u1", s1.ID), transcodeDecision("b")); err != nil {
		t.Fatal(err)
	}
	if !m.End(s1.ID) {
		t.Fatal("DELETE during grace found no session")
	}
	if m.End(s1.ID) {
		t.Error("the replaced session ended twice")
	}
	if got := m.ActiveTranscodes(); got != 2 {
		t.Errorf("activeTranscodes = %d, want 2 (u2's + the replacement; no double release)", got)
	}
	// Reap of a replaced session also must not release its slot again.
	m2 := NewManager()
	m2.SetTranscodeCap(2)
	a, _ := m2.CreateGoverned(CreateInput{UserID: "u1", DeviceID: "d1"}, transcodeDecision("a"))
	m2.CreateGoverned(CreateInput{UserID: "u2", DeviceID: "d1"}, transcodeDecision("x"))
	m2.CreateGoverned(replacingInput("u1", a.ID), transcodeDecision("b"))
	m2.SetNow(func() time.Time { return time.Now().Add(time.Hour) })
	// Touch the two live ones so only the replaced one is idle.
	m2.mu.Lock()
	for id, s := range m2.sessions {
		if id != a.ID {
			s.LastSeen = time.Now().Add(time.Hour)
			m2.sessions[id] = s
		}
	}
	m2.mu.Unlock()
	if n := m2.Reap(time.Minute); n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if got := m2.ActiveTranscodes(); got != 2 {
		t.Errorf("activeTranscodes = %d after reaping the replaced session, want 2", got)
	}
}

// replacingRelay records the replacesSessionId the sharer is sent and hands out a
// distinct sharer session per play.
type replacingRelay struct {
	*markerRelay
	n        int
	replaces []string
}

func (r *replacingRelay) RelayNegotiate(_ context.Context, req RelayRequest) (RelayAnswer, error) {
	r.n++
	r.replaces = append(r.replaces, req.ReplacesSessionID)
	return RelayAnswer{
		LinkID: "l1", RemoteSessionID: "rs" + string(rune('0'+r.n)), RemoteTitleID: "rt1", RemoteEditionID: "re1",
		Tier: TierDirectPlay, Decision: map[string]any{},
	}, nil
}

func TestRelayForwardsTheSharersSessionIDAsReplaces(t *testing.T) {
	f := mp4File(1080, 6_000_000)
	f.Path = ""
	st := relayMarkerStore{&markerStore{ceilingStore: ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{f}})}}}
	svc := NewService(st, nil, "", Governance{})
	r := &replacingRelay{markerRelay: &markerRelay{}}
	svc.SetRelay(r)
	req := Request{
		UserID: "u1", DeviceID: "d1", TitleID: "t1", Profile: uhdProfile(),
		Constraints: Constraints{MaxResolution: "2160p", MaxBitrate: 100_000_000},
		Scope:       access.Scope{AllLibraries: true, MaxStreams: 1},
	}
	first := mustNegotiate(t, svc, req)
	req.ReplacesSessionID = first.ID
	mustNegotiate(t, svc, req)
	if len(r.replaces) != 2 || r.replaces[0] != "" || r.replaces[1] != "rs1" {
		t.Errorf("sharer was sent replacesSessionId %q, want [\"\" \"rs1\"] (the sharer-side id, not the local one)", r.replaces)
	}
	// A local id that is not the caller's own is never forwarded.
	req.UserID = "u9"
	req.ReplacesSessionID = first.ID
	mustNegotiate(t, svc, req)
	if got := r.replaces[2]; got != "" {
		t.Errorf("another User's session id was forwarded as %q", got)
	}
}
