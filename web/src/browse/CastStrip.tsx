import { useEffect, useState } from "react";
import type { Credit } from "../api/types";
import { personPhotoUrl } from "./Poster";

// The Movie (and, later, Show) detail cast section: a horizontally-scrolling row
// of fixed-width cast cards in billing order (cast-photos/01). Each card shows the
// actor's headshot on top, their name in bold beneath, and the character below.
// A member with no photo/ref falls back to an initials placeholder (never a broken
// image), reusing the same graceful onError idiom the Poster component uses on a
// 404. An empty cast renders NOTHING (no empty shell) — the caller can render this
// unconditionally.
//
// The headshot loads from personPhotoUrl (same-origin, media-cookie authenticated),
// so no JS auth plumbing is needed to show faces. Only `cast` kinds are shown; crew
// (unused in this slice) is filtered out.

export interface CastStripProps {
  cast: Credit[];
}

export default function CastStrip({ cast }: CastStripProps) {
  const members = cast.filter((c) => (c.kind ?? "cast") === "cast");
  if (members.length === 0) {
    return null;
  }
  return (
    <div className="detail-cast" data-testid="detail-cast">
      <h2 className="section-title">Cast</h2>
      <ul className="cast-strip" data-testid="cast-strip">
        {members.map((c, i) => (
          <li key={`${c.person}-${i}`} className="cast-member" data-testid="cast-member">
            <CastAvatar person={c.person} personId={c.personId} version={c.photoVersion} />
            <span className="cast-person" data-testid="cast-person">
              {c.person}
            </span>
            {c.character && (
              <span className="cast-character" data-testid="cast-character">
                {c.character}
              </span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

// CastAvatar renders a cast member's headshot with a clean initials fallback,
// mirroring Poster: the same-origin <img> loads via the media cookie, and on a
// load error (the endpoint 404s when the person has no photo — or there's no ref
// to build a URL from at all) it swaps to a placeholder showing the actor's
// initials, so the strip never shows a broken image.
function CastAvatar({
  person,
  personId,
  version,
}: {
  person: string;
  personId?: string;
  version?: string;
}) {
  const [failed, setFailed] = useState(false);
  // Reset the error state when the target changes (a previously-404'd headshot may
  // now exist after a re-enrich bumps the version).
  useEffect(() => {
    setFailed(false);
  }, [personId, version]);

  if (!personId || failed) {
    return (
      <div
        className="cast-photo cast-photo-placeholder"
        data-testid="cast-photo-placeholder"
        role="img"
        aria-label={`${person} (no photo)`}
      >
        <span className="cast-initials" aria-hidden="true">
          {initials(person)}
        </span>
      </div>
    );
  }
  return (
    <img
      className="cast-photo cast-photo-img"
      data-testid="cast-photo"
      src={personPhotoUrl(personId, "profile", version)}
      alt={person}
      loading="lazy"
      onError={() => setFailed(true)}
    />
  );
}

// initials derives a 1-2 letter placeholder label from a person's name, matching
// Poster's placeholder idiom.
function initials(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean);
  if (words.length === 0) return "?";
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (words[0][0] + words[words.length - 1][0]).toUpperCase();
}
