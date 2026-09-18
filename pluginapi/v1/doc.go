// Package v1 is the Plugin contract (ADR-0057): the one shape every unit of code
// that talks to an external source or an outbound integration implements, whether
// it is a Built-in compiled into this binary or, from Phase 2, an Installed plugin
// an Admin added to a running server.
//
// The vocabulary is CONTEXT.md's. A **Plugin** is the code; what it provides is a
// Metadata provider, a Subtitle provider or an Event sink — the closed set of
// **Extension points** (decision 1). A **Built-in** is a Plugin compiled in and
// registered through this same contract, which is what proves the contract honest:
// if it cannot express OpenSubtitles' two-step download or TMDB's image host, that
// is discovered here, while the package can still change freely.
//
// # Wire-shaped from day one
//
// Every value that crosses this contract is a plain struct with JSON tags that
// round-trips (decision 2). No interfaces, function values, readers or channels
// appear in any call's parameters or results; the Go interfaces below exist only
// so a Built-in can be called in-process, and their method sets are wire types in,
// wire types out. Every call takes a context whose deadline the HOST sets. Byte
// payloads come back whole, with a content type, capped by a limit the caller
// states — there is no streaming. Paging is a limit and an offset (see Page).
//
// # Outcomes are values, not errors
//
// A Go error from a call means the transport or the Plugin itself failed — the
// host treats it as transient and retries (ADR-0048). Everything the host would
// otherwise learn from a sentinel error travels in the response's Outcome field as
// one of a small closed enum (see Outcome). Adapters beside each consuming service
// map an Outcome back to that service's existing sentinel, so `errors.Is` callers
// inside the server are untouched by a Plugin moving behind this contract.
//
// # What this package may not import
//
// Nothing. It imports the standard library and no internal package — in
// particular never internal/store, internal/enrich or internal/subfetch. A type
// here that needed one of those would be an internal type leaking into the wire
// shape, which is exactly the mistake this package exists to prevent (the "Why" of
// ADR-0057: Jellyfin pays for plugins on every major release because its internal
// types ARE its wire types).
//
// # What the Built-ins have exposed so far
//
// This package exists to be found wrong while that is still cheap (decision 5).
// The gaps the metadata Built-ins found, all filled additively:
//
//   - Settings needed a Language. A Metadata provider caches responses under a
//     language-dependent key, so the metadata language is part of what the Plugin
//     IS, not a per-call argument — and a language change is already a settings
//     save the host answers by rebuilding every Plugin.
//   - The capability set needed episode-list. TMDB's season and episode lists were
//     an optional Go interface the chains type-asserted on; the contract cannot
//     carry a type assertion across a boundary, so they became a declared
//     capability and a second, optional interface of wire calls.
//   - Settings needed a RateLimitMillis, for the same reason it needed Language and
//     one more: the limiter is keyed by HOST rather than held on the Plugin
//     (ADR-0049), so the operator's interval has to be known when the Plugin is
//     built. It is a pointer because "use your own default" and "do not throttle"
//     are both meaningful and neither is the other's zero.
//   - A source with TWO hosts is not only TMDB. MusicBrainz's images come from the
//     Cover Art Archive, which is a separate registration with its own settings
//     row — so the host resolves that row into the MusicBrainz Plugin's URL2. The
//     field was general enough; what needed saying is that a Descriptor's
//     DefaultURL2 and a Settings' URL2 are not obliged to come from the same
//     registration (Cover Art Archive is the one Built-in with no factory at all:
//     it has no client of its own, so there is nothing for one to construct).
//   - One capability can cover two calls, and ErrNoTracklist did NOT need an
//     Outcome. Album tracklists and album editions are the automatic and the manual
//     half of one question, so album-tracklist declares both (AlbumTracklister),
//     and "this album has no tracklist" is OutcomeNoMatch — losslessly, because
//     the call guarantees a matched answer is never empty. Adding an eighth Outcome
//     would have forced every adapter at every Extension point to decide what
//     "no-tracklist" means for a subtitle search, which is nothing.
//   - A mismatch outcome needs its DETAIL in the response, not in the enum.
//     ref-kind-mismatch is one value; the sentence the host renders names the kind
//     pasted and the kind wanted, so ExternalRefResponse carries GotKind/WantKind
//     and the adapter rebuilds the specific error from them.
//
// # What Phase 2 added, additively
//
//   - Settings needed a SECOND VARIANT, not a second struct. The fixed shape
//     (enabled, secret, url, url2, events, language, rateLimitMillis) is what a
//     Built-in needs and what the provider dialog renders, and it stays the
//     contract's default. An Installed plugin's author cannot add a named field to
//     this package or a control to a screen, so a manifest DECLARES its own typed
//     fields (ManifestSettings.Fields, settings_schema.go) and their values travel
//     in Settings.Values, keyed by the field key that declared them. One field on
//     one struct, so a Built-in is unchanged and a guest that declares nothing is
//     unchanged with it.
//
// # Two things a manifest reader gets wrong, from Phase 1
//
// Both are recorded above in their own right; they are repeated here because an
// author designing an install flow will look for them and not for a gap list.
//
//   - A registration may have NO FACTORY. Cover Art Archive is registered so the
//     settings screen renders it and an operator can override its URL, and nothing
//     is ever built from it — it has no client of its own. The builder skips a nil
//     factory rather than treating it as an error, so "every Plugin is a module"
//     is false, and an install flow that demanded one would have no way to express
//     this shape.
//   - URL2 IS NOT ALWAYS THE SAME REGISTRATION'S SETTING. TMDB's image host is
//     TMDB's own row. MusicBrainz's is Cover Art Archive's row — a separate
//     registration — which the host resolves into the MusicBrainz Plugin's
//     Settings.URL2. A manifest that assumed one settings row per Plugin URL gets
//     Cover Art Archive wrong.
//
// # Lifetime: frozen, additive only
//
// The package churned freely through Phase 1 while the nine Built-ins exposed its
// gaps (decision 7). It has now left internal/, lives at the module root, and
// FROM THIS COMMIT v1 CHANGES ADDITIVELY ONLY:
//
//   - A field may be ADDED to a wire struct. No field is ever removed, renamed or
//     retyped, and no existing field becomes required.
//   - A VALUE may be added to an enum — an Outcome, a Capability, an Extension
//     point, an event type. No value is ever removed or respelled. A guest that
//     meets a value it does not know treats it as it treats an unavailable
//     capability: a known state, not an error.
//   - A CALL may be added behind a new Capability, so a Plugin that does not
//     declare it is never asked. No existing call's parameters or results change.
//   - Unknown fields are TOLERATED in both directions, which is what makes the
//     above safe: the generated schema leaves additionalProperties open on every
//     type, a guest must ignore a field a later host sends, and a host must ignore
//     a field a guest sends back.
//
// Anything else is v2: a second directory beside this one, with both served at
// once until nothing is left on v1.
//
// # The schema is the contract
//
// The Go package is a convenience for a Built-in compiled into this binary. What
// an author in another language reads is pluginapi.schema.json, checked in beside
// these files and generated from these structs by internal/schemagen:
//
//	go generate ./pluginapi/v1
//
// TestCheckedInSchemaIsNotStale regenerates it in memory and fails if the file
// differs, and TestGoldenDocumentsValidateAgainstTheSchema runs every golden
// document the round-trip suite pins through the checked-in file — so the schema
// cannot drift from the structs, and neither can drift from what this server
// actually sends. Two rules that are prose up here are real constraints down
// there, because an author reads the schema and not this comment: an EventActor
// carries a user id or a link id and never both (both absent is legal), and a
// SinkEvent carries a scan block or an enrich block and never both.
//
// # What this package may not import, restated
//
// Still nothing but the standard library. The schema generator needs reflect and
// a JSON writer of its own, which is exactly why it is a neighbouring package
// under internal/schemagen and not a function here.
//
// Imported as `pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"`.
package v1

//go:generate go run ./internal/schemagen/cmd/schemagen
