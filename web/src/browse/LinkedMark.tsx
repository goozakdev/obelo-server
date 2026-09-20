import type { LinkedMarks } from "../api/types";
import { LinkIcon } from "./ActionIcons";

// The ONE mark a mirrored row wears, anywhere in the app (issue 15).
//
// A Linked Library is a mirror of another household's shelf (ADR-0056 §1), and
// every browse row that can come out of one carries `linked`/`available` on the
// wire (docs/api-contract.md §3.4/§3.5). Issue 10 read the pair on the Libraries
// list and the Libraries admin row only, so a mirrored film in Continue Watching,
// a poster in a linked Library's grid, an Album under a linked Artist or a
// mirrored Playlist member all looked local until Play was refused. This is the
// badge and the greying those two screens already had, extracted so every surface
// says the same thing in the same words and the two can never drift.
//
// Two rules, and only two:
//
//   • BADGE — a linked row wears a chain LinkIcon and, when the wire named it
//     (issue 18: `linkedServer` now rides the row itself, not the Admin-only
//     /links join), the sharing Server's name — so a Member reads "🔗 Kate's
//     Obelo", not a bare "Linked". A row with no name yet (a document that
//     carries the pair but not the name) falls back to the icon plus the word
//     "Linked". Nothing else changes about the row: it keeps its
//     poster, its position, its link and its actions, because a mirrored Title
//     is a Title.
//   • GREY — `available: false` means the household that provides it cannot be
//     reached RIGHT NOW (ADR-0056 §6). The row is greyed (`is-unavailable`) and
//     STAYS WHERE IT IS, still clickable: the catalog is here and correct, only
//     the bytes are momentarily out of reach, and a row that vanishes when a
//     friend reboots teaches people their films are gone. The honest sentence
//     belongs at play time, and the player already says it (issue 10).
//
// `available` is only meaningful while `linked` is true — a local row carries
// neither field — so every helper here reads `linked` first.

/** True when this row lives in a mirror of another household's Library. */
export function isLinked(entity: LinkedMarks | null | undefined): boolean {
  return entity?.linked === true;
}

/** True when this row is mirrored AND its providing Server is not answering.
 * A local row is never "unavailable": absence of the pair means local. */
export function isUnavailable(entity: LinkedMarks | null | undefined): boolean {
  return isLinked(entity) && entity?.available === false;
}

/** The one greying rule, as a className. Append it to a row's own class so the
 * caller keeps its layout classes and gains nothing but the dimming:
 *
 *     className={linkedRowClass("poster-tile", title)}
 */
export function linkedRowClass(
  base: string,
  entity: LinkedMarks | null | undefined,
): string {
  return isUnavailable(entity) ? `${base} is-unavailable` : base;
}

export interface LinkedMarkProps {
  /** The row (or Library) whose pair decides the mark. */
  entity: LinkedMarks | null | undefined;
  /** Override the badge's testid. The browse Libraries list, which had this badge
   * before it was shared, keeps the id its spec already selects on
   * (`library-linked-badge`); everything else uses the default, so a new surface
   * is selected the same way everywhere. */
  testId?: string;
  /** The name of the Server providing it, when a screen has it cheaply in hand
   * (see `useLibraryProvider`). Renders "Provided by <name>" beside the badge on
   * the DETAIL screens, which reach a mirror through a document that carries no
   * `linkedServer` (`titleDetailJSON`, issue 14 deviation 2) and read the name
   * off the Admin-only /links join instead. On a ROW the name rides the badge
   * itself, so those callers pass nothing here (issue 18). */
  providedBy?: string;
}

/** The badge itself — nothing at all for a local row, so a caller can render it
 * unconditionally beside a title. */
export default function LinkedMark({
  entity,
  testId = "linked-badge",
  providedBy,
}: LinkedMarkProps) {
  if (!isLinked(entity)) return null;
  const away = isUnavailable(entity);
  // The name the wire put on the row itself (issue 18); a bare "Linked" only
  // when it is absent — a document that carries the pair but not the name (the
  // detail headers, which use `providedBy` for it instead).
  const name = entity?.linkedServer;
  return (
    <>
      <span
        className="linked-badge"
        data-testid={testId}
        data-available={away ? "false" : "true"}
        // The hover text is the sentence LibraryListScreen already says; no new
        // copy, and the greying carries the meaning on its own for a viewer who
        // never hovers.
        title={
          name
            ? away
              ? `${name} — unavailable right now`
              : `Shared from ${name}`
            : away
              ? "unavailable right now"
              : "shared with you"
        }
      >
        <LinkIcon className="linked-badge-icon" />
        {/* The name can be long and a poster card is narrow, so the text — not
            the icon — truncates with an ellipsis rather than overflowing the
            card (the full name stays in the badge's title for hover, and screen
            readers read it whole). */}
        <span className="linked-badge-name">{name || "Linked"}</span>
      </span>
      {providedBy && (
        <span className="linked-provided" data-testid={`${testId}-provided`}>
          Provided by {providedBy}
        </span>
      )}
    </>
  );
}
