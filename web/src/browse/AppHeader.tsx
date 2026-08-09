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

// The Obelo wordmark, inlined for the same reason the nav glyphs are: the header
// carries no icon/asset dependency, and an inline <svg> can inherit the link
// color. The letterforms are currentColor so `.app-title-link` drives them
// (brand lime at rest, brighter on hover); only the play triangle is fixed.
// viewBox is the artwork's own 83.77 x 27.04 (3.10:1) — height is set in CSS and
// the width follows, so the header never has to hard-code a pixel width.
function ObeloWordmark() {
  return (
    <svg
      className="app-wordmark"
      xmlns="http://www.w3.org/2000/svg"
      viewBox="0 0 83.774384 27.037058"
      fill="currentColor"
      aria-hidden="true"
      focusable="false"
    >
      <path d="m 44.932519,18.764021 c 0,0.626565 0.41804,1.547812 0.762,2.024062 1.15888,1.603375 3.12473,2.259542 5.05619,2.047875 0.29369,-0.0344 0.63235,-0.03704 0.91016,-0.14552 0.8599,-0.333375 2.46592,-1.307042 3.22263,-1.30175 1.05833,0.0053 2.05581,0.870479 2.02142,1.965854 -0.0344,1.132416 -1.10596,1.860021 -1.98438,2.370666 -1.86002,1.087438 -4.6646,1.582209 -6.80244,1.164167 -2.21985,-0.431271 -4.21481,-1.36525 -5.7097,-3.119437 -0.83873,-0.981605 -1.3388,-2.119313 -1.70657,-3.339042 -1.16416,-3.852333 -0.24077,-8.284104 2.72786,-11.0754584 3.71739,-3.500438 9.73666,-2.733146 12.69206,1.3229164 1.09273,1.500188 1.76742,3.151188 1.91294,5.013855 0.0582,0.743479 0.11641,1.709208 -0.43657,2.299229 -0.83608,0.883708 -2.43152,0.642937 -3.54012,0.637645 -2.0664,-0.0079 -4.13544,0.02117 -6.20448,0.0079 -0.84667,-0.0026 -2.18017,-0.169333 -2.921,0.127 z m 8.39788,-4.005792 c 0.0688,-0.0026 0.003,-0.248708 -0.005,-0.269875 -0.18785,-0.568854 -0.36512,-1.095375 -0.71437,-1.592791 -0.73025,-1.039813 -2.11931,-1.658938 -3.37079,-1.672167 -1.47373,-0.01852 -2.68552,0.635 -3.60627,1.74625 -0.37836,0.457729 -0.55034,1.034521 -0.73819,1.579563 -0.008,0.02646 -0.082,0.261937 0,0.264583 -0.0609,0.0026 0.0926,0.08731 0.14817,0.105833 0.14552,0.05292 0.30956,0.02381 0.46037,0.02381 0.40217,-0.0053 0.80433,0.0053 1.2065,-0.0026 1.48431,-0.02117 2.96863,0.0053 4.45294,-0.0053 0.49477,-0.0026 0.98954,0.0079 1.48431,0 0.15081,-0.0026 0.31485,0.01852 0.46038,-0.02381 0.0582,-0.01587 0.1217,-0.04233 0.16668,-0.08202 0.0212,-0.02117 0.0847,-0.07144 0.0556,-0.07144 z" />
      <path d="m 61.939009,26.985777 c -1.09008,0.248708 -2.55323,-0.42598 -2.74108,-1.616605 -0.23284,-1.497541 -0.0741,-3.119437 -0.0741,-4.630208 0,-2.905125 0.003,-13.6589614 0.003,-16.5640864 0,-1.778 -0.30427,-3.63272906 1.88648,-4.11691606 1.14035,-0.254 2.49767,0.484187 2.65377,1.70127006 0.18785,1.449917 0.0476,2.987146 0.0476,4.447646 -0.003,2.934229 0.003,13.7198154 0.008,16.6540454 0.003,1.661583 0.31221,3.643312 -1.78329,4.124854 z" />
      <path d="m 25.446989,25.044757 c 0,0.05821 -0.20902,0.375708 -0.25664,0.478895 -0.16404,0.367771 -0.33867,0.738188 -0.67734,0.976313 -0.8202,0.587375 -2.71991,0.568854 -3.31523,-0.373063 -0.48418,-0.767291 -0.40481,-1.658937 -0.40481,-2.521479 0,-1.51077 -0.0132,-3.024187 -0.0185,-4.537604 -0.0132,-3.979333 -0.0106,-7.963958 0.003,-11.9459374 0.005,-1.217083 -0.16934,-4.561417 0.17727,-5.543021 0.71966,-2.02670806 3.34698,-2.11137506 4.27037,-0.201083 0.34925,0.722312 0.16934,1.788583 0.17198,2.569104 0.008,1.635125 -0.0185,3.272896 -0.005,4.908021 0,0.145521 0.0873,0.325437 0.09,0.439208 0,-0.03175 0.0582,0.01852 0.0873,0.01587 0.0556,-0.0079 0.11906,-0.05821 0.15875,-0.0926 0.41275,-0.341313 0.74083,-0.714375 1.20386,-0.997479 1.83885,-1.124479 4.20687,-1.30175 6.25739,-0.693209 4.16454,1.23825 6.05102,5.7123544 6.02456,9.7498964 -0.0238,3.484563 -1.34673,6.57225 -4.25185,8.575146 -0.55298,0.381 -1.25148,0.759354 -1.91294,0.907521 -2.05581,0.460375 -4.35504,0.433916 -6.17537,-0.738188 -0.39688,-0.256646 -0.74613,-0.534458 -1.10861,-0.830792 -0.0608,-0.05292 -0.14287,-0.142875 -0.22754,-0.156104 -0.0291,-0.0053 -0.09,0.03969 -0.09,0.01058 z m 5.17261,-2.235729 c 5.92402,-0.859896 5.34458,-12.271375 -1.48167,-11.321521 -0.69321,0.0979 -1.35996,0.489479 -1.89177,0.923396 -3.70681,3.021541 -1.78065,11.14425 3.37344,10.398125 z" />
      <path d="m 74.963039,27.010476 c -1.32556,0.09525 -2.76225,-0.0635 -4.01637,-0.508 -8.37671,-2.9845 -8.18092,-16.258646 0.22754,-18.8224584 0.71702,-0.219604 1.40758,-0.425979 2.16165,-0.481542 1.5531,-0.113771 3.19352,0.0053 4.64079,0.627063 8.91116,3.8232294 7.27339,18.4282284 -3.01361,19.1849374 z m -0.39952,-4.238625 c 6.47436,-0.518583 5.90815,-11.869208 -0.81756,-11.358563 -6.20448,0.470959 -5.59329,11.871855 0.81756,11.358563 z" />
      <g transform="matrix(0.22459887,0,0,0.22459887,-1.3475932,-7.1581184)">
        <circle cx="50.25" cy="108" r="44.25" />
        {/* The play triangle is a light fill, NOT currentColor: it is a shape
            drawn on top of the disc, so tying it to the link color would make
            it vanish the moment the mark and the triangle matched. Same as the
            tvOS/iOS app icons, which draw it #ffffff over the lime. */}
        <polygon points="37.81,85.9 37.81,130.1 72.35,108" fill="#fff" />
      </g>
    </svg>
  );
}

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
      <Link className="app-title app-title-link" to="/" aria-label="Obelo">
        <ObeloWordmark />
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
// utility links (Playlists, Collections, admin-only Admin), a Switch user section
// (the remembered-Users roster, appletv-parity/10), plus Sign out. Closes on
// outside click, on Escape, and on selection.
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
  const { logout, roster, switchTo, clearActiveSession } = useAuth();
  const showPlaylists = useFeature("playlists");
  const showCollections = useFeature("collections");
  const [open, setOpen] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  // Switch to a remembered User. A Signed-in entry (retained durable token)
  // switches instantly and auth-free, then lands on Home fresh under the new
  // identity; a Known entry steps aside locally and routes to /login with the
  // username pre-filled (the current user's retained token survives the step-aside,
  // so they remain an instant-switch target).
  async function onSwitch(userId: string, signedIn: boolean, name: string) {
    setOpen(false);
    if (signedIn) {
      await switchTo(userId);
      navigate("/", { replace: true });
    } else {
      clearActiveSession();
      navigate(`/login?user=${encodeURIComponent(name)}`, { replace: true });
    }
  }

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
          {roster.length > 0 && (
            <>
              <li role="none" className="nav-dropdown-section" aria-hidden="true">
                Switch user
              </li>
              {roster.map((u) => (
                <li key={u.userId} role="none">
                  <button
                    role="menuitem"
                    className="nav-dropdown-item nav-dropdown-item-button"
                    data-testid="switch-user"
                    data-user-id={u.userId}
                    data-signed-in={u.signedIn}
                    type="button"
                    onClick={() => void onSwitch(u.userId, u.signedIn, u.username)}
                  >
                    {u.username}
                    <span className="switch-user-status" aria-hidden="true">
                      {u.signedIn ? "Signed in" : "Sign in"}
                    </span>
                  </button>
                </li>
              ))}
            </>
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
