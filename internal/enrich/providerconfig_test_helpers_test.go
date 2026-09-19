package enrich

import "time"

// Test shorthand for building a ProviderConfig.
//
// ProviderConfig stopped carrying a named field per shipped provider in
// .scratch/bundled-plugins issue 01 — it is three open maps now, keyed by provider
// id — which is right for the host and verbose in a table of small configs. These
// helpers keep the tests reading as they did: `testConfig(withKey(SlugTMDB, "k"),
// withActive(SlugMusicBrainz, true))` is the old `ProviderConfig{TMDBAPIKey: "k",
// MusicBrainzEnabled: true}`, with the provider named in the argument instead of in
// the field.

// testConfig builds a ProviderConfig from option functions.
func testConfig(opts ...func(*ProviderConfig)) ProviderConfig {
	var c ProviderConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// withKey gives a provider an API key, which (absent an explicit active fact) is
// also what makes it active.
func withKey(slug, key string) func(*ProviderConfig) {
	return func(c *ProviderConfig) {
		if c.ProviderKeys == nil {
			c.ProviderKeys = map[string]string{}
		}
		c.ProviderKeys[slug] = key
	}
}

// withActive states a provider's explicit on/off fact — what a keyless source's
// enablement is, and what the old MusicBrainzEnabled field meant.
func withActive(slug string, on bool) func(*ProviderConfig) {
	return func(c *ProviderConfig) {
		if c.ProviderActive == nil {
			c.ProviderActive = map[string]bool{}
		}
		c.ProviderActive[slug] = on
	}
}

// withURLs points a provider at its hosts.
func withURLs(slug, url, url2 string) func(*ProviderConfig) {
	return func(c *ProviderConfig) {
		if c.ProviderEndpoints == nil {
			c.ProviderEndpoints = map[string]ProviderEndpoint{}
		}
		c.ProviderEndpoints[slug] = ProviderEndpoint{URL: url, URL2: url2}
	}
}

// withRateLimit states the operator's pacing policy, which every provider receives.
func withRateLimit(d time.Duration) func(*ProviderConfig) {
	return func(c *ProviderConfig) {
		ms := int(d / time.Millisecond)
		c.RateLimitMillis = &ms
	}
}

// withMetadataLanguage sets the server-wide metadata language.
func withMetadataLanguage(lang string) func(*ProviderConfig) {
	return func(c *ProviderConfig) { c.MetadataLanguage = lang }
}

// withVideoLead repoints the video kind's Authoritative provider. Its twin
// withMusicLead went with the last test that repointed music
// (.scratch/bundled-plugins: issue 08); ProviderConfig.AuthoritativeMusic is set
// through SettingsToProviderConfig everywhere that still matters.
func withVideoLead(slug string) func(*ProviderConfig) {
	return func(c *ProviderConfig) { c.AuthoritativeVideo = slug }
}
