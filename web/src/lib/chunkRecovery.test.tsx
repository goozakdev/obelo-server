import { describe, it, expect, vi, afterEach } from "vitest";
import { lazy, Suspense } from "react";
import { render, screen, fireEvent } from "@testing-library/react";
import { ChunkErrorBoundary } from "./chunkRecovery";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ChunkErrorBoundary", () => {
  it("shows an inline reload message for a failed lazy import and keeps siblings mounted", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const Broken = lazy(() =>
      Promise.reject(new TypeError("Failed to fetch dynamically imported module: /assets/x.js")),
    );
    const reload = vi.fn();
    render(
      <div>
        <p>sibling</p>
        <ChunkErrorBoundary reload={reload}>
          <Suspense fallback={<p>loading</p>}>
            <Broken />
          </Suspense>
        </ChunkErrorBoundary>
      </div>,
    );
    expect(await screen.findByText(/failed to load/i)).toBeTruthy();
    expect(screen.getByText("sibling")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /reload/i }));
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("logs a non-chunk render error rather than swallowing it", async () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    function Boom(): never {
      throw new Error("kaboom");
    }
    render(
      <ChunkErrorBoundary reload={() => {}}>
        <Boom />
      </ChunkErrorBoundary>,
    );
    expect(await screen.findByRole("button", { name: /reload/i })).toBeTruthy();
    expect(
      err.mock.calls.some((c) => c[0] === "render failed" && c[1] instanceof Error && c[1].message === "kaboom"),
    ).toBe(true);
  });

  it("treats Vite's CSS preload failure as a chunk failure", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const Broken = lazy(() => Promise.reject(new Error("Unable to preload CSS for /assets/x.css")));
    const reload = vi.fn();
    render(
      <ChunkErrorBoundary reload={reload}>
        <Suspense fallback={null}>
          <Broken />
        </Suspense>
      </ChunkErrorBoundary>,
    );
    expect(await screen.findByText(/this page failed to load/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /reload/i })).toBeTruthy();
    expect(reload).not.toHaveBeenCalled();
  });

  it("recovers when resetKey changes (navigating away from the failed route)", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const Broken = lazy(() => Promise.reject(new TypeError("Failed to fetch dynamically imported module")));
    const { rerender } = render(
      <ChunkErrorBoundary resetKey="/a" reload={() => {}}>
        <Suspense fallback={null}>
          <Broken />
        </Suspense>
      </ChunkErrorBoundary>,
    );
    await screen.findByText(/failed to load/i);
    rerender(
      <ChunkErrorBoundary resetKey="/b" reload={() => {}}>
        <p>fine</p>
      </ChunkErrorBoundary>,
    );
    expect(screen.getByText("fine")).toBeTruthy();
  });
});
