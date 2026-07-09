import { useState } from "react";
import { Navigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type { TitleDetail, TitleSummary } from "../api/types";
import { useQueue } from "../player/queue/useQueue";
import { buildAlbumQueue, buildSingleQueue } from "../player/queue/buildQueue";
import { entryFromTitle } from "../player/queue/model";
import { useAsync } from "../browse/useAsync";
import { errorMessage } from "../screens/errorMessage";
import BackLink from "../browse/BackLink";
import Poster from "../browse/Poster";
import AddToPlaylist from "../browse/AddToPlaylist";
import EnrichmentOverridePicker from "../admin/EnrichmentOverridePicker";
import EditItemDialog from "../admin/EditItemDialog";
import { useAuth } from "../auth/session";
import { titleDetailSummary } from "../browse/titleSummary";
import { formatTimecode } from "../time";
import MusicShell from "./MusicShell";

// The music Track detail (/music/tracks/:titleId) — the music counterpart of the
// movie/episode TitleDetailScreen, split out so the track page can be customized
// independently. GET /titles/{id} rendered as the track's Artist/Album context,
// album art, watch-state, and the play + queue + playlist affordances. It
// deliberately OMITS the movie-oriented surfaces (Cast, Editions table, metadata
// editing, Add-to-collection); music can grow its own as the experience diverges.
//
// Playback reuses the shared infrastructure: Play builds the album-from-here Queue
// (buildAlbumQueue) and opens the shared player route (/titles/{id}/play). A
// non-track that lands here (a stale link) is redirected to the generic detail.

export default function TrackDetailScreen() {
  const { titleId = "" } = useParams();
  const state = useAsync(
    (signal) => apiClient.getTitle(titleId, signal),
    [titleId],
  );

  // A Track returns to its Album; until the detail loads, fall back to Home.
  const track = state.status === "ready" ? state.data : undefined;
  const parent = track?.track
    ? { to: `/music/albums/${track.track.albumId}`, label: track.track.albumTitle || "Album" }
    : { to: "/", label: "Home" };

  // A non-track that lands on the music track route (e.g. a stale link) belongs to
  // the generic detail — redirect so the URL and shell match the media kind.
  if (state.status === "ready" && state.data.kind !== "track") {
    return <Navigate to={`/titles/${state.data.id}`} replace />;
  }

  return (
    <MusicShell testId="track-detail-screen">
      <BackLink to={parent.to} label={parent.label} />

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="detail-loading">
          Loading track&hellip;
        </p>
      )}

      {state.status === "error" && (
        <p className="status status-error" data-testid="detail-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}

      {state.status === "ready" && <TrackDetail title={state.data} />}
    </MusicShell>
  );
}

function TrackDetail({ title }: { title: TitleDetail }) {
  const { isAdmin } = useAuth();
  const queue = useQueue();
  // Local genres, seeded from the server and rewritten from the detail an applied
  // Enrichment override returns, so an Admin's "Fix info" is reflected at once.
  const [genres, setGenres] = useState<string[]>(title.genres);
  // Local watch state, seeded from the server and updated by the manual toggle
  // (instant feedback; the server's resolved values are authoritative).
  const [watched, setWatched] = useState(title.watched);
  const [resumeMs, setResumeMs] = useState(title.resumePositionMs);
  const [toggling, setToggling] = useState(false);
  const [toggleError, setToggleError] = useState<string | null>(null);
  // Inline confirmation for the Queue affordances (queue/03).
  const [queueNotice, setQueueNotice] = useState<string | null>(null);

  const resuming = !watched && resumeMs > 0;
  const playable = title.editions.some((ed) => ed.files.some((f) => !f.missing));

  async function toggleWatched() {
    setToggling(true);
    setToggleError(null);
    try {
      const next = await apiClient.setWatchState(title.id, !watched);
      setWatched(next.watched);
      setResumeMs(next.resumePositionMs);
    } catch (err) {
      setToggleError(errorMessage(err));
    } finally {
      setToggling(false);
    }
  }

  // Play through the Queue (queue/02): a Track plays its Album from here
  // (buildAlbumQueue); playback begins in the persistent Now Playing bar
  // (now-playing-bar/01), NO navigation. A failed build (transient/404) falls back
  // to a single-entry Queue of THIS Track so the player is never stranded (story 39).
  async function play() {
    const summary: TitleSummary = titleDetailSummary(title, watched, resumeMs);
    try {
      const albumId = title.track?.albumId;
      const entries = albumId
        ? await buildAlbumQueue(apiClient, albumId, title.id)
        : [];
      queue.playNow(entries.length > 0 ? entries : buildSingleQueue(summary));
    } catch {
      queue.playNow(buildSingleQueue(summary));
    }
  }

  // "Add to queue" / "Play next" (stories 21–23, 29) carrying the LIVE watch state.
  function addToQueue() {
    queue.enqueue([entryFromTitle(titleDetailSummary(title, watched, resumeMs))]);
    setQueueNotice("Added to the end of the queue.");
  }
  function playNextInQueue() {
    queue.playNext([entryFromTitle(titleDetailSummary(title, watched, resumeMs))]);
    setQueueNotice("Playing next.");
  }

  return (
    <article className="detail" data-testid="detail">
      <div className="detail-hero">
        <div className="detail-poster">
          <Poster titleId={title.id} title={title.title} />
        </div>
        <div className="detail-info">
          {/* Track parent context: "Radiohead · OK Computer" so a Track reads in
              its Artist/Album position. */}
          {title.track && (
            <p className="detail-context" data-testid="track-context">
              {title.track.artistName} · {title.track.albumTitle}
            </p>
          )}
          <h1 className="detail-title" data-testid="detail-title">
            {title.title}
          </h1>
          <div className="detail-meta">
            {title.track && (title.track.trackNumber ?? 0) > 0 && (
              <span data-testid="detail-track-number">
                Track {title.track.trackNumber}
              </span>
            )}
            {title.track && (title.track.albumYear ?? 0) > 0 && (
              <span data-testid="detail-album-year">{title.track.albumYear}</span>
            )}
          </div>

          {genres.length > 0 && (
            <ul className="detail-genres" data-testid="detail-genres">
              {genres.map((g) => (
                <li key={g} className="genre-chip">
                  {g}
                </li>
              ))}
            </ul>
          )}

          {/* Watch (play) state indicator. */}
          <div className="detail-watchstate" data-testid="watch-state">
            {watched && (
              <span className="badge badge-watched" data-testid="watch-watched">
                Played
              </span>
            )}
            {resuming && (
              <span className="badge badge-resume" data-testid="watch-resume">
                Resume at {formatTimecode(resumeMs)}
              </span>
            )}
            {!watched && !resuming && (
              <span className="badge badge-unwatched" data-testid="watch-unwatched">
                Unplayed
              </span>
            )}
          </div>

          {/* Play affordance (queue/02): album-from-here Queue + the shared player. */}
          <button
            className="auth-submit play-button"
            data-testid="play-button"
            type="button"
            disabled={!playable}
            title={playable ? undefined : "No playable files for this track"}
            onClick={() => void play()}
          >
            {resuming ? "Resume" : "Play"}
          </button>

          {/* Queue affordances (queue/03). */}
          <div className="queue-affordances" data-testid="queue-affordances">
            <button
              className="nav-link add-to-queue"
              data-testid="add-to-queue-button"
              type="button"
              onClick={addToQueue}
            >
              Add to queue
            </button>
            <button
              className="nav-link play-next"
              data-testid="play-next-button"
              type="button"
              onClick={playNextInQueue}
            >
              Play next
            </button>
            {queueNotice && (
              <p className="status status-ok" data-testid="queue-notice" role="status">
                {queueNotice}
              </p>
            )}
          </div>

          {/* Manual played/unplayed toggle (bypasses the server threshold). */}
          <button
            className="nav-link watch-toggle"
            data-testid="watch-toggle"
            type="button"
            disabled={toggling}
            aria-pressed={watched}
            onClick={toggleWatched}
          >
            {toggling ? "Updating…" : watched ? "Mark unplayed" : "Mark played"}
          </button>
          {toggleError && (
            <p className="status status-error" data-testid="watch-toggle-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {toggleError}
            </p>
          )}

          {/* "Add to playlist": any authenticated User queues this Track into one
              of THEIR playlists (shared affordance). */}
          <AddToPlaylist titleId={title.id} titleKind={title.kind} />

          {/* Edit-item "Search" on a Track leaf (item-editing/unified-search,
              ADR-0019): search MusicBrainz (or paste a URL/id), pick a recording, and
              Update it as an Enrichment override — never touching identity or watch
              state. A Track has no identity anchor, so there is no Replace (no
              onReplace) and no Fix-label; the dialog shows this single Search tab. */}
          {isAdmin && (
            <EditItemDialog
              tabs={[
                {
                  key: "search",
                  label: "Search",
                  node: (
                    <EnrichmentOverridePicker
                      titleId={title.id}
                      provider="musicbrainz"
                      artistScope={title.track?.artistName ?? ""}
                      onApplied={(d) => setGenres(d.genres)}
                    />
                  ),
                },
              ]}
            />
          )}
        </div>
      </div>
    </article>
  );
}
