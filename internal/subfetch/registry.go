package subfetch

// The subtitle-provider catalog (ADR-0021), mirroring enrich/registry.go. Static
// facts about each provider — its name, whether it needs a key, its default host
// — live here as CODE, not DB columns, so adding a provider is a registry entry,
// never a migration. Only MUTABLE state (enabled, api key, base-url override)
// lives in the DB. OpenSubtitles is the first (and, this slice, only) provider.

// SlugOpenSubtitles is the stable provider slug persisted in settings and used in
// the settings API routes.
const SlugOpenSubtitles = "opensubtitles"

// registryOpenSubtitlesBaseURL is the public OpenSubtitles REST API host used when
// no override is configured.
const registryOpenSubtitlesBaseURL = "https://api.opensubtitles.com/api/v1"

// RegistryEntry describes one known subtitle provider statically (analogue of
// enrich.RegistryEntry).
type RegistryEntry struct {
	Slug string
	Name string
	// RequiresKey reports whether the source needs an API key to be enabled. A
	// key-requiring provider can't be turned on with no key on file (the API
	// validates this) and its test-connection fails fast without a call.
	RequiresKey bool
	// DefaultBaseURL is the public endpoint used when no base-URL override is set.
	DefaultBaseURL string
	// Description + DocsURL are human-facing copy for the settings screen.
	Description string
	DocsURL     string
}

// registry is the ordered catalog. Order is stable so the API/UI list is
// deterministic.
var registry = []RegistryEntry{
	{
		Slug:           SlugOpenSubtitles,
		Name:           "OpenSubtitles",
		RequiresKey:    true,
		DefaultBaseURL: registryOpenSubtitlesBaseURL,
		Description:    "Community subtitle database. Matches your exact release by content hash for in-sync subtitles; requires a free API key.",
		DocsURL:        "https://www.opensubtitles.com/en/consumers",
	},
}

// Registry returns the static provider catalog (a copy of the ordering; the
// entries are immutable value types).
func Registry() []RegistryEntry {
	out := make([]RegistryEntry, len(registry))
	copy(out, registry)
	return out
}

// RegistryEntryFor returns the catalog entry for a slug, or ok=false for an
// unknown slug (the API rejects an unknown slug as PROVIDER_UNKNOWN).
func RegistryEntryFor(slug string) (RegistryEntry, bool) {
	for _, e := range registry {
		if e.Slug == slug {
			return e, true
		}
	}
	return RegistryEntry{}, false
}
