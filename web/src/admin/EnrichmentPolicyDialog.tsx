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

// choiceOfOverride maps a stored tri-state override (null / true / false) to the
// control's active segment — shared by the enrich toggle and each Supplement.
function choiceOfOverride(override: boolean | null): EnrichChoice {
  if (override === null) return "inherit";
  return override ? "on" : "off";
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
  // Draft for the free-text language control; synced from the loaded/updated policy.
  const [languageDraft, setLanguageDraft] = useState("");

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    apiClient
      .getEnrichmentPolicy(library.id, controller.signal)
      .then((p) => {
        setPolicy(p);
        setLanguageDraft(p.metadataLanguage ?? "");
      })
      .catch((err) => {
        if (!controller.signal.aborted) setLoadError(errorMessage(err));
      });
    return () => controller.abort();
  }, [library.id]);

  // save applies one partial update, refreshes the view, and re-syncs the language
  // draft to whatever the server stored (so a normalized/cleared value shows).
  async function save(input: Parameters<typeof apiClient.updateEnrichmentPolicy>[1]) {
    if (saving || !policy) return;
    setSaving(true);
    setSaveError(null);
    try {
      const updated = await apiClient.updateEnrichmentPolicy(library.id, input);
      setPolicy(updated);
      setLanguageDraft(updated.metadataLanguage ?? "");
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function select(choice: EnrichChoice) {
    if (!policy || choice === choiceOf(policy)) return;
    await save({ enrichEnabled: enrichEnabledFor(choice) });
  }

  // commitLanguage saves the draft as an override (a blank draft clears to inherit).
  // A no-op when the draft already equals the stored value, so a blur without a
  // change makes no request.
  async function commitLanguage() {
    if (!policy) return;
    const trimmed = languageDraft.trim();
    const next = trimmed === "" ? null : trimmed;
    if (next === policy.metadataLanguage) return;
    await save({ metadataLanguage: next });
  }

  // selectAuthoritative saves the chosen authoritative ("inherit" clears the key).
  async function selectAuthoritative(value: string) {
    if (!policy) return;
    const next = value === "inherit" ? null : value;
    if (next === policy.authoritativeProvider) return;
    await save({ authoritativeProvider: next });
  }

  // selectSupplement saves one Supplement's tri-state ("inherit" clears the key).
  async function selectSupplement(slug: string, choice: EnrichChoice) {
    if (!policy) return;
    const next = choice === "inherit" ? null : choice === "on";
    await save({ providerOverrides: { [slug]: next } });
  }

  const current = policy ? choiceOf(policy) : null;
  const overridden = policy ? policy.enrichEnabled !== null : false;
  const languageOverridden = policy ? policy.metadataLanguage !== null : false;
  const authoritativeOverridden = policy ? policy.authoritativeProvider !== null : false;

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
            <>
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
              </div>

              <div className="field" data-testid="metadata-language-control">
                <div className="policy-control-label">
                  <span className="field-label">Metadata language</span>
                  <span
                    className="policy-override-badge"
                    data-overridden={languageOverridden ? "true" : "false"}
                  >
                    {languageOverridden ? "Overridden" : "Inherited"}
                  </span>
                </div>
                <div className="policy-language-row">
                  <input
                    type="text"
                    className="field-input"
                    data-testid="metadata-language-input"
                    aria-label="Metadata language"
                    value={languageDraft}
                    disabled={saving}
                    placeholder={
                      policy.inheritedMetadataLanguage
                        ? `Inherit (${policy.inheritedMetadataLanguage})`
                        : "Inherit (server default)"
                    }
                    onChange={(e) => setLanguageDraft(e.target.value)}
                    onBlur={() => void commitLanguage()}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        void commitLanguage();
                      }
                    }}
                  />
                  {languageOverridden && (
                    <button
                      type="button"
                      className="nav-link"
                      data-testid="metadata-language-reset"
                      disabled={saving}
                      onClick={() => void save({ metadataLanguage: null })}
                    >
                      Reset to inherit
                    </button>
                  )}
                </div>
                <p className="field-hint">
                  A language/region code (e.g. <code>en-US</code>, <code>ja-JP</code>).
                  Leave blank to inherit the server-wide language.
                </p>
              </div>

              <div className="field" data-testid="authoritative-control">
                <div className="policy-control-label">
                  <span className="field-label">Authoritative provider</span>
                  <span
                    className="policy-override-badge"
                    data-overridden={authoritativeOverridden ? "true" : "false"}
                  >
                    {authoritativeOverridden ? "Overridden" : "Inherited"}
                  </span>
                </div>
                <div className="policy-language-row">
                  <select
                    className="field-input"
                    data-testid="authoritative-select"
                    aria-label="Authoritative provider"
                    value={policy.authoritativeProvider ?? "inherit"}
                    disabled={saving}
                    onChange={(e) => void selectAuthoritative(e.target.value)}
                  >
                    <option value="inherit">
                      Inherit (default: {policy.inheritedAuthoritative.name || "—"})
                    </option>
                    {policy.authoritativeCandidates.map((c) => (
                      <option key={c.slug} value={c.slug}>
                        {c.name}
                      </option>
                    ))}
                  </select>
                  {authoritativeOverridden && (
                    <button
                      type="button"
                      className="nav-link"
                      data-testid="authoritative-reset"
                      disabled={saving}
                      onClick={() => void save({ authoritativeProvider: null })}
                    >
                      Reset to inherit
                    </button>
                  )}
                </div>
                {policy.authoritativeUnreachable ? (
                  <p className="status status-error" data-testid="authoritative-unreachable" role="alert">
                    <span className="dot dot-error" aria-hidden="true" />
                    The chosen provider ({policy.authoritativeUnreachable}) is no longer
                    usable — enrichment fell back to{" "}
                    <strong>{policy.effectiveAuthoritative.name}</strong>. Re-key or
                    re-enable it, or pick another.
                  </p>
                ) : (
                  <p className="field-hint" data-testid="authoritative-effective">
                    Leads with <strong>{policy.effectiveAuthoritative.name || "—"}</strong>;
                    the remaining enabled providers fill the gaps.
                  </p>
                )}
              </div>

              {policy.supplements.length > 0 && (
                <div className="field" data-testid="supplements-control">
                  <span className="field-label">Supplements</span>
                  <p className="field-hint">
                    Force an individual supplement on or off for this library, or leave
                    it on <em>Inherit</em> to track the server-wide setting.
                  </p>
                  {policy.supplements.map((s) => {
                    const supChoice = choiceOfOverride(s.override);
                    return (
                      <div
                        key={s.slug}
                        className="policy-supplement-row"
                        data-testid={`supplement-${s.slug}`}
                      >
                        <span className="policy-supplement-name">{s.name}</span>
                        <div
                          className="tri-state"
                          role="group"
                          aria-label={`${s.name} for this library`}
                        >
                          {(
                            [
                              {
                                value: "inherit" as const,
                                label: `Inherit (${s.inheritedEnabled ? "On" : "Off"})`,
                              },
                              { value: "on" as const, label: "On" },
                              { value: "off" as const, label: "Off" },
                            ]
                          ).map((opt) => (
                            <button
                              key={opt.value}
                              type="button"
                              className="tri-state-option"
                              data-testid={`supplement-${s.slug}-${opt.value}`}
                              data-active={supChoice === opt.value ? "true" : "false"}
                              aria-pressed={supChoice === opt.value}
                              disabled={saving}
                              onClick={() => void selectSupplement(s.slug, opt.value)}
                            >
                              {opt.label}
                            </button>
                          ))}
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}

              {saveError && (
                <p className="status status-error" data-testid="enrichment-policy-save-error" role="alert">
                  <span className="dot dot-error" aria-hidden="true" />
                  {saveError}
                </p>
              )}
            </>
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
