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
    metadataLanguage: null,
    inheritedMetadataLanguage: "en-US",
    authoritativeProvider: null,
    inheritedAuthoritative: { slug: "tmdb", name: "The Movie Database (TMDB)" },
    effectiveAuthoritative: { slug: "tmdb", name: "The Movie Database (TMDB)" },
    authoritativeUnreachable: null,
    authoritativeCandidates: [
      { slug: "tmdb", name: "The Movie Database (TMDB)" },
      { slug: "omdb", name: "OMDb API" },
    ],
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

  it("shows the metadata-language control inheriting by default with the global as placeholder", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const control = await screen.findByTestId("metadata-language-control");
    expect(control).toHaveTextContent("Inherited");
    const input = within(control).getByTestId("metadata-language-input") as HTMLInputElement;
    expect(input.value).toBe("");
    expect(input.placeholder).toBe("Inherit (en-US)");
    // No reset affordance while inheriting.
    expect(screen.queryByTestId("metadata-language-reset")).toBeNull();
  });

  it("committing a language PUTs the override and shows Overridden + a reset", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    updateEnrichmentPolicy.mockResolvedValue(policy({ metadataLanguage: "ja-JP" }));
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const input = await screen.findByTestId("metadata-language-input");
    await user.type(input, "ja-JP");
    await user.tab(); // blur commits

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { metadataLanguage: "ja-JP" }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("metadata-language-control")).toHaveTextContent("Overridden"),
    );
    expect(screen.getByTestId("metadata-language-reset")).toBeInTheDocument();
  });

  it("reset-to-inherit clears the language override (metadataLanguage=null)", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy({ metadataLanguage: "ja-JP" }));
    updateEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    await waitFor(() =>
      expect(screen.getByTestId("metadata-language-control")).toHaveTextContent("Overridden"),
    );
    await user.click(screen.getByTestId("metadata-language-reset"));

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { metadataLanguage: null }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("metadata-language-control")).toHaveTextContent("Inherited"),
    );
  });

  it("a blur with no change to the language makes no request", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const input = await screen.findByTestId("metadata-language-input");
    await user.click(input);
    await user.tab();
    expect(updateEnrichmentPolicy).not.toHaveBeenCalled();
  });

  it("lists the authoritative candidates and inherits by default", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const control = await screen.findByTestId("authoritative-control");
    expect(control).toHaveTextContent("Inherited");
    const select = within(control).getByTestId("authoritative-select") as HTMLSelectElement;
    expect(select.value).toBe("inherit");
    // The inherit option plus each candidate.
    expect(within(select).getByText(/Inherit \(default: The Movie Database/)).toBeInTheDocument();
    expect(within(select).getByRole("option", { name: "OMDb API" })).toBeInTheDocument();
    expect(screen.queryByTestId("authoritative-reset")).toBeNull();
  });

  it("picking an authoritative PUTs the slug and shows Overridden + reset", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    updateEnrichmentPolicy.mockResolvedValue(
      policy({
        authoritativeProvider: "omdb",
        effectiveAuthoritative: { slug: "omdb", name: "OMDb API" },
      }),
    );
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const select = await screen.findByTestId("authoritative-select");
    await user.selectOptions(select, "omdb");

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { authoritativeProvider: "omdb" }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("authoritative-control")).toHaveTextContent("Overridden"),
    );
    expect(screen.getByTestId("authoritative-reset")).toBeInTheDocument();
    expect(screen.getByTestId("authoritative-effective")).toHaveTextContent(/Leads with.*OMDb API/);
  });

  it("resetting the authoritative clears it (authoritativeProvider=null)", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(
      policy({ authoritativeProvider: "omdb", effectiveAuthoritative: { slug: "omdb", name: "OMDb API" } }),
    );
    updateEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    await waitFor(() =>
      expect(screen.getByTestId("authoritative-control")).toHaveTextContent("Overridden"),
    );
    await user.click(screen.getByTestId("authoritative-reset"));

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { authoritativeProvider: null }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("authoritative-control")).toHaveTextContent("Inherited"),
    );
  });

  it("surfaces an unreachable chosen authoritative", async () => {
    getEnrichmentPolicy.mockResolvedValue(
      policy({
        authoritativeProvider: "omdb",
        authoritativeUnreachable: "omdb",
        effectiveAuthoritative: { slug: "tmdb", name: "The Movie Database (TMDB)" },
      }),
    );
    render(<EnrichmentPolicyDialog library={lib()} onClose={() => {}} />);

    const warn = await screen.findByTestId("authoritative-unreachable");
    expect(warn).toHaveTextContent(/no longer\s+usable/);
    expect(warn).toHaveTextContent(/The Movie Database/);
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
