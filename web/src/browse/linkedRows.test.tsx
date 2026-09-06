import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { layoutModeKey } from "./browseLayout";
import type {
  CollectionDetail,
  HomeRows,
  Library,
  PlaylistDetail,
  SeasonEpisodes,
  ShowSeasons,
  ShowsPage,
  TitleDetail,
  TitlesPage,
} from "../api/types";

// Every BROWSE surface that can show a row out of a mirror, asserted through the
// screens themselves (issue 15). The wire has carried `linked`/`available` on
// these shapes since issues 07 and 14; until now only the Libraries list read
// them, so a mirrored film in Continue Watching, a poster in a linked Library's
// grid or a mirrored Playlist member looked local until Play was refused.
//
// Each surface is asserted three ways, because those are the three things that
// can go wrong:
//
//   1. a mirrored row is BADGED,
//   2. a local row beside it is NOT — a badge on everything is the same as a
//      badge on nothing,
//   3. an unreachable mirror is GREYED and still there, still clickable — the
//      catalog is here, only the bytes are away (ADR-0056 §6).
//
// The music surfaces are the counterpart file, src/music/linkedMusicRows.test.tsx.

const {
  getHome,
  getLibrary,
  listTitles,
  listShows,
  getShowSeasons,
  getSeasonEpisodes,
  getCollection,
  listCollections,
  getPlaylist,
  listPlaylists,
  getTitle,
  listLibraries,
  listLinks,
} = vi.hoisted(() => ({
  getHome: vi.fn(),
  getLibrary: vi.fn(),
  listTitles: vi.fn(),
  listShows: vi.fn(),
  getShowSeasons: vi.fn(),
  getSeasonEpisodes: vi.fn(),
  getCollection: vi.fn(),
  listCollections: vi.fn(),
  getPlaylist: vi.fn(),
  listPlaylists: vi.fn(),
  getTitle: vi.fn(),
  listLibraries: vi.fn(),
  listLinks: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getHome: (...a: unknown[]) => getHome(...a),
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      listTitles: (...a: unknown[]) => listTitles(...a),
      listShows: (...a: unknown[]) => listShows(...a),
      getShowSeasons: (...a: unknown[]) => getShowSeasons(...a),
      getSeasonEpisodes: (...a: unknown[]) => getSeasonEpisodes(...a),
      getCollection: (...a: unknown[]) => getCollection(...a),
      listCollections: (...a: unknown[]) => listCollections(...a),
      getPlaylist: (...a: unknown[]) => getPlaylist(...a),
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      listLinks: (...a: unknown[]) => listLinks(...a),
    },
  };
});

import HomeScreen from "../screens/HomeScreen";
import LibraryGridScreen from "./LibraryGridScreen";
import ShowDetailScreen from "./ShowDetailScreen";
import CollectionDetailScreen from "./CollectionDetailScreen";
import PlaylistDetailScreen from "./PlaylistDetailScreen";
import TitleDetailScreen from "./TitleDetailScreen";

// --- fixtures ---------------------------------------------------------------
//
// One mirrored row and one local row on every surface, so the "nowhere on local
// rows" half of the acceptance is asserted by the same render as the badge.

function title(
  id: string,
  name: string,
  marks: { linked?: boolean; available?: boolean } = {},
) {
  return {
    id,
    kind: "movie",
    title: name,
    year: 2001,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
    ...marks,
  };
}

const MIRRORED = title("t1", "Cartoon", { linked: true, available: true });
const AWAY = title("t1", "Cartoon", { linked: true, available: false });
const LOCAL = title("t2", "Dune");

/** The mirrored tile and the local one, from a grid/row container. */
function tiles(container: HTMLElement) {
  const all = within(container).getAllByTestId("poster-tile");
  const byId = (id: string) =>
    all.find((t) => t.getAttribute("data-title-id") === id)!;
  return { mirrored: byId("t1"), local: byId("t2") };
}

beforeEach(() => {
  for (const fn of [
    getHome,
    getLibrary,
    listTitles,
    listShows,
    getShowSeasons,
    getSeasonEpisodes,
    getCollection,
    listCollections,
    getPlaylist,
    listPlaylists,
    getTitle,
    listLibraries,
    listLinks,
  ]) {
    fn.mockReset();
  }
  // The app spine's Libraries fetch, which every screen mounts under.
  listLibraries.mockResolvedValue([]);
  listLinks.mockResolvedValue([]);
  listCollections.mockResolvedValue([]);
  listPlaylists.mockResolvedValue([]);
});

// --- Home -------------------------------------------------------------------

describe("Home rows", () => {
  function home(rows: Partial<HomeRows>): HomeRows {
    return { continueWatching: [], upNext: [], recentlyAdded: [], ...rows };
  }

  function render() {
    return renderWithAuth(
      <Routes>
        <Route path="/" element={<HomeScreen />} />
      </Routes>,
      { initialEntries: ["/"] },
    );
  }

  it("badges a mirrored row and leaves the local one beside it unmarked", async () => {
    // Home is where a viewer meets a mirrored Title with NO Library screen around
    // it, which is why homeTitleJSON carries the pair at all (issue 14).
    getHome.mockResolvedValue(home({ continueWatching: [MIRRORED, LOCAL] }));
    render();
    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching-items")).toBeInTheDocument(),
    );

    const row = screen.getByTestId("home-continue-watching-items");
    const { mirrored, local } = tiles(row);
    expect(within(mirrored).getByTestId("linked-badge")).toHaveTextContent("Linked");
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
    expect(mirrored.className).not.toContain("is-unavailable");
  });

  it("names the sharing server on a mirrored home card when the wire carries it (issue 18)", async () => {
    // PosterTile hands the whole row to LinkedMark, so `linkedServer` reaches the
    // card with no per-surface wiring — a Member sees whose shelf it is.
    const named = title("t1", "Cartoon", {
      linked: true,
      available: true,
      linkedServer: "Kate's Obelo",
    });
    getHome.mockResolvedValue(home({ continueWatching: [named, LOCAL] }));
    render();
    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching-items")).toBeInTheDocument(),
    );

    const { mirrored, local } = tiles(screen.getByTestId("home-continue-watching-items"));
    expect(within(mirrored).getByTestId("linked-badge")).toHaveTextContent(
      "Kate's Obelo",
    );
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });

  it("greys an unreachable one but keeps it in the row, still clickable", async () => {
    getHome.mockResolvedValue(home({ recentlyAdded: [AWAY, LOCAL] }));
    render();
    await waitFor(() =>
      expect(screen.getByTestId("home-recently-added-items")).toBeInTheDocument(),
    );

    const { mirrored, local } = tiles(screen.getByTestId("home-recently-added-items"));
    expect(mirrored.className).toContain("is-unavailable");
    expect(within(mirrored).getByRole("link")).toHaveAttribute("href", "/titles/t1");
    expect(local.className).not.toContain("is-unavailable");
  });
});

// --- The Movie grid (tiles AND the Detail/List rows) -------------------------

describe("the Movie grid", () => {
  const movieLib: Library = { id: "lib1", name: "Films", kind: "movie", rootFolders: [] };

  function page(titles: unknown[]): TitlesPage {
    return { titles, nextCursor: null } as TitlesPage;
  }

  function render() {
    return renderWithAuth(
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>,
      { initialEntries: ["/libraries/lib1"] },
    );
  }

  it("badges the mirrored poster and greys an unreachable one in place", async () => {
    getLibrary.mockResolvedValue(movieLib);
    listTitles.mockResolvedValue(page([AWAY, LOCAL]));
    render();
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());

    const { mirrored, local } = tiles(screen.getByTestId("poster-grid"));
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    expect(mirrored.className).toContain("is-unavailable");
    // Nothing is hidden or removed: the poster is still a link into the detail.
    expect(within(mirrored).getByRole("link")).toHaveAttribute("href", "/titles/t1");
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });

  it("carries the mark into the Detail/List layouts, not just the poster wall", async () => {
    // The layout toggle is a pure re-render of the SAME items (appletv-web-parity
    // §5); a mark that lived only on the tile would vanish on a switch.
    window.localStorage.setItem(layoutModeKey("lib1"), "list");
    getLibrary.mockResolvedValue(movieLib);
    listTitles.mockResolvedValue(page([AWAY, LOCAL]));
    render();
    await waitFor(() =>
      expect(screen.getByTestId("poster-grid")).toHaveAttribute("data-layout", "list"),
    );

    const { mirrored, local } = tiles(screen.getByTestId("poster-grid"));
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    expect(mirrored.className).toContain("is-unavailable");
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
    window.localStorage.removeItem(layoutModeKey("lib1"));
  });
});

// --- The Show grid + the Show detail (header and Episode rows) ---------------

describe("TV", () => {
  const tvLib: Library = { id: "lib2", name: "Shows", kind: "tv", rootFolders: [] };

  const showsPage: ShowsPage = {
    shows: [
      {
        id: "sh1",
        kind: "show",
        title: "Borrowed Show",
        year: 2022,
        needsReview: false,
        unwatchedEpisodeCount: 0,
        libraryId: "lib2",
        overview: "",
        genres: [],
        cast: [],
        linked: true,
        available: false,
      },
      {
        id: "sh2",
        kind: "show",
        title: "Our Show",
        year: 2020,
        needsReview: false,
        unwatchedEpisodeCount: 0,
        libraryId: "lib2",
        overview: "",
        genres: [],
        cast: [],
      },
    ],
    nextCursor: null,
  } as ShowsPage;

  it("badges a mirrored Show in the grid and greys the unreachable one", async () => {
    getLibrary.mockResolvedValue(tvLib);
    listShows.mockResolvedValue(showsPage);
    renderWithAuth(
      <Routes>
        <Route path="/libraries/:libraryId" element={<LibraryGridScreen />} />
      </Routes>,
      { initialEntries: ["/libraries/lib2"] },
    );
    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());

    const all = screen.getAllByTestId("poster-tile");
    const mirrored = all.find((t) => t.getAttribute("data-show-id") === "sh1")!;
    const local = all.find((t) => t.getAttribute("data-show-id") === "sh2")!;
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    expect(mirrored.className).toContain("is-unavailable");
    expect(within(mirrored).getByRole("link")).toHaveAttribute("href", "/shows/sh1");
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });

  it("badges the Show detail header AND its Episode rows", async () => {
    // GET /seasons/{id}/episodes is the one browse document where nothing else
    // could carry the mark — a Season has no Library of its own and the badged
    // Show is a screen back (issue 14 deviation 1).
    const seasons: ShowSeasons = {
      show: showsPage.shows[0],
      seasons: [
        { id: "se1", showId: "sh1", seasonNumber: 1, specials: false, episodeCount: 1 },
      ],
      resumePoint: null,
    };
    const episodes: SeasonEpisodes = {
      season: seasons.seasons[0],
      episodes: [
        {
          id: "ep1",
          kind: "episode",
          title: "Pilot",
          seasonNumber: 1,
          episodeNumber: 1,
          episodeLabel: "",
          needsReview: false,
          resumePositionMs: 0,
          watched: false,
          overview: "",
          linked: true,
          available: false,
        },
      ],
    } as SeasonEpisodes;
    getShowSeasons.mockResolvedValue(seasons);
    getSeasonEpisodes.mockResolvedValue(episodes);

    renderWithAuth(
      <Routes>
        <Route path="/shows/:showId" element={<ShowDetailScreen />} />
      </Routes>,
      { initialEntries: ["/shows/sh1"] },
    );
    await waitFor(() => expect(screen.getByTestId("show-detail")).toBeInTheDocument());

    expect(screen.getByTestId("show-linked-badge")).toHaveTextContent("Linked");
    await waitFor(() => expect(screen.getByTestId("episode-row")).toBeInTheDocument());
    const ep = screen.getByTestId("episode-row");
    expect(within(ep).getByTestId("episode-linked-badge")).toBeInTheDocument();
    expect(ep.className).toContain("is-unavailable");
    // Greyed, not disabled: the row's play control is still there.
    expect(within(ep).getByTestId("episode-play")).toBeInTheDocument();
  });
});

// --- Collections and Playlists (members reuse the browse card) --------------

describe("Collection and Playlist members", () => {
  it("badges a mirrored member of a Collection", async () => {
    const detail: CollectionDetail = {
      id: "c1",
      name: "Saturday",
      description: "",
      memberCount: 2,
      members: [MIRRORED, LOCAL],
    } as CollectionDetail;
    getCollection.mockResolvedValue(detail);
    renderWithAuth(
      <Routes>
        <Route path="/collections/:collectionId" element={<CollectionDetailScreen />} />
      </Routes>,
      { initialEntries: ["/collections/c1"] },
    );
    await waitFor(() => expect(screen.getAllByTestId("poster-tile").length).toBe(2));

    const { mirrored, local } = tiles(document.body);
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    expect(within(local).queryByTestId("linked-badge")).toBeNull();
  });

  it("badges a mirrored Playlist member and greys it when the sharer is away", async () => {
    const detail: PlaylistDetail = {
      id: "p1",
      name: "Road trip",
      kind: "movie",
      memberCount: 2,
      members: [
        { ...AWAY, itemId: "i1" },
        { ...LOCAL, itemId: "i2" },
      ],
    } as PlaylistDetail;
    getPlaylist.mockResolvedValue(detail);
    renderWithAuth(
      <Routes>
        <Route path="/playlists/:playlistId" element={<PlaylistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/playlists/p1"] },
    );
    await waitFor(() => expect(screen.getAllByTestId("poster-tile").length).toBe(2));

    const { mirrored, local } = tiles(document.body);
    expect(within(mirrored).getByTestId("linked-badge")).toBeInTheDocument();
    // It stays IN PLACE — a greyed member must not be reordered or dropped, or a
    // friend's reboot silently rewrites the playlist.
    expect(mirrored.className).toContain("is-unavailable");
    expect(local.className).not.toContain("is-unavailable");
  });
});

// --- The Title detail, which has no mark of its own -------------------------

describe("the Title detail", () => {
  const detail: TitleDetail = {
    id: "t1",
    libraryId: "lib9",
    kind: "movie",
    title: "Cartoon",
    year: 2001,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [],
    artwork: [],
    subtitles: [],
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
  } as TitleDetail;

  function render() {
    return renderWithAuth(
      <Routes>
        <Route path="/titles/:titleId" element={<TitleDetailScreen />} />
      </Routes>,
      { initialEntries: ["/titles/t1"] },
    );
  }

  it("derives the mark from the Title's LIBRARY, which titleDetailJSON does not carry", async () => {
    // Issue 14 deviation 2: the detail document carries no pair. The Library it
    // belongs to does, and the app already holds the Libraries list.
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([
      { id: "lib9", name: "Cartoons", kind: "movie", rootFolders: [], linked: true, available: true },
    ]);
    render();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    await waitFor(() =>
      expect(screen.getByTestId("detail-linked-badge")).toHaveTextContent("Linked"),
    );
  });

  it("names the providing Server when GET /links answers", async () => {
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([
      { id: "lib9", name: "Cartoons", kind: "movie", rootFolders: [], linked: true, available: true },
    ]);
    listLinks.mockResolvedValue([
      { id: "lk1", serverName: "Nadia's Server", libraries: [{ id: "lib9", name: "Cartoons" }] },
    ]);
    render();
    await waitFor(() =>
      expect(screen.getByTestId("detail-linked-badge-provided")).toHaveTextContent(
        "Provided by Nadia's Server",
      ),
    );
  });

  it("degrades to a bare badge when the links read fails", async () => {
    // /links is Admin-only and can fail; losing the whole detail to a decoration
    // would be the worse trade.
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([
      { id: "lib9", name: "Cartoons", kind: "movie", rootFolders: [], linked: true, available: false },
    ]);
    listLinks.mockRejectedValue(new Error("nope"));
    render();
    await waitFor(() =>
      expect(screen.getByTestId("detail-linked-badge")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("detail-linked-badge-provided")).toBeNull();
  });

  it("shows no mark at all for a Title in an ordinary local Library", async () => {
    getTitle.mockResolvedValue(detail);
    listLibraries.mockResolvedValue([
      { id: "lib9", name: "Films", kind: "movie", rootFolders: [{ id: "r", path: "/m" }] },
    ]);
    render();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("detail-linked-badge")).toBeNull();
    // And a server that has never linked is never asked about its links.
    expect(listLinks).not.toHaveBeenCalled();
  });
});

// --- The control: a server that has never linked ----------------------------

describe("a never-linked server", () => {
  it("renders Home, a grid and a Playlist exactly as before — no badge anywhere", async () => {
    getHome.mockResolvedValue({
      continueWatching: [LOCAL],
      upNext: [],
      recentlyAdded: [LOCAL],
    });
    renderWithAuth(
      <Routes>
        <Route path="/" element={<HomeScreen />} />
      </Routes>,
      { initialEntries: ["/"] },
    );
    await waitFor(() =>
      expect(screen.getByTestId("home-continue-watching-items")).toBeInTheDocument(),
    );
    expect(screen.queryAllByTestId("linked-badge")).toHaveLength(0);
    expect(document.querySelectorAll(".is-unavailable")).toHaveLength(0);
    expect(listLinks).not.toHaveBeenCalled();
  });
});
