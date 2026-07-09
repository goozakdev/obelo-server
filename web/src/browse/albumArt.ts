import { API_PREFIX } from "../api/client";

// The same-origin URL for an Album's cover image, served by GET
// /api/v1/albums/{id}/artwork and authenticated by the media cookie (a browser
// <img> cannot set an Authorization header) — mirroring the Title artwork
// strategy in Poster.tsx. Used only when the Album reported hasArtwork.
//
// `version` is the Album's artworkVersion cache-bust token (newest fetched-cover
// timestamp): appended as `?v=`, it makes a re-enriched cover reload in place
// while an unchanged one keeps its URL (and the browser cache). Omit it for a
// local-only cover (no token) — the URL then stays bare and stable.
export function albumArtworkUrl(albumId: string, version?: string): string {
  const base = `${API_PREFIX}/albums/${encodeURIComponent(albumId)}/artwork`;
  return version ? `${base}?v=${encodeURIComponent(version)}` : base;
}
