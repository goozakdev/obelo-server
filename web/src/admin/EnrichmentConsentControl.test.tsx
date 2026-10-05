import { StrictMode } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

// Under StrictMode (main.tsx) the mount effect runs, aborts, and runs again. The
// aborted first read must not leave its error on screen beside the loaded toggle
// (R02-11).

const { getEnrichmentConsent } = vi.hoisted(() => ({ getEnrichmentConsent: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: { getEnrichmentConsent: (...a: unknown[]) => getEnrichmentConsent(...a) },
  };
});

import EnrichmentConsentControl from "./EnrichmentConsentControl";

beforeEach(() => getEnrichmentConsent.mockReset());

describe("EnrichmentConsentControl", () => {
  it("shows no load error when the first, aborted read is rejected under StrictMode (R02-11)", async () => {
    getEnrichmentConsent.mockImplementation(
      (signal?: AbortSignal) =>
        new Promise((resolve, reject) => {
          if (signal?.aborted) return reject(new DOMException("aborted", "AbortError"));
          signal?.addEventListener("abort", () =>
            reject(new DOMException("aborted", "AbortError")),
          );
          setTimeout(() => resolve({ state: "granted", credentialSource: "operator" }), 0);
        }),
    );
    render(
      <StrictMode>
        <EnrichmentConsentControl />
      </StrictMode>,
    );

    await waitFor(() =>
      expect(screen.getByTestId("enrichment-consent-state")).toHaveAttribute(
        "data-state",
        "granted",
      ),
    );
    expect(screen.queryByTestId("enrichment-consent-control-error")).toBeNull();
  });
});
