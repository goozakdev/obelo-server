import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { AlbumEditions, EntityEnrichmentDetail } from "../api/types";

// The Edition section (ADR-0052, album-resolves-its-tracks/12) — the half of the
// correction that used to require leaving the product:
//
//   "requires going to the web page and getting a URL because i was unable to
//    select a specific edition. It shows the best guess, but I cant choose a
//    specific edition out of that release-group."
//
// The fixture is the album that produced that sentence: Andrea Bocelli's *Viaggio
// Italiano*, sixteen local tracks, matched to a release-group holding three
// pressings — two that fit and a deluxe that does not.

const { listAlbumEditions, applyEntityEnrichmentOverride } = vi.hoisted(() => ({
  listAlbumEditions: vi.fn(),
  applyEntityEnrichmentOverride: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listAlbumEditions: (...a: unknown[]) => listAlbumEditions(...a),
      applyEntityEnrichmentOverride: (...a: unknown[]) => applyEntityEnrichmentOverride(...a),
    },
  };
});

import AlbumEditionPicker from "./AlbumEditionPicker";

const RG = "054a22c3-742e-34d3-8ebf-ef912e3679e6";
const IT = "rel-italian";
const XE = "rel-international";
const DELUXE = "rel-deluxe";

function editions(over: Partial<AlbumEditions> = {}): AlbumEditions {
  return {
    albumId: "al1",
    releaseGroupId: RG,
    localTrackCount: 16,
    inUseReleaseId: IT,
    inUseSource: "fit",
    editions: [
      { releaseId: IT, date: "1995-11-06", country: "IT", format: "CD", trackCount: 16 },
      {
        releaseId: XE,
        date: "1996-02-27",
        country: "XE",
        format: "CD",
        trackCount: 16,
        disambiguation: "international edition",
      },
      {
        releaseId: DELUXE,
        date: "2000-01-01",
        country: "US",
        format: "2×CD",
        trackCount: 20,
        disambiguation: "deluxe edition",
      },
    ],
    ...over,
  };
}

function detail(over: Partial<EntityEnrichmentDetail> = {}): EntityEnrichmentDetail {
  return {
    entityType: "album",
    entityId: "al1",
    enrichmentOverride: { externalId: RG, releaseId: XE },
    cascade: { updated: 14, attention: 2 },
    ...over,
  };
}

describe("AlbumEditionPicker", () => {
  beforeEach(() => {
    listAlbumEditions.mockReset();
    applyEntityEnrichmentOverride.mockReset();
  });

  it("lists every edition with date, country, format and track count", async () => {
    // The four facts that tell two pressings apart, on one line each, plus the
    // source's own disambiguation where it has one.
    listAlbumEditions.mockResolvedValue(editions());
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    expect(rows).toHaveLength(3);
    expect(within(rows[0]).getByTestId("album-edition-line")).toHaveTextContent(
      "1995-11-06 · IT · CD · 16 tracks",
    );
    expect(within(rows[2]).getByTestId("album-edition-line")).toHaveTextContent(
      "2000-01-01 · US · 2×CD · 20 tracks",
    );
    expect(rows[2]).toHaveTextContent("deluxe edition");
  });

  it("states the local album's track count beside the list", async () => {
    // Without it the Admin is comparing sixteen numbers against a number that is not
    // on the screen. With it, the deluxe's 20 is wrong at a glance.
    listAlbumEditions.mockResolvedValue(editions());
    render(<AlbumEditionPicker albumId="al1" />);

    expect(await screen.findByTestId("album-editions-local-count")).toHaveTextContent(
      "This album has 16 tracks",
    );
    const rows = screen.getAllByTestId("album-edition");
    expect(within(rows[0]).getByTestId("album-edition-fits")).toBeInTheDocument();
    expect(within(rows[2]).queryByTestId("album-edition-fits")).toBeNull();
  });

  it("marks the edition in use and says whose answer it is", async () => {
    // "It shows the best guess" — now it says so, which is what makes the list worth
    // reading: the marker names the thing the operator was arguing with.
    listAlbumEditions.mockResolvedValue(editions());
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    expect(within(rows[0]).getByTestId("album-edition-in-use")).toHaveTextContent(
      "In use — best guess",
    );
    expect(within(rows[1]).queryByTestId("album-edition-in-use")).toBeNull();
  });

  it("says when the edition in use is the Admin's own choice", async () => {
    listAlbumEditions.mockResolvedValue(
      editions({ inUseReleaseId: XE, inUseSource: "chosen", chosenReleaseId: XE }),
    );
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    expect(within(rows[1]).getByTestId("album-edition-in-use")).toHaveTextContent(
      "In use — your choice",
    );
    // A row already chosen offers no button: there is nothing left to assert.
    expect(within(rows[1]).queryByTestId("album-edition-use")).toBeNull();
    // The guess-tier rows still do.
    expect(within(rows[0]).getByTestId("album-edition-use")).toBeInTheDocument();
  });

  it("says when the edition in use came from the file tags", async () => {
    listAlbumEditions.mockResolvedValue(
      editions({ inUseReleaseId: DELUXE, inUseSource: "tagged" }),
    );
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    expect(within(rows[2]).getByTestId("album-edition-in-use")).toHaveTextContent(
      "In use — from the file tags",
    );
    // Still offered, and NOT a no-op: choosing it turns a tag into a human's
    // assertion, which is what licenses position-alone mapping (ADR-0052).
    expect(within(rows[2]).getByTestId("album-edition-use")).toBeInTheDocument();
  });

  it("applies a chosen edition through the album override, with the cascade", async () => {
    // THE gesture. One call, the album's EXISTING apply — release-group as the
    // record, the picked release as the edition, cascade on — and no second apply
    // path anywhere.
    listAlbumEditions.mockResolvedValue(editions());
    applyEntityEnrichmentOverride.mockResolvedValue(detail());
    const onApplied = vi.fn();
    render(<AlbumEditionPicker albumId="al1" onApplied={onApplied} />);

    const rows = await screen.findAllByTestId("album-edition");
    await userEvent.click(within(rows[1]).getByTestId("album-edition-use"));

    await waitFor(() =>
      expect(applyEntityEnrichmentOverride).toHaveBeenCalledWith("albums", "al1", RG, true, XE),
    );
    expect(onApplied).toHaveBeenCalled();
  });

  it("reports the mapped and attention counts after applying", async () => {
    // The operator's proof the pin was right. A wrong pin propagates confidently
    // (ADR-0052), and the count is what makes a nonsense one visible immediately.
    listAlbumEditions.mockResolvedValue(editions());
    applyEntityEnrichmentOverride.mockResolvedValue(detail());
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    await userEvent.click(within(rows[1]).getByTestId("album-edition-use"));

    expect(await screen.findByTestId("album-edition-cascade")).toHaveTextContent(
      "14 of 16 tracks matched from the album’s track list. 2 still need attention.",
    );
  });

  it("re-reads the list after applying, so the marker moves onto the picked row", async () => {
    // Without the re-read the section would still be describing the state before the
    // click — the Admin's own choice still labelled somebody else's guess.
    listAlbumEditions
      .mockResolvedValueOnce(editions())
      .mockResolvedValueOnce(
        editions({ inUseReleaseId: XE, inUseSource: "chosen", chosenReleaseId: XE }),
      );
    applyEntityEnrichmentOverride.mockResolvedValue(detail());
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    await userEvent.click(within(rows[1]).getByTestId("album-edition-use"));

    await waitFor(() =>
      expect(screen.getByTestId("album-edition-in-use")).toHaveTextContent(
        "In use — your choice",
      ),
    );
  });

  it("renders a release-group holding exactly one release", async () => {
    // Not a degenerate case to skip: it is the confirmation that there was no choice
    // to make, and an empty section is indistinguishable from a broken one.
    listAlbumEditions.mockResolvedValue(
      editions({
        inUseReleaseId: IT,
        editions: [{ releaseId: IT, date: "1995-11-06", country: "IT", format: "CD", trackCount: 16 }],
      }),
    );
    render(<AlbumEditionPicker albumId="al1" />);

    const rows = await screen.findAllByTestId("album-edition");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByTestId("album-edition-in-use")).toBeInTheDocument();
  });

  it("degrades to the paste hatch when the provider cannot list editions", async () => {
    // A provider that cannot answer is not an error page. The pasted-URL escape
    // hatch above still works, and the hint points at it.
    listAlbumEditions.mockRejectedValue(new Error("metadata provider search is unavailable"));
    render(<AlbumEditionPicker albumId="al1" />);

    expect(await screen.findByTestId("album-editions-unavailable")).toHaveTextContent(
      /paste a MusicBrainz release URL/i,
    );
    expect(screen.queryByTestId("album-edition-list")).toBeNull();
  });

  it("renders nothing for an album with no matched release-group", async () => {
    // Nothing to choose from: the album's own match is the thing to fix, and the
    // record picker above this section is where that happens.
    listAlbumEditions.mockResolvedValue({ albumId: "al1", localTrackCount: 16, editions: [] });
    render(<AlbumEditionPicker albumId="al1" />);

    await waitFor(() => expect(listAlbumEditions).toHaveBeenCalled());
    expect(screen.queryByTestId("album-editions")).toBeNull();
  });
});
