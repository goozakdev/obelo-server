import { useState } from "react";
import { apiClient, ApiError } from "../api/client";
import type { PlaylistSummary } from "../api/types";
import { errorMessage } from "../screens/errorMessage";

// AddToPlaylist is the "Add to playlist" affordance (collections-playlists-ui
// issue 03, PRD user stories 21–23) for ANY authenticated User (playlists are
// owner-private, so this is NOT admin-gated). A collapsed action that, when
// opened, lists the caller's own playlists (listPlaylists) plus a "new playlist"
// row, and appends THIS Title to the chosen one via appendPlaylistItem. Adding
// the same Title twice is allowed (two entries). A KIND_MISMATCH (a Title whose
// kind doesn't fit a typed playlist — e.g. a movie into a music playlist) renders
// a readable inline message NAMING the mismatch, built from the Title's kind and
// the playlist's kind, rather than a silent failure; an UNKNOWN_TITLE or transient
// failure surfaces the server's readable message. Creating a new playlist POSTs it
// (empty/untyped, so its first append never mismatches) then appends in one go.
//
// Shared (extracted from TitleDetailScreen) so the movie/episode detail and the
// music track detail both use the same affordance.
export default function AddToPlaylist({
  titleId,
  titleKind,
}: {
  titleId: string;
  titleKind: string;
}) {
  const [open, setOpen] = useState(false);
  const [playlists, setPlaylists] = useState<PlaylistSummary[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  // The "new playlist" sub-form.
  const [newName, setNewName] = useState("");
  const [creating, setCreating] = useState(false);

  async function loadPlaylists() {
    setLoading(true);
    setLoadError(null);
    try {
      setPlaylists(await apiClient.listPlaylists());
    } catch (err) {
      setLoadError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }

  function toggle() {
    if (open) {
      setOpen(false);
      return;
    }
    setOpen(true);
    setActionError(null);
    setSuccess(null);
    if (!playlists && !loading) void loadPlaylists();
  }

  async function addTo(playlist: PlaylistSummary) {
    if (busyId) return;
    setBusyId(playlist.id);
    setActionError(null);
    setSuccess(null);
    try {
      await apiClient.appendPlaylistItem(playlist.id, titleId);
      setSuccess(`Added to ${playlist.name}.`);
    } catch (err) {
      setActionError(appendErrorMessage(err, titleKind, playlist.kind));
    } finally {
      setBusyId(null);
    }
  }

  async function createAndAdd() {
    if (creating) return;
    const trimmed = newName.trim();
    if (!trimmed) {
      setActionError("Enter a playlist name.");
      return;
    }
    setCreating(true);
    setActionError(null);
    setSuccess(null);
    try {
      const created = await apiClient.createPlaylist(trimmed);
      // A new playlist is untyped, so its first append can't KIND_MISMATCH.
      await apiClient.appendPlaylistItem(created.id, titleId);
      setNewName("");
      setSuccess(`Added to ${created.name}.`);
      // Refresh the list so the new playlist shows for a subsequent add.
      await loadPlaylists();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setCreating(false);
    }
  }

  return (
    <div className="add-to-playlist" data-testid="add-to-playlist">
      <button
        className="nav-link"
        type="button"
        data-testid="add-to-playlist-button"
        aria-expanded={open}
        onClick={toggle}
      >
        Add to playlist
      </button>

      {open && (
        <div className="add-to-playlist-panel" data-testid="add-to-playlist-panel">
          {loading && (
            <p
              className="status status-loading"
              data-testid="add-to-playlist-loading"
            >
              Loading playlists&hellip;
            </p>
          )}

          {loadError && (
            <p
              className="status status-error"
              data-testid="add-to-playlist-load-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {loadError}{" "}
              <button
                className="nav-link"
                type="button"
                data-testid="add-to-playlist-retry"
                onClick={() => void loadPlaylists()}
              >
                Retry
              </button>
            </p>
          )}

          {!loading && !loadError && playlists && (
            <>
              {playlists.length > 0 && (
                <ul
                  className="add-to-playlist-list"
                  data-testid="add-to-playlist-list"
                >
                  {playlists.map((p) => (
                    <li key={p.id} className="add-to-playlist-item">
                      <button
                        className="nav-link"
                        type="button"
                        data-testid="playlist-option"
                        data-playlist-id={p.id}
                        disabled={busyId !== null || creating}
                        onClick={() => void addTo(p)}
                      >
                        {busyId === p.id ? `Adding to ${p.name}…` : p.name}
                      </button>
                    </li>
                  ))}
                </ul>
              )}

              <div className="add-to-playlist-new">
                <input
                  className="field-input"
                  data-testid="new-playlist-name-input"
                  type="text"
                  value={newName}
                  placeholder="New playlist name"
                  onChange={(e) => setNewName(e.target.value)}
                  disabled={creating || busyId !== null}
                />
                <button
                  className="nav-link"
                  type="button"
                  data-testid="create-and-add-playlist-button"
                  disabled={creating || busyId !== null}
                  onClick={() => void createAndAdd()}
                >
                  {creating ? "Creating…" : "New playlist + add"}
                </button>
              </div>
            </>
          )}

          {success && (
            <p
              className="status status-ok"
              data-testid="add-to-playlist-success"
              role="status"
            >
              {success}
            </p>
          )}

          {actionError && (
            <p
              className="status status-error"
              data-testid="add-to-playlist-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {actionError}
            </p>
          )}
        </div>
      )}
    </div>
  );
}

// A media-kind noun for a Title's kind (movie/episode/track) and for a playlist's
// kind (movie/tv/music), used to NAME a KIND_MISMATCH inline (PRD user story 22:
// "can't add a movie to a music playlist") rather than show the raw server text.
function titleKindNoun(kind: string): string {
  switch (kind) {
    case "movie":
      return "movie";
    case "episode":
      return "TV episode";
    case "track":
      return "music track";
    default:
      return "title";
  }
}

function playlistKindNoun(kind: string): string {
  switch (kind) {
    case "movie":
      return "movie";
    case "tv":
      return "TV";
    case "music":
      return "music";
    default:
      return kind || "other";
  }
}

// Turn an append failure into a readable message: a KIND_MISMATCH names the two
// kinds ("Can't add a movie to a music playlist."); anything else (UNKNOWN_TITLE,
// a transient failure) shows the server's readable message.
function appendErrorMessage(
  err: unknown,
  titleKind: string,
  playlistKind: string,
): string {
  if (err instanceof ApiError && err.code === "KIND_MISMATCH") {
    return `Can't add a ${titleKindNoun(titleKind)} to a ${playlistKindNoun(
      playlistKind,
    )} playlist.`;
  }
  return errorMessage(err);
}
