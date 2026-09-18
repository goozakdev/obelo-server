package config

// The provider environment table (.scratch/bundled-plugins: issue 01).
//
// Every metadata provider an operator can configure from the environment used to
// have a named field on Config and a named `if os.Getenv(...)` beside nine others,
// and the enrichment domain then had a second ladder of `if`s deciding which of
// those fields turned which DB row on. Which is to say: the shape of the shipped
// provider set was written into four packages, and the compiler could not see the
// connection between them.
//
// Here it is one table. A row says "this variable fills this field of this
// provider", and the two things the server does with the environment — build the
// in-memory config, and seed the DB on a first boot — are both loops over it. The
// environment variables, their exact names, their defaults and their meaning are
// unchanged (ADR-0059 decision 11); what changed is that removing a shipped
// provider is now removing its rows.

// The provider ids the environment can configure. They are the same stable slugs
// the DB rows, the Descriptors and the settings API use; config keeps its own
// copies for the reason the enrichment domain does — a row it seeds must not depend
// on which Plugins happen to be registered in this process.
const (
	ProviderTMDB        = "tmdb"
	ProviderMusicBrainz = "musicbrainz"
	ProviderCoverArt    = "coverart"
	ProviderFanartTV    = "fanarttv"
	ProviderTheAudioDB  = "theaudiodb"
)

// ProviderField names which part of a provider's configuration an environment
// variable fills. It is deliberately the FIXED settings shape (ADR-0057) minus the
// parts no environment variable has ever set: a key, a base URL, a second host, and
// the on/off a keyless source needs because it has no key to infer one from.
type ProviderField string

const (
	// ProviderFieldKey is the API key / credential.
	ProviderFieldKey ProviderField = "key"
	// ProviderFieldURL is the base URL of the source's API.
	ProviderFieldURL ProviderField = "url"
	// ProviderFieldURL2 is the second host, for a source whose images come from
	// somewhere other than its API.
	ProviderFieldURL2 ProviderField = "url2"
	// ProviderFieldEnabled is the explicit opt-in a KEYLESS source needs: it has no
	// key, so nothing else could turn it on (ADR-0001 — a fresh install makes no
	// surprise outbound calls).
	ProviderFieldEnabled ProviderField = "enabled"
)

// ProviderEnvVar is one row of the table: an environment variable, the provider it
// configures, the field it fills, and the value that field holds when the variable
// is unset.
type ProviderEnvVar struct {
	Env      string
	Provider string
	Field    ProviderField
	// Default is the fallback for a URL field (the public endpoint). Empty for a key
	// or an enable flag, whose absence is their default.
	Default string
}

// ProviderEnvTable is THE table. Order is only the order Defaults and FromEnv walk
// it, so it is grouped by provider for a reader.
//
// OMDb, TheTVDB and AniDB are absent on purpose and that absence IS the audit the
// issue asked for, not an omission: none of the three has ever had an environment
// variable. They are configured from the settings screen only, and a fresh install
// seeds no row for them.
func ProviderEnvTable() []ProviderEnvVar {
	return []ProviderEnvVar{
		{Env: "OBELO_TMDB_API_KEY", Provider: ProviderTMDB, Field: ProviderFieldKey},
		{Env: "OBELO_TMDB_BASE_URL", Provider: ProviderTMDB, Field: ProviderFieldURL, Default: DefaultTMDBBaseURL},
		{Env: "OBELO_TMDB_IMAGE_BASE_URL", Provider: ProviderTMDB, Field: ProviderFieldURL2, Default: DefaultTMDBImageBaseURL},

		{Env: "OBELO_MUSICBRAINZ_ENABLED", Provider: ProviderMusicBrainz, Field: ProviderFieldEnabled},
		{Env: "OBELO_MUSICBRAINZ_BASE_URL", Provider: ProviderMusicBrainz, Field: ProviderFieldURL, Default: DefaultMusicBrainzBaseURL},

		// Cover Art Archive is a provider row today and a base URL in all but name:
		// nothing reads its enable switch, and its only live effect is the second host
		// the music lead is built with. Issue 06 re-points this variable at the
		// MusicBrainz plugin's URL2 and the `coverart` id goes; until then it keeps
		// landing exactly where it always has.
		{Env: "OBELO_COVERART_BASE_URL", Provider: ProviderCoverArt, Field: ProviderFieldURL, Default: DefaultCoverArtBaseURL},

		{Env: "OBELO_FANART_TV_API_KEY", Provider: ProviderFanartTV, Field: ProviderFieldKey},
		{Env: "OBELO_FANART_TV_BASE_URL", Provider: ProviderFanartTV, Field: ProviderFieldURL, Default: DefaultFanartTVBaseURL},

		{Env: "OBELO_THEAUDIODB_API_KEY", Provider: ProviderTheAudioDB, Field: ProviderFieldKey},
		{Env: "OBELO_THEAUDIODB_BASE_URL", Provider: ProviderTheAudioDB, Field: ProviderFieldURL, Default: DefaultTheAudioDBBaseURL},
	}
}

// ProviderSettings is one provider's environment-supplied configuration, in the
// fixed settings shape. It replaces the ten named Config fields the table's rows
// used to be spelled out as.
//
// Like every provider value on Config it is a FIRST-BOOT SEED and nothing more:
// once the DB-backed provider settings exist they are authoritative and these are
// ignored at runtime (see the Config field's comment).
type ProviderSettings struct {
	Key     string
	URL     string
	URL2    string
	Enabled bool
}

// defaultProviders is the Providers map Defaults() starts from: every URL field the
// table declares a default for, and nothing else.
func defaultProviders() map[string]ProviderSettings {
	out := map[string]ProviderSettings{}
	for _, b := range ProviderEnvTable() {
		if b.Default == "" {
			continue
		}
		p := out[b.Provider]
		setProviderField(&p, b.Field, b.Default)
		out[b.Provider] = p
	}
	return out
}

// setProviderField writes one field of a provider's settings. It is the table's
// only switch, and it is over the FIELD (four of them, fixed by the contract) —
// never over the provider.
func setProviderField(p *ProviderSettings, field ProviderField, value string) {
	switch field {
	case ProviderFieldKey:
		p.Key = value
	case ProviderFieldURL:
		p.URL = value
	case ProviderFieldURL2:
		p.URL2 = value
	}
}

// ProviderKey returns the API key the environment supplied for a provider.
func (c Config) ProviderKey(id string) string { return c.Providers[id].Key }

// ProviderURL returns the effective base URL for a provider (the operator's
// override, or the public default the table declares).
func (c Config) ProviderURL(id string) string { return c.Providers[id].URL }

// ProviderURL2 returns the effective second host for a provider, empty for the
// sources that have only one.
func (c Config) ProviderURL2(id string) string { return c.Providers[id].URL2 }

// ProviderEnabled returns a keyless provider's explicit environment opt-in.
func (c Config) ProviderEnabled(id string) bool { return c.Providers[id].Enabled }

// SetProviderKey sets a provider's environment key. It exists for the callers that
// build a Config by hand rather than from the environment — tests and the test
// harness — and it is COPY-ON-WRITE, because Config is passed by value everywhere
// and two copies sharing one map would let a harness option reach a Config somebody
// else is holding.
func (c *Config) SetProviderKey(id, key string) {
	p := c.provider(id)
	p.Key = key
	c.setProvider(id, p)
}

// SetProviderEnabled sets a keyless provider's explicit opt-in. Copy-on-write, for
// SetProviderKey's reason.
func (c *Config) SetProviderEnabled(id string, on bool) {
	p := c.provider(id)
	p.Enabled = on
	c.setProvider(id, p)
}

// SetProviderURL sets a provider's base-URL override (and, through URL2, its second
// host). Copy-on-write, for SetProviderKey's reason.
func (c *Config) SetProviderURL(id, url string) {
	p := c.provider(id)
	p.URL = url
	c.setProvider(id, p)
}

// SetProviderURL2 sets a provider's second host. Copy-on-write.
func (c *Config) SetProviderURL2(id, url string) {
	p := c.provider(id)
	p.URL2 = url
	c.setProvider(id, p)
}

func (c Config) provider(id string) ProviderSettings { return c.Providers[id] }

func (c *Config) setProvider(id string, p ProviderSettings) {
	next := make(map[string]ProviderSettings, len(c.Providers)+1)
	for k, v := range c.Providers {
		next[k] = v
	}
	next[id] = p
	c.Providers = next
}

// providerSeedRule is one row of the SEEDING table: a provider a fresh install may
// write a row for, and what turns that row on. SeedProviderRows walks it in order.
//
// Every rule here is the rule SeedIfEmpty used to spell out in a ladder of `if`s,
// moved beside the environment variables it is about and stated once. The clauses
// read oddly because the behaviour they preserve is odd — and preserving it exactly
// is the point of a prefactor (ADR-0059 decision 11).
type providerSeedRule struct {
	// Provider is the id the row is keyed by.
	Provider string
	// EnabledBy is the provider whose own environment opt-in turns this row on.
	// Empty means "this source has no opt-in — a key is what turns it on". It is a
	// field rather than always the row's own provider because Cover Art Archive has
	// no switch of its own: it rides MusicBrainz's, which is what it means to be that
	// Plugin's second host rather than a source.
	EnabledBy string
	// RidesVideoKey preserves the original single-switch behaviour: a configured
	// video source historically turned on every kind, so a deployment that set only
	// OBELO_TMDB_API_KEY still seeds the music rows. It is the video lead's key
	// specifically — a fanart.tv key alone never turned music on and still does not.
	RidesVideoKey bool
}

// providerSeedTable is which rows a FIRST BOOT writes, and what turns each on.
func providerSeedTable() []providerSeedRule {
	return []providerSeedRule{
		{Provider: ProviderTMDB},
		{Provider: ProviderMusicBrainz, EnabledBy: ProviderMusicBrainz, RidesVideoKey: true},
		{Provider: ProviderCoverArt, EnabledBy: ProviderMusicBrainz, RidesVideoKey: true},
		{Provider: ProviderFanartTV},
		{Provider: ProviderTheAudioDB},
	}
}

// ProviderSeedRow is one provider row a first boot writes: the id, whether it
// starts switched on, and the credentials and hosts to write with it. The
// composition root maps it into the enrichment domain's own SeedInput
// (enrich never imports config — ADR-0006).
type ProviderSeedRow struct {
	Provider     string
	Enabled      bool
	APIKey       string
	BaseURL      string
	ImageBaseURL string
}

// SeedProviderRows returns the provider rows a FIRST BOOT seeds from this config,
// in table order, and ONLY the rows it means to enable — a source the environment
// left unconfigured gets no row at all, exactly as before, so an Admin turning it on
// later starts from the Descriptor's defaults.
//
// resolved overrides a provider's key with the one the default-credential chain
// produced (ADR-0032): the operator's BYOK key, else a cached rotation key, else the
// build-injected bootstrap key. It is a map rather than two arguments because the
// chain is per-provider and the caller is the only thing that knows which providers
// it manages. A provider absent from it keeps its environment key.
func (c Config) SeedProviderRows(resolved map[string]string) []ProviderSeedRow {
	key := func(id string) string {
		if k, ok := resolved[id]; ok {
			return k
		}
		return c.ProviderKey(id)
	}
	videoKeyed := key(ProviderTMDB) != ""

	var out []ProviderSeedRow
	for _, rule := range providerSeedTable() {
		p := c.Providers[rule.Provider]
		on := key(rule.Provider) != ""
		if rule.EnabledBy != "" {
			on = on || c.ProviderEnabled(rule.EnabledBy)
		}
		if rule.RidesVideoKey {
			on = on || videoKeyed
		}
		if !on {
			continue
		}
		out = append(out, ProviderSeedRow{
			Provider:     rule.Provider,
			Enabled:      true,
			APIKey:       key(rule.Provider),
			BaseURL:      p.URL,
			ImageBaseURL: p.URL2,
		})
	}
	return out
}
