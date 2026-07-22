import type { ReactNode, RefObject } from "react";
import { Link } from "react-router-dom";
import type { LayoutMode } from "./browseLayout";

// The mode-aware body of a browse grid (appletv-web-parity §5). One <ul> drawn
// three ways from the SAME already-loaded items — so switching layout is a pure
// re-render, never a refetch:
//
//   • tile   — the artwork wall (today's default): each item's existing poster
//              card, unchanged, via `renderTile`.
//   • detail — a thumbnail + text row: a small frame beside the name and a
//              secondary line, both built from list-payload fields already in hand
//              (client ADR-0007 — NO per-row detail fetch). Rich for Shows
//              (overview), serviceable for Movies, thin for Artists/Albums.
//   • list   — names only.
//
// The per-kind screen supplies `renderTile` (its own poster card) and `toRow`
// (which summary fields become the Detail thumbnail + text). Everything else — the
// container, the letter-jump gridRef, the row markup — is shared here so the four
// grids don't each reimplement it.
//
// The <ul> keeps its `poster-grid` testid and takes a `data-layout` attribute in
// every mode, so existing specs (and the letter-jump / infinite-scroll observers)
// find the same container whatever the layout, and a test can assert which layout
// is live. Rows carry the same `poster-tile` testid as tiles so counts hold across
// modes; the kind's `data-*-id` passes through `dataAttrs`.

/** What one item contributes to a Detail / List row — all from already-loaded
 * summary fields (no fetch). */
export interface BrowseRowData {
  /** React key + stable identity. */
  key: string;
  /** Where the row links (the same destination the tile links to). */
  to: string;
  /** The primary label (title / name) — the only thing List shows. */
  name: string;
  /** Extra dataset attributes to mirror the tile's (`data-title-id`, etc.), so
   * tests and cache-busting select a row the same way they select a tile. */
  dataAttrs?: Record<string, string>;
  /** The small thumbnail node (a `.poster-frame`), shown in Detail only. */
  thumb?: ReactNode;
  /** The secondary text under the name, shown in Detail only — year, overview,
   * genres, track count: whatever the kind already has in hand. */
  meta?: ReactNode;
}

export interface BrowseListProps<T> {
  mode: LayoutMode;
  items: T[];
  /** Attach to the <ul> so useLetterJump can scroll its rows by index. Omitted on
   * grids with no alphabet jump (e.g. an Artist's already-loaded Albums). */
  gridRef?: RefObject<HTMLUListElement>;
  /** The item's poster card, for Tile mode (the existing per-kind tile). */
  renderTile: (item: T) => ReactNode;
  /** The item's Detail/List row content, from already-loaded fields. */
  toRow: (item: T) => BrowseRowData;
  /** Override the <ul> testid (defaults to the shared `poster-grid`). */
  testId?: string;
}

export default function BrowseList<T>({
  mode,
  items,
  gridRef,
  renderTile,
  toRow,
  testId = "poster-grid",
}: BrowseListProps<T>) {
  // Tile keeps the poster-grid class (the wall); Detail/List swap to the row
  // layouts. The container id + data-layout are constant so callers/tests/observers
  // key on one element.
  const className =
    mode === "tile" ? "poster-grid" : `browse-rows browse-rows-${mode}`;

  return (
    <ul
      className={className}
      data-testid={testId}
      data-layout={mode}
      ref={gridRef}
    >
      {mode === "tile"
        ? items.map((item) => renderTile(item))
        : items.map((item) => (
            <BrowseRow key={toRow(item).key} mode={mode} row={toRow(item)} />
          ))}
    </ul>
  );
}

function BrowseRow({
  mode,
  row,
}: {
  mode: Exclude<LayoutMode, "tile">;
  row: BrowseRowData;
}) {
  return (
    <li
      className={`poster-tile browse-row browse-row-${mode}`}
      data-testid="poster-tile"
      {...(row.dataAttrs ?? {})}
    >
      <Link className="browse-row-link" to={row.to}>
        {/* Detail leads with a small thumbnail; List is names only. */}
        {mode === "detail" && row.thumb && (
          <div className="browse-row-thumb">{row.thumb}</div>
        )}
        <div className="browse-row-text">
          <span
            className="poster-title"
            data-testid="poster-title"
            title={row.name}
          >
            {row.name}
          </span>
          {mode === "detail" && row.meta && (
            <div className="browse-row-meta" data-testid="browse-row-meta">
              {row.meta}
            </div>
          )}
        </div>
      </Link>
    </li>
  );
}
