import { useEffect, useRef } from "react";

// A small confirmation modal for destructive actions: a native <dialog> (so it
// traps focus and paints a backdrop) carrying a title, a message, and Cancel /
// Confirm buttons. The Confirm button is a danger action and shows a busy label
// while the caller's onConfirm is in flight; an optional error surfaces inline so
// a refused action keeps the dialog open instead of vanishing. Reuses the library
// dialogs' modal chrome (.library-dialog) so it feels native to the admin hub.
//
// Presentational only: the caller owns the action (the API call, the busy flag,
// the error). ESC / backdrop click both cancel — but only while not busy, so an
// in-flight action can't be dismissed out from under itself.

export default function ConfirmDialog({
  title,
  message,
  confirmLabel,
  busyLabel,
  busy = false,
  error = null,
  onConfirm,
  onCancel,
}: {
  title: string;
  message: string;
  confirmLabel: string;
  busyLabel: string;
  busy?: boolean;
  error?: string | null;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog confirm-dialog"
      data-testid="confirm-dialog"
      onCancel={(e) => {
        // ESC fires a native cancel; route it through onCancel, but never while busy.
        e.preventDefault();
        if (!busy) onCancel();
      }}
      onClose={onCancel}
      onClick={(e) => {
        if (e.target === dialogRef.current && !busy) onCancel();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">{title}</h2>
        </header>

        <div className="library-dialog-body">
          <p className="confirm-dialog-message" data-testid="confirm-dialog-message">
            {message}
          </p>
          {error && (
            <p className="auth-error" data-testid="confirm-dialog-error" role="alert">
              {error}
            </p>
          )}
        </div>

        <footer className="library-dialog-footer library-dialog-footer-end">
          <button
            className="nav-link"
            type="button"
            data-testid="confirm-dialog-cancel"
            onClick={onCancel}
            disabled={busy}
          >
            Cancel
          </button>
          <button
            className="nav-link nav-link-danger"
            type="button"
            data-testid="confirm-dialog-confirm"
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? busyLabel : confirmLabel}
          </button>
        </footer>
      </div>
    </dialog>
  );
}
