package enrich

import "github.com/goozakdev/obelo-server/internal/server"

// DefaultUserAgent identifies Obelo to every metadata host it calls.
//
// MusicBrainz REQUIRES this shape — "Application name/<version> ( contact-url )"
// or "( contact-email )" — and throttles anonymous or generic agents (a blank UA,
// "Java", "Python-urllib", Go's default "Go-http-client/1.1") harder than
// identified ones. See https://musicbrainz.org/doc/MusicBrainz_API/Rate_Limiting.
// fanart.tv, TheAudioDB, OMDb, TheTVDB and AniDB all ask for the same courtesy,
// so one identity serves every provider and the artwork fetcher.
//
// Two deliberate choices:
//
// Version is read from server.Version rather than written out here. The previous
// string said "obelo/1.0" while the build was 0.1.0 — a UA whose version drifts
// from the build is worse than useless to a host trying to pin a misbehaving
// release, and a hand-maintained copy drifts by construction.
//
// The contact is the PROJECT's, not the operator's. Obelo is self-hosted, so
// there is no operator address to put here — and a host that needs to reach
// someone about a bad request pattern needs whoever ships the code, not whoever
// happens to run this instance. That is how Picard and beets identify too. There
// is deliberately no config knob: an operator's own throttle policy is already
// expressed by MusicBrainzRateLimit. Each provider still exposes a UserAgent
// field, so a test or a fork can substitute its own.
var DefaultUserAgent = "obelo/" + server.Version + " ( https://www.obelo.tv; metadata@obelo.tv )"
