import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The leaf-Title override picker. Its search/paging machinery is shared with the
// parent picker (enrichmentCandidates.tsx); this pins the one behaviour that
// matters for a stale box: "Show more" pages the query that produced the list.

const { searchEnrichmentCandidates } = vi.hoisted(() => ({
  searchEnrichmentCandidates: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      searchEnrichmentCandidates: (...a: unknown[]) => searchEnrichmentCandidates(...a),
    },
  };
});

import EnrichmentOverridePicker from "./EnrichmentOverridePicker";

describe("EnrichmentOverridePicker", () => {
  beforeEach(() => {
    searchEnrichmentCandidates.mockReset();
  });

  it("pages Show more with the query that produced the list, not the edited box (R02-05)", async () => {
    searchEnrichmentCandidates
      .mockResolvedValueOnce({
        candidates: [{ externalId: "m1", title: "Foo", kind: "movie", source: "tmdb" }],
        hasMore: true,
      })
      .mockResolvedValue({
        candidates: [{ externalId: "m2", title: "Foo 2", kind: "movie", source: "tmdb" }],
        hasMore: false,
      });
    render(<EnrichmentOverridePicker titleId="t1" onApplied={vi.fn()} />);

    const input = screen.getByTestId("enrichment-search-input");
    await userEvent.type(input, "foo");
    await userEvent.click(screen.getByTestId("enrichment-search-button"));
    await screen.findByTestId("enrichment-candidate");

    await userEvent.clear(input);
    await userEvent.type(input, "bar");
    await userEvent.click(screen.getByTestId("enrichment-show-more"));

    await waitFor(() => expect(searchEnrichmentCandidates).toHaveBeenCalledTimes(2));
    expect(searchEnrichmentCandidates).toHaveBeenLastCalledWith(
      "t1",
      "foo",
      expect.objectContaining({ page: 1 }),
    );
  });
});
