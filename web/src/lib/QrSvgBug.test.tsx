import { describe, it, expect, vi } from "vitest";
import { render } from "@testing-library/react";

vi.mock("./qr", async () => {
  const actual = await vi.importActual<typeof import("./qr")>("./qr");
  return {
    ...actual,
    encodeQr: () => {
      throw new TypeError("encoder bug");
    },
  };
});

import QrSvg from "./QrSvg";

describe("QrSvg encoder failures", () => {
  it("only reports 'too long' for a too-long text; any other failure surfaces", () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<QrSvg text="hello" label="QR code" testId="qr" />)).toThrow(
      /encoder bug/,
    );
  });
});
