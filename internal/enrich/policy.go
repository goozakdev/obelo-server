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
// This slice implements the two spine rules:
//   - empty policy ⇒ byte-for-byte today's global result (inherit live);
//   - enrich_enabled=false ⇒ no chain, zero outbound calls.
//
// Later slices extend it with the metadata-language override, the Authoritative-
// provider pointer (always-active-if-keyed, kind-constrained, unreachable
// fallback), and the per-provider Supplement tri-state.
func ResolveLibraryEnrichment(global ProviderConfig, policy store.LibraryEnrichmentPolicy) (ProviderConfig, Enablement) {
	// Start from the global config and layer the sparse overrides. Only the deltas
	// the policy actually sets change; every unset key inherits the global config
	// LIVE (Model A) — an empty policy leaves cfg byte-for-byte equal to global.
	cfg := global

	// Metadata-language override (issue 02): a Library may localize its Enrichment
	// to a language distinct from the server-wide default; unset inherits the global
	// language live. The language threads into every provider constructor at build
	// time, so overriding it here re-localizes the whole effective chain. It is
	// applied regardless of enrich_enabled (harmless when off — no calls are made),
	// so the disabled branch below still returns the same cfg.
	if policy.MetadataLanguage != nil {
		cfg.MetadataLanguage = *policy.MetadataLanguage
	}

	// enrich_enabled=false is the ONLY hard off-switch for a Library (ADR-0027):
	// no chain runs and no outbound call is made. The service gates every fetch on
	// Enablement, so an all-off Enablement guarantees zero traffic for the Library
	// regardless of what keys the effective config still holds. The config itself is
	// carried through unchanged (nothing reads it while both kinds are off), so
	// "inherit vs. deliberately-off" stays a single-key decision, not a config edit.
	if policy.EnrichEnabled != nil && !*policy.EnrichEnabled {
		return cfg, Enablement{}
	}
	// Empty policy (or enrich_enabled explicitly true): inherit the global config
	// LIVE. enrich_enabled=true is the absence of the off-switch, NOT a command to
	// enrich — it still inherits which kinds the global config actually enables, so
	// a globally-unconfigured kind stays off (the Authoritative-provider pointer,
	// added in a later slice, is what makes a specific Full provider lead).
	return cfg, DeriveEnablement(cfg)
}
