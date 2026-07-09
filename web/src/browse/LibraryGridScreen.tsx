import { useState } from "react";
import { Navigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { TitleSort } from "../api/types";
import { useLibraryLiveRefresh } from "../events/enrichEvents";
import { useAsync } from "./useAsync";
import { useTitleGrid } from "./useTitleGrid";
import { useInfiniteScrollSentinel } from "./useInfiniteScrollSentinel";
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
  const sentinelRef = useInfiniteScrollSentinel(grid.loadMore);

  return (
    <>
      <div className="grid-toolbar">
        <h2 className="section-title" data-testid="library-title">
          {libraryName}
        </h2>
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
        <ul className="poster-grid" data-testid="poster-grid">
          {grid.titles.map((t) => (
            <PosterTile key={t.id} title={t} />
          ))}
        </ul>
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
