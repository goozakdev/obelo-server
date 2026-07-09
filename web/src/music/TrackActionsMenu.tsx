import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import type { PlaylistSummary } from "../api/types";

// The per-row "three dots" actions menu on the Album track list. A click opens a
// dropdown (same click-outside / Escape pattern as the header's NavDropdown) with:
//   1) Add to playlist → a submenu of the caller's addable playlists
//   2) Play next        → insert after the now-playing entry
//   3) Add to queue      → append at the end
//   4) Edit              → open the track view (for now)
// Actions 2–4 are provided by the parent (they drive the shared Queue / router);
// this component owns only the playlist listing + append, lazily fetched the first
// time the menu opens.

// A playlist is addable from the music side when it's a music playlist OR still
// untyped (its first item fixes the kind); a movie/tv playlist would be rejected.
function isAddable(pl: PlaylistSummary): boolean {
  return pl.kind === "music" || pl.kind === "";
}

export default function TrackActionsMenu({
  trackId,
  trackTitle,
  onPlayNext,
  onAddToQueue,
  onEdit,
  onNotice,
}: {
  trackId: string;
  trackTitle: string;
  onPlayNext: () => void;
  onAddToQueue: () => void;
  onEdit: () => void;
  onNotice: (message: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [submenuOpen, setSubmenuOpen] = useState(false);
  const [playlists, setPlaylists] = useState<PlaylistSummary[] | null>(null);
  const ref = useRef<HTMLDivElement>(null);

  // Close on outside click / Escape (mirrors the header NavDropdown).
  useEffect(() => {
    if (!open) return;
    function onDocPointer(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) close();
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") close();
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  // Lazily load the caller's playlists the first time the menu opens.
  useEffect(() => {
    if (!open || playlists !== null) return;
    const ctrl = new AbortController();
    apiClient
      .listPlaylists(ctrl.signal)
      .then((pls) => setPlaylists(pls))
      .catch(() => {
        if (!ctrl.signal.aborted) setPlaylists([]);
      });
    return () => ctrl.abort();
  }, [open, playlists]);

  function close() {
    setOpen(false);
    setSubmenuOpen(false);
  }

  async function addToPlaylist(pl: PlaylistSummary) {
    close();
    try {
      await apiClient.appendPlaylistItem(pl.id, trackId);
      onNotice(`Added “${trackTitle}” to ${pl.name}.`);
    } catch {
      onNotice(`Couldn't add “${trackTitle}” to ${pl.name}.`);
    }
  }

  const addable = (playlists ?? []).filter(isAddable);

  return (
    <div className="track-menu" ref={ref}>
      <button
        type="button"
        className="track-menu-toggle"
        data-testid="track-menu-toggle"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`More actions for ${trackTitle}`}
        onClick={() => setOpen((v) => !v)}
      >
        ⋮
      </button>
      {open && (
        <ul className="track-menu-list" role="menu" data-testid="track-menu">
          <li className="track-menu-item track-menu-parent" role="none">
            <button
              type="button"
              className="track-menu-button"
              role="menuitem"
              aria-haspopup="menu"
              aria-expanded={submenuOpen}
              data-testid="track-menu-add-playlist"
              onClick={() => setSubmenuOpen((v) => !v)}
            >
              Add to playlist
              <span className="track-menu-caret" aria-hidden="true">
                ▸
              </span>
            </button>
            {submenuOpen && (
              <ul className="track-submenu" role="menu" data-testid="track-submenu">
                {playlists === null ? (
                  <li className="track-menu-item" role="none">
                    <span className="track-menu-empty">Loading…</span>
                  </li>
                ) : addable.length === 0 ? (
                  <li className="track-menu-item" role="none">
                    <span className="track-menu-empty">No playlists</span>
                  </li>
                ) : (
                  addable.map((pl) => (
                    <li className="track-menu-item" role="none" key={pl.id}>
                      <button
                        type="button"
                        className="track-menu-button"
                        role="menuitem"
                        data-testid="track-menu-playlist-option"
                        data-playlist-id={pl.id}
                        onClick={() => void addToPlaylist(pl)}
                      >
                        {pl.name}
                      </button>
                    </li>
                  ))
                )}
              </ul>
            )}
          </li>
          <li className="track-menu-item" role="none">
            <button
              type="button"
              className="track-menu-button"
              role="menuitem"
              data-testid="track-menu-play-next"
              onClick={() => {
                close();
                onPlayNext();
              }}
            >
              Play next
            </button>
          </li>
          <li className="track-menu-item" role="none">
            <button
              type="button"
              className="track-menu-button"
              role="menuitem"
              data-testid="track-menu-add-queue"
              onClick={() => {
                close();
                onAddToQueue();
              }}
            >
              Add to queue
            </button>
          </li>
          <li className="track-menu-item" role="none">
            <button
              type="button"
              className="track-menu-button"
              role="menuitem"
              data-testid="track-menu-edit"
              onClick={() => {
                close();
                onEdit();
              }}
            >
              Edit
            </button>
          </li>
        </ul>
      )}
    </div>
  );
}
