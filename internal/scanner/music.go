package scanner

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Music identity (issue tv-music/03, amended ADR-0002: embedded tags are the
// identity authority for music, docs/naming-convention.md "Music: embedded tags
// are authority"). This file holds the PURE, offline, unit-testable mapper that
// turns a file's metadata tags (and, as a fallback, its path) into a music
// identity: Artist → Album → Track.
//
// The mapper is deliberately filesystem-free: it takes the already-extracted
// tags (from the Prober) plus the file's path, so it is exercised with table
// tests exactly like ParseIdentity / ParseEpisodeToken. Album-Artist grouping
// and path fallback are the two behaviors the issue calls out by name.

// MusicIdentity is the deterministic music identity of one audio file. The
// Album is grouped under the AlbumArtist (falling back to Artist) so a
// compilation / "Various Artists" album stays one Album rather than fragmenting
// per track artist; the Track is the playable leaf.
type MusicIdentity struct {
	// Artist is the track's performing artist (the "artist" tag); the display
	// artist of the Track. Falls back to the path's top folder when untagged.
	Artist string
	// AlbumArtist groups the Album: the "album_artist" tag, falling back to
	// Artist when absent. This is the Artist ENTITY a compilation files under.
	AlbumArtist string
	// Album is the album title (the "album" tag), falling back to the parent
	// folder name (year stripped) from the path.
	Album string
	// AlbumYear is the album's year parsed from the "date" tag (or the path's
	// "Album (Year)" folder); 0 when unknown.
	AlbumYear int
	// Title is the track title (the "title" tag), falling back to the filename
	// (track-number prefix and extension stripped).
	Title string
	// Disc / Track are the disc and track numbers (the "disc"/"track" tags, which
	// may be "N" or "N/total"); 0 when absent. Disc defaults to 1 for ordering.
	Disc  int
	Track int
	// Genre is the "genre" tag, "" when absent (descriptive only).
	Genre string
	// ArtistMBID / ReleaseGroupMBID / RecordingMBID are the MusicBrainz ids the
	// FILE asserts, read straight from its tags (ADR-0049). They are DECORATION
	// anchors: Enrichment resolves the artist / album / track by id — a lookup —
	// instead of guessing with a text search. Empty for an untagged file, which
	// falls back to the search exactly as before.
	//
	// They are not identity. The release-group id separately does an identity job
	// inside AlbumKey (ADR-0038); these fields never enter a key, and an Admin's
	// Enrichment override outranks all three (ADR-0045).
	ArtistMBID       string
	ReleaseGroupMBID string
	RecordingMBID    string
	// ReleaseMBID is the MusicBrainz RELEASE id the file asserts — the exact
	// edition, read from the "musicbrainz_albumid" tag (ADR-0050). Where
	// ReleaseGroupMBID names the work, this names the pressing whose tracklist has
	// the numbering these files were ripped against, so a matched Album can
	// resolve its own Tracks instead of searching for each one by name.
	//
	// Like the three above it: a DECORATION anchor, re-derived from disk on every
	// scan, never identity (the release-GROUP id already does that job inside
	// AlbumKey, ADR-0038) and never an enrichment record (ADR-0045).
	ReleaseMBID string
	// ReleaseType is the album's normalized primary release type from the
	// release-type tag ("album", "single", "ep", "compilation", …), "" when
	// untagged. Descriptive: the browse UI badges non-album types
	// (album-release-identity/02); identity uses the tag via the AlbumKey
	// tiers (ADR-0038), never this field.
	ReleaseType string
	// ArtistKey / AlbumKey / TrackKey are the stable dedup keys (normalized) used
	// to re-resolve the same Artist / Album / Track across rescans (ADR-0014). The
	// Album key includes the AlbumArtist so two artists' same-named albums
	// ("Greatest Hits") do not merge; the Track key includes disc+track+title so
	// two tracks never collide.
	ArtistKey string
	AlbumKey  string
	TrackKey  string
	// FromTags is true when the identity came from embedded tags, false when it
	// fell back to the path layout. A path-fallback Track is flagged needs-review
	// (a best-effort parse — naming-convention.md "Filed + needs-review").
	FromTags bool
}

// trackNumRe pulls the leading numeric component out of a "track"/"disc" tag,
// which is commonly "3" or "3/12" (the count after the slash is ignored).
var trackNumRe = regexp.MustCompile(`^\s*(\d+)`)

// yearTagRe pulls a 4-digit year out of a "date" tag, which may be a full
// ISO date ("2021-06-01"), a bare year ("2021"), or year-with-extra.
var yearTagRe = regexp.MustCompile(`(19\d{2}|20\d{2})`)

// trackFilePrefixRe matches a leading "NN - " / "NN. " / "NN " track-number
// prefix on a filename, stripped when deriving a title from the path.
var trackFilePrefixRe = regexp.MustCompile(`^\s*\d{1,3}\s*[-_.) ]+\s*`)

// albumFolderYearRe matches a trailing "(YYYY)" on an album folder name,
// e.g. "Greatest Hits (2021)" → album "Greatest Hits", year 2021.
var albumFolderYearRe = regexp.MustCompile(`^(.*?)[\s_]*\((19\d{2}|20\d{2})\)\s*$`)

// artistIdentityKey is the Artist grouping key for an album-artist name. On top
// of normalizeTitle it folds two spelling families that split one band into two
// rows (ADR-0037):
//   - "AND"-INSENSITIVE: every standalone "and" word is dropped, joining the
//     spellings normalizeTitle already collapses ("&", "+") — so "Marina and
//     the Diamonds" and "Marina & the Diamonds" resolve to ONE Artist;
//   - ARTICLE-INSENSITIVE: a leading "the "/"an "/"a " word is then stripped,
//     so "The Smashing Pumpkins" and "Smashing Pumpkins" resolve to ONE Artist.
//
// Both folds apply AFTER normalization — unlike sortTitle, which strips from
// the raw name — so the new key is derivable from the stored key text alone,
// which migrations 0042/0043 rely on to merge pre-existing rows without
// re-probing any file. The "and" drop runs first: it can expose a leading
// article ("And The X" → "the x" → "x"), mirroring how "& The X" normalizes.
func artistIdentityKey(albumArtist string) string {
	name := stripAndWords(normalizeTitle(albumArtist))
	return "artist:" + stripLeadingArticle(name)
}

// stripAndWords drops every standalone "and" word from an already-normalized
// (lower-cased, single-spaced) name. normalizeTitle collapses "&" and "+" to a
// space but the WORD "and" survives it, so the two spellings of one band keyed
// apart; dropping the word folds them together (ADR-0037 amendment, migration
// 0043). A name consisting ONLY of "and" is kept whole — a key never empties.
func stripAndWords(s string) string {
	fields := strings.Fields(s)
	kept := fields[:0]
	for _, f := range fields {
		if f != "and" {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 || len(kept) == len(fields) {
		return s
	}
	return strings.Join(kept, " ")
}

// MusicIdentityFromTags derives the music identity of one audio file from its
// extracted tags, falling back to the file's path layout
// (`Artist/Album (Year)/NN - Title.ext`) for any field the tags do not supply
// (naming-convention.md: "path is fallback only"). It returns ok=false only when
// neither tags nor path yield a minimal identity (no artist AND no album AND no
// usable title) — the caller routes that file to Unmatched, never auto-guessed.
//
// Grouping: the Album is keyed under AlbumArtist (falling back to Artist), so a
// compilation whose tracks have varied "artist" tags but a shared "album_artist"
// (e.g. "Various Artists") stays ONE Album.
func MusicIdentityFromTags(tags map[string]string, path string) (MusicIdentity, bool) {
	get := func(k string) string {
		if tags == nil {
			return ""
		}
		return strings.TrimSpace(tags[k])
	}

	artist := get("artist")
	albumArtist := get("album_artist")
	album := get("album")
	title := get("title")
	disc := parseLeadingInt(get("disc"))
	track := parseLeadingInt(get("track"))
	year := parseYearTag(get("date"))
	genre := get("genre")

	// Any music tag present means tag-derived identity; otherwise fall back to the
	// path layout. "Tagged" is "has at least one of the identity-bearing tags".
	fromTags := artist != "" || albumArtist != "" || album != "" || title != ""

	// Path fallback: Artist/Album (Year)/NN - Title.ext. The grandparent folder is
	// the Artist, the parent folder the Album (year stripped), and the filename the
	// Title (track-number prefix stripped). Each only fills a field the tags left
	// empty (local tags always win — naming-convention.md).
	pArtist, pAlbum, pYear, pTitle, pTrack := parseMusicPath(path)
	if artist == "" {
		artist = pArtist
	}
	if album == "" {
		album = pAlbum
		if year == 0 {
			year = pYear
		}
	}
	if title == "" {
		title = pTitle
	}
	if track == 0 {
		track = pTrack
	}

	// Album Artist grouping: an Album files under its album_artist tag, falling
	// back to the (track) Artist when absent — so a compilation with a shared
	// album_artist stays one Album even as track artists vary.
	if albumArtist == "" {
		albumArtist = artist
	}

	// A file with no artist, no album, and no title yields no minimal identity.
	if artist == "" && album == "" && title == "" {
		return MusicIdentity{}, false
	}

	// Defaults that keep ordering sane: an Album with no album-artist is filed
	// under "Unknown Artist"; a disc-less track is disc 1.
	if albumArtist == "" {
		albumArtist = "Unknown Artist"
		artist = albumArtist
	}
	if album == "" {
		album = "Unknown Album"
	}
	if title == "" {
		// A nameless track falls back to its filename (extension stripped) so it is
		// never blank in browse.
		title = stripExt(filepath.Base(path))
	}
	if disc <= 0 {
		disc = 1
	}

	id := MusicIdentity{
		Artist:      artist,
		AlbumArtist: albumArtist,
		Album:       album,
		AlbumYear:   year,
		Title:       title,
		Disc:        disc,
		Track:       track,
		Genre:       genre,
		ReleaseType: releaseTypeTag(tags),
		FromTags:    fromTags,
	}
	id.ArtistMBID = artistMBID(tags)
	id.ReleaseGroupMBID = releaseGroupID(tags)
	id.RecordingMBID = recordingMBID(tags)
	id.ReleaseMBID = releaseID(tags)
	id.ArtistKey = artistIdentityKey(albumArtist)
	// Album identity is (album artist, album title) ONLY — deliberately NOT the
	// year. A compilation ("Greatest Hits") commonly tags each track with its
	// original release year, so embedding the year here would split one album into
	// one-per-year. The year stays a descriptive field (Album.Year) for display +
	// sorting, resolved from the album's tracks during tree assembly.
	//
	// Same-titled releases (an LP and its title single, whose album tags may
	// differ only in punctuation normalizeTitle strips) are disambiguated by the
	// tags' own release identity when present (ADR-0038): an embedded MusicBrainz
	// release-group id IS the album identity — the music analogue of the Movie
	// "embedded external id wins" rule (identityKey, identity.go) — and failing
	// that, a release-type tag joins the title key so an LP and a single still
	// split. Files with neither tag key exactly as before (zero key churn).
	switch rgid, relType := releaseGroupID(tags), releaseTypeTag(tags); {
	case rgid != "":
		id.AlbumKey = id.ArtistKey + "|album-mbrg:" + rgid
	case relType != "":
		id.AlbumKey = id.ArtistKey + "|album:" + normalizeTitle(album) + "|type:" + relType
	default:
		id.AlbumKey = id.ArtistKey + "|album:" + normalizeTitle(album)
	}
	id.TrackKey = id.AlbumKey + "|d" + pad2(disc) + "t" + pad2(track) + ":" + normalizeTitle(title)
	return id, true
}

// releaseGroupID returns the file's MusicBrainz release-group id, lower-cased,
// "" when untagged. The key spelling varies by container: Vorbis comments
// (FLAC/OGG) surface as "musicbrainz_releasegroupid", ID3v2 TXXX frames (MP3)
// and MP4 freeform atoms as "musicbrainz release group id" (collectTags already
// lower-cased both).
func releaseGroupID(tags map[string]string) string {
	for _, k := range []string{"musicbrainz_releasegroupid", "musicbrainz release group id"} {
		if v := strings.TrimSpace(tags[k]); v != "" {
			return strings.ToLower(v)
		}
	}
	return ""
}

// releaseID returns the file's MusicBrainz RELEASE id — the exact edition, the
// one `/ws/2/release/<mbid>?inc=recordings` has a tracklist for — lower-cased,
// "" when untagged or malformed.
//
// The tag Picard writes it to is named after the ALBUM, not the release:
//
//	release id        →  Vorbis MUSICBRAINZ_ALBUMID, ID3 TXXX / MP4 "MusicBrainz Album Id"
//	release-group id  →  Vorbis MUSICBRAINZ_RELEASEGROUPID, "MusicBrainz Release Group Id"
//	ALBUM ARTIST id   →  Vorbis MUSICBRAINZ_ALBUMARTISTID, "MusicBrainz Album Artist Id"
//
// The third is the trap, and it is the same trap recordingMBID documents: the
// keys are matched EXACTLY, never by prefix or substring, because
// "musicbrainz_albumartistid" begins with "musicbrainz_album" and an artist id
// handed to /release/ 404s — a confident wrong answer where an empty id would
// have left the album to pick its release honestly (ADR-0049, ADR-0050).
//
// Unlike releaseGroupID, which must stay byte-for-byte what AlbumKey has always
// embedded (ADR-0038), this id is pure decoration and so is UUID-validated and
// multi-value-split through firstMBID.
//
// This is the id an ID3 tag actually surfaces: Picard hides the recording id in
// the binary UFID frame ffprobe cannot read, but writes this one to a TXXX frame
// it can — so a Picard-tagged MP3 library, precisely the population ADR-0049 left
// on the search path, has its exact anchor here.
func releaseID(tags map[string]string) string {
	return firstMBID(tags, "musicbrainz_albumid", "musicbrainz album id")
}

// recordingMBID returns the file's MusicBrainz RECORDING id — the one
// `/ws/2/recording/<mbid>` resolves — lower-cased, "" when untagged or malformed.
//
// The spelling is a genuine trap. Picard stores the RECORDING id under the key
// named "track":
//
//	recording id  →  Vorbis MUSICBRAINZ_TRACKID, MP4/ID3 "MusicBrainz Track Id"
//	track id      →  Vorbis MUSICBRAINZ_RELEASETRACKID, "MusicBrainz Release Track Id"
//
// The second is the release-specific track, and it is NOT a recording: handing it
// to /recording/ returns 404, which the pass would file as "no such record" and
// park the Track as unmatched. So the keys are matched EXACTLY — never by prefix
// or substring, which would happily swallow "musicbrainz_releasetrackid".
//
// ID3's canonical home for the recording id is the binary UFID frame, which
// ffprobe does not surface; MP3s tagged only that way have no id here and fall
// back to the search, as before.
func recordingMBID(tags map[string]string) string {
	return firstMBID(tags, "musicbrainz_trackid", "musicbrainz track id")
}

// artistMBID returns the file's MusicBrainz ARTIST id, preferring the ALBUM
// artist's — the Artist entity an Album files under is the album-artist
// (MusicIdentityFromTags), so the id has to describe the same one, or a
// compilation would decorate its "Various Artists" row from the first track
// artist it happened to see.
func artistMBID(tags map[string]string) string {
	if id := firstMBID(tags, "musicbrainz_albumartistid", "musicbrainz album artist id"); id != "" {
		return id
	}
	return firstMBID(tags, "musicbrainz_artistid", "musicbrainz artist id")
}

// mbidRe matches a bare UUID, the shape every MusicBrainz id has.
var mbidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// firstMBID reads the first well-formed MusicBrainz id from the first of keys
// that is present, lower-cased; "" when none is.
//
// Two defences, both load-bearing:
//
//   - MULTI-VALUED tags. A collaboration carries several artist ids, which ffprobe
//     hands over joined by ";" or "/". The first is the primary credit; the joined
//     string is not an id at all.
//   - VALIDATION. Only a real UUID is used. An unvalidated id is worse than no id:
//     the lookup 404s, the provider maps that to ErrNoMatch, and the item is filed
//     as "no such record" — a confident wrong answer, where no id at all would have
//     produced a correct search.
func firstMBID(tags map[string]string, keys ...string) string {
	for _, k := range keys {
		v, ok := tags[k]
		if !ok {
			continue
		}
		for _, part := range strings.FieldsFunc(v, func(r rune) bool {
			return r == ';' || r == '/' || r == ',' || r == ' '
		}) {
			if id := strings.ToLower(strings.TrimSpace(part)); mbidRe.MatchString(id) {
				return id
			}
		}
	}
	return ""
}

// releaseTypeTag returns the file's normalized primary release type ("album",
// "single", "ep", "compilation", …), "" when untagged. Reads the Vorbis
// "releasetype" comment and its ID3v2/MP4 spelling "musicbrainz album type".
// The tag may be multi-valued ("album; compilation" / "album/compilation");
// the primary type is the first value.
func releaseTypeTag(tags map[string]string) string {
	for _, k := range []string{"releasetype", "musicbrainz album type"} {
		if v := tags[k]; v != "" {
			return primaryReleaseType(v)
		}
	}
	return ""
}

// primaryReleaseType reduces a release-type tag to its first value, normalized
// (lower-cased, punctuation collapsed): "Album; Compilation" → "album".
func primaryReleaseType(s string) string {
	for _, sep := range []string{";", "/", ","} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	return normalizeTitle(s)
}

// AlbumOverride is the corrected Album identity from a folder-keyed Match
// override (issue tv-music/04). For Music the override anchors to an ALBUM folder
// (the directory holding the tracks); it re-points the Album the folder's tracks
// group under, the Music analogue of a Show/Movie folder override. Album/Year are
// the corrected display fields; Key is the override's stable identity_key (so the
// grouping is deterministic and survives rescans).
type AlbumOverride struct {
	Album string
	Year  int
	Key   string
}

// MusicIdentityFromTagsWithOverride is MusicIdentityFromTags, plus an Admin's
// folder-keyed Album Match override (issue tv-music/04). When ov is nil it is
// exactly MusicIdentityFromTags (the unchanged tag/path path). When ov is set:
//   - the Album identity (title, year, and grouping key) is forced to the
//     override, so every track in the corrected folder collapses into the one
//     Album the Admin asserted — overruling fragmented/mis-tagged album tags;
//   - the track is treated as a confirmed match (FromTags=true → NOT
//     needs-review), since the Admin deliberately matched it;
//   - a file with no tag/path identity at all is RESCUED (the override asserts it
//     belongs to this Album), mirroring how a Movie/Show folder override rescues
//     an unparseable folder. Artist/Title/track-number then come from the path
//     (or sane fallbacks) since the override only carries the Album.
//
// The Artist grouping is left to the tags/path (the override re-points the Album,
// not the Artist); an album-artist-less rescue files under "Unknown Artist".
func MusicIdentityFromTagsWithOverride(tags map[string]string, path string, ov *AlbumOverride) (MusicIdentity, bool) {
	id, ok := MusicIdentityFromTags(tags, path)
	if ov == nil {
		return id, ok
	}
	if !ok {
		// Rescue: no tag/path identity, but the override asserts this file's Album.
		// Derive Artist/Title/track from the path; Album comes from the override.
		pArtist, _, _, pTitle, pTrack := parseMusicPath(path)
		id = MusicIdentity{Artist: pArtist, AlbumArtist: pArtist, Title: pTitle, Track: pTrack, Disc: 1}
		if strings.TrimSpace(id.Title) == "" {
			id.Title = stripExt(filepath.Base(path))
		}
		if strings.TrimSpace(id.AlbumArtist) == "" {
			id.AlbumArtist = "Unknown Artist"
			id.Artist = "Unknown Artist"
		}
	}
	if id.Disc <= 0 {
		id.Disc = 1
	}
	id.Album = ov.Album
	id.AlbumYear = ov.Year
	id.FromTags = true // an Admin-confirmed match is never needs-review
	id.ArtistKey = artistIdentityKey(id.AlbumArtist)
	// A distinct "album-override:" namespace on the override's identity_key keeps
	// the corrected Album from colliding with any tag-derived AlbumKey.
	id.AlbumKey = id.ArtistKey + "|album-override:" + ov.Key
	id.TrackKey = id.AlbumKey + "|d" + pad2(id.Disc) + "t" + pad2(id.Track) + ":" + normalizeTitle(id.Title)
	return id, true
}

// parseMusicPath extracts the path-fallback fields from an audio file's path,
// per the `Artist/Album (Year)/NN - Title.ext` layout. Any component that does
// not exist (a bare file at a library root) is returned empty. It is pure (no
// filesystem) — it only splits the given path string.
func parseMusicPath(path string) (artist, album string, year int, title string, track int) {
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	base := stripExt(filepath.Base(clean))

	// Title from the filename: strip a leading "NN - " track-number prefix.
	title = strings.TrimSpace(trackFilePrefixRe.ReplaceAllString(base, ""))
	if title == "" {
		title = strings.TrimSpace(base)
	}
	if m := trackFilePrefixRe.FindStringSubmatch(base); m != nil {
		if n := parseLeadingInt(strings.TrimSpace(m[0])); n > 0 {
			track = n
		}
	}

	albumFolder := filepath.Base(dir)
	if albumFolder != "." && albumFolder != string(filepath.Separator) {
		if m := albumFolderYearRe.FindStringSubmatch(albumFolder); m != nil {
			album = strings.TrimSpace(m[1])
			year, _ = strconv.Atoi(m[2])
		} else {
			album = strings.TrimSpace(albumFolder)
		}
	}

	artistFolder := filepath.Base(filepath.Dir(dir))
	if artistFolder != "." && artistFolder != string(filepath.Separator) {
		artist = strings.TrimSpace(artistFolder)
	}
	return artist, album, year, title, track
}

// parseLeadingInt parses the leading integer of a tag like "3" or "3/12",
// returning 0 when none is present.
func parseLeadingInt(s string) int {
	m := trackNumRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// parseYearTag extracts a 4-digit year from a "date" tag (full ISO date or bare
// year), 0 when none is present.
func parseYearTag(s string) int {
	m := yearTagRe.FindString(s)
	if m == "" {
		return 0
	}
	n, _ := strconv.Atoi(m)
	return n
}
