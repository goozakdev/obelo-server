import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { MetadataProvider } from "../api/types";

// R02-15: while a save is in flight the dialog must stay up. jsdom has no
// close-watcher, so the second ESC that a browser turns into a native close is
// simulated by hand: the dialog goes `open = false` and fires `close`. That close
// must not reach onClose (which would drop the save's result on the floor), and the
// dialog must be put back with showModal.

const { updateMetadataProviders } = vi.hoisted(() => ({ updateMetadataProviders: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      updateMetadataProviders: (...a: unknown[]) => updateMetadataProviders(...a),
      testMetadataProvider: vi.fn(),
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

let showModal: ReturnType<typeof vi.fn>;

beforeEach(() => {
  updateMetadataProviders.mockReset();
  showModal = vi.fn(function (this: HTMLDialogElement) {
    this.open = true;
  });
  HTMLDialogElement.prototype.showModal = showModal as unknown as () => void;
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.open = false;
  });
});

function nativeClose() {
  const dialog = screen.getByTestId("provider-config-dialog-omdb") as HTMLDialogElement;
  act(() => {
    dialog.open = false;
    dialog.dispatchEvent(new Event("close"));
  });
}

function renderDialog(onClose: () => void) {
  render(
    <ProviderConfigDialog
      provider={omdb}
      musicBrainzRateLimitMs={1000}
      onSaved={() => {}}
      onClose={onClose}
    />,
  );
}

describe("ProviderConfigDialog — a native close while saving (R02-15)", () => {
  it("re-opens instead of closing", async () => {
    updateMetadataProviders.mockReturnValue(new Promise(() => {}));
    const onClose = vi.fn();
    renderDialog(onClose);
    expect(showModal).toHaveBeenCalledTimes(1);

    const rate = await screen.findByTestId("musicbrainz-rate-limit-input");
    await userEvent.clear(rate);
    await userEvent.type(rate, "250");
    await userEvent.click(screen.getByTestId("provider-config-save-omdb"));
    expect(updateMetadataProviders).toHaveBeenCalledTimes(1);

    nativeClose();

    expect(onClose).not.toHaveBeenCalled();
    expect(showModal).toHaveBeenCalledTimes(2);
  });

  it("still closes on a native close when idle", () => {
    const onClose = vi.fn();
    renderDialog(onClose);

    nativeClose();

    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
