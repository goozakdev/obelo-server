import { useState, type ReactNode } from "react";
import { Navigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { TitleSort, TitleSummary } from "../api/types";
import { useLibraryLiveRefresh } from "../events/enrichEvents";
import { useAsync } from "./useAsync";
import { useTitleGrid } from "./useTitleGrid";
import { useInfiniteScrollSentinel } from "./useInfiniteScrollSentinel";
import { useLetterJump } from "./useLetterJump";
import { useLayoutMode } from "./browseLayout";
import LayoutToggle from "./LayoutToggle";
import BrowseList, { type BrowseRowData } from "./BrowseList";
import LetterJumpBar from "./LetterJumpBar";
import Poster from "./Poster";
import PosterTile from "./PosterTile";
import ShowGrid from "./ShowGrid";
import AppHeader from "./AppHeader";

// A Library's poster grid (issue 03 / PRD user stories 9–11). This is the
// TV/Movie grid: a Movie library renders the Title poster grid (cursor-paginated,
// sortable); a TV library renders a Show poster grid (ShowGrid, tv-music issue
// 01). Music has its own separate experience (music/MusicLibraryScreen) under
// /music; a music library that lands here (a stale /libraries/{id} link) is
// redirected there. The library header (name) comes from a one-shot
// GET /libraries/{id}.

const SORTS: { value: TitleSort; label: string }[] = [
  { value: "title", label: "Title" },
  { value: "dateAdded", label: "Date added" },
];

export default function LibraryGridScreen() {
  const { libraryId = "" } = useParams();
  const lib = useAsync(
    (signal) => apiClient.getLibrary(libraryId, signal),
    [libraryId],
  );

  const libraryName = lib.status === "ready" ? lib.data.name : "Library";
  const kind = lib.status === "ready" ? lib.data.kind : "movie";

  // A music library belongs to the separate music experience — redirect so the
  // URL, shell, and theme all match (keeps old /libraries/{id} links working).
  if (lib.status === "ready" && lib.data.kind === "music") {
    return <Navigate to={`/music/libraries/${libraryId}`} replace />;
  }

  return (
    <div className="app-shell" data-testid="library-grid-screen" data-kind={kind}>
      <AppHeader />
      <main className="app-main app-main-wide">
        {/* Branch on kind: TV → Show grid, else Movie grid. (Music has its own
            experience under /music — see the redirect above.) */}
        {kind === "tv" ? (
          <ShowGrid libraryId={libraryId} libraryName={libraryName} />
        ) : (
          <MovieGrid libraryId={libraryId} libraryName={libraryName} />
        )}
      </main>
    </div>
  );
}

function MovieGrid({
  libraryId,
  libraryName,
}: {
  libraryId: string;
  libraryName: string;
}) {
  const [sort, setSort] = useState<TitleSort>("title");
  const [mode, setMode] = useLayoutMode(libraryId);
  const grid = useTitleGrid(apiClient, libraryId, sort);
  // Live-refresh: as this Library is scanned (Titles appearing) or enriched
  // (posters/fields landing), merge the fresh window in place — new Titles slot
  // in, enriched ones update, and per-Title poster cache-busting means only the
  // changed posters reload. No reset, no blank, no flicker (cf. the old bump-a-
  // reloadKey approach that re-fetched from scratch and busted every poster).
  useLibraryLiveRefresh(libraryId, grid.refresh);

  // Infinite scroll: an IntersectionObserver on a sentinel below the grid fires
  // loadMore when it scrolls into view. The sentinel only renders once the first
  // page has loaded, so we use a callback ref (useInfiniteScrollSentinel) that
  // attaches the observer the moment the sentinel node mounts. loadMore is a
  // no-op while a fetch is in flight or when there are no more pages, so a fast
  // scroll can't double-fetch.
  const sentinelRef = useInfiniteScrollSentinel(grid.loadMore, grid.titles.length);

  // Alphabetical jump bar. It only makes sense while the list is in title order,
  // so it's hidden when sorting by date added (a letter jump has no meaning then).
  const { gridRef, jumpTo } = useLetterJump(grid.titles, getTitleName, grid);

  return (
    <>
      <div className="grid-toolbar">
        <h2 className="section-title" data-testid="library-title">
          {libraryName}
        </h2>
        {sort === "title" && <LetterJumpBar onJump={jumpTo} />}
        <div className="grid-controls">
          <LayoutToggle mode={mode} onChange={setMode} />
          <label className="sort-control">
            <span className="field-label">Sort</span>
            <select
              className="sort-select"
              data-testid="sort-select"
              value={sort}
              onChange={(e) => setSort(e.target.value as TitleSort)}
            >
              {SORTS.map((s) => (
                <option key={s.value} value={s.value}>
                  {s.label}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>

      {grid.loading && (
        <p className="status status-loading" data-testid="grid-loading">
          Loading titles&hellip;
        </p>
      )}

      {!grid.loading && grid.error && grid.titles.length === 0 && (
        <p className="status status-error" data-testid="grid-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {grid.error}{" "}
          <button className="nav-link" type="button" onClick={grid.retry}>
            Retry
          </button>
        </p>
      )}

      {!grid.loading && !grid.error && grid.titles.length === 0 && (
        <div className="card" data-testid="grid-empty">
          <p className="status status-loading">
            No titles here yet. Run a scan to index this library.
          </p>
        </div>
      )}

      {grid.titles.length > 0 && (
        <BrowseList
          mode={mode}
          items={grid.titles}
          gridRef={gridRef}
          renderTile={(t) => <PosterTile key={t.id} title={t} />}
          toRow={movieToRow}
        />
      )}

      {/* Scroll sentinel + inline paging states. Always rendered (when there
          is content) so the observer has something to watch. */}
      {grid.titles.length > 0 && (
        <div className="grid-footer">
          {grid.loadingMore && (
            <p className="status status-loading" data-testid="grid-loading-more">
              Loading more&hellip;
            </p>
          )}
          {grid.error && !grid.loadingMore && (
            <p className="status status-error" role="alert">
              {grid.error}{" "}
              <button className="nav-link" type="button" onClick={grid.retry}>
                Retry
              </button>
            </p>
          )}
          {!grid.hasMore && !grid.loadingMore && (
            <p className="status status-loading" data-testid="grid-end">
              That's everything.
            </p>
          )}
          <div ref={sentinelRef} data-testid="grid-sentinel" aria-hidden="true" />
        </div>
      )}
    </>
  );
}

function getTitleName(t: { title: string }): string {
  return t.title;
}

// A Movie's Detail/List row, built ONLY from already-loaded summary fields (client
// ADR-0007 — no per-row fetch): the poster thumbnail (Detail), the title, and a
// serviceable secondary line (year · rating · genres). Same poster + cache-bust
// token the tile uses, so a re-enrich reloads the thumbnail identically.
function movieToRow(t: TitleSummary): BrowseRowData {
  return {
    key: t.id,
    to: `/titles/${t.id}`,
    name: t.title,
    dataAttrs: { "data-title-id": t.id },
    thumb: (
      <div className="poster-frame">
        <Poster titleId={t.id} title={t.title} version={t.artworkVersion} />
      </div>
    ),
    meta: movieMeta(t),
  };
}

function movieMeta(t: TitleSummary): ReactNode {
  const bits: string[] = [];
  if (t.year > 0) bits.push(String(t.year));
  if (t.contentRating) bits.push(t.contentRating);
  if (t.genres.length > 0) bits.push(t.genres.join(", "));
  return bits.length > 0 ? <span>{bits.join(" · ")}</span> : null;
}
