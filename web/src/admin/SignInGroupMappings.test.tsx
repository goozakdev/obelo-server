import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The Group mapping cards (ADR-0063 decision 4): one per Sign-in provider,
// absent until there is one; saves the whole mapping; says when a provider can
// only be synced at sign-in; flags a provider whose re-checks are failing; and
// runs "re-sync now".

const client = vi.hoisted(() => ({
  getSignInProviders: vi.fn(),
  listLibraries: vi.fn(),
  getGroupMapping: vi.fn(),
  setGroupMapping: vi.fn(),
  resyncSignInProvider: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import SignInGroupMappings from "./SignInGroupMappings";

const empty = { rules: [], recheck: false, intervalHours: 24, defaultInterval: true };

beforeEach(() => {
  Object.values(client).forEach((f) => f.mockReset());
  client.listLibraries.mockResolvedValue([{ id: "l1", name: "Movies", kind: "movie", rootFolders: [] }]);
});

describe("SignInGroupMappings", () => {
  it("renders nothing when there is no Sign-in provider", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [], redirect: [] });
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = render(<SignInGroupMappings />));
    });
    expect(container).toBeEmptyDOMElement();
  });

  it("saves a group mapped to a Library, and says a provider without lookup syncs at sign-in", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [{ id: "dir", name: "Directory" }], redirect: [] });
    client.getGroupMapping.mockResolvedValue(empty);
    client.setGroupMapping.mockResolvedValue({
      ...empty,
      rules: [{ group: "family", role: "member", libraryIds: ["l1"] }],
    });
    render(<SignInGroupMappings />);

    expect(await screen.findByTestId("group-mapping-sign-in-only-dir")).toBeInTheDocument();
    await userEvent.click(screen.getByTestId("group-mapping-add-dir"));
    await userEvent.type(screen.getByLabelText("Group"), "family");
    await userEvent.click(screen.getByLabelText("Movies"));
    await userEvent.click(screen.getByTestId("group-mapping-save-dir"));

    expect(client.setGroupMapping).toHaveBeenCalledWith(
      "dir",
      [{ group: "family", role: "member", libraryIds: ["l1"] }],
      0,
    );
    expect(await screen.findByTestId("group-mapping-note-dir")).toHaveTextContent("Saved");
  });

  it("flags a failing provider and runs re-sync now", async () => {
    client.getSignInProviders.mockResolvedValue({
      providers: [],
      redirect: [{ id: "oidc", name: "OpenID Connect", verified: true, configured: true }],
    });
    client.getGroupMapping.mockResolvedValue({
      ...empty,
      recheck: true,
      failing: { since: new Date().toISOString(), reason: "unreachable" },
    });
    client.resyncSignInProvider.mockResolvedValue({ checked: 2, remapped: 0, failed: 1, revoked: 0 });
    render(<SignInGroupMappings />);

    expect(await screen.findByTestId("group-mapping-failing-oidc")).toHaveTextContent("unreachable");
    expect(screen.queryByTestId("group-mapping-sign-in-only-oidc")).not.toBeInTheDocument();
    await userEvent.click(screen.getByTestId("group-mapping-resync-oidc"));
    expect(client.resyncSignInProvider).toHaveBeenCalledWith("oidc");
    expect(await screen.findByTestId("group-mapping-note-oidc")).toHaveTextContent("2 asked");
  });
});
