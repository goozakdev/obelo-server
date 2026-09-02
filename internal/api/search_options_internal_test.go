package api

import (
	"net/http/httptest"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
)

// White-box tests for the picker's search query params. What matters here is which
// URL produces which SearchOptions — observable from outside only through a
// provider's recorded call, and the mapping is the whole of the server's share of
// needs-fixing/06.

// TestSearchOptionsFromReadsArtistAndRelease: both narrowing params reach the
// provider, trimmed. `release` is the album box on a Track's Needs-Fixing row.
func TestSearchOptionsFromReadsArtistAndRelease(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?q=Whisper+Your+Name&artist=+Harry+Connick%2C+Jr.+&release=+She+&page=2", nil)

	opts := searchOptionsFrom(r)

	if opts.Artist != "Harry Connick, Jr." {
		t.Errorf("Artist = %q, want the trimmed artist", opts.Artist)
	}
	if opts.Release != "She" {
		t.Errorf("Release = %q, want the trimmed album", opts.Release)
	}
	if opts.Limit != enrich.SearchCandidateLimit {
		t.Errorf("Limit = %d, want %d", opts.Limit, enrich.SearchCandidateLimit)
	}
	if opts.Offset != 2*enrich.SearchCandidateLimit {
		t.Errorf("Offset = %d, want page 2", opts.Offset)
	}
}

// TestSearchOptionsFromAbsentScopeIsNoNarrowing: a video search — which sends
// neither param — narrows on neither axis, so its provider query is unchanged by
// this slice. A present-but-blank param means the same thing: the Admin widened the
// box, and widening must actually widen.
func TestSearchOptionsFromAbsentScopeIsNoNarrowing(t *testing.T) {
	for _, url := range []string{"/x?q=Arrival", "/x?q=Arrival&artist=&release=+"} {
		opts := searchOptionsFrom(httptest.NewRequest("GET", url, nil))
		if opts.Artist != "" || opts.Release != "" {
			t.Errorf("%s: narrowing = artist %q release %q, want neither", url, opts.Artist, opts.Release)
		}
		if opts.Offset != 0 {
			t.Errorf("%s: Offset = %d, want the first page", url, opts.Offset)
		}
	}
}
