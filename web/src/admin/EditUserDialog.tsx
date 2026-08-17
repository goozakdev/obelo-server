import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library, User, UserDetail } from "../api/types";

// The Edit-User dialog: everything an Admin can change about an existing User,
// in one modal (same chrome as the Library dialogs) —
//   - a password reset (available for ANY User, including an Admin: a reset is
//     account recovery, not an access grant),
//   - the Library access checklist and the Rating ceiling for a Member,
//   - and a "Delete user" button in the footer, alongside the row's trash icon.
//
// An ADMIN is implicitly all-access and uncapped, so their body carries the
// password field and a plain statement of that — no grant or ceiling control at
// all (the server rejects both with 422 ADMIN_GRANT / ADMIN_CEILING).
//
// ONE SAVE, not three. The dialog collects every edit and "Save changes" applies
// only the dirty ones, in order: password → library access → rating ceiling. Each
// leg is an idempotent PUT (the access call is a REPLACE-set of the full ticked
// list, not a delta), so if a later leg fails the earlier ones simply stand and
// pressing Save again re-applies the whole set safely. A refused save is NOT
// swallowed — it renders inline (ADMIN_GRANT / UNKNOWN_LIBRARY / ADMIN_CEILING /
// UNKNOWN_RATING all arrive as readable ApiErrors) and the dialog stays open with
// the edits intact. Only a clean save closes it.
//
// Delete does not happen here: the button reports up to AdminUsersScreen, which
// owns the one confirmation dialog shared with the row's trash icon.

/** The Rating-ceiling option set (PRD "Rating-ceiling option set"): the MPAA
 * rungs. The dropdown also offers "No limit" (the empty value → `null`, uncapped).
 * One ladder suffices — the server's single maturity rank caps the TV system too,
 * so we deliberately do NOT model a separate TV taxonomy on the client. */
const RATING_RUNGS = ["G", "PG", "PG-13", "R", "NC-17"] as const;

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

  const [detail, setDetail] = useState<UserDetail | null>(null);
  const [libraries, setLibraries] = useState<Library[] | null>(null);
  const [loading, setLoading] = useState(!isAdmin);
  const [loadError, setLoadError] = useState<string | null>(null);

  // The edits in progress.
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [ceiling, setCeiling] = useState("");
  const [password, setPassword] = useState("");

  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);
  // Bumped by Retry to re-run the load effect after a failed fetch.
  const [reloadKey, setReloadKey] = useState(0);

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
        setChecked(new Set(d.libraryIds));
        setCeiling(d.ratingCeiling);
      } catch (err) {
        if (!cancelled) setLoadError(errorMessage(err));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [isAdmin, user.id, reloadKey]);

  function toggleChecked(libraryId: string) {
    setChecked((prev) => {
      const next = new Set(prev);
      if (next.has(libraryId)) next.delete(libraryId);
      else next.add(libraryId);
      return next;
    });
  }

  const accessDirty = detail !== null && !sameSet(checked, detail.libraryIds);
  const ceilingDirty = detail !== null && ceiling !== detail.ratingCeiling;
  const dirty = password.length > 0 || accessDirty || ceilingDirty;

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
      onClose();
    } catch (err) {
      // Refused — surface it and keep the dialog (and the edits) put.
      setSaveError(errorMessage(err));
      setSaving(false);
    }
  }

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="edit-user-dialog"
      onCancel={(e) => {
        e.preventDefault();
        if (!saving) onClose();
      }}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current && !saving) onClose();
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
            disabled={saving}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
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

              {!loading && !loadError && libraries && (
                <div className="field">
                  <span className="field-label">Library access</span>
                  {libraries.length === 0 ? (
                    <p
                      className="status status-empty"
                      data-testid="library-access-empty"
                    >
                      No libraries on this server yet.
                    </p>
                  ) : (
                    <ul className="library-checklist" data-testid="library-checklist">
                      {libraries.map((lib) => (
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
                          </label>
                        </li>
                      ))}
                    </ul>
                  )}
                  {checked.size === 0 && libraries.length > 0 && (
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
            disabled={saving}
          >
            Delete user
          </button>
          <div className="library-dialog-footer-actions">
            <button
              className="button-secondary"
              type="button"
              data-testid="edit-user-cancel"
              onClick={onClose}
              disabled={saving}
            >
              Cancel
            </button>
            <button
              className="auth-submit"
              type="button"
              data-testid="edit-user-save"
              onClick={onSave}
              disabled={saving || !dirty}
            >
              {saving ? "Saving…" : "Save changes"}
            </button>
          </div>
        </footer>
      </div>
    </dialog>
  );
}
