// Client-side URL/ID detection for the unified Edit-item "Search" tab (ADR-0019).
// The Search input accepts a free-text search term, a provider URL, or a bare id.
// looksLikeRef decides whether the input should route to the externalPreview
// endpoint (a by-id/URL lookup that resolves a single candidate) instead of a
// free-text provider search — so a pasted TMDB/MusicBrainz id or URL auto-selects
// its resolved record rather than searching for it.
//
// Provider-aware because a bare id is provider-specific: TMDB ids are numeric
// (video kinds — movie/show/season/episode), MusicBrainz ids are UUIDs (music
// kinds — artist/album/track). A UUID is never a TMDB ref and a bare number is
// never a MusicBrainz ref; a provider URL for either is always a ref (the server
// validates the kind and reports an actionable error on a mismatch).

export type Provider = "tmdb" | "musicbrainz";

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function looksLikeRef(input: string, provider: Provider): boolean {
  const s = input.trim();
  if (s === "") return false;
  // Any provider URL (or a generic http(s) URL) is a ref — let the server resolve
  // and validate it, surfacing its actionable error on a wrong-kind URL.
  if (/themoviedb\.org|musicbrainz\.org/i.test(s)) return true;
  if (/^https?:\/\//i.test(s)) return true;
  // A bare id is provider-specific.
  if (provider === "tmdb") return /^\d+$/.test(s);
  return UUID_RE.test(s);
}
