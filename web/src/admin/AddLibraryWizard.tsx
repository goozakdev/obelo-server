import { useEffect, useRef, useState } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { Library } from "../api/types";
import { LIBRARY_KIND_META } from "../browse/kindIcons";

// The Add-Library wizard: a native <dialog> (showModal → ESC / top-layer / focus
// containment for free) that walks the Admin through three pages —
//   1. pick the media kind, by clicking one of the kind icons (the same glyphs the
//      nav and rows use), each captioned with its label (TV / Movies / Music);
//   2. name the Library;
//   3. give it a root folder path — the final button reads "Add".
// On Add it POSTs the Library and, on success, hands the created Library back to
// the hub (which reloads the list) and closes. A folder-overlap conflict comes
// back as a 409 FOLDER_OVERLAP, rendered inline on the path page exactly as the
// old create form did (data-overlap flags it) rather than crashing the wizard.
//
// A Library holds exactly one kind and one-or-more roots (CONTEXT.md); the wizard
// creates it with a single root, and the Edit dialog adds more later.

const OVERLAP_CODE = "FOLDER_OVERLAP";

type Step = 1 | 2 | 3;

export default function AddLibraryWizard({
  onCreated,
  onClose,
}: {
  /** Called with the created Library after a successful Add; the hub reloads. */
  onCreated: (library: Library) => void;
  /** Close the wizard without creating (ESC, backdrop, ✕, or Cancel). */
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [step, setStep] = useState<Step>(1);
  const [kind, setKind] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [path, setPath] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<{ message: string; overlap: boolean } | null>(
    null,
  );

  // Open the native modal on mount and keep React's lifecycle in sync with the
  // dialog's own close (ESC) via onClose.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);

  const trimmedName = name.trim();
  const trimmedPath = path.trim();

  function next() {
    setError(null);
    setStep((s) => (s === 1 ? 2 : 3));
  }
  function back() {
    setError(null);
    setStep((s) => (s === 3 ? 2 : 1));
  }

  async function onAdd() {
    if (submitting || !kind || !trimmedName || !trimmedPath) return;
    setSubmitting(true);
    setError(null);
    try {
      const created = await apiClient.createLibrary({
        name: trimmedName,
        kind,
        rootFolders: [trimmedPath],
      });
      onCreated(created);
    } catch (err) {
      const overlap = err instanceof ApiError && err.code === OVERLAP_CODE;
      setError({ message: errorMessage(err), overlap });
      setSubmitting(false);
    }
  }

  return (
    <dialog
      ref={dialogRef}
      className="library-dialog"
      data-testid="add-library-dialog"
      onClose={onClose}
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="library-dialog-panel">
        <header className="library-dialog-header">
          <h2 className="library-dialog-title">Add library</h2>
          <button
            className="nav-link library-dialog-close"
            type="button"
            data-testid="add-library-close"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </header>

        <div className="library-dialog-body">
          {step === 1 && (
            <div className="wizard-page" data-testid="add-library-step-kind">
              <p className="wizard-prompt">What kind of library is this?</p>
              <div className="kind-picker" role="radiogroup" aria-label="Library kind">
                {LIBRARY_KIND_META.map(({ kind: k, label, Icon }) => {
                  const selected = kind === k;
                  return (
                    <button
                      key={k}
                      type="button"
                      className={`kind-option${selected ? " is-selected" : ""}`}
                      role="radio"
                      aria-checked={selected}
                      data-testid={`add-library-kind-${k}`}
                      onClick={() => setKind(k)}
                    >
                      <Icon className="kind-option-icon" />
                      <span className="kind-option-label">{label}</span>
                    </button>
                  );
                })}
              </div>
            </div>
          )}

          {step === 2 && (
            <div className="wizard-page" data-testid="add-library-step-name">
              <label className="field-label" htmlFor="add-library-name">
                Library name
              </label>
              <input
                id="add-library-name"
                className="field-input"
                data-testid="add-library-name-input"
                type="text"
                value={name}
                placeholder="Movies"
                autoFocus
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && trimmedName) next();
                }}
              />
            </div>
          )}

          {step === 3 && (
            <div className="wizard-page" data-testid="add-library-step-path">
              <label className="field-label" htmlFor="add-library-path">
                Root folder path
              </label>
              <input
                id="add-library-path"
                className="field-input"
                data-testid="add-library-path-input"
                type="text"
                value={path}
                placeholder="/media/movies"
                autoFocus
                onChange={(e) => setPath(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && trimmedPath) void onAdd();
                }}
              />
              {error && (
                <p
                  className="auth-error"
                  data-testid="add-library-error"
                  data-overlap={error.overlap ? "true" : undefined}
                  role="alert"
                >
                  {error.message}
                </p>
              )}
            </div>
          )}
        </div>

        <footer className="library-dialog-footer">
          {step > 1 ? (
            <button
              className="nav-link"
              type="button"
              data-testid="add-library-back"
              onClick={back}
              disabled={submitting}
            >
              Back
            </button>
          ) : (
            <span />
          )}

          {step < 3 ? (
            <button
              className="auth-submit"
              type="button"
              data-testid="add-library-next"
              onClick={next}
              disabled={step === 1 ? !kind : !trimmedName}
            >
              Next
            </button>
          ) : (
            <button
              className="auth-submit"
              type="button"
              data-testid="add-library-submit"
              onClick={onAdd}
              disabled={submitting || !trimmedPath}
            >
              {submitting ? "Adding…" : "Add"}
            </button>
          )}
        </footer>
      </div>
    </dialog>
  );
}
