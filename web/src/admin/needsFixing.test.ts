import { describe, expect, it } from "vitest";
import {
  buildFixItems,
  episodeCode,
  fileStem,
  hasProblem,
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

/** An enrichment-attention fixture may leave the REASON out, and it defaults to
 * "" — which is a real state, not a hole: every row in a library that has not been
 * re-passed since the server started recording one carries it, as does every
 * outcome with no diagnosis to give. A test about a specific diagnosis opts in. */
type EnrichmentFixture = Omit<EnrichmentAttentionTitle, "enrichmentReason"> &
  Partial<Pick<EnrichmentAttentionTitle, "enrichmentReason">>;

function enrichmentItem(f: EnrichmentFixture): EnrichmentAttentionTitle {
  return { enrichmentReason: "", ...f };
}

function build(over: {
  unmatched?: UnmatchedFile[];
  needsReview?: ReviewFixture[];
  enrichment?: EnrichmentFixture[];
  overrides?: MatchOverride[];
  showProblems?: ShowProblems[];
}): FixItem[] {
  return buildFixItems({
    unmatched: over.unmatched ?? [],
    needsReview: (over.needsReview ?? []).map(reviewItem),
    enrichment: (over.enrichment ?? []).map(enrichmentItem),
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
    unreadablePaths: [],
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

  // ADR-0050. A track reaches this queue four genuinely different ways wanting four
  // different actions, and before the reason column all four rendered one sentence
  // that told the Admin nothing the row's existence had not. These assertions are
  // about the ACTION each sentence names, not its wording: what makes the column
  // worth a migration is that `album-unmatched` sends the Admin to the ALBUM while
  // `search-no-match` sends them to a recording picker.
  const track = (enrichmentReason: string, albumTitle = "OK Computer") => ({
    ...emptyContext,
    id: "t",
    kind: "track",
    title: "Airbag",
    year: 0,
    artistName: "Radiohead",
    albumTitle,
    enrichmentStatus: "unmatched" as const,
    enrichmentReason,
  });

  it("sends the largest bucket to the ALBUM, by name, and not to a recording search", () => {
    // 365 of the developer's 730 unmatched tracks. Every one of them used to be
    // offered a per-track recording search that could not have fixed any of them:
    // the album is what has no record, and matching it resolves all of its tracks
    // from the album's own track list.
    const [row] = build({ enrichment: [track("album-unmatched")] });
    expect(row.problemText).toContain("OK Computer");
    expect(row.problemText).toMatch(/album/i);
    expect(row.problemText).toMatch(/fix the album/i);
    // And not the generic sentence, which claims the PROVIDER had no record for
    // this track's name — a different, and here untrue, statement.
    expect(row.problemText).not.toMatch(/no record for this name/i);
  });

  it("names the album even when the row does not carry its title", () => {
    // The breadcrumb is empty for a track whose album context the server could not
    // supply. The sentence still has to read, and still has to point at the album.
    const [row] = build({ enrichment: [track("album-unmatched", "")] });
    expect(row.problemText).toMatch(/this track.s album/i);
    expect(row.problemText).toMatch(/fix the album/i);
  });

  it("blames the RELEASE when the album matched but its track list has no room", () => {
    const [row] = build({ enrichment: [track("not-in-tracklist")] });
    expect(row.problemText).toMatch(/track list/i);
    expect(row.problemText).toMatch(/wrong edition|release/i);
  });

  it("blames the ID, not the name, when an exact recording id resolved to nothing", () => {
    // The distinguishing claim: searching by name is the wrong offer here, because
    // the name was never what failed.
    const [row] = build({ enrichment: [track("tag-id-unresolved")] });
    expect(row.problemText).toMatch(/id/i);
    expect(row.problemText).toMatch(/retag/i);
    expect(row.problemText).not.toMatch(/album/i);
  });

  it("tells an empty search apart from a rejected near miss", () => {
    // Both end the pass identically in every column but this one. They are different
    // things to tell an Admin: "MusicBrainz has nothing under this title" versus
    // "something came back and was refused rather than stored blind, so the right
    // answer is probably in the picker's list".
    const [empty] = build({ enrichment: [track("search-no-match")] });
    const [rejected] = build({ enrichment: [track("search-rejected")] });
    expect(empty.problemText).not.toBe(rejected.problemText);
    expect(empty.problemText).toMatch(/nothing came back/i);
    expect(rejected.problemText).toMatch(/none of their titles matched/i);
    expect(rejected.problemText).toMatch(/probably in the list/i);
  });

  it("falls back to the generic sentence for a blank or unrecognized reason", () => {
    // The two degradations that must both work: a library not re-passed since the
    // column shipped carries "", and a newer server may send a value this build has
    // never heard of. Neither may blank the row or throw — both render exactly what
    // this screen said before the reason existed.
    const generic = build({
      enrichment: [{ ...emptyContext, id: "m", kind: "movie", title: "A", year: 0,
        enrichmentStatus: "unmatched" as const }],
    })[0].problemText;
    expect(build({ enrichment: [track("")] })[0].problemText).toBe(generic);
    expect(build({ enrichment: [track("some-future-reason")] })[0].problemText).toBe(generic);
  });

  it("keeps the provider-error sentence for a failed row, whatever reason it carries", () => {
    // A 'failed' row is ADR-0048's territory: the provider refused or the trouble has
    // outlived a day of retries. Its reason column is stale by construction (a
    // transient failure never rewrites it), so the status has to win.
    const [row] = build({
      enrichment: [{ ...track("album-unmatched"), enrichmentStatus: "failed" as const }],
    });
    expect(row.problemText).toMatch(/failed/i);
    expect(row.problemText).not.toMatch(/fix the album/i);
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

  it("seeds a track's search with the TRACK, and carries the album as a narrowing term", () => {
    // The opposite of the Episode rule, and reading that rule onto music was the bug
    // (ADR-0050, needs-fixing/06): a Track row searches MusicBrainz /recording, whose
    // subject is the recording, so seeding it with the album searched for every
    // recording ever called "She". The album narrows; it does not name the thing.
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "t",
          kind: "track",
          title: "(I Could Only) Whisper Your Name",
          year: 0,
          enrichmentStatus: "unmatched",
          artistName: "Harry Connick, Jr.",
          albumTitle: "She",
          trackNumber: 5,
        },
      ],
    });
    expect(row.searchSeed).toBe("(I Could Only) Whisper Your Name");
    expect(row.artistScope).toBe("Harry Connick, Jr.");
    expect(row.albumScope).toBe("She");
  });

  it("gives a video row no narrowing axis at all, so its picker shows no boxes", () => {
    const [row] = build({
      enrichment: [
        {
          ...emptyContext,
          id: "m",
          kind: "movie",
          title: "Arrival",
          year: 2016,
          enrichmentStatus: "unmatched",
        },
      ],
    });
    expect(row.searchSeed).toBe("Arrival");
    // undefined, not "": a blank box is a music row the Admin can still narrow by
    // hand, while no box at all is a kind with no artist or release axis.
    expect(row.artistScope).toBeUndefined();
    expect(row.albumScope).toBeUndefined();
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

// A corrupt file is not a naming problem, and the queue used to say it was.
//
// The scanner lists it beside the files it could not name, because both end the same way: on
// disk, in no Title. But its name parsed perfectly — the file matcher even shows it correctly
// placed on its Slot — and ffprobe refused the bytes, so no Episode was built and none can be
// until the FILE is replaced. Told "not recognized as a title", an Admin searches the provider,
// presses "Use this", writes an identity correction for an identity that was already right, and
// the row returns on the next scan. Forever. (ADR-0047.)
describe("buildFixItems — a file that cannot be read", () => {
  const corrupt = {
    id: "u1",
    path: "/media/tv/The Marlow Murder Club/Season 1/S01E01.mkv",
    // The server withholds the anchor for these: there is no fix-match to key.
    folderPath: "",
    kind: "unreadable" as const,
    reason: "ffprobe: Invalid data found when processing input",
  };

  it("says the file could not be read, and quotes what ffprobe said", () => {
    const [row] = build({ unmatched: [corrupt] });
    expect(row.problem).toBe("unreadable");
    expect(row.problemText).toContain("could not be read");
    expect(row.problemText).toContain("Invalid data found when processing input");
    // The old sentence is a lie about this file: its name was never the problem.
    expect(row.problemText).not.toContain("Not recognized as a title");
  });

  it("offers no provider search, because naming the work cannot fix it", () => {
    const [row] = build({ unmatched: [corrupt] });
    expect(row.route).toBe("none");
    expect(row.searchSeed).toBe("");
    expect(row.folderPath).toBe("");
  });

  it("still routes an unidentified file to the search", () => {
    // The two kinds share a list and must not share a fix.
    const [row] = build({
      unmatched: [
        {
          id: "u2",
          path: "/media/movies/mystery.mkv",
          folderPath: "/media/movies",
          kind: "unidentified" as const,
          reason: "",
        },
      ],
    });
    expect(row.problem).toBe("unidentified");
    expect(row.route).toBe("fix-match");
  });

  it("points at the Show it belongs to, so the Admin can go and ignore it", () => {
    // Attributed, never counted: no Show row exists, and the flat row still knows where the
    // one gesture that settles this file lives.
    const rows = build({
      unmatched: [corrupt],
      showProblems: [
        showProblems({
          showId: "sh1",
          title: "The Marlow Murder Club",
          unreadablePaths: [corrupt.path],
        }),
      ],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].problem).toBe("unreadable");
    expect(rows[0].sortPath).toContain("sh1");
    expect(rows[0].breadcrumb).toEqual(["The Marlow Murder Club"]);
  });

  it("keeps its own row rather than being counted inside a Show", () => {
    // The server does not fold an unreadable path into a Show row, so the client never drops
    // it: a file that needs a human must not vanish behind a matcher screen that reports the
    // Show as already sorted.
    const rows = build({
      unmatched: [corrupt],
      showProblems: [
        showProblems({
          showId: "sh1",
          title: "The Marlow Murder Club",
          unassigned: 1,
          path: "/x.mkv",
          unreadablePaths: [corrupt.path],
        }),
      ],
    });
    expect(rows.some((r) => r.problem === "unreadable")).toBe(true);
    expect(rows.some((r) => r.problem === "unassigned")).toBe(true);
  });
});

describe("buildFixItems — one row per Album", () => {
  // Braveheart, measured (ADR-0050, album-resolves-its-tracks/09). 557 flagged
  // tracks in the developer's library were 90 album problems; that one album alone
  // was 18 identical rows, every one of them saying "fix the album's match" and
  // every one of them offering a RECORDING search that cannot perform it.
  //
  // The collapse is deliberately NARROWER than the Show one. There, every
  // episode-level row folded because re-picking the series was inert for all of
  // them. Here a `search-no-match` / `search-rejected` / `tag-id-unresolved` track
  // is genuinely fixed by picking a recording on its own row, so folding those in
  // would take a working action away — which is the assertion this whole suite
  // exists around.

  const albumTrack = (
    id: string,
    enrichmentReason: string,
    over: Partial<EnrichmentAttentionTitle> = {},
  ): EnrichmentFixture => ({
    ...emptyContext,
    id,
    kind: "track",
    title: `Track ${id}`,
    year: 0,
    enrichmentStatus: "unmatched",
    enrichmentReason,
    artistName: "James Horner",
    albumTitle: "Braveheart",
    albumId: "al-bh",
    path: `/media/music/James Horner/Braveheart/${id}.flac`,
    ...over,
  });

  it("collapses one album's album-scoped rows into a single Album row that counts them", () => {
    const rows = build({
      enrichment: [
        albumTrack("1", "album-unmatched"),
        albumTrack("2", "album-unmatched"),
        albumTrack("3", "album-unmatched"),
      ],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("album");
    expect(rows[0].key).toBe("album:al-bh");
    // The count is in the HEADING, because the collapse's only real risk is making
    // a pile of eighteen look like one problem.
    expect(rows[0].name).toBe("Braveheart · 3 tracks have no metadata match");
    expect(rows[0].breadcrumb).toEqual(["James Horner"]);
    expect(rows[0].artworkUrl).toBe("/api/v1/albums/al-bh/artwork/cover");
    expect(rows[0].path).toBe("/media/music/James Horner/Braveheart/1.flac");
  });

  it("collapses per album, never across albums", () => {
    // The measured ratio in miniature: N tracks over M albums are M rows, not one
    // and not N.
    const rows = build({
      enrichment: [
        albumTrack("1", "album-unmatched"),
        albumTrack("2", "not-in-tracklist", { albumId: "al-ok", albumTitle: "OK Computer" }),
        albumTrack("3", "album-unmatched"),
        albumTrack("4", "not-in-tracklist", { albumId: "al-ok", albumTitle: "OK Computer" }),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.key)).toEqual(["album:al-bh", "album:al-ok"]);
    expect(rows[0].name).toBe("Braveheart · 2 tracks have no metadata match");
    expect(rows[1].name).toBe("OK Computer · 2 tracks have no metadata match");
  });

  it("does NOT collapse a reason the per-track picker can actually fix", () => {
    // The central distinction. These three rows each offer a recording search that
    // works; folding them into an album row would replace a working action with one
    // that cannot reach their problem.
    const rows = build({
      enrichment: [
        albumTrack("a", "search-no-match"),
        albumTrack("b", "search-rejected"),
        albumTrack("c", "tag-id-unresolved"),
      ],
    });
    expect(rows).toHaveLength(3);
    expect(rows.map((r) => r.kind)).toEqual(["track", "track", "track"]);
    expect(rows.map((r) => r.route)).toEqual([
      "enrichment-override",
      "enrichment-override",
      "enrichment-override",
    ]);
    // Each still searches for its own RECORDING, narrowed by artist and album.
    expect(rows.map((r) => r.searchSeed)).toEqual(["Track a", "Track b", "Track c"]);
    expect(rows[0].albumScope).toBe("Braveheart");
    expect(rows[0].titleId).toBe("a");
  });

  it("does NOT collapse a blank reason — an un-re-passed library behaves exactly as before", () => {
    // Every row in a library not re-passed since the reason column shipped carries
    // "". A collapse driven by a diagnosis nobody made would rewrite that library's
    // whole queue on an assumption.
    const blank = build({ enrichment: [albumTrack("1", ""), albumTrack("2", "")] });
    expect(blank).toHaveLength(2);
    expect(blank.map((r) => r.kind)).toEqual(["track", "track"]);
    expect(blank[0].problemText).toMatch(/no record for this name/i);
    // And the same for a value this build has never heard of.
    const future = build({ enrichment: [albumTrack("1", "some-future-reason")] });
    expect(future).toHaveLength(1);
    expect(future[0].kind).toBe("track");
  });

  it("folds only the album-scoped rows out of a mixed album", () => {
    const rows = build({
      enrichment: [
        albumTrack("1", "album-unmatched"),
        albumTrack("2", "search-no-match"),
        albumTrack("3", "album-unmatched"),
      ],
    });
    expect(rows).toHaveLength(2);
    expect(rows[0].kind).toBe("album");
    expect(rows[0].name).toBe("Braveheart · 2 tracks have no metadata match");
    expect(rows[1].kind).toBe("track");
    expect(rows[1].titleId).toBe("2");
  });

  it("leads with the album-unmatched sentence when it holds both reasons, and still counts both", () => {
    const [row] = build({
      enrichment: [
        albumTrack("1", "not-in-tracklist"),
        albumTrack("2", "album-unmatched"),
        albumTrack("3", "album-unmatched"),
      ],
    });
    // album-unmatched outranks not-in-tracklist: no record at all is further from
    // fixed than a record of the wrong edition.
    expect(row.problemText).toMatch(/^The album Braveheart has no metadata match/);
    expect(row.problemText).toContain("2 tracks are waiting on that match");
    expect(row.problemText).toContain("1 track is missing from the track list");
    expect(row.name).toBe("Braveheart · 3 tracks have no metadata match");
  });

  it("blames the wrong edition when that is all it holds", () => {
    const [row] = build({
      enrichment: [albumTrack("1", "not-in-tracklist"), albumTrack("2", "not-in-tracklist")],
    });
    expect(row.problemText).toMatch(/wrong edition/i);
    expect(row.problemText).toContain("2 tracks are not on that release’s track list");
    expect(row.problemText).toMatch(/fix the album’s match/i);
  });

  it("offers the ALBUM search the sentence has always named: album kind, album seed, artist narrowing", () => {
    // The gesture the row names is now the gesture the row offers. A track row
    // searches recordings and carries the album as a narrowing term; this searches
    // albums, so the album is the SUBJECT and there is no release axis to narrow by.
    const [row] = build({ enrichment: [albumTrack("1", "album-unmatched")] });
    expect(row.route).toBe("album-enrichment-override");
    expect(row.albumId).toBe("al-bh");
    expect(row.searchSeed).toBe("Braveheart");
    expect(row.artistScope).toBe("James Horner");
    expect(row.albumScope).toBeUndefined();
    // It applies to the ALBUM, not to any one Title.
    expect(row.titleId).toBe("");
    expect(row.detailPath).toBe("/music/albums/al-bh");
    expect(row.canDismiss).toBe(false);
  });

  it("discloses the track rows it collapsed, each still applying to its own track", () => {
    // The cascade declines a track the picked release cannot place (ADR-0050), so
    // the collapsed rows must stay reachable without a recheck pass.
    const [row] = build({
      enrichment: [albumTrack("1", "album-unmatched"), albumTrack("2", "not-in-tracklist")],
    });
    const children = row.children ?? [];
    expect(children.map((c) => c.key)).toEqual(["enrichment:1", "enrichment:2"]);
    expect(children.map((c) => c.kind)).toEqual(["track", "track"]);
    expect(children.map((c) => c.route)).toEqual([
      "enrichment-override",
      "enrichment-override",
    ]);
    expect(children.map((c) => c.titleId)).toEqual(["1", "2"]);
    expect(children[0].searchSeed).toBe("Track 1");
    // And each keeps the per-track sentence, which is a different claim from the
    // album row's: it is about THIS track's place on the release.
    expect(children[1].problemText).toMatch(/not on that release’s track list/i);
  });

  it("keeps its place in the queue's most-stuck ordering rather than floating to one end", () => {
    const rows = build({
      unmatched: [{ id: "u", path: "/m/1080p.mkv", folderPath: "/m", reason: "" }],
      enrichment: [
        { ...emptyContext, id: "mA", kind: "movie", title: "A", year: 0, enrichmentStatus: "unmatched" },
        albumTrack("1", "album-unmatched"),
        { ...emptyContext, id: "mB", kind: "movie", title: "B", year: 0, enrichmentStatus: "unmatched" },
      ],
    });
    expect(rows.map((r) => r.key)).toEqual([
      "unmatched:u",
      "enrichment:mA",
      "album:al-bh",
      "enrichment:mB",
    ]);
  });

  it("leaves a track whose album the server could not name as its own row", () => {
    // There is nothing to collapse INTO: the row's whole action is a search-and-pin
    // against an album entity, and without an id there is no entity to pin.
    const rows = build({ enrichment: [albumTrack("1", "album-unmatched", { albumId: "" })] });
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("track");
    expect(rows[0].route).toBe("enrichment-override");
  });

  it("leaves a FAILED row alone, whatever reason it still carries", () => {
    // A provider refusing requests is not a diagnosis about the album (ADR-0048),
    // and hiding it behind an album pick would promise a fix that cannot work.
    const rows = build({
      enrichment: [albumTrack("1", "album-unmatched", { enrichmentStatus: "failed" })],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("track");
    expect(rows[0].problemText).toMatch(/failed/i);
  });

  it("carries exactly the no-metadata class, so the chips keep agreeing with the queue", () => {
    // The chips count rows and filter rows. An Album row that claimed a class it
    // does not hold — or held one it does not claim — would make the "No metadata"
    // chip disagree with the list it filters, which is the one thing the collapse
    // must not do.
    const rows = build({
      enrichment: [
        albumTrack("1", "album-unmatched"),
        albumTrack("2", "album-unmatched"),
        albumTrack("3", "search-no-match"),
      ],
    });
    expect(rows.filter((r) => hasProblem(r, "no-metadata"))).toHaveLength(rows.length);
    expect(rows).toHaveLength(2);
    expect(rows[0].problems).toEqual(["no-metadata"]);
  });

  it("says one track in the singular", () => {
    const [row] = build({ enrichment: [albumTrack("1", "album-unmatched")] });
    expect(row.name).toBe("Braveheart · 1 track has no metadata match");
    expect(row.problemText).toContain("1 track is waiting on it");
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
