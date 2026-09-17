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
// # Lifetime
//
// It lives under internal/ and churns freely while the Built-ins expose its gaps
// (decision 7). The first Phase 2 issue moves it to the module root, freezes it
// additively, and generates a JSON schema for authors in other languages; the Go
// package is a convenience, the schema is the contract.
//
// Imported as `pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"`.
package v1
