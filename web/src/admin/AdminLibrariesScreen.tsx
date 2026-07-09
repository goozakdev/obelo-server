import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import CreateLibraryForm from "./CreateLibraryForm";
import LibraryAdminRow from "./LibraryAdminRow";

// The library-management hub (issue 06). Behind RequireAdmin (App.tsx) and still
// server-enforced. Three parts:
//   - a create form (name + one-or-more root folders; kind is movie),
//   - the list of existing libraries, each row carrying its scan controls
//     (incremental / full) and a polled scan-status indicator + delete, and
//   - the loading/error/empty states reusing the app's established patterns.
//
// The list is reloaded after a create or delete so the UI reflects the server's
// truth (a created library appears; a deleted one disappears) without trying to
// patch local state. A small reloadable loader is used here rather than the
// load-once useAsync because this screen mutates the very list it shows.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; libraries: Library[] };

export default function AdminLibrariesScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });

  const load = useCallback(async (signal?: AbortSignal) => {
    setState({ status: "loading" });
    try {
      const libraries = await apiClient.listLibraries(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", libraries });
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
    <section className="admin-libraries" data-testid="admin-libraries">
      <CreateLibraryForm onCreated={reload} />

      <h2 className="section-title">Libraries</h2>

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="admin-libraries-loading">
          Loading libraries&hellip;
        </p>
      )}

      {state.status === "error" && (
        <p
          className="status status-error"
          data-testid="admin-libraries-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}

      {state.status === "ready" && state.libraries.length === 0 && (
        <div className="card" data-testid="admin-libraries-empty">
          <p className="status status-loading">
            No libraries yet. Create one above, then run a scan.
          </p>
        </div>
      )}

      {state.status === "ready" && state.libraries.length > 0 && (
        <ul className="admin-library-list" data-testid="admin-library-list">
          {state.libraries.map((lib) => (
            <LibraryAdminRow key={lib.id} library={lib} onDeleted={reload} />
          ))}
        </ul>
      )}
    </section>
  );
}
