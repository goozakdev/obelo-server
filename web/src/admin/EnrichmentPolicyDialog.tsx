import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { EnrichmentPolicy, Library } from "../api/types";
import { LibraryKindIcon, libraryKindLabel } from "../browse/kindIcons";

// The per-Library Enrichment-policy dialog (ADR-0027): a native modal <dialog>,
// mirroring EditLibraryDialog, where an Admin overrides how ONE Library enriches
// relative to the server-wide configuration. This slice carries the single
// "Enrich this library" control — a tri-state (Inherit / On / Off): Inherit tracks
// the global config live (the default; a Library never touched behaves exactly as
// today), and Off is the only way to stop a Library enriching. Each change saves
// immediately and re-enriches the Library server-side, so the effect is visible
// without a scan; the view then reflects the fresh stored + effective state. Later
// slices add the metadata-language, Authoritative-provider, and per-Supplement
// controls to this same panel.

// EnrichChoice is the tri-state the control exposes: inherit (no override), or a
// deliberate on/off. It maps to the wire `enrichEnabled` as null / true / false.
type EnrichChoice = "inherit" | "on" | "off";

function choiceOf(policy: EnrichmentPolicy): EnrichChoice {
  if (policy.enrichEnabled === null) return "inherit";
  return policy.enrichEnabled ? "on" : "off";
}

// enrichEnabledFor maps a chosen tri-state back to the PUT value: inherit clears
// the key (null); on/off set a deliberate override.
function enrichEnabledFor(choice: EnrichChoice): boolean | null {
  if (choice === "inherit") return null;
  return choice === "on";
}

export default function EnrichmentPolicyDialog({
  library,
  onClose,
}: {
  library: Library;
  /** Close the dialog (ESC, backdrop, ✕, or Close). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [policy, setPolicy] = useState<EnrichmentPolicy | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    apiClient
      .getEnrichmentPolicy(library.id, controller.signal)
      .then((p) => setPolicy(p))
      .catch((err) => {
        if (!controller.signal.aborted) setLoadError(errorMessage(err));
      });
    return () => controller.abort();
  }, [library.id]);

  async function select(choice: EnrichChoice) {
    if (saving || !policy || choice === choiceOf(policy)) return;
    setSaving(true);
    setSaveError(null);
    try {
      const updated = await apiClient.updateEnrichmentPolicy(library.id, {
        enrichEnabled: enrichEnabledFor(choice),
      });
      setPolicy(updated);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  const current = policy ? choiceOf(policy) : null;
  const overridden = policy ? policy.enrichEnabled !== null : false;

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="enrichment-policy-dialog"
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">
            <span className="admin-library-icon" aria-hidden="true">
              <LibraryKindIcon kind={library.kind} className="admin-library-kind-icon" />
            </span>
            Enrichment policy
            <span className="library-dialog-kind">{libraryKindLabel(library.kind)}</span>
          </h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid="enrichment-policy-close-x"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          <p className="field-hint">
            Override how <strong>{library.name}</strong> enriches, relative to the
            server-wide settings. Anything left on <em>Inherit</em> tracks the global
            configuration live.
          </p>

          {loadError && (
            <p className="status status-error" data-testid="enrichment-policy-load-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {loadError}
            </p>
          )}

          {policy && (
            <div className="field" data-testid="enrich-enabled-control">
              <div className="policy-control-label">
                <span className="field-label">Enrich this library</span>
                <span
                  className="policy-override-badge"
                  data-overridden={overridden ? "true" : "false"}
                >
                  {overridden ? "Overridden" : "Inherited"}
                </span>
              </div>
              <div className="tri-state" role="group" aria-label="Enrich this library">
                {(
                  [
                    {
                      value: "inherit" as const,
                      label: `Inherit (currently ${policy.inheritedEnrichEnabled ? "On" : "Off"})`,
                    },
                    { value: "on" as const, label: "On" },
                    { value: "off" as const, label: "Off" },
                  ]
                ).map((opt) => (
                  <button
                    key={opt.value}
                    type="button"
                    className="tri-state-option"
                    data-testid={`enrich-enabled-${opt.value}`}
                    data-active={current === opt.value ? "true" : "false"}
                    aria-pressed={current === opt.value}
                    disabled={saving}
                    onClick={() => select(opt.value)}
                  >
                    {opt.label}
                  </button>
                ))}
              </div>
              <p className="field-hint" data-testid="enrich-effective">
                {policy.effective.video || policy.effective.music
                  ? `This library will enrich (${[
                      policy.effective.video ? "video" : null,
                      policy.effective.music ? "music" : null,
                    ]
                      .filter(Boolean)
                      .join(" + ")}).`
                  : "This library will not enrich — no outbound calls are made."}
              </p>
              {saveError && (
                <p className="status status-error" data-testid="enrichment-policy-save-error" role="alert">
                  <span className="dot dot-error" aria-hidden="true" />
                  {saveError}
                </p>
              )}
            </div>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="auth-submit"
            type="button"
            data-testid="enrichment-policy-close"
            onClick={onClose}
          >
            Close
          </button>
        </footer>
      </div>
    </dialog>
  );
}
