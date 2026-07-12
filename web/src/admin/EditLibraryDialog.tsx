import { useEffect, useRef, useState } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import { LibraryKindIcon, libraryKindLabel } from "../browse/kindIcons";
import EnrichmentPolicyPanel from "./EnrichmentPolicyPanel";

// The Edit-Library dialog: a native modal <dialog> that lets an Admin correct a
// Library in place — rename it or add root folders — with a Close button at the
// bottom. Rename and Add-folder each PATCH the Library and update the dialog's
// local view (and notify the hub to reload) without closing, so the Admin can make
// several edits in one sitting; the roots list reflects each added folder
// immediately. Deleting a Library is NOT here: it's the row's ⋮ menu action (with
// its own confirmation modal), so this dialog carries no destructive control.
//
// The dialog is TABBED (ADR-0027): a "General" tab carries the rename / add-folder
// controls, and a "Metadata Providers" tab carries the per-Library Enrichment
// policy (the EnrichmentPolicyPanel). The policy panel is mounted only when its tab
// is active, so its policy is fetched when the Admin first opens the tab — not on
// every Edit-dialog open. Both tabs share the one dialog chrome.
//
// The kind is fixed at creation (a Library holds exactly one media kind,
// CONTEXT.md), so it's shown but not editable. A folder-overlap conflict on Add
// surfaces as a 409 FOLDER_OVERLAP inline (data-overlap), never a crash — the same
// posture as create.

const OVERLAP_CODE = "FOLDER_OVERLAP";

type EditLibraryTab = "general" | "metadata-providers";

export default function EditLibraryDialog({
  library,
  onChanged,
  onClose,
}: {
  library: Library;
  /** Called after a successful rename or add-folder; the hub reloads its list. */
  onChanged: () => void;
  /** Close without further changes (ESC, backdrop, ✕, or Close). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  // Local view of the Library so the roots list reflects an add immediately; the
  // hub also reloads, but the open dialog shows its own up-to-date copy.
  const [lib, setLib] = useState<Library>(library);
  const [tab, setTab] = useState<EditLibraryTab>("general");
  const [name, setName] = useState(library.name);
  const [addPath, setAddPath] = useState("");
  const [renaming, setRenaming] = useState(false);
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState<{ message: string; overlap: boolean } | null>(
    null,
  );

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  const trimmedName = name.trim();
  const trimmedAddPath = addPath.trim();
  const busy = renaming || adding;
  const nameDirty = trimmedName !== "" && trimmedName !== lib.name;

  async function onRename() {
    if (busy || !nameDirty) return;
    setRenaming(true);
    setError(null);
    try {
      const updated = await apiClient.updateLibrary(lib.id, { name: trimmedName });
      setLib(updated);
      setName(updated.name);
      onChanged();
    } catch (err) {
      setError({ message: errorMessage(err), overlap: false });
    } finally {
      setRenaming(false);
    }
  }

  async function onAddFolder() {
    if (busy || !trimmedAddPath) return;
    setAdding(true);
    setError(null);
    try {
      const updated = await apiClient.updateLibrary(lib.id, {
        addRootFolders: [trimmedAddPath],
      });
      setLib(updated);
      setAddPath("");
      onChanged();
    } catch (err) {
      const overlap = err instanceof ApiError && err.code === OVERLAP_CODE;
      setError({ message: errorMessage(err), overlap });
    } finally {
      setAdding(false);
    }
  }

  const tabs: { key: EditLibraryTab; label: string }[] = [
    { key: "general", label: "General" },
    { key: "metadata-providers", label: "Metadata Providers" },
  ];

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="edit-library-dialog"
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">
            <span className="admin-library-icon" aria-hidden="true">
              <LibraryKindIcon kind={lib.kind} className="admin-library-kind-icon" />
            </span>
            Edit library
            <span className="library-dialog-kind">{libraryKindLabel(lib.kind)}</span>
          </h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid="edit-library-close-x"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </header>

        <div className="edit-item-tablist" role="tablist" aria-label="Edit library sections">
          {tabs.map((t) => {
            const selected = t.key === tab;
            return (
              <button
                key={t.key}
                className={`edit-item-tab${selected ? " is-active" : ""}`}
                type="button"
                role="tab"
                aria-selected={selected}
                data-testid={`edit-library-tab-${t.key}`}
                onClick={() => setTab(t.key)}
              >
                {t.label}
              </button>
            );
          })}
        </div>

        <div className="library-dialog-body" role="tabpanel">
          {tab === "general" ? (
            <>
              <div className="field">
                <label className="field-label" htmlFor="edit-library-name">
                  Name
                </label>
                <div className="edit-library-name-row">
                  <input
                    id="edit-library-name"
                    className="field-input"
                    data-testid="edit-library-name-input"
                    type="text"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" && nameDirty) void onRename();
                    }}
                    disabled={busy}
                  />
                  <button
                    className="nav-link"
                    type="button"
                    data-testid="edit-library-save-name"
                    onClick={onRename}
                    disabled={busy || !nameDirty}
                  >
                    {renaming ? "Saving…" : "Save"}
                  </button>
                </div>
              </div>

              <div className="field">
                <span className="field-label">Root folders</span>
                <ul className="edit-library-roots" data-testid="edit-library-roots">
                  {lib.rootFolders.map((root) => (
                    <li key={root.id} className="edit-library-root">
                      {root.path}
                    </li>
                  ))}
                </ul>
                <div className="edit-library-add-row">
                  <input
                    className="field-input"
                    data-testid="edit-library-add-folder-input"
                    type="text"
                    value={addPath}
                    placeholder="/media/movies-4k"
                    onChange={(e) => setAddPath(e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter" && trimmedAddPath) void onAddFolder();
                    }}
                    disabled={busy}
                  />
                  <button
                    className="nav-link"
                    type="button"
                    data-testid="edit-library-add-folder"
                    onClick={onAddFolder}
                    disabled={busy || !trimmedAddPath}
                  >
                    {adding ? "Adding…" : "Add folder"}
                  </button>
                </div>
              </div>

              {error && (
                <p
                  className="auth-error"
                  data-testid="edit-library-error"
                  data-overlap={error.overlap ? "true" : undefined}
                  role="alert"
                >
                  {error.message}
                </p>
              )}
            </>
          ) : (
            <EnrichmentPolicyPanel library={lib} />
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="auth-submit"
            type="button"
            data-testid="edit-library-close"
            onClick={onClose}
          >
            Close
          </button>
        </footer>
      </div>
    </dialog>
  );
}
