// Package metadata is the Metadata provider Extension point's dispatcher: the
// eight //go:wasmexport functions of ADR-0058 decision 3, written once, in front
// of the contract's own Go interfaces.
//
// A plugin author writes a value that implements [Provider] — the same three
// methods a Built-in implements, with the same wire types in and out — and hands
// it over:
//
//	//go:build wasm
//
//	package main
//
//	import (
//		"github.com/goozakdev/obelo-server/pluginsdk"
//		"github.com/goozakdev/obelo-server/pluginsdk/metadata"
//	)
//
//	func main() {}
//
//	func init() { metadata.Serve(newProvider(pluginsdk.Sandbox())) }
//
// # Why init() and not main()
//
// A module built with -buildmode=c-shared is a WASI REACTOR: the host runs
// _initialize, which runs package initialization, and main.main is never called
// at all. A plugin that served its provider from main() would answer every call
// with "no provider" and nothing would say why. [Serve] therefore has to be
// reached from an initializer, and a call that never happened is reported in the
// plugin's last-error rather than guessed at.
//
// # Optional capabilities
//
// All eight exports exist in every module built with this package, because a
// //go:wasmexport is a compile-time fact. What a served value does not implement
// answers OutcomeUnavailable — the same "known state, not an error" the host
// produces for an undeclared capability — so declaring `episode-list` in a
// manifest and not implementing [EpisodeLister] degrades rather than breaks.
//
// The manifest is still what stops the call from being made at all: the host
// consults the declared capabilities BEFORE it calls, so an undeclared capability
// costs no call into the guest. Declare only what you implement.
//
// # Reading an id
//
// Read an external id off the reference you are handed with ref.ID(namespace) —
// pluginapi.NamespaceTMDB, pluginapi.NamespaceMusicBrainz, or a third party's
// plugin id — never from the named TMDBID/IMDBID/... fields. ID reads
// MediaRef.ExternalIDs and falls back to those v1 mirrors, so it answers an older
// host too (ADR-0060 decision 7). It is a method on the contract's own MediaRef,
// because that is the type the ref IS here.
package metadata

import pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"

// Provider is the Metadata provider Extension point's three mandatory calls. It
// is an alias for the contract's own interface and not a new one: a plugin and a
// Built-in implement THE SAME Go type, which is what makes moving a provider
// across the sandbox boundary a build change rather than a rewrite.
type Provider = pluginapi.MetadataProvider

// The three optional interfaces, each behind a declared Capability. A served
// value that implements one answers that call; one that does not answers
// unavailable.
type (
	// EpisodeLister is CapabilityEpisodeList: a series' seasons and a season's
	// episodes.
	EpisodeLister = pluginapi.EpisodeLister
	// AlbumTracklister is CapabilityAlbumTracklist: what an Album holds, and which
	// editions it has to choose from.
	AlbumTracklister = pluginapi.AlbumTracklister
	// ExternalRefParser is CapabilityExternalRef: reading a string an Admin pasted.
	ExternalRefParser = pluginapi.ExternalRefParser
)

// served is the provider this module answers with. There is no lock: a guest
// instance is single-threaded and the host serializes every call into it
// (ADR-0058 decision 7).
var served Provider

// Serve installs the provider this module answers every Metadata provider call
// with. Call it from init() — see the package comment for why main() is too late.
//
// Calling it twice replaces the provider rather than refusing, because a plugin
// that reconfigures itself during initialization is doing something reasonable
// and the last word is the one it meant.
func Serve(p Provider) { served = p }

// Served is the provider currently installed, and nil when Serve has not run. It
// exists so a native test of a plugin's own main.go can assert that its init()
// wired something up.
func Served() Provider { return served }
