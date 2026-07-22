import type { ApiClient } from "../../api/client";
import type {
  EpisodeSummary,
  TitleSummary,
  TrackSummary,
} from "../../api/types";
import { entriesFromTitles, entryFromTitle, type QueueEntry } from "./model";

/** Tag Episode entries with the Show they were walked from, so the player can key
 * the per-Show Playback preference synchronously (appletv-web-parity §1). */
function withShowId(entries: QueueEntry[], showId: string): QueueEntry[] {
  return entries.map((e) => ({ ...e, showId }));
}

/** The subset of a HEAD entry's pre-play Stream picks that is actually set — drops
 * null / undefined fields so a Play with no explicit pick leaves the entry at Auto
 * (an absent id, not `id: null`, keeps the QueueEntry `string | undefined` shape and
 * the negotiation omission). Shared by the single-Title and Show head-seeding paths. */
export function cleanStreams(streams: {
  audioStreamId?: string | null;
  videoStreamId?: string | null;
}): { audioStreamId?: string; videoStreamId?: string } {
  const out: { audioStreamId?: string; videoStreamId?: string } = {};
  if (streams.audioStreamId) out.audioStreamId = streams.audioStreamId;
  if (streams.videoStreamId) out.videoStreamId = streams.videoStreamId;
  return out;
}

// Build helpers turn a PLAY CONTEXT into ordered Queue entries by reading the
// existing ApiClient endpoints (no new server surface — ADR-0010/0012). They are
// the ONLY place in the Queue feature that touches I/O; the model stays pure and
// the result is fed to the store's `playNow`/`enqueue`. Issue 01 shipped the
// Playlist and single-Title builders; this slice (issue 02) adds the
// Album-from-a-Track and Show-from-an-Episode builders.

/** From a Playlist (story 5): GET /playlists/{id} → its ordered, already
 * access/Missing-filtered members → Queue entries (in the server's POSITION
 * order). This unifies the former URL-param playlist play-through into the Queue.
 * A Playlist member is a {@link TitleSummary} (plus an `itemId` the Queue
 * ignores), so it drops straight into an entry. */
export async function buildPlaylistQueue(
  client: ApiClient,
  playlistId: string,
  signal?: AbortSignal,
): Promise<QueueEntry[]> {
  const detail = await client.getPlaylist(playlistId, signal);
  return entriesFromTitles(detail.members);
}

/** From a single Title (story 6): a one-entry Queue. Also the FALLBACK a caller
 * uses when a richer build fails (story 39) — playing just the chosen Title
 * rather than stranding the player. */
export function buildSingleQueue(title: TitleSummary): QueueEntry[] {
  return [entryFromTitle(title)];
}

// A Track / Episode comes back from its ordered-list endpoint in a leaner summary
// shape than a browse {@link TitleSummary}, so map it up to the entry shape the
// Queue renders/plays (filling the fields the lean summary omits with the
// `TitleSummary` defaults — year 0, not ambiguous, no genres). The id/kind/title
// + watch state (resume/watched) the player reads carry straight across.

/** Map an Album {@link TrackSummary} to the {@link TitleSummary} a Queue entry
 * carries (a Track is a playable Title like any other). Exported so a Play
 * affordance on a Track row can build the single-Title fallback from the row it
 * already has. */
export function trackToSummary(t: TrackSummary): TitleSummary {
  return {
    id: t.id,
    kind: t.kind,
    title: t.title,
    year: 0,
    needsReview: t.needsReview,
    ambiguous: false,
    resumePositionMs: t.resumePositionMs,
    watched: t.watched,
    genres: [],
    enrichmentStatus: t.enrichmentStatus,
  };
}

/** Map a Season {@link EpisodeSummary} to the {@link TitleSummary} a Queue entry
 * carries (an Episode is a playable Title like any other). Exported so a Play
 * affordance on an Episode row can build the single-Title fallback. */
export function episodeToSummary(e: EpisodeSummary): TitleSummary {
  return {
    id: e.id,
    kind: e.kind,
    title: e.title,
    year: 0,
    needsReview: e.needsReview,
    ambiguous: false,
    resumePositionMs: e.resumePositionMs,
    watched: e.watched,
    genres: [],
    enrichmentStatus: e.enrichmentStatus,
  };
}

/** From a Track (stories 1–2, 8): `GET /albums/{id}/tracks` → the Album's Tracks
 * in disc/track order, SLICED from the chosen Track forward (Track 3 of 10 →
 * Tracks 3–10), mapped to entries. The chosen Track is the first entry (the
 * caller `playNow`s the result at index 0). If the chosen Track isn't in the
 * returned list (a defensive edge — it was filtered/Missing), the whole Album is
 * returned rather than an empty Queue. A fetch failure propagates so the caller
 * falls back to a single-Title Queue (story 39). */
export async function buildAlbumQueue(
  client: ApiClient,
  albumId: string,
  fromTrackId: string,
  signal?: AbortSignal,
): Promise<QueueEntry[]> {
  const { tracks } = await client.getAlbumTracks(albumId, signal);
  const i = tracks.findIndex((t) => t.id === fromTrackId);
  const fromHere = i < 0 ? tracks : tracks.slice(i);
  return entriesFromTitles(fromHere.map(trackToSummary));
}

/** The slice of the Queue store a build helper resolves entries INTO. A
 * cross-season Show walk plays the first batch immediately (`playNow`) and
 * appends the rest as it resolves (`enqueue`); the store satisfies this
 * structurally, and a fake records the calls in tests. */
export interface QueueSink {
  playNow: (entries: QueueEntry[], startIndex?: number) => void;
  enqueue: (entries: QueueEntry[]) => void;
}

/** The chosen Episode's Show/Season parent context (from its `TitleDetail.episode`
 * or the Show-detail surface), enough to walk the Show from here. */
export interface ShowQueueContext {
  showId: string;
  seasonId: string;
}

/** Options for {@link buildShowQueue}. `headResumeMs` overrides where the HEAD
 * Episode starts (the resume-point Continue/Restart split, ADR-0028): Continue
 * passes the stored resume, Restart/Play pass 0. Omitted → the head keeps its
 * fetched resume. */
export interface BuildShowQueueOptions {
  headResumeMs?: number;
  /** The pre-play Audio / Video Stream picks (appletv-web-parity §1, issue 04) to
   * seed onto the HEAD entry (the chosen Episode). Transient, per-play — see
   * {@link QueueEntry.audioStreamId}. Omitted / null fields leave the head at Auto. */
  headStreams?: { audioStreamId?: string | null; videoStreamId?: string | null };
}

/** From an Episode (stories 3–4, 8): walk the Show from the chosen Episode
 * forward — the current Season's Episodes from the chosen one, then the FOLLOWING
 * Seasons' Episodes in order — into `sink`. There is no single "all Episodes of a
 * Show" endpoint by design, so this composes `getSeasonEpisodes` (current Season)
 * + `getShowSeasons` + per-Season `getSeasonEpisodes` (the tail).
 *
 * CRITICAL (the ticket's now-playing-resolves-fast rule): the current Season's
 * from-here Episodes are `playNow`d as soon as that ONE fetch returns, so the
 * caller can navigate to the player WITHOUT waiting on the cross-season walk; the
 * returned promise resolves at that point. The following Seasons resolve lazily,
 * in order, appended via `enqueue` — exposed as the `tail` promise so tests can
 * await the whole walk while the affordance ignores it. The tail is best-effort:
 * a later-Season fetch failure stops the walk but never disturbs the Queue that
 * is already playing.
 *
 * A failure of the FIRST (current-Season) fetch propagates so the caller falls
 * back to a single-Title Queue (story 39). */
export async function buildShowQueue(
  client: ApiClient,
  ctx: ShowQueueContext,
  fromEpisodeId: string,
  sink: QueueSink,
  signal?: AbortSignal,
  opts?: BuildShowQueueOptions,
): Promise<{ tail: Promise<void> }> {
  const current = await client.getSeasonEpisodes(ctx.seasonId, signal);
  const i = current.episodes.findIndex((e) => e.id === fromEpisodeId);
  const fromHere = i < 0 ? current.episodes : current.episodes.slice(i);
  const summaries = fromHere.map(episodeToSummary);
  // Resume-point Continue vs. Restart differ ONLY in where the HEAD Episode starts
  // (ADR-0028): `headResumeMs` overrides the head's stored resume so the bar's
  // resume-seek machinery seeks there — Continue at the stored offset, Restart/Play
  // at 0. Clearing `watched` keeps the seek honest (a watched Title starts at 0).
  // The tail Episodes keep their own watch state. Undefined leaves the fetched
  // resume untouched (the plain from-here play).
  if (opts?.headResumeMs !== undefined && summaries.length > 0) {
    summaries[0] = {
      ...summaries[0],
      resumePositionMs: opts.headResumeMs,
      watched: false,
    };
  }
  // The now-playing batch — available after a single fetch, so playback starts
  // immediately (the caller navigates as soon as this resolves). The chosen Episode
  // is the head; seed it with the sheet's pre-play Audio / Video picks (issue 04) so
  // the first negotiation carries them (transient — never persisted).
  const entries = withShowId(entriesFromTitles(summaries), ctx.showId);
  if (entries.length > 0 && opts?.headStreams) {
    entries[0] = { ...entries[0], ...cleanStreams(opts.headStreams) };
  }
  sink.playNow(entries);
  return { tail: resolveShowTail(client, ctx, sink, signal) };
}

/** Append the Seasons AFTER the chosen one, in order, as each resolves. A
 * best-effort tail: any failure (a later Season went 404, a transient error)
 * simply stops the walk — the Queue keeps whatever already played. */
async function resolveShowTail(
  client: ApiClient,
  ctx: ShowQueueContext,
  sink: QueueSink,
  signal?: AbortSignal,
): Promise<void> {
  try {
    const { seasons } = await client.getShowSeasons(ctx.showId, signal);
    const pos = seasons.findIndex((s) => s.id === ctx.seasonId);
    const following = pos < 0 ? [] : seasons.slice(pos + 1);
    for (const season of following) {
      const { episodes } = await client.getSeasonEpisodes(season.id, signal);
      if (episodes.length > 0) {
        sink.enqueue(withShowId(entriesFromTitles(episodes.map(episodeToSummary)), ctx.showId));
      }
    }
  } catch {
    // Best-effort tail — never disturb the already-playing Queue.
  }
}

/** The WHOLE Show as ordered Queue entries — every Season's Episodes in the
 * server's Season order. Unlike {@link buildShowQueue} (which plays from a chosen
 * Episode and lazily walks the tail into a sink), this resolves the full series up
 * front and RETURNS the entries, so the Show detail's toolbar can append them
 * ("Add to queue") or insert them ("Play next") in one shot without disturbing
 * what's playing. Composes `getShowSeasons` + per-Season `getSeasonEpisodes`; a
 * fetch failure propagates so the caller can surface it. */
export async function buildFullShowEntries(
  client: ApiClient,
  showId: string,
  signal?: AbortSignal,
): Promise<QueueEntry[]> {
  const { seasons } = await client.getShowSeasons(showId, signal);
  const entries: QueueEntry[] = [];
  for (const season of seasons) {
    const { episodes } = await client.getSeasonEpisodes(season.id, signal);
    entries.push(...withShowId(entriesFromTitles(episodes.map(episodeToSummary)), showId));
  }
  return entries;
}
