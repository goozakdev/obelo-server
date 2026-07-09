import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, fireEvent } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import type { TitleDetail } from "../api/types";
import { entryFromTitle, type QueueState } from "../player/queue/model";
import { saveQueue } from "../player/queue/persist";
import type { TitleSummary } from "../api/types";

// "Add to queue" / "Play next" on TitleDetailScreen (queue/03): appending any
// Title to the Queue from its detail page — allowed cross-Album/Show, cross-kind,
// and as a duplicate (the Queue is a sequence, not a set). We observe the SHARED
// useQueue store via a small probe mounted alongside the screen under one
// QueueProvider (renderWithAuth), so the assertions are on observable Queue state,
// not the store's internals. Mirrors the browse component-test seam (faked
// ApiClient, renderWithAuth).

const { getTitle } = vi.hoisted(() => ({ getTitle: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTitle: (...a: unknown[]) => getTitle(...a),
    },
  };
});

import TitleDetailScreen from "./TitleDetailScreen";
import { useQueue } from "../player/queue/useQueue";

// A movie TitleDetail (a different KIND than the seeded music Track), with one
// present File so it reads as a normal playable Title.
function movieDetail(id: string, name: string): TitleDetail {
  return {
    id,
    kind: "movie",
    title: name,
    year: 2021,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [
      {
        id: "ed1",
        name: "1080p",
        files: [
          {
            id: "f1",
            path: "/m/x.mkv",
            container: "mkv",
            width: 1920,
            height: 1080,
            bitrate: 6_000_000,
            durationMs: 1000,
            sizeBytes: 1,
            missing: false,
            streams: [],
          },
        ],
      },
    ],
    artwork: [],
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
}

function trackSummary(id: string, name: string): TitleSummary {
  return {
    id,
    kind: "track",
    title: name,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

// Reads the shared Queue and renders its entries (title ids, current marker) so a
// test can assert observable Queue state after an affordance click.
function QueueProbe() {
  const q = useQueue();
  return (
    <ul data-testid="probe">
      {q.entries.map((e, i) => (
        <li
          key={e.entryId}
          data-testid="probe-entry"
          data-title-id={e.title.id}
          data-current={i === q.index ? "true" : "false"}
        >
          {e.title.title}
        </li>
      ))}
    </ul>
  );
}

function probeTitleIds() {
  return screen.getAllByTestId("probe-entry").map((li) => li.getAttribute("data-title-id"));
}

// The queue actions live in the ⋯ overflow menu, which closes after each pick — so
// open it right before clicking the item each time.
function openMenu() {
  fireEvent.click(screen.getByTestId("overflow-menu-button"));
}

function renderDetailWithProbe() {
  return renderWithAuth(
    <>
      <Routes>
        <Route path="/titles/:titleId" element={<TitleDetailScreen />} />
      </Routes>
      <QueueProbe />
    </>,
    { initialEntries: ["/titles/m1"] },
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  getTitle.mockReset();
  getTitle.mockImplementation((id: string) => Promise.resolve(movieDetail(id, "Dune")));
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
});

describe("TitleDetailScreen — Add to queue / Play next", () => {
  it("appends the Title to the END of the Queue — cross-source, cross-kind, and as a duplicate", async () => {
    // Seed a one-entry music-Track Queue so the movie append is cross-kind and
    // cross-source onto a non-empty Queue.
    const seeded: QueueState = {
      entries: [entryFromTitle(trackSummary("tr1", "Idioteque"))],
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", seeded);

    renderDetailWithProbe();
    await screen.findByTestId("detail");

    // Append the movie (m1) — allowed though it's a different kind/source.
    openMenu();
    fireEvent.click(screen.getByTestId("add-to-queue-button"));
    await waitFor(() => expect(probeTitleIds()).toEqual(["tr1", "m1"]));
    expect(screen.getByTestId("queue-notice")).toBeInTheDocument();

    // Appending again adds a SECOND occurrence (a Queue is a sequence, not a set).
    openMenu();
    fireEvent.click(screen.getByTestId("add-to-queue-button"));
    await waitFor(() => expect(probeTitleIds()).toEqual(["tr1", "m1", "m1"]));
    // The currently-playing entry (tr1) is unchanged.
    expect(screen.getAllByTestId("probe-entry")[0]).toHaveAttribute("data-current", "true");
  });

  it("adds to an empty Queue (the first appended entry becomes now-playing)", async () => {
    renderDetailWithProbe();
    await screen.findByTestId("detail");

    openMenu();
    fireEvent.click(screen.getByTestId("add-to-queue-button"));
    await waitFor(() => expect(probeTitleIds()).toEqual(["m1"]));
    expect(screen.getAllByTestId("probe-entry")[0]).toHaveAttribute("data-current", "true");
  });

  it("Play next inserts the Title immediately AFTER the current entry", async () => {
    // Seed [a (current), b]; Play next on m1 → [a, m1, b], current still a.
    const seeded: QueueState = {
      entries: [
        entryFromTitle(trackSummary("a", "A")),
        entryFromTitle(trackSummary("b", "B")),
      ],
      currentIndex: 0,
      repeat: "off",
      authoredOrder: null,
    };
    saveQueue(window.sessionStorage, "u1", seeded);

    renderDetailWithProbe();
    await screen.findByTestId("detail");

    openMenu();
    fireEvent.click(screen.getByTestId("play-next-button"));
    await waitFor(() => expect(probeTitleIds()).toEqual(["a", "m1", "b"]));
    // The now-playing entry didn't move.
    expect(screen.getAllByTestId("probe-entry")[0]).toHaveAttribute("data-current", "true");
  });
});
