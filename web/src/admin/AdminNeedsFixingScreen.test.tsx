import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import type { EnrichProgress } from "../events/enrichEvents";
import type {
  EnrichmentAttentionTitle,
  EnrichPassState,
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
  searchEntityEnrichmentCandidates,
  previewExternalCandidate,
  previewLibraryExternalCandidate,
  previewEntityExternalCandidate,
  applyEntityEnrichmentOverride,
  listAlbumEditions,
  listShowProblems,
  reviewShowEpisodes,
  deleteOverride,
  scanLibrary,
  enrichLibrary,
  getEnrichPassState,
  subscribeEvents,
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
  searchEntityEnrichmentCandidates: vi.fn(),
  previewExternalCandidate: vi.fn(),
  previewLibraryExternalCandidate: vi.fn(),
  previewEntityExternalCandidate: vi.fn(),
  applyEntityEnrichmentOverride: vi.fn(),
  listAlbumEditions: vi.fn(),
  listShowProblems: vi.fn(),
  reviewShowEpisodes: vi.fn(),
  deleteOverride: vi.fn(),
  scanLibrary: vi.fn(),
  enrichLibrary: vi.fn(),
  getEnrichPassState: vi.fn(),
  subscribeEvents: vi.fn(),
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
      searchEntityEnrichmentCandidates: (...a: unknown[]) =>
        searchEntityEnrichmentCandidates(...a),
      previewExternalCandidate: (...a: unknown[]) => previewExternalCandidate(...a),
      previewLibraryExternalCandidate: (...a: unknown[]) =>
        previewLibraryExternalCandidate(...a),
      previewEntityExternalCandidate: (...a: unknown[]) =>
        previewEntityExternalCandidate(...a),
      applyEntityEnrichmentOverride: (...a: unknown[]) =>
        applyEntityEnrichmentOverride(...a),
      listAlbumEditions: (...a: unknown[]) => listAlbumEditions(...a),
      listShowProblems: (...a: unknown[]) => listShowProblems(...a),
      reviewShowEpisodes: (...a: unknown[]) => reviewShowEpisodes(...a),
      deleteOverride: (...a: unknown[]) => deleteOverride(...a),
      scanLibrary: (...a: unknown[]) => scanLibrary(...a),
      enrichLibrary: (...a: unknown[]) => enrichLibrary(...a),
      getEnrichPassState: (...a: unknown[]) => getEnrichPassState(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
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
    // No diagnosis by default — the state every row in an un-re-passed library is
    // in, and the one that renders the sentence this screen has always rendered.
    enrichmentReason: "",
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
/** The summary POST /libraries/{id}/enrich returns, normalized (counts never
 * undefined). The Re-check button reports every one of these numbers. */
/** The started-ack a POST resolves with now: queued, not finished. */
function passState(over: Partial<EnrichPassState> = {}): EnrichPassState {
  return { libraryId: "lib1", running: false, started: false, ...over };
}
/** One enrichProgress SSE payload, as the server publishes it. */
function progressEvent(over: Partial<EnrichProgress> = {}): EnrichProgress {
  return {
    libraryId: "lib1",
    total: 0,
    done: 0,
    matched: 0,
    unmatched: 0,
    failed: 0,
    disabled: 0,
    retrying: 0,
    complete: false,
    ...over,
  };
}
/** The SSE callback the screen registered, rebound in beforeEach. */
let emitEvent: (type: string, data: unknown) => void = () => {};
/** Push one server event at the screen, inside act so React flushes it. */
function emit(type: string, data: unknown) {
  act(() => emitEvent(type, data));
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
    searchEntityEnrichmentCandidates,
    previewExternalCandidate,
    previewLibraryExternalCandidate,
    previewEntityExternalCandidate,
    applyEntityEnrichmentOverride,
    listShowProblems,
    reviewShowEpisodes,
    deleteOverride,
    scanLibrary,
    enrichLibrary,
    getEnrichPassState,
    subscribeEvents,
    listAlbumEditions,
  ]) {
    fn.mockReset();
  }
  // The screen subscribes to the enrichProgress SSE stream, which is where a pass's
  // progress and its terminal summary now come from (ADR-0051's amendment: the POST
  // no longer waits for either). Capture the callback so a test can push events as
  // the server would.
  emitEvent = () => {};
  subscribeEvents.mockImplementation((onEvent: (type: string, data: unknown) => void) => {
    emitEvent = onEvent;
    return () => {};
  });
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
  enrichLibrary.mockResolvedValue(passState({ running: true, started: true }));
  getEnrichPassState.mockResolvedValue(passState());
  searchEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
  searchLibraryEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
  searchEntityEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
  applyEntityEnrichmentOverride.mockResolvedValue({ entityType: "albums", entityId: "al1" });
  // An unmatched album has no release-group and therefore no editions (ADR-0052) —
  // which is the state every row in this suite is in unless it says otherwise.
  listAlbumEditions.mockResolvedValue({ albumId: "al-bh", localTrackCount: 0, editions: [] });
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

  it("sends an unmatched track to its ALBUM, by name, rather than to a recording search", async () => {
    // ADR-0050 on the screen itself: the row for a track whose album never matched
    // names the album as the thing to go and fix. It is the single largest bucket in
    // a real library (365 of 730), and it used to render the same sentence as the
    // three cases the recording picker CAN fix.
    listEnrichmentAttention.mockResolvedValue([
      enrichmentItem({
        id: "tr1",
        kind: "track",
        title: "Airbag",
        showTitle: "",
        showId: "",
        seasonNumber: 0,
        episodeNumber: 0,
        artistName: "Radiohead",
        albumTitle: "OK Computer",
        albumId: "al1",
        path: "/media/music/Radiohead/OK Computer/01 Airbag.flac",
        enrichmentStatus: "unmatched",
        enrichmentReason: "album-unmatched",
      }),
    ]);
    render();
    const problem = await screen.findByTestId("fix-item-problem");
    expect(problem).toHaveTextContent(/OK Computer/);
    expect(problem).toHaveTextContent(/fix the album/i);
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

describe("AdminNeedsFixingScreen — a Track row searches for the TRACK", () => {
  // The defect the operator hit (ADR-0050, needs-fixing/06). A Track row routes to
  // searchEnrichmentCandidates, whose searched kind is the Title's own — `track`, so
  // MusicBrainz /ws/2/recording, whose subject is the RECORDING. Seeding it with the
  // album made opening the row for "(I Could Only) Whisper Your Name" search for
  // every recording ever called "She". The album is a narrowing term there, not the
  // search term, and the row now carries it as one.

  const musicLib = () => lib({ id: "lib1", name: "Music", kind: "music" });

  const trackItem = (over: Partial<EnrichmentAttentionTitle> = {}) =>
    enrichmentItem({
      id: "tr1",
      kind: "track",
      title: "(I Could Only) Whisper Your Name",
      showTitle: "",
      showId: "",
      seasonNumber: 0,
      episodeNumber: 0,
      artistName: "Harry Connick, Jr.",
      albumTitle: "She",
      trackNumber: 5,
      path: "/media/music/Harry Connick, Jr./She/05 Whisper Your Name.flac",
      ...over,
    });

  it("seeds the search with the track title and narrows by artist and album", async () => {
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([trackItem()]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenCalledWith(
        "tr1",
        "(I Could Only) Whisper Your Name",
        { page: 0, artist: "Harry Connick, Jr.", release: "She" },
      ),
    );
    // The album is emphatically NOT the query: that is the whole bug.
    expect(searchEnrichmentCandidates).not.toHaveBeenCalledWith("tr1", "She", expect.anything());
  });

  it("pre-fills both narrowing boxes so the common case needs no typing", async () => {
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([trackItem()]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));

    expect(await screen.findByTestId("fix-picker-artist")).toHaveValue("Harry Connick, Jr.");
    expect(screen.getByTestId("fix-picker-album")).toHaveValue("She");
    expect(screen.getByTestId("fix-picker-input")).toHaveValue(
      "(I Could Only) Whisper Your Name",
    );
  });

  it("re-searches narrowed when the Admin edits a box", async () => {
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([trackItem()]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    const album = await screen.findByTestId("fix-picker-album");
    await userEvent.clear(album);
    await userEvent.type(album, "She (Deluxe)");
    await userEvent.click(screen.getByTestId("fix-picker-search-button"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenLastCalledWith(
        "tr1",
        "(I Could Only) Whisper Your Name",
        { page: 0, artist: "Harry Connick, Jr.", release: "She (Deluxe)" },
      ),
    );
  });

  it("widens when a box is blanked — a wrong tag must not strand the row", async () => {
    // The reason the narrowing is a BOX and not a silent AND clause: the artist tag
    // is often itself the thing that is wrong, and a search narrowed to it would
    // return nothing with no control to loosen.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([trackItem()]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    await userEvent.clear(await screen.findByTestId("fix-picker-artist"));
    await userEvent.clear(screen.getByTestId("fix-picker-album"));
    await userEvent.click(screen.getByTestId("fix-picker-search-button"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenLastCalledWith(
        "tr1",
        "(I Could Only) Whisper Your Name",
        { page: 0 },
      ),
    );
  });

  it("renders neither box on a video row, and sends neither param", async () => {
    listEnrichmentAttention.mockResolvedValue([movieEnrichmentItem()]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));

    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenCalledWith("e1", "Arrival", { page: 0 }),
    );
    expect(screen.queryByTestId("fix-picker-artist")).not.toBeInTheDocument();
    expect(screen.queryByTestId("fix-picker-album")).not.toBeInTheDocument();
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

describe("AdminNeedsFixingScreen — one row per Album", () => {
  // The Braveheart case, on screen (ADR-0050, album-resolves-its-tracks/09). 557
  // flagged tracks in the developer's library were 90 album problems; Braveheart
  // alone was 18 rows, each saying "fix the album's match" over a picker that
  // searched RECORDINGS. One row now, and its picker searches the album.
  //
  // The distinction the screen must keep: a `search-no-match` track is still its
  // own row with its own working recording picker.

  const musicLib = () => lib({ id: "lib1", name: "Music", kind: "music" });

  const albumTrack = (
    id: string,
    enrichmentReason: string,
    over: Partial<EnrichmentAttentionTitle> = {},
  ) =>
    enrichmentItem({
      id,
      kind: "track",
      title: `Track ${id}`,
      showTitle: "",
      showId: "",
      seasonNumber: 0,
      episodeNumber: 0,
      artistName: "James Horner",
      albumTitle: "Braveheart",
      albumId: "al-bh",
      path: `/media/music/James Horner/Braveheart/${id}.flac`,
      enrichmentStatus: "unmatched",
      enrichmentReason,
      ...over,
    });

  it("renders one Album row for the album-scoped tracks and leaves a fixable one alone", async () => {
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "album-unmatched"),
      albumTrack("2", "album-unmatched"),
      albumTrack("3", "album-unmatched"),
      albumTrack("4", "search-no-match"),
    ]);
    render();

    await screen.findByTestId("needs-fixing-list");
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(2);
    expect(rows[0].getAttribute("data-kind")).toBe("album");
    expect(within(rows[0]).getByTestId("fix-item-kind")).toHaveTextContent("Album");
    expect(within(rows[0]).getByTestId("fix-item-name")).toHaveTextContent(
      "Braveheart · 3 tracks have no metadata match",
    );
    expect(within(rows[0]).getByTestId("fix-item-breadcrumb")).toHaveTextContent("James Horner");
    // The one the recording picker can actually fix keeps its own row.
    expect(rows[1].getAttribute("data-kind")).toBe("track");
    expect(within(rows[1]).getByTestId("fix-item-name")).toHaveTextContent("Track 4");
  });

  it("keeps the chip count agreeing with the queue it filters", async () => {
    // The collapse must not make the "No metadata" chip and the list it filters
    // tell two different stories: the chip counts rows, and clicking it yields
    // exactly that many.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "album-unmatched"),
      albumTrack("2", "album-unmatched"),
      albumTrack("3", "album-unmatched"),
      albumTrack("4", "search-no-match"),
    ]);
    render();

    await screen.findByTestId("needs-fixing-list");
    expect(screen.getByTestId("needs-fixing-chip-no-metadata")).toHaveTextContent("2");
    expect(screen.getByTestId("needs-fixing-count")).toHaveTextContent("2");

    await userEvent.click(screen.getByTestId("needs-fixing-chip-no-metadata"));
    expect(screen.getAllByTestId("fix-item")).toHaveLength(2);
  });

  it("searches ALBUMS, seeded with the album title and narrowed by the artist", async () => {
    // The gesture the sentence has always named, finally offered: the old row said
    // "fix the album's match" and then searched MusicBrainz /recording.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "album-unmatched"),
      albumTrack("2", "album-unmatched"),
    ]);
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));

    await waitFor(() =>
      expect(searchEntityEnrichmentCandidates).toHaveBeenCalledWith(
        "albums",
        "al-bh",
        "Braveheart",
        { page: 0, artist: "James Horner" },
      ),
    );
    // The artist narrows and stays editable; there is no release axis to narrow an
    // album search by, so no release box and no `release` param.
    expect(screen.getByTestId("fix-picker-artist")).toHaveValue("James Horner");
    expect(screen.queryByTestId("fix-picker-album")).not.toBeInTheDocument();
    expect(searchEnrichmentCandidates).not.toHaveBeenCalled();
  });

  it("applies with the cascade and reports how many tracks it matched", async () => {
    // The point of the whole row: one album pick runs CascadeEntity →
    // mapAlbumTracks over the picked release's track list, which is the path that
    // cleared the *She* album by hand. "12 of 14 tracks matched" is the proof.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "album-unmatched"),
      albumTrack("2", "album-unmatched"),
    ]);
    searchEntityEnrichmentCandidates.mockResolvedValue({
      candidates: [candidate({ externalId: "mbid-bh", title: "Braveheart", year: 1995 })],
      hasMore: false,
    });
    applyEntityEnrichmentOverride.mockResolvedValue({
      entityType: "albums",
      entityId: "al-bh",
      cascade: { updated: 12, attention: 2 },
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    const loads = listEnrichmentAttention.mock.calls.length;
    await userEvent.click(await screen.findByTestId("fix-use-best-guess"));

    await waitFor(() =>
      expect(applyEntityEnrichmentOverride).toHaveBeenCalledWith(
        "albums",
        "al-bh",
        "mbid-bh",
        true,
        // A SEARCHED candidate names no edition (ADR-0052) — only a pasted
        // /release/ URL does — so the apply carries none, and clears any stored one.
        undefined,
      ),
    );
    const summary = await screen.findByTestId("fix-item-cascade");
    expect(summary).toHaveTextContent("12 of 14 tracks matched");
    expect(summary).toHaveTextContent("2 still need attention");
    // Identity is untouched — this is a metadata pin, so no re-file and no rescan.
    expect(fixMatch).not.toHaveBeenCalled();
    expect(screen.queryByTestId("needs-fixing-rescan")).not.toBeInTheDocument();
    // And the queue goes back to the server, because the cascade moved rows.
    await waitFor(() =>
      expect(listEnrichmentAttention.mock.calls.length).toBeGreaterThan(loads),
    );
  });

  it("offers the Edition list on the album row, and only once opened", async () => {
    // ADR-0052 on the queue itself. The row's album is already matched here — its
    // tracks are `not-in-tracklist`, which is *Viaggio Italiano*'s exact state — so
    // the correction is not "which album" but "which pressing of it".
    //
    // The list is a live, rate-limited provider call, so it must not fire for a
    // whole screen of album rows: it mounts with the row's picker, on the click.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "not-in-tracklist"),
      albumTrack("2", "not-in-tracklist"),
    ]);
    listAlbumEditions.mockResolvedValue({
      albumId: "al-bh",
      releaseGroupId: "rg-bh",
      localTrackCount: 16,
      inUseReleaseId: "rel-a",
      inUseSource: "fit",
      editions: [
        { releaseId: "rel-a", date: "1995-05-23", country: "US", format: "CD", trackCount: 18 },
        { releaseId: "rel-b", date: "1997-02-11", country: "IT", format: "CD", trackCount: 16 },
      ],
    });
    render();

    await screen.findByTestId("needs-fixing-list");
    expect(listAlbumEditions).not.toHaveBeenCalled();

    await userEvent.click(screen.getByTestId("fix-item-toggle"));

    await waitFor(() => expect(listAlbumEditions).toHaveBeenCalledWith("al-bh", expect.anything()));
    expect(screen.getByTestId("album-editions-local-count")).toHaveTextContent(
      "This album has 16 tracks",
    );
    const eds = screen.getAllByTestId("album-edition");
    expect(eds).toHaveLength(2);
    expect(within(eds[0]).getByTestId("album-edition-in-use")).toHaveTextContent(
      "In use — best guess",
    );
    // The 16-track pressing is the one that fits, and the row says so rather than
    // leaving the Admin to compare it with a number that is not on screen.
    expect(within(eds[1]).getByTestId("album-edition-fits")).toBeInTheDocument();
  });

  it("applies a chosen edition from the album row, with the cascade and its counts", async () => {
    // The operator's whole workflow, on the row that stands for their tracks: pick
    // the pressing, apply it through the album's existing override carrying the
    // release id, and read what it moved.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "not-in-tracklist"),
      albumTrack("2", "not-in-tracklist"),
    ]);
    listAlbumEditions.mockResolvedValue({
      albumId: "al-bh",
      releaseGroupId: "rg-bh",
      localTrackCount: 16,
      inUseReleaseId: "rel-a",
      inUseSource: "fit",
      editions: [
        { releaseId: "rel-a", date: "1995-05-23", country: "US", format: "CD", trackCount: 18 },
        { releaseId: "rel-b", date: "1997-02-11", country: "IT", format: "CD", trackCount: 16 },
      ],
    });
    applyEntityEnrichmentOverride.mockResolvedValue({
      entityType: "albums",
      entityId: "al-bh",
      cascade: { updated: 16, attention: 0 },
    });
    render();

    await userEvent.click(await screen.findByTestId("fix-item-toggle"));
    await screen.findByTestId("album-edition-list");
    const loads = listEnrichmentAttention.mock.calls.length;
    await userEvent.click(within(screen.getAllByTestId("album-edition")[1]).getByTestId("album-edition-use"));

    await waitFor(() =>
      expect(applyEntityEnrichmentOverride).toHaveBeenCalledWith(
        "albums",
        "al-bh",
        // The release-GROUP stays the record (ADR-0038); the release is the edition.
        "rg-bh",
        true,
        "rel-b",
      ),
    );
    expect(await screen.findByTestId("album-edition-cascade")).toHaveTextContent(
      "16 of 16 tracks matched",
    );
    // Identity is untouched, and the queue goes back to the server because the
    // cascade moved rows.
    expect(fixMatch).not.toHaveBeenCalled();
    await waitFor(() =>
      expect(listEnrichmentAttention.mock.calls.length).toBeGreaterThan(loads),
    );
  });

  it("discloses its track rows on demand, each with its own recording picker", async () => {
    // Collapsed by default — the pile being one decision is the point — but a track
    // the cascade declines must be reachable without a recheck pass.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([
      albumTrack("1", "album-unmatched"),
      albumTrack("2", "album-unmatched"),
    ]);
    render();

    await screen.findByTestId("needs-fixing-list");
    expect(screen.getAllByTestId("fix-item")).toHaveLength(1);
    expect(screen.queryByTestId("fix-item-child-list")).not.toBeInTheDocument();

    await userEvent.click(screen.getByTestId("fix-item-children-toggle"));
    const children = within(screen.getByTestId("fix-item-child-list")).getAllByTestId("fix-item");
    expect(children).toHaveLength(2);
    expect(within(children[0]).getByTestId("fix-item-name")).toHaveTextContent("Track 1");

    // Each child still applies to its own TRACK, through the recording search.
    await userEvent.click(within(children[0]).getByTestId("fix-item-toggle"));
    await waitFor(() =>
      expect(searchEnrichmentCandidates).toHaveBeenCalledWith("1", "Track 1", {
        page: 0,
        artist: "James Horner",
        release: "Braveheart",
      }),
    );
  });

  it("does not collapse a blank reason — an un-re-passed library is untouched", async () => {
    // Every row of a library not re-passed since the reason column shipped carries
    // "". Those rows must render exactly as they did before this collapse existed.
    listLibraries.mockResolvedValue([musicLib()]);
    listEnrichmentAttention.mockResolvedValue([albumTrack("1", ""), albumTrack("2", "")]);
    render();

    await screen.findByTestId("needs-fixing-list");
    const rows = screen.getAllByTestId("fix-item");
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => r.getAttribute("data-kind"))).toEqual(["track", "track"]);
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

// ADR-0051. The failure this block exists for was measured, not imagined: the
// developer shipped six slices of music-matching improvements, rescanned a 10,550
// track library, and the queue went 730 → 722. Nothing in the app could invoke an
// enrichment pass — `POST /libraries/{id}/enrich` had NO client method anywhere in
// web/src — and the one action the screen did offer (a rescan) fires the only-new
// mode, which by construction cannot see a row that has already settled.
describe("AdminNeedsFixingScreen — re-checking the settled rows", () => {
  it("sits beside the library selector and re-checks the SELECTED library", async () => {
    listLibraries.mockResolvedValue([lib(), lib({ id: "lib2", name: "Music", kind: "music" })]);
    render();

    const button = await screen.findByTestId("needs-fixing-recheck-button");
    expect(button).toHaveTextContent("Re-check unmatched items");

    await userEvent.selectOptions(screen.getByTestId("needs-fixing-library-select"), "lib2");
    await userEvent.click(button);

    await waitFor(() => expect(enrichLibrary).toHaveBeenCalled());
    expect(enrichLibrary).toHaveBeenCalledWith("lib2", { mode: "recheck" });
  });

  // The load-bearing change. The POST resolves the moment the pass is QUEUED
  // (ADR-0051's amendment), so a button that re-enabled when the promise settled
  // would tell the operator the pass was over seconds after it began — and the
  // operator who believes that is the one who reloads the page.
  it("stays busy after the request returns, until the pass's terminal event", async () => {
    render();
    const button = await screen.findByTestId("needs-fixing-recheck-button");
    await userEvent.click(button);

    // The request has resolved (the ack), and the pass has not.
    await waitFor(() => expect(enrichLibrary).toHaveBeenCalled());
    expect(button).toBeDisabled();
    expect(button).toHaveTextContent("Re-checking…");
    expect(screen.queryByTestId("needs-fixing-recheck-result")).not.toBeInTheDocument();

    emit("enrichProgress", progressEvent({ total: 1, matched: 1, done: 1, complete: true }));
    await waitFor(() => expect(button).not.toBeDisabled());
    expect(button).toHaveTextContent("Re-check unmatched items");
  });

  it("reports how many rows CLEARED, which is the whole feedback loop", async () => {
    render();
    await userEvent.click(await screen.findByTestId("needs-fixing-recheck-button"));
    await screen.findByTestId("needs-fixing-recheck-progress");

    emit(
      "enrichProgress",
      progressEvent({ total: 722, matched: 14, unmatched: 708, done: 722, complete: true }),
    );

    const result = await screen.findByTestId("needs-fixing-recheck-result");
    expect(result).toHaveTextContent("Re-checked 722 items: 14 now matched, 708 still unmatched.");
  });

  it("says 0 matched rather than going quiet, so 'ran and found nothing' is distinguishable from 'never ran'", async () => {
    render();
    await userEvent.click(await screen.findByTestId("needs-fixing-recheck-button"));
    // The pass is queued and the button is busy; now it finishes.
    await screen.findByTestId("needs-fixing-recheck-progress");

    emit("enrichProgress", progressEvent({ total: 3, unmatched: 3, done: 3, complete: true }));
    expect(await screen.findByTestId("needs-fixing-recheck-result")).toHaveTextContent(
      "Re-checked 3 items: 0 now matched, 3 still unmatched.",
    );
  });

  it("names the retrying count apart from the failed one", async () => {
    render();
    await userEvent.click(await screen.findByTestId("needs-fixing-recheck-button"));
    await screen.findByTestId("needs-fixing-recheck-progress");

    emit(
      "enrichProgress",
      progressEvent({ total: 5, matched: 1, unmatched: 1, failed: 1, retrying: 2, done: 5, complete: true }),
    );
    expect(await screen.findByTestId("needs-fixing-recheck-result")).toHaveTextContent(
      "Re-checked 5 items: 1 now matched, 1 still unmatched, 1 failed, 2 will be retried.",
    );
  });

  it("says plainly when there was nothing to re-check", async () => {
    render();
    await userEvent.click(await screen.findByTestId("needs-fixing-recheck-button"));
    // The pass is queued and the button is busy; now it finishes.
    await screen.findByTestId("needs-fixing-recheck-progress");

    emit("enrichProgress", progressEvent({ complete: true }));
    expect(await screen.findByTestId("needs-fixing-recheck-result")).toHaveTextContent(
      /nothing to re-check/i,
    );
  });

  it("shows how far the pass has got, so a long one is visibly working", async () => {
    render();
    await userEvent.click(await screen.findByTestId("needs-fixing-recheck-button"));

    // Before the pass has counted its work there is nothing to count — a Music
    // recheck re-asks every unmatched parent first (ADR-0051 leaves that unfixed
    // and says so), and the honest thing is to say "working", not "0 of 0".
    expect(await screen.findByTestId("needs-fixing-recheck-progress")).toHaveTextContent(
      /working out what to ask about/i,
    );

    emit("enrichProgress", progressEvent({ total: 722, done: 13 }));
    await waitFor(() =>
      expect(screen.getByTestId("needs-fixing-recheck-progress")).toHaveTextContent(
        "Re-checking… 13 of 722.",
      ),
    );
  });

  // The operator's reload, from the other side: the page comes back mid-pass and
  // must rejoin it rather than offer a button that would start a second one.
  it("rejoins a pass already running when the page loads", async () => {
    getEnrichPassState.mockResolvedValue(
      passState({ running: true, mode: "recheck", progress: { total: 722, done: 13, matched: 1, unmatched: 12, failed: 0, disabled: 0, retrying: 0 } }),
    );
    render();

    const button = await screen.findByTestId("needs-fixing-recheck-button");
    await waitFor(() => expect(button).toBeDisabled());
    expect(button).toHaveTextContent("Re-checking…");
    expect(screen.getByTestId("needs-fixing-recheck-progress")).toHaveTextContent(
      "Re-checking… 13 of 722.",
    );
    expect(getEnrichPassState).toHaveBeenCalledWith("lib1", expect.anything());
    // Nothing was started by arriving: the pass was already the server's.
    expect(enrichLibrary).not.toHaveBeenCalled();

    emit("enrichProgress", progressEvent({ total: 722, matched: 14, unmatched: 708, done: 722, complete: true }));
    await waitFor(() => expect(button).not.toBeDisabled());
    expect(screen.getByTestId("needs-fixing-recheck-result")).toHaveTextContent(
      "Re-checked 722 items: 14 now matched, 708 still unmatched.",
    );
  });

  it("refetches the queue when the pass completes, since the pass is what changed it", async () => {
    listEnrichmentAttention.mockResolvedValue([enrichmentItem()]);
    render();

    await waitFor(() => expect(screen.getAllByTestId("fix-item")).toHaveLength(1));
    const before = listEnrichmentAttention.mock.calls.length;

    // The row cleared server-side; the screen must go and find that out.
    listEnrichmentAttention.mockResolvedValue([]);
    await userEvent.click(screen.getByTestId("needs-fixing-recheck-button"));
    // Still nothing to refetch — the pass has only been queued.
    expect(listEnrichmentAttention.mock.calls.length).toBe(before);

    emit("enrichProgress", progressEvent({ total: 1, matched: 1, done: 1, complete: true }));
    await waitFor(() =>
      expect(listEnrichmentAttention.mock.calls.length).toBeGreaterThan(before),
    );
    expect(await screen.findByTestId("needs-fixing-empty")).toBeInTheDocument();
  });

  it("surfaces a refused start instead of silently doing nothing", async () => {
    // A server running no background passes answers 503 ENRICH_UNAVAILABLE — one of
    // the three states that used to be answered with silence, which is precisely
    // how an operator ends up staring at a button that will never do anything.
    enrichLibrary.mockRejectedValue(
      new Error("this server is not running background enrichment passes"),
    );
    render();

    const button = await screen.findByTestId("needs-fixing-recheck-button");
    await userEvent.click(button);

    const err = await screen.findByTestId("needs-fixing-recheck-error");
    expect(err).toHaveTextContent(/not running background enrichment passes/i);
    expect(screen.queryByTestId("needs-fixing-recheck-result")).not.toBeInTheDocument();
    // And the button goes back to being pressable, rather than sitting in a
    // "Re-checking…" state for a pass that never started.
    await waitFor(() => expect(button).not.toBeDisabled());
  });
});
