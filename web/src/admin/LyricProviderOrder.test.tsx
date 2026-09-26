import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// The Lyric provider order card: absent until there are two providers to order,
// and a move saves the whole new order and shows what the server answered rather
// than what the click assumed.

const client = vi.hoisted(() => ({
  getLyricProviders: vi.fn(),
  setLyricProviderOrder: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import LyricProviderOrder from "./LyricProviderOrder";

const a = { slug: "lrc-a", name: "Lyrics A", description: "", docsURL: "" };
const b = { slug: "lrc-b", name: "Lyrics B", description: "", docsURL: "" };

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
});

describe("LyricProviderOrder", () => {
  it("renders nothing with fewer than two Lyric providers", async () => {
    client.getLyricProviders.mockResolvedValue({ providers: [a] });
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = render(<LyricProviderOrder />));
    });
    expect(client.getLyricProviders).toHaveBeenCalled();
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when the server has no lyric provider settings", async () => {
    client.getLyricProviders.mockRejectedValue(new Error("not available"));
    let container!: HTMLElement;
    await act(async () => {
      ({ container } = render(<LyricProviderOrder />));
    });
    expect(container).toBeEmptyDOMElement();
  });

  it("moves a provider up and saves the whole order", async () => {
    client.getLyricProviders.mockResolvedValue({ providers: [a, b] });
    client.setLyricProviderOrder.mockResolvedValue({ providers: [b, a] });
    render(<LyricProviderOrder />);

    await userEvent.click(await screen.findByTestId("lyric-order-up-lrc-b"));

    expect(client.setLyricProviderOrder).toHaveBeenCalledWith(["lrc-b", "lrc-a"]);
    const items = await screen.findAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Lyrics B");
    expect(items[1]).toHaveTextContent("Lyrics A");
    expect(screen.getByTestId("lyric-order-up-lrc-b")).toBeDisabled();
    expect(screen.getByTestId("lyric-order-down-lrc-a")).toBeDisabled();
  });

  it("shows the server's refusal and keeps the order it had", async () => {
    client.getLyricProviders.mockResolvedValue({ providers: [a, b] });
    client.setLyricProviderOrder.mockRejectedValue(new Error("unknown lyric provider: lrc-a"));
    render(<LyricProviderOrder />);

    await userEvent.click(await screen.findByTestId("lyric-order-down-lrc-a"));

    expect(await screen.findByTestId("lyric-order-error")).toHaveTextContent("unknown lyric provider: lrc-a");
    const items = screen.getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Lyrics A");
  });
});
