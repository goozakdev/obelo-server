import { describe, it, expect } from "vitest";
import { safeReturnPath } from "./returnPath";

// Where a sign-in returns to is a path on this server, or Home.

describe("safeReturnPath", () => {
  it("keeps a path on this server", () => {
    expect(safeReturnPath("/")).toBe("/");
    expect(safeReturnPath("/link/ABCD")).toBe("/link/ABCD");
  });

  it("sends anything else Home", () => {
    for (const path of ["//evil.example", "//evil.example/x", "/\\evil.example", "https://evil.example", "", "link", undefined, 7]) {
      expect(safeReturnPath(path)).toBe("/");
    }
  });

  it("sends Home a path that resolves to another server once tab, CR and LF are dropped", () => {
    for (const path of ["/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\t\\evil.example", "/\r\n/evil.example/x"]) {
      expect(safeReturnPath(path)).toBe("/");
    }
  });
});
