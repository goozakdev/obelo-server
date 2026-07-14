import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { TrackSummary } from "../api/types";
import { useQueue } from "../player/queue/useQueue";
import {
  buildAlbumQueue,
  buildSingleQueue,
  trackToSummary,
} from "../player/queue/buildQueue";
import { entryFromTitle } from "../player/queue/model";
import { usePlaybackTransport } from "../player/transport";
import { useAsync } from "../browse/useAsync";
import { useTargetedScan } from "../browse/useTargetedScan";
import EntityScanMenu from "../browse/EntityScanMenu";
import BackLink from "../browse/BackLink";
import Poster from "../browse/Poster";
import { albumArtworkUrl } from "../browse/albumArt";
import EntityEnrichmentOverridePicker from "../admin/EntityEnrichmentOverridePicker";
import EntityMetadataEditor, { entityArtworkTabs } from "../admin/EntityMetadataEditor";
import EditItemDialog from "../admin/EditItemDialog";
import { useAuth } from "../auth/session";
import { formatTimecode } from "../time";
import MusicShell from "./MusicShell";
import TrackActionsMenu from "./TrackActionsMenu";

// The Album detail screen (tv-music issue 03 / PRD user story 27): GET
// /albums/{id}/tracks rendered as the Album header + its Tracks in disc/track
// order. Each Track row is four columns: a track-number cell that becomes a
// play/pause control (play on hover; pause when THIS track is the now-playing one,
// via the shared playback transport), the title + artist (both links), the track
// length (mm:ss), and a "three dots" actions menu (add to playlist / play next /
// add to queue / edit).
//
// Playing a Track builds the album-from-here Queue (queue/02) — the chosen Track
// and the rest of the Album in disc/track order — and starts the persistent Now
// Playing bar; a failed build falls back to just the chosen Track. Play next / add
// to queue drive the shared Queue directly. Lives in the music module and links
// into /music/...; playback uses the media-agnostic shared player.

export default function AlbumDetailScreen() {
  const { albumId = "" } = useParams();
  const queue = useQueue();
  const transport = usePlaybackTransport();
  const { isAdmin } = useAuth();
  // A transient confirmation for the row actions (added to playlist / queue / play
  // next), shown above the list; replaced by the next action.
  const [notice, setNotice] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const state = useAsync(
    (signal) => apiClient.getAlbumTracks(albumId, signal),
    [albumId, reloadKey],
    // Keep the detail mounted through a post-edit reload so the Fix-info picker's
    // cascade summary survives (item-editing/05).
    { keepPreviousData: true },
  );

  // An Album returns to its Artist; until the detail loads, fall back to Home.
  const album = state.status === "ready" ? state.data.album : undefined;
  // Targeted scan of this Album's folder(s) (ADR-0030), Admin-only. On completion
  // it bumps reloadKey → the detail refetches in place with the new Track set.
  const {
    scanning,
    message: scanMessage,
    scan: runScan,
  } = useTargetedScan(() => setReloadKey((k) => k + 1));
  const parent = album
    ? { to: `/music/artists/${album.artistId}`, label: album.artistName || "Artist" }
    : { to: "/", label: "Home" };

  // Build the album-from-here Queue; playback begins in the persistent Now Playing
  // bar (now-playing-bar/01), NO navigation. A transient/404 build failure falls
  // back to a single-entry Queue of just that Track (story 39).
  const play = useCallback(
    async (track: TrackSummary) => {
      try {
        const entries = await buildAlbumQueue(apiClient, albumId, track.id);
        queue.playNow(
          entries.length > 0 ? entries : buildSingleQueue(trackToSummary(track)),
        );
      } catch {
        queue.playNow(buildSingleQueue(trackToSummary(track)));
      }
    },
    [albumId, queue],
  );

  // Insert this Track right after the now-playing entry.
  const playNext = useCallback(
    (track: TrackSummary) => {
      queue.playNext([entryFromTitle(trackToSummary(track))]);
      setNotice(`“${track.title}” will play next.`);
    },
    [queue],
  );

  // Append this Track at the end of the Queue.
  const addToQueue = useCallback(
    (track: TrackSummary) => {
      queue.enqueue([entryFromTitle(trackToSummary(track))]);
      setNotice(`Added “${track.title}” to the queue.`);
    },
    [queue],
  );

  return (
    <MusicShell testId="album-detail-screen">
      <BackLink to={parent.to} label={parent.label} />

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="album-loading">
          Loading album&hellip;
        </p>
      )}

      {state.status === "error" && (
        <p className="status status-error" data-testid="album-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}

      {state.status === "ready" && (
        <article className="detail" data-testid="album-detail">
          <div className="detail-hero">
            <div className="detail-poster">
              {state.data.album.hasArtwork ? (
                <img
                  className="poster poster-img"
                  data-testid="poster-img"
                  src={albumArtworkUrl(state.data.album.id, state.data.album.artworkVersion)}
                  alt={`${state.data.album.title} cover`}
                />
              ) : (
                <Poster titleId={state.data.album.id} title={state.data.album.title} />
              )}
            </div>
            <div className="detail-info">
              <h1 className="detail-title album-title" data-testid="album-title">
                {state.data.album.title}
              </h1>
              {state.data.album.artistName && (
                <p className="detail-context" data-testid="album-artist">
                  <Link
                    className="nav-link"
                    to={`/music/artists/${encodeURIComponent(state.data.album.artistId)}`}
                  >
                    {state.data.album.artistName}
                  </Link>
                </p>
              )}
              {state.data.album.year > 0 && (
                <div className="detail-meta">
                  <span data-testid="album-year">{state.data.album.year}</span>
                </div>
              )}
              {(state.data.album.genres ?? []).length > 0 && (
                <div className="detail-genres" data-testid="album-genres">
                  {(state.data.album.genres ?? []).join(" · ")}
                </div>
              )}
            </div>
          </div>

          {/* Edit-item (ADR-0019), Admin-only. The Album's two correction actions live
              in a single "Edit item" dialog, one per tab:
              • "Search" (item-editing/unified-search) — correct WHICH release decorates
                the Album: search or paste a provider URL/id, pick the right release, and
                Update it (an Enrichment override). An Album has no per-item identity
                anchor, so there is no Replace (no onReplace).
              • "Fix label" (item-editing/03) — rename the Album or pick a cover; each
                edit is Locked and NEVER cascades to the tracks. */}
          {isAdmin && (
            <EditItemDialog
              tabs={[
                {
                  key: "fix-label",
                  label: "Details",
                  node: (
                    <EntityMetadataEditor
                      entityType="albums"
                      entityId={state.data.album.id}
                      displayName={state.data.album.title}
                      genres={state.data.album.genres}
                      lockedFields={state.data.album.lockedFields}
                      onChanged={() => setReloadKey((k) => k + 1)}
                    />
                  ),
                },
                {
                  key: "search",
                  label: "Search",
                  node: (
                    <EntityEnrichmentOverridePicker
                      entityType="albums"
                      entityId={state.data.album.id}
                      currentExternalId={state.data.album.enrichmentOverride?.externalId}
                      artistScope={state.data.album.artistName ?? ""}
                      initialQuery={state.data.album.title}
                      onApplied={() => setReloadKey((k) => k + 1)}
                    />
                  ),
                },
                // Album Cover artwork tab (artwork-management/01): auto-search on
                // open, apply + Lock on click.
                ...entityArtworkTabs("albums", state.data.album.id, state.data.album.lockedFields, () =>
                  setReloadKey((k) => k + 1),
                ),
              ]}
            />
          )}

          {isAdmin && (
            <EntityScanMenu
              onScan={() => runScan("albums", state.data.album.id)}
              scanning={scanning}
              label="album"
            />
          )}
          {scanMessage && (
            <p className="status status-ok" data-testid="scan-notice" role="status">
              {scanMessage}
            </p>
          )}

          {state.data.tracks.length === 0 ? (
            <p className="status status-loading" data-testid="no-tracks">
              No tracks indexed for this album yet.
            </p>
          ) : (
            <section className="track-listing">
              {notice && (
                <p className="track-notice" role="status" data-testid="track-notice">
                  {notice}
                </p>
              )}
              <ul className="track-list" data-testid="track-list">
                {state.data.tracks.map((track) => (
                  <TrackRow
                    key={track.id}
                    track={track}
                    artistId={state.data.album.artistId}
                    artistName={state.data.album.artistName}
                    isCurrent={queue.current?.title.id === track.id}
                    playing={transport.playing}
                    onPlay={() => void play(track)}
                    onToggle={transport.toggle}
                    onPlayNext={() => playNext(track)}
                    onAddToQueue={() => addToQueue(track)}
                    onNotice={setNotice}
                  />
                ))}
              </ul>
            </section>
          )}
        </article>
      )}
    </MusicShell>
  );
}

function TrackRow({
  track,
  artistId,
  artistName,
  isCurrent,
  playing,
  onPlay,
  onToggle,
  onPlayNext,
  onAddToQueue,
  onNotice,
}: {
  track: TrackSummary;
  artistId: string;
  artistName: string;
  isCurrent: boolean;
  playing: boolean;
  onPlay: () => void;
  onToggle: () => void;
  onPlayNext: () => void;
  onAddToQueue: () => void;
  onNotice: (message: string) => void;
}) {
  const navigate = useNavigate();
  const isPlaying = isCurrent && playing;
  const trackNo = track.trackNumber > 0 ? track.trackNumber : "—";
  const length = track.durationMs > 0 ? formatTimecode(track.durationMs) : "";

  // The row's play control: toggle the shared element when THIS track is the
  // now-playing one, otherwise start it (album-from-here).
  const onActivate = () => (isCurrent ? onToggle() : onPlay());

  return (
    <li
      className={`track-row${isCurrent ? " is-current" : ""}`}
      data-testid="track-row"
      data-track-id={track.id}
      data-track-number={track.trackNumber}
    >
      {/* Col 1: track number, which becomes a play/pause control on hover (or
          always, when this is the now-playing track). */}
      <div className="track-num-cell">
        <span className="track-number" data-testid="track-number" aria-hidden={isCurrent}>
          {trackNo}
        </span>
        <button
          type="button"
          className="track-toggle"
          data-testid="track-play"
          data-track-id={track.id}
          aria-label={isPlaying ? `Pause ${track.title}` : `Play ${track.title}`}
          onClick={onActivate}
        >
          {isPlaying ? "❚❚" : "▶"}
        </button>
      </div>

      {/* Col 2: title (→ track view) over artist (→ artist view); takes the slack. */}
      <div className="track-main">
        <Link
          className="track-title"
          data-testid="track-open"
          to={`/music/tracks/${track.id}`}
        >
          <span data-testid="track-title">{track.title}</span>
        </Link>
        {artistName && (
          <Link
            className="track-artist"
            data-testid="track-artist"
            to={`/music/artists/${encodeURIComponent(artistId)}`}
          >
            {artistName}
          </Link>
        )}
      </div>

      {/* Col 3: length. */}
      <div className="track-length" data-testid="track-length">
        {length}
      </div>

      {/* Col 4: the actions menu. */}
      <TrackActionsMenu
        trackId={track.id}
        trackTitle={track.title}
        onPlayNext={onPlayNext}
        onAddToQueue={onAddToQueue}
        onEdit={() => navigate(`/music/tracks/${track.id}`)}
        onNotice={onNotice}
      />
    </li>
  );
}
