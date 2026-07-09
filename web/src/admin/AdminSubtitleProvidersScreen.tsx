import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type {
  SubtitleProvider,
  SubtitleProvidersView,
  SubtitleProviderUpdate,
  TestProviderResult,
  UpdateSubtitleProvidersInput,
} from "../api/types";
import MaskedKeyInput from "./MaskedKeyInput";

// The Subtitle Providers admin screen (subtitles/05, ADR-0021). The exact shape of
// the Metadata Providers screen, scoped to the subtitle-fetch provider surface: an
// Admin enables OpenSubtitles, enters/rotates/clears its API key (masked — the
// stored value is never shown), overrides its base URL (advanced), tests
// connectivity, and sets the auto-fetch-after-scan language (OFF by default). One
// Save persists a PARTIAL update; the server rebuilds + hot-swaps the running
// provider with no restart. Behind RequireAdmin and still server-enforced.

interface Draft {
  enabled: Record<string, boolean>;
  key: Record<string, string>;
  clearKey: Record<string, boolean>;
  baseURL: Record<string, string>;
  autoFetchLang: string;
}

function draftFromView(view: SubtitleProvidersView): Draft {
  const enabled: Record<string, boolean> = {};
  const key: Record<string, string> = {};
  const clearKey: Record<string, boolean> = {};
  const baseURL: Record<string, string> = {};
  for (const p of view.providers) {
    enabled[p.slug] = p.enabled;
    key[p.slug] = "";
    clearKey[p.slug] = false;
    baseURL[p.slug] = p.baseURL;
  }
  return { enabled, key, clearKey, baseURL, autoFetchLang: view.autoFetchLang };
}

// buildPayload emits ONLY the providers the Admin changed (partial update), so an
// untouched key-requiring source is never dragged into a KEY_REQUIRED rejection.
function buildPayload(view: SubtitleProvidersView, draft: Draft): UpdateSubtitleProvidersInput {
  const providers: SubtitleProviderUpdate[] = [];
  for (const p of view.providers) {
    const u: SubtitleProviderUpdate = { slug: p.slug };
    let changed = false;
    if (draft.enabled[p.slug] !== p.enabled) {
      u.enabled = draft.enabled[p.slug];
      changed = true;
    }
    if (draft.clearKey[p.slug]) {
      u.apiKey = "";
      changed = true;
    } else if (draft.key[p.slug] !== "") {
      u.apiKey = draft.key[p.slug];
      changed = true;
    }
    if (draft.baseURL[p.slug] !== p.baseURL) {
      u.baseURL = draft.baseURL[p.slug];
      changed = true;
    }
    if (changed) providers.push(u);
  }
  const payload: UpdateSubtitleProvidersInput = {};
  if (providers.length) payload.providers = providers;
  if (draft.autoFetchLang !== view.autoFetchLang) payload.autoFetchLang = draft.autoFetchLang;
  return payload;
}

export default function AdminSubtitleProvidersScreen() {
  const [view, setView] = useState<SubtitleProvidersView | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [tests, setTests] = useState<Record<string, TestProviderResult | "pending">>({});

  const load = useCallback(async () => {
    try {
      const v = await apiClient.getSubtitleProviders();
      setView(v);
      setDraft(draftFromView(v));
    } catch (e) {
      setError(errorMessage(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function onSave() {
    if (!view || !draft) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await apiClient.updateSubtitleProviders(buildPayload(view, draft));
      setView(updated);
      setDraft(draftFromView(updated));
      setSaved(true);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  }

  async function onTest(p: SubtitleProvider) {
    if (!draft) return;
    setTests((t) => ({ ...t, [p.slug]: "pending" }));
    try {
      const creds: { apiKey?: string; baseURL?: string } = {};
      if (draft.key[p.slug] !== "") creds.apiKey = draft.key[p.slug];
      if (draft.baseURL[p.slug] !== p.baseURL) creds.baseURL = draft.baseURL[p.slug];
      const res = await apiClient.testSubtitleProvider(p.slug, creds);
      setTests((t) => ({ ...t, [p.slug]: res }));
    } catch (e) {
      setTests((t) => ({ ...t, [p.slug]: { ok: false, detail: errorMessage(e) } }));
    }
  }

  if (error && !view) {
    return (
      <div className="admin-section" data-testid="subtitle-providers-error">
        <p className="form-error">{error}</p>
      </div>
    );
  }
  if (!view || !draft) {
    return <div className="admin-section" data-testid="subtitle-providers-loading">Loading…</div>;
  }

  return (
    <div className="admin-section" data-testid="subtitle-providers-screen">
      <h2 className="admin-section-title">Subtitle Providers</h2>
      <p className="admin-section-note">
        Fetch subtitles from an external provider when a title lacks one in a language
        you can read. Matched to your exact release for in-sync subtitles.
      </p>

      {view.providers.map((p) => {
        const test = tests[p.slug];
        return (
          <div className="provider-card" key={p.slug} data-testid={`subtitle-provider-${p.slug}`}>
            <div className="provider-head">
              <label className="provider-enable">
                <input
                  type="checkbox"
                  data-testid={`subtitle-provider-enable-${p.slug}`}
                  checked={draft.enabled[p.slug]}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      enabled: { ...draft.enabled, [p.slug]: e.target.checked },
                    })
                  }
                />
                <span className="provider-name">{p.name}</span>
              </label>
              <a className="provider-docs" href={p.docsURL} target="_blank" rel="noreferrer">
                Get a key
              </a>
            </div>
            <p className="provider-desc">{p.description}</p>

            {p.requiresKey && (
              <div className="field">
                <label className="field-label">API key</label>
                <MaskedKeyInput
                  slug={p.slug}
                  hasKey={p.hasKey}
                  value={draft.key[p.slug]}
                  cleared={draft.clearKey[p.slug]}
                  onChange={(v) =>
                    setDraft({
                      ...draft,
                      key: { ...draft.key, [p.slug]: v },
                      clearKey: { ...draft.clearKey, [p.slug]: false },
                    })
                  }
                  onClear={() =>
                    setDraft({
                      ...draft,
                      clearKey: { ...draft.clearKey, [p.slug]: !draft.clearKey[p.slug] },
                      key: { ...draft.key, [p.slug]: "" },
                    })
                  }
                  disabled={saving}
                />
              </div>
            )}

            <div className="field">
              <label className="field-label">Base URL (advanced)</label>
              <input
                className="field-input"
                data-testid={`subtitle-provider-baseurl-${p.slug}`}
                value={draft.baseURL[p.slug]}
                onChange={(e) =>
                  setDraft({ ...draft, baseURL: { ...draft.baseURL, [p.slug]: e.target.value } })
                }
                disabled={saving}
              />
            </div>

            <div className="provider-test">
              <button
                className="btn"
                type="button"
                data-testid={`subtitle-provider-test-${p.slug}`}
                onClick={() => onTest(p)}
                disabled={test === "pending"}
              >
                {test === "pending" ? "Testing…" : "Test connection"}
              </button>
              {test && test !== "pending" && (
                <span
                  className={`provider-test-result ${test.ok ? "is-ok" : "is-error"}`}
                  data-testid={`subtitle-provider-test-result-${p.slug}`}
                >
                  {test.ok ? "✓ " : "✗ "}
                  {test.detail}
                </span>
              )}
            </div>
          </div>
        );
      })}

      <div className="field">
        <label className="field-label" htmlFor="subtitle-auto-fetch-lang">
          Auto-fetch language after scan
        </label>
        <input
          id="subtitle-auto-fetch-lang"
          className="field-input"
          data-testid="subtitle-auto-fetch-lang"
          placeholder="off (e.g. en, de)"
          value={draft.autoFetchLang}
          onChange={(e) => setDraft({ ...draft, autoFetchLang: e.target.value.trim() })}
          disabled={saving}
        />
        <p className="field-hint">
          Leave blank to keep auto-fetch OFF (recommended — a large library can exhaust
          the provider's download quota).
        </p>
      </div>

      {error && <p className="form-error" data-testid="subtitle-providers-save-error">{error}</p>}
      {saved && <p className="form-note" data-testid="subtitle-providers-saved">Saved.</p>}

      <div className="admin-actions">
        <button
          className="btn btn-primary"
          type="button"
          data-testid="subtitle-providers-save"
          onClick={onSave}
          disabled={saving}
        >
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
