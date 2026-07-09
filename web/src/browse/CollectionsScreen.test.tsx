import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { CollectionSummary } from "../api/types";

// The Collections browse LIST against a faked apiClient (the one seam). We mock
// the client module so the singleton the screen imports returns canned,
// NORMALIZED data — covering the card rendering (poster + name + member count),
// the link to a Collection's detail, and the empty state.
//
// The error-rendering path (a rejected load → collections-error) is covered
// deterministically by useAsync.test.ts (the hook all these screens share) and
// at screen level by CollectionDetailScreen.test.tsx; it is not re-asserted here
// because, exactly as the LibraryListScreen note records, a directly-mounted
// fast reject races vitest's unhandled-rejection detector. The error markup here
// is byte-identical to the proven LibraryListScreen path.

const { listCollections, createCollection } = vi.hoisted(() => ({
  listCollections: vi.fn(),
  createCollection: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listCollections: (...a: unknown[]) => listCollections(...a),
      createCollection: (...a: unknown[]) => createCollection(...a),
    },
  };
});

import CollectionsScreen from "./CollectionsScreen";

const MEMBER = { id: "u2", username: "ada", role: "member" };

function renderList(user?: { id: string; username: string; role: string }) {
  return renderWithAuth(<CollectionsScreen />, user ? { user } : {});
}

beforeEach(() => {
  listCollections.mockReset();
  createCollection.mockReset();
});

describe("CollectionsScreen", () => {
  it("renders the viewer's collections as cards with a poster, name, and member count", async () => {
    const collections: CollectionSummary[] = [
      {
        id: "c1",
        name: "A24 Films",
        description: "",
        memberCount: 3,
        posterUrl: "/api/v1/titles/t1/artwork/poster",
      },
      { id: "c2", name: "Solo Pick", description: "", memberCount: 1 },
    ];
    listCollections.mockResolvedValue(collections);
    renderList();

    await waitFor(() =>
      expect(screen.getByTestId("collections")).toBeInTheDocument(),
    );
    const tiles = screen.getAllByTestId("collection-tile");
    expect(tiles).toHaveLength(2);

    const a24 = tiles.find((t) => t.getAttribute("data-collection-id") === "c1")!;
    expect(within(a24).getByTestId("collection-name")).toHaveTextContent(
      "A24 Films",
    );
    expect(within(a24).getByTestId("collection-count")).toHaveTextContent(
      "3 items",
    );
    // The representative poster renders from the server's posterUrl.
    expect(within(a24).getByTestId("poster-img")).toHaveAttribute(
      "src",
      "/api/v1/titles/t1/artwork/poster",
    );
    // Singular noun for a one-member Collection.
    const solo = tiles.find((t) => t.getAttribute("data-collection-id") === "c2")!;
    expect(within(solo).getByTestId("collection-count")).toHaveTextContent(
      "1 item",
    );
  });

  it("links each card to its Collection detail", async () => {
    listCollections.mockResolvedValue([
      { id: "c1", name: "A24 Films", description: "", memberCount: 3 },
    ]);
    renderList();

    await waitFor(() =>
      expect(screen.getByTestId("collections")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("collection-item")).toHaveAttribute(
      "href",
      "/collections/c1",
    );
  });

  it("shows a clean empty state when the viewer can see no collections", async () => {
    listCollections.mockResolvedValue([]);
    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("collections-empty")).toBeInTheDocument(),
    );
  });
});

// Curation (issue 02): the Admin "New collection" action — present for an Admin,
// absent for a Member (the same screen, read-only). Creating one calls
// createCollection and the list refetches so the new card appears.
describe("CollectionsScreen — Admin curation", () => {
  it("lets an Admin create a collection; it appears after the list reloads", async () => {
    const user = userEvent.setup();
    // First load empty; after the create, the list refetch returns the new card.
    listCollections
      .mockResolvedValueOnce([])
      .mockResolvedValue([
        { id: "c9", name: "Christmas Movies", description: "", memberCount: 0 },
      ]);
    createCollection.mockResolvedValue({
      id: "c9",
      name: "Christmas Movies",
      description: "",
    });

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("collections-empty")).toBeInTheDocument(),
    );

    // The "New collection" action reveals the inline form.
    await user.click(screen.getByTestId("new-collection-button"));
    await user.type(
      screen.getByTestId("collection-name-input"),
      "Christmas Movies",
    );
    await user.type(
      screen.getByTestId("collection-description-input"),
      "Festive picks",
    );
    await user.click(screen.getByTestId("create-collection-submit"));

    await waitFor(() =>
      expect(createCollection).toHaveBeenCalledWith({
        name: "Christmas Movies",
        description: "Festive picks",
      }),
    );
    // The refetched list shows the new card.
    await waitFor(() =>
      expect(screen.getByTestId("collection-name")).toHaveTextContent(
        "Christmas Movies",
      ),
    );
  });

  it("creates with no description when the field is left blank", async () => {
    const user = userEvent.setup();
    listCollections.mockResolvedValueOnce([]).mockResolvedValue([]);
    createCollection.mockResolvedValue({ id: "c9", name: "Solo", description: "" });

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("collections-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("new-collection-button"));
    await user.type(screen.getByTestId("collection-name-input"), "Solo");
    await user.click(screen.getByTestId("create-collection-submit"));

    // No description key when the optional field is blank.
    await waitFor(() =>
      expect(createCollection).toHaveBeenCalledWith({ name: "Solo" }),
    );
  });

  it("surfaces a failed create as a readable inline error and shows a pending state", async () => {
    const user = userEvent.setup();
    listCollections.mockResolvedValue([]);
    let reject!: (e: unknown) => void;
    createCollection.mockReturnValue(
      new Promise((_, r) => {
        reject = r;
      }),
    );

    renderList();
    await waitFor(() =>
      expect(screen.getByTestId("collections-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("new-collection-button"));
    await user.type(screen.getByTestId("collection-name-input"), "  ");
    // A blank name is caught client-side before any call.
    await user.click(screen.getByTestId("create-collection-submit"));
    expect(
      await screen.findByTestId("create-collection-error"),
    ).toHaveTextContent(/name/i);
    expect(createCollection).not.toHaveBeenCalled();

    // A real name + a server rejection: pending state, then a readable error.
    await user.type(screen.getByTestId("collection-name-input"), "Films");
    await user.click(screen.getByTestId("create-collection-submit"));
    await waitFor(() =>
      expect(screen.getByTestId("create-collection-submit")).toBeDisabled(),
    );
    expect(screen.getByTestId("create-collection-submit")).toHaveTextContent(
      /creating/i,
    );

    reject(new ApiError(400, "BAD_REQUEST", "collection name is required"));
    await waitFor(() =>
      expect(screen.getByTestId("create-collection-error")).toHaveTextContent(
        /required/i,
      ),
    );
    // The form survives a refused create (no crash).
    expect(screen.getByTestId("create-collection-form")).toBeInTheDocument();
  });

  it("shows NO curation controls for a Member (read-only screen)", async () => {
    listCollections.mockResolvedValue([
      { id: "c1", name: "A24 Films", description: "", memberCount: 3 },
    ]);
    renderList(MEMBER);

    await waitFor(() =>
      expect(screen.getByTestId("collections")).toBeInTheDocument(),
    );
    // No "New collection" action, no create form, no curation container.
    expect(screen.queryByTestId("new-collection-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("create-collection-form")).not.toBeInTheDocument();
    expect(screen.queryByTestId("collections-curation")).not.toBeInTheDocument();
    // The list still renders read-only.
    expect(screen.getByTestId("collection-name")).toHaveTextContent("A24 Films");
  });
});
