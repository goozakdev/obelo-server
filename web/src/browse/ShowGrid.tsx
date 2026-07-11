import { useCallback } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import type { ShowSummary } from "../api/types";
import { usePaginatedList } from "./usePaginatedList";
import { useInfiniteScrollSentinel } from "./useInfiniteScrollSentinel";
import { useLibraryLiveRefresh } from "../events/enrichEvents";
import Poster from "./Poster";

// The TV Show poster grid (tv-music issue 01 / PRD user story 11): a TV library
// browses a grid of SHOWS, not a flat wall of episodes. Each tile links to the
// Show detail (Seasons & Episodes). It cursor-paginates through ALL Shows via
// the shared usePaginatedList engine + an IntersectionObserver sentinel (the
// same infinite scroll the Movie grid uses), so a library with more than one
// page of Shows isn't silently capped.

export default function ShowGrid({
  libraryId,
  libraryName,
}: {
  libraryId: string;
  libraryName: string;
}) {
  const fetchPage = useCallback(
    async (cursor: string | null, signal: AbortSignal) => {
      const page = await apiClient.listShows(libraryId, { cursor }, signal);
      return { items: page.shows, nextCursor: page.nextCursor };
    },
    [libraryId],
  );
  const grid = usePaginatedList(fetchPage, getShowId);
  // Live-refresh as this Library is scanned/enriched: new Shows appear and
  // freshly-fetched posters land in place, no manual reload (realtime-events).
  useLibraryLiveRefresh(libraryId, grid.refresh);

  // Infinite scroll: an IntersectionObserver on a sentinel below the grid fires
  // loadMore when it scrolls into view. The sentinel only renders once the first
  // page has loaded, so we use a callback ref (useInfiniteScrollSentinel) that
  // attaches the observer the moment the sentinel node mounts.
  const sentinelRef = useInfiniteScrollSentinel(grid.loadMore, grid.items.length);

  return (
    <>
      <div className="grid-toolbar">
        <h2 className="section-title" data-testid="library-title">
          {libraryName}
        </h2>
      </div>

      {grid.loading && (
        <p className="status status-loading" data-testid="grid-loading">
          Loading shows&hellip;
        </p>
      )}

      {!grid.loading && grid.error && grid.items.length === 0 && (
        <p className="status status-error" data-testid="grid-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {grid.error}{" "}
          <button className="nav-link" type="button" onClick={grid.retry}>
            Retry
          </button>
        </p>
      )}

      {!grid.loading && !grid.error && grid.items.length === 0 && (
        <div className="card" data-testid="grid-empty">
          <p className="status status-loading">
            No shows here yet. Run a scan to index this library.
          </p>
        </div>
      )}

      {grid.items.length > 0 && (
        <ul className="poster-grid" data-testid="poster-grid">
          {grid.items.map((s) => (
            <ShowTile key={s.id} show={s} />
          ))}
        </ul>
      )}

      {/* Scroll sentinel + inline paging states. Rendered once there is content
          so the observer has something to watch. */}
      {grid.items.length > 0 && (
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

function getShowId(s: ShowSummary): string {
  return s.id;
}

// ShowTile mirrors PosterTile but links to the Show detail (Seasons/Episodes)
// instead of a Title. It reuses the shared <Poster> (placeholder fallback when a
// Show has no artwork — Show artwork is a later slice). The poster-tile / -title
// / -img / -placeholder testids match PosterTile so the browse specs select it
// the same way.
function ShowTile({ show }: { show: ShowSummary }) {
  return (
    <li
      className="poster-tile"
      data-testid="poster-tile"
      data-show-id={show.id}
    >
      <Link className="poster-link" to={`/shows/${show.id}`}>
        <div className="poster-frame">
          <Poster titleId={show.id} title={show.title} src={show.posterUrl} />
          {show.needsReview && (
            <span className="badge badge-unwatched" data-testid="badge-needs-review">
              Needs review
            </span>
          )}
          {show.unwatchedEpisodeCount > 0 && (
            <span
              className="badge badge-unwatched-count"
              data-testid="badge-unwatched-count"
              title={`${show.unwatchedEpisodeCount} unwatched`}
            >
              {show.unwatchedEpisodeCount}
            </span>
          )}
        </div>
        <div className="poster-caption">
          <span className="poster-title" data-testid="poster-title" title={show.title}>
            {show.title}
          </span>
          {show.year > 0 && <span className="poster-year">{show.year}</span>}
        </div>
      </Link>
    </li>
  );
}
