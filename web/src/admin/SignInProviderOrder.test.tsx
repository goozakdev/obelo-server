import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The Sign-in order card (ADR-0063 decision 6): absent until there is something
// to order, and a move saves the whole new order and shows what the server
// answered rather than what the click assumed.

const client = vi.hoisted(() => ({
  getSignInProviders: vi.fn(),
  setSignInProviderOrder: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import SignInProviderOrder from "./SignInProviderOrder";

const a = { id: "dir-a", name: "Directory A" };
const b = { id: "dir-b", name: "Directory B" };

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
});

// The Users screen fetches the provider list once and hands it down; do the same
// from whatever the test stubbed.
async function renderOrder() {
  const view = await client.getSignInProviders().catch(() => null);
  return render(<SignInProviderOrder view={view} />);
}

describe("SignInProviderOrder", () => {
  it("renders nothing when no password-flow Sign-in provider is installed", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [] });
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = await renderOrder());
    });
    expect(client.getSignInProviders).toHaveBeenCalled();
    expect(container).toBeEmptyDOMElement();
  });

  it("moves a provider up and saves the whole order", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [a, b] });
    client.setSignInProviderOrder.mockResolvedValue({ providers: [b, a] });
    await renderOrder();

    await userEvent.click(await screen.findByTestId("sign-in-order-up-dir-b"));

    expect(client.setSignInProviderOrder).toHaveBeenCalledWith(["dir-b", "dir-a"]);
    const items = await screen.findAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Directory B");
    expect(items[1]).toHaveTextContent("Directory A");
    expect(screen.getByTestId("sign-in-order-up-dir-b")).toBeDisabled();
  });

  it("shows the server's refusal", async () => {
    client.getSignInProviders.mockResolvedValue({ providers: [a, b] });
    client.setSignInProviderOrder.mockRejectedValue(new Error("not a password sign-in provider"));
    await renderOrder();

    await userEvent.click(await screen.findByTestId("sign-in-order-down-dir-a"));

    expect(await screen.findByTestId("sign-in-order-error")).toHaveTextContent(
      "not a password sign-in provider",
    );
  });
});
