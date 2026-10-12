// Package bglane is the one background lane the Server's heavy idle-time jobs
// share (ADR-0070 §3): Seek preview generation and Marker detection. At most one
// holds it at a time, and when it frees the waiter with the lowest priority number
// takes it, so Seek previews (PrioritySeekPreview) go ahead of Markers
// (PriorityMarkers). Holding the lane is not a Transcode and takes no slot of the
// transcode cap (ADR-0009); a holder still waits out Transcodes itself.
package bglane

import (
	"context"
	"sync"
)

// The priorities, lowest first.
const (
	PrioritySeekPreview = 0
	PriorityMarkers     = 1
)

type waiter struct {
	prio  int
	seq   uint64
	ready chan struct{}
}

// Lane serializes its holders. The zero value is a free lane.
type Lane struct {
	mu      sync.Mutex
	held    bool
	seq     uint64
	waiting []*waiter
	claims  map[int]int
}

// Acquire blocks until the lane is this caller's, or ctx ends. The returned
// release must be called exactly once.
func (l *Lane) Acquire(ctx context.Context, prio int) (release func(), err error) {
	l.mu.Lock()
	if !l.held && l.eligible(prio) {
		l.held = true
		l.mu.Unlock()
		return l.release, nil
	}
	l.seq++
	w := &waiter{prio: prio, seq: l.seq, ready: make(chan struct{})}
	l.waiting = append(l.waiting, w)
	l.mu.Unlock()
	select {
	case <-w.ready:
		return l.release, nil
	case <-ctx.Done():
		l.mu.Lock()
		for i, q := range l.waiting {
			if q == w {
				l.waiting = append(l.waiting[:i], l.waiting[i+1:]...)
				l.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		l.mu.Unlock()
		// Granted in the same instant ctx ended: hand it straight on.
		l.release()
		return nil, ctx.Err()
	}
}

func (l *Lane) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held = false
	l.grant()
}

// Claim says a holder of this priority has work queued and will be back for the
// lane after each turn. While any claim is held, a waiter of a LOWER priority
// (a higher number) is not granted the lane, even in the instant it is free
// between two turns of the claimant: every claimed job goes before them. The
// returned func ends the claim.
func (l *Lane) Claim(prio int) (unclaim func()) {
	l.mu.Lock()
	if l.claims == nil {
		l.claims = map[int]int{}
	}
	l.claims[prio]++
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.claims[prio]--; l.claims[prio] <= 0 {
			delete(l.claims, prio)
		}
		l.grant()
	}
}

// eligible reports whether a waiter of this priority may take the lane: no
// claim is held at a better priority.
func (l *Lane) eligible(prio int) bool {
	for p := range l.claims {
		if p < prio {
			return false
		}
	}
	return true
}

// grant hands a free lane to the best eligible waiter, if there is one.
func (l *Lane) grant() {
	if l.held {
		return
	}
	best := -1
	for i, w := range l.waiting {
		if !l.eligible(w.prio) {
			continue
		}
		if best < 0 || w.prio < l.waiting[best].prio {
			best = i
		}
	}
	if best < 0 {
		return
	}
	w := l.waiting[best]
	l.waiting = append(l.waiting[:best], l.waiting[best+1:]...)
	l.held = true
	close(w.ready)
}
