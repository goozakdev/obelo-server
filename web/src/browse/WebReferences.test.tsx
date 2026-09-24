import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

const { getWebReferences } = vi.hoisted(() => ({ getWebReferences: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: { getWebReferences: (...a: unknown[]) => getWebReferences(...a) },
  };
});

import WebReferences from "./WebReferences";

describe("WebReferences", () => {
  beforeEach(() => {
    getWebReferences.mockReset();
  });

  it("renders one link per reference, opening in a new tab", async () => {
    getWebReferences.mockResolvedValue([
      { label: "IMDb", url: "https://www.imdb.com/title/tt1160419/" },
      { label: "TMDB", url: "https://www.themoviedb.org/movie/438631" },
    ]);
    render(<WebReferences titleId="t1" />);

    const links = await screen.findAllByTestId("web-reference");
    expect(links).toHaveLength(2);
    expect(links[0]).toHaveTextContent("IMDb");
    expect(links[0]).toHaveAttribute("href", "https://www.imdb.com/title/tt1160419/");
    expect(links[0]).toHaveAttribute("target", "_blank");
    expect(links[0]).toHaveAttribute("rel", "noopener noreferrer");
    expect(getWebReferences).toHaveBeenCalledWith("t1", expect.anything());
  });

  it("renders nothing for an empty list", async () => {
    getWebReferences.mockResolvedValue([]);
    const { container } = render(<WebReferences titleId="t1" />);
    await waitFor(() => expect(getWebReferences).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when the request fails", async () => {
    getWebReferences.mockRejectedValue(new Error("boom"));
    const { container } = render(<WebReferences titleId="t1" />);
    await waitFor(() => expect(getWebReferences).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});
