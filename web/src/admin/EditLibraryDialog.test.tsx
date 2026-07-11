import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { EnrichmentPolicy, Library } from "../api/types";

// EditLibraryDialog is now TABBED (ADR-0027): a "General" tab (rename / add-folder /
// delete) and a "Metadata Providers" tab (the per-Library Enrichment policy). These
// tests cover the tab switching, that the General form still works, and that the
// policy panel is LAZY — its policy is fetched only when the Metadata Providers tab
// is first shown, not on dialog open. The full policy-panel behavior lives in
// EnrichmentPolicyPanel.test.tsx; here we only assert the panel mounts + loads.

const { updateLibrary, deleteLibrary, getEnrichmentPolicy, updateEnrichmentPolicy } =
  vi.hoisted(() => ({
    updateLibrary: vi.fn(),
    deleteLibrary: vi.fn(),
    getEnrichmentPolicy: vi.fn(),
    updateEnrichmentPolicy: vi.fn(),
  }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      updateLibrary: (...a: unknown[]) => updateLibrary(...a),
      deleteLibrary: (...a: unknown[]) => deleteLibrary(...a),
      getEnrichmentPolicy: (...a: unknown[]) => getEnrichmentPolicy(...a),
      updateEnrichmentPolicy: (...a: unknown[]) => updateEnrichmentPolicy(...a),
    },
  };
});

import EditLibraryDialog from "./EditLibraryDialog";

function lib(over: Partial<Library> = {}): Library {
  return {
    id: "lib1",
    name: "Home Videos",
    kind: "movie",
    rootFolders: [{ id: "r1", path: "/media/home" }],
    ...over,
  };
}

function policy(over: Partial<EnrichmentPolicy> = {}): EnrichmentPolicy {
  return {
    enrichEnabled: null,
    inheritedEnrichEnabled: true,
    effective: { video: true, music: true },
    metadataLanguage: null,
    inheritedMetadataLanguage: "en-US",
    authoritativeProvider: null,
    inheritedAuthoritative: { slug: "tmdb", name: "The Movie Database (TMDB)" },
    effectiveAuthoritative: { slug: "tmdb", name: "The Movie Database (TMDB)" },
    authoritativeUnreachable: null,
    authoritativeCandidates: [{ slug: "tmdb", name: "The Movie Database (TMDB)" }],
    supplements: [{ slug: "omdb", name: "OMDb API", override: null, inheritedEnabled: true }],
    ...over,
  };
}

function renderDialog(over: Partial<Library> = {}) {
  return render(
    <EditLibraryDialog
      library={lib(over)}
      onChanged={() => {}}
      onDeleted={() => {}}
      onClose={() => {}}
    />,
  );
}

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) {
    this.open = true;
  });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.open = false;
    this.dispatchEvent(new Event("close"));
  });
  updateLibrary.mockReset();
  deleteLibrary.mockReset();
  getEnrichmentPolicy.mockReset();
  updateEnrichmentPolicy.mockReset();
});

describe("EditLibraryDialog tabs", () => {
  it("opens on the General tab with the rename/roots form and no policy fetch", () => {
    renderDialog();

    // General is the active tab and its form is visible.
    expect(screen.getByTestId("edit-library-tab-general")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(screen.getByTestId("edit-library-name-input")).toBeInTheDocument();
    expect(screen.getByTestId("edit-library-roots")).toBeInTheDocument();
    // The policy panel is NOT mounted, so nothing was fetched yet (lazy).
    expect(getEnrichmentPolicy).not.toHaveBeenCalled();
    expect(screen.queryByTestId("enrichment-policy-panel")).toBeNull();
  });

  it("switching to Metadata Providers mounts and lazy-loads the policy panel", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    renderDialog();

    await user.click(screen.getByTestId("edit-library-tab-metadata-providers"));

    // The tab becomes active and the panel loads the policy on mount.
    expect(screen.getByTestId("edit-library-tab-metadata-providers")).toHaveAttribute(
      "aria-selected",
      "true",
    );
    await waitFor(() =>
      expect(getEnrichmentPolicy).toHaveBeenCalledWith("lib1", expect.anything()),
    );
    expect(await screen.findByTestId("enrich-enabled-control")).toBeInTheDocument();
    // General's form is no longer mounted.
    expect(screen.queryByTestId("edit-library-name-input")).toBeNull();

    // Switching back shows General again.
    await user.click(screen.getByTestId("edit-library-tab-general"));
    expect(screen.getByTestId("edit-library-name-input")).toBeInTheDocument();
    expect(screen.queryByTestId("enrich-enabled-control")).toBeNull();
  });

  it("the General tab rename still works (PATCHes the library)", async () => {
    const user = userEvent.setup();
    updateLibrary.mockResolvedValue(lib({ name: "Family Videos" }));
    renderDialog();

    const input = screen.getByTestId("edit-library-name-input");
    await user.clear(input);
    await user.type(input, "Family Videos");
    await user.click(screen.getByTestId("edit-library-save-name"));

    await waitFor(() =>
      expect(updateLibrary).toHaveBeenCalledWith("lib1", { name: "Family Videos" }),
    );
    // No enrichment fetch happened just from using General.
    expect(getEnrichmentPolicy).not.toHaveBeenCalled();
  });

  it("renders a policy control inside the Metadata Providers tab body", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    renderDialog();

    await user.click(screen.getByTestId("edit-library-tab-metadata-providers"));
    const panel = await screen.findByTestId("enrichment-policy-panel");
    expect(within(panel).getByTestId("authoritative-select")).toBeInTheDocument();
    expect(within(panel).getByTestId("supplement-omdb")).toBeInTheDocument();
  });
});
