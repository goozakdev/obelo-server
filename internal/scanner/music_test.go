package scanner

import "testing"

// Pure unit tests for the tag → music identity mapper (issue tv-music/03
// acceptance criterion), mirroring identity_test.go / tv_test.go. No filesystem,
// no ffprobe — just the tag/path mapping from docs/naming-convention.md "Music:
// embedded tags are authority": Album-Artist grouping and path fallback.

func tagsFor(m map[string]string) map[string]string { return m }

func TestMusicIdentityFromTags(t *testing.T) {
	id, ok := MusicIdentityFromTags(tagsFor(map[string]string{
		"artist":       "Radiohead",
		"album_artist": "Radiohead",
		"album":        "OK Computer",
		"title":        "Paranoid Android",
		"track":        "2/12",
		"disc":         "1/1",
		"date":         "1997",
		"genre":        "Alternative",
	}), "/music/Radiohead/OK Computer/02 Paranoid Android.flac")
	if !ok {
		t.Fatal("expected identity from full tags")
	}
	if !id.FromTags {
		t.Error("FromTags should be true when tags present")
	}
	if id.Artist != "Radiohead" || id.AlbumArtist != "Radiohead" {
		t.Errorf("artist/albumArtist = %q/%q, want Radiohead", id.Artist, id.AlbumArtist)
	}
	if id.Album != "OK Computer" || id.AlbumYear != 1997 {
		t.Errorf("album/year = %q/%d, want OK Computer/1997", id.Album, id.AlbumYear)
	}
	if id.Title != "Paranoid Android" {
		t.Errorf("title = %q, want Paranoid Android", id.Title)
	}
	if id.Track != 2 || id.Disc != 1 {
		t.Errorf("track/disc = %d/%d, want 2/1", id.Track, id.Disc)
	}
	if id.Genre != "Alternative" {
		t.Errorf("genre = %q, want Alternative", id.Genre)
	}
}

// TestMusicAlbumArtistGrouping is the headline behavior: a compilation whose
// tracks have DIFFERENT `artist` tags but a SHARED `album_artist` ("Various
// Artists") must produce the SAME ArtistKey and AlbumKey — one Album, not one
// per track artist.
func TestMusicAlbumArtistGrouping(t *testing.T) {
	track1, ok1 := MusicIdentityFromTags(map[string]string{
		"artist":       "Artist A",
		"album_artist": "Various Artists",
		"album":        "Summer Hits",
		"title":        "Song One",
		"track":        "1",
	}, "/music/Various Artists/Summer Hits/01 Song One.mp3")
	track2, ok2 := MusicIdentityFromTags(map[string]string{
		"artist":       "Artist B",
		"album_artist": "Various Artists",
		"album":        "Summer Hits",
		"title":        "Song Two",
		"track":        "2",
	}, "/music/Various Artists/Summer Hits/02 Song Two.mp3")
	if !ok1 || !ok2 {
		t.Fatal("expected both compilation tracks to resolve")
	}
	if track1.ArtistKey != track2.ArtistKey {
		t.Errorf("compilation tracks have different ArtistKey: %q vs %q (should group under album_artist)",
			track1.ArtistKey, track2.ArtistKey)
	}
	if track1.AlbumKey != track2.AlbumKey {
		t.Errorf("compilation tracks have different AlbumKey: %q vs %q (should be ONE Album)",
			track1.AlbumKey, track2.AlbumKey)
	}
	if track1.AlbumArtist != "Various Artists" {
		t.Errorf("albumArtist = %q, want Various Artists", track1.AlbumArtist)
	}
	// The track-level Artist still reflects the performer.
	if track1.Artist != "Artist A" || track2.Artist != "Artist B" {
		t.Errorf("track artists = %q/%q, want Artist A/Artist B", track1.Artist, track2.Artist)
	}
	// Distinct tracks → distinct TrackKeys.
	if track1.TrackKey == track2.TrackKey {
		t.Error("distinct tracks share a TrackKey")
	}
}

// TestMusicAlbumVariedTrackYears: a "Greatest Hits" compilation often tags each
// track with its ORIGINAL release year (the track's "date"), so the album's
// tracks carry several different years. They must still resolve to ONE Album —
// the year is descriptive, not part of the Album's identity. (Regression: the
// AlbumKey once embedded the year, splitting such an album into one per year.)
func TestMusicAlbumVariedTrackYears(t *testing.T) {
	track1, ok1 := MusicIdentityFromTags(map[string]string{
		"artist": "The Band", "album_artist": "The Band",
		"album": "Greatest Hits", "title": "Early Song",
		"track": "1", "date": "1985",
	}, "/music/The Band/Greatest Hits/01 Early Song.mp3")
	track2, ok2 := MusicIdentityFromTags(map[string]string{
		"artist": "The Band", "album_artist": "The Band",
		"album": "Greatest Hits", "title": "Later Song",
		"track": "2", "date": "1994",
	}, "/music/The Band/Greatest Hits/02 Later Song.mp3")
	if !ok1 || !ok2 {
		t.Fatal("expected both compilation tracks to resolve")
	}
	if track1.AlbumKey != track2.AlbumKey {
		t.Errorf("varied-year tracks have different AlbumKey: %q vs %q (should be ONE Album)",
			track1.AlbumKey, track2.AlbumKey)
	}
	// Each track still reports its own year for display/ordering.
	if track1.AlbumYear != 1985 || track2.AlbumYear != 1994 {
		t.Errorf("track years = %d/%d, want 1985/1994", track1.AlbumYear, track2.AlbumYear)
	}
}

// TestMusicAlbumArtistFallbackToArtist: with NO album_artist tag, the Album
// groups under the (track) Artist — so a normal single-artist album files as one.
func TestMusicAlbumArtistFallbackToArtist(t *testing.T) {
	a, _ := MusicIdentityFromTags(map[string]string{
		"artist": "Solo Act", "album": "Debut", "title": "One", "track": "1",
	}, "/m/Solo Act/Debut/01.mp3")
	b, _ := MusicIdentityFromTags(map[string]string{
		"artist": "Solo Act", "album": "Debut", "title": "Two", "track": "2",
	}, "/m/Solo Act/Debut/02.mp3")
	if a.AlbumArtist != "Solo Act" || b.AlbumArtist != "Solo Act" {
		t.Errorf("albumArtist fallback = %q/%q, want Solo Act", a.AlbumArtist, b.AlbumArtist)
	}
	if a.ArtistKey != b.ArtistKey || a.AlbumKey != b.AlbumKey {
		t.Error("two tracks of one single-artist album should share artist+album keys")
	}
}

// TestMusicPathFallback: with NO tags, identity comes from the path layout
// `Artist/Album (Year)/NN - Title.ext` and the Track is flagged needs-review.
func TestMusicPathFallback(t *testing.T) {
	id, ok := MusicIdentityFromTags(nil, "/music/Pink Floyd/The Wall (1979)/05 - Another Brick.flac")
	if !ok {
		t.Fatal("expected path-fallback identity")
	}
	if id.FromTags {
		t.Error("FromTags should be false for a path-fallback (tagless) track")
	}
	if id.Artist != "Pink Floyd" || id.AlbumArtist != "Pink Floyd" {
		t.Errorf("path artist = %q/%q, want Pink Floyd", id.Artist, id.AlbumArtist)
	}
	if id.Album != "The Wall" || id.AlbumYear != 1979 {
		t.Errorf("path album/year = %q/%d, want The Wall/1979", id.Album, id.AlbumYear)
	}
	if id.Title != "Another Brick" {
		t.Errorf("path title = %q, want Another Brick", id.Title)
	}
	if id.Track != 5 {
		t.Errorf("path track = %d, want 5", id.Track)
	}
}

// TestMusicTagsWinOverPath: tags are authority; when both tags AND a path layout
// are present, the tags decide identity (the path only fills tag holes).
func TestMusicTagsWinOverPath(t *testing.T) {
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "Tagged Artist", "album": "Tagged Album", "title": "Tagged Title", "track": "7",
	}, "/music/Path Artist/Path Album (2000)/01 - Path Title.flac")
	if id.Artist != "Tagged Artist" || id.Album != "Tagged Album" || id.Title != "Tagged Title" {
		t.Errorf("tags should win: %+v", id)
	}
	if id.Track != 7 {
		t.Errorf("track from tag = %d, want 7", id.Track)
	}
}

// TestMusicPartialTagsFillFromPath: a partially-tagged file (title only, no
// album/artist) fills the missing fields from the path, still tag-derived.
func TestMusicPartialTagsFillFromPath(t *testing.T) {
	id, ok := MusicIdentityFromTags(map[string]string{
		"title": "Just A Title",
	}, "/music/Some Artist/Some Album/03 - whatever.mp3")
	if !ok {
		t.Fatal("expected identity from partial tags + path")
	}
	if id.Title != "Just A Title" {
		t.Errorf("title from tag = %q, want Just A Title", id.Title)
	}
	if id.Artist != "Some Artist" || id.Album != "Some Album" {
		t.Errorf("artist/album from path = %q/%q, want Some Artist/Some Album", id.Artist, id.Album)
	}
}

// TestMusicNoIdentity: a file with no tags and a bare name (no Artist/Album
// folders) still derives a title from the filename, so it is filed (never
// silently dropped) — but a truly empty name yields nothing.
func TestMusicNoIdentityFromBareName(t *testing.T) {
	// A bare file with just a filename gets a title (the filename) and Unknown
	// Artist/Album — filed, not dropped.
	id, ok := MusicIdentityFromTags(nil, "/music/randomtrack.mp3")
	if !ok {
		t.Fatal("a bare audio file should still file by filename title")
	}
	if id.Title != "randomtrack" {
		t.Errorf("title = %q, want randomtrack", id.Title)
	}
	if id.AlbumArtist != "Unknown Artist" {
		t.Errorf("albumArtist = %q, want Unknown Artist", id.AlbumArtist)
	}
}

func TestMusicDiscTrackParsing(t *testing.T) {
	id, _ := MusicIdentityFromTags(map[string]string{
		"artist": "A", "album": "B", "title": "C", "disc": "2/2", "track": "11/14",
	}, "/m/A/B/11.flac")
	if id.Disc != 2 || id.Track != 11 {
		t.Errorf("disc/track = %d/%d, want 2/11", id.Disc, id.Track)
	}
}

// TestMusicIdentityOverrideRepoints: an Album-folder Match override (issue
// tv-music/04) re-points the Album a track groups under, overruling the tag, and
// marks the track a confirmed match (not needs-review). The corrected AlbumKey is
// derived from the override's identity_key so it is stable across rescans.
func TestMusicIdentityOverrideRepoints(t *testing.T) {
	tags := map[string]string{
		"artist": "Radiohead", "album_artist": "Radiohead",
		"album": "Wrong Album", "title": "Airbag", "track": "1", "date": "1997",
	}
	ov := &AlbumOverride{Album: "OK Computer", Year: 1997, Key: "okc-key"}
	id, ok := MusicIdentityFromTagsWithOverride(tags, "/m/Radiohead/Wrong Album/01 Airbag.flac", ov)
	if !ok {
		t.Fatal("override should yield an identity")
	}
	if id.Album != "OK Computer" || id.AlbumYear != 1997 {
		t.Errorf("album/year = %q/%d, want OK Computer/1997 (override re-points)", id.Album, id.AlbumYear)
	}
	if id.Artist != "Radiohead" {
		t.Errorf("artist = %q, want Radiohead (override re-points Album, not Artist)", id.Artist)
	}
	if !id.FromTags {
		t.Error("an Admin-confirmed match must not be needs-review (FromTags should be true)")
	}
	if id.AlbumKey != "artist:radiohead|album-override:okc-key" {
		t.Errorf("albumKey = %q, want a stable override-derived key", id.AlbumKey)
	}
}

// TestMusicIdentityOverrideRescues: an Album-folder override rescues a file with
// no tag/path identity at all — the override asserts it belongs to the Album, the
// way a Movie/Show folder override rescues an unparseable folder.
func TestMusicIdentityOverrideRescues(t *testing.T) {
	// No tags, and a bare path with no Artist/Album folders → MusicIdentityFromTags
	// alone files by filename; with an override the Album is asserted.
	ov := &AlbumOverride{Album: "Rescued Album", Year: 2000, Key: "resc"}
	id, ok := MusicIdentityFromTagsWithOverride(nil, "/m/loosetrack.mp3", ov)
	if !ok {
		t.Fatal("override should rescue an otherwise-unidentified file")
	}
	if id.Album != "Rescued Album" {
		t.Errorf("album = %q, want Rescued Album", id.Album)
	}
	if id.Title != "loosetrack" {
		t.Errorf("title = %q, want loosetrack (filename fallback)", id.Title)
	}
}

// TestMusicIdentityNilOverrideUnchanged: a nil override is exactly
// MusicIdentityFromTags (no behavior change on the normal path).
func TestMusicIdentityNilOverrideUnchanged(t *testing.T) {
	tags := map[string]string{"artist": "A", "album": "B", "title": "C"}
	a, aok := MusicIdentityFromTags(tags, "/m/A/B/C.mp3")
	b, bok := MusicIdentityFromTagsWithOverride(tags, "/m/A/B/C.mp3", nil)
	if aok != bok || a != b {
		t.Errorf("nil override changed the result:\n got %+v (%v)\nwant %+v (%v)", b, bok, a, aok)
	}
}
