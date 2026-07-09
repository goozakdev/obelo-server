import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import Poster, { posterUrl } from "./Poster";

describe("Poster", () => {
  it("renders a same-origin artwork <img> (cookie-auth, no header) by default", () => {
    render(<Poster titleId="t1" title="Dune" />);
    const img = screen.getByTestId("poster-img") as HTMLImageElement;
    // The src is the artwork endpoint under /api/v1 — authenticated by the media
    // cookie, so the <img> carries no Authorization header.
    expect(img.getAttribute("src")).toBe(posterUrl("t1"));
    expect(posterUrl("t1")).toBe("/api/v1/titles/t1/artwork/poster");
  });

  it("falls back to a clean placeholder when the artwork load errors (404)", () => {
    render(<Poster titleId="t1" title="Blade Runner" />);
    const img = screen.getByTestId("poster-img");
    // Simulate the endpoint 404ing (the Title has no poster) → onError.
    fireEvent.error(img);
    expect(screen.queryByTestId("poster-img")).toBeNull();
    const placeholder = screen.getByTestId("poster-placeholder");
    expect(placeholder).toBeInTheDocument();
    // Initials are derived from the title for a recognizable placeholder.
    expect(placeholder).toHaveTextContent("BR");
  });
});
