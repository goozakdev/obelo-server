import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import type { Library } from "../api/types";

// The Marker detection toggle (ADR-0065 §4) in a Library row's ⋮ menu: a TV
// Library shows it (read on first open, flipped in place); a music or movie
// Library, or a linked TV Library, has no such item at all; on a host without
// ffmpeg it shows as unavailable, not on.

const { getScanStatus, getMarkerDetection, setMarkerDetection } = vi.hoisted(() => ({
  getScanStatus: vi.fn(),
  getMarkerDetection: vi.fn(),
  setMarkerDetection: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getScanStatus: (...a: unknown[]) => getScanStatus(...a),
      getMarkerDetection: (...a: unknown[]) => getMarkerDetection(...a),
      setMarkerDetection: (...a: unknown[]) => setMarkerDetection(...a),
    },
  };
});

import LibraryAdminRow from "./LibraryAdminRow";

function lib(kind: string): Library {
  return { id: "lib1", name: "Shows", kind, rootFolders: [] } as Library;
}

function renderRow(library: Library) {
  return renderWithAuth(
    <ul>
      <LibraryAdminRow library={library} onEdit={() => {}} onDeleted={() => {}} />
    </ul>,
    { user: { id: "u1", username: "operator", role: "admin" } },
  );
}

beforeEach(() => {
  getScanStatus.mockReset().mockResolvedValue({
    libraryId: "lib1",
    state: "idle",
    titleCount: 0,
    titlesFound: 0,
    filesFound: 0,
  });
  getMarkerDetection.mockReset().mockResolvedValue({ enabled: true, available: true });
  setMarkerDetection.mockReset().mockResolvedValue({ enabled: false, available: true });
});

describe("LibraryAdminRow — marker detection toggle", () => {
  it("reads a TV Library's toggle and turns it off", async () => {
    const user = userEvent.setup();
    renderRow(lib("tv"));
    await user.click(screen.getByTestId("library-menu-toggle"));
    const item = await screen.findByText("Detect intros & credits: On");
    expect(item).toHaveAttribute("aria-checked", "true");
    await user.click(item);
    await waitFor(() => expect(setMarkerDetection).toHaveBeenCalledWith("lib1", false));
    await user.click(screen.getByTestId("library-menu-toggle"));
    expect(await screen.findByText("Detect intros & credits: Off")).toHaveAttribute("aria-checked", "false");
  });

  it("has no toggle at all on a music or movie Library", async () => {
    const user = userEvent.setup();
    for (const kind of ["music", "movie"]) {
      const { unmount } = renderRow(lib(kind));
      await user.click(screen.getByTestId("library-menu-toggle"));
      expect(screen.getByTestId("library-menu")).toBeInTheDocument();
      expect(screen.queryByTestId("marker-detection-button")).toBeNull();
      unmount();
    }
    expect(getMarkerDetection).not.toHaveBeenCalled();
  });

  it("has no toggle on a linked TV Library, whose toggle is the sharer's", async () => {
    const user = userEvent.setup();
    renderRow({ ...lib("tv"), linked: true, available: true, linkedServer: "Friend" });
    await user.click(screen.getByTestId("library-menu-toggle"));
    expect(screen.getByTestId("library-menu")).toBeInTheDocument();
    expect(screen.queryByTestId("marker-detection-button")).toBeNull();
    expect(getMarkerDetection).not.toHaveBeenCalled();
  });

  it("shows detection as unavailable, not on, when the server cannot run it", async () => {
    getMarkerDetection.mockResolvedValue({ enabled: true, available: false });
    const user = userEvent.setup();
    renderRow(lib("tv"));
    await user.click(screen.getByTestId("library-menu-toggle"));
    const item = await screen.findByText("Detect intros & credits: Unavailable");
    expect(item).toHaveAttribute("aria-checked", "false");
    expect(item).toBeDisabled();
  });

  it("closes the actions menu on an outside click and on Escape, not on an inside click", async () => {
    const user = userEvent.setup();
    renderRow(lib("tv"));
    const toggle = screen.getByTestId("library-menu-toggle");

    await user.click(toggle);
    await user.click(screen.getByTestId("library-menu"));
    expect(screen.getByTestId("library-menu")).toBeInTheDocument();

    await user.click(document.body);
    expect(screen.queryByTestId("library-menu")).toBeNull();

    await user.click(toggle);
    expect(screen.getByTestId("library-menu")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByTestId("library-menu")).toBeNull();
  });
});
