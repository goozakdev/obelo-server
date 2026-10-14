import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { ApiError } from "../api/errors";
import { errorMessage } from "../screens/errorMessage";
import type { Library, SeekPreviewStatus } from "../api/types";
import ConfirmDialog from "./ConfirmDialog";

// A Movie or TV Library's Seek preview controls (ADR-0070), shown in the Seek Previews
// tab of the Edit-Library dialog:
// the generation toggle, a coverage line ("7 of 10 Files have seek previews ·
// 2 failed"), a collapsible list of the Files whose generation failed — each with
// Retry, plus Retry all — and a Delete that removes every set of the Library and
// turns generation off in the same action (the confirm dialog says so).
//
// The status is read once on mount and re-read after every action. A 404 means the
// Library has no such toggle (music) and the panel renders nothing — absent, not
// off. The caller does not mount it for a linked mirror, whose previews are
// relayed from its origin. With no usable ffmpeg the toggle shows as unavailable,
// not on, whatever the stored setting says (as the Marker detection toggle does).

export default function SeekPreviewsPanel({ library }: { library: Library }) {
  const [status, setStatus] = useState<SeekPreviewStatus | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showFailed, setShowFailed] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  useEffect(() => {
    const ctl = new AbortController();
    apiClient
      .getSeekPreviews(library.id, ctl.signal)
      .then(setStatus)
      .catch(() => {
        // 404 (no toggle) or unreadable: render nothing rather than guess.
      });
    return () => ctl.abort();
  }, [library.id]);

  const refresh = useCallback(async () => {
    try {
      setStatus(await apiClient.getSeekPreviews(library.id));
    } catch (err) {
      // A Library that lost its toggle mid-session is simply gone from the tab.
      if (err instanceof ApiError && err.status === 404) setStatus(null);
      else setError(errorMessage(err));
    }
  }, [library.id]);

  async function run(action: () => Promise<unknown>) {
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await action();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
    await refresh();
  }

  async function onDelete() {
    if (busy) return;
    setBusy(true);
    setDeleteError(null);
    try {
      setStatus(await apiClient.deleteSeekPreviews(library.id));
      setConfirmingDelete(false);
    } catch (err) {
      setDeleteError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  if (!status) return null;
  const on = status.enabled && status.available;
  const failed = status.failed ?? [];

  return (
    <div className="admin-seek-previews" data-testid="seek-previews">
      <label
        title={
          status.available
            ? "Generate scrub thumbnails for every video File in the background"
            : "The server has no usable ffmpeg, so it cannot make seek previews"
        }
      >
        <input
          type="checkbox"
          data-testid="seek-previews-toggle"
          checked={on}
          disabled={!status.available || busy}
          onChange={() =>
            void run(async () =>
              setStatus(
                await apiClient.setSeekPreviews(library.id, !status.enabled),
              ),
            )
          }
        />{" "}
        Enable Seek Previews{status.available ? "" : ": Unavailable"}
      </label>
      <span
        className="admin-seek-previews-coverage"
        data-testid="seek-previews-coverage"
      >
        {status.ready} of {status.eligible} Files have seek previews
        {failed.length > 0 && ` · ${failed.length} failed`}
      </span>
      {failed.length > 0 && (
        <button
          type="button"
          className="button-secondary"
          data-testid="seek-previews-failed-toggle"
          aria-expanded={showFailed}
          onClick={() => setShowFailed((v) => !v)}
        >
          {showFailed ? "Hide failed" : "Show failed"}
        </button>
      )}
      <button
        type="button"
        className="button-secondary"
        data-testid="seek-previews-delete"
        disabled={busy}
        onClick={() => {
          setDeleteError(null);
          setConfirmingDelete(true);
        }}
      >
        Delete seek previews
      </button>

      {showFailed && failed.length > 0 && (
        <div
          className="admin-seek-previews-failed"
          data-testid="seek-previews-failed-list"
        >
          <div className="admin-seek-previews-failed-head">
            <span>
              {failed.length} failed {failed.length === 1 ? "File" : "Files"}
            </span>
            <button
              type="button"
              className="button-secondary"
              data-testid="seek-previews-retry-all"
              disabled={busy}
              onClick={() =>
                void run(() =>
                  apiClient.retrySeekPreviews(library.id, undefined),
                )
              }
            >
              Retry all
            </button>
          </div>
          <ul className="admin-seek-previews-list">
            {failed.map((f) => (
              <li key={f.fileId} data-testid="seek-previews-failed-item">
                <div className="admin-seek-previews-item-text">
                  <strong>{f.label}</strong>
                  <span className="admin-seek-previews-reason">{f.reason}</span>
                  <small className="admin-seek-previews-path">{f.path}</small>
                </div>
                <button
                  type="button"
                  className="button-secondary"
                  data-testid={`seek-previews-retry-${f.fileId}`}
                  aria-label={`Retry ${f.label}`}
                  disabled={busy}
                  onClick={() =>
                    void run(() =>
                      apiClient.retrySeekPreviews(library.id, f.fileId),
                    )
                  }
                >
                  Retry
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}

      {error && (
        <p
          className="status status-error"
          data-testid="seek-previews-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}

      {confirmingDelete && (
        <ConfirmDialog
          title="Delete seek previews"
          message={`This deletes every seek preview of “${library.name}” and turns generation off for it. Turn it back on to generate them again.`}
          confirmLabel="Delete"
          busyLabel="Deleting…"
          busy={busy}
          error={deleteError}
          onConfirm={() => void onDelete()}
          onCancel={() => {
            if (busy) return;
            setConfirmingDelete(false);
            setDeleteError(null);
          }}
        />
      )}
    </div>
  );
}
