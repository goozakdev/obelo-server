import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { EnrichmentPolicy, Library } from "../api/types";

// EnrichmentPolicyPanel (ADR-0027) — the "Metadata Providers" tab of the Edit-
// Library dialog — through the faked API client (the one seam): the enrich on/off
// tri-state, the metadata-language control, the Authoritative-provider dropdown,
// and the per-Supplement tri-states each show inherited-vs-overridden state and PUT
// the right partial update, and the view reflects the fresh policy the server
// returns.

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

import EnrichmentPolicyPanel from "./EnrichmentPolicyPanel";

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
    supplements: [
      { slug: "omdb", name: "OMDb API", override: null, inheritedEnabled: true },
      { slug: "thetvdb", name: "TheTVDB", override: null, inheritedEnabled: false },
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

describe("EnrichmentPolicyPanel", () => {
  it("shows the inherited default: Inherit active, Inherited badge, effective enablement", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

    const input = await screen.findByTestId("metadata-language-input");
    await user.click(input);
    await user.tab();
    expect(updateEnrichmentPolicy).not.toHaveBeenCalled();
  });

  it("lists the authoritative candidates and inherits by default", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

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
    render(<EnrichmentPolicyPanel library={lib()} />);

    const warn = await screen.findByTestId("authoritative-unreachable");
    expect(warn).toHaveTextContent(/no longer\s+usable/);
    expect(warn).toHaveTextContent(/The Movie Database/);
  });

  it("renders a per-supplement tri-state inheriting by default", async () => {
    getEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyPanel library={lib()} />);

    const row = await screen.findByTestId("supplement-omdb");
    expect(within(row).getByTestId("supplement-omdb-inherit")).toHaveAttribute("data-active", "true");
    expect(within(row).getByTestId("supplement-omdb-inherit")).toHaveTextContent(/Inherit \(On\)/);
    // TheTVDB inherits Off (disabled globally).
    expect(
      within(screen.getByTestId("supplement-thetvdb")).getByTestId("supplement-thetvdb-inherit"),
    ).toHaveTextContent(/Inherit \(Off\)/);
  });

  it("forcing a supplement off PUTs providerOverrides", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(policy());
    updateEnrichmentPolicy.mockResolvedValue(
      policy({
        supplements: [
          { slug: "omdb", name: "OMDb API", override: false, inheritedEnabled: true },
          { slug: "thetvdb", name: "TheTVDB", override: null, inheritedEnabled: false },
        ],
      }),
    );
    render(<EnrichmentPolicyPanel library={lib()} />);

    await screen.findByTestId("supplement-omdb");
    await user.click(screen.getByTestId("supplement-omdb-off"));

    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { providerOverrides: { omdb: false } }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("supplement-omdb-off")).toHaveAttribute("data-active", "true"),
    );
  });

  it("forcing a supplement to Inherit clears its override (null)", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(
      policy({
        supplements: [
          { slug: "omdb", name: "OMDb API", override: false, inheritedEnabled: true },
          { slug: "thetvdb", name: "TheTVDB", override: null, inheritedEnabled: false },
        ],
      }),
    );
    updateEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyPanel library={lib()} />);

    await waitFor(() =>
      expect(screen.getByTestId("supplement-omdb-off")).toHaveAttribute("data-active", "true"),
    );
    await user.click(screen.getByTestId("supplement-omdb-inherit"));
    await waitFor(() =>
      expect(updateEnrichmentPolicy).toHaveBeenCalledWith("lib1", { providerOverrides: { omdb: null } }),
    );
  });

  it("Inherit clears the override (enrichEnabled=null) from an overridden state", async () => {
    const user = userEvent.setup();
    getEnrichmentPolicy.mockResolvedValue(
      policy({ enrichEnabled: false, effective: { video: false, music: false } }),
    );
    updateEnrichmentPolicy.mockResolvedValue(policy());
    render(<EnrichmentPolicyPanel library={lib()} />);

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
