import { useState, type FormEvent } from "react";
import { ApiError, apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";

// The create-library form (issue 06). An Admin names a library, picks its media
// kind (Movie / TV / Music — a Library holds exactly one kind, CONTEXT.md), and
// lists one or more root folders on the server's filesystem.
//
// The folder-overlap conflict (a root already owned by another Library, ADR-0002)
// comes back as a 409 FOLDER_OVERLAP ApiError, which the client deliberately does
// NOT swallow. We catch it here and render the server's readable message inline,
// flagging the overlap specifically (data-overlap) so the row is unmistakable and
// the form never crashes. Any other failure shows the same inline error slot.

const OVERLAP_CODE = "FOLDER_OVERLAP";

// The media kinds a Library can hold. The server validates the same set
// (library.Create); a Library holds exactly one kind.
const LIBRARY_KINDS = [
  { value: "movie", label: "Movies", placeholder: "/media/movies" },
  { value: "tv", label: "TV", placeholder: "/media/tv" },
  { value: "music", label: "Music", placeholder: "/media/music" },
] as const;

export default function CreateLibraryForm({
  onCreated,
}: {
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState<string>("movie");
  // At least one root folder row; the Admin can add/remove more.
  const [roots, setRoots] = useState<string[]>([""]);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<{ message: string; overlap: boolean } | null>(
    null,
  );

  function setRoot(index: number, value: string) {
    setRoots((prev) => prev.map((r, i) => (i === index ? value : r)));
  }
  function addRoot() {
    setRoots((prev) => [...prev, ""]);
  }
  function removeRoot(index: number) {
    setRoots((prev) => (prev.length <= 1 ? prev : prev.filter((_, i) => i !== index)));
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting) return;
    const trimmedName = name.trim();
    const folders = roots.map((r) => r.trim()).filter((r) => r.length > 0);
    if (!trimmedName || folders.length === 0) {
      setError({
        message: "Enter a library name and at least one root folder.",
        overlap: false,
      });
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.createLibrary({
        name: trimmedName,
        kind,
        rootFolders: folders,
      });
      // Reset to a fresh form, then let the hub reload the list.
      setName("");
      setKind("movie");
      setRoots([""]);
      onCreated();
    } catch (err) {
      const overlap = err instanceof ApiError && err.code === OVERLAP_CODE;
      setError({ message: errorMessage(err), overlap });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <form
      className="card create-library-form"
      data-testid="create-library-form"
      onSubmit={onSubmit}
    >
      <h2 className="card-title">Create a library</h2>

      <div className="field">
        <label className="field-label" htmlFor="library-name">
          Name
        </label>
        <input
          id="library-name"
          className="field-input"
          data-testid="library-name-input"
          type="text"
          value={name}
          placeholder="Movies"
          onChange={(e) => setName(e.target.value)}
          disabled={submitting}
        />
      </div>

      <div className="field">
        <label className="field-label" htmlFor="library-kind">
          Kind
        </label>
        <select
          id="library-kind"
          className="field-input"
          data-testid="library-kind-select"
          value={kind}
          onChange={(e) => setKind(e.target.value)}
          disabled={submitting}
        >
          {LIBRARY_KINDS.map((k) => (
            <option key={k.value} value={k.value}>
              {k.label}
            </option>
          ))}
        </select>
      </div>

      <fieldset className="field root-folders">
        <legend className="field-label">Root folders</legend>
        {roots.map((root, i) => (
          <div className="root-folder-row" key={i} data-testid="root-folder-row">
            <input
              className="field-input"
              data-testid="root-folder-input"
              type="text"
              value={root}
              placeholder={
                LIBRARY_KINDS.find((k) => k.value === kind)?.placeholder ??
                "/media/movies"
              }
              onChange={(e) => setRoot(i, e.target.value)}
              disabled={submitting}
            />
            {roots.length > 1 && (
              <button
                className="nav-link"
                type="button"
                data-testid="remove-root-button"
                onClick={() => removeRoot(i)}
                disabled={submitting}
                aria-label="Remove root folder"
              >
                Remove
              </button>
            )}
          </div>
        ))}
        <button
          className="nav-link add-root-button"
          type="button"
          data-testid="add-root-button"
          onClick={addRoot}
          disabled={submitting}
        >
          + Add another folder
        </button>
      </fieldset>

      {error && (
        <p
          className="auth-error"
          data-testid="create-library-error"
          data-overlap={error.overlap ? "true" : undefined}
          role="alert"
        >
          {error.message}
        </p>
      )}

      <button
        className="auth-submit"
        data-testid="create-library-submit"
        type="submit"
        disabled={submitting}
      >
        {submitting ? "Creating…" : "Create library"}
      </button>
    </form>
  );
}
