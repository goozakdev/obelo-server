import { useCallback, useEffect, useState } from "react";
import { Link as RouterLink } from "react-router-dom";
import { apiClient } from "../api/client";
import { appEvents } from "../events/enrichEvents";
import { errorMessage } from "../screens/errorMessage";
import { formatAgo, formatDateTime } from "../time";
import type { Link, LinkedLibrary } from "../api/types";
import { linkErrorMessage } from "./linkErrors";
import ConfirmDialog from "./ConfirmDialog";

// The Linked servers admin screen (ADR-0055, ADR-0056, linked-servers issue 10):
// where an Admin pastes the one string a friend sent, watches each Link's
// condition, fixes a revoked one and unlinks. It is the ONLY surface for any of
// that — the acceptance is that the whole PRD walkthrough is reachable from here
// plus the sharer's own Users dialog, with no CLI and no config file.
//
// AdminRemoteAccessScreen is the model, for the same reason: both are the face of
// a state machine somebody else drives. A friend's server goes dark at 3am and
// this page has to say so without anybody pressing reload, so it subscribes to
// the admin-only `linkState` nudge — which carries a link id and nothing else,
// because GET /links is the truth — and refetches on every one.
//
// THE THREE STATES ARE THREE DIFFERENT NEXT MOVES, which is the whole reason
// ADR-0056 §6 makes them a closed set rather than a status string:
//
//   connected     nothing to do; say when the mirror last pulled
//   unreachable   their machine is not answering — wait, or press Sync now;
//                 the server's own reason is quoted verbatim
//   revoked       the credential is dead and will NEVER recover on its own;
//                 the only fix is a fresh invite, so the row says exactly that
//
// A revoked Link is deliberately not red-alarm styling: nothing here is broken or
// lost, the libraries and this household's watch state are all still on disk, and
// one paste brings them back. Unlink is the only thing that deletes, which is why
// it is the only action behind a confirmation and why that confirmation names
// what goes.
//
// This is also the ONLY page that lists a linked Library at all (issue 16): the
// Libraries hub shows local shelves and one line pointing here. So the one write
// the server allows on a mirror — a rename, because what this household calls
// somebody else's shelf is this household's business (ADR-0056 §1,
// api-contract §3.3) — is offered here, as an inline field on the library line
// rather than a second Edit dialog: the real one is mostly root folders and the
// Enrichment policy, both of which a mirror refuses. Only `name` is ever sent, so
// the 409 LINKED_LIBRARY that `addRootFolders` would earn cannot fire from here.

/** The chip beside a server's name. Three states, three sentences: the label is
 * what happened, the note is what to do about it.
 *
 * `lastError` is quoted VERBATIM in the unreachable case, on the same rule the
 * Tailnet panel follows — the server's reason names hostnames, ports and TLS
 * failures, and a friendlier paraphrase would drop the only actionable part. */
export function linkStateNote(link: Link): { label: string; note: string } {
  switch (link.state) {
    case "connected":
      return {
        label: "Connected",
        note: "",
      };
    case "unreachable":
      return {
        label: "Unreachable",
        note: link.lastError
          ? `Their server did not answer. ${link.lastError}`
          : "Their server did not answer. The libraries and your watch state are " +
            "untouched; this server keeps trying on its own.",
      };
    case "revoked":
      return {
        label: "Revoked",
        note:
          "That household withdrew this server's access, so nothing new will " +
          "arrive and nothing will play. Ask them for a fresh invite and paste it " +
          "into Re-key — the libraries and your watch state are kept.",
      };
    default:
      // A state this build has never heard of is reported as itself rather than
      // hidden: an unknown chip an operator can read out loud beats a blank row.
      return { label: link.state as string, note: "" };
  }
}

/** "2h ago" / "never". Null is the server saying the mirror has not pulled once,
 * which is a statement about a brand-new Link and not a missing field. */
export function lastSyncedLabel(at: string | null, now: number = Date.now()): string {
  if (!at) return "never";
  return formatAgo(at, now) || "never";
}

export default function AdminLinkedServersScreen() {
  const [links, setLinks] = useState<Link[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const next = await apiClient.listLinks(signal);
      if (signal?.aborted) return;
      setLinks(next);
      setLoadError(null);
    } catch (err) {
      if (signal?.aborted) return;
      if (err instanceof DOMException && err.name === "AbortError") return;
      setLoadError(errorMessage(err));
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  // The live half. `linkState` is a nudge carrying a link id; the GET is the
  // truth, so every one of them is just "look again". This is what turns a
  // friend's server coming back up into a green chip with no reload.
  useEffect(() => {
    return appEvents.subscribe((type) => {
      if (type !== "linkState") return;
      void load();
    });
  }, [load]);

  return (
    <section className="admin-linked-servers" data-testid="admin-linked-servers">
      <h2 className="section-title">Linked servers</h2>

      <LinkAServerPanel onLinked={() => void load()} />

      {loadError && !links && (
        <p className="status status-error" data-testid="links-load-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {loadError}{" "}
          <button
            className="nav-link"
            type="button"
            data-testid="links-retry"
            onClick={() => void load()}
          >
            Retry
          </button>
        </p>
      )}

      {!links && !loadError && (
        <p className="status status-loading" data-testid="links-loading">
          Loading linked servers&hellip;
        </p>
      )}

      {links && links.length === 0 && (
        <p className="status status-empty" data-testid="links-empty">
          No servers linked yet. When a friend sends you an invite string, paste it
          above and their libraries appear in every app here.
        </p>
      )}

      {links && links.length > 0 && (
        <ul className="link-list" data-testid="link-list">
          {links.map((link) => (
            <LinkRow key={link.id} link={link} onChanged={() => void load()} />
          ))}
        </ul>
      )}
    </section>
  );
}

// --- Link a server ----------------------------------------------------------

/** The paste box. ONE textarea and nothing else, because the invite is one string
 * (ADR-0055 §2): a hostname field and a code field would be two fields, two
 * mistakes and no room for the second address. A phone camera's decoded QR pastes
 * straight in, which is the entirety of the "scan it" story on the web. */
function LinkAServerPanel({ onLinked }: { onLinked: () => void }) {
  const [invite, setInvite] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Link | null>(null);

  async function onSubmit() {
    const trimmed = invite.trim();
    if (!trimmed || busy) return;
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const link = await apiClient.createLink(trimmed);
      setResult(link);
      setInvite("");
      onLinked();
    } catch (err) {
      setError(linkErrorMessage(err, trimmed));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="link-paste card" data-testid="link-a-server">
      <h3 className="card-title">Link a server</h3>
      <p className="tailnet-body">
        Paste the invite your friend sent you. It is one string beginning{" "}
        <code>obelo-link:</code> — from a message, or from your phone&rsquo;s camera
        after scanning their QR code.
      </p>

      <div className="field">
        <label className="field-label" htmlFor="link-invite-input">
          Invite string
        </label>
        <textarea
          id="link-invite-input"
          className="field-input link-invite-input"
          data-testid="link-invite-input"
          rows={3}
          value={invite}
          placeholder="obelo-link:…"
          onChange={(e) => {
            setInvite(e.target.value);
            setError(null);
          }}
          disabled={busy}
        />
      </div>

      <div className="link-paste-bar">
        <button
          className="auth-submit"
          type="button"
          data-testid="link-submit"
          onClick={() => void onSubmit()}
          disabled={busy || invite.trim() === ""}
        >
          {busy ? "Linking…" : "Link server"}
        </button>
      </div>

      {error && (
        <p className="status status-error" data-testid="link-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}

      {result && <LinkResult link={result} />}
    </div>
  );
}

/** What came back: who was linked, and WHAT ARRIVED. The library list is the
 * point — "linked successfully" is not an answer to "did I get the cartoons?" —
 * and an empty one is reported as its own outcome, because a sharer who granted
 * this server nothing is a real and confusing situation that the page must name
 * rather than render as a blank space. */
function LinkResult({ link }: { link: Link }) {
  return (
    <div className="link-result" data-testid="link-result" role="status">
      <p className="link-result-headline">
        Linked to <strong data-testid="link-result-server">{link.serverName}</strong>.
      </p>
      {link.libraries.length > 0 ? (
        <>
          <p className="tailnet-hint">
            {link.libraries.length}{" "}
            {link.libraries.length === 1 ? "library" : "libraries"} received:
          </p>
          <ul className="link-library-list" data-testid="link-result-libraries">
            {link.libraries.map((lib) => (
              <li key={lib.id} className="link-library">
                <span className="link-library-name">{lib.name}</span>
                <span className="link-library-kind">{lib.kind}</span>
              </li>
            ))}
          </ul>
          <p className="tailnet-hint">
            Nobody can see them yet. Grant them to your users on the{" "}
            <RouterLink to="/admin/users" data-testid="link-result-grant">
              Users
            </RouterLink>{" "}
            tab.
          </p>
        </>
      ) : (
        <p className="tailnet-hint" data-testid="link-result-no-libraries">
          They have not shared any libraries with this server yet. Ask them to grant
          some to the remote user they made for you — nothing needs re-linking when
          they do.
        </p>
      )}
    </div>
  );
}

// --- One Link ---------------------------------------------------------------

function LinkRow({ link, onChanged }: { link: Link; onChanged: () => void }) {
  const [busy, setBusy] = useState<null | "sync" | "unlink">(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [rekeying, setRekeying] = useState(false);
  const [rekeyInvite, setRekeyInvite] = useState("");
  const [rekeyBusy, setRekeyBusy] = useState(false);
  const [rekeyError, setRekeyError] = useState<string | null>(null);
  const [confirmUnlink, setConfirmUnlink] = useState(false);

  const { label, note } = linkStateNote(link);

  async function onSync() {
    if (busy) return;
    setBusy("sync");
    setActionError(null);
    try {
      await apiClient.syncLink(link.id);
    } catch (err) {
      // A failed sweep is reported as the failure it was; the row's state has
      // been recorded server-side either way, so the refetch below is still
      // correct and the operator gets the reason rather than a silent no-op.
      setActionError(linkErrorMessage(err));
    } finally {
      setBusy(null);
      onChanged();
    }
  }

  async function onRekey() {
    const trimmed = rekeyInvite.trim();
    if (!trimmed || rekeyBusy) return;
    setRekeyBusy(true);
    setRekeyError(null);
    try {
      await apiClient.rekeyLink(link.id, trimmed);
      setRekeying(false);
      setRekeyInvite("");
      onChanged();
    } catch (err) {
      setRekeyError(linkErrorMessage(err, trimmed));
    } finally {
      setRekeyBusy(false);
    }
  }

  async function onUnlink() {
    if (busy) return;
    setBusy("unlink");
    setActionError(null);
    try {
      await apiClient.deleteLink(link.id);
      setConfirmUnlink(false);
      onChanged();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <li
      className="link-row card"
      data-testid="link-row"
      data-link-id={link.id}
      data-state={link.state}
    >
      <div className="link-row-head">
        <span className="link-server-name" data-testid="link-server-name">
          {link.serverName || link.serverId}
        </span>
        <span
          className={`link-state-chip is-${link.state}`}
          data-testid="link-state-chip"
          data-state={link.state}
        >
          {label}
        </span>
      </div>

      {note && (
        <p className="link-state-note" data-testid="link-state-note">
          {note}
        </p>
      )}

      <dl className="link-facts">
        <div className="link-fact">
          <dt>Reached at</dt>
          <dd data-testid="link-active-origin">
            {link.activeOrigin || "—"}
            {link.origins.length > 1 && (
              <span className="tailnet-hint" data-testid="link-origins">
                {" "}
                (of {link.origins.join(", ")})
              </span>
            )}
          </dd>
        </div>
        <div className="link-fact">
          <dt>Last sync</dt>
          <dd data-testid="link-last-synced" title={formatDateTime(link.lastSyncedAt)}>
            {lastSyncedLabel(link.lastSyncedAt)}
          </dd>
        </div>
      </dl>

      <div className="link-libraries">
        <span className="tailnet-label">Libraries provided</span>
        {link.libraries.length === 0 ? (
          <p className="tailnet-hint" data-testid="link-no-libraries">
            None yet.
          </p>
        ) : (
          <ul className="link-library-list" data-testid="link-libraries">
            {link.libraries.map((lib) => (
              <LinkLibraryItem key={lib.id} library={lib} onRenamed={onChanged} />
            ))}
          </ul>
        )}
      </div>

      <div className="link-actions" data-testid="link-actions">
        <button
          className="button-secondary"
          type="button"
          data-testid="link-sync"
          onClick={() => void onSync()}
          disabled={busy !== null}
        >
          {busy === "sync" ? "Syncing…" : "Sync now"}
        </button>
        <button
          className="button-secondary"
          type="button"
          data-testid="link-rekey-toggle"
          onClick={() => {
            setRekeyError(null);
            setRekeying((v) => !v);
          }}
          disabled={busy !== null}
        >
          {rekeying ? "Cancel re-key" : "Re-key"}
        </button>
        <button
          className="button-danger"
          type="button"
          data-testid="link-unlink"
          onClick={() => {
            setActionError(null);
            setConfirmUnlink(true);
          }}
          disabled={busy !== null}
        >
          Unlink
        </button>
      </div>

      {rekeying && (
        <div className="link-rekey" data-testid="link-rekey">
          <label className="field-label" htmlFor={`link-rekey-input-${link.id}`}>
            Fresh invite from {link.serverName || "that server"}
          </label>
          <textarea
            id={`link-rekey-input-${link.id}`}
            className="field-input link-invite-input"
            data-testid="link-rekey-input"
            rows={3}
            value={rekeyInvite}
            placeholder="obelo-link:…"
            onChange={(e) => {
              setRekeyInvite(e.target.value);
              setRekeyError(null);
            }}
            disabled={rekeyBusy}
          />
          <span className="field-hint">
            Re-keying keeps this link, its libraries and your household&rsquo;s watch
            state — only the credential is replaced. An invite from a different
            server is refused rather than quietly re-pointing this one.
          </span>
          <button
            className="auth-submit"
            type="button"
            data-testid="link-rekey-submit"
            onClick={() => void onRekey()}
            disabled={rekeyBusy || rekeyInvite.trim() === ""}
          >
            {rekeyBusy ? "Re-keying…" : "Re-key link"}
          </button>
          {rekeyError && (
            <p className="status status-error" data-testid="link-rekey-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {rekeyError}
            </p>
          )}
        </div>
      )}

      {actionError && !confirmUnlink && (
        <p className="status status-error" data-testid="link-action-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {actionError}
        </p>
      )}

      {confirmUnlink && (
        <ConfirmDialog
          title={`Unlink ${link.serverName || "this server"}?`}
          // Unlinking is the ONLY thing that deletes what came over a Link
          // (ADR-0056), so the confirmation says what goes, by name and by count,
          // including the watch state — which nothing else on this page can lose
          // and which no re-link brings back.
          message={
            `This removes ${countPhrase(link.libraries.length)} this server provides` +
            `${link.libraries.length > 0 ? ` (${link.libraries.map((l) => l.name).join(", ")})` : ""}` +
            ", everything in them, and your household's watch state for all of it. " +
            "Re-linking later starts that history over. Nothing on their server is affected."
          }
          confirmLabel="Unlink"
          busyLabel="Unlinking…"
          busy={busy === "unlink"}
          error={actionError}
          onConfirm={() => void onUnlink()}
          onCancel={() => {
            if (busy === null) setConfirmUnlink(false);
          }}
        />
      )}
    </li>
  );
}

/** One linked Library under its Link: the name, the kind, the grant shortcut, and
 * the rename.
 *
 * The rename is inline — a field where the name was, Save and Cancel — rather
 * than the Libraries hub's Edit dialog, which is two tabs of things a mirror
 * refuses. It PATCHes `{ name }` and nothing else, then asks the page to refetch,
 * because GET /links is the truth for this list exactly as it is for the state
 * chip; the new name is this household's own label and shows up wherever the
 * Library does. An empty or unchanged name is not a save, so a stray Enter is a
 * no-op rather than a 400. */
function LinkLibraryItem({
  library,
  onRenamed,
}: {
  library: LinkedLibrary;
  /** Called after a successful rename; the page reloads its links. */
  onRenamed: () => void;
}) {
  const [renaming, setRenaming] = useState(false);
  const [name, setName] = useState(library.name);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const trimmed = name.trim();
  const dirty = trimmed !== "" && trimmed !== library.name;

  async function onSave() {
    if (busy || !dirty) return;
    setBusy(true);
    setError(null);
    try {
      await apiClient.updateLibrary(library.id, { name: trimmed });
      setRenaming(false);
      onRenamed();
    } catch (err) {
      // Keep the field open with what was typed: a refused rename is worth
      // another try, and retyping it would be the second insult.
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="link-library" data-library-id={library.id}>
      {renaming ? (
        <div className="link-library-rename" data-testid="link-library-rename">
          <label className="field-label" htmlFor={`link-library-name-${library.id}`}>
            Name for {library.name}
          </label>
          <div className="link-library-rename-row">
            <input
              id={`link-library-name-${library.id}`}
              className="field-input"
              data-testid="link-library-rename-input"
              type="text"
              value={name}
              onChange={(e) => {
                setName(e.target.value);
                setError(null);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" && dirty) void onSave();
                if (e.key === "Escape") {
                  setRenaming(false);
                  setName(library.name);
                  setError(null);
                }
              }}
              disabled={busy}
            />
            <button
              className="nav-link"
              type="button"
              data-testid="link-library-rename-save"
              onClick={() => void onSave()}
              disabled={busy || !dirty}
            >
              {busy ? "Saving…" : "Save"}
            </button>
            <button
              className="nav-link"
              type="button"
              data-testid="link-library-rename-cancel"
              onClick={() => {
                setRenaming(false);
                setName(library.name);
                setError(null);
              }}
              disabled={busy}
            >
              Cancel
            </button>
          </div>
          <span className="field-hint">
            This is only what this household calls the shelf. Nothing on their
            server changes, and the next sync keeps the name you chose.
          </span>
          {error && (
            <p
              className="status status-error"
              data-testid="link-library-rename-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {error}
            </p>
          )}
        </div>
      ) : (
        <>
          <span className="link-library-name">{library.name}</span>
          <span className="link-library-kind">{library.kind}</span>
          <button
            className="nav-link"
            type="button"
            data-testid="link-library-rename-toggle"
            onClick={() => {
              setName(library.name);
              setError(null);
              setRenaming(true);
            }}
          >
            Rename…
          </button>
          {/* The grant itself is per-User and already has a dialog; this is
              a shortcut into it, never a second grant UI. */}
          <RouterLink
            className="nav-link"
            to="/admin/users"
            data-testid="link-grant-users"
          >
            Grant users…
          </RouterLink>
        </>
      )}
    </li>
  );
}

function countPhrase(n: number): string {
  if (n === 0) return "any libraries";
  return `the ${n} ${n === 1 ? "library" : "libraries"}`;
}
