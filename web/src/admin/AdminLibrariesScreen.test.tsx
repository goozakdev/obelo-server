import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { Library, ScanStatus } from "../api/types";

// AdminLibrariesScreen (redesigned UI) end-to-end through the faked API client
// (the one seam): the top bar shows the library count + Add/Scan-All actions; the
// list renders each Library with its kind icon, name, and per-row controls; the
// Add-Library wizard walks kind → name → path and creates; the Edit dialog
// renames, adds a folder, and deletes; Scan-All triggers every row's scan; and
// scan polling reflects running → idle + counts. apiClient is faked at the module
// boundary; scan polling uses fake timers.

const {
  listLibraries,
  createLibrary,
  updateLibrary,
  deleteLibrary,
  scanLibrary,
  getScanStatus,
} = vi.hoisted(() => ({
  listLibraries: vi.fn(),
  createLibrary: vi.fn(),
  updateLibrary: vi.fn(),
  deleteLibrary: vi.fn(),
  scanLibrary: vi.fn(),
  getScanStatus: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      createLibrary: (...a: unknown[]) => createLibrary(...a),
      updateLibrary: (...a: unknown[]) => updateLibrary(...a),
      deleteLibrary: (...a: unknown[]) => deleteLibrary(...a),
      scanLibrary: (...a: unknown[]) => scanLibrary(...a),
      getScanStatus: (...a: unknown[]) => getScanStatus(...a),
    },
  };
});

import AdminLibrariesScreen from "./AdminLibrariesScreen";

function lib(over: Partial<Library>): Library {
  return {
    id: "lib1",
    name: "Movies",
    kind: "movie",
    rootFolders: [{ id: "r1", path: "/media/movies" }],
    ...over,
  };
}
function status(over: Partial<ScanStatus>): ScanStatus {
  return { libraryId: "lib1", state: "idle", titlesFound: 0, filesFound: 0, ...over };
}

// HTMLDialogElement has no jsdom implementation for showModal/close; stub them so
// the dialogs mount without throwing.
beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) {
    this.open = true;
  });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.open = false;
    this.dispatchEvent(new Event("close"));
  });

  listLibraries.mockReset();
  createLibrary.mockReset();
  updateLibrary.mockReset();
  deleteLibrary.mockReset();
  scanLibrary.mockReset();
  getScanStatus.mockReset();
  // Each row reads its scan status on mount; default to a settled idle so rows
  // don't start polling unless a test opts in.
  getScanStatus.mockResolvedValue(status({ state: "idle" }));
});

describe("AdminLibrariesScreen", () => {
  it("renders the count and existing libraries with a kind icon + name", async () => {
    listLibraries.mockResolvedValue([
      lib({ id: "lib1", name: "Movies", kind: "movie" }),
    ]);
    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });

    await waitFor(() =>
      expect(screen.getByTestId("admin-library-list")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("admin-libraries-count")).toHaveTextContent("1 library");
    const row = screen.getByTestId("admin-library-row");
    expect(within(row).getByTestId("admin-library-name")).toHaveTextContent("Movies");
    // The kind icon renders inside the row's identity cluster.
    expect(row.querySelector(".admin-library-kind-icon")).toBeTruthy();
  });

  it("shows the call-to-action empty state with no libraries", async () => {
    listLibraries.mockResolvedValue([]);
    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("admin-libraries-empty")).toHaveTextContent(
      /No libraries configured/i,
    );
    expect(screen.getByTestId("admin-libraries-count")).toHaveTextContent("0 libraries");
    // Scan-All is disabled with nothing to scan.
    expect(screen.getByTestId("scan-all-button")).toBeDisabled();
  });

  it("adds a library through the wizard (kind → name → path → Add)", async () => {
    const user = userEvent.setup();
    listLibraries
      .mockResolvedValueOnce([])
      .mockResolvedValue([
        lib({ id: "libtv", name: "Shows", kind: "tv", rootFolders: [{ id: "r9", path: "/tv" }] }),
      ]);
    createLibrary.mockResolvedValue(lib({ id: "libtv", name: "Shows", kind: "tv" }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-library-button"));
    // Page 1: pick a kind, then Next.
    await user.click(screen.getByTestId("add-library-kind-tv"));
    await user.click(screen.getByTestId("add-library-next"));
    // Page 2: name, then Next.
    await user.type(screen.getByTestId("add-library-name-input"), "Shows");
    await user.click(screen.getByTestId("add-library-next"));
    // Page 3: path, then Add.
    await user.type(screen.getByTestId("add-library-path-input"), "/tv");
    await user.click(screen.getByTestId("add-library-submit"));

    await waitFor(() =>
      expect(createLibrary).toHaveBeenCalledWith({
        name: "Shows",
        kind: "tv",
        rootFolders: ["/tv"],
      }),
    );
    // Dialog closes and the new library appears after reload.
    await waitFor(() =>
      expect(screen.queryByTestId("add-library-dialog")).not.toBeInTheDocument(),
    );
    await waitFor(() => expect(screen.getByText("Shows")).toBeInTheDocument());
  });

  it("Next is gated until each wizard page is valid", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValue([]);
    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-library-button"));
    // No kind chosen yet → Next disabled.
    expect(screen.getByTestId("add-library-next")).toBeDisabled();
    await user.click(screen.getByTestId("add-library-kind-movie"));
    expect(screen.getByTestId("add-library-next")).toBeEnabled();
    await user.click(screen.getByTestId("add-library-next"));
    // Empty name → Next disabled.
    expect(screen.getByTestId("add-library-next")).toBeDisabled();
  });

  it("renders a readable inline overlap error in the wizard (no crash)", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValue([]);
    createLibrary.mockRejectedValue(
      new ApiError(409, "FOLDER_OVERLAP", "root /films overlaps library Movies"),
    );

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-library-button"));
    await user.click(screen.getByTestId("add-library-kind-movie"));
    await user.click(screen.getByTestId("add-library-next"));
    await user.type(screen.getByTestId("add-library-name-input"), "Films");
    await user.click(screen.getByTestId("add-library-next"));
    await user.type(screen.getByTestId("add-library-path-input"), "/films");
    await user.click(screen.getByTestId("add-library-submit"));

    const err = await screen.findByTestId("add-library-error");
    expect(err).toHaveTextContent(/overlaps/i);
    expect(err).toHaveAttribute("data-overlap", "true");
    // Dialog stays open (not crashed).
    expect(screen.getByTestId("add-library-dialog")).toBeInTheDocument();
  });

  it("edits a library: rename and add a folder both PATCH", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValue([
      lib({ id: "lib1", name: "Movies", rootFolders: [{ id: "r1", path: "/media/movies" }] }),
    ]);
    updateLibrary
      .mockResolvedValueOnce(lib({ id: "lib1", name: "Films" }))
      .mockResolvedValue(
        lib({
          id: "lib1",
          name: "Films",
          rootFolders: [
            { id: "r1", path: "/media/movies" },
            { id: "r2", path: "/media/films" },
          ],
        }),
      );

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-library-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("edit-library-button"));
    const dialog = await screen.findByTestId("edit-library-dialog");

    // Rename.
    const nameInput = within(dialog).getByTestId("edit-library-name-input");
    await user.clear(nameInput);
    await user.type(nameInput, "Films");
    await user.click(within(dialog).getByTestId("edit-library-save-name"));
    await waitFor(() =>
      expect(updateLibrary).toHaveBeenCalledWith("lib1", { name: "Films" }),
    );

    // Add a folder.
    await user.type(
      within(dialog).getByTestId("edit-library-add-folder-input"),
      "/media/films",
    );
    await user.click(within(dialog).getByTestId("edit-library-add-folder"));
    await waitFor(() =>
      expect(updateLibrary).toHaveBeenCalledWith("lib1", {
        addRootFolders: ["/media/films"],
      }),
    );
    // The new folder shows in the dialog's roots list.
    await waitFor(() =>
      expect(within(dialog).getByTestId("edit-library-roots")).toHaveTextContent(
        "/media/films",
      ),
    );
  });

  it("deletes a library from the edit dialog (confirm) and removes it", async () => {
    const user = userEvent.setup();
    listLibraries
      .mockResolvedValueOnce([lib({ id: "lib1", name: "Movies" })])
      .mockResolvedValue([]);
    deleteLibrary.mockResolvedValue(undefined);

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-library-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("edit-library-button"));
    const dialog = await screen.findByTestId("edit-library-dialog");
    // Two-click danger: reveal confirm, then confirm.
    await user.click(within(dialog).getByTestId("edit-library-delete"));
    await user.click(within(dialog).getByTestId("edit-library-delete-confirm"));

    await waitFor(() => expect(deleteLibrary).toHaveBeenCalledWith("lib1"));
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );
  });
});

describe("AdminLibrariesScreen scan controls (fake timers)", () => {
  // Under fake timers, Testing Library's waitFor can't be used (it relies on
  // real timers), so we flush microtasks explicitly after each fake tick and
  // give userEvent the timer-advance shim for its internal delays.
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  async function flush() {
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
  }

  it("incremental scan trigger sets running, then polling reflects idle + counts", async () => {
    listLibraries.mockResolvedValue([lib({ id: "lib1", name: "Movies" })]);
    getScanStatus
      .mockResolvedValueOnce(status({ state: "idle" }))
      .mockResolvedValue(status({ state: "idle", titlesFound: 4, filesFound: 6 }));
    scanLibrary.mockResolvedValue(status({ state: "running" }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await flush();
    expect(screen.getByTestId("scan-status")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("scan-button"));
    await flush();
    expect(scanLibrary).toHaveBeenCalledWith("lib1", { mode: "incremental" });
    expect(screen.getByTestId("scan-status")).toHaveAttribute("data-state", "running");
    expect(screen.getByTestId("scan-button")).toBeDisabled();
    expect(screen.getByTestId("full-scan-button")).toBeDisabled();

    await act(async () => {
      vi.advanceTimersByTime(1500);
    });
    await flush();
    expect(screen.getByTestId("scan-status")).toHaveAttribute("data-state", "idle");
    expect(screen.getByTestId("scan-titles-found")).toHaveTextContent("4");
    expect(screen.getByTestId("scan-files-found")).toHaveTextContent("6");
    expect(screen.getByTestId("scan-button")).toBeEnabled();
    expect(screen.getByTestId("full-scan-button")).toBeEnabled();
  });

  it("full scan hits the full endpoint", async () => {
    listLibraries.mockResolvedValue([lib({ id: "lib1", name: "Movies" })]);
    getScanStatus.mockResolvedValue(status({ state: "idle" }));
    scanLibrary.mockResolvedValue(status({ state: "idle", titlesFound: 1, filesFound: 1 }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await flush();
    expect(screen.getByTestId("full-scan-button")).toBeInTheDocument();

    fireEvent.click(screen.getByTestId("full-scan-button"));
    await flush();
    expect(scanLibrary).toHaveBeenCalledWith("lib1", { mode: "full" });
  });

  it("Scan All Libraries triggers an incremental scan on every row", async () => {
    listLibraries.mockResolvedValue([
      lib({ id: "lib1", name: "Movies" }),
      lib({ id: "lib2", name: "Shows", kind: "tv", rootFolders: [{ id: "r2", path: "/tv" }] }),
    ]);
    getScanStatus.mockResolvedValue(status({ state: "idle" }));
    scanLibrary.mockResolvedValue(status({ state: "running" }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await flush();
    expect(screen.getAllByTestId("admin-library-row")).toHaveLength(2);

    fireEvent.click(screen.getByTestId("scan-all-button"));
    await flush();
    expect(scanLibrary).toHaveBeenCalledWith("lib1", { mode: "incremental" });
    expect(scanLibrary).toHaveBeenCalledWith("lib2", { mode: "incremental" });
  });
});
