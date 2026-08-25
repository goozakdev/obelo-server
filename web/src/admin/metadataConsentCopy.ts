import type { MetadataCredentialSource } from "../api/types";

// The words of the first-run metadata-services decision (ADR-0032), in ONE place
// because they are asked in TWO: as a step in the first-run setup wizard
// (SetupScreen), and as the catch-up modal for a server whose consent is still
// unanswered (EnrichmentConsentGate). Two copies of a consent explanation drift,
// and the half that drifts is always the one nobody is looking at.
//
// The load-bearing part is credentialNote: what the operator is consenting to
// differs by BUILD. An official binary or Docker image carries default TMDB /
// fanart.tv credentials the operator never registered and does not control, so
// "enable metadata?" on that build is really "may this server call TMDB using OUR
// key?" — a question they can only answer if we say so. A build-from-source binary
// has no keys and the honest answer is the opposite one. So the note is derived
// from what the server reports (GET /settings/enrichment-consent →
// credentialSource), never assumed, and the source-unknown case says the one thing
// true of every build rather than guessing at a specific one.

/** The services this decision covers, named the way the operator will see them
 * on the Metadata Providers screen. */
export const METADATA_SERVICES = "TMDB and fanart.tv";

/** Where an Admin changes any of this afterwards — the same route in every string
 * here, so the prompt never sends anyone somewhere that doesn't exist. */
export const METADATA_SETTINGS_PATH = "Admin → Metadata Providers";

/** The shared explanation of what enabling actually does. Deliberately states the
 * OFF behavior too: the guarantee ("no external calls until you say yes") is the
 * reason this prompt exists, and it is worth more than the feature pitch. */
export const consentExplanation =
  `Obelo can fetch posters, descriptions, cast, and artwork for your library ` +
  `from ${METADATA_SERVICES}. That means contacting those services over the ` +
  `internet. Until you turn this on, Obelo makes no external metadata calls at all.`;

/** The build-specific sentence about WHOSE API key answers those calls, plus the
 * BYOK pointer. Every branch names the settings screen, because "you can use your
 * own key" is only useful to someone who is told where to put it. */
export function credentialNote(source?: MetadataCredentialSource): string {
  switch (source) {
    case "bootstrap":
    case "rotation":
      // The case this whole prompt was written for: pre-compiled binaries and
      // Docker images ship working credentials, so enrichment would otherwise
      // "just work" using a key the operator never chose and cannot rotate.
      return (
        `This build comes with ${METADATA_SERVICES} API keys supplied by the Obelo ` +
        `project, so this works without signing up for anything — but those keys are ` +
        `shared by every install running an official build. If you would rather use ` +
        `your own, add them under ${METADATA_SETTINGS_PATH}; your keys always take ` +
        `precedence over the bundled ones.`
      );
    case "operator":
      return (
        `This server is using the ${METADATA_SERVICES} API key you supplied, so these ` +
        `requests go out under your own account. You can change it under ` +
        `${METADATA_SETTINGS_PATH}.`
      );
    case "none":
      return (
        `This build comes with no metadata API keys, so nothing is fetched until you ` +
        `add your own ${METADATA_SERVICES} key under ${METADATA_SETTINGS_PATH}. You can ` +
        `still turn this on now — it takes effect as soon as a key is in place.`
      );
    default:
      // Source unknown (an older server, or wiring absent): say only what holds on
      // every build. Never invent a specific provenance for a privacy prompt.
      return (
        `You can add your own ${METADATA_SERVICES} API keys under ` +
        `${METADATA_SETTINGS_PATH} at any time.`
      );
  }
}

/** The closing reassurance — the decision is not a one-way door. */
export const consentReversibleNote = `You can change this later under ${METADATA_SETTINGS_PATH}.`;

/** The two answers, worded so neither reads as the "correct" one. */
export const CONSENT_ENABLE_LABEL = "Enable metadata";
export const CONSENT_DECLINE_LABEL = "Not now";
