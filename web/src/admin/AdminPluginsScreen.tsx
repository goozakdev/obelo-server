import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import PluginSettingsForm from "./PluginSettingsForm";
import type { InstalledPlugin, InstalledPluginsView } from "../api/types";

// The Plugins admin screen (ADR-0058, plugin-system/10): where an Admin puts code
// on a running server, and takes it off again.
//
// It is deliberately NOT where a plugin is configured. What a plugin DOES — the
// URL a sink posts to, the events it hears, the key it signs with — is on the
// screen for its Extension point, in the same card as the Built-in beside it,
// because ADR-0057's whole claim is that nothing downstream can tell the two
// apart. Duplicating those controls here would be the first place that claim
// stopped being true.
//
// Two states the screen has to keep separate, and they are the reason the cards
// look the way they do:
//
//   enabled            — the Admin's own switch. Off means the plugin is not
//                        registered at all, so nothing can deliver to it.
//   disabledByFailure  — this server refusing to call it: it would not load, or it
//                        failed enough times in a row to be stopped. lastError is
//                        the sentence that says why, and Re-enable is how an Admin
//                        says "I fixed it, try again".
//
// A plugin can be both at once, and that is exactly the state that needs
// explaining rather than collapsing into one word.

// PLUGIN_LABELS maps the contract's Extension-point tokens to what an operator
// calls them. An unknown token is shown verbatim rather than hidden: a server one
// version ahead of this bundle should not make a plugin look like it provides
// nothing.
const EXTENSION_POINT_LABELS: Record<string, string> = {
  "event-sink": "Event sink",
  "metadata-provider": "Metadata provider",
  "subtitle-provider": "Subtitle provider",
};

function providesLabel(provides: string[]): string {
  if (!provides.length) return "nothing this server recognises";
  return provides.map((p) => EXTENSION_POINT_LABELS[p] ?? p).join(", ");
}

// sourceLabel turns the recorded provenance into something readable. A URL is
// shown as-is because it is the thing an Admin would paste again.
function sourceLabel(source?: string): string {
  if (!source) return "";
  if (source === "upload") return "Uploaded";
  if (source === "placed by hand") return "Placed in the data directory by hand";
  return source;
}

function PluginCard({
  plugin,
  busy,
  onEnable,
  onDisable,
  onReenable,
  onUninstall,
  onSettingsSaved,
}: {
  plugin: InstalledPlugin;
  busy: boolean;
  onEnable: () => void;
  onDisable: () => void;
  onReenable: () => void;
  onUninstall: () => void;
  onSettingsSaved: (view: InstalledPluginsView) => void;
}) {
  const { id } = plugin;
  return (
    <div className="provider-card" data-testid={`plugin-${id}`}>
      <div className="provider-head">
        <span className="provider-name">{plugin.name}</span>
        {plugin.version && (
          <span className="plugin-version" data-testid={`plugin-version-${id}`}>
            v{plugin.version}
          </span>
        )}
      </div>

      <p className="provider-desc" data-testid={`plugin-provides-${id}`}>
        Provides: {providesLabel(plugin.provides)}
      </p>

      <dl className="plugin-facts">
        <div>
          <dt>Status</dt>
          <dd data-testid={`plugin-status-${id}`}>
            {!plugin.enabled
              ? "Switched off"
              : plugin.disabledByFailure
                ? "Stopped by this server"
                : "Running"}
          </dd>
        </div>
        {plugin.source && (
          <div>
            <dt>Installed from</dt>
            <dd data-testid={`plugin-source-${id}`}>{sourceLabel(plugin.source)}</dd>
          </div>
        )}
        {plugin.installedAt && (
          <div>
            <dt>Installed</dt>
            <dd data-testid={`plugin-installed-at-${id}`}>{plugin.installedAt}</dd>
          </div>
        )}
      </dl>

      {plugin.lastError && (
        <p className="form-error" data-testid={`plugin-error-${id}`}>
          {plugin.lastError}
        </p>
      )}

      {/* The plugin's OWN settings, rendered from the schema its manifest
          declared (plugin-system/13). A plugin that declares none has no panel at
          all rather than an empty one, which is every plugin configured entirely
          through the fixed shape. */}
      <PluginSettingsForm plugin={plugin} disabled={busy} onSaved={onSettingsSaved} />

      <div className="admin-actions">
        {plugin.enabled ? (
          <button
            className="btn"
            type="button"
            data-testid={`plugin-disable-${id}`}
            onClick={onDisable}
            disabled={busy}
          >
            Disable
          </button>
        ) : (
          <button
            className="btn"
            type="button"
            data-testid={`plugin-enable-${id}`}
            onClick={onEnable}
            disabled={busy}
          >
            Enable
          </button>
        )}
        {/* Re-enable is offered only when there is something to forgive. A button
            that does nothing is a button an Admin presses and then wonders about. */}
        {(plugin.disabledByFailure || plugin.lastError) && (
          <button
            className="btn"
            type="button"
            data-testid={`plugin-reenable-${id}`}
            onClick={onReenable}
            disabled={busy}
          >
            Re-enable
          </button>
        )}
        <button
          className="btn btn-danger"
          type="button"
          data-testid={`plugin-uninstall-${id}`}
          onClick={onUninstall}
          disabled={busy}
        >
          Uninstall
        </button>
      </div>
    </div>
  );
}

export default function AdminPluginsScreen() {
  const [view, setView] = useState<InstalledPluginsView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [url, setUrl] = useState("");
  const manifestRef = useRef<HTMLInputElement | null>(null);
  const moduleRef = useRef<HTMLInputElement | null>(null);

  const load = useCallback(async () => {
    try {
      setView(await apiClient.getPlugins());
    } catch (e) {
      setLoadError(errorMessage(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // run wraps every verb identically: clear the last outcome, call, and take the
  // WHOLE list back from the response. Every endpoint answers with the full list
  // for exactly this reason — the screen never has to reconcile, and it can never
  // show an action's result against a stale row.
  async function run(action: () => Promise<InstalledPluginsView>, message: string) {
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      setView(await action());
      setNotice(message);
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function onUpload() {
    const manifest = manifestRef.current?.files?.[0];
    const module = moduleRef.current?.files?.[0];
    if (!manifest || !module) {
      setActionError("Choose both the manifest.json and the .wasm module.");
      setNotice(null);
      return;
    }
    await run(() => apiClient.installPlugin(manifest, module), "Installed.");
    if (manifestRef.current) manifestRef.current.value = "";
    if (moduleRef.current) moduleRef.current.value = "";
  }

  async function onInstallFromURL() {
    const target = url.trim();
    if (!target) {
      setActionError("Paste the URL of a plugin's manifest.json.");
      setNotice(null);
      return;
    }
    await run(() => apiClient.installPluginFromURL({ url: target }), "Installed.");
    setUrl("");
  }

  if (loadError && !view) {
    return (
      <div className="admin-section" data-testid="plugins-error">
        <p className="form-error">{loadError}</p>
      </div>
    );
  }
  if (!view) {
    return (
      <div className="admin-section" data-testid="plugins-loading">
        Loading…
      </div>
    );
  }

  return (
    <div className="admin-section" data-testid="plugins-screen">
      <h2 className="admin-section-title">Plugins</h2>
      <p className="admin-section-note">
        Add a source or an integration this server did not ship with. A plugin runs
        in a sandbox with no filesystem and no network of its own — its only way out
        is a request this server makes on its behalf, to the hosts its manifest
        names. Installing one takes effect immediately; nothing here needs a
        restart.
      </p>

      {view.plugins.length === 0 ? (
        <p className="admin-section-note" data-testid="plugins-empty">
          Nothing is installed. Everything this server does today is built in.
        </p>
      ) : (
        view.plugins.map((p) => (
          <PluginCard
            key={p.id}
            plugin={p}
            busy={busy}
            onEnable={() => void run(() => apiClient.enablePlugin(p.id), "Enabled.")}
            onDisable={() => void run(() => apiClient.disablePlugin(p.id), "Disabled.")}
            onReenable={() => void run(() => apiClient.reenablePlugin(p.id), "Re-enabled.")}
            onUninstall={() => void run(() => apiClient.uninstallPlugin(p.id), "Uninstalled.")}
            onSettingsSaved={setView}
          />
        ))
      )}

      {actionError && (
        <p className="form-error" data-testid="plugins-action-error">
          {actionError}
        </p>
      )}
      {notice && (
        <p className="form-note" data-testid="plugins-notice">
          {notice}
        </p>
      )}

      <div className="provider-card" data-testid="plugin-install-upload">
        <div className="provider-head">
          <span className="provider-name">Install from files</span>
        </div>
        <p className="provider-desc">
          A plugin is two files: its <code>manifest.json</code> and its{" "}
          <code>.wasm</code> module.
        </p>
        <div className="field">
          <label className="field-label" htmlFor="plugin-manifest-file">
            manifest.json
          </label>
          <input
            id="plugin-manifest-file"
            className="field-input"
            data-testid="plugin-manifest-file"
            type="file"
            accept=".json,application/json"
            ref={manifestRef}
            disabled={busy}
          />
        </div>
        <div className="field">
          <label className="field-label" htmlFor="plugin-module-file">
            Module
          </label>
          <input
            id="plugin-module-file"
            className="field-input"
            data-testid="plugin-module-file"
            type="file"
            accept=".wasm,application/wasm"
            ref={moduleRef}
            disabled={busy}
          />
        </div>
        <div className="admin-actions">
          <button
            className="btn btn-primary"
            type="button"
            data-testid="plugin-upload"
            onClick={() => void onUpload()}
            disabled={busy}
          >
            {busy ? "Working…" : "Install"}
          </button>
        </div>
      </div>

      <div className="provider-card" data-testid="plugin-install-url">
        <div className="provider-head">
          <span className="provider-name">Install from a URL</span>
        </div>
        <p className="provider-desc">
          Paste the address of a plugin's <code>manifest.json</code>. Its module is
          fetched from the same directory. A plugin is code this server will run, so
          an address on your own network is refused here — upload the files instead.
        </p>
        <div className="field">
          <label className="field-label" htmlFor="plugin-url">
            Manifest URL
          </label>
          <input
            id="plugin-url"
            className="field-input"
            data-testid="plugin-url"
            placeholder="https://example.com/obelo-discord/manifest.json"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            disabled={busy}
          />
        </div>
        <div className="admin-actions">
          <button
            className="btn btn-primary"
            type="button"
            data-testid="plugin-install-from-url"
            onClick={() => void onInstallFromURL()}
            disabled={busy}
          >
            {busy ? "Working…" : "Fetch and install"}
          </button>
        </div>
      </div>
    </div>
  );
}
