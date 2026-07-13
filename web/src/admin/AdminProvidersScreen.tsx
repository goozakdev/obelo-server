import { useCallback, useEffect, useState } from "react";
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
import EnrichmentConsentControl from "./EnrichmentConsentControl";

// The Metadata Providers admin screen (metadata-providers 02). Behind
// RequireAdmin (App.tsx) and still server-enforced (the /settings API is Admin
// scope regardless of the client gate). An Admin sees every external metadata
// source the server knows about, grouped by the media kind it serves, and can:
//   - enable/disable each source,
//   - enter, rotate, or clear its API key (masked; the stored value is never
//     shown — only a "configured"/"not set" indicator),
//   - override its base URL (advanced), and
//   - test its connectivity/credentials.
// Plus a server-wide metadata language. Saving takes effect at runtime with no
// restart (the server rebuilds + hot-swaps the active provider).
//
// One global Save persists the whole form as a PARTIAL update: only providers the
// Admin actually changed are sent (so an untouched key-requiring source is never
// dragged into a PROVIDER_KEY_REQUIRED rejection). A refused save (validation) is
// NOT swallowed — its readable message shows inline and the form stays put.

/** Per-provider local edits, keyed by slug. `key` is what the Admin is typing now
 * ("" = not typing → unchanged); `clearKey` marks the stored key for clearing;
 * `baseURL` is the (possibly edited) effective host. */
interface Draft {
  enabled: Record<string, boolean>;
  key: Record<string, string>;
  clearKey: Record<string, boolean>;
  baseURL: Record<string, string>;
  imageBaseURL: Record<string, string>;
  language: string;
  // Enrichment behavior knobs (enrichment-runtime-settings). The interval is edited
  // in MINUTES (converted to/from the API's enrichIntervalSeconds); the rate limit
  // stays in milliseconds. Both number inputs are held as strings so the controlled
  // field can be cleared while typing.
  autoEnrichAfterScan: boolean;
  enrichIntervalMinutes: string;
  musicBrainzRateLimitMs: string;
}

function draftFromView(view: MetadataProvidersView): Draft {
  const enabled: Record<string, boolean> = {};
  const key: Record<string, string> = {};
  const clearKey: Record<string, boolean> = {};
  const baseURL: Record<string, string> = {};
  const imageBaseURL: Record<string, string> = {};
  for (const p of view.providers) {
    enabled[p.slug] = p.enabled;
    key[p.slug] = "";
    clearKey[p.slug] = false;
    baseURL[p.slug] = p.baseURL;
    imageBaseURL[p.slug] = p.imageBaseURL ?? "";
  }
  return {
    enabled,
    key,
    clearKey,
    baseURL,
    imageBaseURL,
    language: view.metadataLanguage,
    autoEnrichAfterScan: view.autoEnrichAfterScan,
    enrichIntervalMinutes: String(view.enrichIntervalSeconds / 60),
    musicBrainzRateLimitMs: String(view.musicBrainzRateLimitMs),
  };
}

type TestState =
  | { status: "idle" }
  | { status: "pending" }
  | { status: "done"; result: TestProviderResult };

// The coarse kind groups, in display order (video authoritative first).
const KIND_GROUPS: { kind: string; label: string }[] = [
  { kind: "video", label: "Video" },
  { kind: "music", label: "Music" },
];

export default function AdminProvidersScreen() {
  const [view, setView] = useState<MetadataProvidersView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [tests, setTests] = useState<Record<string, TestState>>({});

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoadError(null);
    try {
      const v = await apiClient.getMetadataProviders(signal);
      if (signal?.aborted) return;
      setView(v);
      setDraft(draftFromView(v));
    } catch (err) {
      if (signal?.aborted) return;
      setLoadError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  function patch(update: (d: Draft) => Draft) {
    setDraft((prev) => (prev ? update(prev) : prev));
    setSaved(false);
  }

  // Build the partial PUT payload from what actually changed vs. the loaded view.
  function buildPayload(v: MetadataProvidersView, d: Draft): UpdateMetadataProvidersInput {
    const providers: ProviderUpdate[] = [];
    for (const p of v.providers) {
      const upd: ProviderUpdate = { slug: p.slug };
      let changed = false;
      if (d.enabled[p.slug] !== p.enabled) {
        upd.enabled = d.enabled[p.slug];
        changed = true;
      }
      const typed = d.key[p.slug] ?? "";
      if (typed !== "") {
        upd.apiKey = typed; // set
        changed = true;
      } else if (d.clearKey[p.slug]) {
        upd.apiKey = ""; // clear
        changed = true;
      }
      const base = d.baseURL[p.slug] ?? p.baseURL;
      if (base !== p.baseURL) {
        upd.baseURL = base; // set or reset (empty)
        changed = true;
      }
      // Image host: only for providers that have one (p.imageBaseURL present).
      if (p.imageBaseURL !== undefined) {
        const img = d.imageBaseURL[p.slug] ?? p.imageBaseURL;
        if (img !== p.imageBaseURL) {
          upd.imageBaseURL = img; // set or reset (empty)
          changed = true;
        }
      }
      if (changed) providers.push(upd);
    }
    const payload: UpdateMetadataProvidersInput = {};
    if (providers.length > 0) payload.providers = providers;
    if (d.language !== v.metadataLanguage) payload.metadataLanguage = d.language;
    // Behavior knobs: send only what changed. The interval is entered in minutes →
    // converted to whole seconds; a blank/invalid number is treated as unchanged.
    if (d.autoEnrichAfterScan !== v.autoEnrichAfterScan) {
      payload.autoEnrichAfterScan = d.autoEnrichAfterScan;
    }
    const minutes = Number(d.enrichIntervalMinutes);
    if (d.enrichIntervalMinutes.trim() !== "" && Number.isFinite(minutes)) {
      const seconds = Math.round(minutes * 60);
      if (seconds !== v.enrichIntervalSeconds) payload.enrichIntervalSeconds = seconds;
    }
    const ms = Number(d.musicBrainzRateLimitMs);
    if (d.musicBrainzRateLimitMs.trim() !== "" && Number.isFinite(ms)) {
      const rounded = Math.round(ms);
      if (rounded !== v.musicBrainzRateLimitMs) payload.musicBrainzRateLimitMs = rounded;
    }
    return payload;
  }

  async function onSave() {
    if (!view || !draft || saving) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const next = await apiClient.updateMetadataProviders(buildPayload(view, draft));
      // Refetch-shaped: the PUT returns the fresh masked view — reset the form to it.
      setView(next);
      setDraft(draftFromView(next));
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function onTest(p: MetadataProvider) {
    if (!draft) return;
    setTests((prev) => ({ ...prev, [p.slug]: { status: "pending" } }));
    const typed = draft.key[p.slug] ?? "";
    const base = draft.baseURL[p.slug] ?? p.baseURL;
    const creds: { apiKey?: string; baseURL?: string } = {};
    if (typed !== "") creds.apiKey = typed;
    if (base !== p.baseURL) creds.baseURL = base;
    try {
      const result = await apiClient.testMetadataProvider(p.slug, creds);
      setTests((prev) => ({ ...prev, [p.slug]: { status: "done", result } }));
    } catch (err) {
      setTests((prev) => ({
        ...prev,
        [p.slug]: { status: "done", result: { ok: false, detail: errorMessage(err) } },
      }));
    }
  }

  if (loadError) {
    return (
      <section className="admin-providers" data-testid="admin-providers">
        <p
          className="status status-error"
          data-testid="admin-providers-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {loadError}{" "}
          <button
            className="nav-link"
            type="button"
            data-testid="admin-providers-retry"
            onClick={() => void load()}
          >
            Retry
          </button>
        </p>
      </section>
    );
  }

  if (!view || !draft) {
    return (
      <section className="admin-providers" data-testid="admin-providers">
        <p className="status status-loading" data-testid="admin-providers-loading">
          Loading providers&hellip;
        </p>
      </section>
    );
  }

  return (
    <section className="admin-providers" data-testid="admin-providers">
      <p className="admin-providers-intro">
        Configure which external metadata sources decorate your library. Changes
        take effect immediately — no restart. Keys are stored on the server and
        never shown again.
      </p>

      {/* The master consent switch (ADR-0032): the operator's off switch for all
          outbound metadata calls, above the per-provider configuration. */}
      <EnrichmentConsentControl />

      {KIND_GROUPS.map(({ kind, label }) => {
        const inGroup = view.providers.filter((p) => p.kinds.includes(kind));
        if (inGroup.length === 0) return null;
        const kindOn = kind === "video" ? view.enablement.video : view.enablement.music;
        return (
          <div
            key={kind}
            className="provider-group card"
            data-testid={`provider-group-${kind}`}
          >
            <div className="provider-group-head">
              <h2 className="section-title">{label}</h2>
              <span
                className="provider-kind-status"
                data-testid={`provider-kind-status-${kind}`}
                data-enabled={kindOn ? "true" : "false"}
              >
                {kindOn ? "Enrichment on" : "Enrichment off"}
              </span>
            </div>

            <ul className="provider-list">
              {inGroup.map((p) => {
                const test = tests[p.slug] ?? { status: "idle" };
                return (
                  <li
                    className="provider-row"
                    data-testid="provider-row"
                    data-slug={p.slug}
                    key={p.slug}
                  >
                    <div className="provider-head">
                      <span
                        className="provider-name"
                        data-testid={`provider-name-${p.slug}`}
                      >
                        {p.name}
                      </span>
                      <span
                        className="provider-role-badge"
                        data-testid={`provider-role-${p.slug}`}
                        data-role={p.role}
                      >
                        {p.role === "authoritative" ? "Authoritative" : "Supplement"}
                      </span>
                      <span className="provider-kinds">{p.kinds.join(", ")}</span>
                      <label className="provider-toggle">
                        <input
                          type="checkbox"
                          data-testid={`provider-toggle-${p.slug}`}
                          checked={draft.enabled[p.slug]}
                          onChange={(e) =>
                            patch((d) => ({
                              ...d,
                              enabled: { ...d.enabled, [p.slug]: e.target.checked },
                            }))
                          }
                          disabled={saving}
                        />{" "}
                        Enabled
                      </label>
                    </div>

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
                          value={draft.key[p.slug] ?? ""}
                          cleared={draft.clearKey[p.slug] ?? false}
                          disabled={saving}
                          onChange={(value) =>
                            patch((d) => ({
                              ...d,
                              key: { ...d.key, [p.slug]: value },
                              clearKey: { ...d.clearKey, [p.slug]: false },
                            }))
                          }
                          onClear={() =>
                            patch((d) => ({
                              ...d,
                              clearKey: { ...d.clearKey, [p.slug]: true },
                              key: { ...d.key, [p.slug]: "" },
                            }))
                          }
                        />
                      </div>
                    )}

                    <details className="provider-advanced">
                      <summary>Advanced</summary>
                      <div className="field">
                        <label
                          className="field-label"
                          htmlFor={`provider-baseurl-${p.slug}`}
                        >
                          Base URL override
                        </label>
                        <input
                          id={`provider-baseurl-${p.slug}`}
                          className="field-input"
                          data-testid={`provider-baseurl-${p.slug}`}
                          type="text"
                          value={draft.baseURL[p.slug] ?? ""}
                          onChange={(e) =>
                            patch((d) => ({
                              ...d,
                              baseURL: { ...d.baseURL, [p.slug]: e.target.value },
                            }))
                          }
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
                            value={draft.imageBaseURL[p.slug] ?? ""}
                            onChange={(e) =>
                              patch((d) => ({
                                ...d,
                                imageBaseURL: {
                                  ...d.imageBaseURL,
                                  [p.slug]: e.target.value,
                                },
                              }))
                            }
                            disabled={saving}
                          />
                        </div>
                      )}
                    </details>

                    <div className="provider-test">
                      <button
                        className="nav-link"
                        type="button"
                        data-testid={`provider-test-${p.slug}`}
                        onClick={() => void onTest(p)}
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
                  </li>
                );
              })}
            </ul>
          </div>
        );
      })}

      <div className="field provider-language card">
        <label className="field-label" htmlFor="metadata-language-input">
          Metadata language
        </label>
        <input
          id="metadata-language-input"
          className="field-input"
          data-testid="metadata-language-input"
          type="text"
          value={draft.language}
          placeholder="en-US"
          onChange={(e) => patch((d) => ({ ...d, language: e.target.value }))}
          disabled={saving}
        />
      </div>

      <div
        className="provider-behavior card"
        data-testid="provider-behavior"
      >
        <h2 className="section-title">Enrichment behavior</h2>
        <label className="provider-toggle">
          <input
            type="checkbox"
            data-testid="auto-enrich-after-scan-input"
            checked={draft.autoEnrichAfterScan}
            onChange={(e) =>
              patch((d) => ({ ...d, autoEnrichAfterScan: e.target.checked }))
            }
            disabled={saving}
          />{" "}
          Auto-enrich after a scan completes
        </label>
        <div className="field">
          <label className="field-label" htmlFor="enrich-interval-minutes-input">
            Scheduled sweep interval (minutes)
          </label>
          <input
            id="enrich-interval-minutes-input"
            className="field-input"
            data-testid="enrich-interval-minutes-input"
            type="number"
            min={0}
            value={draft.enrichIntervalMinutes}
            onChange={(e) =>
              patch((d) => ({ ...d, enrichIntervalMinutes: e.target.value }))
            }
            disabled={saving}
          />
          <span className="field-hint">0 disables the scheduled sweep.</span>
        </div>
        <div className="field">
          <label className="field-label" htmlFor="musicbrainz-rate-limit-input">
            MusicBrainz rate limit (ms)
          </label>
          <input
            id="musicbrainz-rate-limit-input"
            className="field-input"
            data-testid="musicbrainz-rate-limit-input"
            type="number"
            min={0}
            value={draft.musicBrainzRateLimitMs}
            onChange={(e) =>
              patch((d) => ({ ...d, musicBrainzRateLimitMs: e.target.value }))
            }
            disabled={saving}
          />
          <span className="field-hint">0 = no throttle.</span>
        </div>
      </div>

      <div className="provider-save-bar">
        <button
          className="auth-submit"
          type="button"
          data-testid="save-providers-button"
          onClick={() => void onSave()}
          disabled={saving}
        >
          {saving ? "Saving…" : "Save settings"}
        </button>
        {saved && (
          <span
            className="status status-ok"
            data-testid="save-providers-success"
            role="status"
          >
            Settings saved.
          </span>
        )}
        {saveError && (
          <p
            className="status status-error"
            data-testid="save-providers-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {saveError}
          </p>
        )}
      </div>
    </section>
  );
}
