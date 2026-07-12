import { useEffect, useRef, useState } from "react";
import { MoreIcon } from "./ActionIcons";

// A minimal ⋯ kebab holding a single Admin "Scan" action — the Targeted scan
// (ADR-0030) for a detail page that has no other overflow menu (Album / Artist).
// It mirrors the Movie/Show overflow menu popover exactly (same classNames,
// outside-click / Escape close), so the four detail pages share one kebab look.
// Render it only for an Admin; a Member never sees the Scan action.
export default function EntityScanMenu({
  onScan,
  scanning,
  label,
}: {
  /** Trigger the Targeted scan of this entity's folder(s). */
  onScan: () => void;
  scanning: boolean;
  /** The entity noun for the item's tooltip, e.g. "album" / "artist". */
  label: string;
}) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  // Close on an outside click or Escape while open (a lightweight popover; no portal).
  useEffect(() => {
    if (!open) return;
    function onDocDown(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="overflow-menu" ref={wrapRef}>
      <button
        className="icon-button"
        type="button"
        data-testid="overflow-menu-button"
        title="More actions"
        aria-label="More actions"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <MoreIcon />
      </button>

      {open && (
        <div className="overflow-menu-list" role="menu" data-testid="overflow-menu">
          <button
            className="overflow-menu-item scan-item"
            type="button"
            role="menuitem"
            data-testid="scan-item"
            disabled={scanning}
            title={`Re-scan this ${label}'s folder for added or changed files`}
            onClick={() => {
              onScan();
              setOpen(false);
            }}
          >
            {scanning ? "Scanning…" : "Scan"}
          </button>
        </div>
      )}
    </div>
  );
}
