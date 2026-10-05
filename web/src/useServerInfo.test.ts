import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { NetworkError } from "./api/errors";
import type { ApiClient } from "./api/client";
import type { ServerInfo } from "./api/types";
import { useServerInfoHandshake } from "./useServerInfo";

const info = { id: "srv", setupRequired: false, features: {} } as unknown as ServerInfo;

function clientWith(getServerInfo: ReturnType<typeof vi.fn>): ApiClient {
  return { getServerInfo } as unknown as ApiClient;
}

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe("useServerInfoHandshake", () => {
  it("retries with backoff after an unreachable server until it answers", async () => {
    const getServerInfo = vi
      .fn()
      .mockRejectedValueOnce(new NetworkError("down"))
      .mockRejectedValueOnce(new NetworkError("down"))
      .mockResolvedValue(info);
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));

    await act(async () => {});
    expect(result.current.state.status).toBe("unreachable");
    expect(getServerInfo).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(getServerInfo).toHaveBeenCalledTimes(2);
    expect(result.current.state.status).toBe("unreachable");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(getServerInfo).toHaveBeenCalledTimes(3);
    expect(result.current.state.status).toBe("ready");

    // Once ready it stops polling.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(getServerInfo).toHaveBeenCalledTimes(3);
  });

  it("re-tries at once when the browser comes back online", async () => {
    const getServerInfo = vi
      .fn()
      .mockRejectedValueOnce(new NetworkError("down"))
      .mockResolvedValue(info);
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));
    await act(async () => {});
    expect(result.current.state.status).toBe("unreachable");

    await act(async () => {
      window.dispatchEvent(new Event("online"));
    });
    expect(result.current.state.status).toBe("ready");
  });

  it("refresh() re-reads the handshake and keeps the last good one if the re-read fails", async () => {
    const getServerInfo = vi
      .fn()
      .mockResolvedValueOnce({ ...info, setupRequired: true })
      .mockResolvedValueOnce(info)
      .mockRejectedValueOnce(new NetworkError("blip"));
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));
    await act(async () => {});
    expect(result.current.state).toMatchObject({ status: "ready", info: { setupRequired: true } });

    act(() => result.current.refresh());
    await act(async () => {});
    expect(result.current.state).toMatchObject({ status: "ready", info: { setupRequired: false } });

    act(() => result.current.refresh());
    await act(async () => {});
    expect(result.current.state.status).toBe("ready");
  });
});

describe("useServerInfoHandshake re-read retry", () => {
  it("retries a failed refresh() with backoff after the first success, bounded", async () => {
    const getServerInfo = vi
      .fn()
      .mockResolvedValueOnce({ ...info, setupRequired: true })
      .mockRejectedValueOnce(new NetworkError("blip"))
      .mockResolvedValueOnce(info);
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));
    await act(async () => {});
    act(() => result.current.refresh());
    await act(async () => {});
    expect(getServerInfo).toHaveBeenCalledTimes(2);
    // Still showing the last good handshake while the retry is pending.
    expect(result.current.state).toMatchObject({ status: "ready", info: { setupRequired: true } });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(getServerInfo).toHaveBeenCalledTimes(3);
    expect(result.current.state).toMatchObject({ status: "ready", info: { setupRequired: false } });
  });

  it("gives up after a bounded number of failed re-reads", async () => {
    const getServerInfo = vi
      .fn()
      .mockResolvedValueOnce(info)
      .mockRejectedValue(new NetworkError("down"));
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));
    await act(async () => {});
    act(() => result.current.refresh());
    // Step through the backoff (1s, 2s, 4s, 8s, 16s) one timer at a time.
    for (let i = 0; i < 12; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
    }
    // 1 first load + 1 refresh + 5 bounded retries.
    expect(getServerInfo).toHaveBeenCalledTimes(7);
    expect(result.current.state.status).toBe("ready");
  });

  it("refresh() starts a fresh retry budget after the bounded retries are spent", async () => {
    const getServerInfo = vi
      .fn()
      .mockResolvedValueOnce(info)
      .mockRejectedValue(new NetworkError("down"));
    const client = clientWith(getServerInfo);
    const { result } = renderHook(() => useServerInfoHandshake(client));
    await act(async () => {});
    act(() => result.current.refresh());
    for (let i = 0; i < 12; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
    }
    expect(getServerInfo).toHaveBeenCalledTimes(7); // budget spent

    act(() => result.current.refresh());
    for (let i = 0; i < 12; i++) {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
    }
    // The second refresh + 5 new bounded retries, not a single attempt.
    expect(getServerInfo).toHaveBeenCalledTimes(13);
  });
});
