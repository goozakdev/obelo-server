import { useEffect, useState } from "react";

// A Title poster image with a clean placeholder fallback (issue 03 poster
// strategy). The titles-list JSON carries NO artwork flag, so whether a poster
// exists is only known once the browser tries to load it: we render the
// same-origin artwork <img> (authenticated by the media cookie from issue 02 —
// no JS header) and, on a load error (the endpoint 404s when the Title has no
// poster), swap to a placeholder showing the title's initials.
//
// The poster role is "poster" (scanner: poster.jpg/cover.jpg). The src is built
// from the title id so the component needs no detail fetch to show a grid.

import { API_PREFIX } from "../api/client";

export interface PosterProps {
  titleId: string;
  title: string;
  /** Poster role to request; defaults to "poster". */
  role?: string;
  /** Cache-bust token: when it changes, the browser re-fetches the artwork and a
   * previously-404'd poster is retried. Used when Enrichment lands a fetched
   * image while the grid is open. It is per-Title (e.g. the Title's
   * enrichmentStatus), so only a Title whose artwork actually changed reloads —
   * an unchanged poster keeps its src and never flickers. */
  version?: string | number;
  /** An explicit artwork URL to use instead of the title-keyed one — for a browse
   * PARENT (Show/Season/Artist) whose fetched image lives at its own endpoint
   * (issue 03). When set, titleId is used only for the placeholder fallback. */
  src?: string;
}

export function posterUrl(titleId: string, role = "poster", version?: string | number): string {
  const base = `${API_PREFIX}/titles/${encodeURIComponent(titleId)}/artwork/${encodeURIComponent(role)}`;
  return version ? `${base}?v=${encodeURIComponent(version)}` : base;
}

/** Where to fetch a cast member's headshot bytes for a role (profile) — same-origin
 * and authenticated by the media cookie, so it drops straight into an `<img src>`
 * with no JS auth header, exactly like posterUrl (cast-photos/01). The personRef is
 * provider-namespaced ("tmdb:<id>") and url-encoded. version is the optional photo
 * cache-bust token (the person artwork's added_at): when it changes the browser
 * re-fetches and a previously-404'd headshot is retried. */
export function personPhotoUrl(personRef: string, role = "profile", version?: string | number): string {
  const base = `${API_PREFIX}/people/${encodeURIComponent(personRef)}/artwork/${encodeURIComponent(role)}`;
  return version ? `${base}?v=${encodeURIComponent(version)}` : base;
}

export default function Poster({ titleId, title, role = "poster", version, src }: PosterProps) {
  // Reset the error state when the target changes (grid reuse / detail nav) or
  // when version bumps (a poster that 404'd may now exist after enrichment).
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setFailed(false);
  }, [titleId, role, version, src]);

  if (failed) {
    return (
      <div
        className="poster poster-placeholder"
        data-testid="poster-placeholder"
        role="img"
        aria-label={`${title} (no artwork)`}
      >
        <span className="poster-initials" aria-hidden="true">
          {initials(title)}
        </span>
      </div>
    );
  }

  return (
    <img
      className="poster poster-img"
      data-testid="poster-img"
      src={src ?? posterUrl(titleId, role, version)}
      alt={`${title} poster`}
      loading="lazy"
      onError={() => setFailed(true)}
    />
  );
}

function initials(title: string): string {
  const words = title.trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return "?";
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[words.length - 1][0]).toUpperCase();
}
