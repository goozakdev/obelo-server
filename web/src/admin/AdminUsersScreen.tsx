import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { User } from "../api/types";
import AdminListPanel from "./AdminListPanel";
import UserAdminRow from "./UserAdminRow";
import CreateUserDialog from "./CreateUserDialog";
import EditUserDialog from "./EditUserDialog";
import ConfirmDialog from "./ConfirmDialog";

// The Users management hub. Behind RequireAdmin (App.tsx) and still
// server-enforced (a Member never sees the tab and is redirected if they
// deep-link; the /users API is Admin scope regardless). Mirrors the libraries
// hub's composition — a filled header bar over a framed body, held to a
// readable column rather than the full content width:
//   - a "N users" count and an "Add User" button opening the create dialog,
//   - one row per User (username + role) carrying an Edit and a Delete icon,
//   - the loading/error/empty states, which share the list's frame so the panel
//     keeps its shape whatever state it is in.
//
// The three dialogs are mounted on demand and ALL live here, not on the row.
// That's what makes the two delete paths one path: the row's trash icon and the
// Edit dialog's "Delete user" button both just set `deleting`, so there is a
// single confirmation and a single delete call site. Opening the confirmation
// from the Edit dialog closes that dialog first, so two modals never stack.
//
// The list is reloaded after a create or a delete so the UI reflects the server's
// truth rather than patching local state. Edits (password, grants, ceiling) don't
// touch what a row displays, so a clean save needs no reload. A small reloadable
// loader is used here (not the load-once useAsync) because this screen mutates the
// very list it shows.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; users: User[] };

export default function AdminUsersScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const [deleting, setDeleting] = useState<User | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    setState({ status: "loading" });
    try {
      const users = await apiClient.listUsers(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", users });
    } catch (err) {
      if (signal?.aborted) return;
      setState({ status: "error", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  const reload = useCallback(() => void load(), [load]);

  function requestDelete(user: User) {
    // Closing the Edit dialog first keeps the confirmation the only open modal
    // when delete is reached from inside it.
    setEditing(null);
    setDeleteError(null);
    setDeleting(user);
  }

  async function onConfirmDelete() {
    if (!deleting || deleteBusy) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      await apiClient.deleteUser(deleting.id);
      setDeleting(null);
      setDeleteBusy(false);
      reload();
    } catch (err) {
      // Refused (notably 409 LAST_ADMIN) — keep the dialog open with a readable
      // message; the User stays in the list.
      setDeleteError(errorMessage(err));
      setDeleteBusy(false);
    }
  }

  const users = state.status === "ready" ? state.users : [];
  const count = users.length;

  return (
    <section className="admin-users" data-testid="admin-users">
      <AdminListPanel
        count={`${count} ${count === 1 ? "user" : "users"}`}
        countTestId="admin-users-count"
        action={
          <button
            className="auth-submit admin-panel-action"
            type="button"
            data-testid="add-user-button"
            onClick={() => setAddOpen(true)}
          >
            Add User
          </button>
        }
      >
        {state.status === "loading" && (
          <p className="status status-loading" data-testid="admin-users-loading">
            Loading users&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p
            className="status status-error"
            data-testid="admin-users-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && count === 0 && (
          <p className="status status-empty" data-testid="admin-users-empty">
            No users yet. Click “Add User” to create one.
          </p>
        )}

        {state.status === "ready" && count > 0 && (
          <ul className="admin-user-list" data-testid="admin-user-list">
            {users.map((u) => (
              <UserAdminRow
                key={u.id}
                user={u}
                onEdit={setEditing}
                onDelete={requestDelete}
              />
            ))}
          </ul>
        )}
      </AdminListPanel>

      {addOpen && (
        <CreateUserDialog
          onClose={() => setAddOpen(false)}
          onCreated={() => {
            setAddOpen(false);
            reload();
          }}
        />
      )}

      {editing && (
        <EditUserDialog
          user={editing}
          onClose={() => setEditing(null)}
          onRequestDelete={requestDelete}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title="Delete user"
          message={`Delete “${deleting.username}”? Their access, devices, and watch history go with them. This can’t be undone.`}
          confirmLabel="Delete"
          busyLabel="Deleting…"
          busy={deleteBusy}
          error={deleteError}
          onConfirm={onConfirmDelete}
          onCancel={() => {
            if (deleteBusy) return;
            setDeleting(null);
            setDeleteError(null);
          }}
        />
      )}
    </section>
  );
}
