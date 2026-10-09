import { useEffect, useRef } from "react";
import PluginSettingsForm from "./PluginSettingsForm";
import type { InstalledPlugin, InstalledPluginsView, PluginUninstallPreview } from "../api/types";

// PluginDialog is where a single plugin's details and actions live now that the
// Installed tab shows a compact list (plugins-list-dialog D1). It holds
// everything the old per-plugin card used to: provides, the facts <dl>,
// lastError, the plugin's own settings form, and Enable/Disable/Re-enable/
// Uninstall (D3) — with the Uninstall confirmation for a Sign-in provider inline
// (D4). It is a native modal <dialog>, same pattern as EditLibraryDialog: ESC,
// backdrop click and the ✕ all close it the same way.
//
// The caller looks the plugin up by id from its current view on every render
// (D-per-brief) rather than handing this component a copy, so an action's result
// — Enable, Disable, Re-enable, a settings save, or the reinstall of a declined
// plugin — is reflected here as soon as it lands. Uninstalling closes the dialog
// because the plugin it was showing no longer exists.

// EXTENSION_POINT_LABELS maps the contract's Extension-point tokens to what an
// operator calls them. An unknown token is shown verbatim rather than hidden: a
// server one version ahead of this bundle should not make a plugin look like it
// provides nothing.
export const EXTENSION_POINT_LABELS: Record<string, string> = {
  "event-sink": "Event sink",
  "lyric-provider": "Lyric provider",
  "marker-provider": "Marker provider",
  "metadata-provider": "Metadata provider",
  "sign-in-provider": "Sign-in provider",
  "subtitle-provider": "Subtitle provider",
  "web-reference-provider": "Web reference provider",
};

export function providesLabel(provides: string[]): string {
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

// UninstallConfirmation is the step between the Uninstall button and a Sign-in
// provider's uninstall. It says in words that the listed Users are deleted — not
// signed out, deleted, with their watch history — and names each one, because
// the Admin is the last person who can stop it.
function UninstallConfirmation({
  plugin,
  preview,
  busy,
  onConfirm,
  onCancel,
}: {
  plugin: InstalledPlugin;
  preview: PluginUninstallPreview;
  busy: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { id } = plugin;
  const users = preview.usersToDelete;
  return (
    <div className="auth-error" data-testid={`plugin-uninstall-confirmation-${id}`}>
      {users.length > 0 ? (
        <>
          <p>
            Uninstalling {plugin.name} deletes every sign-in it provides. These users
            have no other way to sign in, so they will be deleted, with their watch
            history. This cannot be undone:
          </p>
          <ul>
            {users.map((u) => (
              <li key={u.id} data-testid="plugin-uninstall-user">
                {u.username}
              </li>
            ))}
          </ul>
          <p>Everyone else who signs in through it keeps their account and loses only that sign-in.</p>
        </>
      ) : (
        <p>
          Uninstalling {plugin.name} deletes every sign-in it provides. No user will be
          deleted: everyone who signs in through it has another way to sign in.
        </p>
      )}
      <div className="library-dialog-footer-actions">
        <button
          className="button-secondary"
          type="button"
          data-testid={`plugin-uninstall-cancel-${id}`}
          onClick={onCancel}
          disabled={busy}
        >
          Cancel
        </button>
        <button
          className="button-danger"
          type="button"
          data-testid={`plugin-uninstall-confirm-${id}`}
          onClick={onConfirm}
          disabled={busy}
        >
          {users.length > 0
            ? `Delete ${users.length} ${users.length === 1 ? "user" : "users"} and uninstall`
            : "Uninstall"}
        </button>
      </div>
    </div>
  );
}

export default function PluginDialog({
  plugin,
  busy,
  onEnable,
  onDisable,
  onReenable,
  onReinstallShipped,
  onUninstall,
  onSettingsSaved,
  onUploadNewVersion,
  confirmation,
  onConfirmUninstall,
  onCancelUninstall,
  onClose,
  actionError,
  notice,
}: {
  plugin: InstalledPlugin;
  busy: boolean;
  onEnable: () => void;
  onDisable: () => void;
  onReenable: () => void;
  onReinstallShipped: () => void;
  onUninstall: () => void;
  onSettingsSaved: (view: InstalledPluginsView) => void;
  /** Hand the screen a package to install over this plugin (ADR-0069). */
  onUploadNewVersion?: (pkg: File) => void;
  confirmation?: PluginUninstallPreview | null;
  onConfirmUninstall?: () => void;
  onCancelUninstall?: () => void;
  /** Close without further changes (ESC, backdrop, ✕). */
  onClose: () => void;
  /** The outcome of the last action taken from this dialog, shown here instead
   * of behind it while the dialog is open. */
  actionError?: string | null;
  notice?: string | null;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const packageRef = useRef<HTMLInputElement>(null);
  const { id } = plugin;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  // A DECLINED row is a plugin this server ships and the Admin removed. It has no
  // files, no version and no status, so its dialog gets only the way back —
  // rather than every button disabled, which would read as a broken plugin
  // instead of an absent one.
  if (plugin.state === "declined") {
    return (
      <dialog
        ref={dialogRef}
        className="library-dialog"
        data-testid={`plugin-dialog-${id}`}
        onClose={onClose}
        onClick={(e) => {
          if (e.target === dialogRef.current) onClose();
        }}
      >
        <div className="library-dialog-panel">
          <header className="library-dialog-header">
            <h2 className="library-dialog-title">{plugin.name}</h2>
            <button
              className="nav-link library-dialog-close"
              type="button"
              data-testid={`plugin-dialog-close-x-${id}`}
              aria-label="Close"
              onClick={onClose}
            >
              ✕
            </button>
          </header>
          <div className="library-dialog-body">
            <p className="provider-desc" data-testid={`plugin-declined-note-${id}`}>
              Shipped with Obelo, and you removed it. It will not come back on its own.
            </p>
            {actionError && (
              <p className="auth-error" data-testid="plugins-action-error" role="alert">
                {actionError}
              </p>
            )}
            {notice && (
              <p className="admin-section-note" data-testid="plugins-notice">
                {notice}
              </p>
            )}
          </div>
          <footer className="library-dialog-footer library-dialog-footer-end">
            <button
              className="button-secondary"
              type="button"
              data-testid={`plugin-reinstall-shipped-${id}`}
              onClick={onReinstallShipped}
              disabled={busy}
            >
              Reinstall the shipped version
            </button>
            <button
              className="auth-submit"
              type="button"
              data-testid={`plugin-dialog-close-${id}`}
              onClick={onClose}
            >
              Close
            </button>
          </footer>
        </div>
      </dialog>
    );
  }

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid={`plugin-dialog-${id}`}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">
            {plugin.name}
            {plugin.version && <span className="library-dialog-kind">v{plugin.version}</span>}
          </h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid={`plugin-dialog-close-x-${id}`}
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          <p className="provider-desc" data-testid={`plugin-provides-${id}`}>
            Provides: {providesLabel(plugin.provides)}
          </p>

          {/* Where it came from. A plugin the SERVER shipped says so, in place of
              the upload name or URL an Admin's plugin shows (ADR-0059) — it is
              the one visible difference between the two, and an Admin is
              entitled to know which of their sources they chose and which
              arrived with the server. */}
          {(plugin.origin === "bundled" || plugin.source) && (
            <div className="field">
              <span className="field-label">Installed from</span>
              <span data-testid={`plugin-source-${id}`}>
                {plugin.origin === "bundled" ? "Shipped with Obelo" : sourceLabel(plugin.source)}
              </span>
            </div>
          )}
          {plugin.installedAt && (
            <div className="field">
              <span className="field-label">Installed</span>
              <span data-testid={`plugin-installed-at-${id}`}>{plugin.installedAt}</span>
            </div>
          )}
          {/* Shown only when a signature actually VERIFIED against a pinned
              key. There is deliberately no "Unsigned" row for the plugins
              without one: an empty publisher means nobody checked, which is a
              different claim and not one this server is in a position to
              make. */}
          {plugin.publisher && (
            <div className="field">
              <span className="field-label">Signed by</span>
              <span data-testid={`plugin-publisher-${id}`}>
                {plugin.publisher}
                {plugin.keyId && ` (key ${plugin.keyId})`}
              </span>
            </div>
          )}

          {plugin.lastError && (
            <p className="auth-error" data-testid={`plugin-error-${id}`} role="alert">
              {plugin.lastError}
            </p>
          )}

          {/* The plugin's OWN settings, rendered from the schema its manifest
              declared (plugin-system/13). A plugin that declares none has no
              panel at all rather than an empty one, which is every plugin
              configured entirely through the fixed shape. */}
          <PluginSettingsForm plugin={plugin} disabled={busy} onSaved={onSettingsSaved} />

          {confirmation && (
            <UninstallConfirmation
              plugin={plugin}
              preview={confirmation}
              busy={busy}
              onConfirm={() => onConfirmUninstall?.()}
              onCancel={() => onCancelUninstall?.()}
            />
          )}
          {actionError && (
            <p className="auth-error" data-testid="plugins-action-error" role="alert">
              {actionError}
            </p>
          )}
          {notice && (
            <p className="admin-section-note" data-testid="plugins-notice">
              {notice}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer">
          <button
            className="button-danger"
            type="button"
            data-testid={`plugin-uninstall-${id}`}
            onClick={onUninstall}
            disabled={busy}
          >
            Uninstall
          </button>
          <div className="library-dialog-footer-actions">
            {onUploadNewVersion && (
              <>
                <input
                  ref={packageRef}
                  type="file"
                  accept=".zip,application/zip"
                  hidden
                  data-testid={`plugin-upgrade-file-${id}`}
                  onChange={(e) => {
                    const pkg = e.target.files?.[0];
                    e.target.value = "";
                    if (pkg) onUploadNewVersion(pkg);
                  }}
                />
                <button
                  className="button-secondary"
                  type="button"
                  data-testid={`plugin-upgrade-${id}`}
                  onClick={() => packageRef.current?.click()}
                  disabled={busy}
                >
                  Upload new version
                </button>
              </>
            )}
            {plugin.enabled ? (
              <button
                className="button-secondary"
                type="button"
                data-testid={`plugin-disable-${id}`}
                onClick={onDisable}
                disabled={busy}
              >
                Disable
              </button>
            ) : (
              <button
                className="button-secondary"
                type="button"
                data-testid={`plugin-enable-${id}`}
                onClick={onEnable}
                disabled={busy}
              >
                Enable
              </button>
            )}
            {/* Re-enable is offered only when there is something to forgive. A
                button that does nothing is a button an Admin presses and then
                wonders about. */}
            {(plugin.disabledByFailure || plugin.lastError) && (
              <button
                className="button-secondary"
                type="button"
                data-testid={`plugin-reenable-${id}`}
                onClick={onReenable}
                disabled={busy}
              >
                Re-enable
              </button>
            )}
            <button
              className="auth-submit"
              type="button"
              data-testid={`plugin-dialog-close-${id}`}
              onClick={onClose}
            >
              Close
            </button>
          </div>
        </footer>
      </div>
    </dialog>
  );
}
