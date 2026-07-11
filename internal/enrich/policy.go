package enrich

import "github.com/marioquake/juicebox/internal/store"

// Per-Library Enrichment policy resolution (ADR-0027) — the PRIMARY seam of the
// per-library-enrichment-policy feature. ResolveLibraryEnrichment is the pure
// derivation, a sibling to SettingsToProviderConfig, that layers a Library's
// SPARSE Enrichment policy over the server-wide provider config and returns the
// effective per-Library ProviderConfig + Enablement. Its output feeds the
// UNCHANGED BuildProvider (the Manager builds the effective provider from the
// returned config and pairs it with the returned enablement).
//
// The whole per-Library behavior lives here so it stays table-testable: if a
// change seems to need logic in the store, API, or UI beyond persistence/wiring,
// push it back into this function (PRD "Further Notes").
//
// It encodes every policy rule: inherit-live (an unset key tracks the global
// config); the metadata-language override; the Authoritative-provider pointer
// (always-active-if-keyed, kind-constrained, unreachable→fallback); the
// per-provider Supplement tri-state (force-on/force-off/inherit, with force-off of
// the current authoritative a no-op); and the enrich_enabled hard gate.

// GlobalEnrichment is the server-wide Enrichment configuration the per-Library
// resolver layers a policy over: the composed global ProviderConfig plus the
// per-slug ProviderState (enabled + keyed + key). The composed config alone can't
// answer "keyed but globally disabled" (it drops a disabled provider's key), so the
// ProviderState map fills that gap — the resolver reads it to honor the always-
// active-if-keyed authoritative and the force-on tri-state. The Manager builds it
// once per Reload (both parts derived from the same provider rows).
type GlobalEnrichment struct {
	Config    ProviderConfig
	Providers map[string]ProviderState
}

// Resolution is the effective per-Library Enrichment the resolver derives: the
// ProviderConfig BuildProvider consumes, the per-kind Enablement, and any
// attention signal. AuthoritativeFallback names a chosen Authoritative provider
// that is no longer usable (its key was cleared / it was disabled), so enrichment
// fell back to the kind's global default — the app surfaces it to the Admin
// (never silently dropped, ADR-0027); "" when the authoritative resolved normally.
type Resolution struct {
	Config                ProviderConfig
	Enablement            Enablement
	AuthoritativeFallback string
}

// ResolveLibraryEnrichment resolves a Library's effective Enrichment from the
// global config + its sparse policy. An empty policy resolves byte-for-byte to the
// global config (inherit everything live).
func ResolveLibraryEnrichment(g GlobalEnrichment, policy store.LibraryEnrichmentPolicy) Resolution {
	// Start from the global config and layer the sparse overrides. Only the deltas
	// the policy actually sets change; every unset key inherits the global config
	// LIVE (Model A) — an empty policy leaves cfg byte-for-byte equal to global.
	cfg := g.Config

	// Metadata-language override (issue 02): a Library may localize its Enrichment to
	// a language distinct from the server-wide default; unset inherits the global
	// language live. Applied regardless of enrich_enabled (harmless when off).
	if policy.MetadataLanguage != nil {
		cfg.MetadataLanguage = *policy.MetadataLanguage
	}

	// Authoritative-provider pointer (issue 03): repoint the Full provider that leads.
	fallback := resolveAuthoritative(&cfg, g.Providers, policy.AuthoritativeProvider)

	// Per-provider Supplement tri-state (issue 05): force a supplement on or off for
	// this Library only. Applied AFTER the authoritative is chosen so force-off of the
	// current authoritative can be recognized as a no-op.
	applySupplementOverrides(&cfg, g.Providers, policy.SupplementOverrides)

	// enrich_enabled=false is the ONLY hard off-switch for a Library (ADR-0027): no
	// chain runs and no outbound call is made. The service gates every fetch on
	// Enablement, so an all-off Enablement guarantees zero traffic regardless of what
	// keys the effective config still holds. The config is carried through unchanged.
	if policy.EnrichEnabled != nil && !*policy.EnrichEnabled {
		return Resolution{Config: cfg, Enablement: Enablement{}, AuthoritativeFallback: fallback}
	}
	// Empty policy (or enrich_enabled explicitly true): inherit the global enablement
	// LIVE. enrich_enabled=true is the absence of the off-switch, NOT a command to
	// enrich — a globally-unconfigured kind stays off (the authoritative pointer is
	// what makes a specific Full provider lead).
	return Resolution{Config: cfg, Enablement: DeriveEnablement(cfg), AuthoritativeFallback: fallback}
}

// resolveAuthoritative applies the Authoritative-provider pointer to cfg and
// returns a fallback slug when the chosen provider is unreachable. Rules (ADR-0027):
//   - unset ⇒ inherit the kind's global default (no change to cfg);
//   - a non-Full slug or an unknown/wrong-kind slug ⇒ ignored (defensive; the API
//     constrains the pointer's domain, so this only guards a stale/invalid value);
//   - a keyed Full provider ⇒ it LEADS even when globally disabled (always-active-
//     if-keyed): point cfg at it and inject its key so BuildProvider composes it;
//   - a Full provider that is NOT keyed (its key was cleared after selection) ⇒
//     fall back to the kind's global default authoritative and flag it, never stall.
func resolveAuthoritative(cfg *ProviderConfig, providers map[string]ProviderState, pointer *string) string {
	if pointer == nil {
		return "" // inherit the kind default
	}
	slug := *pointer
	entry, ok := RegistryEntryFor(slug)
	if !ok || entry.Class != ClassFull {
		return "" // not a leadable provider — ignore, inherit the default
	}
	// Only the video kind has multiple Full providers today, so only a video
	// authoritative changes the composition; a music pointer (MusicBrainz, the sole
	// Full music provider) already equals the default and needs no cfg change.
	if !entry.serves(KindVideo) {
		return ""
	}
	st := providers[slug]
	if !st.Keyed {
		// Unreachable after selection: don't point cfg at an unusable lead. Leaving
		// AuthoritativeVideo unset falls back to the kind default (TMDB); flag it so
		// the app can surface the degradation on the attention surface.
		return slug
	}
	cfg.AuthoritativeVideo = slug
	// Inject the key so BuildProvider composes it as the lead even if the provider is
	// globally DISABLED (always-active-if-keyed). The base URL is already in cfg
	// (SettingsToProviderConfig sets every provider's base URL regardless of enabled).
	setProviderKey(cfg, slug, st.APIKey)
	return ""
}

// applySupplementOverrides applies the per-provider Supplement tri-state to cfg by
// injecting or clearing each overridden provider's key (BuildProvider then composes
// exactly the keyed sources). A slug absent from the map inherits its global enabled
// state (already reflected in cfg), so this only touches deltas. Force-off of the
// CURRENT authoritative is a no-op (ADR-0027): its off-switch is enrich_enabled, not
// a per-provider toggle. A force-on activates a source only when it is keyed
// (offline-first — never conjure a call for an unkeyed provider).
func applySupplementOverrides(cfg *ProviderConfig, providers map[string]ProviderState, overrides map[string]bool) {
	for slug, on := range overrides {
		if isCurrentAuthoritative(*cfg, slug) {
			continue // force-off/on of the leader is meaningless — no-op
		}
		if on {
			if st := providers[slug]; st.Keyed {
				setProviderKey(cfg, slug, st.APIKey)
			}
		} else {
			setProviderKey(cfg, slug, "") // muted for this Library — zero calls
		}
	}
}

// isCurrentAuthoritative reports whether slug is the Library's leading provider for
// its kind — the video authoritative (repointable) or the fixed music authoritative
// (MusicBrainz). Used to make force-off of the leader a no-op.
func isCurrentAuthoritative(cfg ProviderConfig, slug string) bool {
	return slug == cfg.videoAuthoritativeSlug() || slug == DefaultAuthoritativeForKind(KindMusic)
}

// setProviderKey sets (or clears, with an empty key) the API-key field for a
// key-bearing provider in cfg — the one place the resolver injects/removes a key to
// activate/mute a source. A keyless provider (MusicBrainz, Cover Art Archive) has no
// key to set here; its activation rides its authoritative's enablement.
func setProviderKey(cfg *ProviderConfig, slug, key string) {
	switch slug {
	case SlugTMDB:
		cfg.TMDBAPIKey = key
	case SlugOMDb:
		cfg.OMDbAPIKey = key
	case SlugTheTVDB:
		cfg.TheTVDBAPIKey = key
	case SlugAniDB:
		cfg.AniDBAPIKey = key
	case SlugFanartTV:
		cfg.FanartTVAPIKey = key
	case SlugTheAudioDB:
		cfg.TheAudioDBAPIKey = key
	}
}
