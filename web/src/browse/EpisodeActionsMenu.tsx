import { useEffect, useRef, useState } from "react";

// The per-row "three dots" actions menu on the Show detail episode list. A click
// opens a dropdown (same click-outside / Escape pattern as the music
// TrackActionsMenu / header NavDropdown) with three items:
//   1) Play next    → insert right after the now-playing entry
//   2) Add to queue  → append at the end of the Queue
//   3) Edit          → open the Episode's Title detail page
// All three actions are provided by the parent (they drive the shared Queue /
// router); this component owns only the open/close menu shell. The toggle stays
// hidden until the row is hovered/focused (CSS), so it doesn't clutter the list.

export default function EpisodeActionsMenu({
  episodeTitle,
  onPlayNext,
  onAddToQueue,
  onEdit,
}: {
  episodeTitle: string;
  onPlayNext: () => void;
  onAddToQueue: () => void;
  onEdit: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  // Close on outside click / Escape (mirrors the music TrackActionsMenu).
  useEffect(() => {
    if (!open) return;
    function onDocPointer(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="row-menu" ref={ref}>
      <button
        type="button"
        className="row-menu-toggle"
        data-testid="episode-menu-toggle"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`More actions for ${episodeTitle}`}
        onClick={() => setOpen((v) => !v)}
      >
        ⋮
      </button>
      {open && (
        <ul className="row-menu-list" role="menu" data-testid="episode-menu">
          <li className="row-menu-item" role="none">
            <button
              type="button"
              className="row-menu-button"
              role="menuitem"
              data-testid="episode-menu-play-next"
              onClick={() => {
                setOpen(false);
                onPlayNext();
              }}
            >
              Play next
            </button>
          </li>
          <li className="row-menu-item" role="none">
            <button
              type="button"
              className="row-menu-button"
              role="menuitem"
              data-testid="episode-menu-add-queue"
              onClick={() => {
                setOpen(false);
                onAddToQueue();
              }}
            >
              Add to queue
            </button>
          </li>
          <li className="row-menu-item" role="none">
            <button
              type="button"
              className="row-menu-button"
              role="menuitem"
              data-testid="episode-menu-edit"
              onClick={() => {
                setOpen(false);
                onEdit();
              }}
            >
              Details
            </button>
          </li>
        </ul>
      )}
    </div>
  );
}
