package scanner

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// R02-15: the scanner sort key IS the store one, so a scanned Title and a
// re-keyed one cannot order differently.
func TestSortTitleIsTheStoresRule(t *testing.T) {
	for _, in := range []string{"The Matrix", "  An Apple ", "A Bugs Life", "Theater", "Anthem", "the"} {
		if got, want := sortTitle(in), store.SortTitle(in); got != want {
			t.Errorf("sortTitle(%q) = %q, store.SortTitle = %q", in, got, want)
		}
	}
	if got := sortTitle("The Matrix"); got != "matrix" {
		t.Errorf("sortTitle(The Matrix) = %q, want matrix", got)
	}
}
