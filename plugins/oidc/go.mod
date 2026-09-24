// The OpenID Connect Bundled plugin (ADR-0063 decision 1): a redirect-flow
// Sign-in provider, and a module of its own for the reason every Bundled plugin
// is one — it depends on the Obelo SDK and on nothing else, so the only way a
// change to the server can reach this code is through the contract.
//
// The two `replace` lines are what make a build work from a working tree; see
// plugins/opensubtitles/go.mod.
module github.com/goozakdev/obelo-server/plugins/oidc

go 1.26

require (
	github.com/goozakdev/obelo-server/pluginapi v0.0.0
	github.com/goozakdev/obelo-server/pluginsdk v0.0.0
)

replace github.com/goozakdev/obelo-server/pluginapi => ../../pluginapi

replace github.com/goozakdev/obelo-server/pluginsdk => ../../pluginsdk
