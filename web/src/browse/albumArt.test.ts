import { describe, it, expect } from "vitest";
import { albumArtworkUrl } from "./albumArt";

// The Album cover URL is built client-side (unlike Show/Artist URLs, which the
// server returns whole), so the cache-bust token is appended here. A re-enriched
// cover changes artworkVersion → the URL changes → the browser reloads it; a
// local-only cover (no token) keeps a bare, stable URL.
describe("albumArtworkUrl", () => {
  it("returns a bare URL when no version is given (local-only cover)", () => {
    expect(albumArtworkUrl("al1")).toBe("/api/v1/albums/al1/artwork");
  });

  it("appends an encoded ?v= cache-bust token when a version is given", () => {
    const v = "2025-06-01 00:00:00";
    expect(albumArtworkUrl("al1", v)).toBe(
      `/api/v1/albums/al1/artwork?v=${encodeURIComponent(v)}`,
    );
  });

  it("encodes the album id", () => {
    expect(albumArtworkUrl("a/b")).toBe("/api/v1/albums/a%2Fb/artwork");
  });
});
