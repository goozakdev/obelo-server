package api

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// TestHeldTitleIDsIncludeTheRecordsAndLetThemOutrankTheFolder: a Title's record
// ids are held — the usual source of them, since most folders assert none — and
// where the record and the folder both name a namespace, the record's id is the
// one held (ADR-0045, ADR-0060). An empty id is not held from either.
func TestHeldTitleIDsIncludeTheRecordsAndLetThemOutrankTheFolder(t *testing.T) {
	t.Parallel()
	got := heldTitleIDs(store.Title{
		IdentityIDs: map[string]string{"imdb": "tt0000001", "tvdb": "81189", "anidb": ""},
		RecordIDs:   map[string]string{"imdb": "tt1160419", "tmdb": "438631", "trakt": ""},
	})
	want := map[string]string{"imdb": "tt1160419", "tvdb": "81189", "tmdb": "438631"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("heldTitleIDs = %v, want %v", got, want)
	}
}
