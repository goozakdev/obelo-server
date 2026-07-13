import { useEffect, useState } from "react";
import { apiClient } from "../api/client";
import type { EnrichmentConsentState } from "../api/types";

// The Admin settings toggle for first-run Enrichment consent (ADR-0032) — the
// always-available counterpart to the first-run prompt. It reads the current
// decision and lets an Admin grant or REVOKE consent at any time; toggling saves
// immediately (like the per-Library policy panel) and the server re-gates the
// running provider with no restart. Revoking is the operator's off switch for all
// outbound metadata calls, independent of which providers are configured.
//
// Rendered as a card at the top of the Metadata Providers screen.

export default function EnrichmentConsentControl() {
  const [state, setState] = useState<EnrichmentConsentState | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    apiClient
      .getEnrichmentConsent(ctrl.signal)
      .then((c) => setState(c.state))
      .catch(() => setError("Couldn't load the enrichment consent setting."));
    return () => ctrl.abort();
  }, []);

  const onToggle = async (granted: boolean) => {
    setSaving(true);
    setError(null);
    try {
      const c = await apiClient.setEnrichmentConsent(granted);
      setState(c.state);
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
        Allow Juice Box to contact TMDB and fanart.tv for posters, descriptions,
        cast, and artwork
      </label>
      <p className="field-hint" data-testid="enrichment-consent-state" data-state={state ?? "loading"}>
        {state === null
          ? "Loading…"
          : granted
            ? "Enrichment is enabled. Uncheck to stop all outbound metadata calls."
            : "Enrichment is off — no external metadata calls are made. Check to enable."}
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
