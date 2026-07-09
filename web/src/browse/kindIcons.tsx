// The media-kind icons (Music / Movies / TV), extracted from AppHeader so the
// admin Libraries hub can front each Library — and the Add-Library type picker —
// with the very same glyphs the nav uses (issue: admin-libraries-ui). Inline
// Lucide paths, no icon-lib dependency; all stroke `currentColor` so they inherit
// the surrounding text color. `className` defaults to `nav-icon` (the header's
// sizing) but callers can pass their own to resize/recolor.

type KindIconProps = { className?: string };

export function MusicIcon({ className = "nav-icon" }: KindIconProps) {
  return (
    <svg
      className={className}
      xmlns="http://www.w3.org/2000/svg"
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M9 18V5l12-2v13" />
      <circle cx="6" cy="18" r="3" />
      <circle cx="18" cy="16" r="3" />
    </svg>
  );
}

export function FilmIcon({ className = "nav-icon" }: KindIconProps) {
  return (
    <svg
      className={className}
      xmlns="http://www.w3.org/2000/svg"
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect width="18" height="18" x="3" y="3" rx="2" />
      <path d="M7 3v18" />
      <path d="M3 7.5h4" />
      <path d="M3 12h18" />
      <path d="M3 16.5h4" />
      <path d="M17 3v18" />
      <path d="M17 7.5h4" />
      <path d="M17 16.5h4" />
    </svg>
  );
}

export function TvIcon({ className = "nav-icon" }: KindIconProps) {
  return (
    <svg
      className={className}
      xmlns="http://www.w3.org/2000/svg"
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="m17 2-5 5-5-5" />
      <rect width="20" height="15" x="2" y="7" rx="2" />
    </svg>
  );
}

// The media kinds, in nav order. `kind` matches the backend Library.kind
// vocabulary (library.service.go: movie | tv | music); `label` is the display
// name shared by the nav and the admin Add-Library type picker.
export const LIBRARY_KIND_META: ReadonlyArray<{
  kind: string;
  label: string;
  Icon: (props: KindIconProps) => JSX.Element;
}> = [
  { kind: "music", label: "Music", Icon: MusicIcon },
  { kind: "movie", label: "Movies", Icon: FilmIcon },
  { kind: "tv", label: "TV", Icon: TvIcon },
];

/** The icon component for a Library kind, falling back to the Film glyph for an
 * unknown kind (never renders nothing). */
export function LibraryKindIcon({
  kind,
  className,
}: {
  kind: string;
  className?: string;
}) {
  const Icon = LIBRARY_KIND_META.find((m) => m.kind === kind)?.Icon ?? FilmIcon;
  return <Icon className={className} />;
}

/** The display label for a Library kind (e.g. "movie" → "Movies"); echoes the
 * raw kind if it's outside the known vocabulary. */
export function libraryKindLabel(kind: string): string {
  return LIBRARY_KIND_META.find((m) => m.kind === kind)?.label ?? kind;
}
