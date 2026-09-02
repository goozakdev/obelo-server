package scanner

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The scan-level half of ADR-0050's release id: the tag mapper is unit-tested in
// music_mbid_test.go, and these prove the value survives the walk and lands on the
// store.AlbumTree the scan hands UpsertArtistTree. A field read correctly and then
// dropped between the tags and the tree is exactly what ADR-0049 found the
// release-GROUP id doing for two years.

// taggedProber returns per-basename tags, so one scan can carry a FLAC's Vorbis
// spelling and an MP3's ID3 spelling of the same id in the same album.
type taggedProber struct {
	container string
	byBase    map[string]map[string]string
}

func (p taggedProber) Probe(_ context.Context, path string) (MediaInfo, error) {
	container := p.container
	if container == "" {
		container = "flac"
	}
	return MediaInfo{
		Container:  container,
		DurationMs: 200_000,
		Streams:    []StreamInfo{{Index: 0, Kind: "audio", Codec: "flac", Channels: 2, IsDefault: true}},
		Tags:       p.byBase[filepath.Base(path)],
	}, nil
}

// scanOneAlbum walks a temp library holding one album folder whose files carry
// the given per-basename tags, and returns the single AlbumTree the scan built.
func scanOneAlbum(t *testing.T, artist, album string, byBase map[string]map[string]string) store.AlbumTree {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, artist, album)
	for base := range byBase {
		writeFile(t, filepath.Join(dir, base))
	}

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "music",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	ms := &musicProbeStore{captureStore: cs}
	if _, err := NewService(ms, taggedProber{byBase: byBase}).Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(ms.artistTrees) != 1 {
		t.Fatalf("artist trees = %d, want 1", len(ms.artistTrees))
	}
	if n := len(ms.artistTrees[0].Albums); n != 1 {
		t.Fatalf("albums = %d, want 1", n)
	}
	return ms.artistTrees[0].Albums[0]
}

// A FLAC's MUSICBRAINZ_ALBUMID and an MP3's TXXX:MusicBrainz Album Id are the same
// fact spelled two ways; both must land the same lower-cased UUID on the Album.
// The MP3 case is the one that matters: its recording id is in a binary UFID frame
// ffprobe never surfaces, so this tag is the file's only exact anchor (ADR-0050).
func TestScanLandsTheReleaseIDOnTheAlbumFromEitherSpelling(t *testing.T) {
	const release = "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"
	common := map[string]string{
		"artist": "Harry Connick, Jr.", "album_artist": "Harry Connick, Jr.",
		"album": "She", "date": "1994",
	}
	tagsFor := func(title, track string, extra map[string]string) map[string]string {
		out := map[string]string{"title": title, "track": track}
		for k, v := range common {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// Vorbis spelling, on its own.
	at := scanOneAlbum(t, "Harry Connick, Jr.", "She", map[string]map[string]string{
		"01 Here Comes the Big Parade.flac": tagsFor("Here Comes the Big Parade", "1",
			map[string]string{"musicbrainz_albumid": release}),
	})
	if at.MusicbrainzReleaseID != release {
		t.Fatalf("Vorbis: AlbumTree.MusicbrainzReleaseID = %q, want %q",
			at.MusicbrainzReleaseID, release)
	}

	// ID3/MP4 spelling, upper-cased as a tagger may leave it, on its own.
	at = scanOneAlbum(t, "Harry Connick, Jr.", "She", map[string]map[string]string{
		"01 Here Comes the Big Parade.mp3": tagsFor("Here Comes the Big Parade", "1",
			map[string]string{"musicbrainz album id": "3FD8BB0D-8F5B-4E64-8E5E-31A1C2A71B2B"}),
	})
	if at.MusicbrainzReleaseID != release {
		t.Fatalf("ID3: AlbumTree.MusicbrainzReleaseID = %q, want the lower-cased %q",
			at.MusicbrainzReleaseID, release)
	}
}

// Within a single scan an Album is assembled from many files, and a later untagged
// track must not blank what an earlier tagged one established — the same
// filled-but-never-blanked rule the release-group and artist ids follow (ADR-0049).
// The album's identity key must also be untouched by the presence of the id.
func TestScanKeepsTheReleaseIDWhenALaterTrackIsUntagged(t *testing.T) {
	const release = "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"
	common := map[string]string{
		"artist": "Harry Connick, Jr.", "album_artist": "Harry Connick, Jr.", "album": "She",
	}
	tagged := map[string]string{"title": "Here Comes the Big Parade", "track": "1",
		"musicbrainz_albumid": release}
	untagged := map[string]string{"title": "(I Could Only) Whisper Your Name", "track": "3"}
	for k, v := range common {
		tagged[k] = v
		untagged[k] = v
	}

	at := scanOneAlbum(t, "Harry Connick, Jr.", "She", map[string]map[string]string{
		"01 Here Comes the Big Parade.flac": tagged,
		"03 Whisper Your Name.flac":         untagged,
	})
	if len(at.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(at.Tracks))
	}
	if at.MusicbrainzReleaseID != release {
		t.Errorf("MusicbrainzReleaseID = %q, want %q — an untagged sibling blanked the id",
			at.MusicbrainzReleaseID, release)
	}

	// Identity is unmoved: no release id anywhere in the album key (ADR-0002/0038).
	plain := scanOneAlbum(t, "Harry Connick, Jr.", "She", map[string]map[string]string{
		"01 Here Comes the Big Parade.flac": untagged,
	})
	if at.IdentityKey != plain.IdentityKey {
		t.Errorf("album identity_key = %q with the release id, %q without — identity moved",
			at.IdentityKey, plain.IdentityKey)
	}
}
