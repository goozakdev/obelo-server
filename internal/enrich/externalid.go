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
//   - yearFromDate is a plain date parser several sources use (MusicBrainz reads
//     it three times). It never belonged to TMDB; it merely lived next door.

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
