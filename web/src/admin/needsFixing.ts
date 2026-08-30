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
 *  - `none`: an orphaned correction, resolved by discarding it, not by searching. */
export type FixRoute = "fix-match" | "enrichment-override" | "none";

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
  /** "movie" | "episode" | "track" | "show" | "file". */
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
}

/** Human labels for the kind badge. A never-identified file is its own "kind" here
 * because it is precisely NOT any of the catalog kinds yet. */
const KIND_LABELS: Record<string, string> = {
  movie: "Movie",
  episode: "Episode",
  track: "Track",
  show: "Show",
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

/** The query to seed the provider search with. For an Episode this is the SHOW,
 * not the episode: TMDB resolves an episode through its show (`/search/tv`), so
 * the show title is both what the search wants and what the Admin would type. */
function searchSeedFor(item: {
  kind: string;
  title: string;
  showTitle: string;
  albumTitle: string;
}): string {
  if (item.kind === "episode") return item.showTitle || item.title;
  if (item.kind === "track") return item.albumTitle || item.title;
  return item.title;
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
  for (const p of showProblems) {
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

  const unidentified: FixItem[] = input.unmatched
    // A file counted inside a Show row must not also be a row of its own: that is
    // the five-rows-one-problem shape returning by the back door.
    .filter((f) => !foldedPaths.has(f.path))
    .map((f) => ({
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
    }));

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
        detailPath: t.kind === "show" ? `/shows/${t.id}` : `/titles/${t.id}`,
        sortPath: t.kind === "episode" && t.showId !== "" ? matcherPath(t.showId) : "",
        titleId: t.kind === "show" ? "" : t.id,
        showId: t.kind === "show" ? t.id : "",
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

  const noMetadata: FixItem[] = input.enrichment.flatMap((t) => {
    // Collapsed into their Show's row above.
    if (t.kind === "episode" && shows.has(t.showId)) return [];
    // One file, one row: an Episode flagged for its numbering already appears as a
    // needs-review row carrying the same actions.
    if (t.kind === "episode" && flaggedTitles.has(t.id)) return [];
    return {
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
          ? "The metadata lookup failed — the provider was unreachable, or it has no episode at this season and number."
          : "No metadata match — the provider had no record for this name, so there is no artwork or description.",
      // An Episode's metadata problem is not fixed by naming a work — see
      // episodeFixRoute. Every other kind IS, so it keeps its search.
      route: (t.kind === "episode" ? "none" : "enrichment-override") as FixRoute,
      searchSeed: searchSeedFor(t),
      detailPath: `/titles/${t.id}`,
      sortPath: t.kind === "episode" && t.showId !== "" ? matcherPath(t.showId) : "",
      titleId: t.id,
      showId: "",
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
    };
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
  unassigned: "Every file in this library is assigned to an episode, or ignored.",
  ambiguous: "No two files in this library claim to be the same title.",
  "orphaned-correction": "Every correction still points at a folder that exists.",
  "uncertain-parse": "Nothing in this library was filed from an uncertain parse.",
  "no-metadata": "Every title in this library has its metadata.",
};
