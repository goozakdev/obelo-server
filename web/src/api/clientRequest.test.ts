import { describe, it, expect, vi, afterEach } from "vitest";
import { ApiClient } from "./client";
import { ApiError } from "./errors";
import { memoryTokenStore } from "./token";

describe("ApiClient request error handling", () => {
  it("does not fire the 401 handler for a token the session has since replaced", async () => {
    const tokenStore = memoryTokenStore("tok-b");
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const fetchImpl = (async () => {
      await gate;
      return new Response(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "no" } }), {
        status: 401,
      });
    }) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore, fetchImpl });
    const onUnauthorized = vi.fn();
    client.setUnauthorizedHandler(onUnauthorized);

    const pending = client.verifySession();
    tokenStore.set("tok-c"); // the user switched while B's request was in flight
    release();
    await expect(pending).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("still fires the 401 handler when the rejected token is the current one", async () => {
    const fetchImpl = (async () =>
      new Response(JSON.stringify({ error: { code: "UNAUTHORIZED", message: "no" } }), {
        status: 401,
      })) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore: memoryTokenStore("tok-b"), fetchImpl });
    const onUnauthorized = vi.fn();
    client.setUnauthorizedHandler(onUnauthorized);
    await expect(client.verifySession()).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("turns a 2xx with a non-JSON body into a BAD_RESPONSE ApiError", async () => {
    const fetchImpl = (async () =>
      new Response("<!doctype html><html></html>", { status: 200 })) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore: memoryTokenStore(null), fetchImpl });
    await expect(client.getServerInfo()).rejects.toMatchObject({
      name: "ApiError",
      code: "BAD_RESPONSE",
      status: 200,
    });
  });
});

describe("ApiClient.subscribeEvents reconnect", () => {
  class FakeEventSource {
    static instances: FakeEventSource[] = [];
    readyState = 0;
    onopen: (() => void) | null = null;
    onerror: (() => void) | null = null;
    closed = false;
    constructor(public url: string) {
      FakeEventSource.instances.push(this);
    }
    addEventListener() {}
    close() {
      this.closed = true;
      this.readyState = 2;
    }
  }

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    FakeEventSource.instances = [];
  });

  it("reopens a stream the browser failed for good, with backoff, until unsubscribed", () => {
    vi.useFakeTimers();
    vi.stubGlobal("EventSource", FakeEventSource);
    const client = new ApiClient({ tokenStore: memoryTokenStore("t") });
    const off = client.subscribeEvents(() => {});
    const all = FakeEventSource.instances;
    expect(all).toHaveLength(1);

    // A transient drop (CONNECTING) is the browser's own retry: leave it alone.
    all[0].readyState = 0;
    all[0].onerror?.();
    vi.advanceTimersByTime(60_000);
    expect(all).toHaveLength(1);

    // A permanent failure (CLOSED) is reopened after the backoff.
    all[0].readyState = 2;
    all[0].onerror?.();
    expect(all).toHaveLength(1);
    vi.advanceTimersByTime(1000);
    expect(all).toHaveLength(2);

    // Unsubscribing cancels a pending reopen and closes the live stream.
    all[1].readyState = 2;
    all[1].onerror?.();
    off();
    vi.advanceTimersByTime(60_000);
    expect(all).toHaveLength(2);
    expect(all[1].closed).toBe(true);
  });
});

describe("ApiClient.reportProgress keepalive", () => {
  it("forwards keepalive to fetch for a report that must outlive the page", async () => {
    const fetchImpl = vi.fn(
      async (..._args: unknown[]) =>
        new Response(JSON.stringify({ watched: false }), { status: 200 }),
    );
    const client = new ApiClient({
      tokenStore: memoryTokenStore("tok"),
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });
    await client.reportProgress("s1", { positionMs: 5, state: "paused" }, undefined, {
      keepalive: true,
    });
    expect(fetchImpl.mock.calls[0][1]).toMatchObject({ keepalive: true });
    await client.reportProgress("s1", { positionMs: 6, state: "playing" });
    expect(fetchImpl.mock.calls[1][1]).not.toHaveProperty("keepalive", true);
  });
});
