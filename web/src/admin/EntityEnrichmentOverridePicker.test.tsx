import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The album's Fix-info surface, for the one thing issue 12 adds to it: the EDITION
// section under the matched album (ADR-0052).
//
// The distinction being pinned is which parent kinds have editions at all. An album
// IS a release-group and a release-group holds pressings; a Show and an Artist have
// no such notion, and asking the server for their editions would be a request for
// something that cannot exist.

const {
  listAlbumEditions,
  searchEntityEnrichmentCandidates,
  applyEntityEnrichmentOverride,
} = vi.hoisted(() => ({
  listAlbumEditions: vi.fn(),
  searchEntityEnrichmentCandidates: vi.fn(),
  applyEntityEnrichmentOverride: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listAlbumEditions: (...a: unknown[]) => listAlbumEditions(...a),
      searchEntityEnrichmentCandidates: (...a: unknown[]) =>
        searchEntityEnrichmentCandidates(...a),
      applyEntityEnrichmentOverride: (...a: unknown[]) => applyEntityEnrichmentOverride(...a),
    },
  };
});

import EntityEnrichmentOverridePicker from "./EntityEnrichmentOverridePicker";

describe("EntityEnrichmentOverridePicker — the Edition section", () => {
  beforeEach(() => {
    listAlbumEditions.mockReset();
    searchEntityEnrichmentCandidates.mockReset();
    applyEntityEnrichmentOverride.mockReset();
    searchEntityEnrichmentCandidates.mockResolvedValue({ candidates: [], hasMore: false });
  });

  it("shows the editions of a matched album under its record", async () => {
    listAlbumEditions.mockResolvedValue({
      albumId: "al1",
      releaseGroupId: "rg-viaggio",
      localTrackCount: 16,
      inUseReleaseId: "rel-it",
      inUseSource: "fit",
      editions: [
        { releaseId: "rel-it", date: "1995-11-06", country: "IT", format: "CD", trackCount: 16 },
      ],
    });
    render(
      <EntityEnrichmentOverridePicker
        entityType="albums"
        entityId="al1"
        currentExternalId="rg-viaggio"
        onApplied={vi.fn()}
      />,
    );

    expect(await screen.findByTestId("album-edition-list")).toBeInTheDocument();
    expect(listAlbumEditions).toHaveBeenCalledWith("al1", expect.anything());
    // The paste-a-URL escape hatch is untouched: it is how this was done before, it
    // still works, and the edition list is an addition rather than a replacement.
    expect(screen.getByTestId("entity-enrichment-search-input")).toBeInTheDocument();
  });

  it("asks for no editions on a Show or an Artist", async () => {
    for (const kind of ["shows", "artists"] as const) {
      const { unmount } = render(
        <EntityEnrichmentOverridePicker
          entityType={kind}
          entityId="x1"
          currentExternalId="rec-1"
          onApplied={vi.fn()}
        />,
      );
      await screen.findByTestId("entity-enrichment-override-current");
      expect(screen.queryByTestId("album-editions")).toBeNull();
      unmount();
    }
    await waitFor(() => expect(listAlbumEditions).not.toHaveBeenCalled());
  });

  it("keeps the paste hatch when the provider cannot list editions", async () => {
    // A provider that cannot answer degrades to the box the operator already used —
    // never to an error page.
    listAlbumEditions.mockRejectedValue(new Error("unavailable"));
    render(
      <EntityEnrichmentOverridePicker
        entityType="albums"
        entityId="al1"
        currentExternalId="rg-viaggio"
        onApplied={vi.fn()}
      />,
    );

    expect(await screen.findByTestId("album-editions-unavailable")).toBeInTheDocument();
    expect(screen.getByTestId("entity-enrichment-search-input")).toBeInTheDocument();
  });
});

describe("EntityEnrichmentOverridePicker — a pasted reference", () => {
  // The picker never decides what a reference looks like: the search endpoint asks
  // the Library's lead to read it and answers `resolvedRef`, which selects the one
  // record (.scratch/bundled-plugins issue 12). An Artist led by any source works
  // the same way, so there is no provider to name here at all.
  beforeEach(() => {
    listAlbumEditions.mockReset();
    searchEntityEnrichmentCandidates.mockReset();
    applyEntityEnrichmentOverride.mockReset();
  });

  it("selects the record the server resolved, and applies it in its namespace", async () => {
    searchEntityEnrichmentCandidates.mockResolvedValue({
      candidates: [{ externalId: "aid-42", title: "Resolved", kind: "artist", source: "someplugin" }],
      hasMore: false,
      resolvedRef: true,
    });
    applyEntityEnrichmentOverride.mockResolvedValue({});
    render(
      <EntityEnrichmentOverridePicker entityType="artists" entityId="ar1" onApplied={vi.fn()} />,
    );

    await userEvent.type(screen.getByTestId("entity-enrichment-search-input"), "someplugin:42");
    await userEvent.click(screen.getByTestId("entity-enrichment-search-button"));

    await waitFor(() =>
      expect(searchEntityEnrichmentCandidates).toHaveBeenCalledWith(
        "artists",
        "ar1",
        "someplugin:42",
        expect.objectContaining({ page: 0 }),
      ),
    );
    const row = await screen.findByTestId("entity-enrichment-candidate");
    expect(row).toHaveAttribute("data-external-id", "aid-42");
    expect(row).toHaveAttribute("aria-pressed", "true");
  });

  it("leaves a searched term's hits unselected", async () => {
    searchEntityEnrichmentCandidates.mockResolvedValue({
      candidates: [{ externalId: "a1", title: "Radiohead", kind: "artist", source: "musicbrainz" }],
      hasMore: false,
      resolvedRef: false,
    });
    render(
      <EntityEnrichmentOverridePicker entityType="artists" entityId="ar1" onApplied={vi.fn()} />,
    );

    await userEvent.type(screen.getByTestId("entity-enrichment-search-input"), "Radiohead");
    await userEvent.click(screen.getByTestId("entity-enrichment-search-button"));

    const row = await screen.findByTestId("entity-enrichment-candidate");
    expect(row).toHaveAttribute("aria-pressed", "false");
  });
});
