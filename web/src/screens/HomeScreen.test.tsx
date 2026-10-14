import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { HomeRows } from "../api/types";

// The Home SCREEN against a faked apiClient (the one seam). We mock the client
// module so the singleton the screen imports returns canned, NORMALIZED Home
// rows — covering: both rows render from /home data and cards link to detail;
// Continue Watching ordering is preserved (server-ordered, most-recent first);
// empty states when a row is empty; loading and error states.

const { getHome } = vi.hoisted(() => ({ getHome: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getHome: (...a: unknown[]) => getHome(...a),
    },
  };
});

import HomeScreen from "./HomeScreen";

function rows(over: Partial<HomeRows> = {}): HomeRows {
  return {
    continueWatching: [
      { id: "t2", kind: "movie", title: "Dune", year: 2021, needsReview: false, ambiguous: false, resumePositionMs: 42000, watched: false , genres: [] },
      { id: "t1", kind: "movie", title: "Alien", year: 1979, needsReview: false, ambiguous: false, resumePositionMs: 12000, watched: false , genres: [] },
    ],
    upNext: [],
    recentlyAdded: [
      { id: "t3", kind: "movie", title: "Zulu", year: 1964, needsReview: false, ambiguous: false, resumePositionMs: 0, watched: false , genres: [] },
      { id: "t2", kind: "movie", title: "Dune", year: 2021, needsReview: false, ambiguous: false, resumePositionMs: 42000, watched: false , genres: [] },
    ],
    ...over,
  };
}

function renderHome() {
  return renderWithAuth(
    <Routes>
      <Route path="/" element={<HomeScreen />} />
    </Routes>,
    { initialEntries: ["/"] },
  );
}

beforeEach(() => {
  getHome.mockReset();
});

describe("HomeScreen", () => {
  it("renders both rows from /home, in server order, with cards linking to detail", async () => {
    getHome.mockResolvedValue(rows());
    renderHome();

    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching")).toBeInTheDocument(),
    );

    // Continue Watching: server order (Dune first — most-recent), with a resume
    // marker, each card linking to its title.
    const cw = screen.getByTestId("home-continue-watching");
    const cwTiles = within(cw).getAllByTestId("poster-tile");
    expect(cwTiles.map((t) => t.getAttribute("data-title-id"))).toEqual(["t2", "t1"]);
    expect(within(cwTiles[0]).getByTestId("badge-resume")).toHaveTextContent("0:42");
    expect(within(cwTiles[0]).getByRole("link")).toHaveAttribute("href", "/titles/t2");

    // Recently Added: its own server order.
    const ra = screen.getByTestId("home-recently-added");
    const raTiles = within(ra).getAllByTestId("poster-tile");
    expect(raTiles.map((t) => t.getAttribute("data-title-id"))).toEqual(["t3", "t2"]);

    // The libraries link is still present (auth/browse specs depend on it).
    expect(screen.getByTestId("browse-link")).toHaveAttribute("href", "/libraries");
  });

  it("renders the Up Next row with parent-context labels on Episode cards", async () => {
    getHome.mockResolvedValue(
      rows({
        upNext: [
          {
            id: "e2",
            kind: "episode",
            title: "Hands",
            year: 0,
            needsReview: false,
            ambiguous: false,
            resumePositionMs: 0,
            watched: false,
            genres: [],
            episode: {
              showId: "sh1",
              showTitle: "The Bear",
              seasonId: "s1",
              seasonNumber: 1,
              episodeNumber: 2,
            },
          },
        ],
        continueWatching: [
          {
            id: "e1",
            kind: "episode",
            title: "System",
            year: 0,
            needsReview: false,
            ambiguous: false,
            resumePositionMs: 30000,
            watched: false,
            genres: [],
            episode: {
              showId: "sh1",
              showTitle: "The Bear",
              seasonId: "s1",
              seasonNumber: 1,
              episodeNumber: 1,
            },
          },
        ],
      }),
    );
    renderHome();

    // The Up Next row appears with the next episode, labeled "The Bear · S01E02".
    const up = await screen.findByTestId("home-up-next");
    const upTile = within(up).getByTestId("poster-tile");
    expect(upTile.getAttribute("data-title-id")).toBe("e2");
    expect(within(upTile).getByTestId("poster-context")).toHaveTextContent("The Bear · S01E02");
    expect(within(upTile).getByTestId("poster-title")).toHaveTextContent("Hands");
    // The card links to the Episode's detail/play page.
    expect(within(upTile).getByRole("link")).toHaveAttribute("href", "/titles/e2");

    // Continue Watching's in-progress Episode also carries its parent context.
    const cwTile = within(screen.getByTestId("home-continue-watching")).getByTestId("poster-tile");
    expect(within(cwTile).getByTestId("poster-context")).toHaveTextContent("The Bear · S01E01");
  });

  it("hides the Up Next row entirely when no Show is in progress", async () => {
    getHome.mockResolvedValue(rows()); // upNext: []
    renderHome();
    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("home-up-next")).toBeNull();
  });

  it("shows a per-row empty state when a row has no items", async () => {
    getHome.mockResolvedValue(rows({ continueWatching: [] }));
    renderHome();

    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching-empty")).toBeInTheDocument(),
    );
    // The empty row shows no cards…
    expect(
      within(screen.getByTestId("home-continue-watching")).queryByTestId("poster-tile"),
    ).toBeNull();
    // …while the populated row still renders its cards.
    expect(
      within(screen.getByTestId("home-recently-added")).getAllByTestId("poster-tile").length,
    ).toBe(2);
  });

  it("renders empty states for both rows on a brand-new/empty server", async () => {
    getHome.mockResolvedValue({ continueWatching: [], recentlyAdded: [] });
    renderHome();

    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching-empty")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("home-recently-added-empty")).toBeInTheDocument();
    expect(screen.queryAllByTestId("poster-tile")).toHaveLength(0);
  });

  it("shows the loading state before data resolves", async () => {
    getHome.mockImplementation(() => new Promise(() => {})); // never resolves
    renderHome();
    expect(screen.getByTestId("home-loading")).toBeInTheDocument();
  });

  it("surfaces a readable error when /home fails", async () => {
    const { ApiError } = await vi.importActual<typeof import("../api/client")>("../api/client");
    getHome.mockImplementation(() =>
      Promise.reject(new ApiError(500, "INTERNAL", "failed to build home")),
    );
    renderHome();

    await waitFor(() => expect(screen.getByTestId("home-error")).toBeInTheDocument());
    expect(screen.getByTestId("home-error")).toHaveTextContent("failed to build home");
  });

  describe("rows", () => {
    const t = (id: string, title: string) => ({
      id, kind: "movie" as const, title, year: 0, needsReview: false, ambiguous: false,
      resumePositionMs: 0, watched: false, genres: [],
    });
    const show = (id: string, title: string) => ({
      id, libraryId: "tv", kind: "show" as const, title, year: 0, needsReview: false,
      unwatchedEpisodeCount: 0, overview: "", genres: [], cast: [],
    });

    it("renders rows in the server's order, ignoring the legacy arrays, and omits empty rows", async () => {
      getHome.mockResolvedValue(
        rows({
          rows: [
            { kind: "continueWatching", title: "Continue Watching", titles: [t("c1", "Dune")], shows: [] },
            { kind: "upNext", title: "Up Next", titles: [], shows: [] },
            {
              kind: "discovery", title: "From A24", mediaKind: "movie",
              titles: [t("d1", "Heat")], shows: [],
              source: { type: "studio", value: "A24", libraryIds: ["libA", "libB"] }, total: 12,
            },
            { kind: "myList", title: "My List", titles: [t("l1", "Alien")], shows: [] },
            {
              kind: "discovery", title: "On HBO", mediaKind: "tv",
              titles: [], shows: [show("s1", "Succession")],
              source: { type: "network", value: "HBO", libraryIds: ["tv"] },
            },
            { kind: "recentlyAdded", title: "Recently Added", titles: [t("r1", "Zulu")], shows: [] },
          ],
        }),
      );
      renderHome();
      await screen.findByTestId("home-rows");

      const heads = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
      expect(heads).toEqual(["Continue Watching", "From A24", "My List", "On HBO", "Recently Added"]);
      expect(screen.queryByText("Up Next")).toBeNull();
      // The legacy arrays' titles (Zulu is also legacy, Dune too) are not double-rendered.
      expect(screen.getAllByText("Dune")).toHaveLength(1);

      // A TV discovery row shows Show cards linking to Show detail.
      const hbo = screen.getByTestId("home-row-3");
      expect(within(hbo).getByRole("link", { name: /Succession/ })).toHaveAttribute("href", "/shows/s1");

      // See all: Discovery -> filtered browse on the first Library; My List -> Playlists.
      expect(within(screen.getByTestId("home-row-1")).getByTestId("home-row-see-all"))
        .toHaveAttribute("href", "/libraries/libA?studio=A24");
      expect(within(hbo).getByTestId("home-row-see-all")).toHaveAttribute("href", "/libraries/tv?network=HBO");
      expect(within(screen.getByTestId("home-row-2")).getByTestId("home-row-see-all"))
        .toHaveAttribute("href", "/playlists");
      expect(within(screen.getByTestId("home-continue-watching")).queryByTestId("home-row-see-all")).toBeNull();
    });

    it("shows one Home-level empty state when rows is empty", async () => {
      getHome.mockResolvedValue(rows({ rows: [] }));
      renderHome();
      const empty = await screen.findByTestId("home-empty");
      expect(empty).toHaveTextContent("Nothing added yet. Scan a library to fill this in.");
      expect(screen.getByTestId("browse-link")).toHaveAttribute("href", "/libraries");
    });

    it("shows the empty state when every row has zero items", async () => {
      getHome.mockResolvedValue(
        rows({
          rows: [
            { kind: "continueWatching", title: "Continue Watching", titles: [], shows: [] },
            { kind: "recentlyAdded", title: "Recently Added", titles: [], shows: [] },
          ],
        }),
      );
      renderHome();
      expect(await screen.findByTestId("home-empty")).toBeInTheDocument();
      expect(screen.queryAllByRole("heading", { level: 2 })).toHaveLength(0);
    });

    it("shows no empty state when at least one row has items", async () => {
      getHome.mockResolvedValue(
        rows({
          rows: [
            { kind: "upNext", title: "Up Next", titles: [], shows: [] },
            { kind: "recentlyAdded", title: "Recently Added", titles: [t("r1", "Zulu")], shows: [] },
          ],
        }),
      );
      renderHome();
      await screen.findByTestId("home-recently-added");
      expect(screen.queryByTestId("home-empty")).toBeNull();
    });

    it("skips a row with an unknown kind", async () => {
      getHome.mockResolvedValue(
        rows({
          rows: [
            { kind: "futureShelf" as never, title: "Future Shelf", titles: [t("f1", "Mystery")], shows: [] },
            { kind: "recentlyAdded", title: "Recently Added", titles: [t("r1", "Zulu")], shows: [] },
          ],
        }),
      );
      renderHome();
      await screen.findByTestId("home-recently-added");
      expect(screen.queryByText("Future Shelf")).toBeNull();
      expect(screen.queryByText("Mystery")).toBeNull();
    });

    it("shows the empty state when the only row has an unknown kind", async () => {
      getHome.mockResolvedValue(
        rows({
          rows: [{ kind: "futureShelf" as never, title: "Future Shelf", titles: [t("f1", "Mystery")], shows: [] }],
        }),
      );
      renderHome();
      expect(await screen.findByTestId("home-empty")).toBeInTheDocument();
      expect(screen.queryByText("Mystery")).toBeNull();
    });

    it("falls back to the three legacy rows when rows is absent", async () => {
      getHome.mockResolvedValue(rows());
      renderHome();
      await screen.findByTestId("home-continue-watching");
      expect(screen.queryByTestId("home-rows")).toBeNull();
      expect(screen.getByTestId("home-recently-added")).toBeInTheDocument();
    });
  });
});
