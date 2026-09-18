package musicbrainz

import "strings"

// MusicBrainz's own URL vocabulary, read here rather than anywhere else.
//
// The HOST keeps a copy of these three functions (internal/enrich/externalid.go)
// and that is not an accident waiting to happen: the host reads a paste only for
// the id namespaces IT keeps columns for — `titles.musicbrainz_id` is the host's
// column — and only when no Plugin declared the external-ref capability
// (ADR-0045/0049, and pluginapi.ExternalRefParser's own doc). A Plugin that
// declares it answers instead, and its answer stands. Two copies of a URL shape
// in two modules is the price of a plugin the server cannot import, and it is the
// price ADR-0059 decided to pay.

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

// ParseRef reads a pasted MusicBrainz reference — a full URL
// (https://musicbrainz.org/release-group/<uuid>, /artist/<uuid>, /recording/<uuid>;
// any scheme/subdomain, optional slug/query/fragment) or a bare MBID (UUID) — into
// (kind, id) for the paste-an-id escape hatch. For a typed URL kind is the matching
// item kind ("album"/"artist"/"track") so the caller can reject an id of the wrong
// kind; a bare UUID returns an empty kind (the caller assumes the item's own kind).
// ok is false when s is neither a UUID nor a recognized entity URL — the paste box
// surfaces that as "unreadable".
func ParseRef(s string) (kind, id string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	if IsUUID(s) {
		return "", strings.ToLower(s), true
	}
	for i, segs := 0, pathSegments(s); i+1 < len(segs); i++ {
		if k, okk := mbEntityKind[segs[i]]; okk && IsUUID(segs[i+1]) {
			return k, strings.ToLower(segs[i+1]), true
		}
	}
	return "", "", false
}

// ParseReleaseRef returns the release MBID when s is a MusicBrainz /release/ URL (a
// specific edition, not itself an album pin — the caller resolves it to its parent
// release-group). Typed URLs only: a bare UUID is ambiguous (any entity), so it
// stays trusted for the item's own kind rather than being guessed as a release.
func ParseReleaseRef(s string) (id string, ok bool) {
	for i, segs := 0, pathSegments(s); i+1 < len(segs); i++ {
		if segs[i] == "release" && IsUUID(segs[i+1]) {
			return strings.ToLower(segs[i+1]), true
		}
	}
	return "", false
}

// RefUnsupported reports whether s is a MusicBrainz URL naming a real but
// unsupported entity type (work/release/label/…). Lets a caller distinguish "a
// valid MusicBrainz link, wrong entity kind" from "not a MusicBrainz reference at
// all".
func RefUnsupported(s string) bool {
	for i, segs := 0, pathSegments(s); i+1 < len(segs); i++ {
		if mbUnsupportedEntity[segs[i]] && IsUUID(segs[i+1]) {
			return true
		}
	}
	return false
}

// pathSegments splits a pasted reference into its slash-separated segments, with
// any query or fragment removed first — the shape all three readers above walk.
func pathSegments(s string) []string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	return strings.Split(s, "/")
}

// IsUUID reports whether s is a canonical 8-4-4-4-12 hex UUID (a MusicBrainz MBID).
func IsUUID(s string) bool {
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
