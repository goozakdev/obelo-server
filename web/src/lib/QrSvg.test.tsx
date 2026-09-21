import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import QrSvg from "./QrSvg";
import { encodeQr } from "./qr";

describe("QrSvg", () => {
  it("draws the encoder's matrix inside a four-module quiet zone", () => {
    render(<QrSvg text="HELLO" label="QR code of HELLO" testId="qr" />);
    const svg = screen.getByTestId("qr");
    const { size, modules } = encodeQr("HELLO");

    // Without the quiet zone a camera cannot find the symbol's edges, so the
    // viewBox is deliberately larger than the matrix.
    expect(svg).toHaveAttribute("viewBox", `0 0 ${size + 8} ${size + 8}`);

    // One <path> for the whole matrix, not one <rect> per module: a version-14
    // symbol is over five thousand modules.
    const paths = svg.querySelectorAll("path");
    expect(paths).toHaveLength(1);
    const dark = modules.flat().filter(Boolean).length;
    expect((paths[0].getAttribute("d")?.split("M").length ?? 0) - 1).toBe(dark);
  });

  it("names itself for a reader that cannot see it", () => {
    render(<QrSvg text="HELLO" label="QR code of the invite string" testId="qr" />);
    expect(screen.getByRole("img", { name: "QR code of the invite string" })).toBe(
      screen.getByTestId("qr"),
    );
  });

  it("says so rather than blanking when the text cannot fit any symbol", () => {
    // The QR is the convenience; the string beside it is the mechanism
    // (ADR-0055, Consequences), so the fallback points at the string.
    render(<QrSvg text={"x".repeat(5000)} label="QR code" testId="qr" />);
    expect(screen.queryByTestId("qr")).not.toBeInTheDocument();
    expect(screen.getByTestId("qr-error")).toHaveTextContent(
      /too long to show as a QR code\. Copy the text instead\./i,
    );
  });
});
