import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";

// The redirect-flow Sign-in providers card (ADR-0063 decision 2): absent until
// there is one, and a provider whose identity the server cannot verify says so.

const client = vi.hoisted(() => ({
  getSignInProviders: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import RedirectSignInProviders from "./RedirectSignInProviders";

beforeEach(() => {
  client.getSignInProviders.mockReset();
});

// The Users screen fetches the provider list once and hands it down; do the same
// from whatever the test stubbed.
async function renderRedirect() {
  const view = await client.getSignInProviders().catch(() => null);
  return render(<RedirectSignInProviders view={view} />);
}

describe("RedirectSignInProviders", () => {
  it("renders nothing when there is no redirect-flow provider", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [], redirect: [] });
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = await renderRedirect());
    });
    expect(container).toBeEmptyDOMElement();
  });

  it("marks a plain OAuth2 provider's identity as not independently verified", async () => {
    client.getSignInProviders.mockResolvedValue({
      providers: [],
      redirect: [
        { id: "oidc", name: "OpenID Connect", verified: true, configured: true },
        { id: "oauth", name: "Some OAuth", verified: false, configured: true },
      ],
    });
    await renderRedirect();

    expect(await screen.findByTestId("redirect-sign-in-unverified-oauth")).toHaveTextContent(
      "Identity not independently verified",
    );
    expect(screen.queryByTestId("redirect-sign-in-unverified-oidc")).not.toBeInTheDocument();
    expect(screen.getByTestId("redirect-sign-in-verified-oidc")).toBeInTheDocument();
  });

  it("lists a configured provider and leaves an unconfigured one out", async () => {
    client.getSignInProviders.mockResolvedValue({
      providers: [],
      redirect: [
        { id: "oidc", name: "OpenID Connect", verified: true, configured: false },
        { id: "oauth", name: "Some OAuth", verified: false, configured: true },
      ],
    });
    await renderRedirect();

    expect(await screen.findByTestId("redirect-sign-in-oauth")).toBeInTheDocument();
    expect(screen.queryByTestId("redirect-sign-in-oidc")).not.toBeInTheDocument();
  });

  it("renders nothing when every redirect provider is unconfigured", async () => {
    client.getSignInProviders.mockResolvedValue({
      providers: [],
      redirect: [{ id: "oidc", name: "OpenID Connect", verified: true, configured: false }],
    });
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = await renderRedirect());
    });
    expect(container).toBeEmptyDOMElement();
  });
});
