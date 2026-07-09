import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { PlaylistSummary } from "../api/types";
import AppHeader from "./AppHeader";
import CreatePlaylistForm from "./CreatePlaylistForm";

// The caller's Playlists list (collections-playlists-ui issue 03 / PRD user
// stories 17, 19–20): GET /playlists rendered as a list of the caller's OWN
// playlists (each with its item count), with create / rename / delete. The
// server returns only the caller's own playlists (another User's never appear),
// so the screen renders exactly what it gets; an empty list is a clean empty
// state.
//
// Owner-private — NOT role-gated like Collections curation: every authenticated
// User manages their own playlists (there is no Admin override; a non-owned
// playlist simply 404s on its detail). Because a create/rename/delete mutates the
// very list shown, the screen loads via a reloadable loader (not the load-once
// useAsync) so it can refetch after a write and reflect the server's truth.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; playlists: PlaylistSummary[] };

export default function PlaylistsScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });

  const load = useCallback(async (signal?: AbortSignal) => {
    setState({ status: "loading" });
    try {
      const playlists = await apiClient.listPlaylists(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", playlists });
    } catch (err) {
      if (signal?.aborted) return;
      setState({ status: "error", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  const reload = useCallback(() => void load(), [load]);

  return (
    <div className="app-shell" data-testid="playlists-screen">
      <AppHeader />
      <main className="app-main app-main-wide">
        <h2 className="section-title">Playlists</h2>

        {/* Every User has their own playlists, so the "New playlist" action is
            always available (not role-gated). */}
        <CreatePlaylistForm onCreated={reload} />

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="playlists-loading">
            Loading playlists&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p
            className="status status-error"
            data-testid="playlists-error"
            role="alert"
          >
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && state.playlists.length === 0 && (
          <div className="card" data-testid="playlists-empty">
            <p className="status status-loading">
              No playlists yet. Create one above, or add a Title from its page.
            </p>
          </div>
        )}

        {state.status === "ready" && state.playlists.length > 0 && (
          <ul className="playlist-list" data-testid="playlists">
            {state.playlists.map((p) => (
              <PlaylistRow key={p.id} playlist={p} onChanged={reload} />
            ))}
          </ul>
        )}
      </main>
    </div>
  );
}

// One row in the Playlists list: the playlist's name (linking to its detail), its
// item count, and — owner-private, so for every User — inline rename and a
// two-step delete. Each write shows a pending state and surfaces a refused write
// (e.g. a blank name → 400 BAD_REQUEST, or a transient failure) as a readable
// inline message without losing the row; on success it asks the parent to refetch
// the list so the name/count (or the row's absence) reflects server truth. Each
// row owns its own rename/delete state.
function PlaylistRow({
  playlist,
  onChanged,
}: {
  playlist: PlaylistSummary;
  onChanged: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(playlist.name);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const [confirming, setConfirming] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  function openEditor() {
    setName(playlist.name);
    setSaveError(null);
    setEditing(true);
  }

  async function onSave() {
    if (saving) return;
    const trimmed = name.trim();
    if (!trimmed) {
      setSaveError("Enter a playlist name.");
      return;
    }
    setSaving(true);
    setSaveError(null);
    try {
      await apiClient.renamePlaylist(playlist.id, trimmed);
      setEditing(false);
      onChanged();
    } catch (err) {
      setSaveError(errorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  async function onConfirmDelete() {
    if (deleting) return;
    setDeleting(true);
    setDeleteError(null);
    try {
      await apiClient.deletePlaylist(playlist.id);
      // On success the parent refetch drops this row; no need to clear state.
      onChanged();
    } catch (err) {
      setDeleteError(errorMessage(err));
      setDeleting(false);
      setConfirming(false);
    }
  }

  return (
    <li
      className="playlist-row"
      data-testid="playlist-row"
      data-playlist-id={playlist.id}
    >
      {!editing ? (
        <div className="playlist-row-main">
          <Link
            className="playlist-link"
            to={`/playlists/${playlist.id}`}
            data-testid="playlist-item"
          >
            <span className="playlist-name" data-testid="playlist-name">
              {playlist.name}
            </span>
            <span className="playlist-count" data-testid="playlist-count">
              {playlist.itemCount} {playlist.itemCount === 1 ? "item" : "items"}
            </span>
          </Link>

          <div className="playlist-row-actions">
            <button
              className="nav-link"
              type="button"
              data-testid="rename-playlist-button"
              onClick={openEditor}
            >
              Rename
            </button>

            {!confirming ? (
              <button
                className="nav-link nav-logout"
                type="button"
                data-testid="delete-playlist-button"
                onClick={() => {
                  setDeleteError(null);
                  setConfirming(true);
                }}
                disabled={deleting}
              >
                Delete
              </button>
            ) : (
              <span
                className="playlist-delete-confirm"
                data-testid="delete-playlist-confirm"
              >
                <span className="confirm-prompt">Delete {playlist.name}?</span>
                <button
                  className="nav-link nav-logout"
                  type="button"
                  data-testid="delete-playlist-confirm-button"
                  onClick={onConfirmDelete}
                  disabled={deleting}
                >
                  {deleting ? "Deleting…" : "Confirm delete"}
                </button>
                <button
                  className="nav-link"
                  type="button"
                  data-testid="delete-playlist-cancel-button"
                  onClick={() => setConfirming(false)}
                  disabled={deleting}
                >
                  Cancel
                </button>
              </span>
            )}
          </div>
        </div>
      ) : (
        <div className="playlist-rename-form" data-testid="playlist-rename-form">
          <input
            className="field-input"
            data-testid="rename-playlist-input"
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={saving}
          />
          <button
            className="auth-submit"
            type="button"
            data-testid="save-playlist-button"
            onClick={onSave}
            disabled={saving}
          >
            {saving ? "Saving…" : "Save"}
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="cancel-playlist-rename-button"
            onClick={() => setEditing(false)}
            disabled={saving}
          >
            Cancel
          </button>
        </div>
      )}

      {saveError && (
        <p
          className="status status-error"
          data-testid="rename-playlist-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {saveError}
        </p>
      )}
      {deleteError && (
        <p
          className="status status-error"
          data-testid="delete-playlist-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {deleteError}
        </p>
      )}
    </li>
  );
}
