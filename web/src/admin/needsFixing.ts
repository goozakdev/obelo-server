import type {
  EnrichmentAttentionTitle,
  MatchOverride,
  NeedsReviewItem,
  ShowProblems,
  UnmatchedFile,
} from "../api/types";
import { API_PREFIX } from "../api/client";
import { folderOf, matcherPath } from "./paths";

// The row model behind the Admin "Needs Fixing" queue.
//
// The server answers four separate questions — which files it couldn't identify
// (Unmatched), which parses it wasn't sure of (needs-review), which Titles it
// couldn't decorate (enrichment attention), and which corrections an Admin has
// already made (Match overrides) — in four different shapes with four different
// vocabularies. The old Attention screen rendered them as four lists, so the Admin
// had to know which vocabulary a given problem lived under before they could fix
// it, and each list carried only enough to name the problem, never enough to act
// on it: an enrichment row for an Episode printed a bare episode name, with no
// Show, no numbering, and no file.
//
// This module collapses all four into ONE uniform row that always answers the same
// four questions in the same places — what is it (kind + breadcrumb), which file,
// what's wrong (one sentence), and how do I fix it (an apply route). Rendering and
// the network live elsewhere; everything here is a pure mapping, so the wording and
// the fix routing are testable without a DOM.
//
// It then collapses a second time, along a different axis (ADR-0044,
// file-matcher/07). A Show's episode-level problems are ONE problem, not one per
// file: five files the provider counts in a re-numbered continuation series used to
// be five rows, each offering to re-pick the series — a fix inert by construction,
// because the series was never the part that was wrong. They become one Show row
// whose action opens the file matcher, the only screen that can express what is
// actually wrong. The row still answers all four questions: it names the Show, a
// representative file, and each problem WITH ITS COUNT, because "5 problems" would
// answer none of them.
//
// The counts have to be able to reach zero, or the queue is decoration. A Show
// stays queued while any of its files is unsettled — which is deliberately not the
// same as "has no metadata": an UNASSIGNED file is undecided, and settling it means
// placing or ignoring it, both one gesture in the matcher (CONTEXT.md
// "Unassigned"). The unassigned and unidentified halves of that count come from the
// server, which reads them off the very arrangement the matcher renders, so the
// number a row shows is exactly the number that screen can clear.
//
// And a THIRD time, on the Album axis (ADR-0050, album-resolves-its-tracks/09).
// Measured on the developer's library after a full recheck, 557 flagged tracks were
// 90 album problems: `Braveheart` alone was 18 identical rows, every one of them
// correctly saying "fix the album's match" and every one of them offering a
// RECORDING search that cannot perform it. Those rows are one Album row whose
// picker searches albums and whose apply cascades, so one pick resolves the whole
// tracklist — the exact gesture that cleared the *She* album by hand.
//
// The difference from the Show collapse is what qualifies. There, EVERY
// episode-level problem folded in, because re-picking the series was inert for all
// of them. Here only the ALBUM-SCOPED reasons fold: a `search-no-match`,
// `search-rejected` or `tag-id-unresolved` track genuinely is fixed by picking a
// recording on its own row, and folding those into an Album row would take a
// working action away. A blank or unknown reason folds nowhere either — a library
// not re-passed since the reason column shipped carries "" on every row, and it
// must keep behaving exactly as it did.
//
// The Album row does not HIDE the tracks it stands for: it counts them in its
// heading, and discloses them (collapsed) as their own rows, so a track the cascade
// could not place is still reachable without hunting.

/** What is wrong with a row — the axis the queue's filter chips segment on.
 *
 * A row may carry SEVERAL of these: a Show's episode-level problems collapse into
 * one row (ADR-0044, file-matcher/07), and one Show can easily have unassigned
 * files and unmatched metadata at once. {@link FixItem.problem} is the one it leads
 * with; {@link FixItem.problems} is every class it holds, and is what the chips
 * filter on — a Show hidden from the "No metadata" chip because unassigned files
 * happened to sort first would be a lie about what is in the library. */
export type FixProblem =
  | "unidentified" // an Unmatched file: no Title was derived at all
  | "unreadable" // ffprobe refused the file's bytes: named fine, cannot be read
  | "unassigned" // a File the Admin took off its Slot: undecided, not settled
  | "ambiguous" // two Files claim ONE Edition, so only the first of them plays
  | "uncertain-parse" // needs-review: a Title exists but the parse was a guess
  | "no-metadata" // enrichment could not settle on a provider record
  | "orphaned-correction"; // a Match override or Placement whose anchor is gone

/** How a row's fix is applied once the Admin picks a provider record. The two
 * routes are deliberately different operations with different blast radii
 * (ADR-0002/0014/0019), and the row's problem — never inference — decides which:
 *
 *  - `fix-match`: a folder-keyed identity correction (a Match override). Used where
 *    the item was FILED wrong, so the next scan must re-file it.
 *  - `enrichment-override`: a durable pin of which provider record decorates the
 *    Title. Used where the item is filed correctly and only its metadata is wrong;
 *    identity and every User's watch state stay untouched.
 *  - `album-enrichment-override`: the same pin applied to the ALBUM, cascading onto
 *    its Tracks. Used where the tracks are filed correctly and every one of them is
 *    stuck on the same missing fact — the album's record (ADR-0050). It is a
 *    separate route rather than a flag because it searches a different provider
 *    kind (albums, not recordings) and applies to a different entity.
 *  - `none`: an orphaned correction, resolved by discarding it, not by searching. */
export type FixRoute =
  | "fix-match"
  | "enrichment-override"
  | "album-enrichment-override"
  | "none";

/** One row of the queue: everything needed to identify the item, say what is wrong
 * with it, and apply a fix — regardless of which server list it came from. */
export interface FixItem {
  /** Stable, unique across every source (ids can repeat between lists). */
  key: string;
  /** The problem the row LEADS with — its badge-level answer to "what's wrong?".
   * For a collapsed Show row this is the most-stuck class it holds. */
  problem: FixProblem;
  /** Every problem class this row holds, for the filter chips. A single-problem
   * row carries exactly `[problem]`. */
  problems: FixProblem[];
  /** "movie" | "episode" | "track" | "show" | "album" | "file". */
  kind: string;
  /** The item's own name — an episode/track name, a movie title, a filename. */
  name: string;
  /** 0 when the item has no year. */
  year: number;
  /** Parent context, outermost first: `["The Wire", "Season 1"]`. Empty for a
   * Movie, a Show, or a file that was never identified. */
  breadcrumb: string[];
  /** The on-disk file (or, for an orphaned correction, the folder it points at).
   * "" only when every File of the item is Missing. */
  path: string;
  /** The Files that claim ONE Edition between them, in play order — the evidence
   * behind an `ambiguous` row, and the only thing that makes it actionable. The
   * first is the one that actually plays; the rest are not being played at all.
   * Empty on every other row. */
  collidingPaths: string[];
  /** One sentence stating what is actually wrong, in the Admin's terms. */
  problemText: string;
  /** How to apply a picked record for this row. */
  route: FixRoute;
  /** Pre-filled provider query — the thing the Admin would have typed. */
  searchSeed: string;
  /** Pre-filled, EDITABLE artist narrowing for a music row's search; `undefined`
   * on a row with no artist axis (every video row), which renders no box and sends
   * no param. It is editable rather than silent because the tag it comes from can
   * itself be the thing that is wrong — a silently-narrowed search would then
   * return nothing with no way to widen it. */
  artistScope?: string;
  /** The album counterpart of {@link artistScope}, narrowing a recording search to
   * the release the track sits on. Same rules: editable, blank widens, absent on a
   * row with no release axis. */
  albumScope?: string;
  /** In-app route to the item's own page, or "" when it has none (a file that
   * never became a Title). */
  detailPath: string;
  /** In-app route to the file matcher, or "" when this row is not fixed there.
   * This is the collapsed Show row's PRIMARY action: five broken episodes are one
   * arrangement problem, and the matcher is the only screen that can express the
   * fix (ADR-0044). The old per-episode rows offered to re-pick the series, which
   * was never the part that was wrong — a button inert by construction. */
  sortPath: string;
  /** The Title this row is about, or "" for an Unmatched file / a Show. */
  titleId: string;
  /** The Show this row is about, or "" — dismissal posts to a different route. */
  showId: string;
  /** The Album this row is about, or "" — the entity an
   * `album-enrichment-override` searches and applies against. Only a collapsed
   * Album row carries one; a Track row's album is context (a breadcrumb and a
   * narrowing term), not the thing its fix acts on. */
  albumId: string;
  /** The folder a fix-match anchors to, or "" when a folder override can't fix it
   * (an Episode's numbering is a problem only Enrichment maps). */
  folderPath: string;
  /** The Match override this row is about (orphaned corrections only). */
  overrideId: string;
  /** True when "Looks right" applies — the flag can be dismissed as a false
   * positive. Only a needs-review row can be dismissed; the others describe a
   * real gap that dismissing would not fill. */
  canDismiss: boolean;
  /** True when dismissing means "every flagged Episode of this Show", not one
   * Title — the collapsed row stands for the whole set it counted, so its
   * dismissal has to settle the same set in one call. */
  dismissEpisodes: boolean;
  /** The artwork to show beside the row, or "" when the item has no entity to
   * fetch one for. A poster is the fastest possible confirmation that a flagged
   * item was filed correctly — far faster than reading a path. */
  artworkUrl: string;
  /** Cache-bust token for that artwork, so a poster that 404'd before enrichment
   * landed is retried rather than staying a placeholder. */
  artworkVersion: string;
  /** What Enrichment matched this item to, ready to print ("Arrival (2016)"). This
   * is the evidence a confirm is made against: a `no-year` item cannot be judged
   * from its own parsed name, but it can be judged from the record it resolved to.
   *
   * "" means "nothing worth printing" — which is NOT the same as "no match", since
   * a match that merely repeats the row's heading is suppressed. Read {@link
   * hasMatch} for whether a record exists; the two are deliberately separate,
   * because collapsing them makes a correctly-matched item claim it is unmatched. */
  matchedAs: string;
  /** Whether Enrichment settled on a record at all. False is what justifies telling
   * the Admin there is nothing to check the filing against. */
  hasMatch: boolean;
  /** The rows this one COLLAPSED, each still carrying its own fix — disclosed under
   * the row, collapsed by default. Absent on a row that stands only for itself.
   *
   * A collapsed row must never be a dead end: the Album cascade places most of an
   * album's tracks and declines the rest (ADR-0050), and a declined track has to
   * stay reachable without the Admin hunting for it. The Show collapse needs no
   * such list — every row it folded was inert, so there was nothing under it worth
   * reaching. */
  children?: FixItem[];
}

/** Human labels for the kind badge. A never-identified file is its own "kind" here
 * because it is precisely NOT any of the catalog kinds yet. */
const KIND_LABELS: Record<string, string> = {
  movie: "Movie",
  episode: "Episode",
  track: "Track",
  show: "Show",
  album: "Album",
  file: "File",
};

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind;
}

/** Season 0 is Specials, not "no season" — the server sends 0 for both because the
 * value and the default coincide, and Specials is the reading that matters. */
function seasonLabel(seasonNumber: number): string {
  return seasonNumber === 0 ? "Specials" : `Season ${seasonNumber}`;
}

/** `S01E03` for a conventionally-numbered Episode; the raw label (a date or an
 * absolute number) when that is all the scanner could read; "" for anything else.
 * This is the code the Admin matches against their own filenames. */
export function episodeCode(
  seasonNumber: number,
  episodeNumber: number,
  episodeLabel: string,
): string {
  if (episodeNumber > 0) {
    const s = String(seasonNumber).padStart(2, "0");
    const e = String(episodeNumber).padStart(2, "0");
    return `S${s}E${e}`;
  }
  return episodeLabel;
}

/** The breadcrumb for a leaf, from whichever parent context its kind carries. An
 * Episode's own name ("Pilot") is not identifying on its own — two Shows can share
 * it — which is exactly what the old screen got wrong. */
function breadcrumbFor(item: {
  kind: string;
  showTitle: string;
  seasonNumber: number;
  artistName: string;
  albumTitle: string;
}): string[] {
  if (item.kind === "episode" && item.showTitle !== "") {
    return [item.showTitle, seasonLabel(item.seasonNumber)];
  }
  if (item.kind === "track") {
    return [item.artistName, item.albumTitle].filter((s) => s !== "");
  }
  return [];
}

/** The query to seed the provider search with — the thing the Admin would have
 * typed, which is whatever the searched PROVIDER ENDPOINT takes as its subject.
 *
 * For an Episode that is the SHOW, not the episode: TMDB resolves an episode
 * through its show (`/search/tv`), so the show title is both what the search wants
 * and what the Admin would type.
 *
 * A Track is the opposite case, and reading the Episode rule onto it was the bug
 * (ADR-0050): a Track row searches `/ws/2/recording`, whose subject is the
 * RECORDING. Seeding it with the album made every row for a track on *She* search
 * for every recording ever called "She". The album is not a subject there — it is a
 * narrowing term, and it is carried as one by {@link musicScopeFor}. */
function searchSeedFor(item: { kind: string; title: string; showTitle: string }): string {
  if (item.kind === "episode") return item.showTitle || item.title;
  return item.title;
}

/** The pre-filled narrowing terms for a music row's picker: the artist and album
 * the scanner already read off the file, AND-ed into the recording search so a
 * common title ("Intro", "She") is answerable.
 *
 * `undefined` — not "" — on a row with no such axis, because the picker renders a
 * box for a term that is defined and blank (a music item whose tags name no artist
 * can still be narrowed by hand) and no box at all for one that is undefined (a
 * Movie/Episode/Show has no artist or release axis to narrow on). */
function musicScopeFor(item: { kind: string; artistName: string; albumTitle: string }): {
  artistScope?: string;
  albumScope?: string;
} {
  if (item.kind !== "track") return {};
  return { artistScope: item.artistName, albumScope: item.albumTitle };
}

/** The name to print for a leaf, prefixed with the number the Admin actually
 * matches against their own filenames — `S01E03 Pilot`, `4. Exit Music`. The bare
 * name alone is not identifying: two Shows can both have a "Pilot", and two albums
 * can both have an "Intro". */
function displayNameFor(item: {
  kind: string;
  title: string;
  seasonNumber: number;
  episodeNumber: number;
  episodeLabel: string;
  trackNumber: number;
}): string {
  if (item.kind === "episode") {
    const code = episodeCode(item.seasonNumber, item.episodeNumber, item.episodeLabel);
    return code === "" ? item.title : `${code} ${item.title}`;
  }
  if (item.kind === "track" && item.trackNumber > 0) {
    return `${item.trackNumber}. ${item.title}`;
  }
  return item.title;
}

/** The year Enrichment's matched record carries, from its RFC3339-ish release
 * date ("2016-11-11" → 2016). 0 when there is no date or it is unreadable. This is
 * where a `no-year` item's missing year comes from — the parse has none by
 * definition, so the matched record is the only source. */
export function releaseYear(releaseDate: string): number {
  const year = Number(releaseDate.slice(0, 4));
  return Number.isInteger(year) && year > 1800 ? year : 0;
}

/** What Enrichment matched an item to, formatted for the row — the display title
 * it resolved to plus that record's year. "" when nothing is matched yet, which is
 * itself worth saying: there is then nothing to confirm the filing against.
 *
 * Also "" when it would only repeat what the row already shows. A Show has no
 * enriched year (entity_enrichment carries no release date), so a correctly-matched
 * Show renders "Matched to The Wire" directly under a heading that says "The Wire"
 * — a line that costs a reading and settles nothing. The line earns its place only
 * by adding a different title or a year the parse lacked; the poster carries the
 * confirmation in every other case. `displayed` is the row's own name+year. */
function matchedAsFor(
  item: {
    title: string;
    year: number;
    enrichedTitle: string;
    releaseDate: string;
    enrichmentStatus: string;
  },
  displayed: string,
): string {
  if (item.enrichmentStatus !== "matched") return "";
  const title = item.enrichedTitle || item.title;
  const year = item.year > 0 ? item.year : releaseYear(item.releaseDate);
  const matched = year > 0 ? `${title} (${year})` : title;
  return matched === displayed ? "" : matched;
}

/** The row's own heading text (name plus year when it has one), which is what the
 * matched line is judged redundant against. */
function displayedNameFor(name: string, year: number): string {
  return year > 0 ? `${name} (${year})` : name;
}

/** Where to fetch the artwork that represents a row. An Episode has no poster of
 * its own — its Show's is the recognizable image — and a Track's cover belongs to
 * its Album, so both address their parent. An item with no entity yet (an
 * Unmatched file, an orphaned correction) has no artwork at all.
 *
 * The URL is built rather than advertised because that is how the browse grid
 * already works: whether a poster exists is only known once the browser tries it,
 * and the Poster component swaps in a placeholder on the 404. */
function artworkUrlFor(item: {
  id: string;
  kind: string;
  showId: string;
  albumId: string;
}): string {
  switch (item.kind) {
    case "movie":
      return `${API_PREFIX}/titles/${encodeURIComponent(item.id)}/artwork/poster`;
    case "show":
      return `${API_PREFIX}/shows/${encodeURIComponent(item.id)}/artwork/poster`;
    case "episode":
      return item.showId === ""
        ? ""
        : `${API_PREFIX}/shows/${encodeURIComponent(item.showId)}/artwork/poster`;
    case "track":
      return item.albumId === ""
        ? ""
        : `${API_PREFIX}/albums/${encodeURIComponent(item.albumId)}/artwork/cover`;
    default:
      return "";
  }
}

/** Which correction a needs-review row can actually apply. A folder anchor means
 * an identity fix-match; anything else has no search to offer.
 *
 * An Episode has no folder, and — since file-matcher/07 — no provider search
 * either. Re-picking the SERIES was never the fix for a file the provider numbers
 * differently: the series was right, the arrangement was wrong. That row's action
 * is the matcher (`sortPath`), and offering a search beside it would be exactly the
 * button-that-cannot-work this queue exists to end. */
function episodeFixRoute(item: { kind: string; folderPath: string }): FixRoute {
  if (item.folderPath !== "") return "fix-match";
  return "none";
}

/** Why the scanner flagged an uncertain parse, in one sentence. Each maps to the
 * exact rule the scanner applied (see the server's needsReviewReason), so the
 * sentence says what actually happened rather than "needs review". */
function uncertainParseText(reason: string, kind: string): string {
  switch (reason) {
    case "episode-numbering":
      return "Numbered by date or absolute number instead of SxxExx, so its place in the season order is a guess. Pick the episode it really is to give it the right details.";
    case "untagged":
      return "No usable tags on the file — the artist, album and track were read from the file path.";
    default:
      return kind === "show"
        ? "The show folder carried no year, so this may have matched the wrong show."
        : "Filed from a name with no year, so this may have matched the wrong release.";
  }
}

/** The generic "nothing matched" sentence — what every no-metadata row said before
 * the server could tell the four cases apart, and what a row still says when it
 * carries no reason or one this build does not know (ADR-0050). Naming it once
 * makes the fallback a deliberate destination rather than a `default:` nobody
 * looked at. */
const NO_METADATA_GENERIC =
  "No metadata match — the provider had no record for this name, so there is no artwork or description.";

/** Why a track's metadata lookup settled without a record, in one sentence that
 * names the ACTION — the point of the whole reason column (ADR-0050).
 *
 * All four Music cases used to render {@link NO_METADATA_GENERIC}, which told the
 * Admin nothing the row's existence had not already told them, and pointed all four
 * at the same per-track recording search. Only one of the four is actually fixed
 * that way. In the developer's library 365 of 730 unmatched tracks are
 * `album-unmatched` — half the queue being sent to a search box that cannot fix
 * them, when the one gesture that clears all 365 is matching their albums.
 *
 * `album` is the album's title when the row carries one, so the biggest bucket can
 * NAME the thing to go and fix instead of gesturing at it; the sentence still reads
 * correctly without it.
 *
 * An unknown or blank reason falls back to the generic sentence, deliberately: a
 * library that has not been re-passed since the column shipped carries "" on every
 * row, and a value a newer server invents must degrade to what this screen said
 * before rather than to a blank. */
function noMetadataText(reason: string, album: string): string {
  const theAlbum = album === "" ? "This track’s album" : `The album ${album}`;
  switch (reason) {
    case "album-unmatched":
      // The action is on the ALBUM, and it is one gesture for every track under it —
      // which is exactly what a per-track search box cannot say.
      return `${theAlbum} has no metadata match, so it could name none of its tracks. Fix the album’s match and its tracks resolve from the album’s own track list.`;
    case "not-in-tracklist":
      // The album matched something; the evidence says it matched the wrong EDITION.
      return `${theAlbum} matched, but this track is not on that release’s track list — so the album is probably matched to the wrong edition. Fix the album’s match to the release these files were ripped from.`;
    case "tag-id-unresolved":
      // The name was never the problem, so searching by it is the wrong offer.
      return "The exact recording id on this track resolved to nothing — the id is wrong, not the name. Retag the file with a working id, or pick the recording by hand.";
    case "search-rejected":
      // A near miss: something came back and was refused rather than stored blind, so
      // the picker's own list is likely to hold the right answer.
      return "The search found recordings, but none of their titles matched this track closely enough to accept automatically. Pick the right recording by hand — the near miss is probably in the list.";
    case "search-no-match":
      // The honest empty answer: no id anywhere, and the search really did find
      // nothing.
      return "No id named this recording, so it was searched for by name and artist — and nothing came back under that title. Pick the recording by hand.";
    default:
      return NO_METADATA_GENERIC;
  }
}

/** The settled-failure reasons whose fix is on the ALBUM rather than on the track,
 * and which therefore collapse into one Album row (ADR-0050, issue 09).
 *
 * The membership test is the whole design. `album-unmatched` and `not-in-tracklist`
 * both name a fact the ALBUM is missing — a record, or the right release — and one
 * album pick supplies it for every track underneath. The other three
 * (`tag-id-unresolved`, `search-rejected`, `search-no-match`) name a fact about ONE
 * recording, and the per-track picker already fixes them: folding those in would
 * remove a working action, which is the opposite of what the Show collapse did.
 *
 * "" is deliberately not a member. A library not re-passed since the reason column
 * shipped carries it on every row, and such a row must keep behaving exactly as it
 * does today rather than being swept into a collapse on a diagnosis nobody made. */
const ALBUM_SCOPED_REASONS = new Set(["album-unmatched", "not-in-tracklist"]);

/** Whether an enrichment-attention row is one an Album row can stand for.
 *
 * `albumId` is required, not incidental: the row's whole action is a search-and-pin
 * against that album entity, so a track whose album the server could not name has
 * nothing to collapse INTO and keeps its own row. A `failed` status is excluded for
 * the same reason it is answered first in the sentence below — a provider refusing
 * requests is not a diagnosis about the album (ADR-0048/0050), and hiding it behind
 * an album pick would make the row promise a fix it cannot perform. */
function collapsesIntoAlbum(t: {
  kind: string;
  albumId: string;
  enrichmentStatus: string;
  enrichmentReason: string;
}): boolean {
  return (
    t.kind === "track" &&
    t.albumId !== "" &&
    t.enrichmentStatus !== "failed" &&
    ALBUM_SCOPED_REASONS.has(t.enrichmentReason)
  );
}

/** What a file collision means, in the Admin's terms, and what to do about it.
 *
 * The naming convention refuses to guess between two files that parse to the same
 * Edition and are not parts ("flagged ambiguous in the web app, never silently
 * guessed"), so the consequence is concrete and worth stating plainly: only the
 * first of them plays. Saying "ambiguous" and stopping would leave the Admin to
 * discover the missing file by watching something.
 *
 * The fix differs by kind because the app can only reach one of them. An Episode's
 * files are arranged in the matcher — the screen that can say which file is which
 * Slot, or that two of them are parts of one episode. A Movie has no such screen:
 * the collision lives in the filenames, and the convention's own escape hatches
 * (an `{edition-…}` tag, or a `- part1`/`- part2` suffix when they really are one
 * work) are what settle it. */
function ambiguousText(kind: string, count: number): string {
  const noun = kind === "episode" ? "episode" : kind === "track" ? "track" : "title";
  const files = count > 2 ? `${count} files claim` : "Two files claim";
  const lead = `${files} to be this ${noun}, and nothing in their names says which — so only the first one plays.`;
  return kind === "episode"
    ? `${lead} Sort this show’s episodes to say which file is which.`
    : `${lead} Rename one of them with an edition tag ({edition-Director’s Cut}), or with a part suffix (- part1 / - part2) if they are two halves of one work, then rescan.`;
}

/** Build the queue rows for one Library from the four server lists.
 *
 * Ordering is by how stuck the Admin is, not by list: a file that produced nothing
 * is the most broken thing in a library, a correction pointing at a vanished folder
 * is the next (it is actively wrong), then items that exist but are filed on a
 * guess, then items that are filed right and only look wrong. Within a group,
 * server order (sort title / path) is preserved.
 *
 * Non-orphaned overrides are NOT rows: they are corrections the Admin already made,
 * and a queue whose entries are already-finished work stops reading as a queue. The
 * screen shows them separately. */
export function buildFixItems(input: {
  unmatched: UnmatchedFile[];
  needsReview: NeedsReviewItem[];
  enrichment: EnrichmentAttentionTitle[];
  overrides: MatchOverride[];
  /** The per-Show unsettled counts the client cannot compute (see
   * {@link ShowProblems}). Optional so a failed fetch degrades to "collapse what I
   * can see" rather than blanking the queue — the collapse itself never depends on
   * it, because every flagged Episode already names its Show. */
  showProblems?: ShowProblems[];
}): FixItem[] {
  const showProblems = input.showProblems ?? [];

  // --- the collapse ----------------------------------------------------------
  //
  // A Show's episode-level problems become ONE row. The Batman case — five files
  // the provider counts in a re-numbered continuation series — used to be five
  // rows, each with a "Use this" that offered to fix the SERIES, which was never
  // the part that was wrong. One problem, five rows, no working fix.
  //
  // The grouping is driven by the Episodes themselves, not by the server's
  // show-problems list, so it holds even when that fetch fails: every flagged
  // Episode already names its Show.
  const shows = new Map<string, ShowRowCounts>();
  const showFor = (showId: string, title: string) => {
    let row = shows.get(showId);
    if (row === undefined) {
      row = {
        showId,
        title,
        year: 0,
        path: "",
        unidentified: 0,
        unassigned: 0,
        ambiguous: 0,
        collidingPaths: [],
        uncertainParse: 0,
        noMetadata: 0,
      };
      shows.set(showId, row);
    }
    if (row.title === "") row.title = title;
    return row;
  };

  // An identity-attention item carries EITHER flag or both, and the two are
  // counted separately: an uncertain parse is a guess the Admin can confirm, a
  // collision is two files one of which is not being played. Folding them into one
  // number would produce a row whose "Looks right" cannot clear its own count.
  for (const t of input.needsReview) {
    if (t.kind !== "episode" || t.showId === "") continue;
    const row = showFor(t.showId, t.showTitle);
    if (t.needsReview) row.uncertainParse++;
    if (t.ambiguous) {
      row.ambiguous++;
      row.collidingPaths.push(...t.collidingPaths);
    }
    if (row.path === "") row.path = t.path;
  }
  // An Episode blocked by its own numbering is ONE problem with one root cause, so
  // it is counted once — exactly as the flat rows deduplicated it before. Only the
  // uncertain-parse flag does that: a collision says nothing about whether the
  // provider had a record, so an ambiguous Episode with no metadata has two
  // genuinely different problems.
  const flaggedTitles = new Set(
    input.needsReview.filter((t) => t.needsReview).map((t) => t.id),
  );
  for (const t of input.enrichment) {
    if (t.kind !== "episode" || t.showId === "") continue;
    if (flaggedTitles.has(t.id)) continue;
    const row = showFor(t.showId, t.showTitle);
    row.noMetadata++;
    if (row.path === "") row.path = t.path;
  }

  // The server's half: files that are in NO list the client already holds. An
  // explicitly unassigned File produces neither a Title nor an Unmatched row, and
  // an Unmatched row is a flat path until the Show's folders say whose it is.
  const orphanRows: FixItem[] = [];
  const foldedPaths = new Set<string>();
  // Which Show an UNREADABLE file belongs to. It is deliberately not folded into that Show's
  // row (the matcher cannot settle it, so counting it there would hide an unclearable file
  // behind a screen that says the Show is fine — ADR-0047), but the row still has to say where
  // to go: the matcher is where Ignore lives, and ignoring is the one gesture that settles a
  // file the Admin does not intend to replace.
  const unreadableShow = new Map<string, { showId: string; title: string }>();
  for (const p of showProblems) {
    for (const path of p.unreadablePaths) {
      unreadableShow.set(path, { showId: p.showId, title: p.title });
    }
    for (const path of p.unmatchedPaths) foldedPaths.add(path);
    if (p.unassigned > 0 || p.unidentified > 0) {
      const row = showFor(p.showId, p.title);
      row.unassigned += p.unassigned;
      row.unidentified += p.unidentified;
      row.year = p.year;
      // The server's representative path is one of the COUNTED files, so it beats
      // a flagged Episode's path at answering "which file?".
      if (p.path !== "") row.path = p.path;
    }
    if (p.orphaned > 0) orphanRows.push(orphanedPlacementRow(p));
  }
  for (const p of showProblems) {
    const row = shows.get(p.showId);
    if (row !== undefined && row.year === 0) row.year = p.year;
  }

  const showRows: FixItem[] = [];
  for (const row of shows.values()) showRows.push(showFixItem(row));

  // --- the flat rows ---------------------------------------------------------

  // An UNREADABLE file is a row of its own kind, and the difference is not cosmetic. Every
  // other row here ends in a provider search: the Admin names the work and the next scan files
  // it. This one cannot. Its name was never the problem — the file matcher even shows it
  // sitting correctly on its Slot, because the filename numbered it — and ffprobe still refuses
  // the bytes, so no Episode was built and none will be until the FILE is replaced. Offered the
  // search, an Admin presses "Use this", writes an identity correction for an identity that was
  // already right, and watches the row come back on the next scan (the server withholds
  // `folderPath` for these, so the offer cannot even be assembled). ADR-0047.
  const unreadableFileRow = (f: UnmatchedFile): FixItem => {
    const show = unreadableShow.get(f.path);
    return {
      key: `unmatched:${f.id}`,
      problem: "unreadable" as const,
      problems: ["unreadable"] as FixProblem[],
      kind: "file",
      name: fileStem(f.path),
      year: 0,
      breadcrumb: show ? [show.title] : [],
      path: f.path,
      collidingPaths: [],
      problemText: f.reason
        ? `This file could not be read — ${f.reason}. Replace it on disk and rescan, or ignore it in the file matcher.`
        : "This file could not be read — it may be corrupt or incomplete. Replace it on disk and rescan, or ignore it in the file matcher.",
      // Nothing to search for: naming the work is not what is wrong.
      route: "none" as const,
      searchSeed: "",
      detailPath: "",
      // The one action that settles it without touching the disk, offered only where the
      // server could say which Show the file is under.
      sortPath: show ? matcherPath(show.showId) : "",
      titleId: "",
      showId: show?.showId ?? "",
      albumId: "",
      folderPath: "",
      overrideId: "",
      canDismiss: false,
      dismissEpisodes: false,
      artworkUrl: show
        ? `${API_PREFIX}/shows/${encodeURIComponent(show.showId)}/artwork/poster`
        : "",
      artworkVersion: "",
      matchedAs: "",
      hasMatch: false,
    };
  };

  // A file nothing could NAME: the original meaning of the Unmatched list, and the one the
  // provider search is for.
  const unidentifiedFileRow = (f: UnmatchedFile): FixItem => ({
    key: `unmatched:${f.id}`,
    problem: "unidentified" as const,
    problems: ["unidentified"] as FixProblem[],
    kind: "file",
    name: fileStem(f.path),
    year: 0,
    breadcrumb: [],
    path: f.path,
    collidingPaths: [],
    problemText: f.reason
      ? `Not recognized as a title — ${f.reason}.`
      : "Not recognized as a title — no name or year could be read from this file.",
    route: "fix-match" as const,
    searchSeed: searchableStem(f.path),
    detailPath: "",
    sortPath: "",
    titleId: "",
    showId: "",
    albumId: "",
    // The server derives the anchor from the Library's kind; folderOf is the
    // fallback for a server that predates that field, and is only ever right for a
    // Movie library (a TV file's own directory is a Season folder, not the Show).
    folderPath: f.folderPath || folderOf(f.path),
    overrideId: "",
    canDismiss: false,
    dismissEpisodes: false,
    // No Title was derived, so there is no entity to fetch artwork for and nothing
    // Enrichment could have matched.
    artworkUrl: "",
    artworkVersion: "",
    matchedAs: "",
    hasMatch: false,
  });

  const unidentified: FixItem[] = input.unmatched
    // A file counted inside a Show row must not also be a row of its own: that is
    // the five-rows-one-problem shape returning by the back door.
    .filter((f) => !foldedPaths.has(f.path))
    // The two kinds share a list and must not share a fix (ADR-0047).
    .map((f) => (f.kind === "unreadable" ? unreadableFileRow(f) : unidentifiedFileRow(f)));

  const orphaned: FixItem[] = input.overrides
    .filter((o) => o.orphaned)
    .map((o) => ({
      key: `override:${o.id}`,
      problem: "orphaned-correction" as const,
      problems: ["orphaned-correction"] as FixProblem[],
      kind: "file",
      name: o.title,
      year: o.year,
      breadcrumb: [],
      path: o.folderPath,
      collidingPaths: [],
      problemText:
        "This correction points at a folder that no longer exists, so it can never apply again. Discard it, or restore the folder.",
      route: "none" as const,
      searchSeed: o.title,
      detailPath: "",
      sortPath: "",
      titleId: "",
      showId: "",
      albumId: "",
      folderPath: o.folderPath,
      overrideId: o.id,
      canDismiss: false,
      dismissEpisodes: false,
      artworkUrl: "",
      artworkVersion: "",
      // The correction itself records the identity it asserts, which is exactly
      // what the Admin is deciding whether to keep.
      matchedAs: o.year > 0 ? `${o.title} (${o.year})` : o.title,
      // The correction IS the assertion, so there is always something to show.
      hasMatch: true,
    }));

  // The identity-attention rows: an uncertain parse, a file collision, or both.
  // BOTH is one row rather than two — the item is one thing, and `problems` is
  // exactly the field that lets a row hold more than one class. Two rows for one
  // Movie would be the five-rows-one-problem shape at a smaller scale.
  const uncertain: FixItem[] = input.needsReview
    // Collapsed into their Show's row above.
    .filter((t) => !(t.kind === "episode" && shows.has(t.showId)))
    .map((t) => {
      const problems: FixProblem[] = [];
      if (t.ambiguous) problems.push("ambiguous");
      if (t.needsReview) problems.push("uncertain-parse");
      const sentences = [
        t.ambiguous ? ambiguousText(t.kind, t.collidingPaths.length) : "",
        t.needsReview ? uncertainParseText(t.reason, t.kind) : "",
      ].filter((line) => line !== "");
      return {
        key: `review:${t.kind}:${t.id}`,
        problem: problems[0] ?? "uncertain-parse",
        problems,
        kind: t.kind,
        name: displayNameFor(t),
        year: t.year,
        breadcrumb: breadcrumbFor(t),
        path: t.path,
        collidingPaths: t.collidingPaths,
        problemText: sentences.join(" "),
        // A folder override is the fix only where one can anchor. An Episode has none
        // — and an Episode's real fix is an arrangement, not a name, so it is offered
        // the matcher rather than a provider search that cannot reach the problem.
        //
        // A row flagged ONLY for a collision gets no search either, for the same
        // reason: naming the work again cannot settle which of two files is it. The
        // fix is on disk (or, for an Episode, in the matcher), and an inert "Use
        // this" is exactly what this queue replaced.
        route: t.needsReview ? episodeFixRoute(t) : "none",
        searchSeed: searchSeedFor(t),
        ...musicScopeFor(t),
        detailPath: t.kind === "show" ? `/shows/${t.id}` : `/titles/${t.id}`,
        sortPath: t.kind === "episode" && t.showId !== "" ? matcherPath(t.showId) : "",
        titleId: t.kind === "show" ? "" : t.id,
        showId: t.kind === "show" ? t.id : "",
        albumId: "",
        folderPath: t.folderPath,
        overrideId: "",
        // "Looks right" settles an uncertain PARSE. It has no answer for a
        // collision — the two files are still both there, and one of them still
        // does not play — so a row flagged only for that is not dismissible.
        canDismiss: t.needsReview,
        dismissEpisodes: false,
        // This is the row that asks the Admin to CONFIRM a filing, so it is the row
        // that most needs the evidence: the poster and the record it matched to.
        artworkUrl: artworkUrlFor(t),
        artworkVersion: t.enrichmentStatus,
        matchedAs: matchedAsFor(t, displayedNameFor(displayNameFor(t), t.year)),
        hasMatch: t.enrichmentStatus === "matched",
      };
    });

  // One enrichment-attention item as its own row. It is a named function rather
  // than an inline literal because the Album collapse needs exactly this row twice:
  // once as a CHILD the Album row discloses, and once in the queue itself for every
  // track the Album row does not stand for.
  const enrichmentFixItem = (t: EnrichmentAttentionTitle): FixItem => ({
    key: `enrichment:${t.id}`,
    problem: "no-metadata" as const,
    problems: ["no-metadata"] as FixProblem[],
    kind: t.kind,
    name: displayNameFor(t),
    year: t.year,
    breadcrumb: breadcrumbFor(t),
    path: t.path,
    collidingPaths: [],
    problemText:
      t.enrichmentStatus === "failed"
        ? // The server retries a reachable-provider blip on its own and keeps it OFF
          // this list until the streak escalates (ADR-0048), so anything that gets
          // here is either a refusal the provider will keep making — a rejected API
          // key, a request it cannot answer — or a failure that has already
          // outlasted a day of retries.
          "The metadata lookup failed repeatedly — the provider is refusing the request (check the API key in Settings), or it has no episode at this season and number."
        : // 'failed' is answered above and carries no reason (a provider error is not a
          // diagnosis, ADR-0048/0050); everything else is a settled 'unmatched', where
          // the reason — when there is one — says which of four different things to go
          // and fix.
          noMetadataText(t.enrichmentReason, t.albumTitle),
    // An Episode's metadata problem is not fixed by naming a work — see
    // episodeFixRoute. Every other kind IS, so it keeps its search.
    route: (t.kind === "episode" ? "none" : "enrichment-override") as FixRoute,
    searchSeed: searchSeedFor(t),
    ...musicScopeFor(t),
    detailPath: `/titles/${t.id}`,
    sortPath: t.kind === "episode" && t.showId !== "" ? matcherPath(t.showId) : "",
    titleId: t.id,
    showId: "",
    albumId: "",
    folderPath: "",
    overrideId: "",
    canDismiss: false,
    dismissEpisodes: false,
    artworkUrl: artworkUrlFor(t),
    artworkVersion: t.enrichmentStatus,
    // Nothing matched — that IS this row's problem — so there is no record to
    // print; the problem sentence already says so.
    matchedAs: "",
    hasMatch: false,
  });

  // --- the album collapse ----------------------------------------------------
  //
  // The Braveheart case: 18 tracks, 18 rows, one missing fact. Every one of those
  // rows said "fix the album's match" and then offered a RECORDING search, which is
  // not the gesture the sentence names. They are one row whose picker searches
  // albums and whose apply cascades onto the tracklist.
  //
  // Only the album-scoped reasons qualify (see collapsesIntoAlbum); everything else
  // keeps the per-track row it can actually be fixed on.
  const albums = new Map<string, AlbumRowCounts>();
  for (const t of input.enrichment) {
    if (!collapsesIntoAlbum(t)) continue;
    let row = albums.get(t.albumId);
    if (row === undefined) {
      row = {
        albumId: t.albumId,
        title: "",
        artist: "",
        path: "",
        unmatched: 0,
        notInTracklist: 0,
        children: [],
      };
      albums.set(t.albumId, row);
    }
    if (row.title === "") row.title = t.albumTitle;
    if (row.artist === "") row.artist = t.artistName;
    if (row.path === "") row.path = t.path;
    if (t.enrichmentReason === "album-unmatched") row.unmatched++;
    else row.notInTracklist++;
    // The row it replaces, kept whole: the cascade will decline some of these, and a
    // declined track has to stay one disclosure away rather than one recheck away.
    row.children.push(enrichmentFixItem(t));
  }

  const emittedAlbums = new Set<string>();

  const noMetadata: FixItem[] = input.enrichment.flatMap((t) => {
    // Collapsed into their Show's row above.
    if (t.kind === "episode" && shows.has(t.showId)) return [];
    // One file, one row: an Episode flagged for its numbering already appears as a
    // needs-review row carrying the same actions.
    if (t.kind === "episode" && flaggedTitles.has(t.id)) return [];
    const album = collapsesIntoAlbum(t) ? albums.get(t.albumId) : undefined;
    if (album !== undefined) {
      // Emitted at the position of the FIRST track it stands for, so it keeps its
      // place in the queue's how-stuck-is-the-Admin ordering rather than being
      // floated to one end as a special case.
      if (emittedAlbums.has(album.albumId)) return [];
      emittedAlbums.add(album.albumId);
      return [albumFixItem(album)];
    }
    return [enrichmentFixItem(t)];
  });

  return [...unidentified, ...orphaned, ...orphanRows, ...showRows, ...uncertain, ...noMetadata];
}

/** The tallied problems behind one collapsed Show row, before it becomes a row. */
interface ShowRowCounts {
  showId: string;
  title: string;
  year: number;
  path: string;
  unidentified: number;
  unassigned: number;
  ambiguous: number;
  /** Every colliding File under this Show, across all of its ambiguous Episodes —
   * the row has to name them, not just count them. */
  collidingPaths: string[];
  uncertainParse: number;
  noMetadata: number;
}

/** The problem classes a Show row holds, most-stuck first. The order is the same
 * "how stuck is the Admin" order the queue itself sorts by, so the class a row
 * LEADS with is the worst thing wrong with it. */
function showRowProblems(c: ShowRowCounts): FixProblem[] {
  const out: FixProblem[] = [];
  if (c.unidentified > 0) out.push("unidentified");
  if (c.unassigned > 0) out.push("unassigned");
  if (c.ambiguous > 0) out.push("ambiguous");
  if (c.uncertainParse > 0) out.push("uncertain-parse");
  if (c.noMetadata > 0) out.push("no-metadata");
  return out;
}

/** One clause per problem class, counted and in the Admin's terms. A row that said
 * only "5 problems" would answer none of the queue's four questions: the count is
 * useless without the noun that says what was counted. */
function showRowClauses(c: ShowRowCounts): string[] {
  const out: string[] = [];
  if (c.unidentified > 0) {
    out.push(
      c.unidentified === 1
        ? "1 file isn’t recognized as an episode"
        : `${c.unidentified} files aren’t recognized as episodes`,
    );
  }
  if (c.unassigned > 0) {
    out.push(
      c.unassigned === 1
        ? "1 file isn’t assigned to an episode"
        : `${c.unassigned} files aren’t assigned to an episode`,
    );
  }
  if (c.ambiguous > 0) {
    out.push(
      c.ambiguous === 1
        ? "1 episode has two files claiming to be it, so only one of them plays"
        : `${c.ambiguous} episodes have two files claiming to be them, so only one of each plays`,
    );
  }
  if (c.uncertainParse > 0) {
    out.push(
      c.uncertainParse === 1
        ? "1 episode was filed on a guess about its numbering"
        : `${c.uncertainParse} episodes were filed on a guess about their numbering`,
    );
  }
  if (c.noMetadata > 0) {
    out.push(
      c.noMetadata === 1
        ? "1 episode has no metadata match"
        : `${c.noMetadata} episodes have no metadata match`,
    );
  }
  return out;
}

/** Join clauses into one readable sentence. */
export function joinClauses(clauses: string[]): string {
  if (clauses.length === 0) return "";
  if (clauses.length === 1) return clauses[0];
  return `${clauses.slice(0, -1).join(", ")}, and ${clauses[clauses.length - 1]}`;
}

/** One Show's episode-level problems as a single queue row.
 *
 * It answers the queue's four questions exactly as every other row does — kind
 * badge and Show title (what), a representative file (which), a sentence naming
 * each problem and its count (what's wrong), and an action that can actually
 * perform the fix (how). The action is the matcher, because arranging files onto
 * Slots is the only operation that can express what is wrong here. */
function showFixItem(c: ShowRowCounts): FixItem {
  const problems = showRowProblems(c);
  return {
    key: `show:${c.showId}`,
    problem: problems[0] ?? "no-metadata",
    problems,
    kind: "show",
    name: c.title,
    year: c.year,
    breadcrumb: [],
    path: c.path,
    collidingPaths: c.collidingPaths,
    problemText: `${joinClauses(showRowClauses(c))}. Sort this show’s episodes to place, renumber or ignore its files in one pass.`,
    // Nothing here is fixed by naming a work: the Show is already identified, and
    // the fix is an arrangement. Offering a provider search would be exactly the
    // inert "Use this" this row exists to replace.
    route: "none",
    searchSeed: c.title,
    detailPath: `/shows/${c.showId}`,
    sortPath: matcherPath(c.showId),
    titleId: "",
    showId: c.showId,
    albumId: "",
    folderPath: "",
    overrideId: "",
    // "Looks right" still means what it always meant: the uncertain PARSE is fine.
    // It settles nothing else, so it is offered only where there is an uncertain
    // parse to dismiss.
    canDismiss: c.uncertainParse > 0,
    dismissEpisodes: true,
    artworkUrl: `${API_PREFIX}/shows/${encodeURIComponent(c.showId)}/artwork/poster`,
    artworkVersion: "",
    matchedAs: "",
    hasMatch: false,
  };
}

/** The tallied problems behind one collapsed Album row, before it becomes a row.
 * `children` are the very Track rows it replaces, kept so the row can disclose
 * them. */
interface AlbumRowCounts {
  albumId: string;
  title: string;
  artist: string;
  path: string;
  /** Tracks whose album has no record at all (`album-unmatched`). */
  unmatched: number;
  /** Tracks the matched release's track list has no room for (`not-in-tracklist`). */
  notInTracklist: number;
  children: FixItem[];
}

/** `18 tracks have` / `1 track has` — the count with the noun that says what was
 * counted, in the tense the heading needs. */
function trackCountHave(n: number): string {
  return n === 1 ? "1 track has" : `${n} tracks have`;
}

/** `18 tracks are` / `1 track is` — the same count for the problem sentence. */
function trackCountAre(n: number): string {
  return n === 1 ? "1 track is" : `${n} tracks are`;
}

/** The Album row's heading: the album, then the size of the pile it stands for.
 *
 * The count is in the HEADING and not only in the sentence because the collapse's
 * whole risk is hiding work. `Braveheart` with no number beside it looks like one
 * problem; `Braveheart · 18 tracks have no metadata match` says what it is standing
 * in for before the Admin has read anything else. */
function albumRowName(c: AlbumRowCounts): string {
  const total = c.unmatched + c.notInTracklist;
  const prefix = c.title === "" ? "" : `${c.title} · `;
  return `${prefix}${trackCountHave(total)} no metadata match`;
}

/** Why an Album's tracks are stuck, phrased for the SET rather than for one track,
 * and naming the gesture this row actually offers.
 *
 * The lead is the most-stuck reason the row holds: `album-unmatched` outranks
 * `not-in-tracklist`, because an album with no record at all is further from being
 * fixed than one pinned to the wrong edition — and a row holding both still says so
 * rather than silently dropping the smaller half. */
function albumRowText(c: AlbumRowCounts): string {
  const theAlbum = c.title === "" ? "This album" : `The album ${c.title}`;
  const action =
    "Fix the album’s match here and its tracks resolve from that release’s own track list, in one go.";
  if (c.unmatched > 0) {
    const counts =
      c.notInTracklist > 0
        ? `${trackCountAre(c.unmatched)} waiting on that match, and ${trackCountAre(
            c.notInTracklist,
          )} missing from the track list of a release it was matched to.`
        : `${trackCountAre(c.unmatched)} waiting on it.`;
    return `${theAlbum} has no metadata match, so it could name none of its tracks — ${counts} ${action}`;
  }
  return `${theAlbum} matched, but ${trackCountAre(
    c.notInTracklist,
  )} not on that release’s track list — so the album is probably matched to the wrong edition. ${action}`;
}

/** One Album's album-scoped track problems as a single queue row (ADR-0050,
 * issue 09).
 *
 * It answers the queue's four questions the way every other row does — an `Album`
 * badge under `Artist › Album` (what), a representative file (which), one sentence
 * naming the reasons and their counts (what's wrong) — and its action is the one
 * the sentence has always named and never offered: a search of the ALBUM kind,
 * seeded with the album's title and narrowed by its artist, applied with
 * `cascade: true` so `mapAlbumTracks` maps the picked release's track list onto
 * these very tracks. One pick, fourteen tracks, no recheck pass. */
function albumFixItem(c: AlbumRowCounts): FixItem {
  return {
    key: `album:${c.albumId}`,
    problem: "no-metadata",
    problems: ["no-metadata"],
    kind: "album",
    name: albumRowName(c),
    // MusicBrainz release-group years are not on this row, and inventing one from a
    // track would be a claim nothing established.
    year: 0,
    breadcrumb: c.artist === "" ? [] : [c.artist],
    path: c.path,
    collidingPaths: [],
    problemText: albumRowText(c),
    route: "album-enrichment-override",
    // The subject of an ALBUM search is the album, so the seed is its title — the
    // narrowing term on a track row is the search term here.
    searchSeed: c.title,
    // Editable, blank widens, exactly as on a track row: the artist tag can itself
    // be the thing that is wrong, and a silent narrowing would strand the row.
    artistScope: c.artist,
    // No album axis to narrow an album search BY — that is the thing being searched
    // for — so the picker renders no release box.
    detailPath: `/music/albums/${c.albumId}`,
    sortPath: "",
    titleId: "",
    showId: "",
    albumId: c.albumId,
    folderPath: "",
    overrideId: "",
    // "Looks right" settles an uncertain PARSE, and there is none here: nothing
    // about these tracks' filing is in doubt, only the record above them.
    canDismiss: false,
    dismissEpisodes: false,
    artworkUrl: `${API_PREFIX}/albums/${encodeURIComponent(c.albumId)}/artwork/cover`,
    artworkVersion: "",
    // Whether the album matched differs BY REASON within one row, so a single
    // "Matched to" line could only be right for half of it. The sentence says which.
    matchedAs: "",
    hasMatch: false,
    children: c.children,
  };
}

/** A Show's orphaned Placements as their own row.
 *
 * A correction pointing at a file that is gone is BROKEN rather than done, so it is
 * promoted into the queue in its own right (CONTEXT.md "Orphaned correction") —
 * exactly as an orphaned folder override already is. It is deliberately not folded
 * into the Show's own row: an undecided file is placed, and an orphan cannot be,
 * because there is nothing left to place. */
function orphanedPlacementRow(p: ShowProblems): FixItem {
  return {
    key: `orphaned-placement:${p.showId}`,
    problem: "orphaned-correction",
    problems: ["orphaned-correction"],
    kind: "show",
    name: p.title,
    year: p.year,
    breadcrumb: [],
    path: p.orphanedPath,
    collidingPaths: [],
    problemText:
      p.orphaned === 1
        ? "1 correction points at a file that is no longer on disk, so it can never apply again. Sort this show’s episodes to drop it, or put the file back."
        : `${p.orphaned} corrections point at files that are no longer on disk, so they can never apply again. Sort this show’s episodes to drop them, or put the files back.`,
    route: "none",
    searchSeed: p.title,
    detailPath: `/shows/${p.showId}`,
    sortPath: matcherPath(p.showId),
    titleId: "",
    showId: p.showId,
    albumId: "",
    folderPath: "",
    overrideId: "",
    canDismiss: false,
    dismissEpisodes: false,
    artworkUrl: `${API_PREFIX}/shows/${encodeURIComponent(p.showId)}/artwork/poster`,
    artworkVersion: "",
    matchedAs: "",
    hasMatch: false,
  };
}

/** The filename without its directory or extension — what a never-identified file
 * is called, since it has no title. */
export function fileStem(path: string): string {
  const base = path.slice(folderOf(path).length).replace(/^[/\\]+/, "");
  const dot = base.lastIndexOf(".");
  return dot > 0 ? base.slice(0, dot) : base;
}

/** A search query guessed from a filename: the stem with the separators, quality
 * tags and release-group noise that never appear in a provider's title stripped
 * out. It is a starting point the Admin can edit, not an identity claim — identity
 * still comes only from what they pick (ADR-0002).
 *
 * When stripping leaves NOTHING — a file called `1080p.mkv` is all noise and no
 * title, which is exactly the kind of file that ends up Unmatched — it falls back
 * to the readable stem rather than an empty query. An empty seed would leave the
 * picker with nothing to search and the Admin staring at a blank box; a bad seed at
 * least shows them what the file is called and what to edit. */
export function searchableStem(path: string): string {
  const readable = fileStem(path).replace(/[._]+/g, " ").replace(/\s+/g, " ").trim();
  const cleaned = readable
    .replace(
      /\b(1080p|2160p|720p|480p|4k|uhd|hdr|x264|x265|h ?264|h ?265|hevc|bluray|blu ray|bdrip|brrip|webrip|web dl|webdl|web|hdtv|dvdrip|remux|proper|repack|extended|unrated|aac|ac3|dts|ddp?5 1|atmos)\b.*$/i,
      "",
    )
    .replace(/[[(].*$/, "")
    .replace(/\s+/g, " ")
    .trim();
  return cleaned === "" ? readable : cleaned;
}

/** The chips across the top of the queue, in queue order, with live counts. A chip
 * with no items is still rendered (dimmed) so the set of things that CAN go wrong
 * is stable and learnable rather than appearing and vanishing. */
export const FIX_PROBLEMS: { problem: FixProblem; label: string }[] = [
  { problem: "unidentified", label: "Not identified" },
  { problem: "unreadable", label: "Unreadable files" },
  { problem: "orphaned-correction", label: "Broken corrections" },
  { problem: "unassigned", label: "Not sorted" },
  { problem: "ambiguous", label: "Conflicting files" },
  { problem: "uncertain-parse", label: "Filed on a guess" },
  { problem: "no-metadata", label: "No metadata" },
];

/** Whether a row belongs under a chip. A collapsed Show row holds several problem
 * classes at once, and belongs under every one of them: filtering to "No metadata"
 * must not hide a Show whose unassigned files merely sorted first. */
export function hasProblem(item: FixItem, problem: FixProblem): boolean {
  return item.problems.includes(problem);
}

/** Per-chip empty-state copy: specific about what is fine, since "nothing here" on
 * a filtered view otherwise reads as a failed load. */
export const EMPTY_BY_PROBLEM: Record<FixProblem, string> = {
  unidentified: "Every media file in this library was recognized as a title.",
  unreadable: "Every media file in this library could be read.",
  unassigned: "Every file in this library is assigned to an episode, or ignored.",
  ambiguous: "No two files in this library claim to be the same title.",
  "orphaned-correction": "Every correction still points at a folder that exists.",
  "uncertain-parse": "Nothing in this library was filed from an uncertain parse.",
  "no-metadata": "Every title in this library has its metadata.",
};
