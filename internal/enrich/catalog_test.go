package enrich

import (
	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
)

// The catalog these tests run against. Since ADR-0057 the Metadata provider
// catalog is a VALUE the composition root derives from the Plugin registry, so a
// test that wants the eight Built-ins has to compose them the same way app.New
// does — register MetadataPlugins() into a Registry and derive a Catalog from it.
// That is the point: there is no package-level catalog to reach for any more, and
// what these suites assert is what a server composed with THESE Plugins does.
//
// It is built per call rather than once in a package variable so no test can leave
// a mutated catalog behind for the next one.

func builtinCatalog() Catalog {
	reg := pluginapi.NewRegistry()
	for _, plugin := range MetadataPlugins() {
		reg.RegisterMetadataProvider(plugin)
	}
	return NewCatalog(reg)
}

// buildProvider composes the chain from the Built-in catalog — the production
// path (BuilderFor's BuildFunc) with the catalog spelled out.
func buildProvider(cfg ProviderConfig) (MetadataProvider, Enablement) {
	return builtinCatalog().BuildProvider(cfg)
}

// pluginSlug reports which registered Plugin a composed video provider IS, or ""
// for anything that did not come through the contract. Since ADR-0057 the chain
// holds adapted Plugins rather than a source's concrete Go type, so "TMDB leads
// and OMDb supplements" is asserted by SLUG — which is also the honest question,
// because the whole point of the contract is that the chain cannot tell a Built-in
// from an Installed plugin by its type.
func pluginSlug(p MetadataProvider) string {
	adapted, ok := p.(pluginProvider)
	if !ok {
		return ""
	}
	return adapted.desc.Slug
}
