// Inline SVG icons for the Title detail action toolbar (watchlist 01). Each renders
// at 1em and inherits the button's text color via fill="currentColor", so a single
// CSS rule sizes/themes them and an active (e.g. watched) state is just a color
// change. The paths are the provided 256×256 artwork, wrapped in the original
// translate/scale group that maps the 0–90 design space into the viewBox.

type IconProps = { className?: string };

function Svg({ children, className }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      className={className}
      viewBox="0 0 256 256"
      width="1em"
      height="1em"
      role="presentation"
      aria-hidden="true"
      focusable="false"
    >
      <g transform="translate(1.4065934065934016 1.4065934065934016) scale(2.81 2.81)">
        {children}
      </g>
    </svg>
  );
}

/** Plus-in-a-circle — "Add to watchlist". */
export function WatchlistIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path
        fill="currentColor"
        fillRule="nonzero"
        d="M 45 0 C 20.187 0 0 20.187 0 45 c 0 24.813 20.187 45 45 45 c 24.813 0 45 -20.187 45 -45 C 90 20.187 69.813 0 45 0 z M 65.454 50 H 50 v 15.454 c 0 2.762 -2.238 5 -5 5 c -2.761 0 -5 -2.238 -5 -5 V 50 H 24.545 c -2.761 0 -5 -2.238 -5 -5 c 0 -2.761 2.239 -5 5 -5 H 40 V 24.545 c 0 -2.761 2.239 -5 5 -5 c 2.762 0 5 2.239 5 5 V 40 h 15.454 c 2.762 0 5 2.239 5 5 C 70.454 47.762 68.216 50 65.454 50 z"
      />
    </Svg>
  );
}

/** Check-in-a-circle — "Mark as watched". */
export function WatchedIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path
        fill="currentColor"
        fillRule="nonzero"
        d="M 45 0 C 20.147 0 0 20.147 0 45 c 0 24.853 20.147 45 45 45 s 45 -20.147 45 -45 C 90 20.147 69.853 0 45 0 z M 68.371 32.98 l -26.521 30 c -0.854 0.967 -2.083 1.52 -3.372 1.52 c -0.01 0 -0.02 0 -0.029 0 c -1.3 -0.009 -2.533 -0.579 -3.381 -1.563 L 21.59 47.284 c -1.622 -1.883 -1.41 -4.725 0.474 -6.347 c 1.884 -1.621 4.725 -1.409 6.347 0.474 l 10.112 11.744 L 61.629 27.02 c 1.645 -1.862 4.489 -2.037 6.352 -0.391 C 69.843 28.275 70.018 31.119 68.371 32.98 z"
      />
    </Svg>
  );
}

/** Pencil-on-a-page — "Edit". */
export function EditIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <path
        fill="currentColor"
        fillRule="nonzero"
        d="M 88.436 5.704 l -4.14 -4.14 c -2.085 -2.085 -5.466 -2.085 -7.551 0 l -6.181 6.181 L 27.239 51.07 c -0.66 0.66 -1.166 1.459 -1.479 2.338 l -5.245 14.699 c -0.306 0.857 0.521 1.684 1.378 1.378 l 14.699 -5.245 c 0.879 -0.314 1.678 -0.819 2.338 -1.479 l 43.325 -43.325 l 6.181 -6.181 C 90.521 11.17 90.521 7.789 88.436 5.704 z"
      />
      <path
        fill="currentColor"
        fillRule="nonzero"
        d="M 75.857 90 H 9.53 C 4.275 90 0 85.726 0 80.471 V 14.142 c 0 -5.255 4.275 -9.53 9.53 -9.53 h 34.803 c 2.209 0 4 1.791 4 4 s -1.791 4 -4 4 H 9.53 c -0.844 0 -1.53 0.686 -1.53 1.53 v 66.329 C 8 81.313 8.686 82 9.53 82 h 66.328 c 0.844 0 1.53 -0.687 1.53 -1.529 V 45.667 c 0 -2.209 1.791 -4 4 -4 s 4 1.791 4 4 v 34.804 C 85.388 85.726 81.112 90 75.857 90 z"
      />
    </Svg>
  );
}

/** Trash can — "Delete". Same artwork as the queue's per-entry remove glyph. */
export function TrashIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <g fill="currentColor">
        <path d="M 67.692 90 H 22.308 c -3.042 0 -5.518 -2.476 -5.518 -5.518 v -61 c 0 -1.104 0.896 -2 2 -2 h 52.42 c 1.104 0 2 0.896 2 2 v 61 C 73.21 87.524 70.734 90 67.692 90 z M 20.79 25.482 v 59 c 0 0.837 0.681 1.518 1.518 1.518 h 45.385 c 0.837 0 1.518 -0.681 1.518 -1.518 v -59 H 20.79 z" />
        <path d="M 73.196 25.482 H 16.804 c -3.042 0 -5.518 -2.475 -5.518 -5.518 v -4.335 c 0 -3.042 2.475 -5.518 5.518 -5.518 h 56.393 c 3.042 0 5.518 2.475 5.518 5.518 v 4.335 C 78.714 23.007 76.238 25.482 73.196 25.482 z M 16.804 14.112 c -0.837 0 -1.518 0.681 -1.518 1.518 v 4.335 c 0 0.837 0.681 1.518 1.518 1.518 h 56.393 c 0.837 0 1.518 -0.681 1.518 -1.518 v -4.335 c 0 -0.837 -0.681 -1.518 -1.518 -1.518 H 16.804 z" />
        <path d="M 57.197 14.112 H 32.803 c -1.104 0 -2 -0.896 -2 -2 V 5.518 C 30.803 2.476 33.278 0 36.321 0 h 17.358 c 3.043 0 5.519 2.476 5.519 5.518 v 6.594 C 59.197 13.216 58.302 14.112 57.197 14.112 z M 34.803 10.112 h 20.395 V 5.518 C 55.197 4.681 54.516 4 53.679 4 H 36.321 c -0.837 0 -1.518 0.681 -1.518 1.518 V 10.112 z" />
        <path d="M 45 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 47 77.729 46.104 78.624 45 78.624 z" />
        <path d="M 58.222 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 60.222 77.729 59.326 78.624 58.222 78.624 z" />
        <path d="M 31.779 78.624 c -1.104 0 -2 -0.896 -2 -2 V 34.856 c 0 -1.104 0.896 -2 2 -2 s 2 0.896 2 2 v 41.768 C 33.779 77.729 32.883 78.624 31.779 78.624 z" />
      </g>
    </Svg>
  );
}

// The two admin-row action glyphs below are drawn on a plain 24×24 grid as
// strokes, not on the 90-unit filled artwork grid the icons above share — a
// hairline wrench and tick stay legible at the 1em size a row button renders at,
// where filled silhouettes turn to mud.
function Svg24({ children, className }: IconProps & { children: React.ReactNode }) {
  return (
    <svg
      className={className}
      viewBox="0 0 24 24"
      width="1em"
      height="1em"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      role="presentation"
      aria-hidden="true"
      focusable="false"
    >
      {children}
    </svg>
  );
}

/** Wrench — "Fix" (an identity or a metadata match). */
export function WrenchIcon({ className }: IconProps) {
  return (
    <Svg24 className={className}>
      {/* Head: a ring left open across the top-right. The mouth is deliberately
          wide (~100°) — narrower gaps read as a magnifying glass once the icon
          is down at row size. */}
      <path d="M14.9 2.6 A5.2 5.2 0 1 0 21.4 9" />
      {/* Handle, running from the head down to the bottom-left corner. */}
      <path d="M12.8 11.2 L4.6 19.4" />
    </Svg24>
  );
}

/** Tick — "Mark reviewed". */
export function CheckIcon({ className }: IconProps) {
  return (
    <Svg24 className={className}>
      <path d="M4.5 12.8 L9.5 17.8 L19.5 6.5" />
    </Svg24>
  );
}

/** Three vertical dots — the overflow menu. */
export function MoreIcon({ className }: IconProps) {
  return (
    <Svg className={className}>
      <circle cx="45" cy="18" r="9" fill="currentColor" />
      <circle cx="45" cy="45" r="9" fill="currentColor" />
      <circle cx="45" cy="72" r="9" fill="currentColor" />
    </Svg>
  );
}
