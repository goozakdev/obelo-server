import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type {
  MetadataProvider,
  MetadataProvidersView,
  ProviderUpdate,
  TestProviderResult,
  UpdateMetadataProvidersInput,
} from "../api/types";
import MaskedKeyInput from "./MaskedKeyInput";

// The MusicBrainz throttle is a server-wide knob (not a per-provider row), but it
// is MusicBrainz-specific, so it lives in MusicBrainz's Advanced section rather
// than the general Enrichment-behavior card.
const MUSICBRAINZ_SLUG = "musicbrainz";

// The per-provider configuration dialog (metadata-providers redesign). Opened from
// the Edit icon on a provider row, it is the one place an Admin edits a single
// source's settings — its API key (masked; the stored value is never shown), its
// base-URL / image-host overrides (Advanced), and a live Test connection — and
// SAVES just that provider. Enablement is NOT here: it's the row's own checkbox,
// so this dialog carries only the configuration knobs.
//
// Save persists a PARTIAL update for this one slug (only the fields that changed):
// omit=unchanged / ""=clear / value=set for the key, and set-or-reset for the URL
// overrides. A save with nothing changed just closes. On success the fresh masked
// view flows back to the parent (onSaved) and the dialog closes; a refused save
// (validation) shows inline and the dialog stays open. Test uses the currently-
// typed (unsaved) key/base URL so the Admin can verify before committing.

type TestState =
  | { status: "idle" }
  | { status: "pending" }
  | { status: "done"; result: TestProviderResult };

export default function ProviderConfigDialog({
  provider,
  musicBrainzRateLimitMs,
  onSaved,
  onClose,
}: {
  provider: MetadataProvider;
  /** The current server-wide MusicBrainz throttle (ms). Only the MusicBrainz
   * dialog edits it; ignored for every other provider. */
  musicBrainzRateLimitMs: number;
  /** Called with the fresh masked view after a successful save. */
  onSaved: (view: MetadataProvidersView) => void;
  /** Close without saving (ESC, backdrop, ✕, or Cancel). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const p = provider;
  const isMusicBrainz = p.slug === MUSICBRAINZ_SLUG;
  // Local edits for this provider only. `key` is what the Admin is typing now
  // ("" = not typing → unchanged); `clearKey` marks the stored key for clearing.
  const [key, setKey] = useState("");
  const [clearKey, setClearKey] = useState(false);
  const [baseURL, setBaseURL] = useState(p.baseURL);
  const [imageBaseURL, setImageBaseURL] = useState(p.imageBaseURL ?? "");
  // The MusicBrainz throttle, held as a string so the field can be cleared while
  // typing (only meaningful in the MusicBrainz dialog).
  const [rateLimitMs, setRateLimitMs] = useState(
    isMusicBrainz ? String(musicBrainzRateLimitMs) : "",
  );
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [test, setTest] = useState<TestState>({ status: "idle" });

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  // Build the partial payload from what actually changed — the per-provider row
  // fields plus, for MusicBrainz, the server-wide throttle — or null when nothing
  // did (a Save with no edits just closes).
  function buildPayload(): UpdateMetadataProvidersInput | null {
    const payload: UpdateMetadataProvidersInput = {};
    const upd: ProviderUpdate = { slug: p.slug };
    let provChanged = false;
    if (key !== "") {
      upd.apiKey = key; // set
      provChanged = true;
    } else if (clearKey) {
      upd.apiKey = ""; // clear
      provChanged = true;
    }
    if (baseURL !== p.baseURL) {
      upd.baseURL = baseURL; // set or reset (empty)
      provChanged = true;
    }
    if (p.imageBaseURL !== undefined && imageBaseURL !== p.imageBaseURL) {
      upd.imageBaseURL = imageBaseURL; // set or reset (empty)
      provChanged = true;
    }
    if (provChanged) payload.providers = [upd];
    if (isMusicBrainz) {
      const ms = Number(rateLimitMs);
      if (rateLimitMs.trim() !== "" && Number.isFinite(ms)) {
        const rounded = Math.round(ms);
        if (rounded !== musicBrainzRateLimitMs) payload.musicBrainzRateLimitMs = rounded;
      }
    }
    return payload.providers || payload.musicBrainzRateLimitMs !== undefined
      ? payload
      : null;
  }

  async function onSave() {
    if (saving) return;
    const payload = buildPayload();
    if (!payload) {
      onClose();
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const next = await apiClient.updateMetadataProviders(payload);
      onSaved(next);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setSaving(false);
    }
  }

  async function onTest() {
    setTest({ status: "pending" });
    const creds: { apiKey?: string; baseURL?: string } = {};
    if (key !== "") creds.apiKey = key;
    if (baseURL !== p.baseURL) creds.baseURL = baseURL;
    try {
      const result = await apiClient.testMetadataProvider(p.slug, creds);
      setTest({ status: "done", result });
    } catch (err) {
      setTest({ status: "done", result: { ok: false, detail: errorMessage(err) } });
    }
  }

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog provider-config-dialog"
      data-testid={`provider-config-dialog-${p.slug}`}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current && !saving) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">
            {p.name}
            <span
              className="provider-role-badge"
              data-role={p.role}
              data-testid={`provider-config-role-${p.slug}`}
            >
              {p.role === "authoritative" ? "Authoritative" : "Supplement"}
            </span>
          </h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid={`provider-config-close-${p.slug}`}
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          <p className="provider-description">
            {p.description}{" "}
            {p.docsURL && (
              <a
                href={p.docsURL}
                target="_blank"
                rel="noreferrer"
                data-testid={`provider-docs-${p.slug}`}
              >
                Get a key
              </a>
            )}
          </p>

          {p.requiresKey && (
            <div className="field provider-key-field">
              <span className="field-label">API key</span>
              <MaskedKeyInput
                slug={p.slug}
                hasKey={p.hasKey}
                value={key}
                cleared={clearKey}
                disabled={saving}
                onChange={(value) => {
                  setKey(value);
                  setClearKey(false);
                }}
                onClear={() => {
                  setClearKey(true);
                  setKey("");
                }}
              />
            </div>
          )}

          <div className="provider-config-advanced">
            <h3 className="provider-config-subhead">Advanced</h3>
            <div className="field">
              <label className="field-label" htmlFor={`provider-baseurl-${p.slug}`}>
                Base URL override
              </label>
              <input
                id={`provider-baseurl-${p.slug}`}
                className="field-input"
                data-testid={`provider-baseurl-${p.slug}`}
                type="text"
                value={baseURL}
                placeholder={p.baseURL}
                onChange={(e) => setBaseURL(e.target.value)}
                disabled={saving}
              />
            </div>
            {p.imageBaseURL !== undefined && (
              <div className="field">
                <label
                  className="field-label"
                  htmlFor={`provider-imagebaseurl-${p.slug}`}
                >
                  Image host override
                </label>
                <input
                  id={`provider-imagebaseurl-${p.slug}`}
                  className="field-input"
                  data-testid={`provider-imagebaseurl-${p.slug}`}
                  type="text"
                  value={imageBaseURL}
                  onChange={(e) => setImageBaseURL(e.target.value)}
                  disabled={saving}
                />
              </div>
            )}
            {isMusicBrainz && (
              <div className="field">
                <label className="field-label" htmlFor="musicbrainz-rate-limit-input">
                  Rate limit (ms)
                </label>
                <input
                  id="musicbrainz-rate-limit-input"
                  className="field-input"
                  data-testid="musicbrainz-rate-limit-input"
                  type="number"
                  min={0}
                  value={rateLimitMs}
                  onChange={(e) => setRateLimitMs(e.target.value)}
                  disabled={saving}
                />
                <span className="field-hint">
                  Minimum spacing between MusicBrainz requests. 0 = no throttle.
                </span>
              </div>
            )}
          </div>

          <div className="provider-test">
            <button
              className="nav-link"
              type="button"
              data-testid={`provider-test-${p.slug}`}
              onClick={() => void onTest()}
              disabled={test.status === "pending"}
            >
              {test.status === "pending" ? "Testing…" : "Test connection"}
            </button>
            {test.status === "done" && (
              <span
                className={`status ${test.result.ok ? "status-ok" : "status-error"}`}
                data-testid={`provider-test-status-${p.slug}`}
                data-ok={test.result.ok ? "true" : "false"}
                role={test.result.ok ? "status" : "alert"}
              >
                <span
                  className={`dot ${test.result.ok ? "dot-ok" : "dot-error"}`}
                  aria-hidden="true"
                />
                {test.result.detail}
              </span>
            )}
          </div>

          {error && (
            <p
              className="status status-error"
              data-testid={`provider-config-error-${p.slug}`}
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {error}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="nav-link"
            type="button"
            data-testid={`provider-config-cancel-${p.slug}`}
            onClick={onClose}
            disabled={saving}
          >
            Cancel
          </button>
          <button
            className="auth-submit"
            type="button"
            data-testid={`provider-config-save-${p.slug}`}
            onClick={() => void onSave()}
            disabled={saving}
          >
            {saving ? "Saving…" : "Save"}
          </button>
        </footer>
      </div>
    </dialog>
  );
}
