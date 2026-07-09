import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library, ScanMode } from "../api/types";
import { LibraryKindIcon } from "../browse/kindIcons";
import { EditIcon } from "../browse/ActionIcons";
import { useScanStatus } from "./useScanStatus";

// One Library row in the redesigned admin hub: its kind icon + name on the left,
// and a right-hand action cluster — the Edit affordance (a pencil that reveals on
// hover/focus, reusing the detail pages' EditIcon), then Scan and Full scan —
// alongside a compact scan-status indicator. The row still owns its own scan
// poller (useScanStatus) so each Library tracks its scan independently; a trigger
// seeds the poller from the response (`begin`), which polls only while the scan is
// running. Delete now lives in the Edit dialog, so the row itself carries no
// destructive control.
//
// `scanAllSignal` is a monotonically increasing counter the parent bumps when the
// Admin clicks "Scan All Libraries"; each increment makes every row kick off its
// own incremental scan (reusing this row's poller/`begin`), so the shared control
// stays reactive without the parent reaching into row state.

export default function LibraryAdminRow({
  library,
  onEdit,
  scanAllSignal = 0,
}: {
  library: Library;
  onEdit: (library: Library) => void;
  scanAllSignal?: number;
}) {
  const scan = useScanStatus(library.id);
  const [scanning, setScanning] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

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

  return (
    <li
      className="admin-library-row card"
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
          {status && state !== "error" && (
            <span className="scan-counts" data-testid="scan-counts">
              <span data-testid="scan-titles-found">{status.titlesFound}</span> titles,{" "}
              <span data-testid="scan-files-found">{status.filesFound}</span> files
            </span>
          )}
        </span>

        <div className="admin-library-actions">
          <button
            className="icon-button admin-library-edit"
            type="button"
            data-testid="edit-library-button"
            title="Edit library"
            aria-label={`Edit ${library.name}`}
            onClick={() => onEdit(library)}
          >
            <EditIcon />
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="scan-button"
            onClick={() => onScan("incremental")}
            disabled={scanRunning}
          >
            {scanRunning ? "Scanning…" : "Scan"}
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="full-scan-button"
            onClick={() => onScan("full")}
            disabled={scanRunning}
          >
            Full scan
          </button>
        </div>
      </div>

      {(scan.error || actionError) && (
        <p className="status status-error" data-testid="admin-action-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {actionError ?? scan.error}
        </p>
      )}
    </li>
  );
}
