import { useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library, User, UserDetail } from "../api/types";

// One User row in the Admin Users hub. Identity (username + role) plus the row's
// controls: the library-access grant editor + Rating ceiling (access-control-
// admin-ui issues 02/03), a password reset (issue 03), and a delete with a
// confirmation step (issue 01). The head/identity, the access section, the
// password section, and the delete actions are kept as distinct blocks so each
// slice extends one without reworking the others.
//
// Library grants (issue 02) + Rating ceiling (issue 03):
//   - An ADMIN is implicitly all-access and uncapped, so the row reads
//     "All libraries · no cap" and exposes NO grant/ceiling control (the server
//     rejects granting or capping an Admin).
//   - For a MEMBER, a "Manage libraries" toggle reveals an inline panel carrying
//     BOTH access dimensions: a checklist of ALL Libraries with the Member's
//     current grants pre-ticked, and a Rating-ceiling dropdown of the MPAA rungs
//     (G/PG/PG-13/R/NC-17) plus "No limit". Saving the checklist sends the FULL
//     ticked set (replace-set, not deltas) via setLibraryAccess; an empty set
//     means "sees no catalog". Choosing a ceiling rung sends that label (or null
//     for "No limit") via setRatingCeiling — the server's single maturity rank
//     caps the TV system too, so one ladder suffices. The collapsed row then
//     summarizes both dimensions.
//   - The detail (getUser) and the Library list (listLibraries) load LAZILY when
//     the panel first opens — so rendering the list costs no per-Member fetches,
//     keeping a large roster's load light. Once loaded they are cached, so the
//     summary persists after the panel is collapsed.
//   - A refused save is NOT swallowed: 422 ADMIN_GRANT/UNKNOWN_LIBRARY (grants)
//     or 422 ADMIN_CEILING/UNKNOWN_RATING (ceiling) — both defensive on a Member
//     row — surface as a readable inline message and the panel stays put.
//
// Password reset (issue 03) is available for ANY User (incl. an Admin — a reset
// is not access-restricted): a "Reset password" toggle reveals an inline field +
// Save; a successful save shows a success affordance and closes the field.
//
// Delete is two-step: the first click reveals an inline Confirm/Cancel, so an
// account (and its watch history) is never removed by a stray click. A refused
// delete — notably 409 LAST_ADMIN when this is the final Admin — is NOT swallowed:
// it renders as a readable inline message and the User stays in the list.

/** The Rating-ceiling option set (PRD "Rating-ceiling option set"): the MPAA
 * rungs. The dropdown also offers "No limit" (the empty value → `null`, uncapped).
 * One ladder suffices — the server's single maturity rank caps the TV system too,
 * so we deliberately do NOT model a separate TV taxonomy on the client. */
const RATING_RUNGS = ["G", "PG", "PG-13", "R", "NC-17"] as const;

/** How a Rating ceiling reads in a summary: the rung label, or "No limit" when
 * the ceiling is empty (uncapped). */
function ceilingLabel(ceiling: string): string {
  return ceiling ? ceiling : "No limit";
}

export default function UserAdminRow({
  user,
  onDeleted,
}: {
  user: User;
  onDeleted: () => void;
}) {
  const isAdmin = user.role === "admin";

  // --- Library grants (issue 02) -----------------------------------------
  const [expanded, setExpanded] = useState(false);
  const [detail, setDetail] = useState<UserDetail | null>(null);
  const [libraries, setLibraries] = useState<Library[] | null>(null);
  const [loadingAccess, setLoadingAccess] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  // --- Rating ceiling (issue 03) -----------------------------------------
  const [savingCeiling, setSavingCeiling] = useState(false);
  const [ceilingError, setCeilingError] = useState<string | null>(null);

  // --- Password reset (issue 03) -----------------------------------------
  const [pwOpen, setPwOpen] = useState(false);
  const [pwValue, setPwValue] = useState("");
  const [savingPw, setSavingPw] = useState(false);
  const [pwError, setPwError] = useState<string | null>(null);
  const [pwSuccess, setPwSuccess] = useState(false);

  // --- Delete (issue 01) -------------------------------------------------
  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function loadAccess() {
    setLoadingAccess(true);
    setLoadError(null);
    try {
      const [d, libs] = await Promise.all([
        apiClient.getUser(user.id),
        apiClient.listLibraries(),
      ]);
      setDetail(d);
      setLibraries(libs);
      setChecked(new Set(d.libraryIds));
    } catch (err) {
      setLoadError(errorMessage(err));
    } finally {
      setLoadingAccess(false);
    }
  }

  function toggleEditor() {
    if (expanded) {
      setExpanded(false);
      return;
    }
    setExpanded(true);
    setSaveError(null);
    // Lazy load on first open (and retry on a prior load failure).
    if ((!detail || !libraries) && !loadingAccess) {
      void loadAccess();
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

  async function onSaveAccess() {
    if (saving) return;
    setSaving(true);
    setSaveError(null);
    try {
      // The FULL desired set (replace-set). An empty array = "sees no catalog".
      await apiClient.setLibraryAccess(user.id, [...checked]);
      // Refetch so the granted summary reflects the server's resulting state.
      const d = await apiClient.getUser(user.id);
      setDetail(d);
      setChecked(new Set(d.libraryIds));
    } catch (err) {
      // Refused (ADMIN_GRANT / UNKNOWN_LIBRARY) — surface it, keep the panel.
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function onChangeCeiling(value: string) {
    if (savingCeiling) return;
    // The empty option ("No limit") clears the ceiling — send `null`, not "".
    const rating = value === "" ? null : value;
    setSavingCeiling(true);
    setCeilingError(null);
    try {
      await apiClient.setRatingCeiling(user.id, rating);
      // Reflect the new ceiling from the known value (the grant editor refetches
      // for its summary, but refetching here would clobber any in-progress
      // checklist edits — the chosen rung is authoritative, so update locally).
      setDetail((prev) => (prev ? { ...prev, ratingCeiling: value } : prev));
    } catch (err) {
      // Refused (ADMIN_CEILING / UNKNOWN_RATING) — surface it, keep the panel.
      setCeilingError(errorMessage(err));
    } finally {
      setSavingCeiling(false);
    }
  }

  async function onSavePassword() {
    if (savingPw || pwValue.length === 0) return;
    setSavingPw(true);
    setPwError(null);
    setPwSuccess(false);
    try {
      await apiClient.setPassword(user.id, pwValue);
      // Success: close the field, clear what was typed, show the affordance.
      setPwSuccess(true);
      setPwOpen(false);
      setPwValue("");
    } catch (err) {
      setPwError(errorMessage(err));
    } finally {
      setSavingPw(false);
    }
  }

  async function onConfirmDelete() {
    if (deleting) return;
    setDeleting(true);
    setError(null);
    try {
      await apiClient.deleteUser(user.id);
      onDeleted();
    } catch (err) {
      // Refused (e.g. LAST_ADMIN) — surface it and keep the row + its controls.
      setError(errorMessage(err));
      setDeleting(false);
      setConfirming(false);
    }
  }

  // Granted Library names for the summary; fall back to the raw id if a granted
  // Library is no longer in the list.
  const grantedNames =
    detail && libraries
      ? detail.libraryIds.map(
          (id) => libraries.find((l) => l.id === id)?.name ?? id,
        )
      : [];

  return (
    <li
      className="admin-user-row card"
      data-testid="admin-user-row"
      data-user-id={user.id}
    >
      <div className="admin-user-head">
        <span className="user-username" data-testid="admin-user-username">
          {user.username}
        </span>
        <span className="user-role" data-testid="admin-user-role">
          {user.role}
        </span>
      </div>

      <div className="admin-user-access" data-testid="user-access">
        {isAdmin ? (
          // An Admin is implicitly all-access AND uncapped; no editable grant or
          // ceiling control (the server rejects both for an Admin).
          <span className="user-access-summary" data-testid="admin-all-libraries">
            All libraries{" "}
            <span data-testid="admin-no-cap">&middot; no cap</span>
          </span>
        ) : (
          <>
            <div className="admin-user-access-head">
              <span className="user-access-label">Library access</span>
              {detail && (
                <span
                  className="user-access-summary"
                  data-testid="granted-libraries"
                >
                  {grantedNames.length > 0
                    ? grantedNames.join(", ")
                    : "No libraries (sees no catalog)"}
                </span>
              )}
              {detail && (
                <span
                  className="user-access-summary"
                  data-testid="rating-ceiling-summary"
                >
                  Rating ceiling: {ceilingLabel(detail.ratingCeiling)}
                </span>
              )}
              <button
                className="nav-link"
                type="button"
                data-testid="manage-libraries-button"
                onClick={toggleEditor}
                aria-expanded={expanded}
              >
                {expanded ? "Close" : "Manage libraries"}
              </button>
            </div>

            {expanded && (
              <div
                className="library-access-editor"
                data-testid="library-access-editor"
              >
                {loadingAccess && (
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
                      onClick={() => void loadAccess()}
                    >
                      Retry
                    </button>
                  </p>
                )}

                {!loadingAccess && !loadError && libraries && (
                  <>
                    {libraries.length === 0 ? (
                      <p
                        className="status status-empty"
                        data-testid="library-access-empty"
                      >
                        No libraries on this server yet.
                      </p>
                    ) : (
                      <ul
                        className="library-checklist"
                        data-testid="library-checklist"
                      >
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

                    <button
                      className="nav-link"
                      type="button"
                      data-testid="save-library-access-button"
                      onClick={onSaveAccess}
                      disabled={saving}
                    >
                      {saving ? "Saving…" : "Save library access"}
                    </button>

                    {saveError && (
                      <p
                        className="status status-error"
                        data-testid="library-access-error"
                        role="alert"
                      >
                        <span className="dot dot-error" aria-hidden="true" />
                        {saveError}
                      </p>
                    )}
                  </>
                )}

                {/* Rating ceiling — needs only the detail (loaded alongside the
                    Library list), so it renders even when the server has no
                    Libraries yet. Choosing a rung saves immediately; "No limit"
                    (the empty value) clears the cap. */}
                {detail && (
                  <div
                    className="rating-ceiling-editor"
                    data-testid="rating-ceiling-editor"
                  >
                    <label className="rating-ceiling-label">
                      Rating ceiling{" "}
                      <select
                        data-testid="rating-ceiling-select"
                        value={detail.ratingCeiling}
                        onChange={(e) => void onChangeCeiling(e.target.value)}
                        disabled={savingCeiling}
                      >
                        <option value="">No limit</option>
                        {RATING_RUNGS.map((rung) => (
                          <option key={rung} value={rung}>
                            {rung}
                          </option>
                        ))}
                      </select>
                    </label>
                    {savingCeiling && (
                      <span
                        className="status status-loading"
                        data-testid="rating-ceiling-saving"
                      >
                        Saving&hellip;
                      </span>
                    )}
                    {ceilingError && (
                      <p
                        className="status status-error"
                        data-testid="rating-ceiling-error"
                        role="alert"
                      >
                        <span className="dot dot-error" aria-hidden="true" />
                        {ceilingError}
                      </p>
                    )}
                  </div>
                )}
              </div>
            )}
          </>
        )}
      </div>

      <div className="admin-user-password" data-testid="user-password">
        {!pwOpen ? (
          <button
            className="nav-link"
            type="button"
            data-testid="reset-password-button"
            onClick={() => {
              setPwOpen(true);
              setPwValue("");
              setPwError(null);
              setPwSuccess(false);
            }}
          >
            Reset password
          </button>
        ) : (
          <div className="password-reset-form" data-testid="password-reset-form">
            <label className="password-reset-label">
              New password{" "}
              <input
                type="password"
                data-testid="new-password-input"
                value={pwValue}
                onChange={(e) => setPwValue(e.target.value)}
                disabled={savingPw}
              />
            </label>
            <button
              className="nav-link"
              type="button"
              data-testid="save-password-button"
              onClick={onSavePassword}
              disabled={savingPw || pwValue.length === 0}
            >
              {savingPw ? "Saving…" : "Save password"}
            </button>
            <button
              className="nav-link"
              type="button"
              data-testid="cancel-password-button"
              onClick={() => setPwOpen(false)}
              disabled={savingPw}
            >
              Cancel
            </button>
          </div>
        )}

        {pwSuccess && (
          <p
            className="status status-ok"
            data-testid="password-reset-success"
            role="status"
          >
            Password updated.
          </p>
        )}

        {pwError && (
          <p
            className="status status-error"
            data-testid="password-reset-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {pwError}
          </p>
        )}
      </div>

      <div className="admin-user-actions">
        {!confirming ? (
          <button
            className="nav-link nav-logout"
            type="button"
            data-testid="delete-user-button"
            onClick={() => {
              setError(null);
              setConfirming(true);
            }}
            disabled={deleting}
          >
            Delete
          </button>
        ) : (
          <span className="admin-user-confirm" data-testid="delete-user-confirm">
            <span className="confirm-prompt">Delete {user.username}?</span>
            <button
              className="nav-link nav-logout"
              type="button"
              data-testid="delete-user-confirm-button"
              onClick={onConfirmDelete}
              disabled={deleting}
            >
              {deleting ? "Deleting…" : "Confirm delete"}
            </button>
            <button
              className="nav-link"
              type="button"
              data-testid="delete-user-cancel-button"
              onClick={() => setConfirming(false)}
              disabled={deleting}
            >
              Cancel
            </button>
          </span>
        )}
      </div>

      {error && (
        <p
          className="status status-error"
          data-testid="delete-user-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </li>
  );
}
