import { useEffect, useRef } from "react";
import { providesLabel } from "./PluginDialog";
import type { PluginUpgradeStaged } from "../api/types";

// The preview of a staged plugin upgrade (ADR-0069): what the new version asks for
// beyond the installed one, what it loses, and who claims to have written it. Nothing
// has been applied while this is open; Confirm applies exactly the staged package and
// Cancel (also ESC and the backdrop) discards it.
//
// Presentational only, like ConfirmDialog: the caller owns the calls. `gone` means the
// server no longer holds the staged package (expired, cancelled, or no longer
// allowed), so there is nothing left to confirm and the button is not offered.

function List({ testId, label, items }: { testId: string; label: string; items: string[] }) {
  if (items.length === 0) return null;
  return (
    <div className="field">
      <span className="field-label">{label}</span>
      <ul data-testid={testId}>
        {items.map((i) => (
          <li key={i}>{i}</li>
        ))}
      </ul>
    </div>
  );
}

// authorLine is what the package says about who signed it. An unpinned name is the
// package's own claim, so it is worded as one; only a pinned (or Obelo) key makes it fact.
function authorLine(p: PluginUpgradeStaged["preview"]): string {
  const key = p.keyId ? `key id ${p.keyId}` : "";
  if (p.publisher) return `Signed by ${p.publisher}${key ? ` (${key})` : ""}`;
  if (p.claimedPublisher) return `Claims to be ${p.claimedPublisher}${key ? `, ${key}` : ""} (not verified)`;
  if (key) return `Signed with ${key} (not verified)`;
  return "Not signed";
}

export default function PluginUpgradeDialog({
  staged,
  busy,
  gone,
  refused = false,
  error,
  onConfirm,
  onCancel,
}: {
  staged: PluginUpgradeStaged;
  busy: boolean;
  gone: boolean;
  /** Confirm was refused for another reason: still staged, so Cancel discards it. */
  refused?: boolean;
  error: string | null;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const { preview: p } = staged;
  const hostsAdded = p.hostsAdded ?? [];
  const hostsRemoved = p.hostsRemoved ?? [];
  const pointsAdded = (p.extensionPointsAdded ?? []).map((k) => providesLabel([k]));
  const pointsRemoved = (p.extensionPointsRemoved ?? []).map((k) => providesLabel([k]));
  const dropped = p.settingsDropped ?? [];
  const deleted = p.settingsDeleted ?? [];
  const nothingElse =
    !hostsAdded.length && !hostsRemoved.length && !pointsAdded.length && !pointsRemoved.length &&
    !p.socketGrantAdded && !dropped.length && !deleted.length;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog confirm-dialog"
      data-testid="plugin-upgrade-dialog"
      onCancel={(e) => {
        e.preventDefault();
        if (!busy) onCancel();
      }}
      onClose={() => {
        if (busy) {
          const dialog = dialogRef.current;
          if (dialog && !dialog.open) dialog.showModal();
          return;
        }
        onCancel();
      }}
      onClick={(e) => {
        if (e.target === dialogRef.current && !busy) onCancel();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">Upgrade {staged.name || staged.id}</h2>
        </header>

        <div className="library-dialog-body">
          <p data-testid="plugin-upgrade-versions">
            {staged.name || staged.id}: {p.from} to {p.to}. Nothing has been applied yet.
          </p>
          <p data-testid="plugin-upgrade-author">{authorLine(p)}</p>
          {p.authorUnconfirmed && (
            <p className="auth-error" data-testid="plugin-upgrade-unconfirmed" role="alert">
              The author of this upgrade cannot be confirmed. The installed copy has no
              recorded signing key, so anyone could have written this package. Confirm only
              if you trust where it came from.
            </p>
          )}
          <List testId="plugin-upgrade-hosts-added" label="Can now reach these hosts" items={hostsAdded} />
          <List testId="plugin-upgrade-hosts-removed" label="No longer reaches these hosts" items={hostsRemoved} />
          <List testId="plugin-upgrade-points-added" label="Now also provides" items={pointsAdded} />
          <List testId="plugin-upgrade-points-removed" label="No longer provides" items={pointsRemoved} />
          {p.socketGrantAdded && (
            <p className="auth-error" data-testid="plugin-upgrade-socket">
              This version is granted a raw network socket it did not have before.
            </p>
          )}
          <List
            testId="plugin-upgrade-settings-dropped"
            label="Saved settings that will be lost"
            items={dropped.map((d) => `${d.key}: ${d.reason}`)}
          />
          <List testId="plugin-upgrade-settings-deleted" label="Settings that will be deleted" items={deleted} />
          {nothingElse && (
            <p className="provider-desc" data-testid="plugin-upgrade-nothing-else">
              It asks for no new access and loses no saved settings.
            </p>
          )}
          <p className="provider-desc" data-testid="plugin-upgrade-expiry">
            {gone ? "This upgrade is no longer waiting." : `Confirm before ${new Date(staged.expiresAt).toLocaleTimeString()}, or it is discarded.`}
          </p>
          {error && (
            <p className="auth-error" data-testid="plugin-upgrade-error" role="alert">
              {error}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="button-secondary"
            type="button"
            data-testid="plugin-upgrade-cancel"
            onClick={onCancel}
            disabled={busy}
          >
            {gone ? "Close" : "Cancel"}
          </button>
          {!gone && !refused && (
            <button
              className="button-danger"
              type="button"
              data-testid="plugin-upgrade-confirm"
              onClick={onConfirm}
              disabled={busy}
            >
              {busy ? "Upgrading…" : "Confirm upgrade"}
            </button>
          )}
        </footer>
      </div>
    </dialog>
  );
}
