import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Role, User } from "../api/types";

// The Add-User dialog: a native modal <dialog> (same chrome as the Library
// dialogs) asking for a username, a password, and a role, with Create / Cancel in
// the footer. Replaces the always-mounted create form that used to sit above the
// list — the hub's "Add User" button is now the single entry point.
//
// Role defaults to Member so an Admin is never minted by accident; "Admin" is an
// explicit opt-in. "Linked server" is the `remote` role (ADR-0054) — another
// household's Server, not a person: it takes NO password (the server 400s one),
// and its username is the label the Admin picks for it ("Brandon's server").
//
// ALL LIBRARIES BY DEFAULT, FOR A PERSON. A newly created Member has no grant
// rows, and an empty grant set means "sees no catalog" (access.Service.Resolve) —
// a new account that can see nothing is a surprising default, so this dialog
// grants every Library right after the create: listLibraries →
// setLibraryAccess(id, all). An Admin needs none of this (all-access by role, and
// the server REFUSES grants on an Admin with 422 ADMIN_GRANT), so the second call
// is skipped for them.
//
// A linked server is skipped too, and for the opposite reason: the default there
// must be NOTHING. "All libraries" is a friendly default for someone in the house
// and a disclosure of the entire collection to a machine in someone else's — the
// sharer picks the two libraries they meant, by editing the user. Convenience
// never chooses the wider blast radius across a household boundary.
//
// That's two calls, and only the first is what the Admin asked for. If the grant
// leg fails, the User still EXISTS — silently closing would leave a member who
// sees an empty catalog with no hint why. So the dialog switches to a
// grant-failed state instead: the create fields lock, the failure is stated
// plainly, and the footer offers Retry (re-runs only the grant — the PUT is a
// replace-set, so retrying is safe) or Close (keeps the User, no libraries).
//
// A duplicate username comes back as a 409 USERNAME_TAKEN ApiError, which the
// client deliberately does NOT swallow: it renders inline (flagged via data-taken)
// without clearing the typed input, so the Admin can pick another name without
// retyping. Any other failure shows the same inline slot; the dialog never
// crashes and never closes on an error.

const TAKEN_CODE = "USERNAME_TAKEN";

// The roles an Admin can create. The server defaults an omitted role to "member";
// we send the chosen value explicitly so the outgoing call is unambiguous.
const ROLES: { value: Role; label: string }[] = [
  { value: "member", label: "Member" },
  { value: "admin", label: "Admin" },
  { value: "remote", label: "Linked server" },
];

/** A linked Server (ADR-0054), not a person: no password, no default grants, and
 * the username is the label the Admin picks for the other household's Server. */
function isLinkedServer(role: Role): boolean {
  return role === "remote";
}

export default function CreateUserDialog({
  onCreated,
  onClose,
}: {
  /** Called once the User exists (and its default grants are settled); the hub
   * closes the dialog and reloads the list. */
  onCreated: () => void;
  /** Close without creating anything (ESC, backdrop, ✕, or Cancel). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("member");
  const [submitting, setSubmitting] = useState(false);
  // Set once the create succeeded: from here the User exists, so the dialog can
  // no longer offer "Cancel" as if nothing happened.
  const [created, setCreated] = useState<User | null>(null);
  const [error, setError] = useState<{ message: string; taken: boolean } | null>(
    null,
  );

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  /** Grant every Library to a freshly created Member (the default access set).
   * An Admin is all-access by role, so they are skipped. Throws on refusal — the
   * caller turns that into the grant-failed state. */
  async function grantAllLibraries(user: User) {
    if (user.role === "admin" || isLinkedServer(user.role)) return;
    const libraries = await apiClient.listLibraries();
    if (libraries.length === 0) return;
    await apiClient.setLibraryAccess(
      user.id,
      libraries.map((l) => l.id),
    );
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting || created) return;
    const trimmedName = username.trim();
    const linked = isLinkedServer(role);
    if (!trimmedName || (!linked && password.length === 0)) {
      setError({
        message: linked
          ? "Enter a name for the linked server."
          : "Enter a username and a password.",
        taken: false,
      });
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const user = await apiClient.createUser({
        // A linked server carries no password key at all. The field is not even
        // rendered for the role, so there is nothing to send; omitting it keeps
        // that true if the role is switched back and forth with text typed in
        // between, which would otherwise post a password the server 400s.
        username: trimmedName,
        ...(linked ? {} : { password }),
        role,
      });
      setCreated(user);
      await grantAllLibraries(user);
      onCreated();
    } catch (err) {
      const taken = err instanceof ApiError && err.code === TAKEN_CODE;
      setError({ message: errorMessage(err), taken });
    } finally {
      setSubmitting(false);
    }
  }

  /** Retry only the grant leg, after the User itself was created. */
  async function onRetryGrant() {
    if (submitting || !created) return;
    setSubmitting(true);
    setError(null);
    try {
      await grantAllLibraries(created);
      onCreated();
    } catch (err) {
      setError({ message: errorMessage(err), taken: false });
    } finally {
      setSubmitting(false);
    }
  }

  // The create succeeded but the default grants did not: the User exists, so
  // "Cancel" would be a lie. Offer Retry / Close instead.
  const grantFailed = created !== null && error !== null;

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="create-user-dialog"
      onCancel={(e) => {
        // ESC fires a native cancel; never while a call is in flight.
        e.preventDefault();
        if (!submitting) onClose();
      }}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current && !submitting) onClose();
      }}
    >
      <form className="library-dialog-panel" onSubmit={onSubmit}>
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">Add user</h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid="create-user-close-x"
            aria-label="Close"
            onClick={onClose}
            disabled={submitting}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          <div className="field">
            <label className="field-label" htmlFor="user-username">
              {isLinkedServer(role) ? "Name" : "Username"}
            </label>
            <input
              id="user-username"
              className="field-input"
              data-testid="user-username-input"
              type="text"
              value={username}
              placeholder={isLinkedServer(role) ? "Brandon’s server" : "ada"}
              autoComplete="off"
              autoFocus
              onChange={(e) => setUsername(e.target.value)}
              disabled={submitting || created !== null}
            />
          </div>

          {!isLinkedServer(role) && (
            <div className="field">
              <label className="field-label" htmlFor="user-password">
                Password
              </label>
              <input
                id="user-password"
                className="field-input"
                data-testid="user-password-input"
                type="password"
                value={password}
                placeholder="A strong password"
                autoComplete="new-password"
                onChange={(e) => setPassword(e.target.value)}
                disabled={submitting || created !== null}
              />
            </div>
          )}

          <div className="field">
            <label className="field-label" htmlFor="user-role">
              Role
            </label>
            <select
              id="user-role"
              className="field-input"
              data-testid="user-role-select"
              value={role}
              onChange={(e) => setRole(e.target.value as Role)}
              disabled={submitting || created !== null}
            >
              {ROLES.map((r) => (
                <option key={r.value} value={r.value}>
                  {r.label}
                </option>
              ))}
            </select>
          </div>

          {isLinkedServer(role) && (
            <p className="field-hint" data-testid="create-user-linked-hint">
              Another household’s server, not a person. It has no password and
              cannot sign in — edit it to grant libraries and generate an invite.
              It starts with access to nothing.
            </p>
          )}

          {role !== "admin" && !isLinkedServer(role) && (
            <p className="field-hint" data-testid="create-user-access-hint">
              Starts with access to all libraries and no rating ceiling. Narrow it
              any time by editing the user.
            </p>
          )}

          {error && (
            <p
              className="auth-error"
              data-testid="create-user-error"
              data-taken={error.taken ? "true" : undefined}
              role="alert"
            >
              {grantFailed
                ? `“${created.username}” was created, but granting access to all libraries failed: ${error.message}`
                : error.message}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          {grantFailed ? (
            <>
              <button
                className="button-secondary"
                type="button"
                data-testid="create-user-close"
                onClick={onCreated}
                disabled={submitting}
              >
                Close
              </button>
              <button
                className="auth-submit"
                type="button"
                data-testid="create-user-retry-grant"
                onClick={onRetryGrant}
                disabled={submitting}
              >
                {submitting ? "Retrying…" : "Retry"}
              </button>
            </>
          ) : (
            <>
              <button
                className="button-secondary"
                type="button"
                data-testid="create-user-cancel"
                onClick={onClose}
                disabled={submitting}
              >
                Cancel
              </button>
              <button
                className="auth-submit"
                type="submit"
                data-testid="create-user-submit"
                disabled={submitting}
              >
                {submitting ? "Creating…" : "Create"}
              </button>
            </>
          )}
        </footer>
      </form>
    </dialog>
  );
}
