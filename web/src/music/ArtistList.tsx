import { useCallback, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { apiClient } from "../api/client";
import type { ArtistSummary } from "../api/types";
import { usePaginatedList } from "../browse/usePaginatedList";
import { useInfiniteScrollSentinel } from "../browse/useInfiniteScrollSentinel";
import { useLetterJump } from "../browse/useLetterJump";
import { useLayoutMode } from "../browse/browseLayout";
import LayoutToggle from "../browse/LayoutToggle";
import BrowseList, { type BrowseRowData } from "../browse/BrowseList";
import LetterJumpBar from "../browse/LetterJumpBar";
import { useLibraryLiveRefresh } from "../events/enrichEvents";
import Poster from "../browse/Poster";

// The Music Artist list (tv-music issue 03 / PRD user story 26): a Music library
// browses Artists → Albums → Tracks. This is the top-level grid (the analogue of
// the TV Show grid). Each tile links to the Artist detail (the Artist's Albums).
// Artists have no artwork of their own this slice, so <Poster> falls back to the
// initials placeholder (the same strategy the Show grid uses). It cursor-
// paginates through ALL Artists via the shared usePaginatedList engine + an
// IntersectionObserver sentinel (the same infinite scroll the Movie grid uses),
// so a library with more than one page of Artists isn't silently capped.
//
// An Artist with a fetched image carries `artworkUrl` (the same field the detail
// uses); the tile passes it to <Poster src>. Artists without a fetched image
// omit the field, so <Poster> falls back to the initials placeholder.
//
// Lives in the music module: it is the body of the music library landing
// (MusicLibraryScreen) and links into the /music/... route subtree.

export default function ArtistList({
  libraryId,
  libraryName,
}: {
  libraryId: string;
  libraryName: string;
}) {
  const fetchPage = useCallback(
    async (cursor: string | null, signal: AbortSignal) => {
      const page = await apiClient.listArtists(libraryId, { cursor }, signal);
      return { items: page.artists, nextCursor: page.nextCursor };
    },
    [libraryId],
  );
  const [mode, setMode] = useLayoutMode(libraryId);
  const grid = usePaginatedList(fetchPage, getArtistId);
  // Live-refresh as this Library is scanned/enriched: new Artists appear as
  // they're indexed, no manual reload (realtime-events web slice).
  useLibraryLiveRefresh(libraryId, grid.refresh);

  // Infinite scroll: an IntersectionObserver on a sentinel below the list fires
  // loadMore when it scrolls into view. The sentinel only renders once the first
  // page has loaded, so we use a callback ref (useInfiniteScrollSentinel) that
  // attaches the observer the moment the sentinel node mounts.
  const sentinelRef = useInfiniteScrollSentinel(grid.loadMore, grid.items.length);

  // Alphabetical jump bar (Artists are always in name order server-side).
  const { gridRef, jumpTo } = useLetterJump(grid.items, getArtistName, grid);

  return (
    <>
      <div className="grid-toolbar">
        <h2 className="section-title" data-testid="library-title">
          {libraryName}
        </h2>
        <LetterJumpBar onJump={jumpTo} />
        <div className="grid-controls">
          <LayoutToggle mode={mode} onChange={setMode} />
        </div>
      </div>

      {grid.loading && (
        <p className="status status-loading" data-testid="grid-loading">
          Loading artists&hellip;
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
            No artists here yet. Run a scan to index this library.
          </p>
        </div>
      )}

      {grid.items.length > 0 && (
        <BrowseList
          mode={mode}
          items={grid.items}
          gridRef={gridRef}
          renderTile={(a) => <ArtistTile key={a.id} artist={a} />}
          toRow={artistToRow}
        />
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

function getArtistId(a: ArtistSummary): string {
  return a.id;
}

function getArtistName(a: ArtistSummary): string {
  return a.name;
}

// An Artist's Detail/List row, from already-loaded summary fields only (client
// ADR-0007). Artists are the THIN case — the list payload has no per-Artist blurb
// beyond the photo and name — so Detail is the circular photo + name, plus genres
// if enrichment supplied any. No per-row fetch.
function artistToRow(a: ArtistSummary): BrowseRowData {
  return {
    key: a.id,
    to: `/music/artists/${a.id}`,
    name: a.name,
    dataAttrs: { "data-artist-id": a.id },
    thumb: (
      <div className="poster-frame artist-frame">
        <Poster titleId={a.id} title={a.name} src={a.artworkUrl} />
      </div>
    ),
    meta: artistMeta(a),
  };
}

function artistMeta(a: ArtistSummary): ReactNode {
  return a.genres.length > 0 ? <span>{a.genres.join(", ")}</span> : null;
}

// ArtistTile mirrors the Show tile but links to the Artist detail (Albums). The
// poster-tile / -title / -img / -placeholder testids match PosterTile so the
// browse specs select it the same way. The extra `artist-tile` class renders the
// frame as a circle (music.css) so an Artist reads as distinct from an Album's
// square art at a glance.
function ArtistTile({ artist }: { artist: ArtistSummary }) {
  return (
    <li
      className="poster-tile artist-tile"
      data-testid="poster-tile"
      data-artist-id={artist.id}
    >
      <Link className="poster-link" to={`/music/artists/${artist.id}`}>
        <div className="poster-frame">
          <Poster titleId={artist.id} title={artist.name} src={artist.artworkUrl} />
        </div>
        <div className="poster-caption">
          <span className="poster-title" data-testid="poster-title" title={artist.name}>
            {artist.name}
          </span>
        </div>
      </Link>
    </li>
  );
}
