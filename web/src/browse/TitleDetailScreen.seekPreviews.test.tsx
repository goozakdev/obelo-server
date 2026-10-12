import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { TitleDetail } from "../api/types";

// "Generate seek previews now" (ADR-0070) on the Title detail's ⋯ menu: an Admin
// sees it on a local Movie or Episode and it POSTs for this Title; a Member, a
// mirrored Title, never sees it. A refusal (503, no ffmpeg) shows as the notice.

const { getTitle, listLibraries, generateTitleSeekPreviews } = vi.hoisted(
  () => ({
    getTitle: vi.fn(),
    listLibraries: vi.fn(),
    generateTitleSeekPreviews: vi.fn(),
  }),
);

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      generateTitleSeekPreviews: (...a: unknown[]) =>
        generateTitleSeekPreviews(...a),
    },
  };
});

import TitleDetailScreen from "./TitleDetailScreen";

const ADMIN = { id: "u1", username: "operator", role: "admin" };
const MEMBER = { id: "u2", username: "ada", role: "member" };

const detail = {
  id: "t1",
  libraryId: "lib-movies",
  kind: "movie",
  title: "Dune",
  year: 2021,
  needsReview: false,
  ambiguous: false,
  hidden: false,
  resumePositionMs: 0,
  watched: false,
  editions: [],
  artwork: [],
  subtitles: [],
  genres: [],
  cast: [],
  lockedFields: [],
} as unknown as TitleDetail;

function renderDetail(user: { id: string; username: string; role: string }) {
  return renderWithAuth(
    <Routes>
      <Route path="/titles/:titleId" element={<TitleDetailScreen />} />
    </Routes>,
    { initialEntries: ["/titles/t1"], user },
  );
}

beforeEach(() => {
  getTitle.mockReset().mockResolvedValue(detail);
  listLibraries.mockReset().mockResolvedValue([
    { id: "lib-movies", name: "Movies", kind: "movie", rootFolders: [] },
    {
      id: "lib-linked",
      name: "Friend films",
      kind: "movie",
      rootFolders: [],
      linked: true,
      available: true,
    },
  ]);
  generateTitleSeekPreviews.mockReset().mockResolvedValue(undefined);
});

describe("TitleDetailScreen — generate seek previews now", () => {
  it("lets an Admin queue generation for a Movie", async () => {
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("generate-seek-previews-item"));
    await waitFor(() =>
      expect(generateTitleSeekPreviews).toHaveBeenCalledWith("t1"),
    );
    expect(await screen.findByTestId("seek-previews-notice")).toHaveTextContent(
      /in the background/,
    );
  });

  it("offers it on an Episode too", async () => {
    getTitle.mockResolvedValue({ ...detail, kind: "episode" });
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(
      screen.getByTestId("generate-seek-previews-item"),
    ).toBeInTheDocument();
  });

  it("offers no such action on a Title that is not Movie or Episode video", async () => {
    getTitle.mockResolvedValue({ ...detail, kind: "other" });
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("generate-seek-previews-item")).toBeNull();
  });

  it("offers a Member no such action", async () => {
    const user = userEvent.setup();
    renderDetail(MEMBER);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("generate-seek-previews-item")).toBeNull();
  });

  it("offers no such action on a mirrored Title", async () => {
    getTitle.mockResolvedValue({ ...detail, libraryId: "lib-linked" });
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await waitFor(() => expect(listLibraries).toHaveBeenCalled());
    expect(screen.queryByTestId("generate-seek-previews-item")).toBeNull();
  });

  it("shows the server's refusal when there is no ffmpeg", async () => {
    generateTitleSeekPreviews.mockRejectedValue(
      new ApiError(503, "UNAVAILABLE", "no usable ffmpeg on this server"),
    );
    const user = userEvent.setup();
    renderDetail(ADMIN);
    await user.click(await screen.findByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("generate-seek-previews-item"));
    expect(await screen.findByTestId("seek-previews-notice")).toHaveTextContent(
      "no usable ffmpeg on this server",
    );
  });
});
