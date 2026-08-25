import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import type { EnrichmentConsentState, MetadataCredentialSource } from "../api/types";

// The Admin settings toggle for first-run Enrichment consent (ADR-0032) — the
// always-available counterpart to the first-run prompt. It reads the current
// decision and lets an Admin grant or REVOKE consent at any time; toggling saves
// immediately (like the per-Library policy panel) and the server re-gates the
// running provider with no restart. Revoking is the operator's off switch for all
// outbound metadata calls, independent of which providers are configured.
//
// Rendered as a card at the top of the Metadata Providers screen.

export default function EnrichmentConsentControl({
  // onDecision fires after a saved decision. The enclosing screen re-reads the
  // provider settings with it, because consent gates the per-kind enablement that
  // screen renders (ADR-0032) — without it, toggling this switch would leave
  // "Enrichment on" sitting a few centimetres below "no external metadata calls
  // are made" until a reload, which is the contradiction this control exists to
  // prevent.
  onDecision,
}: {
  onDecision?: (state: EnrichmentConsentState) => void;
} = {}) {
  const [state, setState] = useState<EnrichmentConsentState | null>(null);
  // Whose API key these calls would use (ADR-0032 precedence). Shown because this
  // is the screen where the answer can be CHANGED — an operator running an
  // official build is using credentials bundled by the project until they type
  // their own into the provider rows below, and that should not be something they
  // have to infer.
  const [credentialSource, setCredentialSource] = useState<
    MetadataCredentialSource | undefined
  >(undefined);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    apiClient
      .getEnrichmentConsent(ctrl.signal)
      .then((c) => {
        setState(c.state);
        setCredentialSource(c.credentialSource);
      })
      .catch(() => setError("Couldn't load the enrichment consent setting."));
    return () => ctrl.abort();
  }, []);

  const onToggle = async (granted: boolean) => {
    setSaving(true);
    setError(null);
    try {
      const c = await apiClient.setEnrichmentConsent(granted);
      setState(c.state);
      onDecision?.(c.state);
    } catch {
      setError("Couldn't save the change. Please try again.");
    } finally {
      setSaving(false);
    }
  };

  // Until loaded, render a stable placeholder card so the screen doesn't jump.
  const granted = state === "granted";

  return (
    <div className="provider-consent card" data-testid="enrichment-consent-control">
      <h2 className="section-title">External metadata enrichment</h2>
      <label className="provider-toggle">
        <input
          type="checkbox"
          data-testid="enrichment-consent-toggle"
          checked={granted}
          onChange={(e) => void onToggle(e.target.checked)}
          disabled={saving || state === null}
        />{" "}
        Allow Obelo to contact TMDB and fanart.tv for posters, descriptions,
        cast, and artwork
      </label>
      <p className="field-hint" data-testid="enrichment-consent-state" data-state={state ?? "loading"}>
        {state === null
          ? "Loading…"
          : granted
            ? "Enrichment is enabled. Uncheck to stop all outbound metadata calls."
            : "Enrichment is off — no external metadata calls are made. Check to enable."}
      </p>
      <p
        className="field-hint"
        data-testid="enrichment-consent-credentials"
        data-source={credentialSource ?? "unknown"}
      >
        {credentialSourceHint(credentialSource)}
      </p>
      {error && (
        <p className="status status-error" data-testid="enrichment-consent-control-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </div>
  );
}

// credentialSourceHint says which key answers these calls, in the one place the
// operator can act on it — the provider rows below this card. Kept short here (the
// first-run prompt in metadataConsentCopy carries the full explanation) but it is
// the same fact, and an unknown source says nothing specific rather than guessing.
function credentialSourceHint(source?: MetadataCredentialSource): string {
  switch (source) {
    case "bootstrap":
    case "rotation":
      return "Currently using the default TMDB / fanart.tv keys bundled with this build, which are shared by every official install. Enter your own below to use your own accounts — your keys always win.";
    case "operator":
      return "Currently using the API key you supplied below, so requests go out under your own account.";
    case "none":
      return "No metadata API keys are configured — this build bundles none. Add your own below before enrichment can fetch anything.";
    default:
      return "You can supply your own TMDB / fanart.tv API keys in the provider rows below.";
  }
}
