import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import Poster from "./Poster";

// R05-11 / R06-03: a caller that knows there is no artwork renders the
// placeholder directly instead of firing a request that is bound to 404.
describe("Poster with src={null}", () => {
  it("renders the placeholder without ever mounting an <img>", () => {
    render(<Poster titleId="ar2" title="Various Artists" src={null} />);
    expect(screen.queryByTestId("poster-img")).toBeNull();
    expect(screen.getByTestId("poster-placeholder")).toHaveTextContent("VA");
  });

  it("an undefined src still falls back to the title-keyed artwork", () => {
    render(<Poster titleId="t1" title="Dune" />);
    expect(screen.getByTestId("poster-img")).toBeInTheDocument();
  });
});
