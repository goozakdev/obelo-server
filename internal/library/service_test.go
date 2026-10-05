package library

import (
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// fakeStore records the roots CreateLibrary was handed; the rest is unused.
type fakeStore struct {
	Store
	created []store.LibraryRootInput
}

func (f *fakeStore) AllLibraryRoots() ([]store.LibraryRoot, error) { return nil, nil }

func (f *fakeStore) CreateLibrary(id, name, kind string, roots []store.LibraryRootInput) (store.Library, error) {
	f.created = roots
	return store.Library{ID: id, Name: name, Kind: kind}, nil
}

// A folder listed twice in one request is de-duplicated, as prepareRoots intends,
// rather than rejected as an overlap of the folder with itself.
func TestCreateDeduplicatesRepeatedRootFolders(t *testing.T) {
	f := &fakeStore{}
	_, err := NewService(f).Create(CreateInput{
		Name: "Movies", Kind: "movie",
		RootFolders: []string{"/media/movies", "/media/movies/", "/media/music"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(f.created) != 2 {
		t.Fatalf("persisted %d roots, want 2 (the duplicate folded away): %+v", len(f.created), f.created)
	}
}

// Genuinely nested roots in one request are still an overlap.
func TestCreateRejectsNestedRootFoldersInOneRequest(t *testing.T) {
	_, err := NewService(&fakeStore{}).Create(CreateInput{
		Name: "Movies", Kind: "movie",
		RootFolders: []string{"/media/movies", "/media/movies/4k"},
	})
	if !errors.Is(err, ErrFolderOverlap) {
		t.Fatalf("err = %v, want ErrFolderOverlap", err)
	}
}
