package bglane

import (
	"context"
	"testing"
	"time"
)

func TestLaneGrantsLowestPriorityNumberFirst(t *testing.T) {
	var l Lane
	ctx := context.Background()
	rel, err := l.Acquire(ctx, PriorityMarkers)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 2)
	start := func(prio int) {
		go func() {
			r, err := l.Acquire(ctx, prio)
			if err != nil {
				return
			}
			order <- prio
			r()
		}()
		time.Sleep(20 * time.Millisecond) // let it queue
	}
	start(PriorityMarkers)
	start(PrioritySeekPreview)
	rel()
	first, second := <-order, <-order
	if first != PrioritySeekPreview || second != PriorityMarkers {
		t.Fatalf("granted %d then %d, want seek preview then markers", first, second)
	}
}

func TestLaneAcquireHonoursContext(t *testing.T) {
	var l Lane
	rel, _ := l.Acquire(context.Background(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, 0); err == nil {
		t.Fatal("Acquire returned without the lane or an error")
	}
	rel()
	r, err := l.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	r()
}

func TestClaimKeepsALowerPriorityWaiterOutBetweenTurns(t *testing.T) {
	var l Lane
	ctx := context.Background()
	unclaim := l.Claim(PrioritySeekPreview)
	rel, _ := l.Acquire(ctx, PrioritySeekPreview)
	got := make(chan struct{})
	go func() {
		r, err := l.Acquire(ctx, PriorityMarkers)
		if err == nil {
			close(got)
			r()
		}
	}()
	time.Sleep(20 * time.Millisecond)
	rel() // the claimant's turn ends; it is back before a Marker may start
	select {
	case <-got:
		t.Fatal("a Marker took the lane between two turns of a claimed preview job")
	case <-time.After(50 * time.Millisecond):
	}
	rel2, _ := l.Acquire(ctx, PrioritySeekPreview)
	rel2()
	unclaim()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the Marker never got the lane once the claim ended")
	}
}
