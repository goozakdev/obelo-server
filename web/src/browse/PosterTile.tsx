import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import type { TitleSummary } from "../api/types";
import Poster from "./Poster";
import { episodeContextLabel, trackContextLabel } from "./episodeLabel";
import { formatTimecode } from "../time";

// The shared poster card: a poster frame (real artwork via <Poster>, with a
// placeholder fallback), a watched badge OR a resume marker, and a caption with
// the title + year. Extracted from the library grid (issue 03) so a card looks
// and behaves identically everywhere it appears — the library grid AND the Home
// rows (issue 05). The whole card links to the Title's detail page.
//
// For an Episode in a Home row (Continue Watching / Up Next / Recently Added)
// the server attaches its Show/Season/episode parent context (issue tv-music/02);
// the caption then leads with a "The Bear · S01E03" context line so a bare
// episode title ("System") is recognizable. A Movie card is unchanged (no
// context line).
//
// The testids here (poster-tile / poster-title / poster-img / poster-placeholder
// via <Poster> / badge-watched / badge-resume) are depended on by the issue-03
// and issue-04 specs and MUST stay intact wherever this card is used.

export interface PosterTileProps {
  title: TitleSummary;
  /** An optional overlay action rendered inside the card but OUTSIDE the Link
   * (so its clicks don't navigate) — used by an Admin curating a Collection to
   * drop a per-member "Remove" control onto the otherwise read-only browse card,
   * keeping decoration parity (collections-playlists-ui issue 02). Absent
   * everywhere else, so the card is byte-for-byte the browse card. */
  action?: ReactNode;
}

export default function PosterTile({ title, action }: PosterTileProps) {
  // Per-Title poster cache-bust: key the artwork URL on this Title's
  // artworkVersion — an opaque token the server changes ONLY when the served
  // poster bytes could have (a re-fetched image, a rescanned local file), not on
  // a text-only edit. So when a live refresh lands fresh artwork, only THIS
  // tile's <img src> changes and reloads; every unchanged poster keeps its src
  // and never flickers — including across a re-enrich (realtime-events web slice).
  return (
    <li className="poster-tile" data-testid="poster-tile" data-title-id={title.id}>
      {/* A Track card opens the music track detail (/music/tracks/{id}); every
          other kind opens the shared Title detail. This single switch reroutes
          track cards wherever PosterTile appears — Home rows, Collections, and
          Playlists. */}
      <Link
        className="poster-link"
        to={title.kind === "track" ? `/music/tracks/${title.id}` : `/titles/${title.id}`}
      >
        <div className="poster-frame">
          <Poster titleId={title.id} title={title.title} version={title.artworkVersion} />
          {title.watched && (
            <span className="badge badge-watched" data-testid="badge-watched">
              Watched
            </span>
          )}
          {!title.watched && title.resumePositionMs > 0 && (
            <span className="badge badge-resume" data-testid="badge-resume">
              Resume {formatTimecode(title.resumePositionMs)}
            </span>
          )}
        </div>
        <div className="poster-caption">
          {title.episode && (
            <span
              className="poster-context"
              data-testid="poster-context"
              title={episodeContextLabel(title.episode)}
            >
              {episodeContextLabel(title.episode)}
            </span>
          )}
          {!title.episode && title.track && (
            <span
              className="poster-context"
              data-testid="poster-context"
              title={trackContextLabel(title.track)}
            >
              {trackContextLabel(title.track)}
            </span>
          )}
          <span className="poster-title" data-testid="poster-title" title={title.title}>
            {title.title}
          </span>
          {!title.episode && !title.track && title.year > 0 && (
            <span className="poster-year">{title.year}</span>
          )}
        </div>
      </Link>
      {action && <div className="poster-action">{action}</div>}
    </li>
  );
}
