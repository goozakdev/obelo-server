import { useState, type FormEvent } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";

// The Admin "New collection" action on the Collections list (collections-
// playlists-ui issue 02 / PRD user story 9). A collapsed action button reveals an
// inline form — a required name + an optional description — that POSTs a new
// Collection and asks the list to reload so the new card appears (the create
// response carries no per-viewer count/poster, so the list refetch is what
// reflects server truth).
//
// Only an Admin ever renders this (the parent gates it on useAuth().isAdmin; the
// server enforces the scope regardless). A blank name is caught client-side; a
// server 400 BAD_REQUEST (and any other failure) surfaces inline without crashing
// the form, exactly as the library create/edit flow branches on FOLDER_OVERLAP.

export default function CreateCollectionForm({
  onCreated,
}: {
  onCreated: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function close() {
    setOpen(false);
    setName("");
    setDescription("");
    setError(null);
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting) return;
    const trimmedName = name.trim();
    if (!trimmedName) {
      setError("Enter a collection name.");
      return;
    }
    const trimmedDesc = description.trim();
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.createCollection({
        name: trimmedName,
        ...(trimmedDesc ? { description: trimmedDesc } : {}),
      });
      // Reset and collapse, then let the list reload so the new card appears.
      setName("");
      setDescription("");
      setOpen(false);
      onCreated();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSubmitting(false);
    }
  }

  if (!open) {
    return (
      <div className="collections-curation" data-testid="collections-curation">
        <button
          className="auth-submit"
          type="button"
          data-testid="new-collection-button"
          onClick={() => setOpen(true)}
        >
          New collection
        </button>
      </div>
    );
  }

  return (
    <form
      className="card create-collection-form"
      data-testid="create-collection-form"
      onSubmit={onSubmit}
    >
      <h2 className="card-title">Create a collection</h2>

      <div className="field">
        <label className="field-label" htmlFor="collection-name">
          Name
        </label>
        <input
          id="collection-name"
          className="field-input"
          data-testid="collection-name-input"
          type="text"
          value={name}
          placeholder="A24 Films"
          onChange={(e) => setName(e.target.value)}
          disabled={submitting}
        />
      </div>

      <div className="field">
        <label className="field-label" htmlFor="collection-description">
          Description <span className="field-optional">(optional)</span>
        </label>
        <textarea
          id="collection-description"
          className="field-input"
          data-testid="collection-description-input"
          rows={2}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          disabled={submitting}
        />
      </div>

      {error && (
        <p className="auth-error" data-testid="create-collection-error" role="alert">
          {error}
        </p>
      )}

      <div className="form-actions">
        <button
          className="auth-submit"
          data-testid="create-collection-submit"
          type="submit"
          disabled={submitting}
        >
          {submitting ? "Creating…" : "Create collection"}
        </button>
        <button
          className="nav-link"
          type="button"
          data-testid="create-collection-cancel"
          onClick={close}
          disabled={submitting}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
