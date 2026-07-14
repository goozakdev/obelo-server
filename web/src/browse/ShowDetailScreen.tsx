import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { apiClient } from "../api/client";
import type {
  EpisodeSummary,
  ResumePoint,
  Season,
  SeasonEpisodes,
  TitleSummary,
} from "../api/types";
import { useQueue } from "../player/queue/useQueue";
import {
  buildFullShowEntries,
  buildShowQueue,
  buildSingleQueue,
  episodeToSummary,
} from "../player/queue/buildQueue";
import { entryFromTitle } from "../player/queue/model";
import { useAsync } from "./useAsync";
import { useTargetedScan } from "./useTargetedScan";
import AppHeader from "./AppHeader";
import BackLink, { useLibraryName } from "./BackLink";
import EpisodeActionsMenu from "./EpisodeActionsMenu";
import TitleLogo from "./TitleLogo";
import CastStrip from "./CastStrip";
import DetailBackdrop from "./DetailBackdrop";
import { EditIcon, MoreIcon } from "./ActionIcons";
import EntityEnrichmentOverridePicker from "../admin/EntityEnrichmentOverridePicker";
import EntityMetadataEditor, { entityArtworkTabs } from "../admin/EntityMetadataEditor";
import EditItemDialog from "../admin/EditItemDialog";
import { useAuth } from "../auth/session";
import { errorMessage } from "../screens/errorMessage";
import { formatTimecode } from "../time";

// The Show detail screen (tv-music issue 01 / PRD user story 12): GET
// /shows/{id}/seasons rendered as the Show header + a Season picker; the SELECTED
// Season's Episodes list in order (via GET /seasons/{id}/episodes). Each Episode
// row is a thumbnail + (episode code/title over its synopsis) + a hover-revealed
// three-dots actions menu (Play next / Add to queue / Edit). Clicking the
// thumbnail or the text block plays that Episode: it builds the show-from-here
// Queue — the chosen Episode forward through its Season, then the following
// Seasons in order (cross-season) — into the persistent Now Playing bar. The
// now-playing entry resolves after a single fetch so playback starts without
// waiting on the whole cross-season walk; a failed build falls back to playing
// just the chosen Episode.

export default function ShowDetailScreen() {
  const { showId = "" } = useParams();
  const navigate = useNavigate();
  const queue = useQueue();
  const { isAdmin } = useAuth();
  // Bumped after an Admin Fix-info so the detail re-fetches and reflects the fix.
  const [reloadKey, setReloadKey] = useState(0);
  const state = useAsync(
    (signal) => apiClient.getShowSeasons(showId, signal),
    [showId, reloadKey],
    // Keep the detail on screen during a post-edit reload so the Edit-item picker
    // stays mounted and its cascade summary survives (item-editing/05).
    { keepPreviousData: true },
  );

  // A Show returns to its owning Library (named from the app-wide Libraries list);
  // until the detail loads, fall back to Home so the link is never dead.
  const show = state.status === "ready" ? state.data.show : undefined;
  // Targeted scan of this Show's folder (ADR-0030), Admin-only. On completion it
  // bumps reloadKey → the detail refetches in place with the new Episode set.
  const {
    scanning,
    message: scanMessage,
    scan: runScan,
  } = useTargetedScan(() => setReloadKey((k) => k + 1));
  const libraryName = useLibraryName(show?.libraryId);
  const parent = show
    ? { to: `/libraries/${show.libraryId}`, label: libraryName }
    : { to: "/", label: "Home" };

  // The Season chosen in the picker. Empty until the user picks one; the render
  // then falls back to the default Season (first non-specials, else first).
  const [selectedSeasonId, setSelectedSeasonId] = useState("");
  const seasons = state.status === "ready" ? state.data.seasons : [];
  const activeSeason = useMemo(
    () =>
      seasons.find((s) => s.id === selectedSeasonId) ??
      seasons.find((s) => !s.specials) ??
      seasons[0],
    [seasons, selectedSeasonId],
  );

  // Where the toolbar's Play button starts the series: the first non-specials
  // Season with Episodes (falling back to the first Season with any, so a
  // specials-only Show is still playable). The Show is playable iff one exists.
  const startSeason = useMemo(
    () =>
      seasons.find((s) => !s.specials && s.episodeCount > 0) ??
      seasons.find((s) => s.episodeCount > 0),
    [seasons],
  );
  const playable = startSeason !== undefined;

  // The Show's resume point (issue 02, ADR-0028): when present it replaces the
  // Show description + Play with the anchor Episode's block (Continue + Restart for
  // an in-progress anchor, a single Play for a fresh next Episode). When null the
  // page reverts to the Show description — with Play only when the Show is NOT
  // started. The two null cases are told apart by unwatchedEpisodeCount: a
  // not-started Show still has unwatched Episodes; a fully-watched one has none
  // (its Play is dropped — restarting a finished series is not a flow).
  const resumePoint = state.status === "ready" ? state.data.resumePoint : null;
  const notStarted = !resumePoint && (show?.unwatchedEpisodeCount ?? 0) > 0;

  // Inline confirmation / failure for the toolbar's whole-series Queue actions, so
  // an "Add to queue" / "Play next" gives feedback without leaving the page.
  const [queueNotice, setQueueNotice] = useState<string | null>(null);
  const [queueError, setQueueError] = useState<string | null>(null);

  // Build the show-from-here Queue; playback begins in the persistent Now Playing
  // bar (now-playing-bar/01), NO navigation. buildShowQueue plays the now-playing
  // batch itself (and lazily appends later Seasons); a first-fetch failure falls
  // back to a single-entry Queue of just that Episode (story 39).
  const play = useCallback(
    async (episode: EpisodeSummary, seasonId: string) => {
      try {
        await buildShowQueue(apiClient, { showId, seasonId }, episode.id, queue);
      } catch {
        queue.playNow(buildSingleQueue(episodeToSummary(episode)));
      }
    },
    [showId, queue],
  );

  // Resume-point Play / Continue / Restart: all build the SAME cross-season
  // show-from-here Queue with the resume-point Episode as the head (buildShowQueue);
  // only where the head starts differs (ADR-0028) — Continue at the anchor's stored
  // resume, Restart and the next-Episode Play at 0. Starting from 0 goes through the
  // playback path, which re-stamps played_at and keeps the anchor pinned to that
  // Episode (Restart re-watches it without flinging the resume point elsewhere). A
  // first-fetch failure falls back to a single-entry Queue of just that Episode.
  const playResume = useCallback(
    async (rp: ResumePoint, headResumeMs: number) => {
      try {
        await buildShowQueue(
          apiClient,
          { showId, seasonId: rp.seasonId },
          rp.id,
          queue,
          undefined,
          { headResumeMs },
        );
      } catch {
        queue.playNow(buildSingleQueue(resumePointToSummary(rp, headResumeMs)));
      }
    },
    [showId, queue],
  );

  // Toolbar "Play": start the series from the beginning — the first Episode of the
  // start Season, walking the Show forward from there (buildShowQueue: cross-season,
  // now-playing resolves after a single fetch, the tail fills in lazily). Fetching
  // the start Season's Episodes first gives us the first Episode to hand `play`,
  // which already carries the single-Episode fallback (story 39).
  const playFromStart = useCallback(async () => {
    if (!startSeason) return;
    try {
      const { episodes } = await apiClient.getSeasonEpisodes(startSeason.id);
      const first = episodes[0];
      if (first) await play(first, startSeason.id);
    } catch {
      // Couldn't load the start Season's Episodes — nothing to start.
    }
  }, [startSeason, play]);

  // Toolbar overflow "Add to queue" / "Play next": the WHOLE series (every Season's
  // Episodes in order) appended to / inserted after the now-playing entry, without
  // disturbing what's playing. buildFullShowEntries resolves the full series up
  // front; a fetch failure surfaces inline.
  const addSeriesToQueue = useCallback(async () => {
    setQueueNotice(null);
    setQueueError(null);
    try {
      const entries = await buildFullShowEntries(apiClient, showId);
      if (entries.length === 0) return;
      queue.enqueue(entries);
      setQueueNotice("Added the series to the end of the queue.");
    } catch (err) {
      setQueueError(errorMessage(err));
    }
  }, [showId, queue]);

  const playSeriesNext = useCallback(async () => {
    setQueueNotice(null);
    setQueueError(null);
    try {
      const entries = await buildFullShowEntries(apiClient, showId);
      if (entries.length === 0) return;
      queue.playNext(entries);
      setQueueNotice("Playing the series next.");
    } catch (err) {
      setQueueError(errorMessage(err));
    }
  }, [showId, queue]);

  // Insert this Episode right after the now-playing entry ("Play next").
  const playNext = useCallback(
    (episode: EpisodeSummary) => {
      queue.playNext([entryFromTitle(episodeToSummary(episode))]);
    },
    [queue],
  );

  // Append this Episode at the end of the Queue ("Add to queue").
  const addToQueue = useCallback(
    (episode: EpisodeSummary) => {
      queue.enqueue([entryFromTitle(episodeToSummary(episode))]);
    },
    [queue],
  );

  return (
    <div className="app-shell" data-testid="show-detail-screen">
      {/* The Show's TMDB Background pinned behind the whole screen; content
          scrolls over it and it fades toward black (capped at 50%). */}
      <DetailBackdrop src={show?.backgroundUrl} />
      <AppHeader />
      {/* app-main-full (not -wide): the episode grid scales to fill the window,
          up to a 4K screen. */}
      <main className="app-main app-main-full app-main-left">
        <BackLink to={parent.to} label={parent.label} />

        {state.status === "loading" && (
          <p className="status status-loading" data-testid="show-loading">
            Loading show&hellip;
          </p>
        )}

        {state.status === "error" && (
          <p className="status status-error" data-testid="show-error" role="alert">
            <span className="dot dot-error" aria-hidden="true" />
            {state.message}
          </p>
        )}

        {state.status === "ready" && (
          <article className="detail" data-testid="show-detail">
            <div className="detail-hero">
              <div className="detail-info">
                {/* The hero leads with the Show's logo artwork rather than a
                    poster: the logo names the series in its own lettering, so it
                    replaces both the poster and the text heading (TitleLogo falls
                    back to the heading when Enrichment fetched no logo). logoUrl
                    already carries the server's artwork-version cache-bust. */}
                <TitleLogo
                  title={state.data.show.title}
                  src={state.data.show.logoUrl}
                  testId="show-title"
                />
                <div className="detail-meta">
                  {state.data.show.year > 0 && (
                    <span data-testid="show-year">{state.data.show.year}</span>
                  )}
                  {state.data.show.contentRating && (
                    <span data-testid="show-content-rating">{state.data.show.contentRating}</span>
                  )}
                  {state.data.show.network && (
                    <span data-testid="show-network">{state.data.show.network}</span>
                  )}
                </div>
                {(state.data.show.genres ?? []).length > 0 && (
                  <div className="detail-genres" data-testid="show-genres">
                    {(state.data.show.genres ?? []).join(" · ")}
                  </div>
                )}

                {/* Resume point (issue 02, ADR-0028): the next-episode block that
                    replaces the fixed description + Play once the Show is started
                    and not yet fully watched. Continue/Restart (in-progress) or a
                    single Play (next); all build the show-from-here Queue from this
                    Episode, differing only in the head start offset. */}
                {resumePoint && (
                  <ResumePointBlock
                    resumePoint={resumePoint}
                    onContinue={() => void playResume(resumePoint, resumePoint.resumePositionMs)}
                    onRestart={() => void playResume(resumePoint, 0)}
                    onPlay={() => void playResume(resumePoint, 0)}
                  />
                )}

                {/* Action toolbar (mirrors the Movie detail): a primary Play plus
                    the icon affordances. Play builds the show-from-the-beginning
                    Queue and starts playback in the persistent Now Playing bar —
                    NO navigation. Edit (Admin) opens the "Edit item" dialog, and the
                    ⋯ overflow carries the whole-series Queue actions. */}
                <div className="detail-actions" data-testid="detail-actions">
                  {/* The whole-series Play shows only for a NOT-started Show (series
                      from the first Episode — unchanged from today). A started Show
                      plays from its resume point above; a fully-watched Show reverts
                      to the description with no Play. */}
                  {notStarted && (
                    <button
                      className="auth-submit play-button"
                      data-testid="play-button"
                      type="button"
                      disabled={!playable}
                      title={playable ? undefined : "No episodes to play for this show"}
                      onClick={() => void playFromStart()}
                    >
                      Play
                    </button>
                  )}

                  {/* Edit-item (ADR-0019), Admin-only. The Edit icon opens one "Edit
                      item" dialog with two tabs — this replaces the former standalone
                      "Edit item" link so the Show toolbar matches the Movie one:
                      • "Search" (item-editing/unified-search) — search or paste a
                        provider URL/id, pick a series, then apply. Update = Fix info
                        (an Enrichment override: re-points WHICH series decorates the
                        Show; keeps identity). Replace = Wrong item (the DESTRUCTIVE
                        identity correction: a genuinely different series — re-keys
                        identity across rescans, resets every Episode's watch state,
                        clears the Show's Locks). A ticked "also apply to children"
                        applies to whichever button. Replace is gated by onReplace.
                      • "Fix label" (item-editing/03) — edit the Show's descriptive
                        fields or pick a poster/background; each edit is Locked,
                        changes only the label, never identity, never cascades. */}
                  {isAdmin && (
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
                              entityType="shows"
                              entityId={state.data.show.id}
                              displayName={state.data.show.title}
                              overview={state.data.show.overview}
                              contentRating={state.data.show.contentRating}
                              network={state.data.show.network}
                              genres={state.data.show.genres}
                              lockedFields={state.data.show.lockedFields}
                              onChanged={() => setReloadKey((k) => k + 1)}
                            />
                          ),
                        },
                        {
                          key: "search",
                          label: "Search",
                          node: (
                            <EntityEnrichmentOverridePicker
                              entityType="shows"
                              entityId={state.data.show.id}
                              currentExternalId={state.data.show.enrichmentOverride?.externalId}
                              initialQuery={state.data.show.title}
                              onApplied={() => setReloadKey((k) => k + 1)}
                              onReplace={(c, cascade) =>
                                apiClient.applyShowIdentityCorrection(state.data.show.id, {
                                  externalId: c.externalId,
                                  title: c.title,
                                  year: c.year,
                                  cascade,
                                })
                              }
                            />
                          ),
                        },
                        // Poster + Background artwork tabs (artwork-management/01):
                        // auto-search on open, apply + Lock on click.
                        ...entityArtworkTabs("shows", state.data.show.id, state.data.show.lockedFields, () =>
                          setReloadKey((k) => k + 1),
                        ),
                      ]}
                    />
                  )}

                  {/* Overflow (⋯): whole-series Queue actions. The Movie menu's
                      "Open in VLC" (a Show has no single File) and "Add to collection"
                      (Collections hold Titles, not Shows) don't apply to a Show. */}
                  <ShowOverflowMenu
                    onAddToQueue={() => void addSeriesToQueue()}
                    onPlayNext={() => void playSeriesNext()}
                    onScan={
                      isAdmin ? () => runScan("shows", state.data.show.id) : undefined
                    }
                    scanning={scanning}
                  />
                </div>

                {scanMessage && (
                  <p className="status status-ok" data-testid="scan-notice" role="status">
                    {scanMessage}
                  </p>
                )}
                {queueNotice && (
                  <p className="status status-ok" data-testid="queue-notice" role="status">
                    {queueNotice}
                  </p>
                )}
                {queueError && (
                  <p className="status status-error" data-testid="queue-error" role="alert">
                    <span className="dot dot-error" aria-hidden="true" />
                    {queueError}
                  </p>
                )}

                {/* The Show description shows only WITHOUT a resume point (a not-
                    started or fully-watched Show); a started Show's hero leads with
                    the resume-point Episode's synopsis instead. */}
                {!resumePoint && state.data.show.overview && (
                  <p className="detail-overview" data-testid="show-overview">
                    {state.data.show.overview}
                  </p>
                )}
              </div>
            </div>

            {/* Cast strip: the same horizontally-scrolling row of faces the
                  Movie detail renders (cast-photos/02). Renders nothing when the
                  Show has no captured cast. */}
              <CastStrip cast={state.data.show.cast ?? []} />

            {state.data.seasons.length === 0 && (
              <p className="status status-loading" data-testid="no-seasons">
                No seasons indexed for this show yet.
              </p>
            )}

            {activeSeason && (
              <div className="season-picker">
                <label
                  className="season-picker-label"
                  htmlFor="season-select"
                >
                  Season
                </label>
                <select
                  id="season-select"
                  className="season-select"
                  data-testid="season-select"
                  value={activeSeason.id}
                  onChange={(e) => setSelectedSeasonId(e.target.value)}
                >
                  {state.data.seasons.map((season) => (
                    <option key={season.id} value={season.id}>
                      {seasonLabel(season)}
                    </option>
                  ))}
                </select>
              </div>
            )}

            {activeSeason && (
              <SeasonBlock
                key={activeSeason.id}
                season={activeSeason}
                onPlay={(episode) => void play(episode, activeSeason.id)}
                onPlayNext={playNext}
                onAddToQueue={addToQueue}
                onEdit={(episode) => navigate(`/titles/${episode.id}`)}
              />
            )}
          </article>
        )}
      </main>
    </div>
  );
}

function seasonLabel(season: Season): string {
  return season.specials ? "Specials" : `Season ${season.seasonNumber}`;
}

// The resume-point Episode's S/E code: "S01E03" (zero-padded), or its degraded-
// offline label when there's no reliable episode number.
function resumePointCode(rp: ResumePoint): string {
  if (rp.episodeLabel) return rp.episodeLabel;
  const s = String(rp.seasonNumber).padStart(2, "0");
  const e = String(rp.episodeNumber).padStart(2, "0");
  return `S${s}E${e}`;
}

// The single-Episode fallback summary a failed resume-point Queue build plays:
// the resume-point Episode as a playable Title, seeded with the head start offset
// (Continue's resume vs. 0) so the bar resumes at the right spot.
function resumePointToSummary(rp: ResumePoint, headResumeMs: number): TitleSummary {
  return {
    id: rp.id,
    kind: rp.kind,
    title: rp.title,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: headResumeMs,
    watched: false,
    genres: [],
    enrichmentStatus: rp.enrichmentStatus,
  };
}

// ResumePointBlock is the Show detail hero's next-episode block (issue 02,
// ADR-0028): the resume-point Episode's S/E code · title · synopsis, plus the
// controls its mode selects — Continue (resume where you left off) + Restart (from
// 0) for an in-progress anchor, or a single Play (from 0) for a fresh next Episode.
function ResumePointBlock({
  resumePoint,
  onContinue,
  onRestart,
  onPlay,
}: {
  resumePoint: ResumePoint;
  onContinue: () => void;
  onRestart: () => void;
  onPlay: () => void;
}) {
  const inProgress = resumePoint.mode === "inProgress";
  // The Continue progress bar only makes sense when the Episode can be resumed
  // (in-progress) and we know its duration; otherwise it's hidden. The fill is
  // clamped to [0,100]% and the label rounds the remaining time up to whole minutes
  // (never below "1 min left" while there's a resume position to continue from).
  const showProgress = inProgress && resumePoint.durationMs > 0;
  const playedFraction = showProgress
    ? Math.min(1, Math.max(0, resumePoint.resumePositionMs / resumePoint.durationMs))
    : 0;
  const minutesLeft = showProgress
    ? Math.max(1, Math.ceil((resumePoint.durationMs - resumePoint.resumePositionMs) / 60000))
    : 0;
  return (
    <div className="detail-resume-point" data-testid="resume-point" data-mode={resumePoint.mode}>
      <div className="resume-point-heading">
        <span className="resume-point-code" data-testid="resume-point-code">
          {resumePointCode(resumePoint)}
        </span>
        <span className="resume-point-title" data-testid="resume-point-title">
          {resumePoint.title}
        </span>
      </div>
      {resumePoint.overview && (
        <p className="detail-overview" data-testid="resume-point-synopsis">
          {resumePoint.overview}
        </p>
      )}
      {showProgress && (
        <div className="resume-point-progress" data-testid="resume-point-progress">
          <div
            className="resume-progress-track"
            role="progressbar"
            aria-label="Episode progress"
            aria-valuemin={0}
            aria-valuemax={resumePoint.durationMs}
            aria-valuenow={resumePoint.resumePositionMs}
          >
            <div
              className="resume-progress-fill"
              data-testid="resume-progress-fill"
              style={{ width: `${playedFraction * 100}%` }}
            />
          </div>
          <span className="resume-progress-remaining" data-testid="resume-progress-remaining">
            {minutesLeft} min left
          </span>
        </div>
      )}
      <div className="resume-point-actions" data-testid="resume-point-actions">
        {inProgress ? (
          <>
            <button
              className="auth-submit play-button"
              data-testid="continue-button"
              type="button"
              onClick={onContinue}
            >
              Continue
            </button>
            <button
              className="nav-link reorder-button restart-button"
              data-testid="restart-button"
              type="button"
              onClick={onRestart}
            >
              Restart
            </button>
          </>
        ) : (
          <button
            className="auth-submit play-button"
            data-testid="resume-play-button"
            type="button"
            onClick={onPlay}
          >
            Play
          </button>
        )}
      </div>
    </div>
  );
}

// ShowOverflowMenu is the ⋯ menu in the Show detail toolbar — the secondary
// whole-series actions that don't warrant their own icon. It mirrors the Movie
// detail's overflow menu but carries only the two that apply to a Show: "Add to
// Queue" (append the whole series) and "Play Next" (insert it after the now-playing
// entry). It owns its open state and closes on an item click, Escape, or an outside
// click.
function ShowOverflowMenu({
  onAddToQueue,
  onPlayNext,
  onScan,
  scanning,
}: {
  onAddToQueue: () => void;
  onPlayNext: () => void;
  /** Present only for an Admin: a Targeted scan of this Show's folder (ADR-0030). */
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
          {onScan && (
            <button
              className="overflow-menu-item scan-item"
              type="button"
              role="menuitem"
              data-testid="scan-item"
              disabled={scanning}
              title="Re-scan this show's folder for added or changed files"
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

// SeasonBlock loads and renders one Season's Episodes lazily (its own GET
// /seasons/{id}/episodes), so a Show with many seasons doesn't fan out into one
// huge request and each season's list paints as it arrives.
function SeasonBlock({
  season,
  onPlay,
  onPlayNext,
  onAddToQueue,
  onEdit,
}: {
  season: Season;
  onPlay: (episode: EpisodeSummary) => void;
  onPlayNext: (episode: EpisodeSummary) => void;
  onAddToQueue: (episode: EpisodeSummary) => void;
  onEdit: (episode: EpisodeSummary) => void;
}) {
  const state = useAsync<SeasonEpisodes>(
    (signal) => apiClient.getSeasonEpisodes(season.id, signal),
    [season.id],
  );

  return (
    <section
      className="detail-editions season-block"
      data-testid="season-block"
      data-season-id={season.id}
      data-season-number={season.seasonNumber}
    >
      <h2 className="section-title" data-testid="season-title">
        <span className="season-count">
          {season.episodeCount} {season.episodeCount === 1 ? "episode" : "episodes"}
        </span>
      </h2>

      {state.status === "loading" && (
        <p className="status status-loading">Loading episodes&hellip;</p>
      )}
      {state.status === "error" && (
        <p className="status status-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}
      {state.status === "ready" && (
        <ul className="episode-list" data-testid="episode-list">
          {state.data.episodes.map((ep) => (
            <EpisodeRow
              key={ep.id}
              episode={ep}
              onPlay={() => onPlay(ep)}
              onPlayNext={() => onPlayNext(ep)}
              onAddToQueue={() => onAddToQueue(ep)}
              onEdit={() => onEdit(ep)}
            />
          ))}
        </ul>
      )}
    </section>
  );
}

function episodeCode(ep: EpisodeSummary): string {
  // A canonical episode shows just its episode number; a degraded-offline one
  // (no reliable number) shows its label instead.
  if (ep.episodeLabel) return ep.episodeLabel;
  return String(ep.episodeNumber);
}

// An Episode tile is a vertical card in the season's 4-across grid: the 16:9
// still on top, a heading line (episode code + title + watched/resume/
// needs-review badges) under it, and the synopsis under that. Clicking the
// still OR the text block plays the Episode (show-from-here); the three-dots
// actions menu (Play next / Add to queue / Edit) floats over the still's
// top-right corner, hover-revealed.
function EpisodeRow({
  episode,
  onPlay,
  onPlayNext,
  onAddToQueue,
  onEdit,
}: {
  episode: EpisodeSummary;
  onPlay: () => void;
  onPlayNext: () => void;
  onAddToQueue: () => void;
  onEdit: () => void;
}) {
  const resuming = !episode.watched && episode.resumePositionMs > 0;
  return (
    <li
      className="episode-tile"
      data-testid="episode-row"
      data-episode-id={episode.id}
      data-episode-number={episode.episodeNumber}
    >
      <button
        className="episode-thumb"
        type="button"
        data-testid="episode-play"
        data-episode-id={episode.id}
        aria-label={`Play ${episode.title}`}
        onClick={onPlay}
      >
        {episode.stillUrl ? (
          <img
            className="episode-still"
            data-testid="episode-still"
            src={episode.stillUrl}
            alt=""
            loading="lazy"
            onError={(e) => {
              (e.currentTarget as HTMLImageElement).style.display = "none";
            }}
          />
        ) : (
          <span className="episode-still episode-still-placeholder" aria-hidden="true">
            ▶
          </span>
        )}
      </button>

      <button
        className="episode-text"
        type="button"
        data-testid="episode-open"
        aria-label={`Play ${episode.title}`}
        onClick={onPlay}
      >
        <span className="episode-heading">
          <span className="episode-code" data-testid="episode-code">
            {episodeCode(episode)}
          </span>
          <span className="episode-title" data-testid="episode-title">
            {episode.title}
          </span>
          {episode.watched && (
            <span className="badge badge-watched" data-testid="episode-watched">
              Watched
            </span>
          )}
          {resuming && (
            <span className="badge badge-resume" data-testid="episode-resume">
              Resume {formatTimecode(episode.resumePositionMs)}
            </span>
          )}
          {episode.needsReview && (
            <span className="badge badge-unwatched" data-testid="episode-needs-review">
              Needs review
            </span>
          )}
        </span>
        {episode.overview && (
          <span className="episode-overview" data-testid="episode-overview">
            {episode.overview}
          </span>
        )}
      </button>

      <EpisodeActionsMenu
        episodeTitle={episode.title}
        onPlayNext={onPlayNext}
        onAddToQueue={onAddToQueue}
        onEdit={onEdit}
      />
    </li>
  );
}
