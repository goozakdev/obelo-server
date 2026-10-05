import { describe, it, expect } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useAsync, type AsyncState } from "./useAsync";

// R05-04: on a deps change the very first render with the new deps must report
// loading, not the previous key's ready data.
describe("useAsync stale-key render (review)", () => {
  it("reports loading on the first render after deps change", async () => {
    const seen: Array<{ key: string; state: AsyncState<string> }> = [];
    const { rerender } = renderHook(
      ({ key }) => {
        const state = useAsync(async () => key, [key]);
        seen.push({ key, state });
        return state;
      },
      { initialProps: { key: "one" } },
    );
    await waitFor(() => expect(seen[seen.length - 1].state.status).toBe("ready"));

    seen.length = 0;
    rerender({ key: "two" });
    await waitFor(() => expect(seen[seen.length - 1].state.status).toBe("ready"));

    const first = seen[0];
    expect(first.key).toBe("two");
    expect(first.state.status).toBe("loading");
  });
});
