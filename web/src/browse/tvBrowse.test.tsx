import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type {
  Library,
  SeasonEpisodes,
  ShowSeasons,
  ShowsPage,
  TitleDetail,
} from "../api/types";

// Kind-aware browse SCREENS against a faked apiClient (tv-music issue 01). A TV
// Library's grid renders Shows (not Titles); the Show detail lists Seasons &
// Episodes with watched/resume markers; an Episode's detail shows its
// Show/Season parent context. All driven through the single typed client seam.

const { getLibrary, listShows, getShowSeasons, getSeasonEpisodes, getTitle } =
  vi.hoisted(() => ({
    getLibrary: vi.fn(),
    listShows: vi.fn(),
    getShowSeasons: vi.fn(),
    getSeasonEpisodes: vi.fn(),
    getTitle: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      listShows: (...a: unknown[]) => listShows(...a),
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
    },
  };
});

import LibraryGridScreen from "./LibraryGridScreen";
import ShowDetailScreen from "./ShowDetailScreen";
import TitleDetailScreen from "./TitleDetailScreen";

const tvLib: Library = { id: "lib1", name: "Shows", kind: "tv", rootFolders: [] };

const showsPage: ShowsPage = {
  shows: [
    { id: "sh1", kind: "show", title: "The Bear", year: 2022, needsReview: false, unwatchedEpisodeCount: 3 },
    { id: "sh2", kind: "show", title: "Double Show", year: 2020, needsReview: false, unwatchedEpisodeCount: 0 },
  ],
  nextCursor: null,
};

const bearSeasons: ShowSeasons = {
  show: { id: "sh1", kind: "show", title: "The Bear", year: 2022, needsReview: false, unwatchedEpisodeCount: 3 },
  seasons: [
    { id: "se0", showId: "sh1", seasonNumber: 0, specials: true, episodeCount: 1 },
    { id: "se1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 2 },
  ],
};

const season1Episodes: SeasonEpisodes = {
  season: { id: "se1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 2 },
  episodes: [
    { id: "t1", kind: "episode", title: "System", seasonNumber: 1, episodeNumber: 1, episodeLabel: "", needsReview: false, resumePositionMs: 0, watched: true },
    { id: "t2", kind: "episode", title: "Hands", seasonNumber: 1, episodeNumber: 2, episodeLabel: "", needsReview: false, resumePositionMs: 30000, watched: false },
  ],
};

const specialsEpisodes: SeasonEpisodes = {
  season: { id: "se0", showId: "sh1", seasonNumber: 0, specials: true, episodeCount: 1 },
  episodes: [
    { id: "t0", kind: "episode", title: "Behind the Bear", seasonNumber: 0, episodeNumber: 1, episodeLabel: "", needsReview: false, resumePositionMs: 0, watched: false },
  ],
};

beforeEach(() => {
  getLibrary.mockReset();
  listShows.mockReset();
  getShowSeasons.mockReset();
  getSeasonEpisodes.mockReset();
  getTitle.mockReset();
  getLibrary.mockResolvedValue(tvLib);
  listShows.mockResolvedValue(showsPage);
  getShowSeasons.mockResolvedValue(bearSeasons);
  getSeasonEpisodes.mockImplementation((seasonId: string) =>
    Promise.resolve(seasonId === "se1" ? season1Episodes : specialsEpisodes),
  );
});

describe("TV library browse", () => {
  it("renders a Show grid for a TV library (not a Title grid)", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>,
      { initialEntries: ["/libraries/lib1"] },
    );

    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    const tiles = screen.getAllByTestId("poster-tile");
    expect(tiles).toHaveLength(2);
    // Tiles link to the Show detail, not a Title detail.
    const bear = screen.getByText("The Bear").closest("a");
    expect(bear).toHaveAttribute("href", "/shows/sh1");
    // The TV branch was taken: listShows was called, listTitles was not needed.
    expect(listShows).toHaveBeenCalledWith("lib1", { cursor: null }, expect.anything());
  });

  it("shows the unwatched-episode count badge on a Show poster (and omits it when 0)", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>,
      { initialEntries: ["/libraries/lib1"] },
    );
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    const tiles = screen.getAllByTestId("poster-tile");
    const bear = tiles.find((t) => t.getAttribute("data-show-id") === "sh1")!;
    const dbl = tiles.find((t) => t.getAttribute("data-show-id") === "sh2")!;
    // The Bear has 3 unwatched → badge shows the count; Double Show has 0 → no badge.
    expect(within(bear).getByTestId("badge-unwatched-count")).toHaveTextContent("3");
    expect(within(dbl).queryByTestId("badge-unwatched-count")).toBeNull();
  });
});

describe("Show detail", () => {
  it("offers a Season picker (Specials labeled) and shows the selected Season's Episodes", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );

    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());
    expect(screen.getByTestId("show-title")).toHaveTextContent("The Bear");

    // The picker lists every Season, Specials (season 0) labeled "Specials".
    const select = await screen.findByTestId("season-select");
    const options = within(select).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["Specials", "Season 1"]);

    // Default selection is the first non-specials Season (Season 1), and only THAT
    // Season's block renders at a time.
    const blocks = await screen.findAllByTestId("season-block");
    expect(blocks).toHaveLength(1);
    expect(blocks[0].getAttribute("data-season-number")).toBe("1");

    // Season 1 episodes: System (watched) then Hands (resume marker), in order.
    const rows = within(blocks[0]).getAllByTestId("episode-row");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByTestId("episode-title")).toHaveTextContent("System");
    expect(within(rows[0]).getByTestId("episode-code")).toHaveTextContent("1");
    expect(within(rows[0]).getByTestId("episode-watched")).toBeInTheDocument();
    expect(within(rows[1]).getByTestId("episode-title")).toHaveTextContent("Hands");
    expect(within(rows[1]).getByTestId("episode-resume")).toBeInTheDocument();
  });

  it("offers distinct Poster and Background artwork tabs (artwork-management/01)", async () => {
    const user = userEvent.setup();
    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    expect(screen.getByTestId("edit-item-tab-search")).toBeInTheDocument();
    expect(screen.getByTestId("edit-item-tab-poster")).toHaveTextContent("Poster");
    expect(screen.getByTestId("edit-item-tab-background")).toHaveTextContent("Background");
    expect(screen.getByTestId("edit-item-tab-fix-label")).toBeInTheDocument();
  });

  it("switches to the chosen Season's Episodes when the picker changes", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );

    const select = (await screen.findByTestId("season-select")) as HTMLSelectElement;
    // Pick the Specials season; its single Episode replaces Season 1's list.
    fireEvent.change(select, { target: { value: "se0" } });

    await waitFor(() =>
      expect(screen.getByTestId("episode-title")).toHaveTextContent("Behind the Bear"),
    );
    const block = screen.getByTestId("season-block");
    expect(block.getAttribute("data-season-number")).toBe("0");
    expect(within(block).getAllByTestId("episode-row")).toHaveLength(1);
  });

  it("row actions menu offers Play next / Add to queue / Edit; Edit opens the Episode detail", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
        <Route
          path="/titles/:titleId"
          element={<div data-testid="title-detail-stub" />}
        />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );

    const rows = await screen.findAllByTestId("episode-row");
    const first = rows[0]; // System (t1)
    fireEvent.click(within(first).getByTestId("episode-menu-toggle"));

    const menu = within(first).getByTestId("episode-menu");
    expect(within(menu).getByTestId("episode-menu-play-next")).toBeInTheDocument();
    expect(within(menu).getByTestId("episode-menu-add-queue")).toBeInTheDocument();

    // Edit navigates to the Episode's Title detail page.
    fireEvent.click(within(menu).getByTestId("episode-menu-edit"));
    await waitFor(() =>
      expect(screen.getByTestId("title-detail-stub")).toBeInTheDocument(),
    );
  });

  it("renders the cast strip (headshot + bold name + character) when the Show has cast (cast-photos/02)", async () => {
    getShowSeasons.mockResolvedValue({
      ...bearSeasons,
      show: {
        ...bearSeasons.show,
        cast: [
          { person: "Jeremy Allen White", character: "Carmy", kind: "cast", personId: "tmdb:10", photoVersion: "v1" },
          { person: "Ayo Edebiri", character: "Sydney", kind: "cast" },
        ],
      },
    });

    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );

    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());
    // The same cast strip the Movie detail renders (reused component).
    expect(await screen.findByTestId("detail-cast")).toBeInTheDocument();
    const members = screen.getAllByTestId("cast-member");
    expect(members).toHaveLength(2);
    expect(screen.getAllByTestId("cast-person")[0]).toHaveTextContent("Jeremy Allen White");
    expect(screen.getAllByTestId("cast-character")[0]).toHaveTextContent("Carmy");
    // First member has a headshot img; the photoless second falls back to a placeholder.
    expect(screen.getByTestId("cast-photo")).toBeInTheDocument();
    expect(screen.getByTestId("cast-photo-placeholder")).toBeInTheDocument();
  });

  it("omits the cast strip when the Show has no captured cast", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );

    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());
    // bearSeasons.show carries no cast → no empty cast section.
    expect(screen.queryByTestId("detail-cast")).toBeNull();
  });
});

describe("Episode detail context", () => {
  it("shows the Show · SxxExx parent context on an Episode's Title detail", async () => {
    const episode: TitleDetail = {
      id: "t1",
      kind: "episode",
      title: "System",
      year: 0,
      needsReview: false,
      ambiguous: false,
      hidden: false,
      resumePositionMs: 0,
      watched: false,
      editions: [
        {
          id: "ed1",
          name: "",
          files: [
            { id: "f1", path: "/tv/x.mkv", container: "mkv", width: 1920, height: 1080, bitrate: 0, durationMs: 1000, sizeBytes: 0, missing: false, streams: [] },
          ],
        },
      ],
      artwork: [],
      overview: "",
      tagline: "",
      contentRating: "",
      releaseDate: "",
      runtimeMinutes: 0,
      studio: "",
      genres: [],
      cast: [],
      enrichmentStatus: "",
      lockedFields: [],
      displayTitle: "",
      episode: {
        showId: "sh1",
        showTitle: "The Bear",
        showYear: 2022,
        seasonId: "se1",
        seasonNumber: 1,
        episodeNumber: 1,
      },
    };
    getTitle.mockResolvedValue(episode);

    renderWithAuth(
      <Routes>
        <Route path="/titles/:titleId" element={<TitleDetailScreen />} />
      </Routes>,
      { initialEntries: ["/titles/t1"] },
    );

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    const ctx = screen.getByTestId("episode-context");
    expect(ctx).toHaveTextContent("The Bear");
    expect(ctx).toHaveTextContent("S01E01");
    // The Play affordance is present (playback reused unchanged for an Episode).
    expect(screen.getByTestId("play-button")).toBeEnabled();
  });
});
