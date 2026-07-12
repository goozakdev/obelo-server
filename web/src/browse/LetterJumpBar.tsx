// The alphabetical jump bar shown beside a library's name: a row of letter-range
// buttons (A-D, E-H, …) that scroll the poster grid to that part of the
// (alphabetically ordered) list. Paired with useLetterJump, which owns the
// scroll + on-demand paging. Rendered by the Movie/TV/Music library grids.

export interface LetterRange {
  label: string;
  /** Lower-case first letter of the range — the jump target passed to onJump. */
  start: string;
}

// Fixed A–Z ranges (roughly even letter spans). Kept static rather than derived
// from the data so the bar is stable regardless of what a library happens to
// hold, and a click on an empty range still lands sensibly (useLetterJump seeks
// the first item at/after the range's start letter).
export const LETTER_RANGES: LetterRange[] = [
  { label: "A-D", start: "a" },
  { label: "E-H", start: "e" },
  { label: "I-L", start: "i" },
  { label: "M-P", start: "m" },
  { label: "Q-U", start: "q" },
  { label: "V-Z", start: "v" },
];

export default function LetterJumpBar({
  onJump,
}: {
  onJump: (startChar: string) => void;
}) {
  return (
    <nav
      className="letter-jump"
      aria-label="Jump to letter range"
      data-testid="letter-jump"
    >
      {LETTER_RANGES.map((r) => (
        <button
          key={r.start}
          type="button"
          className="letter-jump-btn"
          data-testid={`letter-jump-${r.start}`}
          onClick={() => onJump(r.start)}
        >
          {r.label}
        </button>
      ))}
    </nav>
  );
}
