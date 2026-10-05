import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { PlaylistDetail, PlaylistMember } from "../api/types";

// Review fixes on the Playlist detail: a failed background reload keeps the ready
// view (R05-07) and a failed Play says so (R05-08).

const { getPlaylist, removePlaylistItem } = vi.hoisted(() => ({
  getPlaylist: vi.fn(),
  removePlaylistItem: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getPlaylist: (...a: unknown[]) => getPlaylist(...a),
      listPlaylists: () => Promise.resolve([]),
      removePlaylistItem: (...a: unknown[]) => removePlaylistItem(...a),
    },
  };
});

import PlaylistDetailScreen from "./PlaylistDetailScreen";

function member(itemId: string, id: string, title: string): PlaylistMember {
  return {
    itemId,
    id,
    kind: "movie",
    title,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

const detail: PlaylistDetail = {
  id: "p1",
  name: "Watch later",
  kind: "movie",
  memberCount: 2,
  members: [member("i1", "t1", "Dune"), member("i2", "t2", "Arrival")],
};

beforeEach(() => {
  getPlaylist.mockReset().mockResolvedValue(detail);
  removePlaylistItem.mockReset().mockResolvedValue(undefined);
});

function renderDetail() {
  return renderWithAuth(
    <Routes>
      <Route path="/playlists/:id" element={<PlaylistDetailScreen />} />
    </Routes>,
    { initialEntries: ["/playlists/p1"] },
  );
}

describe("PlaylistDetailScreen review fixes", () => {
  it("keeps the ready view when the silent reload after a remove fails (R05-07)", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getAllByTestId("poster-tile")).toHaveLength(2));

    getPlaylist.mockRejectedValue(new Error("boom"));
    await userEvent.click(screen.getAllByTestId("remove-item-button")[0]);
    await waitFor(() => expect(getPlaylist).toHaveBeenCalledTimes(2));

    expect(screen.queryByTestId("playlist-error")).toBeNull();
    expect(screen.getByTestId("playlist-detail")).toBeInTheDocument();
  });

  it("shows an error when Play cannot build the queue (R05-08)", async () => {
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("playlist-play")).toBeInTheDocument());

    getPlaylist.mockRejectedValue(new Error("playlist fetch failed"));
    await userEvent.click(screen.getByTestId("playlist-play"));
    await waitFor(() => expect(screen.getByTestId("play-error")).toBeInTheDocument());
  });
});
