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

// A single folder level above an untagged file is the Artist, not the Album
// (refuter-09): "Artist/song.mp3" has no album folder to read.
func TestMusicPathFallbackOneLevelIsTheArtist(t *testing.T) {
	artist, album, _, title, _ := parseMusicPath("Artist/01 - Song.mp3")
	if artist != "Artist" || album != "" || title != "Song" {
		t.Errorf("artist=%q album=%q title=%q, want Artist / no album / Song", artist, album, title)
	}
	artist, album, _, _, _ = parseMusicPath("Artist/Album (1999)/01 - Song.mp3")
	if artist != "Artist" || album != "Album" {
		t.Errorf("two-level: artist=%q album=%q", artist, album)
	}
}
