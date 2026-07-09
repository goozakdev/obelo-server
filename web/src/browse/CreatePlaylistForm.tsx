import { useState, type FormEvent } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";

// The "New playlist" action on the Playlists list (collections-playlists-ui
// issue 03 / PRD user story 19). A collapsed action button reveals an inline
// form — a single required name — that POSTs an empty, untyped playlist and asks
// the list to reload so the new row appears (the create response carries no item
// count, so the list refetch is what reflects server truth).
//
// Owner-private: EVERY authenticated User has their own playlists, so this is NOT
// role-gated (unlike the Admin "New collection" action). A blank name is caught
// client-side; a server 400 BAD_REQUEST (and any other failure) surfaces inline
// without crashing the form.

export default function CreatePlaylistForm({
  onCreated,
}: {
  onCreated: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function close() {
    setOpen(false);
    setName("");
    setError(null);
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (submitting) return;
    const trimmed = name.trim();
    if (!trimmed) {
      setError("Enter a playlist name.");
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await apiClient.createPlaylist(trimmed);
      // Reset and collapse, then let the list reload so the new row appears.
      setName("");
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
      <div className="playlists-management" data-testid="playlists-management">
        <button
          className="auth-submit"
          type="button"
          data-testid="new-playlist-button"
          onClick={() => setOpen(true)}
        >
          New playlist
        </button>
      </div>
    );
  }

  return (
    <form
      className="card create-playlist-form"
      data-testid="create-playlist-form"
      onSubmit={onSubmit}
    >
      <h2 className="card-title">Create a playlist</h2>

      <div className="field">
        <label className="field-label" htmlFor="playlist-name">
          Name
        </label>
        <input
          id="playlist-name"
          className="field-input"
          data-testid="playlist-name-input"
          type="text"
          value={name}
          placeholder="Watch later"
          onChange={(e) => setName(e.target.value)}
          disabled={submitting}
        />
      </div>

      {error && (
        <p className="auth-error" data-testid="create-playlist-error" role="alert">
          {error}
        </p>
      )}

      <div className="form-actions">
        <button
          className="auth-submit"
          data-testid="create-playlist-submit"
          type="submit"
          disabled={submitting}
        >
          {submitting ? "Creating…" : "Create playlist"}
        </button>
        <button
          className="nav-link"
          type="button"
          data-testid="create-playlist-cancel"
          onClick={close}
          disabled={submitting}
        >
          Cancel
        </button>
      </div>
    </form>
  );
}
