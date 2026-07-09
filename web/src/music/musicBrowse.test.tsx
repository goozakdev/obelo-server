import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type {
  AlbumTracks,
  ArtistAlbums,
  ArtistsPage,
  Library,
  TitleDetail,
} from "../api/types";

// Kind-aware Music browse SCREENS against a faked apiClient — now the separate
// music experience under /music. A Music library's landing renders Artists (not
// Titles); the Artist detail lists Albums; the Album detail lists Tracks in
// disc/track order with watched/resume markers; a Track's detail shows its
// Artist/Album parent context + a Play affordance. All link within /music and go
// through the single typed client.

const {
  getLibrary,
  listArtists,
  getArtistAlbums,
  getAlbumTracks,
  getTitle,
  listPlaylists,
  appendPlaylistItem,
} = vi.hoisted(() => ({
  getLibrary: vi.fn(),
  listArtists: vi.fn(),
  getArtistAlbums: vi.fn(),
  getAlbumTracks: vi.fn(),
  getTitle: vi.fn(),
  listPlaylists: vi.fn(),
  appendPlaylistItem: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getLibrary: (...a: unknown[]) => getLibrary(...a),
      listArtists: (...a: unknown[]) => listArtists(...a),
      getArtistAlbums: (...a: unknown[]) => getArtistAlbums(...a),
      getAlbumTracks: (...a: unknown[]) => getAlbumTracks(...a),
      getTitle: (...a: unknown[]) => getTitle(...a),
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
      appendPlaylistItem: (...a: unknown[]) => appendPlaylistItem(...a),
    },
  };
});

import MusicLibraryScreen from "./MusicLibraryScreen";
import ArtistDetailScreen from "./ArtistDetailScreen";
import AlbumDetailScreen from "./AlbumDetailScreen";
import TrackDetailScreen from "./TrackDetailScreen";

const musicLib: Library = { id: "lib1", name: "Music", kind: "music", rootFolders: [] };

const artistsPage: ArtistsPage = {
  artists: [
    // ar1 has a fetched image (the grid must show it); ar2 has none (placeholder).
    { id: "ar1", kind: "artist", name: "Radiohead", artworkUrl: "/api/v1/artists/ar1/artwork/poster" },
    { id: "ar2", kind: "artist", name: "Various Artists" },
  ],
  nextCursor: null,
};

const radioheadAlbums: ArtistAlbums = {
  artist: { id: "ar1", kind: "artist", name: "Radiohead" },
  albums: [
    { id: "al1", artistId: "ar1", artistName: "Radiohead", title: "OK Computer", year: 1997, hasArtwork: false, trackCount: 2 },
  ],
};

const okComputerTracks: AlbumTracks = {
  album: { id: "al1", artistId: "ar1", artistName: "Radiohead", title: "OK Computer", year: 1997, hasArtwork: false, trackCount: 2 },
  tracks: [
    { id: "t1", kind: "track", title: "Airbag", discNumber: 1, trackNumber: 1, durationMs: 284000, needsReview: false, resumePositionMs: 0, watched: true },
    { id: "t2", kind: "track", title: "Paranoid Android", discNumber: 1, trackNumber: 2, durationMs: 383000, needsReview: false, resumePositionMs: 30000, watched: false },
  ],
};

beforeEach(() => {
  getLibrary.mockReset();
  listArtists.mockReset();
  getArtistAlbums.mockReset();
  getAlbumTracks.mockReset();
  getTitle.mockReset();
  listPlaylists.mockReset();
  appendPlaylistItem.mockReset();
  getLibrary.mockResolvedValue(musicLib);
  listArtists.mockResolvedValue(artistsPage);
  getArtistAlbums.mockResolvedValue(radioheadAlbums);
  getAlbumTracks.mockResolvedValue(okComputerTracks);
  listPlaylists.mockResolvedValue([]);
  appendPlaylistItem.mockResolvedValue(undefined);
});

describe("Music library landing", () => {
  it("renders an Artist list inside the music shell (not a Title grid)", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );

    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    // The music shell is mounted with the shared app header.
    expect(screen.getByTestId("current-user")).toBeInTheDocument();
    const tiles = screen.getAllByTestId("poster-tile");
    expect(tiles).toHaveLength(2);
    // Tiles link to the Artist detail under /music, not a Title.
    const radio = screen.getByText("Radiohead").closest("a");
    expect(radio).toHaveAttribute("href", "/music/artists/ar1");
    // The Music branch was taken: listArtists was called.
    expect(listArtists).toHaveBeenCalledWith("lib1", { cursor: null }, expect.anything());
  });

  it("shows the artist image on the grid when artworkUrl is present", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/libraries/:libraryId" element={<MusicLibraryScreen />} />
      </Routes>,
      { initialEntries: ["/music/libraries/lib1"] },
    );

    await waitFor(() => expect(screen.getByTestId("poster-grid")).toBeInTheDocument());
    // ar1 carries an artworkUrl → the tile renders an <img> with that src, not
    // initials.
    const radioTile = screen.getByText("Radiohead").closest("li")!;
    const img = within(radioTile).getByTestId("poster-img");
    expect(img).toHaveAttribute("src", "/api/v1/artists/ar1/artwork/poster");
  });
});

describe("Artist detail", () => {
  it("lists the Artist's Albums linking to the Album detail under /music", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/artists/ar1"] },
    );

    await waitFor(() => expect(screen.getByTestId("artist-detail")).toBeInTheDocument());
    expect(screen.getByTestId("artist-name")).toHaveTextContent("Radiohead");
    const album = screen.getByText("OK Computer").closest("a");
    expect(album).toHaveAttribute("href", "/music/albums/al1");
  });
});

describe("Album detail", () => {
  it("lists Tracks in disc/track order with watched/resume markers", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );

    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());
    expect(screen.getByTestId("album-title")).toHaveTextContent("OK Computer");
    // Artist name renders under the title as a link to the artist detail view.
    const artistLink = within(screen.getByTestId("album-artist")).getByRole("link");
    expect(artistLink).toHaveTextContent("Radiohead");
    expect(artistLink).toHaveAttribute("href", "/music/artists/ar1");
    expect(screen.getByTestId("album-year")).toHaveTextContent("1997");

    const rows = screen.getAllByTestId("track-row");
    expect(rows).toHaveLength(2);
    // Airbag (track 1) then Paranoid Android (track 2), in disc/track order.
    expect(within(rows[0]).getByTestId("track-title")).toHaveTextContent("Airbag");
    expect(within(rows[0]).getByTestId("track-number")).toHaveTextContent("1");
    // Track length (mm:ss) and the artist link (→ artist view) in the row.
    expect(within(rows[0]).getByTestId("track-length")).toHaveTextContent("4:44");
    const rowArtist = within(rows[0]).getByTestId("track-artist");
    expect(rowArtist).toHaveTextContent("Radiohead");
    expect(rowArtist).toHaveAttribute("href", "/music/artists/ar1");
    // The title links to the track view.
    expect(within(rows[0]).getByTestId("track-open")).toHaveAttribute(
      "href",
      "/music/tracks/t1",
    );
    expect(within(rows[1]).getByTestId("track-title")).toHaveTextContent("Paranoid Android");
    expect(within(rows[1]).getByTestId("track-length")).toHaveTextContent("6:23");
  });

  it("row actions menu: add to playlist / play next / add to queue / edit", async () => {
    listPlaylists.mockResolvedValue([
      { id: "pl1", name: "Roadtrip", kind: "music", itemCount: 3 },
      { id: "pl2", name: "Fresh", kind: "", itemCount: 0 },
      { id: "pl3", name: "Movie Night", kind: "movie", itemCount: 5 },
    ]);
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());

    const rows = screen.getAllByTestId("track-row");
    // Open the first row's actions menu → the four items are present.
    await userEvent.click(within(rows[0]).getByTestId("track-menu-toggle"));
    expect(within(rows[0]).getByTestId("track-menu-play-next")).toBeInTheDocument();
    expect(within(rows[0]).getByTestId("track-menu-add-queue")).toBeInTheDocument();
    expect(within(rows[0]).getByTestId("track-menu-edit")).toBeInTheDocument();

    // The playlists submenu lists only addable (music / untyped) playlists.
    await userEvent.click(within(rows[0]).getByTestId("track-menu-add-playlist"));
    const options = await within(rows[0]).findAllByTestId("track-menu-playlist-option");
    expect(options.map((o) => o.textContent)).toEqual(["Roadtrip", "Fresh"]);

    // Choosing one appends the track to that playlist.
    await userEvent.click(options[0]);
    await waitFor(() =>
      expect(appendPlaylistItem).toHaveBeenCalledWith("pl1", "t1"),
    );
  });
});

// Per-kind artwork tabs (artwork-management/01): an Artist gets a single "Artist
// Photo" tab, an Album a single "Album Cover" tab (music relabels — the role keys
// stay poster/cover), and a Track none. Asserting the tab BUTTONS (opening the
// dialog stays on the Search tab, so the picker bodies never mount / query).
describe("Edit-item artwork tabs (Admin)", () => {
  it("shows a single Artist Photo tab on an Artist", async () => {
    const user = userEvent.setup();
    renderWithAuth(
      <Routes>
        <Route path="/music/artists/:artistId" element={<ArtistDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/artists/ar1"] },
    );
    await waitFor(() => expect(screen.getByTestId("artist-detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    const tab = screen.getByTestId("edit-item-tab-artist-photo");
    expect(tab).toHaveTextContent("Artist Photo");
    // Not "Image", and no video/album artwork tabs.
    expect(tab).not.toHaveTextContent("Image");
    expect(screen.queryByTestId("edit-item-tab-poster")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-background")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-album-cover")).not.toBeInTheDocument();
  });

  it("shows a single Album Cover tab on an Album", async () => {
    const user = userEvent.setup();
    renderWithAuth(
      <Routes>
        <Route path="/music/albums/:albumId" element={<AlbumDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/albums/al1"] },
    );
    await waitFor(() => expect(screen.getByTestId("album-detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    const tab = screen.getByTestId("edit-item-tab-album-cover");
    expect(tab).toHaveTextContent("Album Cover");
    expect(screen.queryByTestId("edit-item-tab-poster")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-artist-photo")).not.toBeInTheDocument();
  });

  it("shows no artwork tab on a Track", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue({
      id: "t2",
      kind: "track",
      title: "Paranoid Android",
      year: 0,
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
      track: { artistId: "ar1", artistName: "Radiohead", albumId: "al1", albumTitle: "OK Computer", albumYear: 1997, discNumber: 1, trackNumber: 2 },
    } as TitleDetail);
    renderWithAuth(
      <Routes>
        <Route path="/music/tracks/:trackId" element={<TrackDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/tracks/t2"] },
    );
    await waitFor(() => expect(screen.getByTestId("edit-item-button")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    expect(screen.getByTestId("edit-item-tab-search")).toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-poster")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-album-cover")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-artist-photo")).not.toBeInTheDocument();
  });
});

describe("Track detail context", () => {
  it("shows the Artist · Album parent context on a Track's detail", async () => {
    const track: TitleDetail = {
      id: "t2",
      kind: "track",
      title: "Paranoid Android",
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
            { id: "f1", path: "/music/x.flac", container: "flac", width: 0, height: 0, bitrate: 0, durationMs: 200000, sizeBytes: 0, missing: false, streams: [] },
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
      track: {
        artistId: "ar1",
        artistName: "Radiohead",
        albumId: "al1",
        albumTitle: "OK Computer",
        albumYear: 1997,
        discNumber: 1,
        trackNumber: 2,
      },
    };
    getTitle.mockResolvedValue(track);

    renderWithAuth(
      <Routes>
        <Route path="/music/tracks/:titleId" element={<TrackDetailScreen />} />
      </Routes>,
      { initialEntries: ["/music/tracks/t2"] },
    );

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    const ctx = screen.getByTestId("track-context");
    expect(ctx).toHaveTextContent("Radiohead");
    expect(ctx).toHaveTextContent("OK Computer");
    // The Play affordance is present (playback reused unchanged for a Track).
    expect(screen.getByTestId("play-button")).toBeEnabled();
  });
});
