import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { Library, ScanStatus } from "../api/types";

// AdminLibrariesScreen end-to-end through the faked API client (the one seam):
// the list renders existing libraries; create succeeds and the new library
// appears; a FOLDER_OVERLAP create shows a readable inline error (no crash);
// delete removes a library; a scan trigger seeds "running" and polling reflects
// idle + counts; incremental vs full hit the right endpoint. apiClient is faked
// at the module boundary; scan polling uses fake timers.

const {
  listLibraries,
  createLibrary,
  deleteLibrary,
  scanLibrary,
  getScanStatus,
} = vi.hoisted(() => ({
  listLibraries: vi.fn(),
  createLibrary: vi.fn(),
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

beforeEach(() => {
  listLibraries.mockReset();
  createLibrary.mockReset();
  deleteLibrary.mockReset();
  scanLibrary.mockReset();
  getScanStatus.mockReset();
  // Each row reads its scan status on mount; default to a settled idle so rows
  // don't start polling unless a test opts in.
  getScanStatus.mockResolvedValue(status({ state: "idle" }));
});

describe("AdminLibrariesScreen", () => {
  it("renders existing libraries with their roots", async () => {
    listLibraries.mockResolvedValue([
      lib({ id: "lib1", name: "Movies", rootFolders: [{ id: "r1", path: "/media/movies" }] }),
    ]);
    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });

    await waitFor(() =>
      expect(screen.getByTestId("admin-library-list")).toBeInTheDocument(),
    );
    const row = screen.getByTestId("admin-library-row");
    expect(within(row).getByTestId("admin-library-name")).toHaveTextContent("Movies");
    expect(within(row).getByTestId("admin-library-roots")).toHaveTextContent(
      "/media/movies",
    );
  });

  it("shows a clean empty state with no libraries", async () => {
    listLibraries.mockResolvedValue([]);
    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );
  });

  it("creates a library and shows it after reload", async () => {
    const user = userEvent.setup();
    // First load: empty. After create: the new library is present.
    listLibraries
      .mockResolvedValueOnce([])
      .mockResolvedValue([lib({ id: "lib9", name: "Films", rootFolders: [{ id: "r9", path: "/films" }] })]);
    createLibrary.mockResolvedValue(lib({ id: "lib9", name: "Films" }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("library-name-input"), "Films");
    await user.type(screen.getByTestId("root-folder-input"), "/films");
    await user.click(screen.getByTestId("create-library-submit"));

    await waitFor(() =>
      expect(createLibrary).toHaveBeenCalledWith({
        name: "Films",
        kind: "movie",
        rootFolders: ["/films"],
      }),
    );
    await waitFor(() => expect(screen.getByText("Films")).toBeInTheDocument());
  });

  it("creates a TV library when the kind is selected", async () => {
    const user = userEvent.setup();
    listLibraries
      .mockResolvedValueOnce([])
      .mockResolvedValue([lib({ id: "libtv", name: "Shows", kind: "tv" })]);
    createLibrary.mockResolvedValue(lib({ id: "libtv", name: "Shows", kind: "tv" }));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("library-name-input"), "Shows");
    await user.selectOptions(screen.getByTestId("library-kind-select"), "tv");
    await user.type(screen.getByTestId("root-folder-input"), "/tv");
    await user.click(screen.getByTestId("create-library-submit"));

    await waitFor(() =>
      expect(createLibrary).toHaveBeenCalledWith({
        name: "Shows",
        kind: "tv",
        rootFolders: ["/tv"],
      }),
    );
  });

  it("renders a readable inline error on FOLDER_OVERLAP (no crash)", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValue([]);
    createLibrary.mockImplementation(() =>
      Promise.reject(
        new ApiError(409, "FOLDER_OVERLAP", "root /films overlaps library Movies"),
      ),
    );

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("library-name-input"), "Films");
    await user.type(screen.getByTestId("root-folder-input"), "/films");
    await user.click(screen.getByTestId("create-library-submit"));

    const err = await screen.findByTestId("create-library-error");
    expect(err).toHaveTextContent(/overlaps/i);
    expect(err).toHaveAttribute("data-overlap", "true");
    // Still on the form, not crashed.
    expect(screen.getByTestId("create-library-form")).toBeInTheDocument();
  });

  it("supports adding and removing root-folder inputs and submits all roots", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValueOnce([]).mockResolvedValue([lib({})]);
    createLibrary.mockResolvedValue(lib({}));

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-libraries-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("library-name-input"), "Movies");
    await user.click(screen.getByTestId("add-root-button"));
    const inputs = screen.getAllByTestId("root-folder-input");
    expect(inputs).toHaveLength(2);
    await user.type(inputs[0], "/a");
    await user.type(inputs[1], "/b");
    await user.click(screen.getByTestId("create-library-submit"));

    await waitFor(() =>
      expect(createLibrary).toHaveBeenCalledWith({
        name: "Movies",
        kind: "movie",
        rootFolders: ["/a", "/b"],
      }),
    );
  });

  it("deletes a library and removes it from the list", async () => {
    const user = userEvent.setup();
    listLibraries
      .mockResolvedValueOnce([lib({ id: "lib1", name: "Movies" })])
      .mockResolvedValue([]);
    deleteLibrary.mockResolvedValue(undefined);

    renderWithAuth(<AdminLibrariesScreen />, { initialEntries: ["/admin"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-library-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-library-button"));
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
    // Mount read settles idle (no polling); the trigger returns running; the
    // poll then returns idle with counts.
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
    // begin(running) flips the indicator to running.
    expect(screen.getByTestId("scan-status")).toHaveAttribute("data-state", "running");
    // The async trigger returns immediately, so the controls must stay disabled
    // for the whole running scan — a second click can't start a concurrent scan.
    expect(screen.getByTestId("scan-button")).toBeDisabled();
    expect(screen.getByTestId("full-scan-button")).toBeDisabled();

    // Advance one poll interval → idle + counts.
    await act(async () => {
      vi.advanceTimersByTime(1500);
    });
    await flush();
    expect(screen.getByTestId("scan-status")).toHaveAttribute("data-state", "idle");
    expect(screen.getByTestId("scan-titles-found")).toHaveTextContent("4");
    expect(screen.getByTestId("scan-files-found")).toHaveTextContent("6");
    // Settled → controls re-enabled.
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
});
