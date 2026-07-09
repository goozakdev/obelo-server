import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { User } from "../api/types";
import CreateUserForm from "./CreateUserForm";
import UserAdminRow from "./UserAdminRow";

// The Users management hub (access-control-admin-ui issue 01). Behind RequireAdmin
// (App.tsx) and still server-enforced (a Member never sees the tab and is
// redirected if they deep-link; the /users API is Admin scope regardless). Mirrors
// the libraries-admin composition — a create form + the list, each row carrying
// its own actions:
//   - a create-User form (username + password; role defaulting to Member),
//   - the list of existing Users, each row showing username + role with a delete
//     (confirm + the LAST_ADMIN inline error), and
//   - the loading/error/empty states reusing the app's established patterns.
//
// The list is reloaded after a create or delete so the UI reflects the server's
// truth (a created User appears; a deleted one disappears) rather than patching
// local state. A small reloadable loader is used here (not the load-once useAsync)
// because this screen mutates the very list it shows.
//
// This is the scaffold issues 02 (library grants) and 03 (rating ceiling +
// password reset) extend: those add row controls to UserAdminRow without reworking
// the screen's load → mutate → refetch loop.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; users: User[] };

export default function AdminUsersScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });

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

  return (
    <section className="admin-users" data-testid="admin-users">
      <CreateUserForm onCreated={reload} />

      <h2 className="section-title">Users</h2>

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

      {state.status === "ready" && state.users.length === 0 && (
        <div className="card" data-testid="admin-users-empty">
          <p className="status status-loading">No users yet. Create one above.</p>
        </div>
      )}

      {state.status === "ready" && state.users.length > 0 && (
        <ul className="admin-user-list" data-testid="admin-user-list">
          {state.users.map((u) => (
            <UserAdminRow key={u.id} user={u} onDeleted={reload} />
          ))}
        </ul>
      )}
    </section>
  );
}
