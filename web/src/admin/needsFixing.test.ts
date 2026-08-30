import { describe, expect, it } from "vitest";
import {
  buildFixItems,
  episodeCode,
  fileStem,
  releaseYear,
  searchableStem,
  type FixItem,
} from "./needsFixing";
import type {
  EnrichmentAttentionTitle,
  MatchOverride,
  NeedsReviewItem,
  ShowProblems,
  UnmatchedFile,
} from "../api/types";

// The row model is a pure mapping, so what a Needs-Fixing row SAYS — which item it
// names, what it claims is wrong, and which fix route it will take — is testable
// without a DOM or a server. These are the assertions that keep the screen's two
// central promises honest: a row always identifies its item unambiguously, and a
// row's fix never silently changes the wrong thing (identity vs. metadata).

const emptyContext = {
  path: "",
  showTitle: "",
  seasonNumber: 0,
  episodeNumber: 0,
  episodeLabel: "",
  artistName: "",
  albumTitle: "",
  discNumber: 0,
  trackNumber: 0,
  showId: "",
  albumId: "",
  enrichedTitle: "",
  releaseDate: "",
};

/** An identity-attention fixture may leave the two flags out: they default the way
 * the list itself does — an uncertain PARSE with no file collision, which is what
 * every item on it meant before the collision rule was surfaced. A test about a
 * collision opts in with `ambiguous` + `collidingPaths`. */
type ReviewFixture = Omit<NeedsReviewItem, "needsReview" | "ambiguous" | "collidingPaths"> &
  Partial<Pick<NeedsReviewItem, "needsReview" | "ambiguous" | "collidingPaths">>;

function reviewItem(f: ReviewFixture): NeedsReviewItem {
  return { needsReview: true, ambiguous: false, collidingPaths: [], ...f };
}

function build(over: {
  unmatched?: UnmatchedFile[];
  needsReview?: ReviewFixture[];
  enrichment?: EnrichmentAttentionTitle[];
  overrides?: MatchOverride[];
  showProblems?: ShowProblems[];
}): FixItem[] {
  return buildFixItems({
    unmatched: over.unmatched ?? [],
    needsReview: (over.needsReview ?? []).map(reviewItem),
    enrichment: over.enrichment ?? [],
    overrides: over.overrides ?? [],
    showProblems: over.showProblems ?? [],
  });
}

/** One Show's server-side unsettled counts, zeroed unless a test says otherwise. */
function showProblems(over: Partial<ShowProblems> & { showId: string }): ShowProblems {
  return {
    title: "",
    year: 0,
    path: "",
    unassigned: 0,
    unidentified: 0,
    unmatchedPaths: [],
    orphaned: 0,
    orphanedPath: "",
    ...over,
  };
}

describe("buildFixItems — naming the item", () => {
  it("names an episode by its show, season and episode code, not just the episode name", () => {
    // The defect the whole revamp exists for: the old screen printed "Pilot" and
    // nothing else, so two shows' pilots were indistinguishable.
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t1",
          kind: "episode",
          title: "Pilot",
          year: 0,
          enrichmentStatus: "unmatched",
          showTitle: "The Wire",
          seasonNumber: 1,
          episodeNumber: 3,
          path: "/media/tv/The Wire/S01/the.wire.103.mkv",
        },
      ],
    });
    expect(row.breadcrumb).toEqual(["The Wire", "Season 1"]);
    expect(row.name).toBe("S01E03 Pilot");
    expect(row.path).toBe("/media/tv/The Wire/S01/the.wire.103.mkv");
  });

  it("reads season 0 as Specials rather than a missing season", () => {
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t1",
          kind: "episode",
          title: "Christmas Special",
          year: 0,
          enrichmentStatus: "unmatched",
          showTitle: "Doctor Who",
          seasonNumber: 0,
          episodeNumber: 1,
        },
      ],
    });
    expect(row.breadcrumb).toEqual(["Doctor Who", "Specials"]);
  });

  it("names a track by its artist, album and track number", () => {
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t1",
          kind: "track",
          title: "Exit Music",
          year: 0,
          enrichmentStatus: "unmatched",
          artistName: "Radiohead",
          albumTitle: "OK Computer",
          trackNumber: 4,
        },
      ],
    });
    expect(row.breadcrumb).toEqual(["Radiohead", "OK Computer"]);
    expect(row.name).toBe("4. Exit Music");
  });

  it("falls back to the episode's raw label when it has no SxxExx numbering", () => {
    // This is exactly the case the scanner flags as needs-review, so the row must
    // show whatever the scanner COULD read rather than a blank code.
    expect(episodeCode(0, 0, "2019-04-14")).toBe("2019-04-14");
    expect(episodeCode(2, 7, "")).toBe("S02E07");
  });
});

describe("buildFixItems — stating the problem", () => {
  it("gives each needs-review reason its own sentence", () => {
    const rows = build({
      needsReview: [
        { ...emptyContext, id: "m", kind: "movie", title: "Arrival", year: 0, folderPath: "/m/Arrival", reason: "no-year" },
        { ...emptyContext, id: "e", kind: "episode", title: "Ep", year: 0, folderPath: "", reason: "episode-numbering" },
        { ...emptyContext, id: "k", kind: "track", title: "Trk", year: 0, folderPath: "/m/a", reason: "untagged" },
      ],
    });
    expect(rows[0].problemText).toMatch(/no year/i);
    expect(rows[1].problemText).toMatch(/SxxExx/);
    expect(rows[2].problemText).toMatch(/tags/i);
  });

  it("distinguishes a provider that had no record from a lookup that failed", () => {
    const rows = build({
      enrichment: [
        { ...emptyContext, id: "a", kind: "movie", title: "A", year: 0, enrichmentStatus: "unmatched" },
        { ...emptyContext, id: "b", kind: "movie", title: "B", year: 0, enrichmentStatus: "failed" },
      ],
    });
    expect(rows[0].problemText).toMatch(/no record/i);
    expect(rows[1].problemText).toMatch(/failed/i);
  });
});

describe("buildFixItems — choosing the fix route", () => {
  it("routes a wrongly-FILED item to an identity correction and a wrongly-DECORATED one to a metadata correction", () => {
    // The two operations have different blast radii (ADR-0002/0014): one re-files on
    // the next scan, the other never touches identity or watch state. The row's
    // problem decides — never inference from what the Admin typed.
    const [needsReview] = build({
      needsReview: [
        { ...emptyContext, id: "m", kind: "movie", title: "Arrival", year: 0, folderPath: "/m/Arrival", reason: "no-year" },
      ],
    });
    const [noMetadata] = build({
      enrichment: [
        { ...emptyContext, id: "t", kind: "movie", title: "Arrival", year: 2016, enrichmentStatus: "unmatched" },
      ],
    });
    expect(needsReview.route).toBe("fix-match");
    expect(noMetadata.route).toBe("enrichment-override");
  });

  it("gives an Episode the matcher instead of a provider search it cannot use", () => {
    // An Episode has no folder to anchor an identity override to, and re-picking its
    // SERIES was never the fix either: a file the provider numbers differently has
    // the right series and the wrong arrangement. So the row offers no search at all
    // — offering one is exactly the button-that-cannot-work this queue exists to end
    // — and its action is the matcher, where the arrangement is expressible.
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "e",
          kind: "episode",
          title: "Ep",
          year: 0,
          folderPath: "",
          reason: "episode-numbering",
          showId: "sh1",
          showTitle: "The Wire",
        },
      ],
    });
    expect(row.route).toBe("none");
    expect(row.sortPath).toBe("/admin/shows/sh1/matcher");
    // It is still dismissible — the parse may simply be right.
    expect(row.canDismiss).toBe(true);
  });

  it("leaves a non-Episode with no folder anchor unfixable, as before", () => {
    const [row] = build({
      needsReview: [
        { ...emptyContext, id: "k", kind: "track", title: "Trk", year: 0, folderPath: "", reason: "untagged" },
      ],
    });
    expect(row.route).toBe("none");
  });

  it("seeds an episode's search with its SHOW, because that is what the provider resolves", () => {
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t",
          kind: "episode",
          title: "Pilot",
          year: 0,
          enrichmentStatus: "unmatched",
          showTitle: "The Wire",
        },
      ],
    });
    expect(row.searchSeed).toBe("The Wire");
  });
});

describe("buildFixItems — evidence for a confirmation", () => {
  // "Looks right" asks the Admin to confirm a filing. A needs-review item is
  // flagged for an UNCERTAIN PARSE — most often a missing year — so its own parsed
  // name is exactly the thing that cannot settle the question. The record it
  // matched to can, and it carries the missing year.

  it("shows the matched record and its year for a yearless movie", () => {
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "m",
          kind: "movie",
          title: "Arrival",
          year: 0,
          folderPath: "/media/movies/Arrival",
          reason: "no-year",
          enrichmentStatus: "matched",
          releaseDate: "2016-11-11",
        },
      ],
    });
    // The year the parse never had, recovered from the matched record.
    expect(row.matchedAs).toBe("Arrival (2016)");
    expect(row.artworkUrl).toBe("/api/v1/titles/m/artwork/poster");
  });

  it("prefers the enriched display title over the parsed one", () => {
    // The parsed name of a badly-named file says nothing; the record it resolved to
    // is the evidence a confirmation is actually made against.
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "m",
          kind: "movie",
          title: "arrival.2016.1080p",
          year: 0,
          folderPath: "/media/movies/arrival",
          reason: "no-year",
          enrichmentStatus: "matched",
          enrichedTitle: "Arrival",
          releaseDate: "2016-11-11",
        },
      ],
    });
    expect(row.matchedAs).toBe("Arrival (2016)");
  });

  it("shows a collapsed Show row its Show's poster", () => {
    // An Episode has no poster of its own; its Show's is the recognizable image, and
    // the collapsed row is about the Show anyway.
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "e",
          kind: "episode",
          title: "2002-06-02",
          year: 0,
          folderPath: "",
          reason: "episode-numbering",
          showId: "sh1",
          showTitle: "The Wire",
        },
      ],
    });
    expect(row.artworkUrl).toBe("/api/v1/shows/sh1/artwork/poster");
  });

  it("claims no match when enrichment has not settled on one", () => {
    for (const status of ["pending", "unmatched", "failed", "disabled"] as const) {
      const [row] = build({
        needsReview: [
          {
            ...emptyContext,
            id: "m",
            kind: "movie",
            title: "Arrival",
            year: 0,
            folderPath: "/m/Arrival",
            reason: "no-year",
            enrichmentStatus: status,
            releaseDate: "2016-11-11",
          },
        ],
      });
      // A release date left over from an earlier pass must not be dressed up as a
      // current match — the row says there is nothing to check against instead.
      expect(row.matchedAs, status).toBe("");
      expect(row.hasMatch, status).toBe(false);
    }
  });

  it("stays quiet when the matched record only repeats the row's own heading", () => {
    // A Show has no enriched year, so a correctly-matched one would render
    // "Matched to The Wire" directly under a heading reading "The Wire" — a line
    // that costs a reading and settles nothing. The poster does the confirming.
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "sh1",
          kind: "show",
          title: "The Wire",
          year: 0,
          folderPath: "/tv/The Wire",
          reason: "no-year",
          enrichmentStatus: "matched",
        },
      ],
    });
    expect(row.matchedAs).toBe("");
    // …but it is still MATCHED. Collapsing "nothing worth printing" into "no match"
    // would make a correctly-matched Show claim it has nothing to check against.
    expect(row.hasMatch).toBe(true);
    expect(row.artworkUrl).toBe("/api/v1/shows/sh1/artwork/poster");
  });

  it("speaks up when the match contradicts the filing", () => {
    // The case that earns the line: a junk-named file matched to a real record.
    // Seeing "x264 (2013)" is what stops an Admin confirming a wrong match.
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "m",
          kind: "movie",
          title: "x264",
          year: 0,
          folderPath: "/m/x264.mkv",
          reason: "no-year",
          enrichmentStatus: "matched",
          releaseDate: "2013-02-14",
        },
      ],
    });
    expect(row.matchedAs).toBe("x264 (2013)");
  });

  it("points a track at its album cover and a show at its own poster", () => {
    const [track] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "k",
          kind: "track",
          title: "Exit Music",
          year: 0,
          folderPath: "/m/a",
          reason: "untagged",
          albumId: "al1",
        },
      ],
    });
    expect(track.artworkUrl).toBe("/api/v1/albums/al1/artwork/cover");

    const [show] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "sh1",
          kind: "show",
          title: "The Wire",
          year: 0,
          folderPath: "/tv/The Wire",
          reason: "no-year",
        },
      ],
    });
    expect(show.artworkUrl).toBe("/api/v1/shows/sh1/artwork/poster");
  });

  it("gives an unmatched file and an orphaned correction no artwork", () => {
    // Neither has an entity to fetch an image for; the correction still states the
    // identity it asserts, which is what the Admin is deciding whether to keep.
    const [file] = build({
      unmatched: [{ id: "u", path: "/m/x.mkv", folderPath: "/m", reason: "" }],
    });
    expect(file.artworkUrl).toBe("");
    expect(file.matchedAs).toBe("");

    const [orphan] = build({
      overrides: [
        { id: "o", folderPath: "/m/Gone", title: "Gone", year: 2001, identityKey: "k", orphaned: true },
      ],
    });
    expect(orphan.artworkUrl).toBe("");
    expect(orphan.matchedAs).toBe("Gone (2001)");
  });

  it("reads a year out of a release date, and refuses a junk one", () => {
    expect(releaseYear("2016-11-11")).toBe(2016);
    expect(releaseYear("")).toBe(0);
    expect(releaseYear("not-a-date")).toBe(0);
    expect(releaseYear("0001-01-01")).toBe(0);
  });
});

// The second collapse (ADR-0044, file-matcher/07): a Show's episode-level problems
// are ONE problem, so they are one row.
//
// The Batman case is what these are about. Five files at the end of season 3 are,
// per the provider, season 1 of a re-numbered continuation series. Every one
// surfaced as its own row with its own "Use this", and every one of those buttons
// was inert: the row offered to fix the SERIES, and the series was never the part
// that was wrong. Five rows, one problem, zero working fixes.
describe("buildFixItems — a Show's episode problems are one row", () => {
  const episode = (id: string, over: Partial<EnrichmentAttentionTitle> = {}) => ({
    ...emptyContext,
    id,
    kind: "episode",
    title: `Ep ${id}`,
    year: 0,
    enrichmentStatus: "unmatched" as const,
    showId: "sh1",
    showTitle: "Batman: The Animated Series",
    seasonNumber: 3,
    episodeNumber: 61,
    path: `/media/tv/Batman/Season 03/batman.${id}.mkv`,
    ...over,
  });

  it("shows one row for five broken episodes, not five", () => {
    const rows = build({
      enrichment: ["a", "b", "c", "d", "e"].map((id) => episode(id)),
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("show");
    expect(rows[0].name).toBe("Batman: The Animated Series");
  });

  it("names the actual problem and its count, not just 'problems'", () => {
    const [row] = build({
      enrichment: ["a", "b", "c", "d", "e"].map((id) => episode(id)),
    });
    // "5 problems" would answer none of the queue's four questions; the noun is
    // what says which problem was counted.
    expect(row.problemText).toContain("5 episodes have no metadata match");
  });

  it("answers all four questions: what, which file, what's wrong, how to fix it", () => {
    const [row] = build({ enrichment: [episode("a")] });
    expect(row.kind).toBe("show"); // what is it
    expect(row.name).toBe("Batman: The Animated Series");
    expect(row.path).toBe("/media/tv/Batman/Season 03/batman.a.mkv"); // which file
    expect(row.problemText).toContain("1 episode has no metadata match"); // what's wrong
    expect(row.sortPath).toBe("/admin/shows/sh1/matcher"); // how do I fix it
  });

  it("offers no fix that cannot work", () => {
    // Nothing here is fixed by naming a work — the Show is already identified. A
    // provider search on this row would be the inert "Use this" all over again.
    const [row] = build({ enrichment: [episode("a")] });
    expect(row.route).toBe("none");
    expect(row.folderPath).toBe("");
    expect(row.titleId).toBe("");
  });

  it("counts each kind of problem separately in one sentence", () => {
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "n1",
          kind: "episode",
          title: "2002-06-02",
          year: 0,
          folderPath: "",
          reason: "episode-numbering",
          showId: "sh1",
          showTitle: "Batman: The Animated Series",
        },
      ],
      enrichment: [episode("a"), episode("b")],
      showProblems: [
        showProblems({
          showId: "sh1",
          title: "Batman: The Animated Series",
          unassigned: 3,
          path: "/media/tv/Batman/Season 03/loose.mkv",
        }),
      ],
    });
    expect(row.problemText).toContain("3 files aren’t assigned to an episode");
    expect(row.problemText).toContain("1 episode was filed on a guess");
    expect(row.problemText).toContain("2 episodes have no metadata match");
    // Every class it holds, so no chip can hide it.
    expect(row.problems).toEqual(["unassigned", "uncertain-parse", "no-metadata"]);
    // The server's representative file is one of the COUNTED ones.
    expect(row.path).toBe("/media/tv/Batman/Season 03/loose.mkv");
  });

  it("uses singular wording for a single file or episode", () => {
    const [row] = build({
      showProblems: [showProblems({ showId: "sh1", title: "Batman", unassigned: 1 })],
    });
    expect(row.problemText).toContain("1 file isn’t assigned to an episode");
  });

  it("keeps a Show queued for an unassigned file — undecided is not settled", () => {
    // CONTEXT.md "Unassigned": the state exists precisely so a file the Admin took
    // off its Slot is not silently forgotten. Settling it is placing or ignoring it,
    // both one gesture in the matcher.
    const rows = build({
      showProblems: [showProblems({ showId: "sh1", title: "Batman", unassigned: 2 })],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].problem).toBe("unassigned");
    expect(rows[0].sortPath).toBe("/admin/shows/sh1/matcher");
  });

  it("clears the row once every file is assigned or ignored", () => {
    // The server drops a Show with nothing unsettled, and no flagged Episode is
    // left to re-create it. A count the matcher cannot clear would make the whole
    // queue decoration.
    expect(build({ showProblems: [] })).toHaveLength(0);
    expect(
      build({ showProblems: [showProblems({ showId: "sh1", title: "Batman" })] }),
    ).toHaveLength(0);
  });

  it("does not list an unmatched file twice — as a row and as a Show's count", () => {
    const rows = build({
      unmatched: [
        { id: "u1", path: "/media/tv/Batman/Season 03/stray.mkv", folderPath: "", reason: "" },
        { id: "u2", path: "/media/tv/loose-at-the-root.mkv", folderPath: "", reason: "" },
      ],
      showProblems: [
        showProblems({
          showId: "sh1",
          title: "Batman",
          unidentified: 1,
          unmatchedPaths: ["/media/tv/Batman/Season 03/stray.mkv"],
          path: "/media/tv/Batman/Season 03/stray.mkv",
        }),
      ],
    });
    const paths = rows.map((r) => r.path);
    expect(rows.filter((r) => r.kind === "file")).toHaveLength(1);
    // The file under the Show folder is counted by the Show row; the one outside any
    // Show folder is nobody's, and stays a row of its own.
    expect(paths).toContain("/media/tv/loose-at-the-root.mkv");
    expect(rows.filter((r) => r.path === "/media/tv/Batman/Season 03/stray.mkv")).toHaveLength(1);
  });

  it("promotes an orphaned Placement into a row of its own", () => {
    // A correction pointing at a file that is gone is BROKEN rather than done
    // (CONTEXT.md "Orphaned correction"), exactly like an orphaned folder override.
    // It is not folded in with the undecided files: an undecided file can be placed,
    // and an orphan cannot, because there is nothing left to place.
    const rows = build({
      showProblems: [
        showProblems({
          showId: "sh1",
          title: "Batman",
          unassigned: 1,
          path: "/media/tv/Batman/Season 03/loose.mkv",
          orphaned: 2,
          orphanedPath: "/media/tv/Batman/Season 03/gone.mkv",
        }),
      ],
    });
    const orphan = rows.find((r) => r.problem === "orphaned-correction");
    expect(orphan).toBeDefined();
    expect(orphan!.path).toBe("/media/tv/Batman/Season 03/gone.mkv");
    expect(orphan!.problemText).toContain("2 corrections point at files that are no longer on disk");
    expect(orphan!.sortPath).toBe("/admin/shows/sh1/matcher");
    // And it is a SEPARATE row from the Show's undecided files.
    expect(rows).toHaveLength(2);
    expect(new Set(rows.map((r) => r.key)).size).toBe(2);
  });

  it("dismisses the whole set a collapsed row stands for, in one call", () => {
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "n1",
          kind: "episode",
          title: "2002-06-02",
          year: 0,
          folderPath: "",
          reason: "episode-numbering",
          showId: "sh1",
          showTitle: "Batman",
        },
      ],
    });
    expect(row.canDismiss).toBe(true);
    expect(row.dismissEpisodes).toBe(true);
  });

  it("offers no dismissal when there is no uncertain parse to dismiss", () => {
    // "Looks right" has always meant "the PARSE is fine". It settles nothing about a
    // missing metadata record or an unplaced file, so it is not offered for them.
    const [row] = build({
      showProblems: [showProblems({ showId: "sh1", title: "Batman", unassigned: 1 })],
    });
    expect(row.canDismiss).toBe(false);
  });

  it("counts one Episode once even when both lists flag it", () => {
    const shared = {
      ...emptyContext,
      id: "t",
      kind: "episode",
      title: "The Target",
      year: 0,
      showId: "sh1",
      showTitle: "The Wire",
    };
    const [row] = build({
      needsReview: [{ ...shared, folderPath: "", reason: "episode-numbering" as const }],
      enrichment: [{ ...shared, enrichmentStatus: "failed" as const }],
    });
    expect(row.problemText).toContain("1 episode was filed on a guess");
    expect(row.problemText).not.toContain("metadata match");
  });

  it("collapses per Show, not across the library", () => {
    const rows = build({
      enrichment: [
        episode("a"),
        episode("b", { showId: "sh2", showTitle: "The Wire" }),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.name).sort()).toEqual([
      "Batman: The Animated Series",
      "The Wire",
    ]);
  });
});

describe("buildFixItems — a file collision", () => {
  // docs/naming-convention.md: "two files that parse to the same Edition identity
  // and are not parts are flagged ambiguous in the web app, never silently
  // guessed". The flag was written, stored, serialized and normalized — and then
  // rendered nowhere, so the promise was half kept. These are the half that was
  // missing, plus the reason it matters: only the first of the colliding files
  // plays, so a queue that says nothing leaves an Admin to find that out by
  // watching something.

  const collidingMovie = {
    ...emptyContext,
    id: "m1",
    kind: "movie",
    title: "Dune",
    year: 2021,
    folderPath: "/media/movies/Dune (2021)",
    reason: "no-year" as const,
    path: "/media/movies/Dune (2021)/Dune (2021).mkv",
    needsReview: false,
    ambiguous: true,
    collidingPaths: [
      "/media/movies/Dune (2021)/Dune (2021).mkv",
      "/media/movies/Dune (2021)/Dune (2021) (repack).mkv",
    ],
  };

  it("gives an ambiguous Movie a row that names the colliding files", () => {
    const [row] = build({ needsReview: [collidingMovie] });
    expect(row.problems).toEqual(["ambiguous"]);
    expect(row.problem).toBe("ambiguous");
    expect(row.collidingPaths).toEqual(collidingMovie.collidingPaths);
    // The consequence, stated: the Admin should not have to discover it by playing
    // the film and finding half of it missing.
    expect(row.problemText).toMatch(/only the first one plays/i);
    // And what to do about it — a Movie has no matcher, so the fix is the
    // convention's own escape hatch.
    expect(row.problemText).toMatch(/edition tag|part suffix/i);
  });

  it("offers neither a dismissal nor a provider search for a collision", () => {
    const [row] = build({ needsReview: [collidingMovie] });
    // "Looks right" answers a question about the PARSE. Both files are still there
    // afterwards and one still does not play, so dismissing would hide a real
    // conflict.
    expect(row.canDismiss).toBe(false);
    // Naming the work again cannot say WHICH file is it — the inert "Use this" this
    // queue exists to replace.
    expect(row.route).toBe("none");
  });

  it("keeps both problems on one row when an item is flagged twice", () => {
    // One item, two genuinely different problems with two different fixes. Two rows
    // would be the five-rows-one-problem shape at a smaller scale; `problems` is
    // exactly the field that lets one row hold both.
    const [row] = build({
      needsReview: [{ ...collidingMovie, needsReview: true, year: 0 }],
    });
    expect(row.problems).toEqual(["ambiguous", "uncertain-parse"]);
    expect(row.problemText).toMatch(/only the first one plays/i);
    expect(row.problemText).toMatch(/no year/i);
    // The parse half IS dismissible and IS searchable; the collision half is not,
    // and does not take those away.
    expect(row.canDismiss).toBe(true);
    expect(row.route).toBe("fix-match");
  });

  it("folds an ambiguous Episode into its Show's row, with the matcher as the fix", () => {
    const [row] = build({
      needsReview: [
        {
          ...emptyContext,
          id: "e1",
          kind: "episode",
          title: "System",
          year: 0,
          folderPath: "",
          reason: "no-year" as const,
          showId: "sh1",
          showTitle: "The Bear",
          path: "/media/tv/The Bear/Season 1/The Bear - S01E05-E06.mkv",
          needsReview: false,
          ambiguous: true,
          collidingPaths: [
            "/media/tv/The Bear/Season 1/The Bear - S01E05-E06.mkv",
            "/media/tv/The Bear/Season 1/The Bear - S01E06.mkv",
          ],
        },
      ],
    });
    // One row per Show (file-matcher/07) — a collision must not reintroduce a row
    // per episode.
    expect(row.kind).toBe("show");
    expect(row.name).toBe("The Bear");
    expect(row.problems).toContain("ambiguous");
    expect(row.problemText).toContain("1 episode has two files claiming to be it");
    expect(row.collidingPaths).toHaveLength(2);
    // The matcher is the screen that can settle it.
    expect(row.sortPath).toBe("/admin/shows/sh1/matcher");
    // Nothing here is an uncertain parse, so there is nothing to call "right".
    expect(row.canDismiss).toBe(false);
  });

  it("counts collisions separately from uncertain parses on one Show row", () => {
    const base = {
      ...emptyContext,
      kind: "episode",
      year: 0,
      folderPath: "",
      showId: "sh1",
      showTitle: "The Bear",
    };
    const [row] = build({
      needsReview: [
        {
          ...base,
          id: "e1",
          title: "System",
          reason: "no-year" as const,
          needsReview: false,
          ambiguous: true,
          collidingPaths: ["/a.mkv", "/b.mkv"],
        },
        { ...base, id: "e2", title: "2002-06-02", reason: "episode-numbering" as const },
      ],
    });
    expect(row.problems).toEqual(["ambiguous", "uncertain-parse"]);
    expect(row.problemText).toContain("1 episode has two files claiming to be it");
    expect(row.problemText).toContain("1 episode was filed on a guess");
    // "Looks right" can still clear the parse half — and only that half.
    expect(row.canDismiss).toBe(true);
  });
});

describe("buildFixItems — corrections", () => {
  const orphan: MatchOverride = {
    id: "o1",
    folderPath: "/media/movies/Gone",
    title: "Gone Movie",
    year: 2001,
    identityKey: "k",
    orphaned: true,
  };
  const settled: MatchOverride = { ...orphan, id: "o2", orphaned: false };

  it("promotes an orphaned correction into the queue and leaves settled ones out", () => {
    const rows = build({ overrides: [orphan, settled] });
    expect(rows).toHaveLength(1);
    expect(rows[0].problem).toBe("orphaned-correction");
    expect(rows[0].overrideId).toBe("o1");
    // It is resolved by discarding it, not by searching for a record.
    expect(rows[0].route).toBe("none");
  });
});

describe("buildFixItems — ordering", () => {
  it("puts the most-broken problems first, regardless of which list they came from", () => {
    const rows = build({
      unmatched: [{ id: "u", path: "/m/x.mkv", folderPath: "/m", reason: "" }],
      needsReview: [
        { ...emptyContext, id: "m", kind: "movie", title: "A", year: 0, folderPath: "/m/A", reason: "no-year" },
      ],
      enrichment: [
        { ...emptyContext, id: "t", kind: "movie", title: "B", year: 0, enrichmentStatus: "unmatched" },
      ],
      overrides: [
        { id: "o", folderPath: "/m/Gone", title: "G", year: 0, identityKey: "k", orphaned: true },
      ],
    });
    expect(rows.map((r) => r.problem)).toEqual([
      "unidentified",
      "orphaned-correction",
      "uncertain-parse",
      "no-metadata",
    ]);
  });
});

describe("buildFixItems — the fix anchor for an unmatched file", () => {
  it("uses the anchor the SERVER derived, not the file's own directory", () => {
    // In a TV library the file's directory is a Season folder; an override keyed
    // there would never match the Show the scanner resolves. The server derives the
    // anchor from the Library's kind, so the client must not re-derive it.
    const [row] = build({
      unmatched: [
        {
          id: "u",
          path: "/media/tv/The Wire/Season 01/loose.mkv",
          folderPath: "/media/tv/The Wire",
          reason: "",
        },
      ],
    });
    expect(row.folderPath).toBe("/media/tv/The Wire");
  });

  it("falls back to the file's directory when the server sent no anchor", () => {
    const [row] = build({
      unmatched: [{ id: "u", path: "/media/movies/x.mkv", folderPath: "", reason: "" }],
    });
    expect(row.folderPath).toBe("/media/movies");
  });
});

describe("filename guessing", () => {
  it("names an unidentified file by its stem", () => {
    expect(fileStem("/media/movies/arrival.2016.1080p.mkv")).toBe("arrival.2016.1080p");
    expect(fileStem("/media/movies/1080p.mkv")).toBe("1080p");
  });

  it("strips release noise so the seeded search is something a provider can match", () => {
    // A seed is a starting point the Admin can edit, never an identity claim —
    // identity still comes only from the record they pick (ADR-0002).
    expect(searchableStem("/m/Arrival.2016.1080p.BluRay.x264-GROUP.mkv")).toBe("Arrival 2016");
    expect(searchableStem("/m/The_Matrix_1999_2160p_HDR.mkv")).toBe("The Matrix 1999");
    expect(searchableStem("/m/Dune (2021) [remux].mkv")).toBe("Dune");
  });

  it("falls back to the readable stem when a filename is ALL release noise", () => {
    // "1080p.mkv" is exactly the kind of file that ends up Unmatched, and it cleans
    // down to nothing. An empty seed would leave the picker with no search to run
    // and the Admin with a blank box; the stem at least shows what to edit.
    expect(searchableStem("/media/movies/1080p.mkv")).toBe("1080p");
    expect(searchableStem("/media/tv/x264.mkv")).toBe("x264");
  });
});
