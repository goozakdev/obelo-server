// Package useragent holds the ONE outbound identity this server sends, and the
// one function that stamps a Plugin's name onto it.
//
// It lives here rather than in internal/enrich because it is not enrichment's
// alone (ADR-0059 decision 7): every fetch a sandboxed guest makes through
// the host's `http_fetch` carries this same identity, stamped by the host, and a
// second copy of the string in internal/plugins would be a second thing to keep
// in step. The package depends on internal/server for the build version and on
// nothing else, so both halves can import it without either importing the other.
//
// # Why this shape
//
// MusicBrainz REQUIRES it — "Application name/<version> ( contact-url )" or
// "( contact-email )" — and throttles anonymous or generic agents (a blank UA,
// "Java", "Python-urllib", Go's default "Go-http-client/1.1") harder than
// identified ones. See https://musicbrainz.org/doc/MusicBrainz_API/Rate_Limiting.
// fanart.tv, TheAudioDB, OMDb, TheTVDB and AniDB all ask for the same courtesy,
// so one identity serves every provider, the artwork fetcher and every Plugin.
//
// Two deliberate choices:
//
// The version is read from server.Version rather than written out here. The
// string this replaced said "obelo/1.0" while the build was 0.1.0 — a UA whose
// version drifts from the build is worse than useless to a host trying to pin a
// misbehaving release, and a hand-maintained copy drifts by construction. The
// host function carried a SECOND such literal until ADR-0059; there is now one.
//
// The contact is the PROJECT's, not the operator's. Obelo is self-hosted, so
// there is no operator address to put here — and a host that needs to reach
// someone about a bad request pattern needs whoever ships the code, not whoever
// happens to run this instance. That is how Picard and beets identify too. It
// holds for a Plugin as well, which is why a guest's own User-Agent is DROPPED
// rather than sent: code running inside Obelo's fetcher, under Obelo's fetch
// policy and Obelo's allowlist, is Obelo as far as the far end is concerned. The
// plugin comment is what tells the far end which of Obelo's parts called.
package useragent

import (
	"strings"

	"github.com/goozakdev/obelo-server/internal/server"
)

// Default identifies Obelo itself to every host it calls.
const Default = "obelo/" + server.Version + " ( https://www.obelo.tv; metadata@obelo.tv )"

// ForPlugin is Default with a Plugin's id and version appended as a comment:
//
//	obelo/0.1.0 ( https://www.obelo.tv; metadata@obelo.tv ) plugin/tmdb/1.0.0
//
// The product-comment token goes AFTER the contact, which is where RFC 9110's
// User-Agent grammar puts a second product: a source reading the agent still sees
// Obelo and the contact first, and one reading further learns which Plugin.
//
// An empty id answers Default — there is nothing to name — and an empty version
// leaves the third segment off rather than sending a bare slash.
func ForPlugin(id, version string) string {
	id = clean(id)
	if id == "" {
		return Default
	}
	ua := Default + " plugin/" + id
	if v := clean(version); v != "" {
		ua += "/" + v
	}
	return ua
}

// clean makes a manifest string safe to put in a header value and readable when
// it gets there.
//
// The id is a slug the loader already validated, but the VERSION is opaque author
// prose the host never parses — so a version holding a carriage return would be a
// Plugin writing its own request headers, and one holding a kilobyte of text
// would be a Plugin filling somebody's access log. Everything outside printable
// ASCII becomes a dash, whitespace collapses into one, and the result is bounded.
func clean(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxToken {
		s = s[:maxToken]
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 0x21 && c <= 0x7e && c != '(' && c != ')' && c != ',' && c != ';':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// maxToken bounds the id and the version separately. A User-Agent is read by
// people and by log pipelines; neither wants a paragraph.
const maxToken = 64
