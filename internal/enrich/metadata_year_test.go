package enrich

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A folder name with no year takes the matched metadata's year, through a real
// ModeNew pass over a real store. The Show starts 'pending' in entity_enrichment,
// the state migration 0007 leaves a yearless matched Show in.

type yearProvider struct{}

func (yearProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "show":
		return TitleMetadata{Matched: true, Name: ref.Title, Year: 2022, ExternalID: "83867", Source: "tmdb"}, nil
	case "movie":
		return TitleMetadata{Matched: true, ReleaseDate: "2021-10-22", ExternalID: "438631", Source: "tmdb"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

func (yearProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (yearProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

func TestEnrichmentPassFillsYearlessShowAndMovieYear(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO libraries (id, name, kind) VALUES ('ltv', 'Shows', 'tv'), ('lmv', 'Movies', 'movie')`,
		`INSERT INTO shows (id, library_id, title, identity_key, sort_title, needs_review)
		   VALUES ('sh1', 'ltv', 'Andor', 'andor', 'andor', 1)`,
		`INSERT INTO entity_enrichment (entity_type, entity_id, enrichment_status) VALUES ('show', 'sh1', 'pending')`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, needs_review)
		   VALUES ('m1', 'lmv', 'movie', 'Dune', 'dune', 'dune', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	svc := NewService(db, yearProvider{}, noArtwork{}, Enablement{Video: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) })
	for _, lib := range []string{"ltv", "lmv"} {
		if _, err := svc.EnrichLibrary(context.Background(), lib, ModeNew); err != nil {
			t.Fatalf("pass %s: %v", lib, err)
		}
	}
	for _, c := range []struct {
		table, id string
		year      int64
	}{{"shows", "sh1", 2022}, {"titles", "m1", 2021}} {
		var year sql.NullInt64
		var review int
		if err := db.QueryRow(`SELECT year, needs_review FROM `+c.table+` WHERE id = ?`, c.id).Scan(&year, &review); err != nil {
			t.Fatal(err)
		}
		if year.Int64 != c.year || review != 0 {
			t.Errorf("%s %s: year=%v needs_review=%d, want %d / 0", c.table, c.id, year, review, c.year)
		}
	}
}
