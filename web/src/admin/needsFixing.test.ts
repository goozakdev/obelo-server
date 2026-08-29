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

function build(over: {
  unmatched?: UnmatchedFile[];
  needsReview?: NeedsReviewItem[];
  enrichment?: EnrichmentAttentionTitle[];
  overrides?: MatchOverride[];
}): FixItem[] {
  return buildFixItems({
    unmatched: over.unmatched ?? [],
    needsReview: over.needsReview ?? [],
    enrichment: over.enrichment ?? [],
    overrides: over.overrides ?? [],
  });
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

  it("gives an Episode the metadata pin, the only correction it can have", () => {
    // An Episode has no folder to anchor an identity override to, but it CAN be
    // pointed at the right series+episode — which is what repairs a file whose
    // on-disk numbering doesn't line up with the provider's.
    const [row] = build({
      needsReview: [
        { ...emptyContext, id: "e", kind: "episode", title: "Ep", year: 0, folderPath: "", reason: "episode-numbering" },
      ],
    });
    expect(row.route).toBe("enrichment-override");
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

  it("prefers the enriched display title, which is the whole point for an episode", () => {
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
          enrichmentStatus: "matched",
          enrichedTitle: "The Target",
          releaseDate: "2002-06-02",
        },
      ],
    });
    expect(row.matchedAs).toBe("The Target (2002)");
    // An Episode has no poster of its own; its Show's is the recognizable image.
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

describe("buildFixItems — an Episode is always fixable", () => {
  it("offers the fix even when the on-disk numbering is unreadable", () => {
    // Enrichment looks an Episode up by the season/episode parsed from its FILENAME,
    // so a file with unreadable numbering (or numbering the provider disagrees with)
    // used to be permanently unmatchable — `/tv/{show}/season/0/episode/0` 404s
    // whatever series is picked. Pinning the series AND the episode addresses the
    // lookup directly, which is what makes such a file fixable at all.
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t",
          kind: "episode",
          title: "The Target",
          year: 0,
          enrichmentStatus: "failed",
          showTitle: "The Wire",
          seasonNumber: 1,
          episodeNumber: 0,
          episodeLabel: "2002-06-02",
        },
      ],
    });
    expect(row.route).toBe("enrichment-override");
  });

  it("lists an Episode once, on the row that carries the fix", () => {
    // The needs-review row already offers the same series+episode pin PLUS a
    // dismissal, so a second row for the same file would be a strict subset of it.
    const episode = {
      ...emptyContext,
      id: "t",
      kind: "episode",
      title: "The Target",
      year: 0,
      showTitle: "The Wire",
      seasonNumber: 1,
      episodeNumber: 0,
      episodeLabel: "2002-06-02",
    };
    const rows = build({
      needsReview: [{ ...episode, folderPath: "", reason: "episode-numbering" as const }],
      enrichment: [{ ...episode, enrichmentStatus: "failed" as const }],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].problem).toBe("uncertain-parse");
    expect(rows[0].route).toBe("enrichment-override");
  });

  it("still lists the metadata row on its own when there is no numbering row", () => {
    const rows = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t",
          kind: "episode",
          title: "The Target",
          year: 0,
          enrichmentStatus: "failed",
          showTitle: "The Wire",
          seasonNumber: 1,
          episodeNumber: 0,
        },
      ],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].problem).toBe("no-metadata");
    expect(rows[0].route).toBe("enrichment-override");
  });

  it("still offers the search for an Episode that IS numbered", () => {
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
          seasonNumber: 1,
          episodeNumber: 3,
        },
      ],
    });
    expect(row.route).toBe("enrichment-override");
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
