package onlinesource

import (
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

const accessRemovedMessage = "You no longer have access to Test Tube"

// TestRevokingAccessLeavesANoteThePlayerReads: a session ended because the User may
// no longer see its source leaves the same kind of short-lived note a gone video
// does, so the player can say why the stream stopped; only the session's own User
// can read it, and a User who still has access keeps their session and gets none.
func TestRevokingAccessLeavesANoteThePlayerReads(t *testing.T) {
	m := newMediaHost(t, 200)
	svc, _ := m.service(t, func(int) ([]pluginapi.OnlineVariant, error) {
		return []pluginapi.OnlineVariant{m.variant("/new.mp4")}, nil
	})
	allowed := true
	svc.SetAccess(func(userID, sourceID string) bool { return allowed })
	sess := playSeq(t, svc)

	if n := svc.Revalidate("u1"); n != 0 {
		t.Fatalf("Revalidate with access = %d, want 0", n)
	}
	if svc.HasGone(sess.ID) {
		t.Fatal("a session that kept its access left a note")
	}

	allowed = false
	if n := svc.Revalidate("u1"); n != 1 {
		t.Fatalf("Revalidate after revocation = %d, want 1", n)
	}
	if _, live := svc.Session(sess.ID); live {
		t.Fatal("the session is still live after revocation")
	}
	if !svc.HasGone(sess.ID) {
		t.Fatal("no note was left for the revoked session")
	}
	msg, ok := svc.Gone(sess.ID, "u1")
	if !ok || msg != accessRemovedMessage {
		t.Fatalf("Gone = %q, %v; want %q", msg, ok, accessRemovedMessage)
	}
	if _, ok := svc.Gone(sess.ID, "someone-else"); ok {
		t.Fatal("another User can read the revoked session's note")
	}
}
