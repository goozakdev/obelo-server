import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import type {
  EnrichmentAttentionTitle,
  Library,
  MatchOverride,
  NeedsReviewItem,
  UnmatchedFile,
} from "../api/types";

// AdminNeedsFixingScreen through the faked API client (the one seam). This replaces
// the four-panel Attention screen: the four server lists fold into one queue whose
// rows all behave the same way, and the raw-id forms are gone in favour of a
// provider search that runs itself.
//
// The assertions that matter here are the ones the old screen could not make:
//   - a row names its item well enough to act on (breadcrumb + file, not a bare name),
//   - opening a row SEARCHES, so a best guess is on screen without typing,
//   - one click applies it, through the route that matches the problem — an identity
//     correction for a mis-filed item, a metadata-only pin for a mis-decorated one,
//   - identity corrections don't fire a scan per row; they queue ONE rescan.

const {
  listLibraries,
  listUnmatched,
  listOverrides,
  listEnrichmentAttention,
  listNeedsReview,
  reviewTitle,
  reviewShow,
  fixMatch,
  applyEnrichmentOverride,
  searchEnrichmentCandidates,
  searchLibraryEnrichmentCandidates,
  previewExternalCandidate,
  previewLibraryExternalCandidate,
  listShowProblems,
  reviewShowEpisodes,
  deleteOverride,
  scanLibrary,
} = vi.hoisted(() => ({
  listLibraries: vi.fn(),
  listUnmatched: vi.fn(),
  listOverrides: vi.fn(),
  listEnrichmentAttention: vi.fn(),
  listNeedsReview: vi.fn(),
  reviewTitle: vi.fn(),
  reviewShow: vi.fn(),
  fixMatch: vi.fn(),
  applyEnrichmentOverride: vi.fn(),
  searchEnrichmentCandidates: vi.fn(),
  searchLibraryEnrichmentCandidates: vi.fn(),
  previewExternalCandidate: vi.fn(),
  previewLibraryExternalCandidate: vi.fn(),
  listShowProblems: vi.fn(),
  reviewShowEpisodes: vi.fn(),
  deleteOverride: vi.fn(),
  scanLibrary: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      listUnmatched: (...a: unknown[]) => listUnmatched(...a),
      listOverrides: (...a: unknown[]) => listOverrides(...a),
      listEnrichmentAttention: (...a: unknown[]) => listEnrichmentAttention(...a),
      listNeedsReview: (...a: unknown[]) => listNeedsReview(...a),
      reviewTitle: (...a: unknown[]) => reviewTitle(...a),
      reviewShow: (...a: unknown[]) => reviewShow(...a),
      fixMatch: (...a: unknown[]) => fixMatch(...a),
      applyEnrichmentOverride: (...a: unknown[]) => applyEnrichmentOverride(...a),
      searchEnrichmentCandidates: (...a: unknown[]) => searchEnrichmentCandidates(...a),
      searchLibraryEnrichmentCandidates: (...a: unknown[]) =>
        searchLibraryEnrichmentCandidates(...a),
      previewExternalCandidate: (...a: unknown[]) => previewExternalCandidate(...a),
      previewLibraryExternalCandidate: (...a: unknown[]) =>
        previewLibraryExternalCandidate(...a),
      listShowProblems: (...a: unknown[]) => listShowProblems(...a),
      reviewShowEpisodes: (...a: unknown[]) => reviewShowEpisodes(...a),
      deleteOverride: (...a: unknown[]) => deleteOverride(...a),
      scanLibrary: (...a: unknown[]) => scanLibrary(...a),
    },
  };
});

import AdminNeedsFixingScreen from "./AdminNeedsFixingScreen";

const context = {
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

function lib(over: Partial<Library> = {}): Library {
  return {
    id: "lib1",
    name: "Movies",
    kind: "movie",
    rootFolders: [{ id: "r1", path: "/media/movies" }],
    ...over,
  };
}
function reviewItem(over: Partial<NeedsReviewItem> = {}): NeedsReviewItem {
  return {
    ...context,
    id: "t1",
    kind: "movie",
    title: "Yearless Movie",
    year: 0,
    folderPath: "/media/movies/Yearless Movie",
    reason: "no-year",
    // Flagged for its parse, not for a file collision — the default shape of this
    // list. A test that wants a collision passes `ambiguous` + `collidingPaths`.
    needsReview: true,
    ambiguous: false,
    collidingPaths: [],
    enrichmentStatus: "matched",
    path: "/media/movies/Yearless Movie/Yearless Movie.mp4",
    ...over,
  };
}
function enrichmentItem(over: Partial<EnrichmentAttentionTitle> = {}): EnrichmentAttentionTitle {
  return {
    ...context,
    id: "e1",
    kind: "episode",
    title: "Pilot",
    year: 0,
    enrichmentStatus: "unmatched",
    showTitle: "The Wire",
    seasonNumber: 1,
    episodeNumber: 3,
    path: "/media/tv/The Wire/S01/the.wire.103.mkv",
    ...over,
  };
}
/** A metadata-attention row whose fix IS a provider record — a Movie. An Episode's
 * is not: its Show is already identified and its problem is an arrangement, so its
 * row offers the matcher rather than a search (file-matcher/07). */
function movieEnrichmentItem(over: Partial<EnrichmentAttentionTitle> = {}): EnrichmentAttentionTitle {
  return enrichmentItem({
    kind: "movie",
    title: "Arrival",
    showTitle: "",
    showId: "",
    seasonNumber: 0,
    episodeNumber: 0,
    path: "/media/movies/arrival.mkv",
    ...over,
  });
}
function unmatchedFile(over: Partial<UnmatchedFile> = {}): UnmatchedFile {
  return {
    id: "f1",
    path: "/media/movies/1080p.mkv",
    folderPath: "/media/movies",
    reason: "no identity",
    ...over,
  };
}
function override(over: Partial<MatchOverride> = {}): MatchOverride {
  return {
    id: "o1",
    folderPath: "/media/movies/Dune (2021)",
    title: "Dune",
    year: 2021,
    identityKey: "k1",
    orphaned: false,
    ...over,
  };
}
function candidate(over: Record<string, unknown> = {}) {
  return {
    externalId: "438631",
    title: "Dune",
    year: 2021,
    kind: "movie",
    disambiguation: "A noble family becomes embroiled…",
    ...over,
  };
}

beforeEach(() => {
  for (const fn of [
    listLibraries,
    listUnmatched,
    listOverrides,
    listEnrichmentAttention,
    listNeedsReview,
    reviewTitle,
    reviewShow,
    fixMatch,
    applyEnrichmentOverride,
    searchEnrichmentCandidates,
    searchLibraryEnrichmentCandidates,
    previewExternalCandidate,
    previewLibraryExternalCandidate,
    listShowProblems,
    reviewShowEpisodes,
    deleteOverride,
    scanLibrary,
  ]) {
    fn.mockReset();
  }
  listLibraries.mockResolvedValue([lib()]);
  listUnmatched.mockResolvedValue([]);
  listOverrides.mockResolvedValue([]);
  listEnrichmentAttention.mockResolvedValue([]);
  listNeedsReview.mockResolvedValue([]);
  listShowProblems.mockResolvedValue([]);
  reviewShowEpisodes.mockResolvedValue(undefined);
  reviewTitle.mockResolvedValue(undefined);
  reviewShow.mockResolvedValue(undefined);
  fixMatch.mockResolvedValue(override());
  applyEnrichmentOverride.mockResolvedValue({});
  deleteOverride.mockResolvedValue(undefined);
  scanLibrary.mockResolvedValue({ state: "idle" });
  searchEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
  searchLibraryEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
});

function render() {
  renderWithAuth(<AdminNeedsFixingScreen />, { initialEntries: ["/admin/needs-fixing"] });
}

describe("AdminNeedsFixingScreen — the queue", () => {
  it("shows one queue containing every kind of problem, not four lists", async () => {
    listUnmatched.mockResolvedValue([unmatchedFile()]);
    listNeedsReview.mockResolvedValue([reviewItem()]);
    listEnrichmentAttention.mockResolvedValue([enrichmentItem()]);
    listOverrides.mockResolvedValue([override({ id: "o2", orphaned: true })]);
    render();

    await waitFor(() => expect(screen.getByTestId("needs-fixing-list")).toBeInTheDocument());
    expect(screen.getAllByTestId("fix-item")).toHaveLength(4);
    expect(screen.getByTestId("needs-fixing-count")).toHaveTextContent("4");
  });

  it("names an episode by its show, season and file — the thing the old screen omitted", async () => {
    listEnrichmentAttention.mockResolvedValue([enrichmentItem()]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).getByTestId("fix-item-kind")).toHaveTextContent("Episode");
    expect(within(row).getByTestId("fix-item-breadcrumb")).toHaveTextContent("The Wire › Season 1");
    expect(within(row).getByTestId("fix-item-name")).toHaveTextContent("S01E03 Pilot");
    expect(within(row).getByTestId("fix-item-path")).toHaveTextContent(
      "/media/tv/The Wire/S01/the.wire.103.mkv",
    );
  });

  it("shows an ambiguous item, names the files that collide, and offers no inert search", async () => {
    // The convention promises a collision is "flagged ambiguous IN THE WEB APP"
    // (docs/naming-convention.md). Until now the flag reached the browser and was
    // rendered nowhere at all — so this asserts the whole chain: a row exists, it
    // is filed under the collision chip, it prints both paths, and it does not
    // pretend a provider search or a dismissal could settle it.
    listNeedsReview.mockResolvedValue([
      reviewItem({
        needsReview: false,
        ambiguous: true,
        collidingPaths: [
          "/media/movies/Dune (2021)/Dune (2021).mkv",
          "/media/movies/Dune (2021)/Dune (2021) (repack).mkv",
        ],
      }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(row).toHaveAttribute("data-problem", "ambiguous");
    expect(within(row).getByTestId("fix-item-problem")).toHaveTextContent(/only the first one plays/i);
    const collisions = within(row).getByTestId("fix-item-collisions");
    expect(collisions).toHaveTextContent("/media/movies/Dune (2021)/Dune (2021).mkv");
    expect(collisions).toHaveTextContent("/media/movies/Dune (2021)/Dune (2021) (repack).mkv");
    expect(within(row).queryByTestId("fix-item-dismiss")).toBeNull();
    expect(within(row).queryByTestId("fix-item-toggle")).toBeNull();
    expect(screen.getByTestId("needs-fixing-chip-ambiguous")).toHaveTextContent("1");
  });

  it("states what is wrong in a sentence rather than a status word", async () => {
    listEnrichmentAttention.mockResolvedValue([enrichmentItem({ enrichmentStatus: "failed" })]);
    render();
    expect(await screen.findByTestId("fix-item-problem")).toHaveTextContent(/lookup failed/i);
  });

  it("filters by problem with live counts", async () => {
    listUnmatched.mockResolvedValue([unmatchedFile()]);
    listEnrichmentAttention.mockResolvedValue([enrichmentItem()]);
    render();

    await waitFor(() => expect(screen.getAllByTestId("fix-item")).toHaveLength(2));
    expect(screen.getByTestId("needs-fixing-chip-unidentified")).toHaveTextContent("1");

    await userEvent.click(screen.getByTestId("needs-fixing-chip-unidentified"));
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveAttribute("data-problem", "unidentified");
  });

  it("says nothing needs fixing when the library is clean", async () => {
    render();
    expect(await screen.findByTestId("needs-fixing-empty")).toHaveTextContent(/nothing needs fixing/i);
  });
});

describe("AdminNeedsFixingScreen — fixing without typing an id", () => {
  it("searches on open and offers the top hit as the best guess", async () => {
    listEnrichmentAttention.mockResolvedValue([movieEnrichmentItem()]);
    searchEnrichmentCandidates.mockResolvedValue({
      candidates: [candidate({ title: "Arrival", year: 2016 })],
      hasMore: false,
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenCalledWith("e1", "Arrival", { page: 0 }),
    );
    expect(await screen.findByTestId("fix-best-guess")).toBeInTheDocument();
    expect(screen.getByTestId("fix-candidate-title")).toHaveTextContent("Arrival (2016)");
  });

  it("applies a metadata correction with one click, never touching identity", async () => {
    // A Movie: the picked record IS the answer, so one click finishes it. (An
    // Episode needs a second step — see the episode-picking tests below.)
    listEnrichmentAttention.mockResolvedValue([
      enrichmentItem({ id: "e1", kind: "movie", title: "Arrival", showTitle: "", showId: "" }),
    ]);
    searchEnrichmentCandidates.mockResolvedValue({
      candidates: [candidate({ externalId: "1438" })],
      hasMore: false,
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    await userEvent.click(await screen.findByTestId("fix-use-best-guess"));

    await waitFor(() => expect(applyEnrichmentOverride).toHaveBeenCalledWith("e1", "1438"));
    // A metadata pin is not an identity change, so nothing is re-filed and no scan
    // is offered (ADR-0014).
    expect(fixMatch).not.toHaveBeenCalled();
    expect(screen.queryByTestId("needs-fixing-rescan")).not.toBeInTheDocument();
  });

  it("applies an identity correction as a fix-match, dismisses the flag, and queues ONE rescan", async () => {
    listNeedsReview.mockResolvedValue([reviewItem()]);
    searchLibraryEnrichmentCandidates.mockResolvedValue({
      candidates: [candidate()],
      hasMore: false,
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    await userEvent.click(await screen.findByTestId("fix-use-best-guess"));

    await waitFor(() =>
      expect(fixMatch).toHaveBeenCalledWith("lib1", {
        folderPath: "/media/movies/Yearless Movie",
        title: "Dune",
        year: 2021,
        tmdbId: "438631",
      }),
    );
    // The Admin just resolved the very uncertainty the flag was raised about.
    expect(reviewTitle).toHaveBeenCalledWith("t1");
    // A Match override is read by the SCANNER, so the correction lands on the next
    // scan — offered once, not fired per row.
    expect(await screen.findByTestId("needs-fixing-rescan")).toBeInTheDocument();
    expect(scanLibrary).not.toHaveBeenCalled();

    await userEvent.click(screen.getByTestId("needs-fixing-rescan-button"));
    await waitFor(() => expect(scanLibrary).toHaveBeenCalledWith("lib1"));
  });

  it("searches the LIBRARY for an unmatched file, which has no Title to search through", async () => {
    listUnmatched.mockResolvedValue([
      unmatchedFile({ path: "/media/movies/Arrival.2016.1080p.BluRay.mkv" }),
    ]);
    searchLibraryEnrichmentCandidates.mockResolvedValue({
      candidates: [candidate({ title: "Arrival", year: 2016 })],
      hasMore: false,
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    await waitFor(() =>
      expect(searchLibraryEnrichmentCandidates).toHaveBeenCalledWith("lib1", "Arrival 2016", {
        page: 0,
      }),
    );
  });

  it("lets the Admin search again when the guess is wrong", async () => {
    listEnrichmentAttention.mockResolvedValue([movieEnrichmentItem()]);
    searchEnrichmentCandidates.mockResolvedValue({ candidates: [candidate()], hasMore: false });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    const input = await screen.findByTestId("fix-picker-input");
    await userEvent.clear(input);
    await userEvent.type(input, "The Corner");
    await userEvent.click(screen.getByTestId("fix-picker-search-button"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenCalledWith("e1", "The Corner", { page: 0 }),
    );
  });

  it("routes a pasted provider id to the by-id preview instead of a search", async () => {
    listEnrichmentAttention.mockResolvedValue([movieEnrichmentItem()]);
    searchEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
    previewExternalCandidate.mockResolvedValue(candidate({ externalId: "1438" }));
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    const input = await screen.findByTestId("fix-picker-input");
    await userEvent.clear(input);
    await userEvent.type(input, "1438");
    await userEvent.click(screen.getByTestId("fix-picker-search-button"));

    await waitFor(() => expect(previewExternalCandidate).toHaveBeenCalledWith("e1", "1438"));
  });
});

describe("AdminNeedsFixingScreen — one row per Show", () => {
  // The Batman case, on screen. Five files the provider counts in a re-numbered
  // continuation series used to be five rows, each with a "Use this" that offered
  // to fix the SERIES — the one part that was already right. One problem, five
  // rows, no working fix. They are one row now, and its action is the matcher.

  const brokenEpisode = (id: string) =>
    enrichmentItem({
      id,
      title: `Ep ${id}`,
      showId: "sh1",
      showTitle: "Batman: The Animated Series",
      seasonNumber: 3,
      path: `/media/tv/Batman/Season 03/batman.${id}.mkv`,
    });

  it("shows one row for five broken episodes, naming the problem and its count", async () => {
    listEnrichmentAttention.mockResolvedValue(
      ["a", "b", "c", "d", "e"].map(brokenEpisode),
    );
    render();

    await screen.findByTestId("needs-fixing-list");
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByTestId("fix-item-name")).toHaveTextContent(
      "Batman: The Animated Series",
    );
    expect(within(rows[0]).getByTestId("fix-item-problem")).toHaveTextContent(
      "5 episodes have no metadata match",
    );
    // And it still answers "which file?" — a count with no file is not actionable.
    expect(within(rows[0]).getByTestId("fix-item-path")).toHaveTextContent("batman.a.mkv");
  });

  it("offers the matcher, and no fix that cannot work", async () => {
    listEnrichmentAttention.mockResolvedValue([brokenEpisode("a")]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).getByTestId("fix-item-sort")).toHaveAttribute(
      "href",
      "/admin/shows/sh1/matcher",
    );
    // No provider search: nothing here is fixed by naming a work.
    expect(within(row).queryByTestId("fix-item-toggle")).not.toBeInTheDocument();
  });

  it("keeps a Show queued for a file the Admin merely left unassigned", async () => {
    // Undecided is not settled (CONTEXT.md "Unassigned"). This is the count no
    // client-side list can see: an unassigned file is neither a Title nor an
    // Unmatched row.
    listShowProblems.mockResolvedValue([
      {
        showId: "sh1",
        title: "Batman: The Animated Series",
        year: 1992,
        path: "/media/tv/Batman/Season 03/loose.mkv",
        unassigned: 3,
        unidentified: 0,
        unmatchedPaths: [],
        unreadablePaths: [],
        orphaned: 0,
        orphanedPath: "",
      },
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).getByTestId("fix-item-problem")).toHaveTextContent(
      "3 files aren’t assigned to an episode",
    );
    expect(within(row).getByTestId("fix-item-sort")).toBeInTheDocument();
  });

  it("counts the collapsed rows in the chips and the total, not the symptoms", async () => {
    listEnrichmentAttention.mockResolvedValue(
      ["a", "b", "c", "d", "e"].map(brokenEpisode),
    );
    render();

    await screen.findByTestId("needs-fixing-list");
    // One problem, so one. The old queue said five.
    expect(screen.getByTestId("needs-fixing-count")).toHaveTextContent("1");
    expect(screen.getByTestId("needs-fixing-chip-no-metadata")).toHaveTextContent("1");
  });

  it("counts the library selector's badge the same collapsed way", async () => {
    // The badge and the queue must agree. A badge that summed the raw lists would
    // say "5 to fix" over a queue showing one row, and leave the Admin to work out
    // which of the two numbers was lying.
    listEnrichmentAttention.mockResolvedValue(
      ["a", "b", "c", "d", "e"].map(brokenEpisode),
    );
    render();

    await screen.findByTestId("needs-fixing-list");
    await waitFor(() =>
      expect(screen.getByTestId("needs-fixing-library-select")).toHaveTextContent(
        "Movies — 1 to fix",
      ),
    );
  });

  it("lists an orphaned Placement as its own row", async () => {
    listShowProblems.mockResolvedValue([
      {
        showId: "sh1",
        title: "Batman: The Animated Series",
        year: 0,
        path: "/media/tv/Batman/Season 03/loose.mkv",
        unassigned: 1,
        unidentified: 0,
        unmatchedPaths: [],
        unreadablePaths: [],
        orphaned: 1,
        orphanedPath: "/media/tv/Batman/Season 03/gone.mkv",
      },
    ]);
    render();

    await screen.findByTestId("needs-fixing-list");
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(2);
    const orphan = rows.find(
      (r) => r.getAttribute("data-problem") === "orphaned-correction",
    );
    expect(orphan).toBeDefined();
    expect(within(orphan!).getByTestId("fix-item-path")).toHaveTextContent("gone.mkv");
  });

  it("dismisses every flagged Episode of the Show in one call", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({
        id: "ep1",
        kind: "episode",
        title: "Ep",
        folderPath: "",
        reason: "episode-numbering",
        showId: "sh1",
        showTitle: "Batman: The Animated Series",
      }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    await userEvent.click(within(row).getByTestId("fix-item-dismiss"));

    // One call, for the whole set the row stands for — N calls could half-succeed
    // and leave a count the Admin cannot explain.
    await waitFor(() => expect(reviewShowEpisodes).toHaveBeenCalledWith("sh1"));
    expect(reviewTitle).not.toHaveBeenCalled();
  });

  it("does not list a file both inside a Show row and as a row of its own", async () => {
    listUnmatched.mockResolvedValue([
      unmatchedFile({ id: "u1", path: "/media/tv/Batman/Season 03/stray.mkv" }),
    ]);
    listShowProblems.mockResolvedValue([
      {
        showId: "sh1",
        title: "Batman: The Animated Series",
        year: 0,
        path: "/media/tv/Batman/Season 03/stray.mkv",
        unassigned: 0,
        unidentified: 1,
        unmatchedPaths: ["/media/tv/Batman/Season 03/stray.mkv"],
        unreadablePaths: [],
        orphaned: 0,
        orphanedPath: "",
      },
    ]);
    render();

    await screen.findByTestId("needs-fixing-list");
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(1);
    expect(rows[0].getAttribute("data-kind")).toBe("show");
  });
});

describe("AdminNeedsFixingScreen — confirming a filing", () => {
  it("shows the matched record and a poster, so 'Looks right' is an informed click", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({ title: "Arrival", releaseDate: "2016-11-11" }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    // The year the parse never had — without it the Admin is confirming a bare name.
    expect(within(row).getByTestId("fix-item-matched")).toHaveTextContent("Arrival (2016)");
    const img = within(row).getByTestId("fix-item-art").querySelector("img");
    expect(img).toHaveAttribute("src", expect.stringContaining("/titles/t1/artwork/poster"));
  });

  it("does not claim 'unmatched' for a matched item whose record adds nothing", async () => {
    // The regression this guards: suppressing a redundant "Matched to" line must
    // not make a correctly-matched item announce it has nothing to check against.
    listNeedsReview.mockResolvedValue([
      reviewItem({
        id: "sh1",
        kind: "show",
        title: "The Wire",
        folderPath: "/tv/The Wire",
        enrichmentStatus: "matched",
        releaseDate: "",
      }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).queryByTestId("fix-item-matched")).not.toBeInTheDocument();
    // The poster is what confirms it instead.
    const img = within(row).getByTestId("fix-item-art").querySelector("img");
    expect(img).toHaveAttribute("src", expect.stringContaining("/shows/sh1/artwork/poster"));
  });

  it("says so plainly when there is no matched record to check against", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({ enrichmentStatus: "unmatched", releaseDate: "" }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).getByTestId("fix-item-matched")).toHaveTextContent(
      /nothing to check this against/i,
    );
  });

  it("shows an episode its SHOW's poster, since an episode has none of its own", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({
        id: "ep1",
        kind: "episode",
        title: "The Target",
        folderPath: "",
        reason: "episode-numbering",
        showId: "sh1",
        showTitle: "The Wire",
      }),
    ]);
    render();

    const img = (await screen.findByTestId("fix-item-art")).querySelector("img");
    expect(img).toHaveAttribute("src", expect.stringContaining("/shows/sh1/artwork/poster"));
  });
});

describe("AdminNeedsFixingScreen — the non-search resolutions", () => {
  it("dismisses a needs-review flag the Admin judges correct", async () => {
    listNeedsReview.mockResolvedValue([reviewItem()]);
    render();
    await userEvent.click(await screen.findByTestId("fix-item-dismiss"));
    await waitFor(() => expect(reviewTitle).toHaveBeenCalledWith("t1"));
  });

  it("dismisses a Show through the show route, not the title route", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({ id: "s1", kind: "show", title: "Unyeared Show", folderPath: "/media/tv/Show" }),
    ]);
    render();
    await userEvent.click(await screen.findByTestId("fix-item-dismiss"));
    await waitFor(() => expect(reviewShow).toHaveBeenCalledWith("s1"));
    expect(reviewTitle).not.toHaveBeenCalled();
  });

  it("offers an orphaned correction a discard, since no search can repair it", async () => {
    listOverrides.mockResolvedValue([override({ orphaned: true })]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(row).toHaveAttribute("data-problem", "orphaned-correction");
    expect(within(row).queryByTestId("fix-item-toggle")).not.toBeInTheDocument();

    await userEvent.click(within(row).getByTestId("fix-item-discard"));
    await waitFor(() => expect(deleteOverride).toHaveBeenCalledWith("lib1", "o1"));
  });

  it("gives an Episode the matcher and a dismissal, never a search it cannot use", async () => {
    // An Episode has no folder to anchor an identity override to, and re-picking its
    // series was never the fix — the series was right and the arrangement was wrong.
    // So the row offers no provider search at all; its action is the matcher.
    listNeedsReview.mockResolvedValue([
      reviewItem({
        id: "ep1",
        kind: "episode",
        title: "Ep",
        folderPath: "",
        reason: "episode-numbering",
        showId: "sh1",
        showTitle: "The Wire",
      }),
    ]);
    render();

    const row = await screen.findByTestId("fix-item");
    expect(within(row).queryByTestId("fix-item-toggle")).not.toBeInTheDocument();
    expect(within(row).getByTestId("fix-item-sort")).toHaveAttribute(
      "href",
      "/admin/shows/sh1/matcher",
    );
    expect(within(row).getByTestId("fix-item-dismiss")).toBeInTheDocument();
  });
});

describe("AdminNeedsFixingScreen — corrections already made", () => {
  it("keeps settled corrections out of the queue but reachable", async () => {
    listOverrides.mockResolvedValue([override()]);
    render();

    await waitFor(() =>
      expect(screen.getByTestId("needs-fixing-empty")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("fix-item")).not.toBeInTheDocument();

    await userEvent.click(screen.getByTestId("needs-fixing-corrections-toggle"));
    expect(await screen.findByTestId("override-item")).toHaveTextContent("Dune");
  });
});
