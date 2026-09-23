package enrich

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0062, driven through real passes over a real store: ModeMissing's whole
// reason to exist is a 'failed' item whose retry is still in the future — the
// one population ModeRecheck deliberately leaves alone (ADR-0048). These assert
// the CALL LOG, same as recheck_mode_test.go: "asked" and "not asked" are the
// only place ModeMissing and ModeRecheck actually differ for such a row.

// --- TV: a Show parked with a future retry, and its episodes -------------------

// tvCallProvider answers every TV Lookup as a match and records what it was
// asked, tagged by kind.
type tvCallProvider struct {
	mu    sync.Mutex
	calls []string
}

func (p *tvCallProvider) note(s string) {
	p.mu.Lock()
	p.calls = append(p.calls, s)
	p.mu.Unlock()
}

func (p *tvCallProvider) history() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *tvCallProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "show":
		p.note("show:" + ref.Title)
		return TitleMetadata{Matched: true, Name: ref.Title, ExternalID: "show-1", Source: "tmdb"}, nil
	case "season":
		p.note(fmt.Sprintf("season:%d", ref.SeasonNumber))
		return TitleMetadata{Matched: true, ExternalID: "season-1", Source: "tmdb"}, nil
	case "episode":
		p.note("episode:" + ref.Title)
		return TitleMetadata{Matched: true, Name: ref.Title, Overview: "synopsis", ExternalID: "ep-1", Source: "tmdb"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

func (p *tvCallProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *tvCallProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// buildTVFixture seeds one tv Library: a Show sh1 ('failed', retryAt in the
// future) with one Season and two Episodes — ep1 'pending', ep2 'failed' with
// the same future retry as its Show — plus a second, already-'matched' Show
// sh2 with one 'pending' episode (a control so the pass is not visiting an
// empty library).
func buildTVFixture(t *testing.T, future string) (*Service, *store.DB, *tvCallProvider) {
	t.Helper()
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
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Shows', 'tv')`)

	exec(`INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	      VALUES ('sh1', 'lib', 'Parked Show', 'parked show', 'parked show')`)
	exec(`INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status, enrichment_retry_at)
	      VALUES ('show', 'sh1', 'failed', ?)`, future)
	exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se1', 'sh1', 1, 'parked show|s01')`)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                          season_id, season_number, episode_number, enrichment_status)
	      VALUES ('ep1', 'lib', 'episode', 'Pending Episode', 'parked show|e1', 'pending episode', 'se1', 1, 1, 'pending')`)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                          season_id, season_number, episode_number, enrichment_status, enrichment_retry_at)
	      VALUES ('ep2', 'lib', 'episode', 'Parked Episode', 'parked show|e2', 'parked episode', 'se1', 1, 2, 'failed', ?)`, future)

	exec(`INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	      VALUES ('sh2', 'lib', 'Matched Show', 'matched show', 'matched show')`)
	exec(`INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status, external_id, external_id_namespace)
	      VALUES ('show', 'sh2', 'matched', 'show-2', 'tmdb')`)
	exec(`INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se2', 'sh2', 1, 'matched show|s01')`)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                          season_id, season_number, episode_number, enrichment_status)
	      VALUES ('ep3', 'lib', 'episode', 'Control Episode', 'matched show|e1', 'control episode', 'se2', 1, 1, 'pending')`)

	prov := &tvCallProvider{}
	svc := NewService(db, prov, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) })
	return svc, db, prov
}

func hasCall(calls []string, s string) bool {
	for _, c := range calls {
		if c == s {
			return true
		}
	}
	return false
}

// The headline: a Show parked with a retry still in the future — and its
// likewise-parked Episode — are asked under ModeMissing and NOT under ModeNew
// or ModeRecheck (which both leave a future retry to the server's own
// schedule, ADR-0048).
func TestMissingReAsksAFutureRetryShowAndItsEpisodesUnlikeNewOrRecheck(t *testing.T) {
	future := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)

	for _, mode := range []struct {
		name string
		mode Mode
		want bool // whether the parked show/episode should have been asked
	}{
		{"ModeNew", ModeNew, false},
		{"ModeRecheck", ModeRecheck, false},
		{"ModeMissing", ModeMissing, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			svc, _, prov := buildTVFixture(t, future)
			if _, err := svc.EnrichLibrary(context.Background(), "lib", mode.mode); err != nil {
				t.Fatalf("pass: %v", err)
			}
			calls := prov.history()
			gotShow := hasCall(calls, "show:Parked Show")
			gotEp := hasCall(calls, "episode:Parked Episode")
			if gotShow != mode.want || gotEp != mode.want {
				t.Fatalf("%s: show asked=%v episode asked=%v (calls: %v), want both %v",
					mode.name, gotShow, gotEp, calls, mode.want)
			}
			// The pending episode under the parked show is always in scope, in every
			// mode — this isolates the assertion above to the future-retry rows.
			if !hasCall(calls, "episode:Pending Episode") {
				t.Fatalf("%s: the plain pending episode was skipped (calls: %v)", mode.name, calls)
			}
		})
	}
}

// A 'matched' Show is not re-asked under ModeMissing: matched is not missing,
// even though ModeMissing widens past ModeRecheck's boundary elsewhere.
func TestMissingDoesNotReAskAMatchedShow(t *testing.T) {
	future := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)
	svc, _, prov := buildTVFixture(t, future)

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeMissing); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if hasCall(prov.history(), "show:Matched Show") {
		t.Fatalf("a matched Show was re-asked under ModeMissing (calls: %v) — a matched parent "+
			"is not missing, in any mode short of Full", prov.history())
	}
}

// --- Music: an Album parked with a future retry ---------------------------

// The same shape one level down the Music tree: an Album settled 'failed' with
// a retry still in the future is reached by ModeMissing and left alone by
// ModeNew/ModeRecheck.
func TestMissingReAsksAFutureRetryAlbumUnlikeNewOrRecheck(t *testing.T) {
	future := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)

	for _, mode := range []struct {
		name string
		mode Mode
		want int // expected album lookups
	}{
		{"ModeNew", ModeNew, 0},
		{"ModeRecheck", ModeRecheck, 0},
		{"ModeMissing", ModeMissing, 1},
	} {
		t.Run(mode.name, func(t *testing.T) {
			prov := &albumTierProvider{
				albumRG:    "rg-she",
				tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
				recordings: map[string]string{"rec-1": "Whisper Your Name"},
			}
			svc, db := newAlbumFixture(t, prov, seedAlbum{
				entityStatus: "failed", // parked, no record of its own yet
				tracks: []seedTrack{
					{id: "t1", title: "Whisper Your Name", num: 1, status: "pending"},
				},
			})
			if _, err := db.Exec(
				`UPDATE entity_enrichment SET enrichment_retry_at = ? WHERE entity_type = 'album' AND entity_id = 'al1'`,
				future,
			); err != nil {
				t.Fatalf("seed retry: %v", err)
			}

			if _, err := svc.EnrichLibrary(context.Background(), "lib", mode.mode); err != nil {
				t.Fatalf("pass: %v", err)
			}
			if n := prov.count("rg?search"); n != mode.want {
				t.Fatalf("%s: album searched %d times (calls: %v), want %d", mode.name, n, prov.history(), mode.want)
			}
		})
	}
}
