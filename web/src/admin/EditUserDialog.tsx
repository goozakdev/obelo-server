import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { formatDateTime } from "../time";
import QrSvg from "../lib/QrSvg";
import { tailnetAddress } from "./AdminRemoteAccessScreen";
import LinkedMark from "../browse/LinkedMark";
import type { LinkInvite, Library, User, UserDetail } from "../api/types";

// The Edit-User dialog: everything an Admin can change about an existing User,
// in one modal (same chrome as the Library dialogs) —
//   - a password reset (available for ANY User EXCEPT a linked Server, which has
//     no password at all — its only credential is the token an Invite leaves
//     behind, ADR-0054),
//   - for a linked Server, a Link section that mints that Invite,
//   - the Library access checklist, the Rating ceiling and the Playback ceiling
//     for any NON-ADMIN role (a Member, and equally the `remote` role a linked
//     Server holds — ADR-0054 §2 makes the ceiling general because the mechanism
//     is: capping a kid's iPad at 720p is the same code path as capping a
//     friend's Server) — with one difference for the `remote` role: the
//     checklist offers this Server's OWN Libraries only, because a Library that
//     arrived over a Link is never re-shared onward (ADR-0054 §4; the server
//     refuses such a set with 422 LINKED_GRANT),
//   - and a "Delete user" button in the footer, alongside the row's trash icon.
//
// An ADMIN is implicitly all-access and uncapped, so their body carries the
// password field and a plain statement of that — no grant or ceiling control at
// all (the server rejects both with 422 ADMIN_GRANT / ADMIN_CEILING).
//
// ONE SAVE, not four. The dialog collects every edit and "Save changes" applies
// only the dirty ones, in order: password → library access → rating ceiling →
// playback ceiling. Each
// leg is an idempotent PUT (the access call is a REPLACE-set of the full ticked
// list, not a delta), so if a later leg fails the earlier ones simply stand and
// pressing Save again re-applies the whole set safely. A refused save is NOT
// swallowed — it renders inline (ADMIN_GRANT / UNKNOWN_LIBRARY / ADMIN_CEILING /
// UNKNOWN_RATING all arrive as readable ApiErrors) and the dialog stays open with
// the edits intact. Only a clean save closes it.
//
// Delete does not happen here: the button reports up to AdminUsersScreen, which
// owns the one confirmation dialog shared with the row's trash icon.
//
// THE LINK SECTION (ADR-0055 §2) is deliberately NOT part of that one save.
// Minting an invite is not an edit to the User — nothing about the User changes
// — it is an irreversible act with a side effect on the outside world: the
// previous invite dies the moment a new one is born, and the raw code exists
// only in the response, so a mint that happens as a side effect of pressing
// "Save changes" would be a mis-click that silently strands a friend mid-link.
// It has its own button, its own error, and its own result panel.
//
// The origins in the invite are TYPED BY THE ADMIN, because the Server does not
// know its own public address and deliberately never emits one (ADR-0005's
// retired External URL). The one address it does know is its MagicDNS name, so
// that row is pre-filled from `GET /settings/tailscale` — with the scheme the
// node ACHIEVED, never the one the operator asked for, which is why this reuses
// AdminRemoteAccessScreen's `tailnetAddress` rather than restating the rule (the
// restatement is exactly the bug ADR-0043's panel records).

/** The Rating-ceiling option set (PRD "Rating-ceiling option set"): the MPAA
 * rungs. The dropdown also offers "No limit" (the empty value → `null`, uncapped).
 * One ladder suffices — the server's single maturity rank caps the TV system too,
 * so we deliberately do NOT model a separate TV taxonomy on the client. */
const RATING_RUNGS = ["G", "PG", "PG-13", "R", "NC-17"] as const;

/** The Playback-ceiling resolution rungs (ADR-0054 §2) — the exact set the server
 * accepts on `PUT /users/{id}/playbackCeiling`; anything else is 422
 * UNKNOWN_RESOLUTION. The dropdown also offers "No limit" (the empty value). */
const RESOLUTION_RUNGS = ["720p", "1080p", "2160p"] as const;

/** Bits/sec ⇄ Mbps for the bitrate field. The server speaks bits/sec (the unit the
 * negotiator's Constraints use); an operator thinks in Mbps, so the input holds
 * Mbps and converts at the edges. Blank/0/nonsense means "no limit" in both
 * directions, so a cleared field clears the cap rather than capping at zero. */
function mbpsToBits(text: string): number {
  const n = Number.parseFloat(text);
  return Number.isFinite(n) && n > 0 ? Math.round(n * 1_000_000) : 0;
}
function bitsToMbps(bits: number): string {
  return bits > 0 ? String(bits / 1_000_000) : "";
}

/** The stream cap as a whole number; blank/0/negative means "no limit". */
function toStreams(text: string): number {
  const n = Number.parseInt(text, 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

/** Same members, order-insensitive — used to tell an untouched checklist from an
 * edited one so a no-op save sends nothing. */
function sameSet(a: Set<string>, b: string[]): boolean {
  return a.size === b.length && b.every((id) => a.has(id));
}

export default function EditUserDialog({
  user,
  onClose,
  onRequestDelete,
}: {
  user: User;
  /** Close the dialog (ESC, backdrop, ✕, Cancel, or a clean save). */
  onClose: () => void;
  /** Hand the User to the hub's delete confirmation. */
  onRequestDelete: (user: User) => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const isAdmin = user.role === "admin";
  /** A linked Server (ADR-0054): no password, and the one role with a Link. */
  const isRemote = user.role === "remote";

  const [detail, setDetail] = useState<UserDetail | null>(null);
  const [libraries, setLibraries] = useState<Library[] | null>(null);
  const [loading, setLoading] = useState(!isAdmin);
  const [loadError, setLoadError] = useState<string | null>(null);

  // The edits in progress.
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [ceiling, setCeiling] = useState("");
  const [password, setPassword] = useState("");
  // The Playback ceiling's three knobs, held as the strings their inputs carry.
  const [maxResolution, setMaxResolution] = useState("");
  const [maxBitrate, setMaxBitrate] = useState("");
  const [maxStreams, setMaxStreams] = useState("");

  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // Bumped by Retry to re-run the load effect after a failed fetch.
  const [reloadKey, setReloadKey] = useState(0);

  // --- The Link section (remote only) --------------------------------------
  // One row per origin, in the order the redeeming Server will try them
  // (ADR-0055 §2 — the order is meaningful, so the rows are a list and not a
  // set). It starts as a single blank free-text row; the MagicDNS origin is
  // pushed in front of it once the Tailnet answers.
  const [origins, setOrigins] = useState<string[]>([""]);
  const [invite, setInvite] = useState<LinkInvite | null>(null);
  const [minting, setMinting] = useState(false);
  const [mintError, setMintError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  // An Admin has neither grants nor a ceiling to edit, so their dialog fetches
  // nothing — the password field is all it needs.
  useEffect(() => {
    if (isAdmin) return;
    let cancelled = false;
    void (async () => {
      setLoading(true);
      setLoadError(null);
      try {
        const [d, libs] = await Promise.all([
          apiClient.getUser(user.id),
          apiClient.listLibraries(),
        ]);
        if (cancelled) return;
        setDetail(d);
        setLibraries(libs);
        // For a linked Server, a grant naming a Library that itself arrived over
        // a Link cannot exist (the server refuses it with 422 LINKED_GRANT and
        // never resolves one into the Scope). One CAN still be read back from a
        // database written before that rule, so it is dropped here too — leaving
        // it ticked would show a box that cannot be untied to any visible row and
        // would make Save fail on a set the Admin never chose.
        setChecked(
          new Set(
            isRemote
              ? d.libraryIds.filter((id) =>
                  libs.some((l) => l.id === id && l.linked !== true),
                )
              : d.libraryIds,
          ),
        );
        setCeiling(d.ratingCeiling);
        setMaxResolution(d.maxResolution);
        setMaxBitrate(bitsToMbps(d.maxBitrate));
        setMaxStreams(d.maxStreams > 0 ? String(d.maxStreams) : "");
      } catch (err) {
        if (!cancelled) setLoadError(errorMessage(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [isAdmin, isRemote, user.id, reloadKey]);

  // Pre-fill the MagicDNS origin. Best-effort in every direction: a build with
  // no Tailnet support answers 503, a node that is not running has no `fqdn`,
  // and either way the Admin simply gets the blank free-text row. A failure here
  // is NOT surfaced — nothing is broken, there is just one fewer address to
  // offer, and an error beside a field the operator did not ask for would send
  // them to debug remote access instead of sending their invite.
  useEffect(() => {
    if (!isRemote) return;
    let cancelled = false;
    void (async () => {
      try {
        const view = await apiClient.getTailnet();
        const address = tailnetAddress(view);
        if (cancelled || !address) return;
        setOrigins((prev) =>
          prev.some((o) => o.trim() !== "") ? prev : [address, ""],
        );
      } catch {
        // No tailnet, no pre-fill.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [isRemote]);

  /** The origins as they will be sent: trimmed, blanks dropped, order kept. */
  const filledOrigins = origins.map((o) => o.trim()).filter((o) => o !== "");

  async function onGenerateInvite() {
    if (minting || filledOrigins.length === 0) return;
    setMinting(true);
    setMintError(null);
    setCopied(false);
    try {
      const minted = await apiClient.createLinkInvite(user.id, filledOrigins);
      setInvite(minted);
    } catch (err) {
      // 422 INVALID_ORIGIN / NOT_REMOTE_USER arrive as readable ApiErrors. The
      // previously shown invite (if any) stays on screen: a refused mint does
      // not spend the live one.
      setMintError(errorMessage(err));
    } finally {
      setMinting(false);
    }
  }

  async function onCopyInvite() {
    if (!invite) return;
    try {
      await navigator.clipboard?.writeText(invite.invite);
      setCopied(true);
    } catch {
      // A browser that refuses clipboard access is not an error worth a panel —
      // the string is right there in a selectable field.
    }
  }

  function toggleChecked(libraryId: string) {
    setChecked((prev) => {
      const next = new Set(prev);
      if (next.has(libraryId)) next.delete(libraryId);
      else next.add(libraryId);
      return next;
    });
  }

  // What this User may be granted. A linked Server is never granted a Library
  // that itself arrived over a Link (ADR-0054 §4): the owner of those files
  // decided who sees them, and one hop later that decision would be made by
  // somebody they never met. The server refuses such a set with 422
  // LINKED_GRANT, so the checklist does not offer the row at all — an
  // unavoidable error is not a choice. For every other role a mirror is an
  // ordinary Library (ADR-0056 §2), which is what the household's own people
  // are granted.
  const grantableLibraries =
    libraries === null
      ? null
      : isRemote
        ? libraries.filter((lib) => lib.linked !== true)
        : libraries;
  /** True when rows were withheld above — the note explains the absence. */
  const hasLinkedLibraries =
    isRemote && (libraries?.some((lib) => lib.linked === true) ?? false);

  const accessDirty = detail !== null && !sameSet(checked, detail.libraryIds);
  const ceilingDirty = detail !== null && ceiling !== detail.ratingCeiling;
  // Compared as the NUMBERS that will be sent, not as the strings typed: "8.0"
  // and "8" are the same 8 Mbps, and a blank field is the same 0 as a "0".
  const playbackDirty =
    detail !== null &&
    (maxResolution !== detail.maxResolution ||
      mbpsToBits(maxBitrate) !== detail.maxBitrate ||
      toStreams(maxStreams) !== detail.maxStreams);
  const dirty = password.length > 0 || accessDirty || ceilingDirty || playbackDirty;

  async function onSave() {
    if (saving || !dirty) return;
    setSaving(true);
    setSaveError(null);
    try {
      if (password.length > 0) {
        await apiClient.setPassword(user.id, password);
      }
      if (accessDirty) {
        // The FULL desired set (replace-set). An empty array = "sees no catalog".
        await apiClient.setLibraryAccess(user.id, [...checked]);
      }
      if (ceilingDirty) {
        // The empty option ("No limit") clears the ceiling — send `null`, not "".
        await apiClient.setRatingCeiling(user.id, ceiling === "" ? null : ceiling);
      }
      if (playbackDirty) {
        // The WHOLE ceiling every time (a replace, like the grant set): an emptied
        // field sends "" / 0 and clears that cap.
        await apiClient.setPlaybackCeiling(user.id, {
          maxResolution,
          maxBitrate: mbpsToBits(maxBitrate),
          maxStreams: toStreams(maxStreams),
        });
      }
      onClose();
    } catch (err) {
      // Refused — surface it and keep the dialog (and the edits) put.
      setSaveError(errorMessage(err));
      setSaving(false);
    }
  }

  // Closing mid-mint would throw away a string that only exists in that one
  // response — and the invite it replaced is already dead server-side — so the
  // dialog is held shut for the same reasons a save holds it shut.
  const busy = saving || minting;

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="edit-user-dialog"
      onCancel={(e) => {
        e.preventDefault();
        if (!busy) onClose();
      }}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current && !busy) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">
            Edit user
            <span className="library-dialog-kind" data-testid="edit-user-username">
              {user.username}
            </span>
          </h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid="edit-user-close-x"
            aria-label="Close"
            onClick={onClose}
            disabled={busy}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          {isRemote ? (
            // A linked Server has no password to reset — the role carries none,
            // and the schema refuses to give it one. Saying so is better than an
            // absence, because the field is where an operator would look.
            <p className="field-hint" data-testid="remote-no-password">
              A linked server has no password. Its only credential is the invite
              below, which it redeems once for a token of its own.
            </p>
          ) : (
            <div className="field">
              <label className="field-label" htmlFor="edit-user-password">
                New password
              </label>
              <input
                id="edit-user-password"
                className="field-input"
                data-testid="new-password-input"
                type="password"
                value={password}
                placeholder="Leave blank to keep the current password"
                autoComplete="new-password"
                onChange={(e) => setPassword(e.target.value)}
                disabled={saving}
              />
            </div>
          )}

          {isRemote && (
            <div className="field" data-testid="link-section">
              <span className="field-label">Link</span>
              <p className="field-hint">
                The addresses this server can be reached at, tried in this order
                by the server that redeems the invite. This server cannot know
                its own public address, so type it if you have one.
              </p>
              <ul className="origin-list" data-testid="origin-list">
                {origins.map((origin, i) => (
                  <li key={i} className="origin-row">
                    <input
                      className="field-input"
                      data-testid={`origin-input-${i}`}
                      type="url"
                      inputMode="url"
                      value={origin}
                      placeholder="https://media.example.org"
                      aria-label={`Address ${i + 1}`}
                      onChange={(e) =>
                        setOrigins((prev) =>
                          prev.map((o, j) => (j === i ? e.target.value : o)),
                        )
                      }
                      disabled={minting}
                    />
                    {origins.length > 1 && (
                      <button
                        className="nav-link"
                        type="button"
                        data-testid={`origin-remove-${i}`}
                        aria-label={`Remove address ${i + 1}`}
                        onClick={() =>
                          setOrigins((prev) => prev.filter((_, j) => j !== i))
                        }
                        disabled={minting}
                      >
                        ✕
                      </button>
                    )}
                  </li>
                ))}
              </ul>
              <button
                className="nav-link"
                type="button"
                data-testid="origin-add"
                onClick={() => setOrigins((prev) => [...prev, ""])}
                disabled={minting}
              >
                Add another address
              </button>

              <button
                className="auth-submit"
                type="button"
                data-testid="generate-invite"
                onClick={onGenerateInvite}
                disabled={minting || filledOrigins.length === 0}
              >
                {minting
                  ? "Generating…"
                  : invite
                    ? "Generate a new invite"
                    : "Generate invite"}
              </button>

              {mintError && (
                <p className="auth-error" data-testid="invite-error" role="alert">
                  {mintError}
                </p>
              )}

              {invite && (
                <div className="invite-result" data-testid="invite-result">
                  <label className="field-label" htmlFor="edit-user-invite">
                    Send this to the other household
                  </label>
                  <input
                    id="edit-user-invite"
                    className="field-input invite-string"
                    data-testid="invite-string"
                    type="text"
                    readOnly
                    value={invite.invite}
                    onFocus={(e) => e.currentTarget.select()}
                  />
                  <button
                    className="button-secondary"
                    type="button"
                    data-testid="invite-copy"
                    onClick={onCopyInvite}
                  >
                    {copied ? "Copied" : "Copy"}
                  </button>
                  <QrSvg
                    className="invite-qr"
                    testId="invite-qr"
                    text={invite.invite}
                    label="QR code of the invite string"
                  />
                  <p className="field-hint" data-testid="invite-expiry">
                    Expires {formatDateTime(invite.expiresAt)}.
                  </p>
                  <p className="tailnet-warning" data-testid="invite-warning">
                    Anyone with this string can link once, within 24 hours.
                    Generating another one replaces it.
                  </p>
                </div>
              )}
            </div>
          )}

          {isAdmin ? (
            <p className="field-hint" data-testid="admin-all-libraries">
              Admins see all libraries <span data-testid="admin-no-cap">
                and have no rating ceiling
              </span>
              , so there is nothing to grant or cap here.
            </p>
          ) : (
            <>
              {loading && (
                <p
                  className="status status-loading"
                  data-testid="library-access-loading"
                >
                  Loading libraries&hellip;
                </p>
              )}

              {loadError && (
                <p
                  className="status status-error"
                  data-testid="library-access-load-error"
                  role="alert"
                >
                  <span className="dot dot-error" aria-hidden="true" />
                  {loadError}{" "}
                  <button
                    className="nav-link"
                    type="button"
                    data-testid="library-access-retry"
                    onClick={() => setReloadKey((n) => n + 1)}
                  >
                    Retry
                  </button>
                </p>
              )}

              {!loading && !loadError && grantableLibraries && (
                <div className="field">
                  <span className="field-label">Library access</span>
                  {grantableLibraries.length === 0 ? (
                    <p
                      className="status status-empty"
                      data-testid="library-access-empty"
                    >
                      {hasLinkedLibraries
                        ? "No libraries of this server's own yet."
                        : "No libraries on this server yet."}
                    </p>
                  ) : (
                    <ul className="library-checklist" data-testid="library-checklist">
                      {grantableLibraries.map((lib) => (
                        <li key={lib.id} className="library-checklist-item">
                          <label>
                            <input
                              type="checkbox"
                              data-testid={`library-checkbox-${lib.id}`}
                              checked={checked.has(lib.id)}
                              onChange={() => toggleChecked(lib.id)}
                              disabled={saving}
                            />{" "}
                            {lib.name}
                            {/* A linked shelf is grantable to a person, but the
                                Admin should see whose it is before sharing it on
                                (issue 18). The mark never shows for a `remote`
                                target — those rows are filtered out above. */}
                            <LinkedMark entity={lib} />
                          </label>
                        </li>
                      ))}
                    </ul>
                  )}
                  {hasLinkedLibraries && (
                    <p className="field-hint" data-testid="linked-not-grantable">
                      Libraries provided by another server can&rsquo;t be shared
                      onward.
                    </p>
                  )}
                  {checked.size === 0 && grantableLibraries.length > 0 && (
                    <p className="field-hint" data-testid="no-libraries-hint">
                      No libraries ticked — this user sees no catalog.
                    </p>
                  )}
                </div>
              )}

              {!loading && !loadError && detail && (
                <div className="field">
                  <label className="field-label" htmlFor="edit-user-ceiling">
                    Rating ceiling
                  </label>
                  <select
                    id="edit-user-ceiling"
                    className="field-input"
                    data-testid="rating-ceiling-select"
                    value={ceiling}
                    onChange={(e) => setCeiling(e.target.value)}
                    disabled={saving}
                  >
                    <option value="">No limit</option>
                    {RATING_RUNGS.map((rung) => (
                      <option key={rung} value={rung}>
                        {rung}
                      </option>
                    ))}
                  </select>
                </div>
              )}

              {!loading && !loadError && detail && (
                <div className="field" data-testid="playback-ceiling">
                  <span className="field-label">Playback ceiling</span>
                  <p className="field-hint">
                    How this user&rsquo;s streams may play. A capped title still
                    appears &mdash; it plays at the cap.
                  </p>
                  <label className="field-label" htmlFor="edit-user-max-resolution">
                    Max resolution
                  </label>
                  <select
                    id="edit-user-max-resolution"
                    className="field-input"
                    data-testid="max-resolution-select"
                    value={maxResolution}
                    onChange={(e) => setMaxResolution(e.target.value)}
                    disabled={saving}
                  >
                    <option value="">No limit</option>
                    {RESOLUTION_RUNGS.map((rung) => (
                      <option key={rung} value={rung}>
                        {rung}
                      </option>
                    ))}
                  </select>
                  <label className="field-label" htmlFor="edit-user-max-bitrate">
                    Max bitrate (Mbps)
                  </label>
                  <input
                    id="edit-user-max-bitrate"
                    className="field-input"
                    data-testid="max-bitrate-input"
                    type="number"
                    min="0"
                    step="0.5"
                    value={maxBitrate}
                    placeholder="No limit"
                    onChange={(e) => setMaxBitrate(e.target.value)}
                    disabled={saving}
                  />
                  <label className="field-label" htmlFor="edit-user-max-streams">
                    Max concurrent streams
                  </label>
                  <input
                    id="edit-user-max-streams"
                    className="field-input"
                    data-testid="max-streams-input"
                    type="number"
                    min="0"
                    step="1"
                    value={maxStreams}
                    placeholder="No limit"
                    onChange={(e) => setMaxStreams(e.target.value)}
                    disabled={saving}
                  />
                </div>
              )}
            </>
          )}

          {saveError && (
            <p className="auth-error" data-testid="edit-user-error" role="alert">
              {saveError}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer">
          <button
            className="button-danger"
            type="button"
            data-testid="edit-user-delete"
            onClick={() => onRequestDelete(user)}
            disabled={busy}
          >
            Delete user
          </button>
          <div className="library-dialog-footer-actions">
            <button
              className="button-secondary"
              type="button"
              data-testid="edit-user-cancel"
              onClick={onClose}
              disabled={busy}
            >
              Cancel
            </button>
            <button
              className="auth-submit"
              type="button"
              data-testid="edit-user-save"
              onClick={onSave}
              disabled={busy || !dirty}
            >
              {saving ? "Saving…" : "Save changes"}
            </button>
          </div>
        </footer>
      </div>
    </dialog>
  );
}
