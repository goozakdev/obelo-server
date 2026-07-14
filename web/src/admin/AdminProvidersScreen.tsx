import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type {
  MetadataProvider,
  MetadataProvidersView,
  UpdateMetadataProvidersInput,
} from "../api/types";
import EnrichmentConsentControl from "./EnrichmentConsentControl";
import ProviderConfigDialog from "./ProviderConfigDialog";

// The Metadata Providers admin screen (metadata-providers redesign). Behind
// RequireAdmin (App.tsx) and still server-enforced (the /settings API is Admin
// scope regardless of the client gate). An Admin sees every external metadata
// source the server knows about, grouped by the media kind it serves (Video,
// Music). Within a group the authoritative source is pinned to the top and
// labeled; the rest follow. Each source is a compact row with:
//   - an Enabled checkbox that persists IMMEDIATELY (save-per-action), and
//   - an Edit icon that opens a per-provider configuration dialog (API key,
//     base-URL overrides, Test connection) which saves just that provider.
// The stored API key is never shown — only a "configured"/"not set" indicator.
// Saving takes effect at runtime with no restart (the server rebuilds + hot-swaps
// the active provider).
//
// Server-wide knobs that are NOT per-provider — the metadata language and the
// enrichment-behavior trio — keep their own explicit "Save settings" button at
// the bottom, since a number field shouldn't persist mid-typing.

// The coarse kind groups, in display order (video first).
const KIND_GROUPS: { kind: string; label: string }[] = [
  { kind: "video", label: "Video" },
  { kind: "music", label: "Music" },
];

/** Local edits for the server-wide settings block (the only draft the screen
 * still holds; per-provider edits live in the config dialog — including the
 * MusicBrainz throttle, which lives in that provider's dialog). The interval is
 * edited in MINUTES (converted to/from the API's enrichIntervalSeconds); it is
 * held as a string so the controlled input can be cleared. */
interface SettingsDraft {
  language: string;
  autoEnrichAfterScan: boolean;
  enrichIntervalMinutes: string;
}

function settingsDraftFromView(view: MetadataProvidersView): SettingsDraft {
  return {
    language: view.metadataLanguage,
    autoEnrichAfterScan: view.autoEnrichAfterScan,
    enrichIntervalMinutes: String(view.enrichIntervalSeconds / 60),
  };
}

// PencilIcon is the Edit affordance on each provider row — a plain stroke glyph
// that inherits the row's text color (theme-aware, no asset).
function PencilIcon() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="15"
      height="15"
      aria-hidden="true"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M11.2 2.3l2.5 2.5" />
      <path d="M12.4 1.1a1.2 1.2 0 0 1 1.7 1.7L4.9 12l-3 .8.8-3z" />
    </svg>
  );
}

export default function AdminProvidersScreen() {
  const [view, setView] = useState<MetadataProvidersView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [settings, setSettings] = useState<SettingsDraft | null>(null);
  // Per-provider enable-toggle state: which slug's toggle is in flight, and the
  // last inline error for a row whose immediate save was refused.
  const [toggling, setToggling] = useState<Record<string, boolean>>({});
  const [rowError, setRowError] = useState<Record<string, string | null>>({});
  // The provider whose configuration dialog is open (null = none).
  const [editing, setEditing] = useState<MetadataProvider | null>(null);
  // Server-wide settings Save state.
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoadError(null);
    try {
      const v = await apiClient.getMetadataProviders(signal);
      if (signal?.aborted) return;
      setView(v);
      setSettings(settingsDraftFromView(v));
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

  function patchSettings(update: (d: SettingsDraft) => SettingsDraft) {
    setSettings((prev) => (prev ? update(prev) : prev));
    setSaved(false);
  }

  // Toggling a provider's Enabled checkbox persists immediately (save-per-action).
  // The checkbox is driven by the loaded view, so on a refused save the view is
  // untouched and the box simply stays where it was — we only surface the error.
  async function onToggle(p: MetadataProvider, enabled: boolean) {
    setRowError((prev) => ({ ...prev, [p.slug]: null }));
    setToggling((prev) => ({ ...prev, [p.slug]: true }));
    try {
      const next = await apiClient.updateMetadataProviders({
        providers: [{ slug: p.slug, enabled }],
      });
      setView(next);
    } catch (err) {
      setRowError((prev) => ({ ...prev, [p.slug]: errorMessage(err) }));
    } finally {
      setToggling((prev) => ({ ...prev, [p.slug]: false }));
    }
  }

  // A per-provider dialog save returns the fresh masked view; adopt it and clear
  // any stale row error for that provider.
  function onProviderSaved(next: MetadataProvidersView) {
    setView(next);
    if (editing) setRowError((prev) => ({ ...prev, [editing.slug]: null }));
  }

  // Build the server-wide settings payload (language + behavior) from what changed.
  function buildSettingsPayload(
    v: MetadataProvidersView,
    d: SettingsDraft,
  ): UpdateMetadataProvidersInput {
    const payload: UpdateMetadataProvidersInput = {};
    if (d.language !== v.metadataLanguage) payload.metadataLanguage = d.language;
    if (d.autoEnrichAfterScan !== v.autoEnrichAfterScan) {
      payload.autoEnrichAfterScan = d.autoEnrichAfterScan;
    }
    const minutes = Number(d.enrichIntervalMinutes);
    if (d.enrichIntervalMinutes.trim() !== "" && Number.isFinite(minutes)) {
      const seconds = Math.round(minutes * 60);
      if (seconds !== v.enrichIntervalSeconds) payload.enrichIntervalSeconds = seconds;
    }
    return payload;
  }

  async function onSaveSettings() {
    if (!view || !settings || saving) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const next = await apiClient.updateMetadataProviders(
        buildSettingsPayload(view, settings),
      );
      setView(next);
      setSettings(settingsDraftFromView(next));
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
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

  if (!view || !settings) {
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
        Configure which external metadata sources decorate your library. Enabling a
        source and editing its settings take effect immediately — no restart. Keys
        are stored on the server and never shown again.
      </p>

      {/* The master consent switch (ADR-0032): the operator's off switch for all
          outbound metadata calls, above the per-provider configuration. */}
      <EnrichmentConsentControl />

      {KIND_GROUPS.map(({ kind, label }) => {
        const inGroup = view.providers.filter((p) => p.kinds.includes(kind));
        if (inGroup.length === 0) return null;
        // The single authoritative source for the kind is the first provider whose
        // role is authoritative (registry order → TMDB for video, MusicBrainz for
        // music); pin it to the top, the rest follow in their existing order.
        const authIdx = inGroup.findIndex((p) => p.role === "authoritative");
        const authSlug = authIdx >= 0 ? inGroup[authIdx].slug : null;
        const ordered =
          authIdx > 0
            ? [inGroup[authIdx], ...inGroup.filter((_, i) => i !== authIdx)]
            : inGroup;
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
              {ordered.map((p) => {
                const isAuth = p.slug === authSlug;
                return (
                  <li
                    className="provider-row"
                    data-testid="provider-row"
                    data-slug={p.slug}
                    data-authoritative={isAuth ? "true" : undefined}
                    key={p.slug}
                  >
                    <div className="provider-row-main">
                      <input
                        type="checkbox"
                        className="provider-enable"
                        data-testid={`provider-toggle-${p.slug}`}
                        aria-label={`Enable ${p.name}`}
                        checked={p.enabled}
                        onChange={(e) => void onToggle(p, e.target.checked)}
                        disabled={toggling[p.slug]}
                      />
                      <span
                        className="provider-name"
                        data-testid={`provider-name-${p.slug}`}
                      >
                        {p.name}
                      </span>
                      {isAuth && (
                        <span
                          className="provider-role-badge"
                          data-testid={`provider-role-${p.slug}`}
                          data-role="authoritative"
                        >
                          Authoritative
                        </span>
                      )}
                      <button
                        className="provider-edit"
                        type="button"
                        data-testid={`provider-edit-${p.slug}`}
                        aria-label={`Configure ${p.name}`}
                        onClick={() => setEditing(p)}
                      >
                        <PencilIcon />
                      </button>
                    </div>
                    {rowError[p.slug] && (
                      <p
                        className="status status-error provider-row-error"
                        data-testid={`provider-row-error-${p.slug}`}
                        role="alert"
                      >
                        <span className="dot dot-error" aria-hidden="true" />
                        {rowError[p.slug]}
                      </p>
                    )}
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
          value={settings.language}
          placeholder="en-US"
          onChange={(e) => patchSettings((d) => ({ ...d, language: e.target.value }))}
          disabled={saving}
        />
      </div>

      <div className="provider-behavior card" data-testid="provider-behavior">
        <h2 className="section-title">Enrichment behavior</h2>
        <label className="provider-toggle">
          <input
            type="checkbox"
            data-testid="auto-enrich-after-scan-input"
            checked={settings.autoEnrichAfterScan}
            onChange={(e) =>
              patchSettings((d) => ({ ...d, autoEnrichAfterScan: e.target.checked }))
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
            value={settings.enrichIntervalMinutes}
            onChange={(e) =>
              patchSettings((d) => ({ ...d, enrichIntervalMinutes: e.target.value }))
            }
            disabled={saving}
          />
          <span className="field-hint">0 disables the scheduled sweep.</span>
        </div>
      </div>

      <div className="provider-save-bar">
        <button
          className="auth-submit"
          type="button"
          data-testid="save-providers-button"
          onClick={() => void onSaveSettings()}
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

      {editing && (
        <ProviderConfigDialog
          key={editing.slug}
          provider={editing}
          musicBrainzRateLimitMs={view.musicBrainzRateLimitMs}
          onSaved={onProviderSaved}
          onClose={() => setEditing(null)}
        />
      )}
    </section>
  );
}
