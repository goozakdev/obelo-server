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
	"github.com/goozakdev/obelo-server/internal/builtins/webhook"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Register adds every Built-in to reg. It is called once, from the composition
// root, before anything reads the Registry. It panics on a malformed or duplicate
// registration, which is a programming error in this file and is better found at
// boot than by an Admin.
//
// ONE BUILT-IN REMAINS: the Webhook sink. Nothing that talks to an external
// source is compiled into this binary any more (ADR-0059, decision 11 as amended
// by .scratch/bundled-plugins issue 09). The eight metadata sources left in
// issues 04-08, and OpenSubtitles — this file's last Subtitle provider — left in
// issue 09. All of them are Bundled plugins: WebAssembly modules built from
// plugins/<id>/, embedded by internal/bundled, installed into the data directory
// on first boot and reaching this same Registry through the Installed-plugin
// loader, ahead of whatever this file registers.
//
// The Webhook stays: it talks to no third-party source, only to the URL the
// operator types, and bundling it was never part of ADR-0059. What it registers
// is also what internal/plugins' duplicate
// check refuses an Admin's upload for — an id this binary answers to — which is
// now exactly `webhook`.
func Register(reg *pluginapi.Registry) {
	RegisterEventSinks(reg)
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
