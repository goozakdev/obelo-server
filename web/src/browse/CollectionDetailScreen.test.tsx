import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { CollectionDetail, TitleSummary } from "../api/types";

// The Collection DETAIL against a faked apiClient (the one seam). We mock the
// client so the singleton the screen imports returns canned, NORMALIZED data —
// covering the member poster grid (reusing PosterTile/the grid), each member
// card linking to its Title, the 404 hide-existence "not found" state, the empty
// and generic-error states, the loading state, and the header nav link reaching
// /collections.

const {
  getCollection,
  listCollections,
  updateCollection,
  deleteCollection,
  removeCollectionItem,
} = vi.hoisted(() => ({
  getCollection: vi.fn(),
  listCollections: vi.fn(),
  updateCollection: vi.fn(),
  deleteCollection: vi.fn(),
  removeCollectionItem: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getCollection: (...a: unknown[]) => getCollection(...a),
      listCollections: (...a: unknown[]) => listCollections(...a),
      updateCollection: (...a: unknown[]) => updateCollection(...a),
      deleteCollection: (...a: unknown[]) => deleteCollection(...a),
      removeCollectionItem: (...a: unknown[]) => removeCollectionItem(...a),
    },
  };
});

import CollectionDetailScreen from "./CollectionDetailScreen";
import CollectionsScreen from "./CollectionsScreen";

const MEMBER = { id: "u2", username: "ada", role: "member" };

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function member(id: string, title: string, extra: Partial<TitleSummary> = {}): TitleSummary {
  return {
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

function detail(over: Partial<CollectionDetail> = {}): CollectionDetail {
  return {
    id: "c1",
    name: "A24 Films",
    description: "",
    memberCount: 2,
    members: [member("t1", "Hereditary"), member("t2", "Moonlight")],
    ...over,
  };
}

function renderDetail(
  id = "c1",
  user?: { id: string; username: string; role: string },
) {
  return renderWithAuth(
    <Routes>
      <Route path="/collections" element={<CollectionsScreen />} />
      <Route path="/collections/:id" element={<CollectionDetailScreen />} />
      <Route
        path="/titles/:titleId"
        element={<div data-testid="title-landing">title</div>}
      />
    </Routes>,
    { initialEntries: [`/collections/${id}`], ...(user ? { user } : {}) },
  );
}

beforeEach(() => {
  getCollection.mockReset();
  listCollections.mockReset();
  updateCollection.mockReset();
  deleteCollection.mockReset();
  removeCollectionItem.mockReset();
  listCollections.mockResolvedValue([]);
});

describe("CollectionDetailScreen", () => {
  it("renders the members as a poster grid, each card linking to its Title", async () => {
    getCollection.mockResolvedValue(detail());
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("collection-members")).toBeInTheDocument(),
    );
    expect(getCollection).toHaveBeenCalledWith("c1", expect.anything());
    expect(screen.getByTestId("collection-title")).toHaveTextContent("A24 Films");

    const tiles = screen.getAllByTestId("poster-tile");
    expect(tiles).toHaveLength(2);
    // A member card reuses PosterTile and links to its Title's detail page.
    const hereditary = tiles.find((t) => t.getAttribute("data-title-id") === "t1")!;
    expect(within(hereditary).getByRole("link")).toHaveAttribute(
      "href",
      "/titles/t1",
    );
    expect(within(hereditary).getByTestId("poster-title")).toHaveTextContent(
      "Hereditary",
    );
  });

  it("renders a 'not found' state when the Collection 404s (hide-existence)", async () => {
    getCollection.mockRejectedValue(
      new ApiError(404, "NOT_FOUND", "collection not found"),
    );
    renderDetail("hidden");

    await waitFor(() =>
      expect(screen.getByTestId("collection-not-found")).toBeInTheDocument(),
    );
    // No member grid, no generic error — existence is hidden, nothing leaks.
    expect(screen.queryByTestId("collection-members")).not.toBeInTheDocument();
    expect(screen.queryByTestId("collection-error")).not.toBeInTheDocument();
  });

  it("renders a readable error (not 'not found') for a non-404 failure", async () => {
    getCollection.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to get collection"),
    );
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("collection-error")).toHaveTextContent(
        "failed to get collection",
      ),
    );
    expect(screen.queryByTestId("collection-not-found")).not.toBeInTheDocument();
  });

  it("shows an empty state for a Collection with no visible members", async () => {
    getCollection.mockResolvedValue(detail({ memberCount: 0, members: [] }));
    renderDetail();

    await waitFor(() =>
      expect(screen.getByTestId("collection-empty")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("collection-members")).not.toBeInTheDocument();
  });

  it("shows a loading state while the detail is in flight", async () => {
    let resolve!: (d: CollectionDetail) => void;
    getCollection.mockReturnValue(
      new Promise<CollectionDetail>((r) => {
        resolve = r;
      }),
    );
    renderDetail();

    expect(screen.getByTestId("collection-loading")).toBeInTheDocument();
    resolve(detail());
    await waitFor(() =>
      expect(screen.getByTestId("collection-members")).toBeInTheDocument(),
    );
  });

  it("reaches /collections from the header nav link", async () => {
    getCollection.mockResolvedValue(detail());
    renderDetail();
    await waitFor(() =>
      expect(screen.getByTestId("collection-members")).toBeInTheDocument(),
    );

    // Collections now lives in the account (username) dropdown.
    await userEvent.click(screen.getByTestId("user-menu-toggle"));
    await userEvent.click(screen.getByTestId("nav-collections"));

    await waitFor(() =>
      expect(screen.getByTestId("collections-screen")).toBeInTheDocument(),
    );
    expect(listCollections).toHaveBeenCalled();
  });
});

// Curation (issue 02): the Admin rename / edit-description / delete header, the
// per-member remove control, pending/error handling, and the Member read-only
// split — all asserted with the admin-vs-member renderWithAuth session.
describe("CollectionDetailScreen — Admin curation", () => {
  it("renames a collection and the view reflects it after the refetch", async () => {
    const user = userEvent.setup();
    getCollection
      .mockResolvedValueOnce(detail({ name: "A24 Films" }))
      .mockResolvedValue(detail({ name: "A24 Reups" }));
    updateCollection.mockResolvedValue({
      id: "c1",
      name: "A24 Reups",
      description: "",
    });

    renderDetail();
    await screen.findByTestId("collection-members");

    await user.click(screen.getByTestId("edit-collection-button"));
    const nameInput = screen.getByTestId("edit-collection-name-input");
    await user.clear(nameInput);
    await user.type(nameInput, "A24 Reups");
    await user.click(screen.getByTestId("save-collection-button"));

    await waitFor(() =>
      expect(updateCollection).toHaveBeenCalledWith("c1", {
        name: "A24 Reups",
        description: "",
      }),
    );
    // The silent refetch settles the header to the server's new name.
    await waitFor(() =>
      expect(screen.getByTestId("collection-title")).toHaveTextContent(
        "A24 Reups",
      ),
    );
  });

  it("edits the description and the view reflects it after the refetch", async () => {
    const user = userEvent.setup();
    getCollection
      .mockResolvedValueOnce(detail({ description: "" }))
      .mockResolvedValue(detail({ description: "Indie darlings" }));
    updateCollection.mockResolvedValue({
      id: "c1",
      name: "A24 Films",
      description: "Indie darlings",
    });

    renderDetail();
    await screen.findByTestId("collection-members");

    await user.click(screen.getByTestId("edit-collection-button"));
    await user.type(
      screen.getByTestId("edit-collection-description-input"),
      "Indie darlings",
    );
    await user.click(screen.getByTestId("save-collection-button"));

    await waitFor(() =>
      expect(updateCollection).toHaveBeenCalledWith("c1", {
        name: "A24 Films",
        description: "Indie darlings",
      }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("collection-description")).toHaveTextContent(
        "Indie darlings",
      ),
    );
  });

  it("deletes a collection and navigates back to the list", async () => {
    const user = userEvent.setup();
    getCollection.mockResolvedValue(detail());
    deleteCollection.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("collection-members");

    await user.click(screen.getByTestId("delete-collection-button"));
    // Two-step confirm so a stray click can't drop a curated row.
    await user.click(screen.getByTestId("delete-collection-confirm-button"));

    await waitFor(() => expect(deleteCollection).toHaveBeenCalledWith("c1"));
    await waitFor(() =>
      expect(screen.getByTestId("collections-screen")).toBeInTheDocument(),
    );
  });

  it("removes a member and it disappears after the refetch (count refreshes)", async () => {
    const user = userEvent.setup();
    getCollection
      .mockResolvedValueOnce(detail())
      .mockResolvedValue(
        detail({ memberCount: 1, members: [member("t2", "Moonlight")] }),
      );
    removeCollectionItem.mockResolvedValue(undefined);

    renderDetail();
    await screen.findByTestId("collection-members");
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);

    // Each member card carries a per-member remove control (data-title-id).
    const removeButtons = screen.getAllByTestId("remove-member-button");
    const removeT1 = removeButtons.find(
      (b) => b.getAttribute("data-title-id") === "t1",
    )!;
    await user.click(removeT1);

    await waitFor(() =>
      expect(removeCollectionItem).toHaveBeenCalledWith("c1", "t1"),
    );
    // The refetch drops t1; only Moonlight remains and the count reads "1 item".
    await waitFor(() =>
      expect(screen.getAllByTestId("poster-tile")).toHaveLength(1),
    );
    expect(screen.getByTestId("collection-member-count")).toHaveTextContent(
      "1 item",
    );
  });

  it("surfaces a failed rename inline and keeps the editor (no crash)", async () => {
    const user = userEvent.setup();
    getCollection.mockResolvedValue(detail());
    updateCollection.mockRejectedValue(
      new ApiError(400, "BAD_REQUEST", "collection name is required"),
    );

    renderDetail();
    await screen.findByTestId("collection-members");

    await user.click(screen.getByTestId("edit-collection-button"));
    const nameInput = screen.getByTestId("edit-collection-name-input");
    await user.clear(nameInput);
    await user.type(nameInput, "x");
    await user.click(screen.getByTestId("save-collection-button"));

    const err = await screen.findByTestId("collection-edit-error");
    expect(err).toHaveTextContent(/required/i);
    // The editor survives a refused write.
    expect(screen.getByTestId("collection-edit-form")).toBeInTheDocument();
  });

  it("surfaces a failed member remove inline and keeps the card", async () => {
    const user = userEvent.setup();
    getCollection.mockResolvedValue(detail());
    removeCollectionItem.mockRejectedValue(
      new ApiError(500, "INTERNAL", "failed to remove collection item"),
    );

    renderDetail();
    await screen.findByTestId("collection-members");

    const removeT1 = screen
      .getAllByTestId("remove-member-button")
      .find((b) => b.getAttribute("data-title-id") === "t1")!;
    await user.click(removeT1);

    expect(await screen.findByTestId("remove-member-error")).toHaveTextContent(
      /failed to remove/i,
    );
    // Both cards remain (the remove was refused).
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);
  });

  it("shows a pending state during a rename and recovers on error", async () => {
    const user = userEvent.setup();
    getCollection.mockResolvedValue(detail());
    const pending = deferred<{ id: string; name: string; description: string }>();
    updateCollection.mockReturnValue(pending.promise);

    renderDetail();
    await screen.findByTestId("collection-members");

    await user.click(screen.getByTestId("edit-collection-button"));
    await user.click(screen.getByTestId("save-collection-button"));

    await waitFor(() =>
      expect(screen.getByTestId("save-collection-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("save-collection-button")).toHaveTextContent(
      /saving/i,
    );

    pending.reject(new ApiError(500, "INTERNAL", "boom"));
    await waitFor(() =>
      expect(screen.getByTestId("save-collection-button")).toBeEnabled(),
    );
    expect(screen.getByTestId("collection-edit-error")).toHaveTextContent(/boom/i);
  });
});

describe("CollectionDetailScreen — Member (read-only)", () => {
  it("shows NO curation controls for a Member", async () => {
    getCollection.mockResolvedValue(detail());
    renderDetail("c1", MEMBER);

    await screen.findByTestId("collection-members");
    // The members still render (read-only browse), but no curation affordances.
    expect(screen.getAllByTestId("poster-tile")).toHaveLength(2);
    expect(screen.queryByTestId("collection-curation")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-collection-button")).not.toBeInTheDocument();
    expect(
      screen.queryByTestId("delete-collection-button"),
    ).not.toBeInTheDocument();
    expect(screen.queryByTestId("remove-member-button")).not.toBeInTheDocument();
  });
});
