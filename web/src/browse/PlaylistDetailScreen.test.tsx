import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { useQueue } from "../player/queue/useQueue";
import { ApiError } from "../api/errors";
import type { PlaylistDetail, PlaylistMember } from "../api/types";

// The Playlist DETAIL against a faked apiClient (the one seam). We mock the
// client so the singleton the screen imports returns canned, NORMALIZED data —
// covering the ORDERED member grid (reusing PosterTile/the grid), each member
// card linking to its Title, remove-by-itemId (a duplicate's other entry
// survives, order preserved), the owner-private 404 "not found" state, the empty,
// generic-error, and loading states, and the header nav link reaching /playlists.

const { getPlaylist, listPlaylists, removePlaylistItem, reorderPlaylistItems } =
  vi.hoisted(() => ({
    getPlaylist: vi.fn(),
    listPlaylists: vi.fn(),
    removePlaylistItem: vi.fn(),
    reorderPlaylistItems: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getPlaylist: (...a: unknown[]) => getPlaylist(...a),
      listPlaylists: (...a: unknown[]) => listPlaylists(...a),
      removePlaylistItem: (...a: unknown[]) => removePlaylistItem(...a),
      reorderPlaylistItems: (...a: unknown[]) => reorderPlaylistItems(...a),
    },
  };
});

import PlaylistDetailScreen from "./PlaylistDetailScreen";
import PlaylistsScreen from "./PlaylistsScreen";

function member(
  itemId: string,
  id: string,
  title: string,
  extra: Partial<PlaylistMember> = {},
): PlaylistMember {
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
    ...extra,
  };
}

function detail(over: Partial<PlaylistDetail> = {}): PlaylistDetail {
  return {
    id: "p1",
    name: "Watch later",
    kind: "movie",
    memberCount: 3,
    members: [
      member("i1", "t1", "Dune"),
      member("i2", "t2", "Arrival"),
      member("i3", "t1", "Dune"), // a deliberate duplicate of t1 (distinct itemId)
    ],
    ...over,
  };
}

// A persistent probe over the shared Queue that echoes what the Play affordance
// built (the current entry's Title + the current index + length). The Play
// affordance now starts playback in the persistent Now Playing bar (no navigation),
// so the queue context is read from the `useQueue` store, not a landing route.
function QueueProbe() {
  const queue = useQueue();
  return (
    <div
      data-testid="play-landing"
      data-queue-current={queue.current?.title.id ?? ""}
      data-queue-index={String(queue.index)}
      data-queue-length={String(queue.length)}
    />
  );
}

function renderDetail(id = "p1") {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/playlists" element={<PlaylistsScreen />} />
        <Route path="/playlists/:id" element={<PlaylistDetailScreen />} />
        <Route
          path="/titles/:titleId"
          element={<div data-testid="title-landing">title</div>}
        />
      </Routes>
      <QueueProbe />
    </>,
    { initialEntries: [`/playlists/${id}`] },
  );
}

beforeEach(() => {
  getPlaylist.mockReset();
  listPlaylists.mockReset();
  removePlaylistItem.mockReset();
  reorderPlaylistItems.mockReset();
  listPlaylists.mockResolvedValue([]);
});

// The rendered titles, top-to-bottom — the visible member order.
function memberTitles() {
  return screen.getAllByTestId("poster-title").map((el) => el.textContent);
}

// The move-down button belonging to a given itemId's entry.
function moveDownFor(itemId: string) {
  return screen
    .getAllByTestId("move-down-button")
    .find((b) => b.getAttribute("data-item-id") === itemId)!;
}

// The move-up button belonging to a given itemId's entry.
function moveUpFor(itemId: string) {
  return screen
    .getAllByTestId("move-up-button")
    .find((b) => b.getAttribute("data-item-id") === itemId)!;
}

describe("PlaylistDetailScreen", () => {
  it("renders members in position order, each card linking to its Title", async () => {
    getPlaylist.mockResolvedValue(detail());
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("playlist-members")).toBeInTheDocument(),
    );
    expect(getPlaylist).toHaveBeenCalledWith("p1", expect.anything());
    expect(screen.getByTestId("playlist-title")).toHaveTextContent("Watch later");

    // Order is preserved: the cards appear in the server's member order.
    const titles = screen
      .getAllByTestId("poster-title")
      .map((el) => el.textContent);
    expect(titles).toEqual(["Dune", "Arrival", "Dune"]);

    // A member card reuses PosterTile and links to its Title's detail page.
    const firstTile = screen.getAllByTestId("poster-tile")[0];
    expect(within(firstTile).getByRole("link")).toHaveAttribute(
      "href",
      "/titles/t1",
    );
  });

  it("removes exactly one entry by its itemId; the duplicate's other entry survives, order preserved", async () => {
    const user = userEvent.setup();
    getPlaylist
      .mockResolvedValueOnce(detail())
      // After removing item i1 (the FIRST Dune), the second Dune (i3) survives.
      .mockResolvedValue(
        detail({
          memberCount: 2,
          members: [member("i2", "t2", "Arrival"), member("i3", "t1", "Dune")],
        }),
      );
    removePlaylistItem.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("playlist-members");
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(3);

    // Remove by ITEM id (not title id) — pick the first Dune's entry, i1.
    const removeI1 = screen
      .getAllByTestId("remove-item-button")
      .find((b) => b.getAttribute("data-item-id") === "i1")!;
    await user.click(removeI1);

    await waitFor(() =>
      expect(removePlaylistItem).toHaveBeenCalledWith("p1", "i1"),
    );
    // The refetch drops only i1; Arrival + the second Dune (i3) remain, in order.
    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile")).toHaveLength(2),
    );
    const titles = screen
      .getAllByTestId("poster-title")
      .map((el) => el.textContent);
    expect(titles).toEqual(["Arrival", "Dune"]);
    expect(screen.getByTestId("playlist-member-count")).toHaveTextContent(
      "2 items",
    );
  });

  it("surfaces a failed remove inline and keeps the entry", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    removePlaylistItem.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to remove playlist item"),
    );

    renderDetail();
    await screen.findByTestId("playlist-members");

    const removeI1 = screen
      .getAllByTestId("remove-item-button")
      .find((b) => b.getAttribute("data-item-id") === "i1")!;
    await user.click(removeI1);

    expect(await screen.findByTestId("remove-item-error")).toHaveTextContent(
      /failed to remove/i,
    );
    // All three entries remain (the remove was refused).
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(3);
  });

  it("renders a 'not found' state when the playlist 404s (owner-private hide-existence)", async () => {
    getPlaylist.mockRejectedValue(
      new ApiError(404, "NOT_FOUND", "playlist not found"),
    );
    renderDetail("foreign");

    await waitFor(() =>
      expect(screen.getByTestId("playlist-not-found")).toBeInTheDocument(),
    );
    // No member grid, no generic error — existence is hidden, nothing leaks.
    expect(screen.queryByTestId("playlist-members")).not.toBeInTheDocument();
    expect(screen.queryByTestId("playlist-error")).not.toBeInTheDocument();
  });

  it("renders a readable error (not 'not found') for a non-404 failure", async () => {
    getPlaylist.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to get playlist"),
    );
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("playlist-error")).toHaveTextContent(
        "failed to get playlist",
      ),
    );
    expect(screen.queryByTestId("playlist-not-found")).not.toBeInTheDocument();
  });

  it("shows an empty state for a playlist with no members", async () => {
    getPlaylist.mockResolvedValue(detail({ memberCount: 0, members: [] }));
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("playlist-empty")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("playlist-members")).not.toBeInTheDocument();
  });

  it("shows a loading state while the detail is in flight", async () => {
    let resolve!: (d: PlaylistDetail) => void;
    getPlaylist.mockReturnValue(
      new Promise<PlaylistDetail>((r) => {
        resolve = r;
      }),
    );
    renderDetail();

    expect(screen.getByTestId("playlist-loading")).toBeInTheDocument();
    resolve(detail());
    await waitFor(() =>
      expect(screen.getByTestId("playlist-members")).toBeInTheDocument(),
    );
  });

  it("moves an entry and saves the FULL new permutation; the detail reflects the new order", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    reorderPlaylistItems.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("playlist-members");
    expect(memberTitles()).toEqual(["Dune", "Arrival", "Dune"]);

    // Move the FIRST entry (i1 / Dune) down one slot.
    await user.click(moveDownFor("i1"));

    // The full ordered itemId permutation is sent (a replace, not a delta).
    await waitFor(() =>
      expect(reorderPlaylistItems).toHaveBeenCalledWith("p1", [
        "i2",
        "i1",
        "i3",
      ]),
    );
    // The detail reflects the new order (optimistic; no refetch needed).
    expect(memberTitles()).toEqual(["Arrival", "Dune", "Dune"]);
  });

  it("reorders duplicate Titles independently by itemId", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    reorderPlaylistItems.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("playlist-members");

    // i1 and i3 are both "Dune"; move the SECOND copy (i3, last) up one slot.
    // Only that entry moves — the first Dune (i1) stays put.
    await user.click(moveUpFor("i3"));

    await waitFor(() =>
      expect(reorderPlaylistItems).toHaveBeenCalledWith("p1", [
        "i1",
        "i3",
        "i2",
      ]),
    );
    expect(memberTitles()).toEqual(["Dune", "Dune", "Arrival"]);
  });

  it("end controls are disabled (can't move the first up or the last down)", async () => {
    getPlaylist.mockResolvedValue(detail());
    renderDetail();
    await screen.findByTestId("playlist-members");

    expect(moveUpFor("i1")).toBeDisabled(); // first entry: no up
    expect(moveDownFor("i3")).toBeDisabled(); // last entry: no down
    expect(moveDownFor("i1")).toBeEnabled();
    expect(moveUpFor("i3")).toBeEnabled();
  });

  it("keeps the existing order and shows a readable inline error when the reorder fails", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    reorderPlaylistItems.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to reorder playlist"),
    );

    renderDetail();
    await screen.findByTestId("playlist-members");

    await user.click(moveDownFor("i1"));

    expect(await screen.findByTestId("reorder-error")).toHaveTextContent(
      /failed to reorder/i,
    );
    // The prior order is restored — a failed save never disturbs what's visible.
    expect(memberTitles()).toEqual(["Dune", "Arrival", "Dune"]);
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(3);
  });

  it("surfaces a defensive ITEM_SET_MISMATCH readably and keeps the order", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    reorderPlaylistItems.mockRejectedValue(
      new ApiError(
        422,
        "ITEM_SET_MISMATCH",
        "itemIds must be exactly the playlist's current item ids",
      ),
    );

    renderDetail();
    await screen.findByTestId("playlist-members");

    await user.click(moveDownFor("i1"));

    expect(await screen.findByTestId("reorder-error")).toHaveTextContent(
      /exactly the playlist's current item ids/i,
    );
    expect(memberTitles()).toEqual(["Dune", "Arrival", "Dune"]);
  });

  it("shows a pending (disabled) state on the move controls during the save", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    let resolveReorder!: () => void;
    reorderPlaylistItems.mockReturnValue(
      new Promise<void>((r) => {
        resolveReorder = r;
      }),
    );

    renderDetail();
    await screen.findByTestId("playlist-members");

    await user.click(moveDownFor("i1"));

    // While the save is in flight every move control is disabled (one outstanding
    // permutation at a time), and the optimistic order is already showing.
    await waitFor(() => expect(moveDownFor("i2")).toBeDisabled());
    expect(memberTitles()).toEqual(["Arrival", "Dune", "Dune"]);

    resolveReorder();
    // After it settles the controls re-enable (a non-end one becomes clickable).
    await waitFor(() => expect(moveDownFor("i1")).toBeEnabled());
  });

  it("reorders using the CURRENT item ids after a remove changed the set", async () => {
    const user = userEvent.setup();
    getPlaylist
      .mockResolvedValueOnce(detail())
      // After removing i1, the live set is just i2, i3 (in that order).
      .mockResolvedValue(
        detail({
          memberCount: 2,
          members: [member("i2", "t2", "Arrival"), member("i3", "t1", "Dune")],
        }),
      );
    removePlaylistItem.mockResolvedValue(undefined);
    reorderPlaylistItems.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("playlist-members");

    // Remove i1, then wait for the refetch to settle the live set to 2 entries.
    const removeI1 = screen
      .getAllByTestId("remove-item-button")
      .find((b) => b.getAttribute("data-item-id") === "i1")!;
    await user.click(removeI1);
    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile")).toHaveLength(2),
    );

    // Now reorder — the permutation must be the CURRENT ids (i2,i3), not stale.
    await user.click(moveDownFor("i2"));
    await waitFor(() =>
      expect(reorderPlaylistItems).toHaveBeenCalledWith("p1", ["i3", "i2"]),
    );
    expect(memberTitles()).toEqual(["Dune", "Arrival"]);
  });

  it("the header Play builds a Queue from the members and starts at the first (index 0)", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    renderDetail();
    await screen.findByTestId("playlist-members");

    await user.click(screen.getByTestId("playlist-play"));

    const landing = screen.getByTestId("play-landing");
    // The Queue holds all three members; playback starts on the first (t1) at
    // index 0 — no URL playlist/index context, it's in the store.
    await waitFor(() => expect(landing).toHaveAttribute("data-queue-current", "t1"));
    expect(landing).toHaveAttribute("data-queue-index", "0");
    expect(landing).toHaveAttribute("data-queue-length", "3");
  });

  it("a per-member Play starts the Queue at THAT member's index (disambiguating a duplicate)", async () => {
    const user = userEvent.setup();
    getPlaylist.mockResolvedValue(detail());
    renderDetail();
    await screen.findByTestId("playlist-members");

    // The SECOND Dune (the duplicate t1) is the third entry, itemId i3 / index 2.
    const playI3 = screen
      .getAllByTestId("play-item-button")
      .find((b) => b.getAttribute("data-item-id") === "i3")!;
    await user.click(playI3);

    const landing = screen.getByTestId("play-landing");
    // Same Title id as the first Dune, but the Queue's current index (2)
    // disambiguates which occurrence is playing.
    await waitFor(() => expect(landing).toHaveAttribute("data-queue-index", "2"));
    expect(landing).toHaveAttribute("data-queue-current", "t1");
    expect(landing).toHaveAttribute("data-queue-length", "3");
  });

  it("shows no Play affordance for an empty playlist", async () => {
    getPlaylist.mockResolvedValue(detail({ memberCount: 0, members: [] }));
    renderDetail();
    await screen.findByTestId("playlist-empty");
    expect(screen.queryByTestId("playlist-play")).not.toBeInTheDocument();
  });

  it("reaches /playlists from the header nav link", async () => {
    getPlaylist.mockResolvedValue(detail());
    renderDetail();
    await waitFor(() =>
      expect(screen.getByTestId("playlist-members")).toBeInTheDocument(),
    );

    // Playlists now lives in the account (username) dropdown.
    await userEvent.click(screen.getByTestId("user-menu-toggle"));
    await userEvent.click(screen.getByTestId("nav-playlists"));

    await waitFor(() =>
      expect(screen.getByTestId("playlists-screen")).toBeInTheDocument(),
    );
    expect(listPlaylists).toHaveBeenCalled();
  });
});
