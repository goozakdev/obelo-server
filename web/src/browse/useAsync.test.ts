import { describe, it, expect } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useAsync } from "./useAsync";
import { ApiError, NetworkError } from "../api/client";

// useAsync is the shared load-once hook behind the browse screens. Its error
// handling is tested here at the hook level (deterministic, no screen/act
// timing), so the screen tests don't need to re-assert the rejected path.

describe("useAsync", () => {
  it("resolves to ready with the data", async () => {
    const { result } = renderHook(() => useAsync(async () => 42, []));
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(result.current).toMatchObject({ status: "ready", data: 42 });
  });

  it("surfaces an ApiError's message", async () => {
    const { result } = renderHook(() =>
      useAsync(() => Promise.reject(new ApiError(403, "FORBIDDEN", "admin only")), []),
    );
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current).toMatchObject({ status: "error", message: "admin only" });
  });

  it("surfaces a friendly message when the server is unreachable", async () => {
    const { result } = renderHook(() =>
      useAsync(() => Promise.reject(new NetworkError("boom")), []),
    );
    await waitFor(() => expect(result.current.status).toBe("error"));
    expect(result.current.status === "error" && result.current.message).toMatch(/reach the server/i);
  });
});
