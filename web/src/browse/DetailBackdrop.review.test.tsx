import { describe, it, expect } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import DetailBackdrop from "./DetailBackdrop";

// R05-12: a new src after a failed one gets its own try.
describe("DetailBackdrop", () => {
  it("retries with a new src after the previous one failed", () => {
    const { rerender } = render(<DetailBackdrop src="/a.jpg" />);
    fireEvent.error(document.querySelector("img")!);
    expect(screen.queryByTestId("detail-backdrop")).toBeNull();

    rerender(<DetailBackdrop src="/b.jpg" />);
    expect(screen.getByTestId("detail-backdrop")).toBeInTheDocument();
    expect(document.querySelector("img")).toHaveAttribute("src", "/b.jpg");
  });
});
