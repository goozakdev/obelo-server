import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { MatcherDocument } from "../api/types";

// The TV adapter. FileMatcher's own tests use a made-up kind precisely so that
// nothing TV-shaped can hide inside the component; these are the tests that the
// TV WORDS exist, and that they come from here.

const {
  getShowMatcher,
  applyShowMatcher,
  listSeriesSlots,
  searchEntityEnrichmentCandidates,
  previewEntityExternalCandidate,
} = vi.hoisted(() => ({
  getShowMatcher: vi.fn(),
  applyShowMatcher: vi.fn(),
  listSeriesSlots: vi.fn(),
  searchEntityEnrichmentCandidates: vi.fn(),
  previewEntityExternalCandidate: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getShowMatcher: (...a: unknown[]) => getShowMatcher(...a),
      applyShowMatcher: (...a: unknown[]) => applyShowMatcher(...a),
      listSeriesSlots: (...a: unknown[]) => listSeriesSlots(...a),
      searchEntityEnrichmentCandidates: (...a: unknown[]) => searchEntityEnrichmentCandidates(...a),
      previewEntityExternalCandidate: (...a: unknown[]) => previewEntityExternalCandidate(...a),
    },
  };
});

import ShowMatcherScreen from "./ShowMatcherScreen";

const FILE = "/tv/Batman/Season 03/Batman - S03E61 - Holiday Knights.mkv";

function doc(over: Partial<MatcherDocument> = {}): MatcherDocument {
  return {
    containerId: "show1",
    containerType: "show",
    libraryId: "lib1",
    title: "Batman: The Animated Series",
    year: 1992,
    seriesExternalId: "tmdb:2098",
    groups: [
      {
        number: 3,
        source: "provider",
        slotCount: 61,
        slotsLoaded: true,
        fileCount: 1,
        placedCount: 1,
        unassignedCount: 0,
        ignoredCount: 0,
        slots: [{ group: 3, slot: 61, name: "Holiday Knights" }],
      },
      {
        number: 1,
        source: "provider",
        slotCount: 5,
        slotsLoaded: false,
        fileCount: 0,
        placedCount: 0,
        unassignedCount: 0,
        ignoredCount: 0,
        slots: [],
      },
    ],
    files: [
      {
        path: FILE,
        state: "placed",
        parsed: [{ group: 3, slot: 61 }],
        placements: [{ group: 3, slot: 61, ordinal: 1 }],
        decided: false,
        orphaned: false,
        reason: "",
      },
    ],
    ...over,
  };
}

function renderScreen() {
  return render(
    <MemoryRouter initialEntries={["/admin/shows/show1/matcher"]}>
      <Routes>
        <Route path="/admin/shows/:showId/matcher" element={<ShowMatcherScreen />} />
        <Route path="/shows/:showId" element={<p>show detail</p>} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getShowMatcher.mockReset();
  applyShowMatcher.mockReset();
  listSeriesSlots.mockReset();
  searchEntityEnrichmentCandidates.mockReset();
  previewEntityExternalCandidate.mockReset();
});

describe("ShowMatcherScreen", () => {
  it("opens with the cheap first load — no group named", async () => {
    getShowMatcher.mockResolvedValue(doc());
    renderScreen();
    await screen.findByTestId("file-matcher");
    expect(getShowMatcher).toHaveBeenCalledTimes(1);
    expect(getShowMatcher.mock.calls[0][1]).toBeUndefined();
  });

  it("supplies the TV wording the kind-neutral component has none of", async () => {
    const user = userEvent.setup();
    getShowMatcher.mockResolvedValue(doc());
    renderScreen();
    await screen.findByTestId("file-matcher");

    expect(screen.getByTestId("show-matcher-title")).toHaveTextContent(
      "Batman: The Animated Series (1992)",
    );
    const seasonThree = document.querySelector(
      '[data-testid="matcher-group"][data-group="3"]',
    ) as HTMLElement;
    expect(within(seasonThree).getByTestId("matcher-group-toggle")).toHaveTextContent("Season 3");
    await user.click(within(seasonThree).getByTestId("matcher-group-toggle"));
    expect(within(seasonThree).getByTestId("matcher-slot-code")).toHaveTextContent("S03E61");
    expect(screen.getByTestId("matcher-unsorted")).toHaveTextContent("Unsorted");
  });

  it("fetches one season's records on expand, and only that one", async () => {
    const user = userEvent.setup();
    getShowMatcher.mockResolvedValue(doc());
    renderScreen();
    await screen.findByTestId("file-matcher");

    const seasonOne = document.querySelector(
      '[data-testid="matcher-group"][data-group="1"]',
    ) as HTMLElement;
    await user.click(within(seasonOne).getByTestId("matcher-group-toggle"));
    await waitFor(() => expect(getShowMatcher).toHaveBeenCalledTimes(2));
    expect(getShowMatcher.mock.calls[1].slice(0, 2)).toEqual(["show1", 1]);
  });

  it("names which of the four reasons the episode titles are missing", async () => {
    getShowMatcher.mockResolvedValue(doc({ slotsUnavailable: "provider-cannot-list" }));
    renderScreen();
    const banner = await screen.findByTestId("matcher-slots-unavailable");
    expect(banner).toHaveTextContent("only TMDB can");
    expect(banner).toHaveTextContent("Renumbering still works");
  });

  it("says so when the show itself never matched, rather than blaming enrichment", async () => {
    getShowMatcher.mockResolvedValue(doc({ slotsUnavailable: "no-series-match" }));
    renderScreen();
    expect(await screen.findByTestId("matcher-slots-unavailable")).toHaveTextContent(
      "never matched a provider record",
    );
  });

  it("offers the escape hatch when the SHOW is the thing that is wrong", async () => {
    getShowMatcher.mockResolvedValue(doc());
    renderScreen();
    const link = await screen.findByTestId("show-matcher-wrong-series");
    expect(link).toHaveAttribute("href", "/shows/show1");
  });

  it("surfaces a failed first load rather than an empty screen", async () => {
    getShowMatcher.mockRejectedValue(new Error("boom"));
    renderScreen();
    expect(await screen.findByTestId("show-matcher-error")).toHaveTextContent("boom");
  });

  it("applies through the show's own route and leaves for the show page", async () => {
    const user = userEvent.setup();
    getShowMatcher.mockResolvedValue(doc());
    applyShowMatcher.mockResolvedValue({
      ...doc(),
      applied: { rearranged: 1, displaced: [], deferred: [] },
    });
    renderScreen();
    await screen.findByTestId("file-matcher");

    const seasonThree = document.querySelector(
      '[data-testid="matcher-group"][data-group="3"]',
    ) as HTMLElement;
    await user.click(within(seasonThree).getByTestId("matcher-group-toggle"));
    await user.click(within(seasonThree).getByTestId("matcher-part-unassign"));
    await user.click(screen.getByTestId("matcher-apply"));

    expect(applyShowMatcher).toHaveBeenCalledWith("show1", {
      files: [{ path: FILE, state: "unassigned" }],
    });
    expect(await screen.findByText("show detail")).toBeInTheDocument();
  });
});

// --- The Batman case, end to end -------------------------------------------
//
// The PRD's opening example, finished. Five files at the end of season 3 belong,
// per the provider, to season 1 of a re-numbered continuation series. Issue 06 let
// the Admin place them in Season 4, where they now correctly sit; TMDB's Batman
// has no season 4, so those five Slots sit bare with no way from this screen to
// reach The New Batman Adventures. This is that way.

const TAIL = [61, 62, 63, 64, 65].map(
  (n) => `/tv/Batman/Season 03/Batman - S03E${n} - Tail.mkv`,
);

/** The Show as it stands after the placement: five files on a Season 4 that
 * exists on no disk and in no provider, so every Slot is bare. */
function placedInSeasonFour(): MatcherDocument {
  return doc({
    groups: [
      {
        number: 4,
        source: "local",
        slotCount: 0,
        slotsLoaded: true,
        fileCount: 5,
        placedCount: 5,
        unassignedCount: 0,
        ignoredCount: 0,
        slots: [1, 2, 3, 4, 5].map((slot) => ({ group: 4, slot, titleId: `t${slot}` })),
      },
    ],
    files: TAIL.map((path, i) => ({
      path,
      state: "placed" as const,
      parsed: [{ group: 3, slot: 61 + i }],
      placements: [{ group: 4, slot: i + 1, ordinal: 1 }],
      decided: true,
      orphaned: false,
      reason: "",
    })),
  });
}

describe("borrowing another series' records from the matcher", () => {
  it("fills a whole season's records in one gesture, keeping the local numbering", async () => {
    const user = userEvent.setup();
    getShowMatcher.mockResolvedValue(placedInSeasonFour());
    // The continuation series, found by searching the Show's own name.
    searchEntityEnrichmentCandidates.mockResolvedValue({
      candidates: [
        { externalId: "77777", title: "The New Batman Adventures", year: 1997, kind: "show" },
      ],
      hasMore: false,
    });
    // It has ONE season, numbered 1 — so the chooser cannot open on the season
    // being repointed (4) and must fall back, which is the normal path here.
    listSeriesSlots.mockImplementation(async (_show: string, _id: string, group?: number) => ({
      externalId: "77777",
      groups: [{ number: 1, slotCount: 5 }],
      group: group === undefined ? undefined : { number: group },
      slots:
        group === undefined
          ? []
          : [1, 2, 3, 4, 5].map((slot) => ({
              group: 1,
              slot,
              name: `New Batman ${slot}`,
              overview: `overview ${slot}`,
            })),
    }));
    applyShowMatcher.mockResolvedValue({
      ...placedInSeasonFour(),
      applied: { rearranged: 0, displaced: [], deferred: [] },
    });

    renderScreen();
    await screen.findByTestId("file-matcher");
    const seasonFour = document.querySelector(
      '[data-testid="matcher-group"][data-group="4"]',
    ) as HTMLElement;
    await user.click(within(seasonFour).getByTestId("matcher-group-toggle"));

    // Bare before: the Show's own series has nothing to say about season 4.
    expect(within(seasonFour).getAllByTestId("matcher-slot-bare")).toHaveLength(5);

    await user.click(screen.getByTestId("matcher-group-fill-records"));
    // Step one: the series. It is searched on the Show's own name, because the
    // record living one season along in the SAME series is the common case.
    await waitFor(() => expect(searchEntityEnrichmentCandidates).toHaveBeenCalled());
    expect(searchEntityEnrichmentCandidates.mock.calls[0].slice(0, 3)).toEqual([
      "shows",
      "show1",
      "Batman: The Animated Series",
    ]);
    await user.click(await screen.findByTestId("fix-use-best-guess"));

    // Step two: the records, listed off the Show + the chosen series — the anchor
    // EpisodeChooser's `load` seam exists for.
    await screen.findByTestId("episode-choice-list");
    expect(listSeriesSlots.mock.calls[0].slice(0, 3)).toEqual(["show1", "77777", undefined]);
    expect(listSeriesSlots.mock.calls[1].slice(0, 3)).toEqual(["show1", "77777", 1]);

    // Picking the first record fills the run from there.
    await user.click(screen.getAllByTestId("episode-choice")[0]);

    await waitFor(() =>
      expect(within(slotFour(1)).getByTestId("matcher-slot-name")).toHaveTextContent(
        "New Batman 1",
      ),
    );
    for (const n of [1, 2, 3, 4, 5]) {
      // The words are borrowed...
      expect(within(slotFour(n)).getByTestId("matcher-slot-name")).toHaveTextContent(
        `New Batman ${n}`,
      );
      // ...and the code is not. The borrowed run is numbered 1..5 and this Show has
      // a real Season 1; that is the collision ADR-0044 keeps the record separate to
      // prevent.
      expect(within(slotFour(n)).getByTestId("matcher-slot-code")).toHaveTextContent(
        `S04E0${n}`,
      );
      expect(within(slotFour(n)).getByTestId("matcher-slot-pin")).toHaveTextContent(
        `Record from S01E0${n} of series 77777`,
      );
    }

    await user.click(screen.getByTestId("matcher-apply"));
    const [, payload] = applyShowMatcher.mock.calls[0];
    expect(payload.slots).toHaveLength(5);
    expect(payload.slots[0]).toMatchObject({
      group: 4,
      slot: 1,
      record: { externalId: "77777", group: 1, slot: 1 },
    });
  });
});

const slotFour = (slot: number) =>
  document.querySelector(
    `[data-testid="matcher-slot"][data-group="4"][data-slot="${slot}"]`,
  ) as HTMLElement;
