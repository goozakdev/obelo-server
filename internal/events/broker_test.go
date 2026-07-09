package events

import (
	"testing"
	"time"
)

// admin is a convenience Identity for tests that don't care about gating: an
// Admin sees every event (broadcast, admin-only, and any library-scoped).
var admin = Identity{UserID: "admin", IsAdmin: true}

// A published event reaches a live subscriber.
func TestBrokerPublishReachesSubscriber(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	ch, cancel := b.Subscribe(admin)
	defer cancel()

	b.PublishEnrichProgress(EnrichProgress{LibraryID: "lib1", Total: 3, Done: 1})

	select {
	case e := <-ch:
		if e.Type != TypeEnrichProgress {
			t.Fatalf("event type = %q, want %q", e.Type, TypeEnrichProgress)
		}
		p, ok := e.Data.(EnrichProgress)
		if !ok || p.LibraryID != "lib1" || p.Total != 3 || p.Done != 1 {
			t.Fatalf("payload = %+v", e.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

// After cancel, a subscriber no longer receives events and its channel is closed.
func TestBrokerUnsubscribe(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	ch, cancel := b.Subscribe(admin)
	cancel()

	// The channel is closed (drained → zero value, ok=false).
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after cancel")
	}

	// Publishing after cancel does not panic (the subscriber is gone).
	b.PublishEnrichProgress(EnrichProgress{LibraryID: "lib1"})
}

// Publish never blocks even when a subscriber's buffer is full (slow consumer).
func TestBrokerPublishNonBlocking(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	_, cancel := b.Subscribe(admin) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ { // far more than the 32-deep buffer
			b.PublishEnrichProgress(EnrichProgress{LibraryID: "lib1", Done: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
}

// Close closes every subscriber channel so their SSE handlers return.
func TestBrokerClose(t *testing.T) {
	b := NewBroker()
	ch, _ := b.Subscribe(admin)
	b.Close()

	if _, ok := <-ch; ok {
		t.Fatal("Close should close subscriber channels")
	}
	// Subscribing after Close yields an already-closed channel.
	ch2, _ := b.Subscribe(admin)
	if _, ok := <-ch2; ok {
		t.Fatal("Subscribe after Close should return a closed channel")
	}
}

// TestAudienceMatches is the focused unit test of the gating predicate — the
// heart of the new mechanism. broadcast reaches everyone; admin-only reaches
// only Admins; library-scoped reaches a subscriber whose accessible set contains
// the id, and any Admin (who sees all Libraries).
func TestAudienceMatches(t *testing.T) {
	adminID := Identity{UserID: "a", IsAdmin: true}
	// A Member who can see lib1 but not lib2.
	member := Identity{UserID: "m", Libraries: map[string]struct{}{"lib1": {}}}
	// A Member with no accessible Libraries (e.g. resolution failed / none granted).
	noLibs := Identity{UserID: "n"}

	broadcast := Audience{Kind: AudienceBroadcast}
	adminOnly := Audience{Kind: AudienceAdmin}
	lib1 := Audience{Kind: AudienceLibrary, LibraryID: "lib1"}
	lib2 := Audience{Kind: AudienceLibrary, LibraryID: "lib2"}

	cases := []struct {
		name string
		aud  Audience
		id   Identity
		want bool
	}{
		{"broadcast→admin", broadcast, adminID, true},
		{"broadcast→member", broadcast, member, true},
		{"broadcast→noLibs", broadcast, noLibs, true},

		{"adminOnly→admin", adminOnly, adminID, true},
		{"adminOnly→member", adminOnly, member, false},
		{"adminOnly→noLibs", adminOnly, noLibs, false},

		{"lib1→admin", lib1, adminID, true},               // Admin sees all Libraries
		{"lib1→member with lib1", lib1, member, true},     // in accessible set
		{"lib2→member without lib2", lib2, member, false}, // not in accessible set
		{"lib1→noLibs", lib1, noLibs, false},              // empty/nil set matches nothing
	}
	for _, tc := range cases {
		if got := tc.aud.matches(tc.id); got != tc.want {
			t.Errorf("%s: matches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPublishGatesByAudience proves the gate at the Publish seam: an admin-only
// event reaches an Admin subscriber but NOT a Member subscriber, while a
// broadcast event reaches both. This is the security-relevant property —
// absence on the Member channel.
func TestPublishGatesByAudience(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	adminCh, cancelA := b.Subscribe(Identity{UserID: "a", IsAdmin: true})
	defer cancelA()
	memberCh, cancelM := b.Subscribe(Identity{UserID: "m"})
	defer cancelM()

	// An admin-only event: only the Admin channel should carry it.
	b.Publish(Event{Type: "sessionStarted", Audience: Audience{Kind: AudienceAdmin}})
	// A broadcast event right after: both channels carry it. Receiving the
	// broadcast on the Member channel without first seeing the admin-only event
	// proves the admin-only event was filtered out (channels preserve order).
	b.Publish(Event{Type: TypeEnrichProgress, Audience: Audience{Kind: AudienceBroadcast}})

	if e := recvWithin(t, adminCh); e.Type != "sessionStarted" {
		t.Fatalf("admin first event = %q, want sessionStarted", e.Type)
	}
	if e := recvWithin(t, adminCh); e.Type != TypeEnrichProgress {
		t.Fatalf("admin second event = %q, want %q", e.Type, TypeEnrichProgress)
	}
	if e := recvWithin(t, memberCh); e.Type != TypeEnrichProgress {
		t.Fatalf("member first event = %q, want %q (admin-only must be filtered)", e.Type, TypeEnrichProgress)
	}
}

func recvWithin(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}
