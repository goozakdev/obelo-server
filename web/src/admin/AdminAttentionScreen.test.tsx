import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type {
  Library,
  MatchOverride,
  NeedsReviewItem,
  UnmatchedFile,
} from "../api/types";
import { folderOf } from "./FixMatchForm";

// AdminAttentionScreen through the faked API client (the one seam): a library
// picker drives the per-library lists. needs-review is now a single Admin call
// (GET /libraries/{id}/needs-review) that works for every Library kind; each item
// can be dismissed (mark reviewed) and a Movie can additionally be corrected via a
// folder-keyed fix-match. The Unmatched list renders files; a fix-match submits
// keyed on the file's folder and refreshes the lists; overrides highlight orphans.

const {
  listLibraries,
  listUnmatched,
  listOverrides,
  listEnrichmentAttention,
  listNeedsReview,
  reviewTitle,
  reviewShow,
  fixMatch,
  setEnrichmentMatch,
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
  setEnrichmentMatch: vi.fn(),
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
      setEnrichmentMatch: (...a: unknown[]) => setEnrichmentMatch(...a),
      scanLibrary: (...a: unknown[]) => scanLibrary(...a),
    },
  };
});

import AdminAttentionScreen from "./AdminAttentionScreen";

function lib(over: Partial<Library>): Library {
  return {
    id: "lib1",
    name: "Movies",
    kind: "movie",
    rootFolders: [{ id: "r1", path: "/media/movies" }],
    ...over,
  };
}
function override(over: Partial<MatchOverride>): MatchOverride {
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
function unmatched(over: Partial<UnmatchedFile>): UnmatchedFile {
  return { id: "f1", path: "/media/movies/1080p.mkv", reason: "no identity", ...over };
}
function reviewItem(over: Partial<NeedsReviewItem>): NeedsReviewItem {
  return { id: "t1", kind: "movie", title: "Yearless Movie", year: 0, folderPath: "", ...over };
}

beforeEach(() => {
  listLibraries.mockReset();
  listUnmatched.mockReset();
  listOverrides.mockReset();
  listEnrichmentAttention.mockReset();
  listNeedsReview.mockReset();
  reviewTitle.mockReset();
  reviewShow.mockReset();
  fixMatch.mockReset();
  setEnrichmentMatch.mockReset();
  scanLibrary.mockReset();
  // Sensible empty defaults so a test only sets what it cares about.
  listLibraries.mockResolvedValue([lib({})]);
  listUnmatched.mockResolvedValue([]);
  listOverrides.mockResolvedValue([]);
  listEnrichmentAttention.mockResolvedValue([]);
  listNeedsReview.mockResolvedValue([]);
  reviewTitle.mockResolvedValue(undefined);
  reviewShow.mockResolvedValue(undefined);
  setEnrichmentMatch.mockResolvedValue(undefined);
  scanLibrary.mockResolvedValue({ state: "idle" });
});

describe("AdminAttentionScreen — needs-review", () => {
  it("lists the library's needs-review items (any kind) from one server call", async () => {
    listNeedsReview.mockResolvedValue([
      reviewItem({ id: "t1", kind: "movie", title: "Yearless Movie", year: 0, folderPath: "/media/movies/Yearless Movie" }),
      reviewItem({ id: "ep3", kind: "episode", title: "Date Episode", year: 0, folderPath: "" }),
      reviewItem({ id: "sh9", kind: "show", title: "Yearless Show", year: 0, folderPath: "" }),
    ]);

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });

    await waitFor(() =>
      expect(screen.getByTestId("needs-review-list")).toBeInTheDocument(),
    );
    const items = screen.getAllByTestId("needs-review-item");
    expect(items).toHaveLength(3);
    expect(listNeedsReview).toHaveBeenCalledWith("lib1", expect.anything());
    // A Title links to its detail; a Show links to the show route.
    expect(within(items[0]).getByRole("link")).toHaveAttribute("href", "/titles/t1");
    expect(within(items[2]).getByRole("link")).toHaveAttribute("href", "/shows/sh9");
    // Only the Movie (with a folder) offers fix-identity; all offer mark-reviewed.
    expect(within(items[0]).getByTestId("needs-review-fix-button")).toBeInTheDocument();
    expect(within(items[1]).queryByTestId("needs-review-fix-button")).not.toBeInTheDocument();
    expect(screen.getAllByTestId("needs-review-mark-button")).toHaveLength(3);
  });

  it("shows an empty state when nothing needs review", async () => {
    listNeedsReview.mockResolvedValue([]);
    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-empty")).toBeInTheDocument(),
    );
  });

  it("dismisses a Title via mark-reviewed and refreshes the list", async () => {
    const user = userEvent.setup();
    listNeedsReview
      .mockResolvedValueOnce([reviewItem({ id: "t1", title: "Yearless Movie" })])
      // After the dismissal the item is gone.
      .mockResolvedValue([]);

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("needs-review-mark-button"));
    await waitFor(() => expect(reviewTitle).toHaveBeenCalledWith("t1"));
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-empty")).toBeInTheDocument(),
    );
    expect(reviewShow).not.toHaveBeenCalled();
  });

  it("dismisses a Show via the show review endpoint", async () => {
    const user = userEvent.setup();
    listNeedsReview
      .mockResolvedValueOnce([reviewItem({ id: "sh9", kind: "show", title: "Yearless Show" })])
      .mockResolvedValue([]);

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("needs-review-mark-button"));
    await waitFor(() => expect(reviewShow).toHaveBeenCalledWith("sh9"));
    expect(reviewTitle).not.toHaveBeenCalled();
  });

  it("surfaces a readable error when mark-reviewed fails", async () => {
    const user = userEvent.setup();
    listNeedsReview.mockResolvedValue([reviewItem({ id: "t1" })]);
    reviewTitle.mockRejectedValue(new ApiError(500, "INTERNAL", "boom"));

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("needs-review-mark-button"));
    const err = await screen.findByTestId("needs-review-action-error");
    expect(err).toHaveTextContent(/boom/i);
    // The item is still standing (no crash).
    expect(screen.getByTestId("needs-review-item")).toBeInTheDocument();
  });

  it("corrects a Movie's identity, fetches its metadata, and resolves it", async () => {
    const user = userEvent.setup();
    listNeedsReview
      .mockResolvedValueOnce([
        reviewItem({ id: "t1", title: "Yearless Movie", folderPath: "/media/movies/Yearless Movie" }),
      ])
      // After the fix resolves the item it leaves the list.
      .mockResolvedValue([]);
    fixMatch.mockResolvedValue(override({ id: "o9" }));

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("needs-review-fix-button"));
    const form = screen.getByTestId("fix-match-form");
    // The form anchors to the Movie's folder (the override key).
    expect(form).toHaveAttribute("data-folder-path", "/media/movies/Yearless Movie");

    await user.type(screen.getByTestId("fix-match-title"), "Yearless Movie");
    await user.type(screen.getByTestId("fix-match-year"), "2010");
    await user.type(screen.getByTestId("fix-match-tmdb"), "603");
    await user.click(screen.getByTestId("fix-match-submit"));

    // Identity override recorded...
    await waitFor(() =>
      expect(fixMatch).toHaveBeenCalledWith("lib1", {
        folderPath: "/media/movies/Yearless Movie",
        title: "Yearless Movie",
        year: 2010,
        tmdbId: "603",
        imdbId: undefined,
      }),
    );
    // ...and the corrected metadata/artwork fetched immediately by the same id...
    await waitFor(() =>
      expect(setEnrichmentMatch).toHaveBeenCalledWith("t1", { tmdbId: "603", imdbId: undefined }),
    );
    // ...and the item resolved (marked reviewed → leaves the list)...
    await waitFor(() => expect(reviewTitle).toHaveBeenCalledWith("t1"));
    // ...and a rescan is kicked off so the corrected identity/year applies now.
    await waitFor(() => expect(scanLibrary).toHaveBeenCalledWith("lib1"));
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-empty")).toBeInTheDocument(),
    );
  });

  it("fix-identity with no external id records the override without a metadata fetch", async () => {
    const user = userEvent.setup();
    listNeedsReview
      .mockResolvedValueOnce([
        reviewItem({ id: "t1", title: "Yearless Movie", folderPath: "/media/movies/Yearless Movie" }),
      ])
      .mockResolvedValue([]);
    fixMatch.mockResolvedValue(override({ id: "o9" }));

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("needs-review-fix-button"));
    await user.type(screen.getByTestId("fix-match-title"), "Yearless Movie");
    await user.type(screen.getByTestId("fix-match-year"), "2010");
    await user.click(screen.getByTestId("fix-match-submit"));

    await waitFor(() => expect(fixMatch).toHaveBeenCalled());
    // No external id → no immediate enrichment fetch (the rescan's auto-enrich
    // pass picks up the corrected identity instead).
    expect(setEnrichmentMatch).not.toHaveBeenCalled();
    // Still resolved + rescanned so the corrected identity applies.
    await waitFor(() => expect(reviewTitle).toHaveBeenCalledWith("t1"));
    await waitFor(() => expect(scanLibrary).toHaveBeenCalledWith("lib1"));
  });

  it("fixes a Show via the show endpoints (no title-scoped enrichment match)", async () => {
    const user = userEvent.setup();
    listNeedsReview
      .mockResolvedValueOnce([
        reviewItem({ id: "sh9", kind: "show", title: "Anime Show", folderPath: "/media/tv/Anime Show" }),
      ])
      .mockResolvedValue([]);
    fixMatch.mockResolvedValue(override({ id: "o9" }));

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("needs-review-item")).toBeInTheDocument(),
    );

    // A Show carries a folder anchor, so it offers fix-identity.
    await user.click(screen.getByTestId("needs-review-fix-button"));
    expect(screen.getByTestId("fix-match-form")).toHaveAttribute(
      "data-folder-path",
      "/media/tv/Anime Show",
    );
    await user.type(screen.getByTestId("fix-match-tmdb"), "1399");
    await user.click(screen.getByTestId("fix-match-submit"));

    await waitFor(() =>
      expect(fixMatch).toHaveBeenCalledWith("lib1", {
        folderPath: "/media/tv/Anime Show",
        title: undefined,
        year: undefined,
        tmdbId: "1399",
        imdbId: undefined,
      }),
    );
    // A Show resolves via the show endpoint and rescan — never the title ones.
    await waitFor(() => expect(reviewShow).toHaveBeenCalledWith("sh9"));
    await waitFor(() => expect(scanLibrary).toHaveBeenCalledWith("lib1"));
    expect(setEnrichmentMatch).not.toHaveBeenCalled();
    expect(reviewTitle).not.toHaveBeenCalled();
  });

  it("opens only the clicked item's fix-identity form, even when items share a folder", async () => {
    const user = userEvent.setup();
    // Two bare yearless movies directly under the same root → identical folder.
    listNeedsReview.mockResolvedValue([
      reviewItem({ id: "t1", title: "Alpha", folderPath: "/media/movies" }),
      reviewItem({ id: "t2", title: "Beta", folderPath: "/media/movies" }),
    ]);

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getAllByTestId("needs-review-item")).toHaveLength(2),
    );

    const items = screen.getAllByTestId("needs-review-item");
    // Open the FIRST item's form.
    await user.click(within(items[0]).getByTestId("needs-review-fix-button"));
    // Exactly one form is open, and it belongs to the clicked item.
    expect(screen.getAllByTestId("fix-match-form")).toHaveLength(1);
    expect(within(items[0]).getByTestId("fix-match-form")).toBeInTheDocument();
    expect(within(items[1]).queryByTestId("fix-match-form")).not.toBeInTheDocument();
    // The other item's button still reads "Fix identity" (not "Close").
    expect(within(items[1]).getByTestId("needs-review-fix-button")).toHaveTextContent(
      "Fix identity",
    );
  });
});

describe("AdminAttentionScreen — unmatched + fix-match", () => {
  it("renders unmatched files (path + reason)", async () => {
    listUnmatched.mockResolvedValue([
      unmatched({ id: "f1", path: "/media/movies/1080p.mkv", reason: "no title token" }),
    ]);
    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("unmatched-list")).toBeInTheDocument(),
    );
    const item = screen.getByTestId("unmatched-item");
    expect(within(item).getByTestId("unmatched-path")).toHaveTextContent("/media/movies/1080p.mkv");
    expect(within(item).getByTestId("unmatched-reason")).toHaveTextContent("no title token");
  });

  it("submits a fix-match keyed on the file's folder, then refreshes the lists", async () => {
    const user = userEvent.setup();
    listUnmatched
      .mockResolvedValueOnce([unmatched({ id: "f1", path: "/media/movies/1080p.mkv" })])
      // After the fix-match, the file is gone.
      .mockResolvedValue([]);
    listOverrides
      .mockResolvedValueOnce([])
      // After the fix-match, the override exists.
      .mockResolvedValue([override({ id: "o9", folderPath: "/media/movies", title: "Fixed" })]);
    fixMatch.mockResolvedValue(override({ id: "o9" }));

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("unmatched-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("unmatched-fix-button"));
    const form = screen.getByTestId("fix-match-form");
    // The form anchors to the file's FOLDER (directory of /media/movies/1080p.mkv).
    expect(form).toHaveAttribute("data-folder-path", "/media/movies");
    expect(screen.getByTestId("fix-match-folder")).toHaveTextContent("/media/movies");

    await user.type(screen.getByTestId("fix-match-title"), "Fixed Title");
    await user.type(screen.getByTestId("fix-match-year"), "2021");
    await user.click(screen.getByTestId("fix-match-submit"));

    await waitFor(() =>
      expect(fixMatch).toHaveBeenCalledWith("lib1", {
        folderPath: "/media/movies",
        title: "Fixed Title",
        year: 2021,
        tmdbId: undefined,
        imdbId: undefined,
      }),
    );
    // Lists refresh: the unmatched file disappears, the override appears.
    await waitFor(() =>
      expect(screen.getByTestId("unmatched-empty")).toBeInTheDocument(),
    );
    await waitFor(() =>
      expect(screen.getByTestId("overrides-list")).toBeInTheDocument(),
    );
    expect(screen.getByText("Fixed (2021)")).toBeInTheDocument();
  });

  it("surfaces a readable error when fix-match fails (overlap/validation)", async () => {
    const user = userEvent.setup();
    listUnmatched.mockResolvedValue([unmatched({ id: "f1", path: "/media/movies/1080p.mkv" })]);
    fixMatch.mockImplementation(() =>
      Promise.reject(new ApiError(400, "BAD_REQUEST", "at least one identity signal is required")),
    );

    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("unmatched-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("unmatched-fix-button"));
    await user.type(screen.getByTestId("fix-match-title"), "Whatever");
    await user.click(screen.getByTestId("fix-match-submit"));

    const err = await screen.findByTestId("fix-match-error");
    expect(err).toHaveTextContent(/identity signal/i);
    // The form is still standing (no crash).
    expect(screen.getByTestId("fix-match-form")).toBeInTheDocument();
  });
});

describe("AdminAttentionScreen — overrides", () => {
  it("renders overrides and highlights folder-rename orphans", async () => {
    listOverrides.mockResolvedValue([
      override({ id: "o1", title: "Dune", year: 2021, orphaned: false }),
      override({ id: "o2", title: "Gone", year: 1999, folderPath: "/media/movies/Renamed", orphaned: true }),
    ]);
    renderWithAuth(<AdminAttentionScreen />, { initialEntries: ["/admin/attention"] });
    await waitFor(() =>
      expect(screen.getByTestId("overrides-list")).toBeInTheDocument(),
    );
    const items = screen.getAllByTestId("override-item");
    expect(items).toHaveLength(2);
    // Non-orphan: no marker.
    expect(items[0]).toHaveAttribute("data-orphaned", "false");
    expect(within(items[0]).queryByTestId("override-orphaned")).not.toBeInTheDocument();
    // Orphan: marker present + flagged.
    expect(items[1]).toHaveAttribute("data-orphaned", "true");
    expect(within(items[1]).getByTestId("override-orphaned")).toBeInTheDocument();
  });
});

describe("folderOf", () => {
  it("returns the directory of a file path (the fix-match anchor)", () => {
    expect(folderOf("/media/movies/Dune (2021)/dune.mkv")).toBe("/media/movies/Dune (2021)");
    // A bare file directly under a root → the root dir.
    expect(folderOf("/media/movies/1080p.mkv")).toBe("/media/movies");
    // Windows separators.
    expect(folderOf("C:\\media\\movies\\x.mkv")).toBe("C:\\media\\movies");
    // A trailing slash is trimmed first.
    expect(folderOf("/media/movies/Dune/")).toBe("/media/movies");
  });
});
