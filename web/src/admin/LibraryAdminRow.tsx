import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library, ScanMode } from "../api/types";
import { LibraryKindIcon } from "../browse/kindIcons";
import { useScanStatus } from "./useScanStatus";
import ConfirmDialog from "./ConfirmDialog";

// One Library row in the redesigned admin hub: its kind icon + name on the left,
// and a right-hand action cluster — a "three dots" (⋮) menu alongside a compact
// scan-status indicator. The menu (same click-outside / Escape dropdown as the
// browse EpisodeActionsMenu) gathers the row's actions: Edit, Scan, Full scan,
// Refresh missing metadata, Refresh all metadata, and a destructive Delete.
// Scan/Full scan/the two refresh actions stay disabled
// for the whole running scan so a second pick can't start a concurrent scan; the
// row still owns its own scan poller (useScanStatus) so each Library tracks its
// scan independently, and a trigger seeds the poller from the response (`begin`),
// which polls only while the scan is running.
//
// The two refresh actions START an Enrichment pass (ADR-0062) exactly like Scan
// starts a scan — a POST that returns as soon as the pass is queued, never
// awaited by this row. "Refresh missing metadata" (mode `missing`) starts at
// once; "Refresh all metadata" (mode `full`) goes through the same ConfirmDialog
// Delete uses, because a full re-fetch of every item can take a long time. A
// `started: false` ack (a pass was already running) and a request error both
// surface as a visible inline message — there is no client-side wait for either
// pass to finish.
//
// Delete is the row's single destructive path (the Edit dialog carries no delete):
// picking it opens a confirmation modal (ConfirmDialog) — deleting a Library and
// its catalog is irreversible, so it's never a stray click. On success the row
// tells the hub the Library is gone (`onDeleted`); a refused delete keeps the
// dialog open with a readable inline message. The per-Library Enrichment policy
// (the "Metadata Providers" tab) lives in the Edit dialog.
//
// `scanAllSignal` is a monotonically increasing counter the parent bumps when the
// Admin clicks "Scan All Libraries"; each increment makes every row kick off its
// own incremental scan (reusing this row's poller/`begin`), so the shared control
// stays reactive without the parent reaching into row state.
//
// This row is only ever a LOCAL Library: the hub filters mirrors out of the list
// entirely (issue 16), so there is no linked branch here and no disabled cluster.
// A linked Library (ADR-0056 §1) refuses every writer below with 409
// LINKED_LIBRARY; the one write it does take, a rename, lives with the rest of
// its per-Link actions on Settings → Linked servers.

export default function LibraryAdminRow({
  library,
  onEdit,
  onDeleted,
  scanAllSignal = 0,
}: {
  library: Library;
  onEdit: (library: Library) => void;
  /** Called after a successful delete; the hub reloads its list. */
  onDeleted: () => void;
  scanAllSignal?: number;
}) {
  const scan = useScanStatus(library.id);
  const [scanning, setScanning] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  const [confirmingRefreshAll, setConfirmingRefreshAll] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  const status = scan.status;
  const state = status?.state ?? "idle";
  // The scan trigger is asynchronous (202 + background scan), so `scanning` (the
  // in-flight POST) clears almost immediately while the scan keeps running. Keep
  // the controls disabled for the whole running scan so a second click can't
  // start a concurrent scan of the same Library.
  const scanRunning = scanning || state === "running";

  async function onScan(mode: ScanMode) {
    if (scanRunning) return;
    setScanning(true);
    setActionError(null);
    setRefreshError(null);
    try {
      const status = await apiClient.scanLibrary(library.id, { mode });
      // Seed the poller; it keeps polling only while the scan is still running.
      scan.begin(status);
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setScanning(false);
    }
  }

  // Refresh missing / Refresh all metadata (ADR-0062): a POST /enrich, started
  // and never awaited — exactly like onScan above. `started: false` means a pass
  // was already running for this Library, which is a normal outcome (not an
  // error) but still worth a visible message so the click doesn't feel ignored.
  async function onRefresh(mode: "missing" | "full") {
    if (scanRunning || refreshing) return;
    setRefreshing(true);
    setRefreshError(null);
    try {
      const state = await apiClient.enrichLibrary(library.id, { mode });
      if (!state.started) {
        setRefreshError(`A metadata pass is already running for “${library.name}”.`);
      }
    } catch (err) {
      setRefreshError(errorMessage(err));
    } finally {
      setRefreshing(false);
    }
  }

  async function onDelete() {
    if (deleting) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await apiClient.deleteLibrary(library.id);
      onDeleted();
    } catch (err) {
      // Keep the confirm dialog open with a readable message on refusal.
      setDeleteError(errorMessage(err));
      setDeleting(false);
    }
  }

  // "Scan All": when the parent bumps the signal, this row triggers its own
  // incremental scan. Guard on >0 so the initial render (signal 0) is a no-op,
  // and keep the trigger out of the effect deps via a ref so only a genuine
  // signal change fires it.
  const onScanRef = useRef(onScan);
  onScanRef.current = onScan;
  useEffect(() => {
    if (scanAllSignal > 0) void onScanRef.current("incremental");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scanAllSignal]);

  // Close the actions menu on outside click / Escape (mirrors EpisodeActionsMenu).
  useEffect(() => {
    if (!menuOpen) return;
    function onDocPointer(e: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setMenuOpen(false);
      }
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setMenuOpen(false);
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [menuOpen]);

  return (
    <li
      className="admin-library-row admin-panel-row"
      data-testid="admin-library-row"
      data-library-id={library.id}
    >
      <div className="admin-library-identity">
        <span className="admin-library-icon" aria-hidden="true">
          <LibraryKindIcon kind={library.kind} className="admin-library-kind-icon" />
        </span>
        <span className="admin-library-name" data-testid="admin-library-name">
          {library.name}
        </span>
      </div>

      <div className="admin-library-aside">
        <span
          className={`scan-status scan-state-${state}`}
          data-testid="scan-status"
          data-state={state}
          role="status"
        >
          {state === "running" && (
            <>
              <span className="dot dot-loading" aria-hidden="true" />
              Scanning&hellip;
            </>
          )}
          {state === "error" && (
            <>
              <span className="dot dot-error" aria-hidden="true" />
              Scan error{status?.errorMessage ? `: ${status.errorMessage}` : ""}
            </>
          )}
          {status && state !== "error" && state !== "running" && (
            <span className="scan-counts" data-testid="scan-counts">
              <span data-testid="scan-title-count">{status.titleCount}</span>{" "}
              {status.titleCount === 1 ? "title" : "titles"}
            </span>
          )}
        </span>

        {/* Keep the cluster visible while the menu is open, so moving the mouse off
            the row doesn't collapse an open dropdown (`.admin-row-actions`
            reveals on hover/focus otherwise). */}
        <div className={`admin-row-actions${menuOpen ? " is-active" : ""}`}>
          <div className="row-menu admin-library-menu" ref={menuRef}>
            <button
              type="button"
              className="row-menu-toggle"
              data-testid="library-menu-toggle"
              aria-haspopup="menu"
              aria-expanded={menuOpen}
              aria-label={`More actions for ${library.name}`}
              onClick={() => setMenuOpen((v) => !v)}
            >
              ⋮
            </button>
            {menuOpen && (
              <ul className="row-menu-list" role="menu" data-testid="library-menu">
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button"
                    role="menuitem"
                    data-testid="edit-library-button"
                    onClick={() => {
                      setMenuOpen(false);
                      onEdit(library);
                    }}
                  >
                    Edit
                  </button>
                </li>
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button"
                    role="menuitem"
                    data-testid="scan-button"
                    disabled={scanRunning}
                    onClick={() => {
                      setMenuOpen(false);
                      void onScan("incremental");
                    }}
                  >
                    {scanRunning ? "Scanning…" : "Scan"}
                  </button>
                </li>
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button"
                    role="menuitem"
                    data-testid="full-scan-button"
                    disabled={scanRunning}
                    onClick={() => {
                      setMenuOpen(false);
                      void onScan("full");
                    }}
                  >
                    Full scan
                  </button>
                </li>
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button"
                    role="menuitem"
                    data-testid="refresh-missing-button"
                    disabled={scanRunning}
                    onClick={() => {
                      setMenuOpen(false);
                      void onRefresh("missing");
                    }}
                  >
                    Refresh missing metadata
                  </button>
                </li>
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button"
                    role="menuitem"
                    data-testid="refresh-all-button"
                    disabled={scanRunning}
                    onClick={() => {
                      setMenuOpen(false);
                      setRefreshError(null);
                      setConfirmingRefreshAll(true);
                    }}
                  >
                    Refresh all metadata
                  </button>
                </li>
                <li className="row-menu-item" role="none">
                  <button
                    type="button"
                    className="row-menu-button row-menu-button-danger"
                    role="menuitem"
                    data-testid="delete-library-button"
                    onClick={() => {
                      setMenuOpen(false);
                      setDeleteError(null);
                      setConfirmingDelete(true);
                    }}
                  >
                    Delete
                  </button>
                </li>
              </ul>
            )}
          </div>
        </div>
      </div>

      {(scan.error || actionError || refreshError) && (
        <p className="status status-error" data-testid="admin-action-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {actionError ?? refreshError ?? scan.error}
        </p>
      )}

      {confirmingRefreshAll && (
        <ConfirmDialog
          title="Refresh all metadata"
          message={`Re-fetch metadata for every item in “${library.name}” from the provider? This may take a long time.`}
          confirmLabel="Refresh all"
          busyLabel="Starting…"
          busy={refreshing}
          onConfirm={() => {
            setConfirmingRefreshAll(false);
            void onRefresh("full");
          }}
          onCancel={() => setConfirmingRefreshAll(false)}
        />
      )}

      {confirmingDelete && (
        <ConfirmDialog
          title="Delete library"
          message={`Delete “${library.name}” and its catalog? This can’t be undone.`}
          confirmLabel="Delete"
          busyLabel="Deleting…"
          busy={deleting}
          error={deleteError}
          onConfirm={onDelete}
          onCancel={() => {
            if (deleting) return;
            setConfirmingDelete(false);
            setDeleteError(null);
          }}
        />
      )}
    </li>
  );
}
