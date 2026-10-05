import OverflowPopover from "./OverflowPopover";

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
  return (
    <OverflowPopover>
      {({ pick }) => (
        <>
          <button
            className="overflow-menu-item scan-item"
            type="button"
            role="menuitem"
            data-testid="scan-item"
            disabled={scanning}
            title={`Re-scan this ${label}'s folder for added or changed files`}
            onClick={pick(onScan)}
          >
            {scanning ? "Scanning…" : "Scan"}
          </button>
        </>
      )}
    </OverflowPopover>
  );
}
