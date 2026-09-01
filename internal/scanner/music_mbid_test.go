package scanner

import (
	"strings"
	"testing"
)

// The MusicBrainz ids a tagger already wrote into the file (ADR-0049). Getting a
// key name wrong here is not a missing feature — it feeds a wrong id to a lookup,
// which 404s, which the pass files as "no such record". A confident wrong answer
// where no id at all would have produced a correct search.

func TestRecordingMBIDReadsTheRecordingNotTheTrack(t *testing.T) {
	const recording = "b9ad642e-b012-41c7-b72a-42cf4911a0f1"
	const releaseTrack = "1d1e0e1b-2c3d-4e5f-8a9b-0c1d2e3f4a5b"

	// Vorbis (FLAC/OGG), as Picard writes it: MUSICBRAINZ_TRACKID holds the
	// RECORDING id, MUSICBRAINZ_RELEASETRACKID the release-specific track id.
	id, ok := MusicIdentityFromTags(map[string]string{
		"artist": "Boards of Canada", "album": "Music Has the Right", "title": "Roygbiv",
		"musicbrainz_trackid":        recording,
		"musicbrainz_releasetrackid": releaseTrack,
	}, "/m/a/b.flac")
	if !ok {
		t.Fatal("no identity")
	}
	if id.RecordingMBID != recording {
		t.Fatalf("RecordingMBID = %q, want the recording id %q — reading the RELEASE TRACK id "+
			"instead sends /ws/2/recording/<track-id>, which 404s and files a perfectly "+
			"matchable Track as unmatched", id.RecordingMBID, recording)
	}

	// The release-track id ALONE must not be mistaken for a recording id. This is
	// the trap an exact-key match exists to avoid: a prefix or substring test on
	// "musicbrainz_track" happily swallows "musicbrainz_releasetrackid".
	id, _ = MusicIdentityFromTags(map[string]string{
		"artist": "A", "title": "B",
		"musicbrainz_releasetrackid": releaseTrack,
	}, "/m/a/b.flac")
	if id.RecordingMBID != "" {
		t.Fatalf("RecordingMBID = %q from a release-track tag alone, want empty", id.RecordingMBID)
	}
}

func TestArtistMBIDPrefersTheAlbumArtist(t *testing.T) {
	const albumArtist = "89ad4ac3-39f7-470e-963a-56509c546377" // Various Artists
	const trackArtist = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

	// A compilation: the Album files under its album-artist, so the id has to
	// describe the same entity or the "Various Artists" row is decorated from
	// whichever track artist happened to be seen first.
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "Radiohead", "album_artist": "Various Artists", "album": "Now 42", "title": "Creep",
		"musicbrainz_artistid":      trackArtist,
		"musicbrainz_albumartistid": albumArtist,
	}, "/m/a/b.flac")
	if id.ArtistMBID != albumArtist {
		t.Fatalf("ArtistMBID = %q, want the ALBUM artist %q", id.ArtistMBID, albumArtist)
	}

	// With no album-artist id, the track artist's is the honest fallback.
	id, _ = MusicIdentityFromTags(map[string]string{
		"artist": "Radiohead", "album": "OK Computer", "title": "Creep",
		"musicbrainz_artistid": trackArtist,
	}, "/m/a/b.flac")
	if id.ArtistMBID != trackArtist {
		t.Fatalf("ArtistMBID = %q, want the fallback %q", id.ArtistMBID, trackArtist)
	}
}

// A malformed id must be dropped, not forwarded. An unvalidated id is worse than
// none: the lookup 404s and the item is filed as "no such record", where an empty
// id would have produced a correct search.
func TestMalformedMBIDsAreDropped(t *testing.T) {
	for _, bad := range []string{"", "not-a-uuid", "12345", "b9ad642e-b012-41c7-b72a", "null", "0"} {
		id, _ := MusicIdentityFromTags(map[string]string{
			"artist": "A", "title": "B", "musicbrainz_trackid": bad,
		}, "/m/a/b.flac")
		if id.RecordingMBID != "" {
			t.Errorf("tag %q yielded RecordingMBID %q, want it dropped", bad, id.RecordingMBID)
		}
	}
}

// Collaborations carry several ids in one tag; the joined string is not an id.
func TestMultiValuedMBIDTakesTheFirst(t *testing.T) {
	const first = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	const second = "89ad4ac3-39f7-470e-963a-56509c546377"
	for _, joined := range []string{
		first + ";" + second,
		first + "/" + second,
		first + "; " + second,
	} {
		id, _ := MusicIdentityFromTags(map[string]string{
			"artist": "A", "title": "B", "musicbrainz_artistid": joined,
		}, "/m/a/b.flac")
		if id.ArtistMBID != first {
			t.Errorf("tag %q yielded %q, want the first credit %q", joined, id.ArtistMBID, first)
		}
	}
}

// The ID3/MP4 spellings ffprobe surfaces (lower-cased, spaced) must work too, or
// the feature covers FLAC libraries only.
func TestMBIDsFromSpacedTagSpellings(t *testing.T) {
	const recording = "b9ad642e-b012-41c7-b72a-42cf4911a0f1"
	const artist = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "A", "title": "B",
		"musicbrainz track id":        recording,
		"musicbrainz album artist id": artist,
	}, "/m/a/b.m4a")
	if id.RecordingMBID != recording {
		t.Errorf("RecordingMBID = %q, want %q from the MP4/ID3 spelling", id.RecordingMBID, recording)
	}
	if id.ArtistMBID != artist {
		t.Errorf("ArtistMBID = %q, want %q from the MP4/ID3 spelling", id.ArtistMBID, artist)
	}
}

// The release-group id already drives album IDENTITY (ADR-0038); ADR-0049 also
// carries it out as a lookup anchor. Both must hold at once.
func TestReleaseGroupIDIsBothIdentityAndLookupAnchor(t *testing.T) {
	const rg = "b84ee12a-09ef-421b-82de-0441a926375b"
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C", "musicbrainz_releasegroupid": rg,
	}, "/m/a/b.flac")
	if id.ReleaseGroupMBID != rg {
		t.Errorf("ReleaseGroupMBID = %q, want %q", id.ReleaseGroupMBID, rg)
	}
	if want := "|album-mbrg:" + rg; !strings.Contains(id.AlbumKey, want) {
		t.Errorf("AlbumKey = %q, want it to still carry %q (ADR-0038 identity unchanged)",
			id.AlbumKey, want)
	}
}
