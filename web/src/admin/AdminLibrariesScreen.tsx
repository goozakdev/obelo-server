import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import AdminListPanel from "./AdminListPanel";
import LibraryAdminRow from "./LibraryAdminRow";
import AddLibraryWizard from "./AddLibraryWizard";
import EditLibraryDialog from "./EditLibraryDialog";

// The library-management hub, redesigned for a cleaner layout (issue:
// admin-libraries-ui). Behind RequireAdmin (App.tsx) and still server-enforced.
// Three parts:
//   - a "Scan All Libraries" area in its own band at the top;
//   - a bar below it: an "N libraries" count on the left and the "Add Library"
//     action on the right;
//   - the list of Libraries, each row (LibraryAdminRow) carrying its kind icon,
//     name, status, an Edit affordance, and a ⋮ actions menu (Scan / Full scan /
//     Delete); an empty list shows a single call-to-action line instead;
//   - two modal dialogs, mounted on demand: the Add-Library wizard and the
//     Edit-Library dialog (rename / add folders). Delete lives on the row's ⋮
//     menu (its own confirmation modal), not in the Edit dialog.
//
// A LINKED Library (ADR-0056 §1) is badged and its write affordances disabled, in
// the row. The Library JSON says only that it IS linked — it never names whose it
// is — so the server name behind "provided by …" comes from GET /links, which is
// read ONLY when at least one Library is linked: a household that has never linked
// makes exactly the requests it always did. A failed read is not an error state
// here; the badge stands and the note says "another server", because losing the
// whole libraries list to a second request would be the worse trade.
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
  // libraryId → the name of the Server providing it. Empty on a server with no
  // Links, which is the overwhelmingly common case.
  const [providers, setProviders] = useState<Record<string, string>>({});
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<Library | null>(null);
  const [scanAllSignal, setScanAllSignal] = useState(0);

  const loadProviders = useCallback(async (signal?: AbortSignal) => {
    try {
      const links = await apiClient.listLinks(signal);
      if (signal?.aborted) return;
      const map: Record<string, string> = {};
      for (const link of links) {
        for (const lib of link.libraries) map[lib.id] = link.serverName;
      }
      setProviders(map);
    } catch {
      // The badge is the load-bearing part and it is already on the row; a name
      // this could not fetch degrades to "another server" rather than to an
      // error banner over a libraries list that loaded perfectly well.
    }
  }, []);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setState({ status: "loading" });
      try {
        const libraries = await apiClient.listLibraries(signal);
        if (signal?.aborted) return;
        setState({ status: "ready", libraries });
        // Only when something IS a mirror: a household that has never linked
        // makes exactly the requests it always did.
        if (libraries.some((l) => l.linked)) void loadProviders(signal);
      } catch (err) {
        if (signal?.aborted) return;
        setState({ status: "error", message: errorMessage(err) });
      }
    },
    [loadProviders],
  );

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
      {state.status === "ready" && (
        <div className="admin-libraries-scan-all">
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
      )}

      <AdminListPanel
        count={`${count} ${count === 1 ? "library" : "libraries"}`}
        countTestId="admin-libraries-count"
        action={
          <button
            className="auth-submit admin-panel-action"
            type="button"
            data-testid="add-library-button"
            onClick={() => setAddOpen(true)}
          >
            Add Library
          </button>
        }
      >
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
                onDeleted={reload}
                scanAllSignal={scanAllSignal}
                providedBy={providers[lib.id]}
              />
            ))}
          </ul>
        )}
      </AdminListPanel>

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
          onClose={() => setEditing(null)}
        />
      )}
    </section>
  );
}
