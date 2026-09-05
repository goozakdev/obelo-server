import type { LinkedMarks } from "../api/types";

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
//   • BADGE — a linked row wears "Linked". Nothing else changes about it: it
//     keeps its poster, its position, its link and its actions, because a
//     mirrored Title is a Title.
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
  /** Override the badge's testid. The two surfaces that had this badge before it
   * was shared keep the ids their specs already select on
   * (`library-linked-badge` / `admin-library-linked-badge`); everything else uses
   * the default, so a new surface is selected the same way everywhere. */
  testId?: string;
  /** The name of the Server providing it, when a screen has it cheaply in hand
   * (see `useLibraryProvider`). Renders "Provided by <name>" beside the badge on
   * the detail screens; omitted, the badge stands on its own. */
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
  return (
    <>
      <span
        className="linked-badge"
        data-testid={testId}
        data-available={away ? "false" : "true"}
        // The hover text is the sentence LibraryListScreen already says; no new
        // copy, and the greying carries the meaning on its own for a viewer who
        // never hovers.
        title={away ? "unavailable right now" : "shared with you"}
      >
        Linked
      </span>
      {providedBy && (
        <span className="linked-provided" data-testid={`${testId}-provided`}>
          Provided by {providedBy}
        </span>
      )}
    </>
  );
}
