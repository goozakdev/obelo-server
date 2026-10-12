import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { Library, SeekPreviewStatus } from "../api/types";

// The Seek preview controls (ADR-0070) on a Library row: a toggle, a coverage
// line, a collapsible failed list with Retry / Retry all, and a Delete that
// confirms it also turns generation off (DECISIONS 24). Movie and TV Libraries
// only; a music Library, a linked mirror, or a 404 shows nothing.

const {
  getScanStatus,
  getSeekPreviews,
  setSeekPreviews,
  deleteSeekPreviews,
  retrySeekPreviews,
} = vi.hoisted(() => ({
  getScanStatus: vi.fn(),
  getSeekPreviews: vi.fn(),
  setSeekPreviews: vi.fn(),
  deleteSeekPreviews: vi.fn(),
  retrySeekPreviews: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getScanStatus: (...a: unknown[]) => getScanStatus(...a),
      getSeekPreviews: (...a: unknown[]) => getSeekPreviews(...a),
      setSeekPreviews: (...a: unknown[]) => setSeekPreviews(...a),
      deleteSeekPreviews: (...a: unknown[]) => deleteSeekPreviews(...a),
      retrySeekPreviews: (...a: unknown[]) => retrySeekPreviews(...a),
    },
  };
});

import LibraryAdminRow from "./LibraryAdminRow";

function lib(kind: string): Library {
  return { id: "lib1", name: "Films", kind, rootFolders: [] } as Library;
}

function renderRow(library: Library) {
  return renderWithAuth(
    <ul>
      <LibraryAdminRow
        library={library}
        onEdit={() => {}}
        onDeleted={() => {}}
      />
    </ul>,
    { user: { id: "u1", username: "operator", role: "admin" } },
  );
}

const failure = {
  fileId: "f1",
  titleId: "t1",
  label: "Dune (2021)",
  path: "/m/Dune.mkv",
  reason: "ffmpeg lacks zscale; cannot tone-map HDR",
};
const status = (over: Partial<SeekPreviewStatus> = {}): SeekPreviewStatus => ({
  enabled: true,
  available: true,
  eligible: 10,
  ready: 7,
  failed: [],
  ...over,
});

beforeEach(() => {
  getScanStatus.mockReset().mockResolvedValue({
    libraryId: "lib1",
    state: "idle",
    titleCount: 0,
    titlesFound: 0,
    filesFound: 0,
  });
  getSeekPreviews.mockReset().mockResolvedValue(status());
  setSeekPreviews.mockReset().mockResolvedValue(status({ enabled: false }));
  deleteSeekPreviews
    .mockReset()
    .mockResolvedValue(status({ enabled: false, ready: 0 }));
  retrySeekPreviews.mockReset().mockResolvedValue(undefined);
});

describe("LibraryAdminRow — seek previews", () => {
  it("shows the toggle and coverage line for a movie and a TV Library", async () => {
    for (const kind of ["movie", "tv"]) {
      const { unmount } = renderRow(lib(kind));
      expect(
        await screen.findByTestId("seek-previews-coverage"),
      ).toHaveTextContent("7 of 10 Files have seek previews");
      expect(screen.getByTestId("seek-previews-toggle")).toBeChecked();
      expect(getSeekPreviews).toHaveBeenLastCalledWith(
        "lib1",
        expect.anything(),
      );
      unmount();
    }
  });

  it("shows nothing on a music Library or a linked mirror, and does not ask the server", async () => {
    const music = renderRow(lib("music"));
    music.unmount();
    renderRow({
      ...lib("movie"),
      linked: true,
      available: true,
      linkedServer: "Friend",
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId("seek-previews")).toBeNull();
    expect(getSeekPreviews).not.toHaveBeenCalled();
  });

  it("shows nothing when the server answers 404", async () => {
    getSeekPreviews.mockRejectedValue(
      new ApiError(404, "NOT_FOUND", "this library has no seek previews"),
    );
    renderRow(lib("movie"));
    await waitFor(() => expect(getSeekPreviews).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByTestId("seek-previews")).toBeNull();
  });

  it("flips the toggle through the client and shows the saved state", async () => {
    const user = userEvent.setup();
    renderRow(lib("movie"));
    const toggle = await screen.findByTestId("seek-previews-toggle");
    getSeekPreviews.mockResolvedValue(status({ enabled: false }));
    await user.click(toggle);
    await waitFor(() =>
      expect(setSeekPreviews).toHaveBeenCalledWith("lib1", false),
    );
    await waitFor(() =>
      expect(screen.getByTestId("seek-previews-toggle")).not.toBeChecked(),
    );
  });

  it("shows generation as unavailable, not on, when the server cannot run it", async () => {
    getSeekPreviews.mockResolvedValue(status({ available: false }));
    renderRow(lib("movie"));
    const toggle = await screen.findByTestId("seek-previews-toggle");
    expect(toggle).not.toBeChecked();
    expect(toggle).toBeDisabled();
    expect(screen.getByTestId("seek-previews")).toHaveTextContent(
      /Unavailable/,
    );
  });

  it("says nothing about failures when none failed", async () => {
    renderRow(lib("movie"));
    const line = await screen.findByTestId("seek-previews-coverage");
    expect(line.textContent).toBe("7 of 10 Files have seek previews");
    expect(screen.queryByTestId("seek-previews-failed-toggle")).toBeNull();
  });

  it("does not delete when the confirm is cancelled", async () => {
    const user = userEvent.setup();
    renderRow(lib("movie"));
    await user.click(await screen.findByTestId("seek-previews-delete"));
    await user.click(await screen.findByTestId("confirm-dialog-cancel"));
    expect(deleteSeekPreviews).not.toHaveBeenCalled();
    expect(screen.queryByTestId("confirm-dialog")).toBeNull();
    expect(screen.getByTestId("seek-previews-toggle")).toBeChecked();
    expect(screen.getByTestId("seek-previews-coverage").textContent).toBe(
      "7 of 10 Files have seek previews",
    );
  });

  it("adds the failed count to the coverage line and lists failures in a collapsible", async () => {
    getSeekPreviews.mockResolvedValue(
      status({
        ready: 8,
        failed: [failure, { ...failure, fileId: "f2", label: "Heat (1995)" }],
      }),
    );
    const user = userEvent.setup();
    renderRow(lib("movie"));
    expect(
      await screen.findByTestId("seek-previews-coverage"),
    ).toHaveTextContent("8 of 10 Files have seek previews · 2 failed");
    expect(screen.queryByTestId("seek-previews-failed-list")).toBeNull();
    await user.click(screen.getByTestId("seek-previews-failed-toggle"));
    const list = screen.getByTestId("seek-previews-failed-list");
    expect(within(list).getByText("Dune (2021)")).toBeInTheDocument();
    expect(
      within(list).getAllByText("ffmpeg lacks zscale; cannot tone-map HDR"),
    ).toHaveLength(2);
    expect(within(list).getAllByText("/m/Dune.mkv")).toHaveLength(2);
    expect(
      within(list).getAllByRole("button", { name: /^Retry (Dune|Heat)/ }),
    ).toHaveLength(2);
  });

  it("retries one File, or all, and refreshes the status after", async () => {
    getSeekPreviews.mockResolvedValue(status({ failed: [failure] }));
    const user = userEvent.setup();
    renderRow(lib("movie"));
    await user.click(await screen.findByTestId("seek-previews-failed-toggle"));
    const before = getSeekPreviews.mock.calls.length;
    await user.click(screen.getByTestId("seek-previews-retry-f1"));
    await waitFor(() =>
      expect(retrySeekPreviews).toHaveBeenCalledWith("lib1", "f1"),
    );
    await waitFor(() =>
      expect(getSeekPreviews.mock.calls.length).toBeGreaterThan(before),
    );

    await user.click(screen.getByTestId("seek-previews-retry-all"));
    await waitFor(() =>
      expect(retrySeekPreviews).toHaveBeenLastCalledWith("lib1", undefined),
    );
  });

  it("confirms before deleting, says it turns generation off, then deletes", async () => {
    const user = userEvent.setup();
    renderRow(lib("movie"));
    await user.click(await screen.findByTestId("seek-previews-delete"));
    expect(deleteSeekPreviews).not.toHaveBeenCalled();
    const message = await screen.findByTestId("confirm-dialog-message");
    expect(message).toHaveTextContent(/deletes every seek preview of “Films”/);
    expect(message).toHaveTextContent(/turns generation off/);
    await user.click(screen.getByTestId("confirm-dialog-confirm"));
    await waitFor(() =>
      expect(deleteSeekPreviews).toHaveBeenCalledWith("lib1"),
    );
    await waitFor(() =>
      expect(screen.getByTestId("seek-previews-toggle")).not.toBeChecked(),
    );
    expect(screen.getByTestId("seek-previews-coverage")).toHaveTextContent(
      "0 of 10 Files have seek previews",
    );
  });

  it("shows a refused action's message on the row", async () => {
    retrySeekPreviews.mockRejectedValue(
      new ApiError(503, "UNAVAILABLE", "no ffmpeg on this server"),
    );
    getSeekPreviews.mockResolvedValue(status({ failed: [failure] }));
    const user = userEvent.setup();
    renderRow(lib("tv"));
    await user.click(await screen.findByTestId("seek-previews-failed-toggle"));
    await user.click(screen.getByTestId("seek-previews-retry-all"));
    expect(await screen.findByTestId("seek-previews-error")).toHaveTextContent(
      "no ffmpeg on this server",
    );
  });
});
