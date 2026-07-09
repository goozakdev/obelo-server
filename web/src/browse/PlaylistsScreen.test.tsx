import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { PlaylistSummary } from "../api/types";

// The Playlists LIST against a faked apiClient (the one seam). We mock the client
// so the singleton the screen imports returns canned, NORMALIZED data — covering
// the own-only list rendering (name + item count), the empty state, and the
// create / rename / delete management flow (each refetching the list).
//
// Owner-private: the screen renders whatever listPlaylists returns (the server
// only ever returns the caller's OWN playlists); create/rename/delete are
// available to EVERY authenticated User (not role-gated), so these run as the
// default Admin AND as a Member with no difference.

const { listPlaylists, createPlaylist, renamePlaylist, deletePlaylist } =
  vi.hoisted(() => ({
    listPlaylists: vi.fn(),
    createPlaylist: vi.fn(),
    renamePlaylist: vi.fn(),
    deletePlaylist: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
      createPlaylist: (...a: unknown[]) => createPlaylist(...a),
      renamePlaylist: (...a: unknown[]) => renamePlaylist(...a),
      deletePlaylist: (...a: unknown[]) => deletePlaylist(...a),
    },
  };
});

import PlaylistsScreen from "./PlaylistsScreen";

const MEMBER = { id: "u2", username: "ada", role: "member" };

function summary(over: Partial<PlaylistSummary> = {}): PlaylistSummary {
  return { id: "p1", name: "Watch later", kind: "movie", itemCount: 2, ...over };
}

function renderList(user?: { id: string; username: string; role: string }) {
  return renderWithAuth(<PlaylistsScreen />, user ? { user } : {});
}

beforeEach(() => {
  listPlaylists.mockReset();
  createPlaylist.mockReset();
  renamePlaylist.mockReset();
  deletePlaylist.mockReset();
});

describe("PlaylistsScreen", () => {
  it("lists only the caller's own playlists with their item counts", async () => {
    listPlaylists.mockResolvedValue([
      summary({ id: "p1", name: "Watch later", itemCount: 3 }),
      summary({ id: "p2", name: "Solo", kind: "", itemCount: 1 }),
    ]);
    renderList();

    await waitFor(() =>
      expect(screen.getByTestId("playlists")).toBeInTheDocument(),
    );
    // The screen renders exactly what listPlaylists (the caller's own only)
    // returns; another User's playlists never reach the client.
    expect(listPlaylists).toHaveBeenCalled();
    const rows = screen.getAllByTestId("playlist-row");
    expect(rows).toHaveLength(2);

    const p1 = rows.find((r) => r.getAttribute("data-playlist-id") === "p1")!;
    expect(within(p1).getByTestId("playlist-name")).toHaveTextContent(
      "Watch later",
    );
    expect(within(p1).getByTestId("playlist-count")).toHaveTextContent("3 items");
    expect(within(p1).getByTestId("playlist-item")).toHaveAttribute(
      "href",
      "/playlists/p1",
    );
    // Singular noun for a one-item playlist.
    const p2 = rows.find((r) => r.getAttribute("data-playlist-id") === "p2")!;
    expect(within(p2).getByTestId("playlist-count")).toHaveTextContent("1 item");
  });

  it("shows a clean empty state when the caller has no playlists", async () => {
    listPlaylists.mockResolvedValue([]);
    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("playlists-empty")).toBeInTheDocument(),
    );
  });

  it("renders a readable error when the list load fails", async () => {
    listPlaylists.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to list playlists"),
    );
    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("playlists-error")).toHaveTextContent(
        "failed to list playlists",
      ),
    );
  });

  it("creates a playlist; it appears after the list reloads", async () => {
    const user = userEvent.setup();
    listPlaylists
      .mockResolvedValueOnce([])
      .mockResolvedValue([summary({ id: "p9", name: "Road trip", kind: "", itemCount: 0 })]);
    createPlaylist.mockResolvedValue({ id: "p9", name: "Road trip", kind: "" });

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("playlists-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("new-playlist-button"));
    await user.type(screen.getByTestId("playlist-name-input"), "Road trip");
    await user.click(screen.getByTestId("create-playlist-submit"));

    await waitFor(() =>
      expect(createPlaylist).toHaveBeenCalledWith("Road trip"),
    );
    // The refetched list shows the new row.
    await waitFor(() =>
      expect(screen.getByTestId("playlist-name")).toHaveTextContent("Road trip"),
    );
  });

  it("does not call create for a blank name (caught client-side)", async () => {
    const user = userEvent.setup();
    listPlaylists.mockResolvedValue([]);

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("playlists-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("new-playlist-button"));
    await user.type(screen.getByTestId("playlist-name-input"), "  ");
    await user.click(screen.getByTestId("create-playlist-submit"));

    expect(
      await screen.findByTestId("create-playlist-error"),
    ).toHaveTextContent(/name/i);
    expect(createPlaylist).not.toHaveBeenCalled();
  });

  it("surfaces a failed create as a readable inline error and shows a pending state", async () => {
    const user = userEvent.setup();
    listPlaylists.mockResolvedValue([]);
    let reject!: (e: unknown) => void;
    createPlaylist.mockReturnValue(
      new Promise((_, r) => {
        reject = r;
      }),
    );

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("playlists-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("new-playlist-button"));
    await user.type(screen.getByTestId("playlist-name-input"), "Films");
    await user.click(screen.getByTestId("create-playlist-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("create-playlist-submit")).toBeDisabled(),
    );
    expect(screen.getByTestId("create-playlist-submit")).toHaveTextContent(
      /creating/i,
    );

    reject(new ApiError(400, "BAD_REQUEST", "playlist name is required"));
    await waitFor(() =>
      expect(screen.getByTestId("create-playlist-error")).toHaveTextContent(
        /required/i,
      ),
    );
    // The form survives a refused create (no crash).
    expect(screen.getByTestId("create-playlist-form")).toBeInTheDocument();
  });

  it("renames a playlist and the list reflects it after the refetch", async () => {
    const user = userEvent.setup();
    listPlaylists
      .mockResolvedValueOnce([summary({ id: "p1", name: "Watch later" })])
      .mockResolvedValue([summary({ id: "p1", name: "Tonight" })]);
    renamePlaylist.mockResolvedValue({ id: "p1", name: "Tonight", kind: "movie" });

    renderList();
    await screen.findByTestId("playlists");

    await user.click(screen.getByTestId("rename-playlist-button"));
    const input = screen.getByTestId("rename-playlist-input");
    await user.clear(input);
    await user.type(input, "Tonight");
    await user.click(screen.getByTestId("save-playlist-button"));

    await waitFor(() =>
      expect(renamePlaylist).toHaveBeenCalledWith("p1", "Tonight"),
    );
    await waitFor(() =>
      expect(screen.getByTestId("playlist-name")).toHaveTextContent("Tonight"),
    );
  });

  it("surfaces a failed rename inline and keeps the editor (no crash)", async () => {
    const user = userEvent.setup();
    listPlaylists.mockResolvedValue([summary({ id: "p1", name: "Watch later" })]);
    renamePlaylist.mockRejectedValue(
      new ApiError(400, "BAD_REQUEST", "playlist name is required"),
    );

    renderList();
    await screen.findByTestId("playlists");

    await user.click(screen.getByTestId("rename-playlist-button"));
    const input = screen.getByTestId("rename-playlist-input");
    await user.clear(input);
    await user.type(input, "x");
    await user.click(screen.getByTestId("save-playlist-button"));

    expect(await screen.findByTestId("rename-playlist-error")).toHaveTextContent(
      /required/i,
    );
    // The editor survives a refused rename.
    expect(screen.getByTestId("playlist-rename-form")).toBeInTheDocument();
  });

  it("deletes a playlist (two-step confirm) and it disappears after the refetch", async () => {
    const user = userEvent.setup();
    listPlaylists
      .mockResolvedValueOnce([
        summary({ id: "p1", name: "Watch later" }),
        summary({ id: "p2", name: "Solo" }),
      ])
      .mockResolvedValue([summary({ id: "p2", name: "Solo" })]);
    deletePlaylist.mockResolvedValue(undefined);

    renderList();
    await screen.findByTestId("playlists");
    expect(screen.getAllByTestId("playlist-row")).toHaveLength(2);

    const p1 = screen
      .getAllByTestId("playlist-row")
      .find((r) => r.getAttribute("data-playlist-id") === "p1")!;
    await user.click(within(p1).getByTestId("delete-playlist-button"));
    // Two-step confirm so a stray click can't drop a playlist.
    await user.click(within(p1).getByTestId("delete-playlist-confirm-button"));

    await waitFor(() => expect(deletePlaylist).toHaveBeenCalledWith("p1"));
    await waitFor(() =>
      expect(screen.getAllByTestId("playlist-row")).toHaveLength(1),
    );
    expect(screen.getByTestId("playlist-name")).toHaveTextContent("Solo");
  });

  it("offers the same management to a Member (playlists are owner-private)", async () => {
    listPlaylists.mockResolvedValue([summary()]);
    renderList(MEMBER);
    await screen.findByTestId("playlists");
    // Not role-gated: a Member sees the create action + per-row controls too.
    expect(screen.getByTestId("new-playlist-button")).toBeInTheDocument();
    expect(screen.getByTestId("rename-playlist-button")).toBeInTheDocument();
    expect(screen.getByTestId("delete-playlist-button")).toBeInTheDocument();
  });
});
