import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import LibraryAdminRow from "./LibraryAdminRow";
import AddLibraryWizard from "./AddLibraryWizard";
import EditLibraryDialog from "./EditLibraryDialog";

// The library-management hub, redesigned for a cleaner layout (issue:
// admin-libraries-ui). Behind RequireAdmin (App.tsx) and still server-enforced.
// Three parts:
//   - a top bar: an "N libraries" count on the left, and "Add Library" +
//     "Scan All Libraries" actions on the right;
//   - the list of Libraries, each row (LibraryAdminRow) carrying its kind icon,
//     name, per-Library scan controls + status, and an Edit affordance; an empty
//     list shows a single call-to-action line instead;
//   - two modal dialogs, mounted on demand: the Add-Library wizard and the
//     Edit-Library dialog (rename / add folders / delete).
//
// The list is reloaded after any create / edit / delete so the UI reflects the
// server's truth without patching local state. A small reloadable loader is used
// here (rather than a load-once useAsync) because this screen mutates the very
// list it shows. "Scan All" bumps a signal every row observes to trigger its own
// incremental scan, so each Library's poller stays in charge of its own status.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; libraries: Library[] };

export default function AdminLibrariesScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<Library | null>(null);
  const [scanAllSignal, setScanAllSignal] = useState(0);

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

  const libraries = state.status === "ready" ? state.libraries : [];
  const count = libraries.length;

  return (
    <section className="admin-libraries" data-testid="admin-libraries">
      <div className="admin-libraries-bar">
        <span className="admin-libraries-count" data-testid="admin-libraries-count">
          {count} {count === 1 ? "library" : "libraries"}
        </span>
        <div className="admin-libraries-bar-actions">
          <button
            className="auth-submit admin-libraries-add"
            type="button"
            data-testid="add-library-button"
            onClick={() => setAddOpen(true)}
          >
            Add Library
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="scan-all-button"
            onClick={() => setScanAllSignal((n) => n + 1)}
            disabled={count === 0}
          >
            Scan All Libraries
          </button>
        </div>
      </div>

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

      {state.status === "ready" && count === 0 && (
        <p className="status status-empty" data-testid="admin-libraries-empty">
          No libraries configured. Click “Add Library” to get started.
        </p>
      )}

      {state.status === "ready" && count > 0 && (
        <ul className="admin-library-list" data-testid="admin-library-list">
          {libraries.map((lib) => (
            <LibraryAdminRow
              key={lib.id}
              library={lib}
              onEdit={setEditing}
              scanAllSignal={scanAllSignal}
            />
          ))}
        </ul>
      )}

      {addOpen && (
        <AddLibraryWizard
          onClose={() => setAddOpen(false)}
          onCreated={() => {
            setAddOpen(false);
            reload();
          }}
        />
      )}

      {editing && (
        <EditLibraryDialog
          library={editing}
          onChanged={reload}
          onDeleted={reload}
          onClose={() => setEditing(null)}
        />
      )}
    </section>
  );
}
