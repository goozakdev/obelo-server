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

// The RELEASE id (ADR-0050) lives under a tag named after the ALBUM, and it
// shares the prefix "musicbrainz_album" with the ALBUM ARTIST id — exactly the
// substring a prefix test would match on. An artist id handed to
// /ws/2/release/<id> 404s, so the loose test produces ADR-0049's
// confident-wrong-answer wearing a different tag name.
func TestReleaseIDReadsTheAlbumIDNotTheAlbumArtistID(t *testing.T) {
	const release = "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"
	const albumArtist = "a74b1b7f-71a5-4011-9441-d0b5e4122711"
	const releaseGroup = "b84ee12a-09ef-421b-82de-0441a926375b"

	id, ok := MusicIdentityFromTags(map[string]string{
		"artist": "Harry Connick, Jr.", "album": "She", "title": "Whisper Your Name",
		"musicbrainz_albumid":        release,
		"musicbrainz_albumartistid":  albumArtist,
		"musicbrainz_releasegroupid": releaseGroup,
	}, "/m/a/b.flac")
	if !ok {
		t.Fatal("no identity")
	}
	if id.ReleaseMBID != release {
		t.Fatalf("ReleaseMBID = %q, want the release id %q", id.ReleaseMBID, release)
	}
	// The release id must not have displaced the release-GROUP id, which is a
	// different anchor with a different endpoint (and an identity job, ADR-0038).
	if id.ReleaseGroupMBID != releaseGroup {
		t.Fatalf("ReleaseGroupMBID = %q, want %q", id.ReleaseGroupMBID, releaseGroup)
	}

	// The album-artist id ALONE must yield NOTHING here. A prefix/substring test on
	// "musicbrainz_album" swallows it and renumbers the whole album against a
	// tracklist that does not exist.
	id, _ = MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C",
		"musicbrainz_albumartistid": albumArtist,
	}, "/m/a/b.flac")
	if id.ReleaseMBID != "" {
		t.Fatalf("ReleaseMBID = %q from an album-ARTIST tag alone, want empty", id.ReleaseMBID)
	}
	// Same for the release-group id, whose key also begins "musicbrainz_release".
	id, _ = MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C",
		"musicbrainz_releasegroupid": releaseGroup,
	}, "/m/a/b.flac")
	if id.ReleaseMBID != "" {
		t.Fatalf("ReleaseMBID = %q from a release-GROUP tag alone, want empty", id.ReleaseMBID)
	}
}

// The ID3/MP4 spelling is the whole point of this id: Picard writes the RECORDING
// id to the binary UFID frame ffprobe cannot read, but writes the RELEASE id to a
// TXXX frame it can. Miss this spelling and the feature covers only the libraries
// that were never stuck.
func TestReleaseIDFromTheID3SpellingAndValidation(t *testing.T) {
	const release = "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C",
		"musicbrainz album id": strings.ToUpper(release),
	}, "/m/a/b.mp3")
	if id.ReleaseMBID != release {
		t.Errorf("ReleaseMBID = %q, want the lower-cased %q from the ID3/MP4 spelling",
			id.ReleaseMBID, release)
	}

	// A malformed value is dropped, not stored: an id that 404s files the album as
	// "no such release", where no id would have picked a release by fit.
	for _, bad := range []string{"", "not-a-uuid", "12345", "3fd8bb0d-8f5b-4e64-8e5e", "null", "0"} {
		id, _ := MusicIdentityFromTags(map[string]string{
			"artist": "A", "album": "B", "title": "C", "musicbrainz_albumid": bad,
		}, "/m/a/b.flac")
		if id.ReleaseMBID != "" {
			t.Errorf("tag %q yielded ReleaseMBID %q, want it dropped", bad, id.ReleaseMBID)
		}
	}

	// Multi-valued: the first credit, never the joined string.
	const second = "89ad4ac3-39f7-470e-963a-56509c546377"
	id, _ = MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C",
		"musicbrainz_albumid": release + "; " + second,
	}, "/m/a/b.flac")
	if id.ReleaseMBID != release {
		t.Errorf("multi-valued tag yielded %q, want the first credit %q", id.ReleaseMBID, release)
	}
}

// The release id is DECORATION, not identity (ADR-0002/0038/0050). A file that
// carries it must produce byte-identical Artist/Album/Track keys to one that does
// not, or an existing library re-keys — and re-points its watch state — the moment
// its owner retags it.
func TestReleaseIDNeverEntersAnIdentityKey(t *testing.T) {
	base := map[string]string{
		"artist": "Harry Connick, Jr.", "album_artist": "Harry Connick, Jr.",
		"album": "She", "title": "(I Could Only) Whisper Your Name",
		"disc": "1", "track": "3", "date": "1994",
	}
	plain, ok := MusicIdentityFromTags(base, "/m/a/b.flac")
	if !ok {
		t.Fatal("no identity")
	}

	tagged := map[string]string{"musicbrainz_albumid": "3fd8bb0d-8f5b-4e64-8e5e-31a1c2a71b2b"}
	for k, v := range base {
		tagged[k] = v
	}
	withID, _ := MusicIdentityFromTags(tagged, "/m/a/b.flac")

	if withID.ReleaseMBID == "" {
		t.Fatal("ReleaseMBID empty; the fixture is not exercising the id")
	}
	if withID.ArtistKey != plain.ArtistKey {
		t.Errorf("ArtistKey = %q, want %q unchanged", withID.ArtistKey, plain.ArtistKey)
	}
	if withID.AlbumKey != plain.AlbumKey {
		t.Errorf("AlbumKey = %q, want %q unchanged (a release id in a key re-keys an album on retag)",
			withID.AlbumKey, plain.AlbumKey)
	}
	if withID.TrackKey != plain.TrackKey {
		t.Errorf("TrackKey = %q, want %q unchanged", withID.TrackKey, plain.TrackKey)
	}
	if strings.Contains(withID.AlbumKey, withID.ReleaseMBID) {
		t.Errorf("AlbumKey %q embeds the release id", withID.AlbumKey)
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
