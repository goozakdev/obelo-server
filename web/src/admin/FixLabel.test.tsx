import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ArtworkCandidate } from "../api/types";
import { ArtworkPicker } from "./FixLabel";

// The reworked ArtworkPicker (artwork-management/01): it is the body of a dedicated
// per-role artwork tab, so it AUTO-SEARCHES on mount (no "Choose image" pre-click)
// and applies + Locks on click. These props-only component tests drive it against
// faked apiClient callbacks — the same seam the detail screens wire it through.

const CANDIDATES: ArtworkCandidate[] = [
  { url: "https://img/one.jpg", width: 1000, height: 1500, source: "tmdb" },
  { url: "https://img/two.jpg", width: 680, height: 1000, source: "tmdb" },
];

let listCandidates: ReturnType<typeof vi.fn>;
let pick: ReturnType<typeof vi.fn>;
let release: ReturnType<typeof vi.fn>;
let upload: ReturnType<typeof vi.fn>;

beforeEach(() => {
  listCandidates = vi.fn().mockResolvedValue(CANDIDATES);
  pick = vi.fn().mockResolvedValue(undefined);
  release = vi.fn().mockResolvedValue(undefined);
  upload = vi.fn().mockResolvedValue(undefined);
});

function renderPicker(overrides: Partial<Parameters<typeof ArtworkPicker>[0]> = {}) {
  return render(
    <ArtworkPicker
      role="poster"
      label="Poster"
      locked={false}
      listCandidates={listCandidates}
      pick={pick}
      release={release}
      upload={upload}
      {...overrides}
    />,
  );
}

describe("ArtworkPicker (auto-search on open)", () => {
  it("auto-loads candidates on mount with no 'Choose image' pre-click, showing dimensions", async () => {
    renderPicker();

    // No pre-click affordance — the old "Choose image" toggle is gone.
    expect(screen.queryByTestId("choose-artwork-poster")).not.toBeInTheDocument();

    // A loading state shows while the provider is queried, then one thumbnail per
    // candidate appears — mounting IS the search.
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());
    expect(listCandidates).toHaveBeenCalledWith("poster");
    const choices = screen.getAllByTestId("artwork-choice");
    expect(choices).toHaveLength(2);
    // Dimensions are shown so a higher-res image is easy to prefer.
    expect(screen.getAllByTestId("artwork-dims")[0]).toHaveTextContent("1000×1500");
  });

  it("applies a candidate on click (set + Lock) and marks it Applied", async () => {
    const user = userEvent.setup();
    renderPicker();
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    const second = screen.getAllByTestId("artwork-choice")[1];
    await user.click(second);

    expect(pick).toHaveBeenCalledWith("poster", "https://img/two.jpg");
    // The picked candidate reads as "Applied" without re-querying the provider.
    await waitFor(() => expect(within(second).getByText("Applied")).toBeInTheDocument());
    expect(listCandidates).toHaveBeenCalledTimes(1);
  });

  it("surfaces a pick failure and leaves the grid intact", async () => {
    const user = userEvent.setup();
    pick.mockRejectedValue(new Error("nope"));
    renderPicker();
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    await user.click(screen.getAllByTestId("artwork-choice")[0]);

    await waitFor(() => expect(screen.getByTestId("artwork-error-poster")).toHaveTextContent("nope"));
    // No "Applied" ribbon on a failed pick.
    expect(screen.queryByText("Applied")).not.toBeInTheDocument();
  });

  it("shows a clear, non-blocking message when the provider offers nothing", async () => {
    listCandidates.mockResolvedValue([]);
    renderPicker();

    await waitFor(() => expect(screen.getByTestId("artwork-none-poster")).toBeInTheDocument());
    expect(screen.queryByTestId("artwork-grid-poster")).not.toBeInTheDocument();
  });

  it("shows a graceful error message when the provider is unreachable", async () => {
    listCandidates.mockRejectedValue(new Error("provider unreachable"));
    renderPicker();

    await waitFor(() =>
      expect(screen.getByTestId("artwork-error-poster")).toHaveTextContent("provider unreachable"),
    );
    // A failed load is non-blocking: no empty-grid claim, no crash.
    expect(screen.queryByTestId("artwork-none-poster")).not.toBeInTheDocument();
  });

  it("releases the role's lock via the Locked badge", async () => {
    const user = userEvent.setup();
    renderPicker({ locked: true });
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    await user.click(screen.getByTestId("release-poster"));

    expect(release).toHaveBeenCalledWith("poster");
  });
});

describe("ArtworkPicker (upload your own — upload is selecting)", () => {
  it("uploads a file chosen via Browse (the multipart client method)", async () => {
    const user = userEvent.setup();
    renderPicker();
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    const file = new File([new Uint8Array([0xff, 0xd8, 0xff])], "poster.jpg", { type: "image/jpeg" });
    await user.upload(screen.getByTestId("artwork-file-poster"), file);

    expect(upload).toHaveBeenCalledTimes(1);
    expect(upload.mock.calls[0][0]).toBe("poster");
    expect(upload.mock.calls[0][1]).toBe(file);
    // No error on a clean upload — the caller's refetch reloads the image.
    expect(screen.queryByTestId("artwork-error-poster")).not.toBeInTheDocument();
  });

  it("uploads a file dropped onto the drop-zone", async () => {
    renderPicker();
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    const file = new File([new Uint8Array([0x89, 0x50, 0x4e, 0x47])], "poster.png", { type: "image/png" });
    const zone = screen.getByTestId("artwork-upload-poster");
    fireEvent.drop(zone, { dataTransfer: { files: [file] } });

    await waitFor(() => expect(upload).toHaveBeenCalledTimes(1));
    expect(upload.mock.calls[0][0]).toBe("poster");
    expect(upload.mock.calls[0][1]).toBe(file);
  });

  it("surfaces a server-rejected upload's error and leaves the image unchanged", async () => {
    const user = userEvent.setup();
    // The server sniffs the bytes and rejects a disguised/oversize file; the picker
    // surfaces that error and applies no change (the drop-zone accept only filters
    // the file dialog — the server is the authority).
    upload.mockRejectedValue(new Error("unsupported image type — use JPEG, PNG, or WebP"));
    renderPicker();
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());

    const file = new File([new Uint8Array([0xff, 0xd8, 0xff])], "poster.jpg", { type: "image/jpeg" });
    await user.upload(screen.getByTestId("artwork-file-poster"), file);

    await waitFor(() =>
      expect(screen.getByTestId("artwork-error-poster")).toHaveTextContent("unsupported image type"),
    );
    // No "Applied" ribbon appears on a rejected upload.
    expect(screen.queryByText("Applied")).not.toBeInTheDocument();
  });

  it("offers the upload affordance even when the provider grid is empty (upload-only)", async () => {
    listCandidates.mockResolvedValue([]);
    renderPicker();

    // The Artist-Photo/offline case: no candidate grid, but upload still works.
    await waitFor(() => expect(screen.getByTestId("artwork-none-poster")).toBeInTheDocument());
    expect(screen.getByTestId("artwork-upload-poster")).toBeInTheDocument();
    expect(screen.getByTestId("artwork-browse-poster")).toBeInTheDocument();
  });
});
