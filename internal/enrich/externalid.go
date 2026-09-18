package enrich

import (
	"strconv"
	"strings"
)

// The three small parsers that survived TMDB leaving this package
// (.scratch/bundled-plugins issue 04). They are here rather than in
// plugins/tmdb/ because the HOST is what calls them, and the host still needs
// them after the provider has gone:
//
//   - parseTMDBRef and isDigits are read by Service.ResolveExternalRef. A Plugin
//     that declares CapabilityExternalRef answers a paste for itself, and its
//     answer stands; a host that gets "unavailable" is free to read the paste for
//     the id namespaces IT owns, and `titles.tmdb_id` is one of the host's own
//     columns (ADR-0045/0049). Deleting this would mean an Admin's pasted TMDB URL
//     stopped working the moment TMDB became a plugin, for a column the plugin has
//     no say over. Follow-up issue 10 — a source-namespaced external-id map on the
//     Title — is what retires it.
//   - yearFromDate is a plain date parser several sources use. It never belonged
//     to TMDB; it merely lived next door. After MusicBrainz left too
//     (.scratch/bundled-plugins issue 06) it has no caller in this package — the
//     plugin carries its own — and it is LEFT here deliberately for issue 08's
//     dead-code sweep rather than deleted mid-wave.
//
// The MusicBrainz reference parsers at the bottom joined them for the same reason
// (.scratch/bundled-plugins issue 06): `titles.musicbrainz_id` is the HOST's
// column, and Service.ResolveExternalRef reads a paste for it whenever no Plugin
// claims CapabilityExternalRef. The MusicBrainz plugin DOES claim it, so on a
// stock server its answer is the one that stands and these are the fallback — but
// a server whose Admin uninstalled that plugin, or pointed a Library at some other
// music source, still has that column and still has to be able to read a URL for
// it.

// parseTMDBRef reads a pasted TMDB reference — one of that site's own URLs
// (/movie/<id> or /tv/<id>, the id optionally carrying a "-slug" suffix; any scheme/subdomain,
// optional query/fragment) or a bare numeric id — into (urlKind, id) for the paste
// escape hatch (item-editing/search-improvements). urlKind is "movie" or "tv" for
// a typed URL (so the caller can validate movie↔movie / tv↔show|season|episode),
// or "" for a bare id (the caller assumes the item's kind). ok is false when s is
// neither a positive integer nor a recognized TMDB entity URL.
func parseTMDBRef(s string) (urlKind, id string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if isDigits(s) {
		return "", s, true
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	segs := strings.Split(s, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "movie" || segs[i] == "tv" {
			num := segs[i+1]
			if j := strings.IndexByte(num, '-'); j > 0 {
				num = num[:j] // strip the "-slug" TMDB appends to shareable URLs
			}
			if isDigits(num) {
				return segs[i], num, true
			}
		}
	}
	return "", "", false
}

// isDigits reports whether s is a non-empty run of ASCII digits (a TMDB id).
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// yearFromDate extracts the 4-digit year from a provider date string
// ("YYYY-MM-DD", or just "YYYY"); 0 when absent/unparseable.
func yearFromDate(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

// --- MusicBrainz's URL vocabulary, as the HOST reads it ------------------------

// mbEntityKind maps a MusicBrainz URL entity segment to our search/lookup kind, so
// a pasted typed URL can be validated against the item being corrected.
var mbEntityKind = map[string]string{
	"release-group": "album",
	"artist":        "artist",
	"recording":     "track",
}

// mbUnsupportedEntity are real MusicBrainz URL entity segments we can't use as an
// Enrichment override — an album is a release-group (not a work or a specific
// release), a track is a recording, etc. Recognized only so the paste box can say
// "wrong kind of record" instead of "unreadable". (`release` and `work` are the ones
// users hit most: a release is one edition of a release-group; a work is the abstract
// composition — neither identifies the album/artist/track we pin.)
var mbUnsupportedEntity = map[string]bool{
	"release": true, "work": true, "label": true, "area": true, "place": true,
	"event": true, "series": true, "instrument": true, "genre": true, "url": true,
	"editor": true, "collection": true,
}

// ParseMusicBrainzRef reads a pasted MusicBrainz reference — a full URL
// (https://musicbrainz.org/release-group/<uuid>, /artist/<uuid>, /recording/<uuid>;
// any scheme/subdomain, optional slug/query/fragment) or a bare MBID (UUID) — into
// (kind, id). For a typed URL kind is the matching item kind ("album"/"artist"/
// "track") so the caller can reject an id of the wrong kind; a bare UUID returns an
// empty kind (the caller assumes the item's own kind). ok is false when s is neither
// a UUID nor a recognized entity URL — the handler surfaces that as "unreadable".
func ParseMusicBrainzRef(s string) (kind, id string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if isUUID(s) {
		return "", strings.ToLower(s), true
	}
	for i, segs := 0, refSegments(s); i+1 < len(segs); i++ {
		if k, okk := mbEntityKind[segs[i]]; okk && isUUID(segs[i+1]) {
			return k, strings.ToLower(segs[i+1]), true
		}
	}
	return "", "", false
}

// parseMusicBrainzReleaseRef returns the release MBID when s is a MusicBrainz
// /release/ URL (a specific edition, not itself an album pin — the caller resolves
// it to its parent release-group). Typed URLs only: a bare UUID is ambiguous (any
// entity), so it stays trusted for the item's own kind rather than being guessed as
// a release.
func parseMusicBrainzReleaseRef(s string) (id string, ok bool) {
	for i, segs := 0, refSegments(s); i+1 < len(segs); i++ {
		if segs[i] == "release" && isUUID(segs[i+1]) {
			return strings.ToLower(segs[i+1]), true
		}
	}
	return "", false
}

// MusicBrainzRefUnsupported reports whether s is a MusicBrainz URL naming a real but
// unsupported entity type (work/release/label/…). Lets a caller distinguish "a valid
// MusicBrainz link, wrong entity kind" from "not a MusicBrainz reference at all".
func MusicBrainzRefUnsupported(s string) bool {
	for i, segs := 0, refSegments(s); i+1 < len(segs); i++ {
		if mbUnsupportedEntity[segs[i]] && isUUID(segs[i+1]) {
			return true
		}
	}
	return false
}

// refSegments splits a pasted reference into its slash-separated segments, with any
// query or fragment removed first — the shape all three readers above walk.
func refSegments(s string) []string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Split(s, "/")
}

// isUUID reports whether s is a canonical 8-4-4-4-12 hex UUID (a MusicBrainz MBID).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}
