package enrich

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The enrichment pass skips a linked Library (ADR-0056 §1).
//
// A mirrored Title's descriptive fields are whatever the SHARING Server's own
// pass decided. Re-deriving them here would spend this household's provider quota
// to overwrite the answer the identity authority already gave (ADR-0002,
// ADR-0019) — and would then be overwritten again by the next pull, forever.
//
// The API refuses POST /libraries/{id}/enrich on a mirror with 409
// LINKED_LIBRARY; this is the backstop for the scheduled sweep and the
// auto-after-scan trigger, which reach the service directly.
func TestAnEnrichPassRefusesALinkedLibrary(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	exec(`INSERT INTO links (id, server_id, server_name, origins, active_origin, token,
	        link_protocol_version, state, created_at)
	      VALUES ('lk', 'peer', 'Dave', '["http://dave"]', 'http://dave', 't', 1, 'connected',
	              '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO libraries (id, name, kind, source, link_id, remote_library_id)
	      VALUES ('mirror', "Dave's films", 'movie', 'linked', 'lk', 'their-lib')`)
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('local', 'Ours', 'movie')`)

	counting := &countingLinkedProvider{}
	svc := NewService(db, counting, nil, Enablement{Video: true}, t.TempDir(), 0)

	if _, err := svc.EnrichLibrary(context.Background(), "mirror", ModeNew); !errors.Is(err, ErrLinkedLibrary) {
		t.Errorf("enriching a mirror = %v, want ErrLinkedLibrary", err)
	}
	if counting.calls != 0 {
		t.Errorf("a refused pass still made %d provider calls", counting.calls)
	}

	// The control: an ordinary Library still passes (it is empty, so the Result is
	// zeroes — the point is that it is not a refusal).
	if _, err := svc.EnrichLibrary(context.Background(), "local", ModeNew); err != nil {
		t.Fatalf("enriching a LOCAL library = %v, want success", err)
	}
}

// countingLinkedProvider is the assertion that a refused pass reaches nobody: it
// records every call and there must be none.
type countingLinkedProvider struct{ calls int }

func (p *countingLinkedProvider) Lookup(context.Context, TitleRef) (TitleMetadata, error) {
	p.calls++
	return TitleMetadata{}, ErrNoMatch
}

func (p *countingLinkedProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	p.calls++
	return nil, ErrSearchUnavailable
}

func (p *countingLinkedProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	p.calls++
	return nil, nil
}
