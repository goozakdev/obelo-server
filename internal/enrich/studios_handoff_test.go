package enrich

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// studioProvider answers every movie and show Lookup as a match carrying a
// multi-valued Studios list.
type studioProvider struct {
	studios []string
}

func (p *studioProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "show":
		return TitleMetadata{
			Matched: true, Name: ref.Title, ExternalID: "x-1", Source: "tmdb",
			Studio: p.studios[0], Studios: p.studios,
		}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

func (p *studioProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *studioProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// The service must hand a provider's whole Studios list to the store, not just
// the first: a Movie keeps every Studio and a Show every network.
func TestTheServiceStoresEveryStudioAndNetworkAProviderReturns(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO libraries (id, name, kind) VALUES ('lm', 'Movies', 'movie'), ('lt', 'TV', 'tv')`,
		`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, enrichment_status)
		 VALUES ('m1', 'lm', 'movie', 'A Movie', 'a movie', 'a movie', 'pending')`,
		`INSERT INTO shows (id, library_id, title, identity_key, sort_title)
		 VALUES ('s1', 'lt', 'A Show', 'a show', 'a show')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	movie := &studioProvider{studios: []string{"Legendary", "Warner Bros.", "Syncopy"}}
	svc := NewService(db, movie, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	if _, err := svc.EnrichLibrary(context.Background(), "lm", ModeNew); err != nil {
		t.Fatalf("movie pass: %v", err)
	}
	gotStudios, err := db.StudiosForTitles([]string{"m1"})
	if err != nil {
		t.Fatalf("StudiosForTitles: %v", err)
	}
	if !reflect.DeepEqual(gotStudios["m1"], movie.studios) {
		t.Errorf("movie studios = %v, want %v", gotStudios["m1"], movie.studios)
	}

	show := &studioProvider{studios: []string{"HBO", "Max"}}
	svc = NewService(db, show, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	if _, err := svc.EnrichLibrary(context.Background(), "lt", ModeNew); err != nil {
		t.Fatalf("show pass: %v", err)
	}
	gotNetworks, err := db.NetworksForEntities(store.EntityShow, []string{"s1"})
	if err != nil {
		t.Fatalf("NetworksForEntities: %v", err)
	}
	if !reflect.DeepEqual(gotNetworks["s1"], show.studios) {
		t.Errorf("show networks = %v, want %v", gotNetworks["s1"], show.studios)
	}
}
