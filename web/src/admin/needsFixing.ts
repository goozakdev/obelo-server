import type {
  EnrichmentAttentionTitle,
  MatchOverride,
  NeedsReviewItem,
  UnmatchedFile,
} from "../api/types";
import { API_PREFIX } from "../api/client";
import { folderOf } from "./paths";

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

/** What is wrong with a row — the axis the queue's filter chips segment on. */
export type FixProblem =
  | "unidentified" // an Unmatched file: no Title was derived at all
  | "uncertain-parse" // needs-review: a Title exists but the parse was a guess
  | "no-metadata" // enrichment could not settle on a provider record
  | "orphaned-correction"; // a Match override whose anchor folder is gone

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
  /** Stable, unique across all four sources (ids can repeat between lists). */
  key: string;
  problem: FixProblem;
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
  /** One sentence stating what is actually wrong, in the Admin's terms. */
  problemText: string;
  /** How to apply a picked record for this row. */
  route: FixRoute;
  /** Pre-filled provider query — the thing the Admin would have typed. */
  searchSeed: string;
  /** In-app route to the item's own page, or "" when it has none (a file that
   * never became a Title). */
  detailPath: string;
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
 * an identity fix-match; an Episode has no folder, so its correction is a metadata
 * pin at series+episode grain — the only thing that can repair a file the provider
 * numbers differently. Anything else has no search to offer. */
function episodeFixRoute(item: { kind: string; folderPath: string }): FixRoute {
  if (item.folderPath !== "") return "fix-match";
  return item.kind === "episode" ? "enrichment-override" : "none";
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
}): FixItem[] {
  const unidentified: FixItem[] = input.unmatched.map((f) => ({
    key: `unmatched:${f.id}`,
    problem: "unidentified" as const,
    kind: "file",
    name: fileStem(f.path),
    year: 0,
    breadcrumb: [],
    path: f.path,
    problemText: f.reason
      ? `Not recognized as a title — ${f.reason}.`
      : "Not recognized as a title — no name or year could be read from this file.",
    route: "fix-match" as const,
    searchSeed: searchableStem(f.path),
    detailPath: "",
    titleId: "",
    showId: "",
    // The server derives the anchor from the Library's kind; folderOf is the
    // fallback for a server that predates that field, and is only ever right for a
    // Movie library (a TV file's own directory is a Season folder, not the Show).
    folderPath: f.folderPath || folderOf(f.path),
    overrideId: "",
    canDismiss: false,
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
      kind: "file",
      name: o.title,
      year: o.year,
      breadcrumb: [],
      path: o.folderPath,
      problemText:
        "This correction points at a folder that no longer exists, so it can never apply again. Discard it, or restore the folder.",
      route: "none" as const,
      searchSeed: o.title,
      detailPath: "",
      titleId: "",
      showId: "",
      folderPath: o.folderPath,
      overrideId: o.id,
      canDismiss: false,
      artworkUrl: "",
      artworkVersion: "",
      // The correction itself records the identity it asserts, which is exactly
      // what the Admin is deciding whether to keep.
      matchedAs: o.year > 0 ? `${o.title} (${o.year})` : o.title,
      // The correction IS the assertion, so there is always something to show.
      hasMatch: true,
    }));

  const uncertain: FixItem[] = input.needsReview.map((t) => ({
    key: `review:${t.kind}:${t.id}`,
    problem: "uncertain-parse" as const,
    kind: t.kind,
    name: displayNameFor(t),
    year: t.year,
    breadcrumb: breadcrumbFor(t),
    path: t.path,
    problemText: uncertainParseText(t.reason, t.kind),
    // A folder override is the fix only where one can anchor. An Episode has no
    // folder of its own — but it can still be pointed at the right provider record
    // (series + episode), which is the only correction an Episode has and the one
    // that fixes a file whose numbering doesn't line up with the provider's.
    route: episodeFixRoute(t),
    searchSeed: searchSeedFor(t),
    detailPath: t.kind === "show" ? `/shows/${t.id}` : `/titles/${t.id}`,
    titleId: t.kind === "show" ? "" : t.id,
    showId: t.kind === "show" ? t.id : "",
    folderPath: t.folderPath,
    overrideId: "",
    canDismiss: true,
    // This is the row that asks the Admin to CONFIRM a filing, so it is the row
    // that most needs the evidence: the poster and the record it matched to.
    artworkUrl: artworkUrlFor(t),
    artworkVersion: t.enrichmentStatus,
    matchedAs: matchedAsFor(t, displayedNameFor(displayNameFor(t), t.year)),
    hasMatch: t.enrichmentStatus === "matched",
  }));

  // Titles that already have a needs-review row. An Episode blocked by its own
  // numbering would otherwise appear TWICE — once for the numbering, once for the
  // metadata that numbering blocks — same file, same root cause, and only the first
  // row actionable. One row with one action beats two rows and a riddle.
  const flaggedTitles = new Set(input.needsReview.map((t) => t.id));

  const noMetadata: FixItem[] = input.enrichment.flatMap((t) => {
    // One file, one row. An Episode flagged for its numbering already appears above
    // as a needs-review row, and that row now carries the same series+episode fix
    // (see episodeFixRoute) plus a "looks right" dismissal — so a second row for the
    // same file would offer a subset of the same actions. Dismiss the first and this
    // one returns, still fixable.
    if (t.kind === "episode" && flaggedTitles.has(t.id)) return [];
    return {
      key: `enrichment:${t.id}`,
      problem: "no-metadata" as const,
      kind: t.kind,
      name: displayNameFor(t),
      year: t.year,
      breadcrumb: breadcrumbFor(t),
      path: t.path,
      problemText:
        t.enrichmentStatus === "failed"
          ? "The metadata lookup failed — the provider was unreachable, or it has no episode at this season and number."
          : "No metadata match — the provider had no record for this name, so there is no artwork or description.",
      route: "enrichment-override" as const,
      searchSeed: searchSeedFor(t),
      detailPath: `/titles/${t.id}`,
      titleId: t.id,
      showId: "",
      folderPath: "",
      overrideId: "",
      canDismiss: false,
      artworkUrl: artworkUrlFor(t),
      artworkVersion: t.enrichmentStatus,
      // Nothing matched — that IS this row's problem — so there is no record to
      // print; the problem sentence already says so.
      matchedAs: "",
      hasMatch: false,
    };
  });

  return [...unidentified, ...orphaned, ...uncertain, ...noMetadata];
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
  { problem: "uncertain-parse", label: "Filed on a guess" },
  { problem: "no-metadata", label: "No metadata" },
];

/** Per-chip empty-state copy: specific about what is fine, since "nothing here" on
 * a filtered view otherwise reads as a failed load. */
export const EMPTY_BY_PROBLEM: Record<FixProblem, string> = {
  unidentified: "Every media file in this library was recognized as a title.",
  "orphaned-correction": "Every correction still points at a folder that exists.",
  "uncertain-parse": "Nothing in this library was filed from an uncertain parse.",
  "no-metadata": "Every title in this library has its metadata.",
};
