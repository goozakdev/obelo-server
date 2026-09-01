import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/client";
import type { MatcherDocument, MatcherFile, MatcherGroup, MatcherSlot } from "../api/types";
import FileMatcher, { type RepointRequest } from "./FileMatcher";
import type { MatcherLabels } from "./matcherLabels";

// FileMatcher, driven the way an Admin drives it.
//
// The labels below deliberately describe a kind that does not exist — volumes and
// chapters — for one reason: the component must carry NO TV vocabulary of its own,
// or the Album matcher is a rewrite rather than an adapter (ADR-0044). One test
// asserts that directly, and every other test reads in that vocabulary, which is
// itself a standing check that nothing has leaked back in.
//
// The gestures are exercised through the CLICK path wherever both paths exist,
// because that is the path that has to keep up: it is what a finger and a keyboard
// use, and it is the one a drag-first implementation quietly lets rot.

const labels: MatcherLabels = {
  groupName: (group) => `Volume ${group}`,
  slotCode: (group, slot) => `V${group}-${String(slot).padStart(2, "0")}`,
  slotNoun: "chapter",
  slotNounPlural: "chapters",
  groupNoun: "volume",
  seriesNoun: "collection",
  unsortedName: "Unsorted",
  unsortedHint: "Files claiming no volume at all.",
  unavailableSentence: (reason) =>
    ({
      "no-series-match": "Nothing is matched to this container yet.",
      "enrichment-disabled": "Enrichment is switched off for this library.",
      "provider-cannot-list": "This provider cannot list chapters.",
      "provider-unreachable": "The provider could not be reached.",
    })[reason],
};

const A = "/media/Show/Vol 03/Show - S03E01 - Holiday Knights.mkv";
const B = "/media/Show/Vol 03/Show.S03E02.1080p.WEB-DL.x264-GRP.mkv";
const LOOSE = "/media/Show/sample.mkv";

function file(over: Partial<MatcherFile> & { path: string }): MatcherFile {
  return {
    state: "placed",
    parsed: [],
    placements: [],
    decided: false,
    orphaned: false,
    unreadable: false,
    reason: "",
    ...over,
  };
}

function group(over: Partial<MatcherGroup> & { number: number }): MatcherGroup {
  return {
    source: "provider",
    slotCount: 0,
    slotsLoaded: false,
    fileCount: 0,
    placedCount: 0,
    unassignedCount: 0,
    ignoredCount: 0,
    slots: [],
    ...over,
  };
}

// One misnumbered container: two files sitting on volume 3, and one loose file
// nothing could number. `Holiday Nights` is a deliberate one-letter provider typo
// against the file's `Holiday Knights`; the second file is a scene release whose
// name carries no title at all.
function doc(over: Partial<MatcherDocument> = {}): MatcherDocument {
  return {
    containerId: "c1",
    containerType: "show",
    libraryId: "lib1",
    title: "Show",
    groups: [
      group({
        number: 3,
        slotCount: 2,
        slotsLoaded: true,
        fileCount: 2,
        placedCount: 2,
        slots: [
          { group: 3, slot: 1, name: "Holiday Nights" },
          { group: 3, slot: 2, name: "Sins of the Father" },
        ],
      }),
      group({ number: 4, slotCount: 2 }),
    ],
    files: [
      file({
        path: A,
        parsed: [{ group: 3, slot: 1 }],
        placements: [{ group: 3, slot: 1, ordinal: 1 }],
      }),
      file({
        path: B,
        parsed: [{ group: 3, slot: 2 }],
        placements: [{ group: 3, slot: 2, ordinal: 1 }],
      }),
      file({ path: LOOSE, state: "unassigned" }),
    ],
    ...over,
  };
}

const VOLUME_4 = group({
  number: 4,
  slotCount: 2,
  slotsLoaded: true,
  slots: [
    { group: 4, slot: 1, name: "Holiday Knights" },
    { group: 4, slot: 2, name: "Sins of the Father" },
  ],
});

function setup(over: Partial<MatcherDocument> = {}, opts: { loadGroup?: typeof loadGroup } = {}) {
  const applied = doc(over);
  const apply = vi.fn().mockResolvedValue({ ...applied, applied: { rearranged: 1, displaced: [], deferred: [] } });
  const onClose = vi.fn();
  const loadGroupFn = opts.loadGroup ?? loadGroup;
  const utils = render(
    <FileMatcher
      matcher={applied}
      labels={labels}
      loadGroup={loadGroupFn}
      apply={apply}
      onClose={onClose}
    />,
  );
  return { apply, onClose, loadGroup: loadGroupFn, ...utils };
}

const loadGroup = vi.fn(async (n: number) => (n === 4 ? VOLUME_4 : null));

const groupToggle = (n: number) =>
  within(
    document.querySelector(`[data-testid="matcher-group"][data-group="${n}"]`) as HTMLElement,
  ).getByTestId("matcher-group-toggle");

const slotEl = (g: number, s: number) =>
  document.querySelector(
    `[data-testid="matcher-slot"][data-group="${g}"][data-slot="${s}"]`,
  ) as HTMLElement;

const partEl = (path: string) =>
  document.querySelector(`[data-testid="matcher-part"][data-path="${CSS.escape(path)}"]`) as HTMLElement;

beforeEach(() => {
  loadGroup.mockClear();
});

describe("the local half renders before any provider call", () => {
  it("shows every file, its counts and the Unsorted tray with no expand", () => {
    setup();
    // The tray is pinned and always visible so it can be dragged from into
    // whichever group happens to be open.
    const tray = screen.getByTestId("matcher-unsorted");
    expect(within(tray).getByText("sample.mkv")).toBeInTheDocument();
    expect(groupToggle(3)).toHaveTextContent("2 assigned · 0 unassigned");
    expect(loadGroup).not.toHaveBeenCalled();
  });

  it("carries no vocabulary of its own — the seam that makes another kind an adapter", () => {
    const { container } = setup();
    expect(container.textContent ?? "").not.toMatch(/season|episode/i);
    expect(container.textContent ?? "").toMatch(/Volume 3/);
  });
});

describe("the click path", () => {
  it("moves a file into another group, one click on the file and one on the slot", async () => {
    const user = userEvent.setup();
    const { apply, onClose } = setup();

    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    // The selection banner is what tells the Admin the gesture is half-done.
    expect(screen.getByTestId("matcher-selection")).toHaveTextContent("Holiday Knights.mkv");

    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());
    await user.click(within(slotEl(4, 1)).getByTestId("matcher-slot-target"));

    expect(within(slotEl(4, 1)).getByTestId("matcher-part-pick")).toHaveTextContent(
      "Holiday Knights.mkv",
    );
    expect(screen.getByTestId("matcher-apply")).toHaveTextContent("Apply 1 change");

    await user.click(screen.getByTestId("matcher-apply"));
    expect(apply).toHaveBeenCalledWith({
      files: [{ path: A, state: "placed", placements: [{ group: 4, slot: 1, ordinal: 1 }] }],
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("reaches the same drop from the keyboard alone", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 2)).toBeInTheDocument());

    within(partEl(A)).getByTestId("matcher-part-pick").focus();
    await user.keyboard("{Enter}");
    within(slotEl(4, 2)).getByTestId("matcher-slot-target").focus();
    await user.keyboard("{Enter}");

    expect(within(slotEl(4, 2)).getByTestId("matcher-part-pick")).toHaveTextContent(
      "Holiday Knights.mkv",
    );
  });

  it("takes a file off its slot and leaves it unassigned", async () => {
    const user = userEvent.setup();
    const { apply } = setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));

    expect(partEl(A)).toBeNull();
    const files = document.querySelector('[data-testid="matcher-files"][data-group="3"]') as HTMLElement;
    // Found by its FULL name (kept on the title attribute), shown elided.
    expect(within(files).getByTitle("Show - S03E01 - Holiday Knights.mkv")).toHaveTextContent(
      "…S03E01 - Holiday Knights.mkv",
    );

    await user.click(screen.getByTestId("matcher-apply"));
    // Unassigned has to be SAID: a sparse store spends "no row" on the parse, so
    // omitting the file would put it straight back on its slot at the next scan.
    expect(apply).toHaveBeenCalledWith({ files: [{ path: A, state: "unassigned" }] });
  });

  it("ignores a file and restores it, without touching anything on disk", async () => {
    const user = userEvent.setup();
    const { apply } = setup();
    const tray = screen.getByTestId("matcher-unsorted");
    await user.click(within(tray).getByTestId("matcher-file-ignore"));
    expect(within(screen.getByTestId("matcher-unsorted")).queryByText("sample.mkv")).toBeNull();

    await user.click(screen.getByTestId("matcher-ignored-toggle"));
    expect(screen.getByTestId("matcher-ignored-file")).toHaveTextContent("sample.mkv");

    await user.click(screen.getByTestId("matcher-apply"));
    expect(apply).toHaveBeenCalledWith({ files: [{ path: LOOSE, state: "ignored" }] });

    await user.click(screen.getByTestId("matcher-restore"));
    expect(within(screen.getByTestId("matcher-unsorted")).getByText("sample.mkv")).toBeInTheDocument();
  });
});

describe("the drop onto an occupied slot", () => {
  it("asks, and never silently leaves two files claiming one slot", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-slot-target"));

    const choice = within(slotEl(3, 2)).getByTestId("matcher-occupied-choice");
    expect(choice).toHaveTextContent("V3-02 already holds");
    // Nothing has moved yet.
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
  });

  it("merges into ordered parts, reorderable by the click path", async () => {
    const user = userEvent.setup();
    const { apply } = setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-slot-target"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-choice-merge"));

    let parts = within(slotEl(3, 2)).getAllByTestId("matcher-part");
    expect(parts.map((p) => p.dataset.path)).toEqual([B, A]);
    expect(within(parts[0]).getByTestId("matcher-part-label")).toHaveTextContent("Part 1");

    await user.click(within(partEl(A)).getByTestId("matcher-part-up"));
    parts = within(slotEl(3, 2)).getAllByTestId("matcher-part");
    expect(parts.map((p) => p.dataset.path)).toEqual([A, B]);

    await user.click(screen.getByTestId("matcher-apply"));
    // BOTH halves are named, including the one that parses onto this slot: a
    // Placement colliding with a bare parse displaces it rather than merging.
    expect(apply).toHaveBeenCalledWith({
      files: [
        { path: A, state: "placed", placements: [{ group: 3, slot: 2, ordinal: 1 }] },
        { path: B, state: "placed", placements: [{ group: 3, slot: 2, ordinal: 2 }] },
      ],
    });
  });

  it("displaces the sitting file back into the Files column", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-slot-target"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-choice-displace"));

    expect(within(slotEl(3, 2)).getAllByTestId("matcher-part").map((p) => p.dataset.path)).toEqual([A]);
    const files = document.querySelector('[data-testid="matcher-files"][data-group="3"]') as HTMLElement;
    expect(
      within(files).getByTitle("Show.S03E02.1080p.WEB-DL.x264-GRP.mkv"),
    ).toHaveTextContent("…S03E02.1080p.WEB-DL.x264-GRP.mkv");
  });
});

describe("the per-file actions", () => {
  it("names both icons in the Slot's own vocabulary", async () => {
    // The words are gone from the row, so the tooltip and the accessible name are
    // the only place they survive — and they are still the adapter's nouns, never
    // "episode" hard-coded into a component that also sorts chapters.
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    const part = within(partEl(A));

    const share = part.getByTestId("matcher-part-share");
    expect(share).toHaveAttribute("title", "Include this file in an additional chapter");
    expect(share).toHaveAccessibleName("Include this file in an additional chapter");

    const unlink = part.getByTestId("matcher-part-unassign");
    expect(unlink).toHaveAttribute("title", "Unlink this file from chapter");
    expect(unlink).toHaveAccessibleName("Unlink this file from chapter");
  });
});

describe("one file across several slots", () => {
  it("shows on both, marked shared", async () => {
    const user = userEvent.setup();
    const { apply } = setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-share"));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());
    await user.click(within(slotEl(4, 1)).getByTestId("matcher-slot-target"));

    const here = within(slotEl(3, 1)).getByTestId("matcher-part");
    const there = within(slotEl(4, 1)).getByTestId("matcher-part");
    expect(here.dataset.shared).toBe("true");
    expect(there.dataset.shared).toBe("true");
    expect(within(here).getByTestId("matcher-part-shared")).toHaveTextContent("V4-01");

    await user.click(screen.getByTestId("matcher-apply"));
    expect(apply).toHaveBeenCalledWith({
      files: [
        {
          path: A,
          state: "placed",
          placements: [
            { group: 3, slot: 1, ordinal: 1 },
            { group: 4, slot: 1, ordinal: 1 },
          ],
        },
      ],
    });
  });
});

// The two labels are read AGAINST each other, so what matters is that they come
// out as two lines of the same shape: position, separator, title. Anything that
// makes a placed File's label start with something other than its position puts
// the two columns of characters out of step and the comparison back on the Admin.
describe("the two labels", () => {
  it("writes a Slot's label as one line, code then title", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    expect(slotEl(3, 1).querySelector(".matcher-slot-target")).toHaveTextContent(
      "V3-01 - Holiday Nights",
    );
  });

  it("cuts the container's name off a placed file, leaving the position first", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    const name = within(partEl(A)).getByTestId("matcher-part-pick");
    // "Show - S03E01 - Holiday Knights.mkv", with the "Show" every file in this
    // container repeats replaced by the one character that stands for it.
    expect(name).toHaveTextContent("…S03E01 - Holiday Knights.mkv");
    // Nothing is actually hidden: the whole filename is still there to be had.
    expect(name).toHaveAttribute("title", "Show - S03E01 - Holiday Knights.mkv");
  });

  it("cuts the container's name in the Files column too", async () => {
    // The unplaced column is the one an Admin reads WHILE dragging, against the
    // Slots beside it, and every file in it repeats the same prefix. Take a file
    // off its Slot and it should read the same way there as it did on the Slot.
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));

    const card = document.querySelector(
      `[data-testid="matcher-file"][data-path="${CSS.escape(A)}"]`,
    ) as HTMLElement;
    expect(within(card).getByTestId("matcher-file-pick")).toHaveTextContent(
      "…S03E01 - Holiday Knights.mkv",
    );
  });

  it("leaves a file the container's name does not open alone", async () => {
    // "sample.mkv" never names the Show, so there is no prefix to stand in for.
    setup();
    expect(within(screen.getByTestId("matcher-unsorted")).getByText("sample.mkv")).toBeInTheDocument();
  });
});

describe("the comparison", () => {
  it("says nothing about a correctly-matched dotted filename", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    // Show.S03E02.1080p.WEB-DL.x264-GRP.mkv on V3-02 "Sins of the Father": the
    // numbers agree and the filename carries no title, so the row is silent.
    expect(partEl(B).dataset.titleMismatch).toBe("false");
    expect(slotEl(3, 2).dataset.positionMismatch).toBe("false");
    expect(within(partEl(B)).queryByTestId("matcher-part-mark-title")).toBeNull();
    expect(within(partEl(B)).queryByTestId("matcher-part-mark-position")).toBeNull();
  });

  it("marks the disagreeing TITLE, on the filename and nowhere else", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));

    // The provider's "Holiday Nights" against the file's "Holiday Knights". The
    // whole claimed title is marked, in place, inside the name it belongs to —
    // there is no second copy of the text to read it against.
    const marks = within(partEl(A)).getAllByTestId("matcher-part-mark-title");
    expect(marks.map((m) => m.textContent).join("")).toBe("Holiday Knights");
    // The numbers agree here, so the position it claims is left alone...
    expect(within(partEl(A)).queryByTestId("matcher-part-mark-position")).toBeNull();
    // ...and the Slot's own words are never marked, whatever disagrees.
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).not.toHaveClass("is-mismatch");
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-code")).not.toHaveClass("is-mismatch");
  });

  it("marks the disagreeing POSITION, on the filename and nowhere else", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());
    await user.click(within(slotEl(4, 1)).getByTestId("matcher-slot-target"));

    // The file is named S03E01 and now sits on V4-01, so its own S03E01 is marked.
    const marks = within(slotEl(4, 1)).getAllByTestId("matcher-part-mark-position");
    expect(marks.map((m) => m.textContent).join("")).toBe("S03E01");
    // The row is still findable at a glance: the disagreement is on the Slot's edge.
    expect(slotEl(4, 1).dataset.positionMismatch).toBe("true");
    // But the Slot's own code is NOT marked — it is what the file is measured
    // against, and marking both sides is the thing this replaced.
    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-code")).not.toHaveClass("is-mismatch");
    expect(within(slotEl(4, 1)).getByTestId("matcher-position-mismatch")).toHaveTextContent(
      "the filename says V3-01",
    );
  });
});

describe("committing", () => {
  it("Revert restores the opening arrangement exactly", async () => {
    const user = userEvent.setup();
    const { apply } = setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-slot-target"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-choice-merge"));
    await user.click(screen.getByTestId("matcher-file-ignore"));
    expect(screen.getByTestId("matcher-apply")).toHaveTextContent("Apply 2 changes");

    await user.click(screen.getByTestId("matcher-revert"));

    expect(within(slotEl(3, 1)).getByTestId("matcher-part").dataset.path).toBe(A);
    expect(within(slotEl(3, 2)).getByTestId("matcher-part").dataset.path).toBe(B);
    expect(within(screen.getByTestId("matcher-unsorted")).getByText("sample.mkv")).toBeInTheDocument();
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
    expect(screen.getByTestId("matcher-revert")).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("Cancel warns before discarding unsaved changes, and writes nothing", async () => {
    const user = userEvent.setup();
    const { apply, onClose } = setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));

    await user.click(screen.getByTestId("matcher-cancel"));
    expect(screen.getByTestId("matcher-confirm-leave")).toHaveTextContent("1 unsaved change");
    expect(onClose).not.toHaveBeenCalled();

    await user.click(screen.getByTestId("matcher-confirm-leave-stay"));
    expect(screen.queryByTestId("matcher-confirm-leave")).toBeNull();

    await user.click(screen.getByTestId("matcher-cancel"));
    await user.click(screen.getByTestId("matcher-confirm-leave-discard"));
    expect(onClose).toHaveBeenCalled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("Cancel leaves at once when nothing has changed", async () => {
    const user = userEvent.setup();
    const { onClose } = setup();
    await user.click(screen.getByTestId("matcher-cancel"));
    expect(onClose).toHaveBeenCalled();
  });

  it("warns the browser about navigating away with unsaved changes", async () => {
    const user = userEvent.setup();
    setup();
    const before = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(before);
    expect(before.defaultPrevented).toBe(false);

    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));

    const after = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(after);
    expect(after.defaultPrevented).toBe(true);
  });
});

describe("what the server answers", () => {
  it("reports deferred placements instead of closing, so a correction never looks dropped", async () => {
    const user = userEvent.setup();
    const applied = doc();
    const apply = vi.fn().mockResolvedValue({
      ...applied,
      applied: { rearranged: 1, displaced: [B], deferred: [A] },
    });
    const onClose = vi.fn();
    render(
      <FileMatcher matcher={applied} labels={labels} loadGroup={loadGroup} apply={apply} onClose={onClose} />,
    );
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));
    await user.click(screen.getByTestId("matcher-apply"));

    const panel = await screen.findByTestId("matcher-applied");
    expect(onClose).not.toHaveBeenCalled();
    expect(within(panel).getByTestId("matcher-applied-deferred")).toHaveTextContent(
      "Show - S03E01 - Holiday Knights.mkv",
    );
    expect(within(panel).getByTestId("matcher-applied-displaced")).toHaveTextContent(
      "Show.S03E02.1080p.WEB-DL.x264-GRP.mkv",
    );
    await user.click(within(panel).getByTestId("matcher-applied-done"));
    expect(onClose).toHaveBeenCalled();
  });

  it("offers the three fixes for a SLOT_COLLISION, naming the slot and both paths", async () => {
    const user = userEvent.setup();
    const applied = doc();
    const apply = vi
      .fn()
      .mockRejectedValue(
        new ApiError(409, "SLOT_COLLISION", "two files claim V3-02", {
          slot: { group: 3, slot: 2 },
          paths: [A, B],
        }),
      );
    render(
      <FileMatcher matcher={applied} labels={labels} loadGroup={loadGroup} apply={apply} onClose={vi.fn()} />,
    );
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));
    await user.click(screen.getByTestId("matcher-apply"));

    const panel = await screen.findByTestId("matcher-collision");
    expect(panel).toHaveTextContent("two files claim V3-02");
    expect(within(panel).getAllByTestId("matcher-collision-path").map((li) => li.dataset.path)).toEqual([A, B]);
    expect(within(panel).getByTestId("matcher-collision-merge")).toBeInTheDocument();
    expect(within(panel).getAllByTestId("matcher-collision-move")).toHaveLength(2);
    expect(within(panel).getAllByTestId("matcher-collision-unassign")).toHaveLength(2);

    // The merge fix arranges both files onto the contested slot as parts.
    await user.click(within(panel).getByTestId("matcher-collision-merge"));
    expect(screen.queryByTestId("matcher-collision")).toBeNull();
    expect(within(slotEl(3, 2)).getAllByTestId("matcher-part").map((p) => p.dataset.path)).toEqual([A, B]);
  });

  it("treats SCAN_RUNNING as retryable, because nothing was written", async () => {
    const user = userEvent.setup();
    const applied = doc();
    const apply = vi
      .fn()
      .mockRejectedValueOnce(new ApiError(409, "SCAN_RUNNING", "a scan is running for this library"))
      .mockResolvedValue({ ...applied, applied: { rearranged: 1, displaced: [], deferred: [] } });
    const onClose = vi.fn();
    render(
      <FileMatcher matcher={applied} labels={labels} loadGroup={loadGroup} apply={apply} onClose={onClose} />,
    );
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-unassign"));
    await user.click(screen.getByTestId("matcher-apply"));

    const panel = await screen.findByTestId("matcher-scan-running");
    expect(panel).toHaveTextContent("a scan is running");
    await user.click(within(panel).getByTestId("matcher-retry-apply"));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(apply).toHaveBeenCalledTimes(2);
  });
});

describe("the degraded path", () => {
  it("names WHICH of the four reasons there are no records", () => {
    setup({ slotsUnavailable: "provider-cannot-list" });
    const banner = screen.getByTestId("matcher-slots-unavailable");
    expect(banner).toHaveTextContent("This provider cannot list chapters.");
    expect(banner.dataset.reason).toBe("provider-cannot-list");
  });

  it("still renders bare numbered slots, because renumbering works offline", async () => {
    const user = userEvent.setup();
    setup({
      slotsUnavailable: "enrichment-disabled",
      groups: [
        group({ number: 3, slotsLoaded: true, slots: [{ group: 3, slot: 1 }, { group: 3, slot: 2 }] }),
      ],
    });
    await user.click(groupToggle(3));
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-code")).toHaveTextContent("V3-01");
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-bare")).toBeInTheDocument();
    // A slot with no record has nothing to compare a title against, so no row lights up.
    expect(within(slotEl(3, 1)).getByTestId("matcher-part").dataset.titleMismatch).toBe("false");
  });

  it("reports a per-group failure without disturbing the rest of the screen", async () => {
    const user = userEvent.setup();
    const failing = vi.fn(async (n: number) =>
      n === 4 ? group({ number: 4, slotsLoaded: true, slotsUnavailable: "provider-unreachable" }) : null,
    );
    setup({}, { loadGroup: failing });
    await user.click(groupToggle(4));
    const banner = await screen.findByTestId("matcher-group-unavailable");
    expect(banner).toHaveTextContent("The provider could not be reached.");
    // The local half is untouched.
    expect(screen.getByTestId("matcher-unsorted")).toHaveTextContent("sample.mkv");
  });
});

describe("an empty slot", () => {
  it("holds the outline of the file it is waiting for", async () => {
    // An empty Slot that shrinks to its title line says only that a name exists.
    // The outline says the Slot is somewhere a file GOES, which is the whole job of
    // this screen — and a Slot that already has one has nothing left to ask for.
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());

    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-empty")).toHaveTextContent(
      "Drag a file here",
    );

    await user.click(groupToggle(3));
    expect(within(slotEl(3, 1)).queryByTestId("matcher-slot-empty")).toBeNull();
  });

  it("is the click target for the second half of a click-to-place", async () => {
    // The outline is the part of an empty Slot an Admin aims at, so it has to take
    // the click — landing a selected file exactly as the title line does. It says
    // so, too: mid-gesture the prompt is the act being finished, not the other one.
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());

    const outline = within(slotEl(4, 1)).getByTestId("matcher-slot-empty");
    expect(outline).toHaveTextContent("Place it here");
    await user.click(outline);

    expect(within(slotEl(4, 1)).getByTestId("matcher-part-pick")).toHaveTextContent(
      "Holiday Knights.mkv",
    );
    // Filled now, so there is no outline left to offer.
    expect(within(slotEl(4, 1)).queryByTestId("matcher-slot-empty")).toBeNull();
  });
});

describe("the notice dock", () => {
  it("raises every transient notice into it, not into the page", async () => {
    // The dock is fixed to the viewport. What matters here is that the notices are
    // INSIDE it — a notice rendered as a sibling is a notice in the document, which
    // is the thing an Admin standing at season 4 never sees.
    const user = userEvent.setup();
    setup();
    const dock = () => screen.getByTestId("matcher-notices");

    // Empty until something is said.
    expect(within(dock()).queryByTestId("matcher-selection")).toBeNull();

    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    expect(within(dock()).getByTestId("matcher-selection")).toHaveTextContent(
      "choose a chapter to place it on",
    );

    // Finish the gesture on an empty chapter: the prompt has been answered, so it
    // leaves the dock.
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());
    await user.click(within(slotEl(4, 1)).getByTestId("matcher-slot-target"));
    expect(within(dock()).queryByTestId("matcher-selection")).toBeNull();

    // And the leave confirmation, which is the other thing that used to appear a
    // scroll away from the button that raised it.
    await user.click(screen.getByTestId("matcher-cancel"));
    expect(within(dock()).getByTestId("matcher-confirm-leave")).toBeInTheDocument();
  });
});

describe("adding a slot", () => {
  it("extends a group past the highest number anything claims", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    const slots = document.querySelectorAll('[data-testid="matcher-slot"][data-group="3"]');
    expect(slots).toHaveLength(2);
    await user.click(
      within(document.querySelectorAll('[data-testid="matcher-slots"]')[0] as HTMLElement).getByTestId(
        "matcher-add-slot",
      ),
    );
    expect(slotEl(3, 3)).toBeInTheDocument();
  });

  it("takes it away again, which nothing else could", async () => {
    // Revert does not touch extraSlots, so before this a mis-clicked "+ add" was
    // permanent for the session. The Slot the Admin invented is the one this screen
    // is allowed to remove.
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(
      within(document.querySelectorAll('[data-testid="matcher-slots"]')[0] as HTMLElement).getByTestId(
        "matcher-add-slot",
      ),
    );

    await openSlotMenu(user, 3, 3);
    // Nothing to decorate on an empty Slot, so Delete is the whole menu.
    expect(screen.queryByTestId("matcher-slot-repoint")).toBeNull();
    await user.click(screen.getByTestId("matcher-slot-delete"));

    expect(document.querySelector('[data-testid="matcher-slot"][data-slot="3"]')).toBeNull();
    // Nothing was written: adding and removing a Slot of one's own is not a change.
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
  });

  it("will not delete a slot the provider listed, nor one holding a file", async () => {
    // Two different refusals, and both matter. A provider Slot is not this screen's
    // to remove — there is no request that deletes a record out of TMDB. And an
    // added Slot with a file on it is holding the Admin's own work: taking the file
    // off first is one click, and it says out loud where the file went.
    // (setupRepoint, because a Slot with no record actions has no menu at all.)
    const user = userEvent.setup();
    setupRepoint();
    await user.click(groupToggle(3));

    await openSlotMenu(user, 3, 1);
    expect(screen.getByTestId("matcher-slot-repoint")).toBeInTheDocument();
    expect(screen.queryByTestId("matcher-slot-delete")).toBeNull();
    await user.keyboard("{Escape}");

    // An added Slot, then the file moved onto it.
    await user.click(
      within(document.querySelectorAll('[data-testid="matcher-slots"]')[0] as HTMLElement).getByTestId(
        "matcher-add-slot",
      ),
    );
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 3)).getByTestId("matcher-slot-target"));

    await openSlotMenu(user, 3, 3);
    expect(screen.getByTestId("matcher-slot-repoint")).toBeInTheDocument();
    expect(screen.queryByTestId("matcher-slot-delete")).toBeNull();
  });
});

// --- Dragging ---------------------------------------------------------------
//
// Pointer events, not HTML5 drag-and-drop: HTML5 DnD does not exist on touch and
// this screen has to work on a tablet. A drop is resolved with elementFromPoint,
// which is the only thing that works for a finger (a touch pointer is implicitly
// captured, so enter/leave never fire on the element underneath).

function pointer(type: string, x: number, y: number, target: EventTarget) {
  act(() => {
    target.dispatchEvent(
      new MouseEvent(type, { clientX: x, clientY: y, bubbles: true, cancelable: true, button: 0 }),
    );
  });
}

function dragTo(from: Element, to: Element) {
  const original = document.elementFromPoint;
  document.elementFromPoint = () => to as Element;
  pointer("pointerdown", 10, 100, from);
  pointer("pointermove", 60, 300, window);
  pointer("pointerup", 60, 300, window);
  document.elementFromPoint = original;
}

describe("dragging", () => {
  it("drops a file onto a slot, landing in the same handler the click path uses", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 2)).toBeInTheDocument());

    dragTo(within(partEl(A)).getByTestId("matcher-part-pick"), slotEl(4, 2));

    expect(within(slotEl(4, 2)).getByTestId("matcher-part").dataset.path).toBe(A);
  });

  it("reorders parts by dragging one in front of the other", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    await user.click(within(partEl(A)).getByTestId("matcher-part-pick"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-slot-target"));
    await user.click(within(slotEl(3, 2)).getByTestId("matcher-choice-merge"));
    expect(within(slotEl(3, 2)).getAllByTestId("matcher-part").map((p) => p.dataset.path)).toEqual([B, A]);

    dragTo(within(partEl(A)).getByTestId("matcher-part-pick"), partEl(B));

    expect(within(slotEl(3, 2)).getAllByTestId("matcher-part").map((p) => p.dataset.path)).toEqual([A, B]);
  });

  it("unassigns a file dragged back into a Files column", async () => {
    const user = userEvent.setup();
    setup();
    await user.click(groupToggle(3));
    const files = document.querySelector('[data-testid="matcher-files"][data-group="3"]') as HTMLElement;

    dragTo(within(partEl(A)).getByTestId("matcher-part-pick"), files);

    expect(partEl(A)).toBeNull();
    // Found by its FULL name (kept on the title attribute), shown elided.
    expect(within(files).getByTitle("Show - S03E01 - Holiday Knights.mkv")).toHaveTextContent(
      "…S03E01 - Holiday Knights.mkv",
    );
  });
});

// The motivating case (PRD problem statement): the provider counts a run of files
// at the end of one group as the start of another. Five files, ONE problem, and
// until now five queue rows each offering to fix the thing that was not wrong.
describe("a misnumbered run", () => {
  const RUN = [61, 62, 63, 64, 65].map(
    (n) => `/media/Show/Vol 03/Show - S03E${n} - Ep ${n}.mkv`,
  );

  function runDoc(): MatcherDocument {
    return doc({
      groups: [
        group({
          number: 3,
          slotCount: 65,
          slotsLoaded: true,
          slots: RUN.map((_, i) => ({ group: 3, slot: 61 + i })),
        }),
        group({ number: 4, slotCount: 5 }),
      ],
      files: RUN.map((path, i) =>
        file({
          path,
          parsed: [{ group: 3, slot: 61 + i }],
          placements: [{ group: 3, slot: 61 + i, ordinal: 1 }],
        }),
      ),
    });
  }

  it("is fixed in ONE pass — five moves, one Apply, one payload", async () => {
    const user = userEvent.setup();
    const fixture = runDoc();
    const apply = vi
      .fn()
      .mockResolvedValue({ ...fixture, applied: { rearranged: 5, displaced: [], deferred: [] } });
    const onClose = vi.fn();
    const loadFour = vi.fn(async (n: number) =>
      n === 4
        ? group({
            number: 4,
            slotCount: 5,
            slotsLoaded: true,
            slots: [1, 2, 3, 4, 5].map((s) => ({ group: 4, slot: s, name: `Ep ${60 + s}` })),
          })
        : null,
    );
    render(
      <FileMatcher matcher={fixture} labels={labels} loadGroup={loadFour} apply={apply} onClose={onClose} />,
    );

    await user.click(groupToggle(3));
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 5)).toBeInTheDocument());

    for (let i = 0; i < RUN.length; i++) {
      await user.click(within(partEl(RUN[i])).getByTestId("matcher-part-pick"));
      await user.click(within(slotEl(4, i + 1)).getByTestId("matcher-slot-target"));
    }

    expect(screen.getByTestId("matcher-apply")).toHaveTextContent("Apply 5 changes");
    await user.click(screen.getByTestId("matcher-apply"));

    expect(apply).toHaveBeenCalledTimes(1);
    expect(apply).toHaveBeenCalledWith({
      files: RUN.map((path, i) => ({
        path,
        state: "placed",
        placements: [{ group: 4, slot: i + 1, ordinal: 1 }],
      })),
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });
});

// --- Repointing a Slot's record ---------------------------------------------
//
// The second half of a Slot, and the last mile of the case the whole screen was
// built for: five files placed into a group the container's own series does not
// have, whose records live in season 1 of a re-numbered continuation.
//
// The borrowed run is numbered from 1 and the container has a real group 1 of its
// own, so the property under test is mostly a NEGATIVE one — a borrowed record
// supplies words and provenance, never a position. Every one of these reads in the
// fake volumes-and-chapters vocabulary, which is also a standing check that the
// picker's wiring leaked no TV words into the component.

/** The kind adapter's picker, stubbed: it captures the request it was handed and
 * hands back a canned run when clicked. The real one searches a provider. */
function fakePicker(records: MatcherSlot[], externalId = "77777") {
  const held: { request: RepointRequest | null } = { request: null };
  const render = (request: RepointRequest) => {
    held.request = request;
    return (
      <button
        type="button"
        data-testid="fake-picker-pick"
        onClick={() => request.onPicked(externalId, records)}
      >
        pick
      </button>
    );
  };
  return { held, render };
}

// The record actions live behind the Slot's kebab now, so every one of them is a
// two-step reach: open the menu, then pick the item.
async function openSlotMenu(
  user: ReturnType<typeof userEvent.setup>,
  group: number,
  slot: number,
) {
  await user.click(within(slotEl(group, slot)).getByTestId("matcher-slot-menu-toggle"));
}

const BORROWED: MatcherSlot[] = [
  { group: 1, slot: 1, name: "Borrowed One", overview: "first" },
  { group: 1, slot: 2, name: "Borrowed Two" },
  { group: 1, slot: 3, name: "Borrowed Three" },
];

function setupRepoint(records: MatcherSlot[] = BORROWED) {
  const picker = fakePicker(records);
  const applied = doc();
  const apply = vi
    .fn()
    .mockResolvedValue({ ...applied, applied: { rearranged: 1, displaced: [], deferred: [] } });
  render(
    <FileMatcher
      matcher={applied}
      labels={labels}
      loadGroup={loadGroup}
      apply={apply}
      repointRecord={picker.render}
      onClose={vi.fn()}
    />,
  );
  return { apply, picker };
}

describe("repointing a slot's record", () => {
  it("offers no record action on an empty slot", async () => {
    // A record decorates something. An empty Slot has nothing to decorate and no
    // Title to carry the pin, so the action is not on offer at all.
    const user = userEvent.setup();
    setupRepoint();
    await user.click(groupToggle(4));
    await waitFor(() => expect(slotEl(4, 1)).toBeInTheDocument());

    expect(within(slotEl(4, 1)).queryByTestId("matcher-slot-menu-toggle")).toBeNull();
    await user.click(groupToggle(3));
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-menu-toggle")).toBeInTheDocument();
  });

  it("puts the picker OVER the page, not above it", async () => {
    // Repointing starts from a Slot far down the grid. A panel spliced in above the
    // groups is one the Admin never scrolls back to, so the click reads as a no-op —
    // the picker has to be a modal that arrives wherever they are standing.
    const user = userEvent.setup();
    setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 1);
    await user.click(screen.getByTestId("matcher-slot-repoint"));

    const picker = screen.getByTestId("matcher-repoint");
    expect(picker.tagName).toBe("DIALOG");
    expect(picker).toHaveAttribute("open");
    // ...and the picker itself is inside it, not left behind in the page flow.
    expect(within(picker).getByTestId("fake-picker-pick")).toBeInTheDocument();
  });

  it("closes the picker without repointing anything", async () => {
    // The modal covers the page, so backing out has to be reachable from the modal
    // itself — and backing out must leave the Slot exactly as it was.
    const user = userEvent.setup();
    const { apply } = setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 1);
    await user.click(screen.getByTestId("matcher-slot-repoint"));

    await user.click(screen.getByTestId("matcher-repoint-close"));
    expect(screen.queryByTestId("matcher-repoint")).toBeNull();
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).toHaveTextContent(
      "Holiday Nights",
    );
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("closes the picker on ESC", async () => {
    // ESC reaches the dialog as a native cancel event; it must clear the draft
    // rather than leaving a dialog that closed itself behind the component's back.
    const user = userEvent.setup();
    setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 1);
    await user.click(screen.getByTestId("matcher-slot-repoint"));

    fireEvent(screen.getByTestId("matcher-repoint"), new Event("cancel", { cancelable: true }));
    await waitFor(() => expect(screen.queryByTestId("matcher-repoint")).toBeNull());
  });

  it("borrows a whole group's records in ONE gesture, in order", async () => {
    const user = userEvent.setup();
    const { apply, picker } = setupRepoint();
    await user.click(groupToggle(3));

    // One button for the run, not one pin per Slot.
    await user.click(screen.getByTestId("matcher-group-fill-records"));
    expect(picker.held.request?.targets).toEqual([
      { group: 3, slot: 1 },
      { group: 3, slot: 2 },
    ]);
    await user.click(screen.getByTestId("fake-picker-pick"));

    // The words follow, in order...
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).toHaveTextContent("Borrowed One");
    expect(within(slotEl(3, 2)).getByTestId("matcher-slot-name")).toHaveTextContent("Borrowed Two");
    // ...and the CODE does not. The borrowed records are numbered 1 and 2; volume 1
    // is a real volume of this container, and this is the collision that would put
    // these two files on top of it.
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-code")).toHaveTextContent("V3-01");
    expect(within(slotEl(3, 2)).getByTestId("matcher-slot-code")).toHaveTextContent("V3-02");
    // The borrowed numbering appears once, as provenance.
    const pin = within(slotEl(3, 1)).getByTestId("matcher-slot-pin");
    expect(pin).toHaveTextContent("Record from V1-01");
    expect(pin).toHaveTextContent("collection 77777");

    await user.click(screen.getByTestId("matcher-apply"));
    // Nothing about the files changed, so the payload's own half is empty — and the
    // records ride in the SAME call, because the Titles they decorate may not exist
    // until it commits.
    expect(apply).toHaveBeenCalledWith({
      files: [],
      slots: [
        { group: 3, slot: 1, record: { externalId: "77777", ...BORROWED[0] } },
        { group: 3, slot: 2, record: { externalId: "77777", ...BORROWED[1] } },
      ],
    });
  });

  it("says so when the two runs are different lengths, instead of guessing", async () => {
    const user = userEvent.setup();
    const { apply } = setupRepoint(BORROWED.slice(0, 1));
    await user.click(groupToggle(3));
    await user.click(screen.getByTestId("matcher-group-fill-records"));
    await user.click(screen.getByTestId("fake-picker-pick"));

    expect(screen.getByTestId("matcher-repoint-note")).toHaveTextContent("Filled 1 of 2 chapters");
    // The Slot the run did not reach keeps the record it had — clearing it is a
    // decision the Admin did not make.
    expect(within(slotEl(3, 2)).getByTestId("matcher-slot-name")).toHaveTextContent(
      "Sins of the Father",
    );
    expect(within(slotEl(3, 2)).queryByTestId("matcher-slot-pin")).toBeNull();

    await user.click(screen.getByTestId("matcher-apply"));
    expect(apply).toHaveBeenCalledWith({
      files: [],
      slots: [{ group: 3, slot: 1, record: { externalId: "77777", ...BORROWED[0] } }],
    });
  });

  it("repoints one slot on its own, for the one-off", async () => {
    const user = userEvent.setup();
    const { picker } = setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 2);
    await user.click(screen.getByTestId("matcher-slot-repoint"));

    expect(picker.held.request?.targets).toEqual([{ group: 3, slot: 2 }]);
    await user.click(screen.getByTestId("fake-picker-pick"));
    // Only the Slot that was asked about moves; the run stops at one target.
    expect(within(slotEl(3, 2)).getByTestId("matcher-slot-name")).toHaveTextContent("Borrowed One");
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).toHaveTextContent(
      "Holiday Nights",
    );
  });

  it("offers the record actions on the title line, behind one kebab", async () => {
    // The actions are corrections, reached rarely — they belong behind a menu on the
    // Slot's own line, not on a permanent second row under every filled Slot. And
    // "use this chapter's own record" is meaningless until one has been borrowed.
    const user = userEvent.setup();
    setupRepoint();
    await user.click(groupToggle(3));

    expect(screen.queryByTestId("matcher-slot-menu")).toBeNull();
    await openSlotMenu(user, 3, 1);
    expect(screen.getByTestId("matcher-slot-repoint")).toHaveTextContent(
      "Take the record from elsewhere",
    );
    expect(screen.queryByTestId("matcher-slot-clear-record")).toBeNull();

    await user.click(screen.getByTestId("matcher-slot-repoint"));
    await user.click(screen.getByTestId("fake-picker-pick"));

    // Borrowed: the same item now offers to REPLACE the record, and giving it back
    // has joined the menu beneath it.
    await openSlotMenu(user, 3, 1);
    expect(screen.getByTestId("matcher-slot-repoint")).toHaveTextContent(
      "Use a different record",
    );
    expect(screen.getByTestId("matcher-slot-clear-record")).toBeInTheDocument();
  });

  it("closes the kebab on Escape without touching the record", async () => {
    const user = userEvent.setup();
    const { apply } = setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 1);

    await user.keyboard("{Escape}");
    expect(screen.queryByTestId("matcher-slot-menu")).toBeNull();
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("clears a record back to the container's own", async () => {
    const user = userEvent.setup();
    const { apply } = setupRepoint();
    await user.click(groupToggle(3));
    await openSlotMenu(user, 3, 1);
    await user.click(screen.getByTestId("matcher-slot-repoint"));
    await user.click(screen.getByTestId("fake-picker-pick"));
    await openSlotMenu(user, 3, 1);
    await user.click(screen.getByTestId("matcher-slot-clear-record"));

    // Back to this container's own record at this position.
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).toHaveTextContent(
      "Holiday Nights",
    );
    expect(within(slotEl(3, 1)).queryByTestId("matcher-slot-pin")).toBeNull();
    // Clearing a record the SERVER never had is not a change, so there is nothing
    // to apply — the draft collapsed back to "untouched".
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("counts a repointed record as a change, and Revert undoes it", async () => {
    const user = userEvent.setup();
    const { apply } = setupRepoint();
    await user.click(groupToggle(3));
    await user.click(screen.getByTestId("matcher-group-fill-records"));
    await user.click(screen.getByTestId("fake-picker-pick"));

    expect(screen.getByTestId("matcher-apply")).toHaveTextContent("Apply 2 changes");

    // A record set in the session must not be the one change Revert cannot undo.
    await user.click(screen.getByTestId("matcher-revert"));
    expect(within(slotEl(3, 1)).getByTestId("matcher-slot-name")).toHaveTextContent(
      "Holiday Nights",
    );
    expect(within(slotEl(3, 1)).queryByTestId("matcher-slot-pin")).toBeNull();
    expect(screen.getByTestId("matcher-apply")).toBeDisabled();
    expect(apply).not.toHaveBeenCalled();
  });

  it("borrows a record for a slot the container's own series cannot decorate", async () => {
    // The Batman shape in miniature: a group with a file on it and no record of its
    // own. Bare before, borrowed after, and still V4-01 either way.
    const user = userEvent.setup();
    const bare = doc({
      groups: [group({ number: 4, slotCount: 1, slotsLoaded: true, slots: [{ group: 4, slot: 1 }] })],
      files: [
        file({
          path: A,
          parsed: [{ group: 3, slot: 1 }],
          placements: [{ group: 4, slot: 1, ordinal: 1 }],
          decided: true,
        }),
      ],
    });
    const picker = fakePicker(BORROWED);
    const apply = vi.fn().mockResolvedValue(bare);
    render(
      <FileMatcher
        matcher={bare}
        labels={labels}
        loadGroup={loadGroup}
        apply={apply}
        repointRecord={picker.render}
        onClose={vi.fn()}
      />,
    );
    await user.click(groupToggle(4));
    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-bare")).toBeInTheDocument();

    await openSlotMenu(user, 4, 1);
    await user.click(screen.getByTestId("matcher-slot-repoint"));
    await user.click(screen.getByTestId("fake-picker-pick"));

    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-name")).toHaveTextContent("Borrowed One");
    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-code")).toHaveTextContent("V4-01");
    expect(within(slotEl(4, 1)).getByTestId("matcher-slot-pin")).toHaveTextContent("Record from V1-01");
  });
});

// A file the container's own filenames place perfectly and ffprobe cannot read.
//
// This is the one disagreement between this screen and the catalog that placing cannot
// resolve: the name numbers it, so it sits on its Slot looking finished, and no Title was ever
// built from it. A screen that stayed silent would report the one file in the library that
// needs a human as the most correct thing on it — which is exactly what an Admin saw while the
// Needs-Fixing queue told them the same file was "not recognized as a title" (ADR-0047).
describe("a file that could not be read", () => {
  it("says so on the slot it is placed on", async () => {
    const user = userEvent.setup();
    setup({
      files: [
        file({
          path: A,
          parsed: [{ group: 3, slot: 1 }],
          placements: [{ group: 3, slot: 1, ordinal: 1 }],
          unreadable: true,
          reason: "ffprobe: Invalid data found when processing input",
        }),
        file({
          path: B,
          parsed: [{ group: 3, slot: 2 }],
          placements: [{ group: 3, slot: 2, ordinal: 1 }],
        }),
      ],
    });
    await user.click(groupToggle(3));

    expect(within(partEl(A)).getByTestId("matcher-part-unreadable")).toBeInTheDocument();
    // The readable file beside it carries no such claim.
    expect(within(partEl(B)).queryByTestId("matcher-part-unreadable")).toBeNull();
  });

  it("says so in the tray when it is not placed", () => {
    setup({
      files: [file({ path: LOOSE, state: "unassigned", unreadable: true, reason: "broken" })],
    });
    const tray = screen.getByTestId("matcher-unsorted");
    expect(within(tray).getByTestId("matcher-file-unreadable")).toBeInTheDocument();
  });
});
