// Package builtins registers the Plugins compiled into this server (ADR-0057
// decision 5). A Built-in is a Plugin like any other — it goes through the same
// contract an Installed plugin will, and nothing downstream distinguishes the two.
//
// Registration is EXPLICIT: the composition root calls Register and that is the
// only way a Plugin reaches a running server. There is no init() side effect
// anywhere, so a test composes a server with exactly the Plugins it means and no
// others, and a future loader adds Installed plugins to the same Registry value
// these Built-ins were registered into.
//
// This package is the only place that knows both the contract and the Built-in
// implementations, which keeps the dependency arrows one-way: a Plugin package
// knows the contract and its own source; a consuming service knows the contract
// and its own domain; neither knows the other.
package builtins

import (
	"github.com/goozakdev/obelo-server/internal/builtins/opensubtitles"
	"github.com/goozakdev/obelo-server/internal/builtins/webhook"
	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
)

// Register adds every Built-in to reg. It is called once, from the composition
// root, before anything reads the Registry. It panics on a malformed or duplicate
// registration, which is a programming error in this file and is better found at
// boot than by an Admin.
func Register(reg *pluginapi.Registry) {
	RegisterSubtitleProviders(reg)
	RegisterEventSinks(reg)
}

// RegisterSubtitleProviders adds the Built-in Subtitle providers. OpenSubtitles is
// the only one, and the static facts here — its name, that it needs a key, its
// default host, the copy the settings screen shows — are the catalog that used to
// be a package-level slice in the subtitle domain (ADR-0021's registry.go).
func RegisterSubtitleProviders(reg *pluginapi.Registry) {
	reg.RegisterSubtitleProvider(pluginapi.SubtitleProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        opensubtitles.Slug,
			Name:        "OpenSubtitles",
			RequiresKey: true,
			// Searching is what a Subtitle provider is for, so the declaration is
			// honest rather than load-bearing here; the host calls search either way.
			Capabilities: []pluginapi.Capability{pluginapi.CapabilitySearch},
			DefaultURL:   opensubtitles.DefaultBaseURL,
			Description:  "Community subtitle database. Matches your exact release by content hash for in-sync subtitles; requires a free API key.",
			DocsURL:      "https://www.opensubtitles.com/en/consumers",
		},
		New: opensubtitles.New,
	})
}

// RegisterEventSinks adds the Built-in Event sinks. The Webhook is the only one,
// and it is the first Plugin at this Extension point — the piece of the plugin
// system the maintainer would use today (ADR-0057, "Why").
//
// RequiresKey is true because a sink's secret is its SIGNING key: enabling one
// without it would post unsigned documents a receiver has no way to trust, so the
// settings endpoint refuses it for exactly the reason it refuses a key-requiring
// provider with no key.
func RegisterEventSinks(reg *pluginapi.Registry) {
	reg.RegisterEventSink(pluginapi.EventSinkRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        webhook.Slug,
			Name:        "Webhook",
			RequiresKey: true,
			Description: "POST one signed JSON document per event to a URL you choose. " +
				"Each request is signed with HMAC-SHA256 over the body under your secret, " +
				"so your receiver can reject anything this server did not send.",
		},
		New: webhook.New,
	})
}
