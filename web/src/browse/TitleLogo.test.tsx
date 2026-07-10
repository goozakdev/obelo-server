import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import TitleLogo from "./TitleLogo";

// The detail hero's identity block (logo-hero): the logo artwork stands in for
// the title text; a title without one — or whose logo fails to load — falls
// back to the large-text heading so the page always names itself.

describe("TitleLogo", () => {
  it("renders the logo <img> (named via alt) and no text heading when src is set", () => {
    render(<TitleLogo title="Dune" src="/api/v1/titles/t1/artwork/logo" />);
    const img = screen.getByTestId("detail-logo");
    expect(img).toHaveAttribute("src", "/api/v1/titles/t1/artwork/logo");
    expect(img).toHaveAttribute("alt", "Dune");
    expect(screen.queryByTestId("detail-title")).toBeNull();
  });

  it("falls back to the text heading when there is no logo", () => {
    render(<TitleLogo title="Dune" />);
    expect(screen.getByTestId("detail-title")).toHaveTextContent("Dune");
    expect(screen.queryByTestId("detail-logo")).toBeNull();
  });

  it("falls back to the text heading when the logo fails to load (404)", () => {
    render(<TitleLogo title="Dune" src="/api/v1/titles/t1/artwork/logo" />);
    fireEvent.error(screen.getByTestId("detail-logo"));
    expect(screen.queryByTestId("detail-logo")).toBeNull();
    expect(screen.getByTestId("detail-title")).toHaveTextContent("Dune");
  });

  it("uses the screen's heading testid for the fallback", () => {
    render(<TitleLogo title="The Bear" testId="show-title" />);
    expect(screen.getByTestId("show-title")).toHaveTextContent("The Bear");
  });
});
