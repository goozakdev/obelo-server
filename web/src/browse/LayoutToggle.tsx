import { LAYOUT_LABELS, LAYOUT_MODES, type LayoutMode } from "./browseLayout";

// The browse layout toggle (appletv-web-parity §5): a small segmented control that
// picks how a Library grid draws — Tile / Detail / List. Shown above the four
// browse grids (Movies, Shows, Artists, Albums) and NOT on Playlists or Home. Flat
// wireframe style, matching the letter-jump buttons: bare text until the active
// one, which takes the lime accent. Switching the mode is a pure client re-layout
// — it never refetches the list (the parent grid keeps its already-loaded items).

export default function LayoutToggle({
  mode,
  onChange,
}: {
  mode: LayoutMode;
  onChange: (mode: LayoutMode) => void;
}) {
  return (
    <div
      className="layout-toggle"
      role="group"
      aria-label="Grid layout"
      data-testid="layout-toggle"
    >
      {LAYOUT_MODES.map((m) => {
        const active = m === mode;
        return (
          <button
            key={m}
            type="button"
            className="layout-toggle-btn"
            data-testid={`layout-${m}`}
            aria-pressed={active}
            onClick={() => onChange(m)}
          >
            {LAYOUT_LABELS[m]}
          </button>
        );
      })}
    </div>
  );
}
