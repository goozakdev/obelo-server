import { useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library, ScanMode } from "../api/types";
import { useScanStatus } from "./useScanStatus";

// One library row in the admin hub (issue 06): identity (name / kind / roots),
// the scan controls (incremental + full), the polled scan-status indicator, and
// delete. The row owns its scan poller (useScanStatus) so each library tracks
// its own scan independently; triggering a scan seeds the poller from the
// trigger's response (`begin`), which starts polling iff the scan is still
// running and stops it once it settles (idle/error).

export default function LibraryAdminRow({
  library,
  onDeleted,
}: {
  library: Library;
  onDeleted: () => void;
}) {
  const scan = useScanStatus(library.id);
  const [scanning, setScanning] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  async function onScan(mode: ScanMode) {
    if (scanning) return;
    setScanning(true);
    setActionError(null);
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

  async function onDelete() {
    if (deleting) return;
    setDeleting(true);
    setActionError(null);
    try {
      await apiClient.deleteLibrary(library.id);
      onDeleted();
    } catch (err) {
      setActionError(errorMessage(err));
      setDeleting(false);
    }
  }

  const status = scan.status;
  const state = status?.state ?? "idle";
  // The scan trigger is asynchronous (202 + background scan), so `scanning` (the
  // in-flight POST) clears almost immediately while the scan keeps running. Keep
  // the controls disabled for the whole running scan so a second click can't
  // start a concurrent scan of the same Library.
  const scanRunning = scanning || state === "running";

  return (
    <li
      className="admin-library-row card"
      data-testid="admin-library-row"
      data-library-id={library.id}
    >
      <div className="admin-library-head">
        <span className="library-name" data-testid="admin-library-name">
          {library.name}
        </span>
        <span className="library-kind">{library.kind}</span>
      </div>

      <ul className="admin-library-roots" data-testid="admin-library-roots">
        {library.rootFolders.map((root) => (
          <li key={root.id} className="admin-library-root">
            {root.path}
          </li>
        ))}
      </ul>

      <div
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
        {state === "idle" && (
          <>
            <span className="dot dot-ok" aria-hidden="true" />
            Idle
          </>
        )}
        {state === "error" && (
          <>
            <span className="dot dot-error" aria-hidden="true" />
            Scan error{status?.errorMessage ? `: ${status.errorMessage}` : ""}
          </>
        )}
        {status && (
          <span className="scan-counts" data-testid="scan-counts">
            <span data-testid="scan-titles-found">{status.titlesFound}</span> titles,{" "}
            <span data-testid="scan-files-found">{status.filesFound}</span> files
          </span>
        )}
      </div>

      {scan.error && (
        <p className="status status-error" data-testid="scan-status-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {scan.error}
        </p>
      )}

      <div className="admin-library-actions">
        <button
          className="nav-link"
          type="button"
          data-testid="scan-button"
          onClick={() => onScan("incremental")}
          disabled={scanRunning || deleting}
        >
          {scanRunning ? "Scanning…" : "Scan"}
        </button>
        <button
          className="nav-link"
          type="button"
          data-testid="full-scan-button"
          onClick={() => onScan("full")}
          disabled={scanRunning || deleting}
        >
          Full scan
        </button>
        <button
          className="nav-link nav-logout"
          type="button"
          data-testid="delete-library-button"
          onClick={onDelete}
          disabled={deleting || scanRunning}
        >
          {deleting ? "Deleting…" : "Delete"}
        </button>
      </div>

      {actionError && (
        <p className="status status-error" data-testid="admin-action-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {actionError}
        </p>
      )}
    </li>
  );
}
