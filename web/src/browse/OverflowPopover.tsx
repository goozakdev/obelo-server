import { useRef, useState, type ReactNode } from "react";
import { MoreIcon } from "./ActionIcons";
import { useDismiss } from "../lib/useDismiss";

// OverflowPopover is the ⋯ kebab and its popover list shared by every detail page's
// overflow menu (Title, Show, Track, Album/Artist scan). It owns the open state and
// closes on Escape or an outside click; the caller renders the menu items, using
// `pick(fn)` for an item that should run fn and then close, or `close` directly.
export default function OverflowPopover({
  children,
}: {
  children: (menu: { close: () => void; pick: (fn: () => void) => () => void }) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  const close = () => setOpen(false);

  useDismiss(wrapRef, open, close);

  // Run an item's action, then close the menu.
  const pick = (fn: () => void) => () => {
    fn();
    close();
  };

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
          {children({ close, pick })}
        </div>
      )}
    </div>
  );
}
