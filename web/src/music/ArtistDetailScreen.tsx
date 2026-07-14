import { useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { Album } from "../api/types";
import { useAsync } from "../browse/useAsync";
import { useTargetedScan } from "../browse/useTargetedScan";
import EntityScanMenu from "../browse/EntityScanMenu";
import { EditIcon } from "../browse/ActionIcons";
import BackLink, { useLibraryName } from "../browse/BackLink";
import DetailBackdrop from "../browse/DetailBackdrop";
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
  // Targeted scan of this Artist's album folders (ADR-0030), Admin-only. On
  // completion it bumps reloadKey → the detail refetches in place.
  const {
    scanning,
    message: scanMessage,
    scan: runScan,
  } = useTargetedScan(() => setReloadKey((k) => k + 1));
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
          {/* The Artist's fanart.tv Background pinned behind the whole screen (like a
              Show/Movie); content scrolls over it and it fades toward black. */}
          <DetailBackdrop src={state.data.artist.backgroundUrl} />
          <div className="detail-hero">
            <ArtistHero
              name={state.data.artist.name}
              logoUrl={state.data.artist.logoUrl}
              photoUrl={state.data.artist.artworkUrl}
            >
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

              {/* Action toolbar (ADR-0019), Admin-only — mirrors the Movie/Show hero
                  toolbar: the Edit icon opens the "Edit item" dialog and the ⋯ kebab
                  holds the Targeted scan. The Artist's two correction actions live in
                  the dialog, one per tab:
                  • "Search" (item-editing/unified-search) — the "wrong Nirvana" record
                    fix: search or paste a provider URL/id, pick the right artist record,
                    and Update it (an Enrichment override). An Artist has no per-item
                    identity anchor, so there is no Replace (no onReplace).
                  • "Fix label" (item-editing/03) — rename the Artist, edit its bio/genres,
                    or pick an image; each edit is Locked and NEVER cascades. */}
              {isAdmin && (
                <div className="detail-actions" data-testid="detail-actions">
                  <EditItemDialog
                    renderTrigger={(open) => (
                      <button
                        className="icon-button edit-item-button"
                        type="button"
                        data-testid="edit-item-button"
                        title="Edit"
                        aria-label="Edit"
                        onClick={open}
                      >
                        <EditIcon />
                      </button>
                    )}
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
                            initialQuery={state.data.artist.name}
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
                  <EntityScanMenu
                    onScan={() => runScan("artists", state.data.artist.id)}
                    scanning={scanning}
                    label="artist"
                  />
                </div>
              )}
            </ArtistHero>
          </div>

          {scanMessage && (
            <p className="status status-ok" data-testid="scan-notice" role="status">
              {scanMessage}
            </p>
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

// ArtistHero renders the detail-hero's identity: the fetched ClearLOGO leads (the
// artist's name in its own lettering, like a Show/Movie), and when there's no logo
// — or its image fails to load — it falls back to the artist photo avatar beside a
// text heading, and finally to the plain text name alone. `children` (genres + bio)
// follow the heading down the info column. The logo, when present, stands in for
// both the avatar and the text heading, matching the show detail.
function ArtistHero({
  name,
  logoUrl,
  photoUrl,
  children,
}: {
  name: string;
  logoUrl?: string;
  photoUrl?: string;
  children?: ReactNode;
}) {
  // Reset failure state when the target changes (detail nav / a newly picked image):
  // a URL that 404'd on a stale version may now resolve.
  const [logoFailed, setLogoFailed] = useState(false);
  const [photoFailed, setPhotoFailed] = useState(false);
  useEffect(() => setLogoFailed(false), [logoUrl]);
  useEffect(() => setPhotoFailed(false), [photoUrl]);

  const showLogo = Boolean(logoUrl) && !logoFailed;
  const showPhoto = !showLogo && Boolean(photoUrl) && !photoFailed;

  return (
    <>
      {showPhoto && (
        <div className="detail-poster artist-avatar">
          <img
            className="poster poster-img"
            data-testid="artist-image"
            src={photoUrl}
            alt={`${name} image`}
            loading="lazy"
            onError={() => setPhotoFailed(true)}
          />
        </div>
      )}
      <div className="detail-info">
        {showLogo ? (
          <h1 className="detail-logo-heading">
            <img
              className="detail-logo"
              data-testid="artist-logo"
              src={logoUrl}
              alt={name}
              onError={() => setLogoFailed(true)}
            />
          </h1>
        ) : (
          <h1 className="detail-title" data-testid="artist-name">
            {name}
          </h1>
        )}
        {children}
      </div>
    </>
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
