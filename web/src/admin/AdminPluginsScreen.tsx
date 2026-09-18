import { useCallback, useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import PluginSettingsForm from "./PluginSettingsForm";
import type {
  InstalledPlugin,
  InstalledPluginsView,
  PluginCatalogEntry,
  PluginCatalogView,
  PluginPublishersView,
} from "../api/types";

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
//
// # The two OPTIONAL things on this screen (plugin-system/15)
//
// A catalog to browse, and publisher keys to pin. Both are off until an Admin
// turns them on, because this project runs no catalog and vouches for no
// publisher (ADR-0001), and the screen is built so that BOTH BEING OFF IS NOT A
// DEGRADED STATE: with no catalog there is no Browse tab and no tab bar at all,
// and with no pinned keys the publisher card says in words that nothing is
// checked.
//
// The one behaviour worth stating up front: A CATALOG THAT WILL NOT LOAD IS A
// NOTE, NEVER AN ERROR. The upload and paste-URL cards have nothing to do with
// the catalog, so an index that is down must leave them exactly where they were
// and say one sentence about itself. The server sends that sentence in `error`
// beside a 200 for the same reason.

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
        {/* Shown only when a signature actually VERIFIED against a pinned key.
            There is deliberately no "Unsigned" row for the plugins without one:
            an empty publisher means nobody checked, which is a different claim
            and not one this server is in a position to make. */}
        {plugin.publisher && (
          <div>
            <dt>Signed by</dt>
            <dd data-testid={`plugin-publisher-${id}`}>
              {plugin.publisher}
              {plugin.keyId && ` (key ${plugin.keyId})`}
            </dd>
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

// CatalogBrowser is the Browse tab: what the operator's chosen index offers, and
// an Install button per entry.
//
// Every fact on a row is the INDEX AUTHOR'S CLAIM, because the manifest fetched
// at install time is what actually decides all of them. `publisher` is the
// sharpest case: it is a word in somebody's file until a key is pinned for it, so
// it is labelled "Published by (claimed)" and never shown as a verified fact. The
// "Signed by" line on an installed card is the verified one, and the difference
// between the two labels is the whole of what signing buys.
function CatalogBrowser({
  catalog,
  installed,
  busy,
  onInstall,
}: {
  catalog: PluginCatalogView;
  installed: Set<string>;
  busy: boolean;
  onInstall: (entry: PluginCatalogEntry) => void;
}) {
  return (
    <div data-testid="plugin-catalog">
      <p className="admin-section-note">
        Plugins offered by the catalog you pointed this server at. Installing one
        fetches its manifest and module from the address the catalog gave, under
        exactly the rules that apply to an address you paste yourself.
      </p>

      {catalog.error && (
        <p className="form-note" data-testid="plugin-catalog-note">
          {catalog.error}
        </p>
      )}

      {catalog.entries.length === 0
        ? !catalog.error && (
            <p className="admin-section-note" data-testid="plugin-catalog-empty">
              This catalog is offering nothing at the moment.
            </p>
          )
        : catalog.entries.map((entry) => (
            <div
              className="provider-card"
              key={`${entry.id}-${entry.manifestUrl}`}
              data-testid={`catalog-entry-${entry.id}`}
            >
              <div className="provider-head">
                <span className="provider-name">{entry.name}</span>
                {entry.version && (
                  <span
                    className="plugin-version"
                    data-testid={`catalog-version-${entry.id}`}
                  >
                    v{entry.version}
                  </span>
                )}
              </div>
              {entry.description && (
                <p className="provider-desc">{entry.description}</p>
              )}
              <dl className="plugin-facts">
                <div>
                  <dt>Provides</dt>
                  <dd data-testid={`catalog-provides-${entry.id}`}>
                    {providesLabel(entry.provides ?? [])}
                  </dd>
                </div>
                {entry.publisher && (
                  <div>
                    <dt>Published by (claimed)</dt>
                    <dd data-testid={`catalog-publisher-${entry.id}`}>
                      {entry.publisher}
                    </dd>
                  </div>
                )}
                <div>
                  <dt>Manifest</dt>
                  <dd data-testid={`catalog-manifest-${entry.id}`}>
                    {entry.manifestUrl}
                  </dd>
                </div>
              </dl>
              <div className="admin-actions">
                {installed.has(entry.id) ? (
                  <span
                    className="form-note"
                    data-testid={`catalog-installed-${entry.id}`}
                  >
                    Already installed
                  </span>
                ) : (
                  <button
                    className="btn btn-primary"
                    type="button"
                    data-testid={`catalog-install-${entry.id}`}
                    onClick={() => onInstall(entry)}
                    disabled={busy}
                  >
                    {busy ? "Working…" : "Install"}
                  </button>
                )}
              </div>
            </div>
          ))}
    </div>
  );
}

export default function AdminPluginsScreen() {
  const [view, setView] = useState<InstalledPluginsView | null>(null);
  const [catalog, setCatalog] = useState<PluginCatalogView | null>(null);
  const [publishers, setPublishers] = useState<PluginPublishersView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [url, setUrl] = useState("");
  const [tab, setTab] = useState<"installed" | "browse">("installed");
  const [catalogUrl, setCatalogUrl] = useState("");
  const [publisherName, setPublisherName] = useState("");
  const [publisherKey, setPublisherKey] = useState("");
  const manifestRef = useRef<HTMLInputElement | null>(null);
  const moduleRef = useRef<HTMLInputElement | null>(null);
  const signatureRef = useRef<HTMLInputElement | null>(null);

  const load = useCallback(async () => {
    try {
      setView(await apiClient.getPlugins());
    } catch (e) {
      setLoadError(errorMessage(e));
    }
    // The catalog and the pinned keys are loaded SEPARATELY and swallow their own
    // failures. Neither is what this screen is for, and a server that cannot
    // answer about either must still let an Admin install and uninstall a plugin —
    // the same reason an unreachable catalog is a note rather than an error one
    // level down.
    try {
      const got = await apiClient.getPluginCatalog();
      setCatalog(got);
      setCatalogUrl(got.url);
    } catch {
      setCatalog(null);
    }
    try {
      setPublishers(await apiClient.getPluginPublishers());
    } catch {
      setPublishers(null);
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
    // The signature part is OPTIONAL and the form does not insist on it: most
    // plugins have none, and whether THIS server needs one is a question only the
    // server can answer, from the keys its Admin pinned.
    const signature = signatureRef.current?.files?.[0];
    await run(() => apiClient.installPlugin(manifest, module, signature), "Installed.");
    if (manifestRef.current) manifestRef.current.value = "";
    if (moduleRef.current) moduleRef.current.value = "";
    if (signatureRef.current) signatureRef.current.value = "";
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

  // Installing a catalog entry IS installing a URL. There is no catalog-specific
  // call and there must not be one: the entry's manifestUrl goes through exactly
  // the request an Admin's own pasted address goes through, so an entry pointing
  // into this network is refused by the same policy, in the same words.
  async function onInstallEntry(entry: PluginCatalogEntry) {
    await run(
      () =>
        apiClient.installPluginFromURL({
          url: entry.manifestUrl,
          ...(entry.signatureUrl ? { signatureUrl: entry.signatureUrl } : {}),
        }),
      `Installed ${entry.name}.`,
    );
  }

  // Saving the catalog address answers with a freshly fetched view, so an address
  // that does not answer says so at once rather than on the next page load.
  async function onSaveCatalog() {
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      const got = await apiClient.setPluginCatalog({ url: catalogUrl.trim() });
      setCatalog(got);
      setCatalogUrl(got.url);
      if (!got.url) setTab("installed");
      setNotice(got.url ? "Catalog saved." : "Catalog cleared.");
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function runPublishers(
    action: () => Promise<PluginPublishersView>,
    message: string,
  ) {
    setBusy(true);
    setActionError(null);
    setNotice(null);
    try {
      setPublishers(await action());
      setNotice(message);
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  async function onPinPublisher() {
    const name = publisherName.trim();
    const key = publisherKey.trim();
    if (!name || !key) {
      setActionError("A pinned key needs both a publisher name and the key itself.");
      setNotice(null);
      return;
    }
    await runPublishers(
      () => apiClient.pinPluginPublisher({ publisher: name, publicKey: key }),
      `Pinned ${name}.`,
    );
    setPublisherName("");
    setPublisherKey("");
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

  // THE TAB BAR EXISTS ONLY WHEN A CATALOG DOES. With none configured — the
  // default, and the state of every server that has not opted in — this screen is
  // exactly what it was before catalogs existed, rather than one tab of two with
  // the second permanently empty.
  const hasCatalog = Boolean(catalog?.url);
  const browsing = hasCatalog && tab === "browse";
  const installedIds = new Set(view.plugins.map((p) => p.id));

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

      {hasCatalog && (
        <div className="admin-actions" data-testid="plugin-tabs">
          <button
            className={tab === "installed" ? "btn btn-primary" : "btn"}
            type="button"
            data-testid="plugin-tab-installed"
            onClick={() => setTab("installed")}
          >
            Installed
          </button>
          <button
            className={tab === "browse" ? "btn btn-primary" : "btn"}
            type="button"
            data-testid="plugin-tab-browse"
            onClick={() => setTab("browse")}
          >
            Browse
          </button>
        </div>
      )}

      {browsing && catalog && (
        <>
          <CatalogBrowser
            catalog={catalog}
            installed={installedIds}
            busy={busy}
            onInstall={(entry) => void onInstallEntry(entry)}
          />
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
        </>
      )}

      {!browsing && (
        <>
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
          <div className="field">
            <label className="field-label" htmlFor="plugin-signature-file">
              Signature (optional)
            </label>
            <input
              id="plugin-signature-file"
              className="field-input"
              data-testid="plugin-signature-file"
              type="file"
              accept=".json,application/json"
              ref={signatureRef}
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
        </>
      )}

      {/* The catalog address. Shown on both tabs, because it is how an operator
          turns the Browse tab on in the first place and how they turn it off. */}
      <div className="provider-card" data-testid="plugin-catalog-settings">
        <div className="provider-head">
          <span className="provider-name">Catalog</span>
        </div>
        <p className="provider-desc">
          A catalog is a JSON index of plugins, published by whoever you decide to
          trust — your own, or a community's. This server ships with none and
          recommends none. Set an address to gain a Browse tab; clear it to lose
          one. Installing from a catalog is installing from an address, under
          exactly the rules that apply to one you paste yourself.
        </p>
        <div className="field">
          <label className="field-label" htmlFor="plugin-catalog-url">
            Catalog URL
          </label>
          <input
            id="plugin-catalog-url"
            className="field-input"
            data-testid="plugin-catalog-url"
            placeholder="https://example.com/obelo-plugins/index.json"
            value={catalogUrl}
            onChange={(e) => setCatalogUrl(e.target.value)}
            disabled={busy}
          />
        </div>
        <div className="admin-actions">
          <button
            className="btn btn-primary"
            type="button"
            data-testid="plugin-catalog-save"
            onClick={() => void onSaveCatalog()}
            disabled={busy}
          >
            {busy ? "Working…" : "Save"}
          </button>
        </div>
      </div>

      {/* Pinned publisher keys. The empty state is a POLICY and says so in words:
          an empty table with nothing under it reads as "not set up yet", which is
          the opposite of what it means. */}
      <div className="provider-card" data-testid="plugin-publishers">
        <div className="provider-head">
          <span className="provider-name">Publisher keys</span>
        </div>
        <p className="provider-desc">
          Pin a publisher's public key and this server will install only plugins
          signed by a publisher you have pinned, refusing anything else by name.
          Pin nothing and nothing is checked. There is no registry behind this and
          no keys are shipped: a key is trusted because you put it here.
        </p>

        {publishers && publishers.publishers.length === 0 && (
          <p className="admin-section-note" data-testid="plugin-publishers-empty">
            No keys are pinned, so plugin signatures are not checked. Any plugin
            you install will be installed.
          </p>
        )}

        {publishers?.publishers.map((p) => (
          <dl
            className="plugin-facts"
            key={p.publisher}
            data-testid={`publisher-${p.publisher}`}
          >
            <div>
              <dt>{p.publisher}</dt>
              <dd data-testid={`publisher-key-${p.publisher}`}>
                {p.keyId ? `key ${p.keyId} — ` : ""}
                {p.publicKey}
              </dd>
            </div>
            <div>
              <dt />
              <dd>
                <button
                  className="btn btn-danger"
                  type="button"
                  data-testid={`publisher-unpin-${p.publisher}`}
                  onClick={() =>
                    void runPublishers(
                      () => apiClient.unpinPluginPublisher(p.publisher),
                      `Unpinned ${p.publisher}.`,
                    )
                  }
                  disabled={busy}
                >
                  Unpin
                </button>
              </dd>
            </div>
          </dl>
        ))}

        <div className="field">
          <label className="field-label" htmlFor="plugin-publisher-name">
            Publisher
          </label>
          <input
            id="plugin-publisher-name"
            className="field-input"
            data-testid="plugin-publisher-name"
            placeholder="Example Publisher"
            value={publisherName}
            onChange={(e) => setPublisherName(e.target.value)}
            disabled={busy}
          />
        </div>
        <div className="field">
          <label className="field-label" htmlFor="plugin-publisher-key">
            Public key
          </label>
          <input
            id="plugin-publisher-key"
            className="field-input"
            data-testid="plugin-publisher-key"
            placeholder="base64 ed25519 public key"
            value={publisherKey}
            onChange={(e) => setPublisherKey(e.target.value)}
            disabled={busy}
          />
        </div>
        <div className="admin-actions">
          <button
            className="btn btn-primary"
            type="button"
            data-testid="plugin-publisher-pin"
            onClick={() => void onPinPublisher()}
            disabled={busy}
          >
            {busy ? "Working…" : "Pin"}
          </button>
        </div>
      </div>
    </div>
  );
}
