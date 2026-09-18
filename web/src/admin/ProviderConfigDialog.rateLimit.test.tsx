import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MetadataProvider } from "../api/types";

// The request throttle is ONE server-wide setting that now paces every Metadata
// provider (ADR-0059 decision 5), so the control renders in every provider's
// dialog — not only MusicBrainz's, which is the only place it used to appear
// because it was the only source the host handed it to. This exercises the case
// the change is really about: a fill-only supplement that has never had a
// rate-limit control, saving through the same field.

const { updateMetadataProviders, testMetadataProvider } = vi.hoisted(() => ({
  updateMetadataProviders: vi.fn(),
  testMetadataProvider: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      updateMetadataProviders: (...a: unknown[]) => updateMetadataProviders(...a),
      testMetadataProvider: (...a: unknown[]) => testMetadataProvider(...a),
    },
  };
});

import ProviderConfigDialog from "./ProviderConfigDialog";

const omdb: MetadataProvider = {
  slug: "omdb",
  name: "OMDb API",
  kinds: ["video"],
  role: "supplement",
  requiresKey: true,
  enabled: true,
  hasKey: true,
  baseURL: "https://www.omdbapi.com",
  description: "Fills a movie's plot and content rating.",
  docsURL: "https://example.test/key",
};

beforeEach(() => {
  vi.clearAllMocks();
  // jsdom has no <dialog> showModal.
  if (!HTMLDialogElement.prototype.showModal) {
    HTMLDialogElement.prototype.showModal = function showModal() {
      this.open = true;
    };
  }
  if (!HTMLDialogElement.prototype.close) {
    HTMLDialogElement.prototype.close = function close() {
      this.open = false;
    };
  }
});

describe("ProviderConfigDialog rate limit", () => {
  it("renders the throttle for a supplement and round-trips a change", async () => {
    const user = userEvent.setup();
    updateMetadataProviders.mockResolvedValue({
      providers: [omdb],
      metadataLanguage: "en-US",
      autoEnrichAfterScan: true,
      enrichIntervalSeconds: 0,
      musicBrainzRateLimitMs: 250,
    });
    render(
      <ProviderConfigDialog
        provider={omdb}
        musicBrainzRateLimitMs={1000}
        onSaved={() => {}}
        onClose={() => {}}
      />,
    );

    const rate = (await screen.findByTestId(
      "musicbrainz-rate-limit-input",
    )) as HTMLInputElement;
    expect(rate.value).toBe("1000"); // seeded from the server-wide value

    await user.clear(rate);
    await user.type(rate, "250");
    await user.click(screen.getByTestId("provider-config-save-omdb"));

    await waitFor(() =>
      expect(updateMetadataProviders).toHaveBeenCalledWith({ musicBrainzRateLimitMs: 250 }),
    );
  });
});
