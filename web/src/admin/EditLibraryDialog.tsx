import { useEffect, useRef, useState } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import { LibraryKindIcon, libraryKindLabel } from "../browse/kindIcons";

// The Edit-Library dialog: a native modal <dialog> that lets an Admin correct a
// Library in place — rename it, add root folders, or delete it — with a Close
// button at the bottom. Rename and Add-folder each PATCH the Library and update
// the dialog's local view (and notify the hub to reload) without closing, so the
// Admin can make several edits in one sitting; the roots list reflects each added
// folder immediately. Delete is a two-click danger action (click → confirm) that,
// on success, closes the dialog and tells the hub the Library is gone.
//
// The kind is fixed at creation (a Library holds exactly one media kind,
// CONTEXT.md), so it's shown but not editable. A folder-overlap conflict on Add
// surfaces as a 409 FOLDER_OVERLAP inline (data-overlap), never a crash — the same
// posture as create.

const OVERLAP_CODE = "FOLDER_OVERLAP";

export default function EditLibraryDialog({
  library,
  onChanged,
  onDeleted,
  onClose,
}: {
  library: Library;
  /** Called after a successful rename or add-folder; the hub reloads its list. */
  onChanged: () => void;
  /** Called after a successful delete; the hub reloads and this dialog closes. */
  onDeleted: () => void;
  /** Close without further changes (ESC, backdrop, ✕, or Close). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  // Local view of the Library so the roots list reflects an add immediately; the
  // hub also reloads, but the open dialog shows its own up-to-date copy.
  const [lib, setLib] = useState<Library>(library);
  const [name, setName] = useState(library.name);
  const [addPath, setAddPath] = useState("");
  const [renaming, setRenaming] = useState(false);
  const [adding, setAdding] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [error, setError] = useState<{ message: string; overlap: boolean } | null>(
    null,
  );

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  const trimmedName = name.trim();
  const trimmedAddPath = addPath.trim();
  const busy = renaming || adding || deleting;
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

  async function onDelete() {
    if (busy) return;
    setDeleting(true);
    setError(null);
    try {
      await apiClient.deleteLibrary(lib.id);
      onDeleted();
      onClose();
    } catch (err) {
      setError({ message: errorMessage(err), overlap: false });
      setDeleting(false);
      setConfirmingDelete(false);
    }
  }

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

        <div className="library-dialog-body">
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

          <div className="edit-library-danger">
            {!confirmingDelete ? (
              <button
                className="nav-link nav-link-danger"
                type="button"
                data-testid="edit-library-delete"
                onClick={() => {
                  setError(null);
                  setConfirmingDelete(true);
                }}
                disabled={busy}
              >
                Delete library
              </button>
            ) : (
              <div className="edit-library-confirm">
                <span className="confirm-prompt">
                  Delete “{lib.name}” and its catalog? This can’t be undone.
                </span>
                <button
                  className="nav-link nav-link-danger"
                  type="button"
                  data-testid="edit-library-delete-confirm"
                  onClick={onDelete}
                  disabled={deleting}
                >
                  {deleting ? "Deleting…" : "Delete"}
                </button>
                <button
                  className="nav-link"
                  type="button"
                  data-testid="edit-library-delete-cancel"
                  onClick={() => setConfirmingDelete(false)}
                  disabled={deleting}
                >
                  Cancel
                </button>
              </div>
            )}
          </div>
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="auth-submit"
            type="button"
            data-testid="edit-library-close"
            onClick={onClose}
            disabled={deleting}
          >
            Close
          </button>
        </footer>
      </div>
    </dialog>
  );
}
