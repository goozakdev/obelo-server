import { useEffect, useRef, useState, type ReactNode } from "react";

// EditItemDialog collects the Edit-item actions (ADR-0019) into a single Admin-only
// "Edit item" dialog, so a detail page stays clean instead of stacking the inline
// correction forms. Since the unified-search redesign the actions are "Search"
// (Fix info + Wrong item merged — search/paste a candidate, then Update or Replace),
// the per-role artwork tabs (Poster / Background / Artist Photo / Album Cover —
// artwork-management/01, ADR-0026), and "Fix label"; the caller passes only the tabs
// that apply to the item's kind (a Track has just Search; a Movie/Show adds Poster +
// Background). The action forms render inside the active tab's body, so every inner
// data-testid is preserved — and because only the ACTIVE tab's node is mounted,
// selecting an artwork tab is what mounts (and thus auto-searches) its picker.
//
// It is a native <dialog> opened with showModal(): that gives ESC-to-close, the
// top layer, focus containment, and a ::backdrop for free — no portal or custom
// trap. Applying an edit deliberately keeps the dialog OPEN so the picker's
// post-apply summary stays visible; the detail underneath refreshes as it does
// today. Rendered only when the caller is an Admin (the screens gate on isAdmin).

export type EditItemTabKey =
  | "search"
  | "fix-label"
  | "poster"
  | "background"
  | "logo"
  | "artist-photo"
  | "album-cover";

export interface EditItemTab {
  /** Stable action key, drives the tab's data-testid (edit-item-tab-<key>). */
  key: EditItemTabKey;
  /** The tab button label ("Search" / "Fix label"). */
  label: string;
  /** Marks a destructive action so the tab reads as a danger zone. Unused now that
   * the destructive Replace lives inside the (non-danger) Search tab, but kept so a
   * future danger tab can opt in. */
  danger?: boolean;
  /** The existing editor form to render when this tab is active. */
  node: ReactNode;
}

export default function EditItemDialog({
  tabs,
  renderTrigger,
}: {
  tabs: EditItemTab[];
  /** Optional custom trigger. Receives an `open` callback; render whatever opens
   * the dialog (e.g. a toolbar icon). Defaults to the "Edit item" text button.
   * Keep the `edit-item-button` data-testid on the trigger so callers/tests that
   * open the dialog by that id keep working. */
  renderTrigger?: (open: () => void) => ReactNode;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [open, setOpen] = useState(false);
  // The selected tab key; defaults to the first provided tab. Reset to the first
  // tab each time the dialog opens so it always lands on the primary action.
  const [activeKey, setActiveKey] = useState<EditItemTabKey | null>(
    tabs[0]?.key ?? null,
  );

  // Drive the native dialog imperatively so ESC / the top layer / focus come for
  // free; keep React's `open` state in sync via the dialog's own close event.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open && !dialog.open) dialog.showModal();
    if (!open && dialog.open) dialog.close();
  }, [open]);

  if (tabs.length === 0) return null;

  const active = tabs.find((t) => t.key === activeKey) ?? tabs[0];
  // With a single action (Track → Fix info) the tab row is redundant; hide it but
  // still render the (single) tab button so the tab interaction stays satisfiable.
  const single = tabs.length === 1;

  function openDialog() {
    setActiveKey(tabs[0].key);
    setOpen(true);
  }

  return (
    <div className="edit-item">
      {renderTrigger ? (
        renderTrigger(openDialog)
      ) : (
        <button
          className="nav-link edit-item-button"
          type="button"
          data-testid="edit-item-button"
          onClick={openDialog}
        >
          Edit item
        </button>
      )}

      <dialog
        ref={dialogRef}
        className="edit-item-dialog"
        data-testid="edit-item-dialog"
        // Native ESC / the dialog's own close both flip our state back.
        onClose={() => setOpen(false)}
        // Backdrop click: a modal <dialog>'s click target is the dialog element
        // itself only when the ::backdrop (outside the panel) is clicked.
        onClick={(e) => {
          if (e.target === dialogRef.current) setOpen(false);
        }}
      >
        <div className="edit-item-panel">
          <header className="edit-item-header">
            <h2 className="edit-item-title">Edit item</h2>
            <button
              className="nav-link edit-item-close"
              type="button"
              data-testid="edit-item-close"
              aria-label="Close"
              onClick={() => setOpen(false)}
            >
              ✕
            </button>
          </header>

          <div
            className={`edit-item-tablist${single ? " edit-item-tablist-single" : ""}`}
            role="tablist"
            aria-label="Edit item actions"
          >
            {tabs.map((tab) => {
              const selected = tab.key === active.key;
              return (
                <button
                  key={tab.key}
                  className={`edit-item-tab${selected ? " is-active" : ""}${tab.danger ? " edit-item-tab-danger" : ""}`}
                  type="button"
                  role="tab"
                  aria-selected={selected}
                  data-testid={`edit-item-tab-${tab.key}`}
                  onClick={() => setActiveKey(tab.key)}
                >
                  {tab.label}
                </button>
              );
            })}
          </div>

          <div className="edit-item-body" role="tabpanel">
            {active.node}
          </div>
        </div>
      </dialog>
    </div>
  );
}
