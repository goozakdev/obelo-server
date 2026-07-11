import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { EnrichmentPolicy, Library } from "../api/types";

// EnrichmentPolicyDialog (ADR-0027, slice 01) through the faked API client (the
// one seam): the enrich on/off tri-state shows inherited-vs-overridden state,
// selecting Off / Inherit PUTs the right override (false / null), and the view
// reflects the fresh effective enablement the server returns.

const { getEnrichmentPolicy, updateEnrichmentPolicy } = vi.hoisted(() => ({
  getEnrichmentPolicy: vi.fn(),
  updateEnrichmentPolicy: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getEnrichmentPolicy: (...a: unknown[]) => getEnrichmentPolicy(...a),
      updateEnrichmentPolicy: (...a: unknown[]) => updateEnrichmentPolicy(...a),
    },
  };
});

import EnrichmentPolicyDialog from "./EnrichmentPolicyDialog";

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
    ...over,
  };
}

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = vi.fn(function (this: HTMLDialogElement) {
    this.open = true;
  });
  HTMLDialogElement.prototype.close = vi.fn(function (this: HTMLDialogElement) {
    this.open = false;
    this.dispatchEvent(new Event("close"));
  });
  getEnrichmentPolicy.mockReset();
  updateEnrichmentPolicy.mockReset();
});

describe("EnrichmentPolicyDialog", () => {
  it("shows the inherited default: Inherit active, Inherited badge, effective enablement", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const control = await screen.findByTestId("enrich-enabled-control");
    expect(getEnrichmentPolicy).toHaveBeenCalledWith("lib1", expect.anything());
    expect(within(control).getByTestId("enrich-enabled-inherit")).toHaveAttribute(
      "data-active",
      "true",
    );
    expect(within(control).getByTestId("enrich-enabled-inherit")).toHaveTextContent(
      /Inherit \(currently On\)/,
    );
    expect(control).toHaveTextContent("Inherited");
    expect(screen.getByTestId("enrich-effective")).toHaveTextContent(
      /will enrich \(video \+ music\)/,
    );
  });

  it("selecting Off PUTs enrichEnabled=false and reflects the switched-off view", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    updateEnrichmentPolicy.mockResolvedValue(
      policy({ enrichEnabled: false, effective: { video: false, music: false } }),
    );
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    await screen.findByTestId("enrich-enabled-control");
    await user.click(screen.getByTestId("enrich-enabled-off"));

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { enrichEnabled: false }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("enrich-enabled-off")).toHaveAttribute("data-active", "true"),
    );
    expect(screen.getByTestId("enrich-enabled-control")).toHaveTextContent("Overridden");
    expect(screen.getByTestId("enrich-effective")).toHaveTextContent(/will not enrich/);
  });

  it("Inherit clears the override (enrichEnabled=null) from an overridden state", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(
      policy({ enrichEnabled: false, effective: { video: false, music: false } }),
    );
    updateEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    // Starts overridden-off.
    await waitFor(() =>
      expect(screen.getByTestId("enrich-enabled-off")).toHaveAttribute("data-active", "true"),
    );
    await user.click(screen.getByTestId("enrich-enabled-inherit"));

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { enrichEnabled: null }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("enrich-enabled-inherit")).toHaveAttribute("data-active", "true"),
    );
    expect(screen.getByTestId("enrich-enabled-control")).toHaveTextContent("Inherited");
  });
});
