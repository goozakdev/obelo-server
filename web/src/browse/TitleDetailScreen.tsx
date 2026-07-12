import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Navigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type {
  AudioStream,
  CollectionSummary,
  Edition,
  MediaFile,
  SubtitleTrack,
  TitleDetail,
  VideoStream,
} from "../api/types";
import type { TitleSummary } from "../api/types";
import { useAuth } from "../auth/session";
import { useQueue } from "../player/queue/useQueue";
import {
  buildShowQueue,
  buildSingleQueue,
} from "../player/queue/buildQueue";
import { entryFromTitle } from "../player/queue/model";
import { useAsync } from "./useAsync";
import { useTargetedScan } from "./useTargetedScan";
import { errorMessage } from "../screens/errorMessage";
import { posterUrl } from "./Poster";
import TitleLogo from "./TitleLogo";
import CastStrip from "./CastStrip";
import DetailBackdrop from "./DetailBackdrop";
import AppHeader from "./AppHeader";
import BackLink, { useLibraryName } from "./BackLink";
import {
  WatchlistIcon,
  WatchedIcon,
  EditIcon,
  MoreIcon,
} from "./ActionIcons";
import EnrichmentOverridePicker from "../admin/EnrichmentOverridePicker";
import EditItemDialog from "../admin/EditItemDialog";
import { ArtworkPicker, LockBadge } from "../admin/FixLabel";
import { titleDetailSummary } from "./titleSummary";
import { episodeContextCode } from "./episodeLabel";
import { formatDate, formatDuration, formatTimecode } from "../time";
import { downloadVlcPlaylist } from "./openInVlc";

// The Title detail page (issue 03 / PRD user stories 12–13): GET /titles/{id}
// rendered as summary/year/artwork + the Editions → Files (quality/version
// info, Streams) + a watch-state indicator (watched badge or resume marker).
//
// Issue 04 wires this page: the Play/Resume button navigates to the player
// route, and a manual watched/unwatched toggle calls PUT /titles/{id}/watchState
// and reflects the server-resolved result locally (the server owns the
// threshold; a manual toggle bypasses it).
//
// Scope: this is the MOVIE / TV-EPISODE detail. Music tracks have their own
// detail (music/TrackDetailScreen) under /music/tracks/{id}; a stale /titles/{id}
// link to a track is redirected there below.

export default function TitleDetailScreen() {
  const { titleId = "" } = useParams();
  // A Targeted scan (ADR-0030) bumps this so the detail refetches in place once
  // the scan settles, surfacing any newly-added / now-missing File.
  const [reloadKey, setReloadKey] = useState(0);
  const state = useAsync(
    (signal) => apiClient.getTitle(titleId, signal),
    [titleId, reloadKey],
  );
  const title = state.status === "ready" ? state.data : undefined;
  // Targeted scan of this Movie's folder (ADR-0030), Admin-only. On completion it
  // bumps reloadKey → useAsync refetches → Detail remounts with the fresh File set.
  const {
    scanning: titleScanning,
    message: scanMessage,
    scan: runScan,
  } = useTargetedScan(() => setReloadKey((k) => k + 1));
  // An Episode returns to its Show; a Movie returns to its owning Library (named
  // from the app-wide Libraries list). Until the detail loads we don't yet know
  // which — fall back to Home so the link is never dead on the loading/error view.
  const libraryName = useLibraryName(title?.libraryId);
  const parent = title?.episode
    ? { to: `/shows/${title.episode.showId}`, label: title.episode.showTitle }
    : title
      ? { to: `/libraries/${title.libraryId}`, label: libraryName }
      : { to: "/", label: "Home" };

  // A Track that lands here (an old bookmark / deep link) belongs to the music
  // experience — redirect to the music track detail so the URL and shell match.
  if (state.status === "ready" && state.data.kind === "track") {
    return <Navigate to={`/music/tracks/${state.data.id}`} replace />;
  }

  return (
    <div className="app-shell" data-testid="title-detail-screen">
      <AppHeader />
      <main className="app-main app-main-wide app-main-left">
        <BackLink to={parent.to} label={parent.label} />

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="detail-loading">
            Loading title&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p className="status status-error" data-testid="detail-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && (
          <Detail
            title={state.data}
            onScan={() => runScan("titles", state.data.id)}
            scanning={titleScanning}
            scanMessage={scanMessage}
          />
        )}
      </main>
    </div>
  );
}

function Detail({
  title: initialTitle,
  onScan,
  scanning,
  scanMessage,
}: {
  title: TitleDetail;
  /** Trigger a Targeted scan of this Movie's folder (ADR-0030). */
  onScan: () => void;
  scanning: boolean;
  scanMessage: string | null;
}) {
  const { isAdmin } = useAuth();
  const queue = useQueue();
  // The full title, held in state so an Admin correction (Fix info Enrichment
  // override / metadata edit) refreshes the WHOLE object — display title, poster,
  // and the external id the Fix info picker shows as "current record" — at once,
  // without a reload. The correction endpoints return the updated detail.
  const [title, setTitle] = useState(initialTitle);
  // Local watch state, seeded from the server and updated optimistically-after-
  // confirmation by the manual toggle. Keeping it here (rather than refetching
  // the whole detail) gives instant feedback; the server's resolved values are
  // authoritative, so we write back exactly what it returns.
  const [watched, setWatched] = useState(title.watched);
  const [resumeMs, setResumeMs] = useState(title.resumePositionMs);
  const [toggling, setToggling] = useState(false);
  const [toggleError, setToggleError] = useState<string | null>(null);
  // Local overview + lockedFields, seeded from the server and rewritten from the
  // detail the metadata-edit / lock-release endpoints return, so an Admin's edit
  // (and its Locked badge) appears instantly without a full refetch.
  const [overview, setOverview] = useState(title.overview);
  // Local needs-review flag so an Admin's "Mark reviewed" hides the notice at once
  // (the server makes the dismissal stick across rescans).
  const [needsReview, setNeedsReview] = useState(title.needsReview);
  const [reviewing, setReviewing] = useState(false);
  const [reviewError, setReviewError] = useState<string | null>(null);
  // Inline confirmation for the Queue affordances (queue/03): a transient message
  // after appending / inserting this Title, so enqueueing gives feedback without
  // leaving the page.
  const [queueNotice, setQueueNotice] = useState<string | null>(null);
  // "Add to watchlist" (watchlist 01): busy + inline result of the POST that adds
  // this Title to the User's system Watchlist.
  const [watchlisting, setWatchlisting] = useState(false);
  const [watchlistNotice, setWatchlistNotice] = useState<string | null>(null);
  const [watchlistError, setWatchlistError] = useState<string | null>(null);
  // The Admin "Add to collection" panel the overflow (⋯) menu opens.
  const [collectionOpen, setCollectionOpen] = useState(false);

  const resuming = !watched && resumeMs > 0;
  // The first present (non-missing) File across the Editions — the one Play falls
  // back to and the target of "Open in VLC". A Title is playable iff one exists.
  const primaryFile = title.editions
    .flatMap((ed) => ed.files)
    .find((f) => !f.missing);
  const playable = primaryFile !== undefined;

  // "Open in VLC" (relocated to the toolbar's overflow menu): download a one-track
  // .xspf pointing at the sessionless direct-file URL (self-authenticating via
  // ?token=), which the OS hands to VLC. Targets the primary playable File.
  function openInVlc() {
    if (!primaryFile) return;
    const url = apiClient.directFileDownloadUrl(primaryFile.id);
    if (!url) return;
    downloadVlcPlaylist(url, title.title);
  }

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

  // Play through the Queue (queue/02). The Play affordance builds a play context
  // from the Title's KIND and parent context, then playback begins in the persistent
  // Now Playing bar (now-playing-bar/01) — NO navigation. An Episode plays its Show
  // from here (buildShowQueue, cross-season), and a Movie/standalone Title is a
  // single-entry Queue. A failed build (transient/404) falls back to a single-entry
  // Queue of THIS Title so the player is never stranded (story 39). (Tracks have
  // their own detail/play in music/; this screen serves movie + episode only.)
  async function play() {
    const summary: TitleSummary = titleDetailSummary(title, watched, resumeMs);
    try {
      if (title.kind === "episode" && title.episode) {
        // buildShowQueue plays the now-playing batch itself and resolves once it
        // can; the cross-season tail fills in lazily (we don't await it).
        await buildShowQueue(
          apiClient,
          { showId: title.episode.showId, seasonId: title.episode.seasonId },
          title.id,
          queue,
        );
      } else {
        queue.playNow(buildSingleQueue(summary));
      }
    } catch {
      // A transient/404 build failure never strands the player — fall back to
      // playing just this Title.
      queue.playNow(buildSingleQueue(summary));
    }
  }

  // "Add to queue" (story 21–22, 29): append THIS Title to the END of the Queue.
  // Allowed for any Title — a different Album/Show, a different media kind, and as
  // a duplicate (the Queue is a sequence, not a set); the store's `enqueue` never
  // disturbs what's playing. "Play next" (story 23): insert it immediately after
  // the now-playing entry via `playNext`. Both carry the LIVE watch state so a
  // queued entry resumes from the right spot.
  function addToQueue() {
    queue.enqueue([entryFromTitle(titleDetailSummary(title, watched, resumeMs))]);
    setQueueNotice("Added to the end of the queue.");
  }
  function playNextInQueue() {
    queue.playNext([entryFromTitle(titleDetailSummary(title, watched, resumeMs))]);
    setQueueNotice("Playing next.");
  }

  // "Add to watchlist" (watchlist 01): POST this Title to the User's system
  // Watchlist. The server ensures the Watchlist exists, so this is a single call;
  // a KIND_MISMATCH (once the Watchlist is typed to movies, a non-movie) or a
  // transient failure surfaces inline without leaving the page.
  async function addToWatchlist() {
    if (watchlisting) return;
    setWatchlisting(true);
    setWatchlistNotice(null);
    setWatchlistError(null);
    try {
      await apiClient.addToWatchlist(title.id);
      setWatchlistNotice("Added to your watchlist.");
    } catch (err) {
      setWatchlistError(errorMessage(err));
    } finally {
      setWatchlisting(false);
    }
  }

  // Edit-item (ADR-0019), Admin-only. Its trigger is the Edit icon in the toolbar
  // (renderTrigger), but the dialog itself is a modal so it can live anywhere. Two
  // tabs in one "Edit item" dialog:
  //   • "Search" (item-editing/unified-search) — search or paste a provider URL/id,
  //     pick a candidate, then apply. Update = Fix info (an Enrichment override:
  //     re-points WHICH record decorates the item; keeps identity + watch state).
  //     Replace = Wrong item (the DESTRUCTIVE identity correction: a genuinely
  //     different work — resets watch state + clears Locks). Replace shows on a Movie
  //     ONLY (a redirect sends Tracks elsewhere and Episodes have no per-episode
  //     anchor), gated by passing onReplace.
  //   • "Fix label" (item-editing/03) — hand-edit the descriptive fields or pick a
  //     specific provider image; each edit Locks that field. Changes only the LABEL,
  //     never identity or watch state, and never cascades.
  const editItemDialog = isAdmin ? (
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
            <FixLabelEditor
              title={title}
              onUpdated={(d) => {
                setTitle(d);
                setOverview(d.overview);
              }}
            />
          ),
        },
        {
          key: "search",
          label: "Search",
          node: (
            <EnrichmentOverridePicker
              titleId={title.id}
              provider="tmdb"
              currentExternalId={title.tmdbId}
              onApplied={(d) => {
                setTitle(d);
                setOverview(d.overview);
                setWatched(d.watched);
                setResumeMs(d.resumePositionMs);
              }}
              // Replace (identity correction) is Movie-only.
              onReplace={
                title.kind === "movie"
                  ? (c) =>
                      apiClient.applyTitleIdentityCorrection(title.id, {
                        externalId: c.externalId,
                        title: c.title,
                        year: c.year,
                      })
                  : undefined
              }
            />
          ),
        },
        // Per-role artwork tabs (artwork-management/01, ADR-0026): a Movie manages
        // Poster + Background + Logo from dedicated tabs that auto-search on open
        // and apply + Lock on click. An Episode leaf gets no artwork tab (Episode
        // stills are out of scope), so gate on the Movie kind.
        ...(title.kind === "movie"
          ? (
              [
                ["poster", "Poster"],
                ["background", "Background"],
                ["logo", "Logo"],
              ] as const
            ).map(([role, label]) => ({
              key: role,
              label,
              node: (
                <ArtworkPicker
                  role={role}
                  label={label}
                  locked={title.lockedFields.includes(role)}
                  listCandidates={(r) => apiClient.searchTitleArtworkCandidates(title.id, r)}
                  pick={async (r, url) => {
                    const d = await apiClient.pickTitleArtwork(title.id, r, url);
                    setTitle(d);
                    setOverview(d.overview);
                  }}
                  release={async (field) => {
                    const d = await apiClient.releaseLock(title.id, field);
                    setTitle(d);
                    setOverview(d.overview);
                  }}
                  upload={async (r, file) => {
                    const d = await apiClient.uploadTitleArtwork(title.id, r, file);
                    setTitle(d);
                    setOverview(d.overview);
                  }}
                />
              ),
            }))
          : []),
      ]}
    />
  ) : null;

  // The hero leads with the logo artwork rather than a poster: the logo names
  // the title in the artist's own lettering, so it replaces BOTH the poster and
  // the text heading (TitleLogo falls back to the heading when there's none —
  // always the case for an Episode, which has no logo role). Cache-bust on the
  // row's path (the cached file changes when an Admin picks a new Logo in the
  // Edit-item dialog) so the hero reloads without a page refresh.
  const logoArt = title.artwork.find((a) => a.role === "logo");
  const logoSrc = logoArt ? posterUrl(title.id, "logo", logoArt.path) : undefined;

  // The Title's fetched Background pinned behind the whole screen (the same
  // fixed, scroll-fading backdrop the Show detail uses — position: fixed, so
  // mounting inside the article still spans the viewport). Rendered from the
  // locally-held title and cache-busted on the row's path, so picking a new
  // Background in the Edit-item dialog swaps it without a page refresh.
  const backgroundArt = title.artwork.find((a) => a.role === "background");
  const backdropSrc = backgroundArt ? posterUrl(title.id, "background", backgroundArt.path) : undefined;

  return (
    <article className="detail" data-testid="detail">
      <DetailBackdrop src={backdropSrc} />
      <div className="detail-hero">
        <div className="detail-info">
          {/* Episode parent context (tv-music issue 01): "The Bear · S01E03" so
              an Episode reads in its Show/Season position. Absent for a Movie. */}
          {title.episode && (
            <p className="detail-context" data-testid="episode-context">
              {title.episode.showTitle} · {episodeContextCode(title.episode)}
            </p>
          )}
          <TitleLogo title={title.title} src={logoSrc} />
          <div className="detail-meta">
            {title.year > 0 && (
              <span data-testid="detail-year">{title.year}</span>
            )}
            {/* Enrichment: content rating / runtime decorate the meta line when
                present (external-metadata-enrichment, user stories 2/33). */}
            {title.contentRating && (
              <span className="badge badge-rating" data-testid="detail-content-rating">
                {title.contentRating}
              </span>
            )}
            {title.runtimeMinutes > 0 && (
              <span data-testid="detail-runtime">{formatRuntime(title.runtimeMinutes)}</span>
            )}
            {title.addedAt && (
              <span data-testid="detail-added">
                Added {formatDate(title.addedAt)}
              </span>
            )}
          </div>

          {/* Enrichment: genres as chips (also drive browse-by-genre). */}
          {title.genres.length > 0 && (
            <ul className="detail-genres" data-testid="detail-genres">
              {title.genres.map((g) => (
                <li key={g} className="genre-chip">
                  {g}
                </li>
              ))}
            </ul>
          )}

          {/* Watch-state indicator (PRD user story 13). */}
          <div className="detail-watchstate" data-testid="watch-state">
            {watched && (
              <span className="badge badge-watched" data-testid="watch-watched">
                Watched
              </span>
            )}
            {resuming && (
              <span className="badge badge-resume" data-testid="watch-resume">
                Resume at {formatTimecode(resumeMs)}
              </span>
            )}
            {!watched && !resuming && (
              <span className="badge badge-unwatched" data-testid="watch-unwatched">
                Unwatched
              </span>
            )}
          </div>

          {/* Action toolbar: Play plus the icon affordances (watchlist 01). Play
              builds the play context's Queue (single Title / show-from-here) and
              opens the player; the icons sit alongside it — add-to-watchlist, the
              watched toggle, Edit (Admin), and the ⋯ overflow (queue actions +
              Admin add-to-collection). */}
          <div className="detail-actions" data-testid="detail-actions">
            <button
              className="auth-submit play-button"
              data-testid="play-button"
              type="button"
              disabled={!playable}
              title={playable ? undefined : "No playable files for this title"}
              onClick={() => void play()}
            >
              {resuming ? "Resume" : "Play"}
            </button>

            {/* Add to watchlist — always available to the authenticated User. */}
            <button
              className="icon-button"
              data-testid="add-to-watchlist-button"
              type="button"
              disabled={watchlisting}
              title="Add to watchlist"
              aria-label="Add to watchlist"
              onClick={() => void addToWatchlist()}
            >
              <WatchlistIcon />
            </button>

            {/* Manual watched/unwatched toggle (bypasses the server threshold); the
                icon fills in when watched and the tooltip reflects the next action. */}
            <button
              className={`icon-button watch-toggle${watched ? " is-active" : ""}`}
              data-testid="watch-toggle"
              type="button"
              disabled={toggling}
              aria-pressed={watched}
              title={watched ? "Mark as unwatched" : "Mark as watched"}
              aria-label={watched ? "Mark as unwatched" : "Mark as watched"}
              onClick={toggleWatched}
            >
              <WatchedIcon />
            </button>

            {/* Edit (ADR-0019), Admin-only: the icon opens the "Edit item" dialog. */}
            {editItemDialog}

            {/* Overflow: queue actions + Open in VLC for everyone, add-to-collection
                for Admins. Open in VLC only when there's a playable File. */}
            <OverflowMenu
              isAdmin={isAdmin}
              onAddToQueue={addToQueue}
              onPlayNext={playNextInQueue}
              onOpenInVlc={playable ? openInVlc : undefined}
              onAddToCollection={() => setCollectionOpen(true)}
              // Scan is a Movie-only Admin action here: an Episode is re-scanned
              // from its Show detail, not this per-Title page (ADR-0030).
              onScan={isAdmin && title.kind === "movie" ? onScan : undefined}
              scanning={scanning}
            />
          </div>

          {scanMessage && (
            <p className="status status-ok" data-testid="scan-notice" role="status">
              {scanMessage}
            </p>
          )}

          {/* Inline results for the toolbar actions (queue append/insert, watchlist
              add, and the watched-toggle failure), kept below the row so a click
              gives feedback without leaving the page. */}
          {queueNotice && (
            <p className="status status-ok" data-testid="queue-notice" role="status">
              {queueNotice}
            </p>
          )}
          {watchlistNotice && (
            <p className="status status-ok" data-testid="watchlist-notice" role="status">
              {watchlistNotice}
            </p>
          )}
          {watchlistError && (
            <p className="status status-error" data-testid="watchlist-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {watchlistError}
            </p>
          )}
          {toggleError && (
            <p className="status status-error" data-testid="watch-toggle-error" role="alert">
              <span className="dot dot-error" aria-hidden="true" />
              {toggleError}
            </p>
          )}

          {/* "Add to collection" (collections-playlists-ui issue 02, Admin only):
              opened from the overflow menu. A Member never gets the menu item; the
              server enforces the scope regardless. */}
          {isAdmin && (
            <AddToCollection
              titleId={title.id}
              open={collectionOpen}
              onClose={() => setCollectionOpen(false)}
            />
          )}

          {needsReview && (
            <div className="notice" data-testid="detail-needs-review">
              <span>This title needs review (filed from a partial match).</span>
              {isAdmin && (
                <button
                  className="nav-link"
                  type="button"
                  data-testid="detail-mark-reviewed"
                  disabled={reviewing}
                  onClick={async () => {
                    setReviewError(null);
                    setReviewing(true);
                    try {
                      await apiClient.reviewTitle(title.id);
                      setNeedsReview(false);
                    } catch (err) {
                      setReviewError(errorMessage(err));
                    } finally {
                      setReviewing(false);
                    }
                  }}
                >
                  {reviewing ? "Marking…" : "Mark reviewed"}
                </button>
              )}
              {reviewError && (
                <span className="status status-error" data-testid="detail-review-error" role="alert">
                  {reviewError}
                </span>
              )}
            </div>
          )}

          {/* Enrichment: tagline + overview + cast (external-metadata-enrichment,
              user stories 2/33). Each renders only when enrichment supplied it. */}
          {title.tagline && (
            <p className="detail-tagline" data-testid="detail-tagline">
              {title.tagline}
            </p>
          )}
          {overview && (
            <p className="detail-overview" data-testid="detail-overview">
              {overview}
            </p>
          )}

        </div>
      </div>

      {/* Cast strip: a horizontally-scrolling row of faces in billing order
          (cast-photos/01). Sits BELOW the poster+info hero so it spans the full
          width rather than being confined to the info column. Renders nothing when
          the cast is empty. */}
      <CastStrip cast={title.cast} />

      <section className="detail-editions">
        <h2 className="section-title">Editions &amp; files</h2>
        {title.editions.length === 0 && (
          <p className="status status-loading" data-testid="no-editions">
            No playable files for this title.
          </p>
        )}
        {title.editions.map((ed) => (
          <EditionBlock key={ed.id} edition={ed} />
        ))}
        <SubtitleSummary subtitles={title.subtitles} />
      </section>
    </article>
  );
}

// AddToCollection is the Admin-only "Add to collection" affordance (collections-
// playlists-ui issue 02, PRD user stories 12–13): the panel opened from the Title
// detail's overflow (⋯) menu. It lists the existing Collections (listCollections)
// plus a "new collection" row, and files THIS Title into the chosen one via
// addCollectionItems (idempotent — re-adding an existing member is a harmless 204
// no-op). Creating a new Collection POSTs it then adds the Title in one go. Loads
// the list lazily the first time it opens, shows a pending state per row during the
// add, and surfaces a refused call (e.g. 422 UNKNOWN_TITLE, or a transient failure)
// as a readable inline message without losing the page. Open/close is CONTROLLED by
// the parent (the menu item toggles it). Only an Admin ever renders it; the server
// enforces the scope regardless.
function AddToCollection({
  titleId,
  open,
  onClose,
}: {
  titleId: string;
  open: boolean;
  onClose: () => void;
}) {
  const [collections, setCollections] = useState<CollectionSummary[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  // The "new collection" sub-form.
  const [newName, setNewName] = useState("");
  const [creating, setCreating] = useState(false);

  const loadCollections = useCallback(async () => {
    setLoading(true);
    setLoadError(null);
    try {
      setCollections(await apiClient.listCollections());
    } catch (err) {
      setLoadError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  // Lazy-load the Collection list the first time the panel opens.
  useEffect(() => {
    if (open && collections === null && !loading && !loadError) {
      void loadCollections();
    }
  }, [open, collections, loading, loadError, loadCollections]);

  async function addTo(collectionId: string, label: string) {
    if (busyId) return;
    setBusyId(collectionId);
    setActionError(null);
    setSuccess(null);
    try {
      // Idempotent: re-adding an existing member is a harmless no-op (204).
      await apiClient.addCollectionItems(collectionId, [titleId]);
      setSuccess(`Added to ${label}.`);
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusyId(null);
    }
  }

  async function createAndAdd() {
    if (creating) return;
    const trimmed = newName.trim();
    if (!trimmed) {
      setActionError("Enter a collection name.");
      return;
    }
    setCreating(true);
    setActionError(null);
    setSuccess(null);
    try {
      const created = await apiClient.createCollection({ name: trimmed });
      await apiClient.addCollectionItems(created.id, [titleId]);
      setNewName("");
      setSuccess(`Added to ${created.name}.`);
      // Refresh the list so the new Collection shows for a subsequent add.
      await loadCollections();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setCreating(false);
    }
  }

  if (!open) return null;

  return (
    <div className="add-to-collection" data-testid="add-to-collection">
      {
        <div className="add-to-collection-panel" data-testid="add-to-collection-panel">
          <div className="add-to-collection-head">
            <span className="add-to-collection-title">Add to collection</span>
            <button
              className="nav-link"
              type="button"
              data-testid="add-to-collection-close"
              aria-label="Close"
              onClick={onClose}
            >
              ✕
            </button>
          </div>
          {loading && (
            <p
              className="status status-loading"
              data-testid="add-to-collection-loading"
            >
              Loading collections&hellip;
            </p>
          )}

          {loadError && (
            <p
              className="status status-error"
              data-testid="add-to-collection-load-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {loadError}{" "}
              <button
                className="nav-link"
                type="button"
                data-testid="add-to-collection-retry"
                onClick={() => void loadCollections()}
              >
                Retry
              </button>
            </p>
          )}

          {!loading && !loadError && collections && (
            <>
              {collections.length > 0 && (
                <ul
                  className="add-to-collection-list"
                  data-testid="add-to-collection-list"
                >
                  {collections.map((c) => (
                    <li key={c.id} className="add-to-collection-item">
                      <button
                        className="nav-link"
                        type="button"
                        data-testid="collection-option"
                        data-collection-id={c.id}
                        disabled={busyId !== null || creating}
                        onClick={() => void addTo(c.id, c.name)}
                      >
                        {busyId === c.id ? `Adding to ${c.name}…` : c.name}
                      </button>
                    </li>
                  ))}
                </ul>
              )}

              <div className="add-to-collection-new">
                <input
                  className="field-input"
                  data-testid="new-collection-name-input"
                  type="text"
                  value={newName}
                  placeholder="New collection name"
                  onChange={(e) => setNewName(e.target.value)}
                  disabled={creating || busyId !== null}
                />
                <button
                  className="nav-link"
                  type="button"
                  data-testid="create-and-add-button"
                  disabled={creating || busyId !== null}
                  onClick={() => void createAndAdd()}
                >
                  {creating ? "Creating…" : "New collection + add"}
                </button>
              </div>
            </>
          )}

          {success && (
            <p
              className="status status-ok"
              data-testid="add-to-collection-success"
              role="status"
            >
              {success}
            </p>
          )}

          {actionError && (
            <p
              className="status status-error"
              data-testid="add-to-collection-error"
              role="alert"
            >
              <span className="dot dot-error" aria-hidden="true" />
              {actionError}
            </p>
          )}
        </div>
      }
    </div>
  );
}

// OverflowMenu is the ⋯ menu in the Title detail toolbar: the secondary actions that
// don't warrant their own icon. "Add to queue" / "Play next" are available to every
// User; "Add to collection" is Admin-only (the parent passes isAdmin). It owns its
// own open state and closes on an item click, on Escape, or on an outside click.
function OverflowMenu({
  isAdmin,
  onAddToQueue,
  onPlayNext,
  onOpenInVlc,
  onAddToCollection,
  onScan,
  scanning,
}: {
  isAdmin: boolean;
  onAddToQueue: () => void;
  onPlayNext: () => void;
  /** Present only when the Title has a playable File. */
  onOpenInVlc?: () => void;
  onAddToCollection: () => void;
  /** Present only for an Admin on a Movie: a Targeted scan of its folder. */
  onScan?: () => void;
  scanning: boolean;
}) {
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  // Close on an outside click or Escape while open (a lightweight popover; no portal).
  useEffect(() => {
    if (!open) return;
    function onDocDown(e: MouseEvent) {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  // Run an item's action, then close the menu.
  const pick = (fn: () => void) => () => {
    fn();
    setOpen(false);
  };

  return (
    <div className="overflow-menu" ref={wrapRef}>
      <button
        className="icon-button"
        type="button"
        data-testid="overflow-menu-button"
        title="More actions"
        aria-label="More actions"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <MoreIcon />
      </button>

      {open && (
        <div className="overflow-menu-list" role="menu" data-testid="overflow-menu">
          <button
            className="overflow-menu-item add-to-queue"
            type="button"
            role="menuitem"
            data-testid="add-to-queue-button"
            onClick={pick(onAddToQueue)}
          >
            Add to Queue
          </button>
          <button
            className="overflow-menu-item play-next"
            type="button"
            role="menuitem"
            data-testid="play-next-button"
            onClick={pick(onPlayNext)}
          >
            Play Next
          </button>
          {onOpenInVlc && (
            <button
              className="overflow-menu-item open-in-vlc"
              type="button"
              role="menuitem"
              data-testid="open-in-vlc"
              title="Download a playlist that opens this title in VLC"
              onClick={pick(onOpenInVlc)}
            >
              Open in VLC
            </button>
          )}
          {isAdmin && (
            <button
              className="overflow-menu-item"
              type="button"
              role="menuitem"
              data-testid="add-to-collection-button"
              onClick={pick(onAddToCollection)}
            >
              Add to Collection
            </button>
          )}
          {onScan && (
            <button
              className="overflow-menu-item scan-item"
              type="button"
              role="menuitem"
              data-testid="scan-item"
              disabled={scanning}
              title="Re-scan this title's folder for added or changed files"
              onClick={pick(onScan)}
            >
              {scanning ? "Scanning…" : "Scan"}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

// FixLabelEditor is the Admin-only "Fix label" affordance (item-editing/03,
// ADR-0019): hand-edit the full descriptive field set — overview, tagline, content
// rating, release date, studio, runtime, display name, genres, cast — and pick a
// specific provider poster/background. Each save Locks the fields it wrote so
// re-enrichment never overwrites the correction; a Locked field shows a badge + a
// Release control (back to auto). Renaming here changes only the DISPLAY label,
// never identity or watch state — and this box is kept visibly distinct from
// "Fix info" (wrong record) and the later "Wrong item" (wrong work) so a rename is
// never mistaken for a re-identification. Per-item; never cascades.
function FixLabelEditor({
  title,
  onUpdated,
}: {
  title: TitleDetail;
  onUpdated: (detail: TitleDetail) => void;
}) {
  const locked = (f: string) => title.lockedFields.includes(f);
  // One draft per editable field, seeded from the current detail.
  const [overview, setOverview] = useState(title.overview);
  const [tagline, setTagline] = useState(title.tagline);
  const [displayName, setDisplayName] = useState(title.displayTitle || title.title);
  const [contentRating, setContentRating] = useState(title.contentRating);
  const [releaseDate, setReleaseDate] = useState(title.releaseDate);
  const [studio, setStudio] = useState(title.studio);
  const [genres, setGenres] = useState(title.genres.join(", "));
  const [cast, setCast] = useState(title.cast.map((c) => c.person).join("\n"));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  async function run(fn: () => Promise<TitleDetail>) {
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      onUpdated(await fn());
      setSaved(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  // Save writes EVERY field whose draft differs from the current value (each becomes
  // Locked). Genres are the comma-split list; cast is one person per line.
  const save = () =>
    run(async () => {
      const edit: Record<string, unknown> = {};
      if (overview !== title.overview) edit.overview = overview;
      if (tagline !== title.tagline) edit.tagline = tagline;
      if (displayName !== (title.displayTitle || title.title)) edit.title = displayName;
      if (contentRating !== title.contentRating) edit.contentRating = contentRating;
      if (releaseDate !== title.releaseDate) edit.releaseDate = releaseDate;
      if (studio !== title.studio) edit.studio = studio;
      const nextGenres = genres.split(",").map((g) => g.trim()).filter(Boolean);
      if (nextGenres.join(", ") !== title.genres.join(", ")) edit.genres = nextGenres;
      const nextCast = cast.split("\n").map((p) => p.trim()).filter(Boolean);
      if (nextCast.join("\n") !== title.cast.map((c) => c.person).join("\n")) {
        edit.cast = nextCast.map((person) => ({ person, kind: "cast" }));
      }
      return apiClient.editMetadata(title.id, edit);
    });

  const release = (field: string) => run(() => apiClient.releaseLock(title.id, field));

  return (
    <section className="metadata-editor" data-testid="fix-label-editor">
      <h2 className="section-title">Details</h2>
      <p className="detail-hint">
        Type your own information or choose a different provider image. Each edit is
        kept (Locked) so re-enrichment won&rsquo;t overwrite it. This changes only how
        this item is labelled — it never changes identity or watch state.
      </p>

      <FixLabelField label="Display name" field="title" locked={locked("title")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-title" type="text" value={displayName}
          disabled={busy} onChange={(e) => setDisplayName(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Overview" field="overview" locked={locked("overview")} onRelease={release} busy={busy}>
        <textarea className="metadata-input" data-testid="edit-overview" rows={3} value={overview}
          disabled={busy} onChange={(e) => setOverview(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Tagline" field="tagline" locked={locked("tagline")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-tagline" type="text" value={tagline}
          disabled={busy} onChange={(e) => setTagline(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Content rating" field="content_rating" locked={locked("content_rating")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-content-rating" type="text" value={contentRating}
          disabled={busy} onChange={(e) => setContentRating(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Release date" field="release_date" locked={locked("release_date")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-release-date" type="text" value={releaseDate}
          disabled={busy} onChange={(e) => setReleaseDate(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Studio" field="studio" locked={locked("studio")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-studio" type="text" value={studio}
          disabled={busy} onChange={(e) => setStudio(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Genres (comma-separated)" field="genres" locked={locked("genres")} onRelease={release} busy={busy}>
        <input className="metadata-input" data-testid="edit-genres" type="text" value={genres}
          disabled={busy} onChange={(e) => setGenres(e.target.value)} />
      </FixLabelField>
      <FixLabelField label="Cast (one per line)" field="cast" locked={locked("cast")} onRelease={release} busy={busy}>
        <textarea className="metadata-input" data-testid="edit-cast" rows={3} value={cast}
          disabled={busy} onChange={(e) => setCast(e.target.value)} />
      </FixLabelField>

      <div className="metadata-actions">
        <button className="auth-submit" data-testid="save-metadata" type="button" disabled={busy} onClick={save}>
          {busy ? "Saving…" : "Save & lock"}
        </button>
        {saved && (
          <span className="status status-ok" data-testid="metadata-saved" role="status">
            Saved.
          </span>
        )}
      </div>

      {/* The poster/background image pickers moved OUT of Fix label into their own
          Poster and Background tabs (artwork-management/01). Fix label keeps only the
          descriptive-field editing above. */}

      {error && (
        <p className="status status-error" data-testid="metadata-edit-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </section>
  );
}

// FixLabelField wraps one editable field with its label + Locked badge/Release.
function FixLabelField({
  label,
  field,
  locked,
  onRelease,
  busy,
  children,
}: {
  label: string;
  field: string;
  locked: boolean;
  onRelease: (field: string) => void;
  busy: boolean;
  children: ReactNode;
}) {
  return (
    <label className="metadata-field">
      <span className="metadata-label">
        {label}
        <LockBadge field={field} locked={locked} onRelease={onRelease} busy={busy} />
      </span>
      {children}
    </label>
  );
}

// SubtitleSummary lists the Subtitle tracks the Title offers, from every source
// (embedded Streams, sidecar files, online-fetched), as a title-level line under
// the Editions & files section — the server already dedupes and labels them by the
// observable (kind|language|forced), so this is the same set the player's captions
// menu shows. An image track (PGS/VOBSUB) is tagged since it burns in on transcode;
// a fetched track is tagged "online". Renders nothing when the Title has no subtitle.
function SubtitleSummary({ subtitles }: { subtitles: SubtitleTrack[] }) {
  // Defensive: the normalized TitleDetail always carries a list, but a hand-built
  // fixture (or an older server response) may omit it — treat that as "none".
  if (!subtitles || subtitles.length === 0) return null;
  return (
    <div className="edition-subtitles" data-testid="detail-subtitles">
      <h3 className="edition-name">Subtitles</h3>
      <ul className="subtitle-chips">
        {subtitles.map((s) => (
          <li
            key={s.id}
            className="subtitle-chip"
            data-testid="detail-subtitle"
            data-sub-lang={s.language || ""}
            data-sub-kind={s.kind}
            data-sub-source={s.source}
          >
            <span className="subtitle-chip-label">{s.label}</span>
            {s.kind === "image" && (
              <span className="subtitle-chip-tag" title="Bitmap subtitle — burned in when selected">
                image
              </span>
            )}
            {s.source === "fetched" && (
              <span className="subtitle-chip-tag" title="Fetched from an online provider">
                online
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

function EditionBlock({ edition }: { edition: Edition }) {
  return (
    <div className="edition" data-testid="edition" data-edition-id={edition.id}>
      <h3 className="edition-name" data-testid="edition-name">
        {edition.name || "Default"}
      </h3>
      <ul className="file-list">
        {edition.files.map((f) => (
          <FileRow key={f.id} file={f} />
        ))}
      </ul>
    </div>
  );
}

function FileRow({ file }: { file: MediaFile }) {
  const quality = resolutionLabel(file);
  const codecs = [file.videoCodec, file.audioCodec].filter(Boolean).join(" · ");
  // "Open in VLC" now lives on the title toolbar's overflow menu (targeting the
  // primary playable File), so a File row only presents its quality/codec info.
  return (
    <li className="file-row" data-testid="file-row" data-file-id={file.id}>
      <div className="file-primary">
        <span className="file-container" data-testid="file-container">
          {file.container.toUpperCase()}
        </span>
        {quality && (
          <span className="file-quality" data-testid="file-quality">
            {quality}
          </span>
        )}
        {file.missing && (
          <span className="badge badge-missing" data-testid="file-missing">
            Missing
          </span>
        )}
      </div>
      <div className="file-secondary">
        {codecs && <span data-testid="file-codecs">{codecs}</span>}
        {file.durationMs > 0 && <span>{formatDuration(file.durationMs)}</span>}
        {file.bitrate > 0 && <span>{formatBitrate(file.bitrate)}</span>}
      </div>
      <VideoStreamChips streams={file.videoStreams} />
      <AudioStreamChips streams={file.audioStreams} />
    </li>
  );
}

// VideoStreamChips lists a File's selectable video Streams as labeled chips under
// its row (selectable-video, ADR-0025), mirroring the Audio chips: the server's
// catalog projection reuses the same selectable set as playback negotiation
// (non-cover-art video Streams) and builds the label (title tag, else resolution),
// so this just renders them. Gated at ≥2 — a lone video Stream's resolution/codec
// already show in the File row, and the feature (and the player's Video menu) only
// becomes meaningful with alternates. The default carries the container disposition
// at browse time. Per CONTEXT.md these are video Streams, not a coined "Video track".
function VideoStreamChips({ streams }: { streams: VideoStream[] }) {
  // Defensive: a hand-built fixture (or an older server response) may omit the
  // list. Only surface the chips when there's a genuine choice (≥2).
  if (!streams || streams.length < 2) return null;
  return (
    <div className="file-video" data-testid="file-video">
      <ul className="video-chips">
        {streams.map((s) => (
          <li
            key={s.id}
            className="video-chip"
            data-testid="video-stream"
            data-video-codec={s.codec}
            data-video-default={s.isDefault ? "1" : ""}
          >
            <span className="video-chip-label">{s.label}</span>
            <span className="video-chip-codec">{s.codec.toUpperCase()}</span>
            {s.isDefault && (
              <span className="video-chip-tag" title="Plays by default">
                default
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

// AudioStreamChips lists a File's embedded audio Streams as labeled chips under
// its row (audio-streams/01), mirroring the Subtitle chips: the server already
// normalizes the language, resolves the channel layout, and builds the menu
// label, so this just renders them. The default Stream is marked, a commentary
// chip is tagged, and each carries its normalized language/codec as data
// attributes for the integration test. Per CONTEXT.md these are audio Streams,
// not a coined "Audio track". Renders nothing when the File has no audio.
function AudioStreamChips({ streams }: { streams: AudioStream[] }) {
  // Defensive: a hand-built fixture (or an older server response) may omit the
  // list — treat that as "none".
  if (!streams || streams.length === 0) return null;
  return (
    <div className="file-audio" data-testid="file-audio">
      <ul className="audio-chips">
        {streams.map((s) => (
          <li
            key={s.id}
            className="audio-chip"
            data-testid="audio-stream"
            data-audio-lang={s.language || ""}
            data-audio-codec={s.codec}
            data-audio-default={s.isDefault ? "1" : ""}
          >
            <span className="audio-chip-label">{s.label}</span>
            <span className="audio-chip-codec">{s.codec.toUpperCase()}</span>
            {s.isDefault && (
              <span className="audio-chip-tag" title="Plays by default">
                default
              </span>
            )}
            {s.commentary && (
              <span className="audio-chip-tag" title="Commentary track">
                commentary
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

// resolutionLabel turns a File's pixel height into a friendly quality tag
// (2160p/1080p/720p/…), falling back to the raw WxH when it's an odd size.
function resolutionLabel(f: MediaFile): string {
  if (f.height <= 0) return "";
  if (f.height >= 2160) return "4K";
  if (f.height >= 1080) return "1080p";
  if (f.height >= 720) return "720p";
  if (f.height >= 480) return "480p";
  return `${f.width}×${f.height}`;
}

function formatBitrate(bps: number): string {
  const mbps = bps / 1_000_000;
  if (mbps >= 1) return `${mbps.toFixed(1)} Mbps`;
  return `${Math.round(bps / 1000)} kbps`;
}

// formatRuntime renders an enriched runtime (minutes) as "2h 35m" / "47m".
function formatRuntime(minutes: number): string {
  const h = Math.floor(minutes / 60);
  const m = minutes % 60;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  return `${m}m`;
}
