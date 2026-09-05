import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, ScanStatus } from "../api/types";

// The Libraries admin hub, seen from the LINKED side (ADR-0056 §1, linked-servers
// issue 16). Issue 10 listed a mirror here with every writer disabled; this is
// what replaced that: the page lists LOCAL Libraries only, and all that remains of
// the mirrors is one line at the foot saying how many are provided by linked
// servers, with a way there.
//
// The three things worth asserting are the three ways this can go wrong:
//   - a mirror leaking back into the list (a row an Admin can do nothing with),
//   - the pointer line appearing on a household that never linked (a signpost to
//     a tab that is not even shown),
//   - and this page depending on GET /links, which it no longer needs for
//     anything — the provider name lived on the row that is gone. (The app-wide
//     librariesContext still reads the links for the browse-side badge when a
//     mirror is in hand, which is why the assertion here is that a FAILED read
//     costs this page nothing, not that no call happens at all.)

const { listLibraries, getScanStatus, scanLibrary, listLinks } = vi.hoisted(() => ({
  listLibraries: vi.fn(),
  getScanStatus: vi.fn(),
  scanLibrary: vi.fn(),
  listLinks: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      getScanStatus: (...a: unknown[]) => getScanStatus(...a),
      scanLibrary: (...a: unknown[]) => scanLibrary(...a),
      listLinks: (...a: unknown[]) => listLinks(...a),
    },
  };
});

import AdminLibrariesScreen from "./AdminLibrariesScreen";

const LOCAL: Library = {
  id: "lib1",
  name: "Movies",
  kind: "movie",
  rootFolders: [{ id: "r1", path: "/media/movies" }],
};

const LOCAL2: Library = {
  id: "lib2",
  name: "Shows",
  kind: "tv",
  rootFolders: [{ id: "r2", path: "/media/tv" }],
};

const MIRROR: Library = {
  id: "lib9",
  name: "Cartoons",
  kind: "movie",
  rootFolders: [],
  linked: true,
  available: true,
};

const MIRROR2: Library = {
  id: "lib8",
  name: "Sam's music",
  kind: "music",
  rootFolders: [],
  linked: true,
  available: false,
};

function idleStatus(id: string): ScanStatus {
  return { libraryId: id, state: "idle", titleCount: 3, titlesFound: 0, filesFound: 0 };
}

beforeEach(() => {
  listLibraries.mockReset();
  getScanStatus.mockReset();
  scanLibrary.mockReset();
  listLinks.mockReset();
  getScanStatus.mockImplementation((id: string) => Promise.resolve(idleStatus(id)));
  scanLibrary.mockResolvedValue(idleStatus("lib1"));
});

async function rows() {
  await waitFor(() => expect(screen.getByTestId("admin-library-list")).toBeInTheDocument());
  return screen.getAllByTestId("admin-library-row");
}

describe("AdminLibrariesScreen — linked libraries live elsewhere", () => {
  it("lists local libraries only, and points at the linked ones", async () => {
    listLibraries.mockResolvedValue([LOCAL, MIRROR, LOCAL2, MIRROR2]);
    renderWithAuth(<AdminLibrariesScreen />);

    const list = await rows();
    expect(list).toHaveLength(2);
    const names = list.map((r) => within(r).getByTestId("admin-library-name").textContent);
    expect(names).toEqual(["Movies", "Shows"]);
    // Not merely un-badged: the mirrors are not on the page in any form.
    expect(screen.queryByText("Cartoons")).toBeNull();
    expect(screen.queryByText("Sam's music")).toBeNull();

    // The count is the local count — the number of shelves this page can act on.
    expect(screen.getByTestId("admin-libraries-count")).toHaveTextContent("2 libraries");

    const note = screen.getByTestId("admin-libraries-linked-note");
    expect(note).toHaveTextContent("2 more libraries are provided by linked servers");
    expect(screen.getByTestId("admin-libraries-linked-link")).toHaveAttribute(
      "href",
      "/admin/linked-servers",
    );
  });

  it("counts one mirror in the singular", async () => {
    listLibraries.mockResolvedValue([LOCAL, MIRROR]);
    renderWithAuth(<AdminLibrariesScreen />);

    await rows();
    expect(screen.getByTestId("admin-libraries-linked-note")).toHaveTextContent(
      "1 more library is provided by linked servers",
    );
  });

  it("shows the pointer even when every shelf on the page is somebody else's", async () => {
    // All linked: the list is legitimately empty and says so, but the pointer has
    // to be there or the Admin is told they have no libraries at all.
    listLibraries.mockResolvedValue([MIRROR, MIRROR2]);
    renderWithAuth(<AdminLibrariesScreen />);

    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("admin-libraries-linked-note")).toHaveTextContent(
      "2 more libraries are provided by linked servers",
    );
  });

  it("renders exactly as before on a household that never linked", async () => {
    listLibraries.mockResolvedValue([LOCAL, LOCAL2]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLibrariesScreen />);

    const list = await rows();
    expect(list).toHaveLength(2);
    // No pointer line at all — not an empty one.
    expect(screen.queryByTestId("admin-libraries-linked-note")).toBeNull();
    // And exactly the requests it always made: nothing anywhere in the tree asks
    // for the links, because nothing in hand is a mirror.
    expect(listLinks).not.toHaveBeenCalled();

    // Every local action is live, none of them disabled by a linked branch that
    // no longer exists.
    await user.click(within(list[0]).getByTestId("library-menu-toggle"));
    expect(within(list[0]).getByTestId("edit-library-button")).toBeEnabled();
    expect(within(list[0]).getByTestId("scan-button")).toBeEnabled();
    expect(within(list[0]).getByTestId("full-scan-button")).toBeEnabled();
    expect(within(list[0]).getByTestId("delete-library-button")).toBeEnabled();
  });

  it("does not depend on the links being readable", async () => {
    // The page no longer joins GET /links for anything — the provider's name
    // lived on the row that is gone — so a links read that fails (the app-wide
    // librariesContext does one for the browse-side badge) must leave this page
    // whole: the rows, the count, and the pointer, with no error banner.
    listLibraries.mockResolvedValue([LOCAL, MIRROR]);
    listLinks.mockRejectedValue(new Error("boom"));
    renderWithAuth(<AdminLibrariesScreen />);

    const list = await rows();
    expect(list).toHaveLength(1);
    expect(screen.getByTestId("admin-libraries-linked-note")).toHaveTextContent(
      "1 more library is provided by linked servers",
    );
    expect(screen.queryByTestId("admin-libraries-error")).toBeNull();
  });

  it("Scan All can no longer reach a mirror — there is no row to reach it from", async () => {
    listLibraries.mockResolvedValue([LOCAL, MIRROR]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLibrariesScreen />);

    await waitFor(() => expect(screen.getByTestId("scan-all-button")).toBeEnabled());
    await user.click(screen.getByTestId("scan-all-button"));

    await waitFor(() => expect(scanLibrary).toHaveBeenCalled());
    for (const call of scanLibrary.mock.calls) {
      expect(call[0]).not.toBe("lib9");
    }
  });

  it("disables Scan All when every library is a mirror", async () => {
    listLibraries.mockResolvedValue([MIRROR, MIRROR2]);
    renderWithAuth(<AdminLibrariesScreen />);

    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("scan-all-button")).toBeDisabled();
    expect(scanLibrary).not.toHaveBeenCalled();
  });
});
