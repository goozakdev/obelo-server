package onlinesource

import (
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/playback"
)

// TestTheReaperKillsARunningFFmpegOfAnIdleSession: an ffmpeg that never exits on its
// own (the fake job blocks until it is killed) belongs to a session nobody is
// watching. Before the idle timeout nothing is touched, and a keepalive resets the
// clock; after it, the reaper stops the process, removes the scratch directory and
// releases the cap slot, so the slot does not stay taken by an abandoned encode.
func TestTheReaperKillsARunningFFmpegOfAnIdleSession(t *testing.T) {
	const idle = 5 * time.Minute
	rig := newFFmpegRig(t, splitV())
	var clockMu sync.Mutex
	clock := time.Now()
	rig.svc.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	advance := func(d time.Duration) { clockMu.Lock(); clock = clock.Add(d); clockMu.Unlock() }

	sess, _, err := rig.play(t, playback.Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	job := rig.runner.jobs[0]
	killed := func() bool {
		select {
		case <-job.killed:
			return true
		default:
			return false
		}
	}
	if rig.slots.heldNow() != 1 || killed() || len(scratchEntries(t, rig.scratch)) == 0 {
		t.Fatalf("running encode: slots=%d killed=%v scratch=%v; want 1 slot, running, with files",
			rig.slots.heldNow(), killed(), scratchEntries(t, rig.scratch))
	}

	// Within the timeout: nothing is reaped, and a keepalive moves the deadline.
	advance(idle - time.Second)
	if n := rig.svc.Reap(idle); n != 0 || killed() {
		t.Fatalf("Reap before the timeout = %d, killed=%v; want 0, running", n, killed())
	}
	rig.svc.Touch(sess.ID)
	advance(idle - time.Second)
	if n := rig.svc.Reap(idle); n != 0 || killed() {
		t.Fatalf("Reap after a keepalive = %d, killed=%v; want 0, running", n, killed())
	}

	// Idle past the timeout: reaped, with everything the encode held.
	advance(2 * time.Second)
	if n := rig.svc.Reap(idle); n != 1 {
		t.Fatalf("Reap after the timeout = %d, want 1", n)
	}
	if !killed() {
		t.Fatal("the running ffmpeg was not stopped")
	}
	if got := scratchEntries(t, rig.scratch); len(got) != 0 {
		t.Fatalf("scratch left after the reap: %v", got)
	}
	if rig.slots.heldNow() != 0 {
		t.Fatalf("%d cap slots still held after the reap", rig.slots.heldNow())
	}
	if _, live := rig.svc.Session(sess.ID); live {
		t.Fatal("the reaped session is still live")
	}
}
