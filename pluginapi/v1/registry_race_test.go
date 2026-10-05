package v1

import (
	"sync"
	"testing"
)

// Concurrent registrations of one slug must leave exactly one registration: the
// duplicate check and the append are one critical section.
func TestConcurrentDuplicateRegistrationAdmitsOne(t *testing.T) {
	for round := 0; round < 200; round++ {
		r := NewRegistry()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = recover() }()
				<-start
				r.RegisterEventSink(EventSinkRegistration{
					Descriptor: Descriptor{Slug: "dup", Name: "Dup"},
					New:        func(Settings) (EventSink, error) { return nil, nil },
				})
			}()
		}
		close(start)
		wg.Wait()
		if n := len(r.EventSinks()); n != 1 {
			t.Fatalf("round %d: %d registrations of one slug, want 1", round, n)
		}
	}
}
