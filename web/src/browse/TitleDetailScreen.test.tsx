import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { useQueue } from "../player/queue/useQueue";
import type { TitleDetail } from "../api/types";
import { ApiError } from "../api/errors";

// The detail SCREEN against a faked apiClient: summary/year/added-date,
// Editions → Files (quality/version info), the watch-state indicator, the
// (now-playing-bar/01) Play affordance — which builds the Queue and starts
// playback in the persistent bar with NO navigation — the manual watched toggle,
// and the error-envelope path.

const {
  getTitle,
  setWatchState,
  listCollections,
  createCollection,
  addCollectionItems,
  addToWatchlist,
  searchTitleArtworkCandidates,
  pickTitleArtwork,
  releaseLock,
  listLibraries,
} = vi.hoisted(() => ({
  getTitle: vi.fn(),
  setWatchState: vi.fn(),
  listCollections: vi.fn(),
  createCollection: vi.fn(),
  addCollectionItems: vi.fn(),
  addToWatchlist: vi.fn(),
  searchTitleArtworkCandidates: vi.fn(),
  pickTitleArtwork: vi.fn(),
  releaseLock: vi.fn(),
  listLibraries: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
      setWatchState: (...a: unknown[]) => setWatchState(...a),
      listCollections: (...a: unknown[]) => listCollections(...a),
      createCollection: (...a: unknown[]) => createCollection(...a),
      addCollectionItems: (...a: unknown[]) => addCollectionItems(...a),
      addToWatchlist: (...a: unknown[]) => addToWatchlist(...a),
      searchTitleArtworkCandidates: (...a: unknown[]) => searchTitleArtworkCandidates(...a),
      pickTitleArtwork: (...a: unknown[]) => pickTitleArtwork(...a),
      releaseLock: (...a: unknown[]) => releaseLock(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
    },
  };
});

import TitleDetailScreen from "./TitleDetailScreen";

// A probe over the shared Queue so tests assert the Play affordance built the
// Queue (playback now begins in the persistent Now Playing bar — no navigation).
function QueueProbe() {
  const q = useQueue();
  return <div data-testid="queue-current">{q.current?.title.id ?? ""}</div>;
}

const MEMBER = { id: "u2", username: "ada", role: "member" };

function renderDetail(user?: { id: string; username: string; role: string }) {
  return renderWithAuth(
    <Routes>
      <Route
        path="/titles/:titleId"
        element={
          <>
            <TitleDetailScreen />
            <QueueProbe />
          </>
        }
      />
    </Routes>,
    { initialEntries: ["/titles/t1"], ...(user ? { user } : {}) },
  );
}

beforeEach(() => {
  getTitle.mockReset();
  setWatchState.mockReset();
  listCollections.mockReset();
  createCollection.mockReset();
  addCollectionItems.mockReset();
  addToWatchlist.mockReset();
  searchTitleArtworkCandidates.mockReset();
  pickTitleArtwork.mockReset();
  releaseLock.mockReset();
  // The Add-to-collection picker (Admin only) lazily lists Collections on open;
  // default it empty so unrelated tests don't trip on it.
  listCollections.mockResolvedValue([]);
  // Artwork tabs auto-search on open; default the provider empty so tests that
  // merely assert the tab set don't trip on a pending query.
  searchTitleArtworkCandidates.mockResolvedValue([]);
  // The parent "Back" link names a Movie's owning Library from this app-wide list.
  listLibraries.mockResolvedValue([
    { id: "lib-movies", name: "Movies", kind: "movie", rootFolders: [] },
  ]);
});

const detail: TitleDetail = {
  id: "t1",
  libraryId: "lib-movies",
  kind: "movie",
  title: "Dune",
  year: 2021,
  needsReview: false,
  ambiguous: false,
  hidden: false,
  resumePositionMs: 42000,
  watched: false,
  addedAt: "2021-10-22T08:30:00Z",
  editions: [
    {
      id: "e1",
      name: "1080p",
      files: [
        {
          id: "f1",
          path: "/m/Dune.mp4",
          container: "mp4",
          videoCodec: "h264",
          audioCodec: "aac",
          width: 1920,
          height: 1080,
          bitrate: 6_000_000,
          durationMs: 9480000,
          sizeBytes: 0,
          missing: false,
          streams: [],
          audioStreams: [],
        },
      ],
    },
  ],
  artwork: [],
  subtitles: [],
  overview: "",
  tagline: "",
  contentRating: "",
  releaseDate: "",
  runtimeMinutes: 0,
  studio: "",
  genres: [],
  cast: [],
  enrichmentStatus: "",
  lockedFields: [],
  displayTitle: "",
};

// The parent "Back" link returns the item to its parent (not browser history) and
// names the destination: a Movie to its owning Library, an Episode to its Show.
describe("TitleDetailScreen — parent Back link", () => {
  it("returns a Movie to its owning Library, named", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail();

    const back = await screen.findByTestId("back-link");
    expect(back).toHaveAttribute("href", "/libraries/lib-movies");
    // The label resolves to the Library's name once the app-wide list loads.
    await waitFor(() => expect(back).toHaveTextContent("Movies"));
  });

  it("returns an Episode to its Show, named", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      id: "ep1",
      kind: "episode",
      title: "System Down",
      episode: {
        showId: "show9",
        showTitle: "The Bear",
        seasonId: "s1",
        seasonNumber: 1,
        episodeNumber: 3,
      },
    });
    renderDetail();

    const back = await screen.findByTestId("back-link");
    expect(back).toHaveAttribute("href", "/shows/show9");
    expect(back).toHaveTextContent("The Bear");
  });
});

describe("TitleDetailScreen", () => {
  it("renders summary/year/added-date, editions+files, watch state, and a disabled Play", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail();

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.getByTestId("detail-title")).toHaveTextContent("Dune");
    expect(screen.getByTestId("detail-year")).toHaveTextContent("2021");
    // RFC3339 → local date (year is locale/zone-stable).
    expect(screen.getByTestId("detail-added")).toHaveTextContent("2021");

    // Edition + file quality/version info.
    expect(screen.getByTestId("edition-name")).toHaveTextContent("1080p");
    expect(screen.getByTestId("file-container")).toHaveTextContent("MP4");
    expect(screen.getByTestId("file-quality")).toHaveTextContent("1080p");
    expect(screen.getByTestId("file-codecs")).toHaveTextContent("h264");

    // Watch state: a resume marker (not watched) → "Resume at 0:42".
    expect(screen.getByTestId("watch-resume")).toHaveTextContent("0:42");

    // Play affordance is now wired (issue 04): enabled (a playable file exists)
    // and labelled "Resume" because there's a resume position.
    const play = screen.getByTestId("play-button");
    expect(play).toBeEnabled();
    expect(play).toHaveTextContent("Resume");
  });

  it("lists the available subtitle tracks in the Editions & files section", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      subtitles: [
        { id: "s1", source: "embedded", kind: "text", language: "en", forced: false, label: "English" },
        { id: "s2", source: "sidecar", kind: "text", language: "es", forced: true, label: "Spanish (Forced)" },
        { id: "s3", source: "embedded", kind: "image", language: "de", forced: false, label: "German" },
        { id: "s4", source: "fetched", kind: "text", language: "fr", forced: false, label: "French" },
      ],
    });
    renderDetail();

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    const summary = screen.getByTestId("detail-subtitles");
    expect(summary).toHaveTextContent("English");
    expect(summary).toHaveTextContent("Spanish (Forced)");
    const chips = screen.getAllByTestId("detail-subtitle");
    expect(chips).toHaveLength(4);
    // An image track is tagged (it burns in); a fetched track is tagged "online".
    const german = chips.find((c) => c.getAttribute("data-sub-lang") === "de")!;
    expect(german).toHaveTextContent("image");
    const french = chips.find((c) => c.getAttribute("data-sub-source") === "fetched")!;
    expect(french).toHaveTextContent("online");
  });

  it("omits the subtitles summary when the Title has none", async () => {
    getTitle.mockResolvedValue({ ...detail, subtitles: [] });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("detail-subtitles")).toBeNull();
  });

  it("lists a File's embedded audio Streams as labeled chips", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      editions: [
        {
          ...detail.editions[0],
          files: [
            {
              ...detail.editions[0].files[0],
              audioStreams: [
                { id: "a1", index: 1, codec: "aac", language: "en", channels: 2, layout: "Stereo", isDefault: true, label: "English Stereo" },
                { id: "a2", index: 2, codec: "ac3", language: "ja", channels: 6, layout: "5.1", isDefault: false, label: "Japanese 5.1" },
                { id: "a3", index: 3, codec: "dts", language: "en", channels: 2, layout: "Stereo", isDefault: false, commentary: true, label: "English Director's Commentary" },
                { id: "a4", index: 4, codec: "aac", language: "", channels: 1, layout: "Mono", isDefault: false, label: "Unknown Mono" },
              ],
            },
          ],
        },
      ],
    });
    renderDetail();

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    const chips = screen.getAllByTestId("audio-stream");
    expect(chips).toHaveLength(4);
    // Familiar layouts and the commentary/Unknown labels render as-is.
    expect(screen.getByTestId("file-audio")).toHaveTextContent("English Stereo");
    expect(screen.getByTestId("file-audio")).toHaveTextContent("Japanese 5.1");
    expect(screen.getByTestId("file-audio")).toHaveTextContent("English Director's Commentary");
    expect(screen.getByTestId("file-audio")).toHaveTextContent("Unknown Mono");
    // The default Stream is marked; the commentary Stream is tagged.
    const def = chips.find((c) => c.getAttribute("data-audio-default") === "1")!;
    expect(def).toHaveTextContent("default");
    expect(def.getAttribute("data-audio-lang")).toBe("en");
    const commentary = chips.find((c) => c.getAttribute("data-audio-codec") === "dts")!;
    expect(commentary).toHaveTextContent("commentary");
  });

  it("shows no audio chips when a File has no audio Streams", async () => {
    getTitle.mockResolvedValue(detail); // fixture file carries audioStreams: []
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("file-audio")).toBeNull();
  });

  it("lists a File's selectable video Streams as labeled chips when it has ≥2", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      editions: [
        {
          ...detail.editions[0],
          files: [
            {
              ...detail.editions[0].files[0],
              videoStreams: [
                { id: "v0", index: 0, codec: "h264", width: 1920, height: 1080, isDefault: true, label: "1080p" },
                { id: "v1", index: 1, codec: "h264", width: 1280, height: 720, isDefault: false, label: "720p" },
              ],
            },
          ],
        },
      ],
    });
    renderDetail();

    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    const chips = screen.getAllByTestId("video-stream");
    expect(chips).toHaveLength(2);
    expect(screen.getByTestId("file-video")).toHaveTextContent("1080p");
    expect(screen.getByTestId("file-video")).toHaveTextContent("720p");
    // The default (container-disposition) Stream is marked.
    const def = chips.find((c) => c.getAttribute("data-video-default") === "1")!;
    expect(def).toHaveTextContent("default");
    expect(def.getAttribute("data-video-codec")).toBe("h264");
  });

  it("shows no video chips when a File has a single video Stream", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      editions: [
        {
          ...detail.editions[0],
          files: [
            {
              ...detail.editions[0].files[0],
              videoStreams: [
                { id: "v0", index: 0, codec: "h264", width: 1920, height: 1080, isDefault: true, label: "1080p" },
              ],
            },
          ],
        },
      ],
    });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("file-video")).toBeNull();
  });

  it("shows a watched badge when the Title is watched", async () => {
    getTitle.mockResolvedValue({ ...detail, watched: true, resumePositionMs: 0 });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("watch-watched")).toBeInTheDocument());
    expect(screen.queryByTestId("watch-resume")).toBeNull();
    expect(screen.getByTestId("play-button")).toHaveTextContent("Play");
  });

  it("Play builds the Queue (playback starts in the bar, no navigation)", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    await userEvent.click(screen.getByTestId("play-button"));
    // A single-entry Queue of this Movie becomes the now-playing entry — the
    // persistent bar plays it, and we stayed on the detail screen (no route change).
    await waitFor(() =>
      expect(screen.getByTestId("queue-current")).toHaveTextContent("t1"),
    );
    expect(screen.getByTestId("detail")).toBeInTheDocument();
  });

  it("disables Play when no playable file exists", async () => {
    getTitle.mockResolvedValue({ ...detail, editions: [] });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.getByTestId("play-button")).toBeDisabled();
  });

  it("manual watched toggle calls setWatchState and reflects the server result", async () => {
    getTitle.mockResolvedValue({ ...detail, watched: false, resumePositionMs: 42000 });
    // Server applies the manual override and returns watched + cleared resume.
    setWatchState.mockResolvedValue({
      titleId: "t1",
      watched: true,
      resumePositionMs: 0,
    });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    // Starts unwatched (with a resume marker); the toggle icon's label reads
    // "Mark as watched".
    expect(screen.getByTestId("watch-resume")).toBeInTheDocument();
    const toggle = screen.getByTestId("watch-toggle");
    expect(toggle).toHaveAttribute("aria-label", "Mark as watched");

    await userEvent.click(toggle);

    // After the server confirms, the badge flips to Watched, the resume marker
    // is gone, and the toggle icon now offers "Mark as unwatched".
    await waitFor(() => expect(screen.getByTestId("watch-watched")).toBeInTheDocument());
    expect(setWatchState).toHaveBeenCalledWith("t1", true);
    expect(screen.queryByTestId("watch-resume")).toBeNull();
    expect(screen.getByTestId("watch-toggle")).toHaveAttribute(
      "aria-label",
      "Mark as unwatched",
    );
  });

  it("surfaces a watch-toggle error without losing the page", async () => {
    getTitle.mockResolvedValue(detail);
    setWatchState.mockImplementation(() =>
      Promise.reject(new ApiError(500, "INTERNAL", "failed to set watch state")),
    );
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    await userEvent.click(screen.getByTestId("watch-toggle"));
    await waitFor(() =>
      expect(screen.getByTestId("watch-toggle-error")).toHaveTextContent(
        "failed to set watch state",
      ),
    );
  });

  it("surfaces an API error from the envelope as a readable message", async () => {
    getTitle.mockRejectedValue(new ApiError(404, "NOT_FOUND", "title not found"));
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail-error")).toBeInTheDocument());
    expect(screen.getByTestId("detail-error")).toHaveTextContent("title not found");
  });

  // Enrichment (external-metadata-enrichment): when the server decorated the
  // Title, the detail renders overview, genres, cast, content rating, runtime,
  // and tagline; an un-enriched Title (the base fixture) shows none of them.
  it("renders enriched metadata when present", async () => {
    getTitle.mockResolvedValue({
      ...detail,
      overview: "Paul Atreides leads a desert rebellion.",
      tagline: "Fear is the mind-killer.",
      contentRating: "PG-13",
      runtimeMinutes: 155,
      genres: ["Science Fiction", "Adventure"],
      cast: [
        { person: "Timothée Chalamet", character: "Paul Atreides", kind: "cast" },
        { person: "Zendaya", character: "Chani", kind: "cast" },
      ],
      enrichmentStatus: "matched",
    });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    expect(screen.getByTestId("detail-overview")).toHaveTextContent("desert rebellion");
    expect(screen.getByTestId("detail-tagline")).toHaveTextContent("mind-killer");
    expect(screen.getByTestId("detail-content-rating")).toHaveTextContent("PG-13");
    expect(screen.getByTestId("detail-runtime")).toHaveTextContent("2h 35m");
    expect(screen.getByTestId("detail-genres")).toHaveTextContent("Science Fiction");
    const cast = screen.getAllByTestId("cast-member");
    expect(cast).toHaveLength(2);
    expect(cast[0]).toHaveTextContent("Timothée Chalamet");
    expect(cast[0]).toHaveTextContent("Paul Atreides");
  });

  it("omits enrichment sections for an un-enriched Title", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(screen.queryByTestId("detail-overview")).toBeNull();
    expect(screen.queryByTestId("detail-genres")).toBeNull();
    expect(screen.queryByTestId("detail-cast")).toBeNull();
    expect(screen.queryByTestId("detail-content-rating")).toBeNull();
  });
});

// "Add to collection" (collections-playlists-ui issue 02, Admin only): the picker
// of existing Collections + a "new collection" row, the idempotent re-add no-op,
// the failed-call inline error, and the Member read-only split.
describe("TitleDetailScreen — Add to collection (Admin)", () => {
  it("adds the Title to a chosen existing collection", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    listCollections.mockResolvedValue([
      { id: "c1", name: "A24 Films", description: "", memberCount: 3 },
      { id: "c2", name: "Christmas", description: "", memberCount: 1 },
    ]);
    addCollectionItems.mockResolvedValue(undefined);

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    // The picker loads the existing Collections lazily on open.
    // The Add-to-collection action lives in the ⋯ overflow menu; open it first.
    await user.click(screen.getByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("add-to-collection-button"));
    await screen.findByTestId("add-to-collection-list");
    const a24 = screen
      .getAllByTestId("collection-option")
      .find((b) => b.getAttribute("data-collection-id") === "c1")!;
    await user.click(a24);

    await waitFor(() =>
      expect(addCollectionItems).toHaveBeenCalledWith("c1", ["t1"]),
    );
    expect(
      await screen.findByTestId("add-to-collection-success"),
    ).toHaveTextContent("A24 Films");
  });

  it("re-adding an existing member is a harmless no-op (idempotent)", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    listCollections.mockResolvedValue([
      { id: "c1", name: "A24 Films", description: "", memberCount: 3 },
    ]);
    // The server is idempotent — a re-add returns 204 just like the first add.
    addCollectionItems.mockResolvedValue(undefined);

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    // The Add-to-collection action lives in the ⋯ overflow menu; open it first.
    await user.click(screen.getByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("add-to-collection-button"));
    await screen.findByTestId("add-to-collection-list");

    const option = () => screen.getByTestId("collection-option");
    await user.click(option());
    await screen.findByTestId("add-to-collection-success");
    // Add the same Title again → still succeeds, no error, no duplicate handling.
    await user.click(option());

    await waitFor(() => expect(addCollectionItems).toHaveBeenCalledTimes(2));
    expect(addCollectionItems).toHaveBeenNthCalledWith(2, "c1", ["t1"]);
    expect(screen.queryByTestId("add-to-collection-error")).not.toBeInTheDocument();
    expect(screen.getByTestId("add-to-collection-success")).toBeInTheDocument();
  });

  it("creates a new collection and adds the Title to it", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    listCollections.mockResolvedValue([]);
    createCollection.mockResolvedValue({
      id: "c9",
      name: "Sci-Fi Night",
      description: "",
    });
    addCollectionItems.mockResolvedValue(undefined);

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    // The Add-to-collection action lives in the ⋯ overflow menu; open it first.
    await user.click(screen.getByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("add-to-collection-button"));
    await screen.findByTestId("new-collection-name-input");

    await user.type(
      screen.getByTestId("new-collection-name-input"),
      "Sci-Fi Night",
    );
    await user.click(screen.getByTestId("create-and-add-button"));

    await waitFor(() =>
      expect(createCollection).toHaveBeenCalledWith({ name: "Sci-Fi Night" }),
    );
    await waitFor(() =>
      expect(addCollectionItems).toHaveBeenCalledWith("c9", ["t1"]),
    );
    expect(
      await screen.findByTestId("add-to-collection-success"),
    ).toHaveTextContent("Sci-Fi Night");
  });

  it("surfaces a failed add (UNKNOWN_TITLE) as a readable inline error", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    listCollections.mockResolvedValue([
      { id: "c1", name: "A24 Films", description: "", memberCount: 3 },
    ]);
    addCollectionItems.mockRejectedValue(
      new ApiError(422, "UNKNOWN_TITLE", "item set names a title that does not exist"),
    );

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    // The Add-to-collection action lives in the ⋯ overflow menu; open it first.
    await user.click(screen.getByTestId("overflow-menu-button"));
    await user.click(screen.getByTestId("add-to-collection-button"));
    await screen.findByTestId("add-to-collection-list");
    await user.click(screen.getByTestId("collection-option"));

    expect(
      await screen.findByTestId("add-to-collection-error"),
    ).toHaveTextContent(/does not exist/i);
    // The panel survives a refused add (no crash).
    expect(screen.getByTestId("add-to-collection-panel")).toBeInTheDocument();
  });

  it("shows a Member NO 'Add to collection' control", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail(MEMBER);
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    expect(
      screen.queryByTestId("add-to-collection-button"),
    ).not.toBeInTheDocument();
    // The metadata editor (also Admin-only) is likewise absent for a Member.
    expect(screen.queryByTestId("metadata-editor")).not.toBeInTheDocument();
  });
});

// "Add to watchlist" (watchlist 01): the icon affordance every authenticated User
// gets — a single POST that adds this Title to their system Watchlist (the server
// ensures the Watchlist exists), with an inline success/error. NOT admin-gated.
describe("TitleDetailScreen — Add to watchlist (any User)", () => {
  it("adds the Title to the watchlist and shows a confirmation", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    addToWatchlist.mockResolvedValue(undefined);

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("add-to-watchlist-button"));

    await waitFor(() => expect(addToWatchlist).toHaveBeenCalledWith("t1"));
    expect(await screen.findByTestId("watchlist-notice")).toHaveTextContent(
      /watchlist/i,
    );
    expect(screen.queryByTestId("watchlist-error")).not.toBeInTheDocument();
  });

  it("surfaces a refused add (KIND_MISMATCH) as a readable inline error", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail);
    addToWatchlist.mockRejectedValue(
      new ApiError(422, "KIND_MISMATCH", "title kind does not match the watchlist's media kind"),
    );

    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    await user.click(screen.getByTestId("add-to-watchlist-button"));

    expect(await screen.findByTestId("watchlist-error")).toHaveTextContent(/kind/i);
    // The page survives a refused add (no crash).
    expect(screen.getByTestId("detail")).toBeInTheDocument();
  });

  it("offers the watchlist affordance to a Member (not admin-gated)", async () => {
    getTitle.mockResolvedValue(detail);
    renderDetail(MEMBER);
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    // A Member gets the watchlist button; the Admin-only ones stay hidden.
    expect(screen.getByTestId("add-to-watchlist-button")).toBeInTheDocument();
    expect(screen.getByTestId("watch-toggle")).toBeInTheDocument();
    // The Edit icon is Admin-only.
    expect(screen.queryByTestId("edit-item-button")).not.toBeInTheDocument();
  });
});

// Per-role artwork tabs (artwork-management/01): a Movie manages Poster +
// Background + Logo from dedicated Edit-item tabs that auto-search on open and
// apply + Lock on click; an Episode leaf gets none. Picking bumps the hero image
// so it reloads.
describe("TitleDetailScreen — artwork tabs (Admin)", () => {
  it("shows distinct Poster, Background, and Logo tabs on a Movie (beside Search and Fix label)", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail); // kind: "movie"
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    expect(screen.getByTestId("edit-item-tab-search")).toBeInTheDocument();
    expect(screen.getByTestId("edit-item-tab-poster")).toBeInTheDocument();
    expect(screen.getByTestId("edit-item-tab-background")).toBeInTheDocument();
    expect(screen.getByTestId("edit-item-tab-logo")).toBeInTheDocument();
    expect(screen.getByTestId("edit-item-tab-fix-label")).toBeInTheDocument();
  });

  it("shows no artwork tab on an Episode leaf", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue({ ...detail, kind: "episode" });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("edit-item-button"));
    expect(screen.getByTestId("edit-item-tab-search")).toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-poster")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-background")).not.toBeInTheDocument();
    expect(screen.queryByTestId("edit-item-tab-logo")).not.toBeInTheDocument();
  });

  it("auto-searches the Poster tab on open (no pre-click) and applies on click, reloading the hero", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue({
      ...detail,
      artwork: [{ role: "poster", url: "https://prov/orig.jpg", path: "", source: "fetched" }],
    });
    searchTitleArtworkCandidates.mockResolvedValue([
      { url: "https://prov/new.jpg", width: 1000, height: 1500, source: "tmdb" },
    ]);
    pickTitleArtwork.mockResolvedValue({
      ...detail,
      lockedFields: ["poster"],
      artwork: [{ role: "poster", url: "https://prov/new.jpg", path: "", source: "fetched" }],
    });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());
    // The hero image is cache-busted on the current poster artwork.
    expect(screen.getByTestId("poster-img")).toHaveAttribute("src", expect.stringContaining("orig.jpg"));

    await user.click(screen.getByTestId("edit-item-button"));
    await user.click(screen.getByTestId("edit-item-tab-poster"));

    // Auto-search: the grid renders one thumbnail per candidate with no "Choose
    // image" pre-click.
    await waitFor(() => expect(screen.getByTestId("artwork-grid-poster")).toBeInTheDocument());
    expect(searchTitleArtworkCandidates).toHaveBeenCalledWith("t1", "poster");
    expect(screen.queryByTestId("choose-artwork-poster")).not.toBeInTheDocument();

    await user.click(screen.getByTestId("artwork-choice"));

    expect(pickTitleArtwork).toHaveBeenCalledWith("t1", "poster", "https://prov/new.jpg");
    // The hero reloads without a page refresh: its cache-bust version changed.
    await waitFor(() =>
      expect(screen.getByTestId("poster-img")).toHaveAttribute("src", expect.stringContaining("new.jpg")),
    );
  });
});

// "Open in VLC" now lives in the ⋯ overflow menu (targeting the primary playable
// File) rather than on each file row: present when the Title has a playable File,
// absent when it has none.
describe("TitleDetailScreen — Open in VLC (overflow menu)", () => {
  it("shows Open in VLC in the overflow menu when the Title is playable", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue(detail); // has one present file
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    // Not rendered on the file rows anymore, only inside the menu once opened.
    expect(screen.queryByTestId("open-in-vlc")).toBeNull();
    await user.click(screen.getByTestId("overflow-menu-button"));
    expect(screen.getByTestId("open-in-vlc")).toBeInTheDocument();
  });

  it("omits Open in VLC when the Title has no playable File", async () => {
    const user = userEvent.setup();
    getTitle.mockResolvedValue({ ...detail, editions: [] });
    renderDetail();
    await waitFor(() => expect(screen.getByTestId("detail")).toBeInTheDocument());

    await user.click(screen.getByTestId("overflow-menu-button"));
    expect(screen.queryByTestId("open-in-vlc")).toBeNull();
  });
});
