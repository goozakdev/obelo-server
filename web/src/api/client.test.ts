import { describe, it, expect } from "vitest";
import { ApiClient, EVENT_TYPES } from "./client";
import { memoryTokenStore } from "./token";

describe("ApiClient.directFileDownloadUrl", () => {
  it("returns an absolute, token-bearing URL for the direct-file route", () => {
    const client = new ApiClient({ tokenStore: memoryTokenStore("tok-123") });
    const url = client.directFileDownloadUrl("file-1");
    // Absolute (so an external player can resolve it) + self-authenticating.
    expect(url).toBe(
      `${window.location.origin}/api/v1/files/file-1/download?token=tok-123`,
    );
  });

  it("URL-encodes the file id and token", () => {
    const client = new ApiClient({ tokenStore: memoryTokenStore("a b/c") });
    const url = client.directFileDownloadUrl("id/with space");
    expect(url).toContain("/files/id%2Fwith%20space/download");
    expect(url).toContain("token=a%20b%2Fc");
  });

  it("returns null when there is no token (logged out)", () => {
    const client = new ApiClient({ tokenStore: memoryTokenStore(null) });
    expect(client.directFileDownloadUrl("file-1")).toBeNull();
  });
});

describe("ApiClient.startPlayback (conditional body fields)", () => {
  const profile = {
    containers: [],
    videoCodecs: [],
    audioCodecs: [],
    maxAudioChannels: 2,
    textSubtitleFormats: [],
  };
  const constraints = { maxBitrate: 100_000_000 };

  async function captureBody(opts: Parameters<ApiClient["startPlayback"]>[1]) {
    let body: Record<string, unknown> | null = null;
    const fetchImpl = (async (_url: string, init: RequestInit) => {
      body = JSON.parse(init.body as string) as Record<string, unknown>;
      return new Response(JSON.stringify({ sessionId: "s1" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl });
    await client.startPlayback("t1", opts);
    return body!;
  }

  it("sends remuxSelectedOnly: true when set (appletv-web-parity §10)", async () => {
    const body = await captureBody({
      deviceProfile: profile,
      constraints,
      remuxSelectedOnly: true,
    });
    expect(body.remuxSelectedOnly).toBe(true);
  });

  it("omits the field when absent or false — never `false` on the wire", async () => {
    // Absent: an older server rejects the unknown field, so off must be OMISSION.
    let body = await captureBody({ deviceProfile: profile, constraints });
    expect("remuxSelectedOnly" in body).toBe(false);
    // Explicit false: same — the server defaults the absent field to false.
    body = await captureBody({
      deviceProfile: profile,
      constraints,
      remuxSelectedOnly: false,
    });
    expect("remuxSelectedOnly" in body).toBe(false);
  });
});

describe("ApiClient.scanEntity (Targeted scan)", () => {
  it("POSTs to the entity's /scan route and normalizes the scope-tagged status", async () => {
    let captured: { url: string; method?: string } | null = null;
    const fetchImpl = (async (url: string, init: RequestInit) => {
      captured = { url, method: init.method };
      return new Response(
        JSON.stringify({ libraryId: "lib1", state: "running", scope: "The Wire" }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      );
    }) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl });

    const status = await client.scanEntity("shows", "show/1");

    expect(captured).not.toBeNull();
    // Hits POST /{entityType}/{id}/scan with the id URL-encoded.
    expect(captured!.url).toContain("/api/v1/shows/show%2F1/scan");
    expect(captured!.method).toBe("POST");
    // The running, scope-tagged status is normalized (counts filled, scope kept).
    expect(status).toMatchObject({ state: "running", scope: "The Wire", titlesFound: 0 });
  });
});

describe("ApiClient artwork upload (multipart)", () => {
  it("POSTs a FormData image part to the role's upload route, without a JSON content-type", async () => {
    let captured: { url: string; init: RequestInit } | null = null;
    const fetchImpl = (async (url: string, init: RequestInit) => {
      captured = { url, init };
      return new Response(JSON.stringify({ id: "t1", overview: "", artwork: [] }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }) as unknown as typeof fetch;
    const client = new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl });

    const file = new File([new Uint8Array([0xff, 0xd8, 0xff])], "poster.jpg", { type: "image/jpeg" });
    await client.uploadTitleArtwork("title-1", "poster", file);

    expect(captured).not.toBeNull();
    const { url, init } = captured!;
    // Hits the multipart upload route with the role in the query.
    expect(url).toContain("/api/v1/titles/title-1/artworkUpload?role=poster");
    expect(init.method).toBe("POST");
    // The body is passed through as FormData carrying the image part — NOT JSON.
    expect(init.body).toBeInstanceOf(FormData);
    expect((init.body as FormData).get("image")).toBe(file);
    const headers = new Headers(init.headers);
    // The browser sets the multipart Content-Type/boundary; we must not force JSON.
    expect(headers.get("Content-Type")).not.toBe("application/json");
    expect(headers.get("Authorization")).toBe("Bearer tok-1");
  });
});

describe("ApiClient Tailnet remote access (ADR-0043)", () => {
  // One response shape for all five routes, so the only thing worth pinning here
  // is that each verb is the right method on the right path — a screen that
  // POSTed disconnect at the forget route would wipe an identity the operator
  // meant to keep, and nothing in the payload would show it.
  function capturing() {
    const calls: { url: string; method?: string; body?: string }[] = [];
    const fetchImpl = (async (url: string, init: RequestInit) => {
      calls.push({ url, method: init.method, body: init.body as string });
      return new Response(
        JSON.stringify({
          enabled: true,
          hostname: "obelo",
          controlURL: "",
          httpsEnabled: false,
          status: { state: "needsLogin", keyExpiry: null, loginURL: "https://login.test/a/1" },
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }) as unknown as typeof fetch;
    return {
      calls,
      client: new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl }),
    };
  }

  it("GETs the settings and carries the live status through", async () => {
    const { calls, client } = capturing();
    const v = await client.getTailnet();
    expect(calls[0].url).toContain("/api/v1/settings/tailscale");
    expect(calls[0].method).toBe("GET");
    // keyExpiry null is "no expiry", not a missing field — it must survive.
    expect(v.status).toMatchObject({ state: "needsLogin", keyExpiry: null });
    expect(v.status.loginURL).toBe("https://login.test/a/1");
  });

  it("PUTs a partial settings update (omitted = unchanged)", async () => {
    const { calls, client } = capturing();
    await client.updateTailnet({ hostname: "cinema" });
    expect(calls[0].url).toContain("/api/v1/settings/tailscale");
    expect(calls[0].method).toBe("PUT");
    expect(JSON.parse(calls[0].body!)).toEqual({ hostname: "cinema" });
  });

  it("POSTs each verb at its own route", async () => {
    const { calls, client } = capturing();
    await client.connectTailnet();
    await client.disconnectTailnet();
    await client.forgetTailnet();
    expect(calls.map((c) => c.method)).toEqual(["POST", "POST", "POST"]);
    expect(calls[0].url).toContain("/api/v1/settings/tailscale/connect");
    expect(calls[1].url).toContain("/api/v1/settings/tailscale/disconnect");
    expect(calls[2].url).toContain("/api/v1/settings/tailscale/forget");
  });
});

describe("EVENT_TYPES", () => {
  it("registers tailscaleState, or the Tailnet nudge never reaches the browser", () => {
    // EventSource only delivers events whose NAME has a registered listener, so
    // an unregistered type is not a missing feature — it is a silent one: the
    // admin screen would sit on a stale state forever and the login link would
    // only ever appear on a manual reload.
    expect(EVENT_TYPES).toContain("tailscaleState");
  });
});

describe("ApiClient file matcher (ADR-0044)", () => {
  function stub(payload: unknown) {
    const calls: { url: string; init: RequestInit }[] = [];
    const fetchImpl = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify(payload), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }) as unknown as typeof fetch;
    return { calls, client: new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl }) };
  }

  it("omits ?group entirely on the cheap first load", async () => {
    // Opening a ten-season Show must cost ONE round-trip, not ten: the group
    // parameter names the single group whose provider records are fetched.
    const { calls, client } = stub({ containerId: "s1" });
    await client.getShowMatcher("s1");
    expect(calls[0].url).toBe("/api/v1/shows/s1/matcher");
  });

  it("names one group on expand", async () => {
    const { calls, client } = stub({ containerId: "s1" });
    await client.getShowMatcher("s1", 4);
    expect(calls[0].url).toBe("/api/v1/shows/s1/matcher?group=4");
  });

  it("sends the whole arrangement as a PUT body", async () => {
    const { calls, client } = stub({ containerId: "s1", applied: { rearranged: 1 } });
    const doc = await client.applyShowMatcher("s1", {
      files: [{ path: "/tv/a.mkv", state: "placed", placements: [{ group: 4, slot: 1, ordinal: 1 }] }],
    });
    expect(calls[0].init.method).toBe("PUT");
    expect(JSON.parse(calls[0].init.body as string)).toEqual({
      files: [{ path: "/tv/a.mkv", state: "placed", placements: [{ group: 4, slot: 1, ordinal: 1 }] }],
    });
    // The answer is the RE-READ document plus `applied`, normalized.
    expect(doc.applied).toEqual({ rearranged: 1, displaced: [], deferred: [] });
  });

  it("asks another series for its slots by external id", async () => {
    const { calls, client } = stub({ externalId: "tmdb:5", groups: [{ number: 1, slotCount: 5 }] });
    const res = await client.listSeriesSlots("s1", "tmdb:5", 1);
    expect(calls[0].url).toBe("/api/v1/shows/s1/seriesSeasons?externalId=tmdb%3A5&group=1");
    expect(res.groups).toEqual([{ number: 1, slotCount: 5 }]);
    expect(res.slots).toEqual([]);
  });
});

describe("ApiClient Needs-Fixing show rows (file-matcher/07)", () => {
  function stub(payload: unknown) {
    const calls: { url: string; init: RequestInit }[] = [];
    const fetchImpl = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(JSON.stringify(payload), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }) as unknown as typeof fetch;
    return { calls, client: new ApiClient({ tokenStore: memoryTokenStore("tok-1"), fetchImpl }) };
  }

  it("fills a show-problems entry's holes so a row can add its counts", async () => {
    // Absent means ZERO here, not "unknown": a Show with nothing unsettled is simply
    // not in the list, so every field the server omitted is a real zero.
    const { calls, client } = stub({ shows: [{ showId: "sh1", title: "Batman", unassigned: 3 }] });
    const shows = await client.listShowProblems("lib1");
    expect(calls[0].url).toBe("/api/v1/libraries/lib1/show-problems");
    expect(shows).toEqual([
      {
        showId: "sh1",
        title: "Batman",
        year: 0,
        path: "",
        unassigned: 3,
        unidentified: 0,
        unmatchedPaths: [],
        orphaned: 0,
        orphanedPath: "",
        unreadablePaths: [],
      },
    ]);
  });

  it("answers an empty list for a Library with no Shows", async () => {
    const { client } = stub({});
    expect(await client.listShowProblems("lib1")).toEqual([]);
  });

  it("dismisses a Show's flagged Episodes in ONE call", async () => {
    // A collapsed row stands for the whole set it counted; N calls could
    // half-succeed and leave a count the Admin has no way to explain.
    const { calls, client } = stub({});
    await client.reviewShowEpisodes("sh 1");
    expect(calls[0].url).toBe("/api/v1/shows/sh%201/reviewEpisodes");
    expect(calls[0].init.method).toBe("POST");
  });
});
