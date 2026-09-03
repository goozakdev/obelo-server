import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library, Link, ScanStatus } from "../api/types";

// The Libraries admin list, seen from the LINKED side (ADR-0056 §1,
// linked-servers issue 10): a mirror of another household's Library is badged,
// says whose it is, and has every writer disabled — because the server refuses
// each of them with 409 LINKED_LIBRARY and a button whose only outcome is an
// error is worse than no button.
//
// The server name is the interesting part. Library JSON carries `linked` and
// `available` and NEVER says whose the Library is, so the hub joins it against
// GET /links — which it must not call at all on a household that has never
// linked, and must degrade from rather than fail when it cannot.

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

const MIRROR: Library = {
  id: "lib9",
  name: "Cartoons",
  kind: "movie",
  rootFolders: [],
  linked: true,
  available: true,
};

const LINK: Link = {
  id: "link1",
  serverId: "srv-sam",
  serverName: "Sam's server",
  state: "connected",
  activeOrigin: "https://sam.example.net",
  origins: ["https://sam.example.net"],
  lastSyncedAt: "2026-09-02T10:00:00Z",
  lastError: "",
  libraries: [{ id: "lib9", name: "Cartoons", kind: "movie" }],
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

async function rowFor(name: string) {
  await waitFor(() => expect(screen.getByTestId("admin-library-list")).toBeInTheDocument());
  const rows = screen.getAllByTestId("admin-library-row");
  const row = rows.find((r) => within(r).queryByText(name));
  if (!row) throw new Error(`no row for ${name}`);
  return row;
}

describe("AdminLibrariesScreen — a linked library", () => {
  it("badges it and names the server providing it", async () => {
    listLibraries.mockResolvedValue([LOCAL, MIRROR]);
    listLinks.mockResolvedValue([LINK]);
    renderWithAuth(<AdminLibrariesScreen />);

    const row = await rowFor("Cartoons");
    expect(within(row).getByTestId("admin-library-linked-badge")).toHaveTextContent(
      "Linked",
    );
    await waitFor(() =>
      expect(within(row).getByTestId("admin-library-provided")).toHaveTextContent(
        /provided by sam's server/i,
      ),
    );

    // The local Library beside it is untouched.
    const local = await rowFor("Movies");
    expect(within(local).queryByTestId("admin-library-linked-badge")).toBeNull();
  });

  it("disables Edit, Scan, Full scan and Delete, and says why", async () => {
    listLibraries.mockResolvedValue([MIRROR]);
    listLinks.mockResolvedValue([LINK]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLibrariesScreen />);

    const row = await rowFor("Cartoons");
    await user.click(within(row).getByTestId("library-menu-toggle"));

    expect(within(row).getByTestId("edit-library-button")).toBeDisabled();
    expect(within(row).getByTestId("scan-button")).toBeDisabled();
    expect(within(row).getByTestId("full-scan-button")).toBeDisabled();
    // Delete too: unlinking is the only thing that removes what came over a Link,
    // and the note points there rather than leaving the reader guessing.
    expect(within(row).getByTestId("delete-library-button")).toBeDisabled();
    expect(within(row).getByText(/remove it with unlink/i)).toBeInTheDocument();
  });

  it("Scan All skips it — no scan is ever POSTed for a mirror", async () => {
    listLibraries.mockResolvedValue([LOCAL, MIRROR]);
    listLinks.mockResolvedValue([LINK]);
    const user = userEvent.setup();
    renderWithAuth(<AdminLibrariesScreen />);

    await waitFor(() => expect(screen.getByTestId("scan-all-button")).toBeEnabled());
    await user.click(screen.getByTestId("scan-all-button"));

    await waitFor(() => expect(scanLibrary).toHaveBeenCalled());
    for (const call of scanLibrary.mock.calls) {
      expect(call[0]).not.toBe("lib9");
    }
  });

  it("does not ask for the links at all when nothing is linked", async () => {
    listLibraries.mockResolvedValue([LOCAL]);
    renderWithAuth(<AdminLibrariesScreen />);

    await rowFor("Movies");
    // A household that has never linked makes exactly the requests it always did.
    expect(listLinks).not.toHaveBeenCalled();
  });

  it("keeps the badge when the links cannot be read, with a nameless note", async () => {
    listLibraries.mockResolvedValue([MIRROR]);
    listLinks.mockRejectedValue(new Error("boom"));
    renderWithAuth(<AdminLibrariesScreen />);

    const row = await rowFor("Cartoons");
    expect(within(row).getByTestId("admin-library-linked-badge")).toBeInTheDocument();
    expect(within(row).getByTestId("admin-library-provided")).toHaveTextContent(
      /provided by another server/i,
    );
    // And no error banner over a libraries list that loaded perfectly well.
    expect(screen.queryByTestId("admin-libraries-error")).toBeNull();
  });
});
