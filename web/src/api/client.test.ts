import { describe, it, expect } from "vitest";
import { ApiClient } from "./client";
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
