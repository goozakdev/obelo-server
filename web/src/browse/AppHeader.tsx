import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useAuth } from "../auth/session";
import { useFeature } from "../serverInfoContext";
import { useEnrichmentActivity } from "../events/enrichEvents";
import { useLibraries } from "./librariesContext";
import { MusicIcon, FilmIcon, TvIcon } from "./kindIcons";
import type { Library } from "../api/types";

// The shared authed header: app title (links to the landing) on the left, the
// main nav centered, and the current user menu pinned right. The header is
// locked to the top of the viewport (see `.app-header` sticky) so it stays
// visible while a screen scrolls. Reused across Home and the browse screens so
// the nav + logout behavior lives in one place (issues 02/03).
//
// Nav layout: a Home link, then a link per media kind (Music / Movies / TV)
// derived from the caller's Libraries, each fronted by its icon. A kind with a
// single Library is a direct link; a kind with several becomes a dropdown so the
// user picks which one. A kind with no Libraries shows nothing. The user's
// utility links (Playlists, Collections, Admin, Sign out) live in a dropdown
// under the username on the right.

// Inline Lucide icons (kept local so the header has no icon-lib dependency).
// All share currentColor so they inherit the surrounding link color.
function HomeIcon() {
  return (
    <svg
      className="nav-icon"
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
      <path d="M15 21v-8a1 1 0 0 0-1-1h-4a1 1 0 0 0-1 1v8" />
      <path d="M3 10a2 2 0 0 1 .709-1.528l7-6a2 2 0 0 1 2.582 0l7 6A2 2 0 0 1 21 10v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
    </svg>
  );
}

// The media-kind glyphs (MusicIcon / FilmIcon / TvIcon) now live in ./kindIcons
// so the admin Libraries hub reuses the exact same icons; HomeIcon stays local
// because it's nav-only.

// The media kinds, in nav order. `kind` matches the backend Library.kind
// vocabulary (library.service.go: movie | tv | music).
const LIBRARY_KINDS: ReadonlyArray<{
  kind: string;
  label: string;
  testid: string;
  Icon: (props: { className?: string }) => JSX.Element;
}> = [
  { kind: "music", label: "Music", testid: "nav-music", Icon: MusicIcon },
  { kind: "movie", label: "Movies", testid: "nav-movies", Icon: FilmIcon },
  { kind: "tv", label: "TV", testid: "nav-tv", Icon: TvIcon },
];

// A music Library opens the separate music experience (/music/...); Movie/TV
// Libraries use the shared poster grid. Mirrors LibraryListScreen's routing.
function libraryPath(lib: Library): string {
  return lib.kind === "music"
    ? `/music/libraries/${lib.id}`
    : `/libraries/${lib.id}`;
}

export default function AppHeader() {
  const { session, isAdmin } = useAuth();
  // Realtime: show an unobtrusive indicator while an Enrichment pass is running
  // anywhere on the server (ADR-0016 SSE; external-metadata-enrichment issue 02).
  const enriching = useEnrichmentActivity();
  const libraries = useLibraries();

  return (
    <header className="app-header">
      <Link className="app-title app-title-link" to="/">
        Juice Box
      </Link>
      <nav className="app-nav" aria-label="Main">
        <Link className="nav-link nav-icon-link" to="/" data-testid="nav-home">
          <HomeIcon />
          <span>Home</span>
        </Link>
        {libraries.status === "ready" ? (
          <MediaNav libraries={libraries.data} />
        ) : libraries.status === "error" ? (
          // Fall back to the generic list link if the Library fetch failed, so
          // navigation is never lost.
          <Link className="nav-link" to="/libraries" data-testid="nav-libraries">
            Libraries
          </Link>
        ) : null}
      </nav>
      <div className="app-user">
        {enriching && (
          <span
            className="enriching-indicator"
            data-testid="enriching-indicator"
            role="status"
          >
            <span className="enriching-dot" aria-hidden="true" />
            Updating metadata&hellip;
          </span>
        )}
        <UserMenu
          username={session?.user.username ?? ""}
          isAdmin={isAdmin}
        />
      </div>
    </header>
  );
}

// UserMenu is the far-right account dropdown: the username toggles a menu of the
// utility links (Playlists, Collections, admin-only Admin) plus Sign out.
// Closes on outside click, on Escape, and on selection.
//
// Playlists and Collections are gated on the server's advertised feature flags
// (Apple TV → Web parity §4): a server that does not advertise `playlists` /
// `collections` has no such routes, so the link is hidden rather than offering a
// dead end. We gate on the flag, never on the server version.
function UserMenu({
  username,
  isAdmin,
}: {
  username: string;
  isAdmin: boolean;
}) {
  const navigate = useNavigate();
  const { logout } = useAuth();
  const showPlaylists = useFeature("playlists");
  const showCollections = useFeature("collections");
  const [open, setOpen] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocPointer(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  async function onLogout() {
    setLoggingOut(true);
    await logout();
    navigate("/login", { replace: true });
  }

  return (
    <div className="nav-dropdown user-menu" ref={ref}>
      <button
        type="button"
        className="nav-link nav-dropdown-toggle user-menu-toggle"
        data-testid="user-menu-toggle"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <span className="nav-user" data-testid="current-user">
          {username}
        </span>
        <span className="nav-dropdown-caret" aria-hidden="true">
          ▾
        </span>
      </button>
      {open && (
        <ul
          className="nav-dropdown-menu nav-dropdown-menu-end"
          role="menu"
          data-testid="user-menu"
        >
          {showPlaylists && (
            <li role="none">
              <Link
                role="menuitem"
                className="nav-dropdown-item"
                to="/playlists"
                data-testid="nav-playlists"
                onClick={() => setOpen(false)}
              >
                Playlists
              </Link>
            </li>
          )}
          {showCollections && (
            <li role="none">
              <Link
                role="menuitem"
                className="nav-dropdown-item"
                to="/collections"
                data-testid="nav-collections"
                onClick={() => setOpen(false)}
              >
                Collections
              </Link>
            </li>
          )}
          {isAdmin && (
            <li role="none">
              <Link
                role="menuitem"
                className="nav-dropdown-item"
                to="/admin"
                data-testid="admin-link"
                onClick={() => setOpen(false)}
              >
                Admin
              </Link>
            </li>
          )}
          <li role="none">
            <button
              role="menuitem"
              className="nav-dropdown-item nav-dropdown-item-button"
              data-testid="logout-button"
              type="button"
              onClick={onLogout}
              disabled={loggingOut}
            >
              {loggingOut ? "Signing out…" : "Sign out"}
            </button>
          </li>
        </ul>
      )}
    </div>
  );
}

// MediaNav renders one entry per media kind that has at least one Library: a
// direct link when there's exactly one, a dropdown when there are several.
function MediaNav({ libraries }: { libraries: Library[] }) {
  return (
    <>
      {LIBRARY_KINDS.map(({ kind, label, testid, Icon }) => {
        const libs = libraries.filter((lib) => lib.kind === kind);
        if (libs.length === 0) return null;
        if (libs.length === 1) {
          return (
            <Link
              key={kind}
              className="nav-link nav-icon-link"
              to={libraryPath(libs[0])}
              data-testid={testid}
            >
              <Icon />
              <span>{label}</span>
            </Link>
          );
        }
        return (
          <LibraryDropdown
            key={kind}
            label={label}
            testid={testid}
            Icon={Icon}
            libraries={libs}
          />
        );
      })}
    </>
  );
}

// LibraryDropdown is the multi-Library affordance for a kind: a toggle that
// opens a menu of that kind's Libraries by name. Closes on outside click, on
// Escape, and on selection.
function LibraryDropdown({
  label,
  testid,
  Icon,
  libraries,
}: {
  label: string;
  testid: string;
  Icon: (props: { className?: string }) => JSX.Element;
  libraries: Library[];
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    function onDocPointer(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    document.addEventListener("mousedown", onDocPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDocPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="nav-dropdown" ref={ref}>
      <button
        type="button"
        className="nav-link nav-dropdown-toggle nav-icon-link"
        data-testid={testid}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <Icon />
        <span>{label}</span>
        <span className="nav-dropdown-caret" aria-hidden="true">
          ▾
        </span>
      </button>
      {open && (
        <ul
          className="nav-dropdown-menu"
          role="menu"
          data-testid={`${testid}-menu`}
        >
          {libraries.map((lib) => (
            <li key={lib.id} role="none">
              <Link
                role="menuitem"
                className="nav-dropdown-item"
                to={libraryPath(lib)}
                data-testid="nav-library-option"
                data-library-id={lib.id}
                onClick={() => setOpen(false)}
              >
                {lib.name}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
