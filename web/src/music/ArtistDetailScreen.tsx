import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { Album } from "../api/types";
import { useAsync } from "../browse/useAsync";
import BackLink, { useLibraryName } from "../browse/BackLink";
import Poster from "../browse/Poster";
import { albumArtworkUrl } from "../browse/albumArt";
import EntityEnrichmentOverridePicker from "../admin/EntityEnrichmentOverridePicker";
import EntityMetadataEditor, { entityArtworkTabs } from "../admin/EntityMetadataEditor";
import EditItemDialog from "../admin/EditItemDialog";
import { useAuth } from "../auth/session";
import MusicShell from "./MusicShell";

// The Artist detail screen (tv-music issue 03 / PRD user story 26): GET
// /artists/{id}/albums rendered as the Artist header + a grid of its Albums in
// (year, title) order. Each Album links to the Album detail (its ordered track
// list). The player is reached from a Track, so this screen is pure navigation.
//
// Lives in the music module and links into /music/...: it renders inside the
// music shell, and an Album tile opens /music/albums/{id}.

export default function ArtistDetailScreen() {
  const { artistId = "" } = useParams();
  const { isAdmin } = useAuth();
  const [reloadKey, setReloadKey] = useState(0);
  const state = useAsync(
    (signal) => apiClient.getArtistAlbums(artistId, signal),
    [artistId, reloadKey],
    // Keep the detail mounted through a post-edit reload so the Fix-info picker's
    // cascade summary survives (item-editing/05).
    { keepPreviousData: true },
  );

  // An Artist returns to its owning Music Library (named from the app-wide
  // Libraries list); until the detail loads, fall back to the Music home.
  const artist = state.status === "ready" ? state.data.artist : undefined;
  const libraryName = useLibraryName(artist?.libraryId, "Music");
  const parent = artist
    ? { to: `/music/libraries/${artist.libraryId}`, label: libraryName }
    : { to: "/", label: "Home" };

  return (
    <MusicShell testId="artist-detail-screen">
      <BackLink to={parent.to} label={parent.label} />

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="artist-loading">
          Loading artist&hellip;
        </p>
      )}

      {state.status === "error" && (
        <p className="status status-error" data-testid="artist-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}

      {state.status === "ready" && (
        <article className="detail" data-testid="artist-detail">
          <div className="detail-hero">
            {state.data.artist.artworkUrl && (
              <div className="detail-poster artist-avatar">
                <img
                  className="poster poster-img"
                  data-testid="artist-image"
                  src={state.data.artist.artworkUrl}
                  alt={`${state.data.artist.name} image`}
                  loading="lazy"
                  onError={(e) => {
                    (e.currentTarget as HTMLImageElement).style.display = "none";
                  }}
                />
              </div>
            )}
            <div className="detail-info">
              <h1 className="detail-title" data-testid="artist-name">
                {state.data.artist.name}
              </h1>
              {(state.data.artist.genres ?? []).length > 0 && (
                <div className="detail-genres" data-testid="artist-genres">
                  {(state.data.artist.genres ?? []).join(" · ")}
                </div>
              )}
              {state.data.artist.overview && (
                <p className="detail-overview" data-testid="artist-overview">
                  {state.data.artist.overview}
                </p>
              )}
            </div>
          </div>

          {/* Edit-item (ADR-0019), Admin-only. The Artist's two correction actions
              live in a single "Edit item" dialog, one per tab:
              • "Search" (item-editing/unified-search) — the "wrong Nirvana" record fix:
                search or paste a provider URL/id, pick the right artist record, and
                Update it (an Enrichment override). An Artist has no per-item identity
                anchor, so there is no Replace (no onReplace).
              • "Fix label" (item-editing/03) — rename the Artist, edit its bio/genres,
                or pick an image; each edit is Locked and NEVER cascades to albums/tracks. */}
          {isAdmin && (
            <EditItemDialog
              tabs={[
                {
                  key: "fix-label",
                  label: "Details",
                  node: (
                    <EntityMetadataEditor
                      entityType="artists"
                      entityId={state.data.artist.id}
                      displayName={state.data.artist.name}
                      overview={state.data.artist.overview}
                      genres={state.data.artist.genres}
                      lockedFields={state.data.artist.lockedFields}
                      onChanged={() => setReloadKey((k) => k + 1)}
                    />
                  ),
                },
                {
                  key: "search",
                  label: "Search",
                  node: (
                    <EntityEnrichmentOverridePicker
                      entityType="artists"
                      entityId={state.data.artist.id}
                      currentExternalId={state.data.artist.enrichmentOverride?.externalId}
                      onApplied={() => setReloadKey((k) => k + 1)}
                    />
                  ),
                },
                // Artist Photo artwork tab (artwork-management/01): auto-searches on
                // open; its grid stays empty until slice 02 wires artist candidates.
                ...entityArtworkTabs("artists", state.data.artist.id, state.data.artist.lockedFields, () =>
                  setReloadKey((k) => k + 1),
                ),
              ]}
            />
          )}

          {state.data.albums.length === 0 && (
            <p className="status status-loading" data-testid="no-albums">
              No albums indexed for this artist yet.
            </p>
          )}

          {state.data.albums.length > 0 && (
            <ul className="poster-grid" data-testid="album-grid">
              {state.data.albums.map((album) => (
                <AlbumTile key={album.id} album={album} />
              ))}
            </ul>
          )}
        </article>
      )}
    </MusicShell>
  );
}

// AlbumTile is an album card linking to the Album detail (its track list). When
// the Album has a local cover the tile shows it (via the album artwork endpoint);
// otherwise <Poster> falls back to the initials placeholder. The `album-tile`
// class squares the frame (music.css) so square cover art shows uncropped.
function AlbumTile({ album }: { album: Album }) {
  return (
    <li
      className="poster-tile album-tile"
      data-testid="poster-tile"
      data-album-id={album.id}
    >
      <Link className="poster-link" to={`/music/albums/${album.id}`}>
        <div className="poster-frame">
          {album.hasArtwork ? (
            <img
              className="poster poster-img"
              data-testid="poster-img"
              src={albumArtworkUrl(album.id, album.artworkVersion)}
              alt={`${album.title} cover`}
              loading="lazy"
            />
          ) : (
            <Poster titleId={album.id} title={album.title} />
          )}
        </div>
        <div className="poster-caption">
          <span className="poster-title" data-testid="poster-title" title={album.title}>
            {album.title}
          </span>
          {album.year > 0 && <span className="poster-year">{album.year}</span>}
        </div>
      </Link>
    </li>
  );
}
