import { lazy, Suspense, useEffect, type ReactNode } from "react";
import { BrowserRouter, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { AuthProvider, useAuth } from "./auth/session";
import { RequireAdmin, RequireAuth } from "./auth/guards";
import { ServerInfoProvider, useServerInfoContext } from "./serverInfoContext";
import SetupScreen from "./screens/SetupScreen";
import LoginScreen from "./screens/LoginScreen";
import SignInCallbackScreen from "./screens/SignInCallbackScreen";
import HomeScreen from "./screens/HomeScreen";
import NowPlayingBar from "./player/NowPlayingBar";
import MediaSessionBridge from "./player/MediaSessionBridge";
import EnrichmentConsentGate from "./admin/EnrichmentConsentGate";
import { QueueProvider } from "./player/queue/useQueue";
import { PlaybackTransportProvider } from "./player/transport";
import { LibrariesProvider } from "./browse/librariesContext";
import ScrollToTop from "./ScrollToTop";
import { appEvents } from "./events/enrichEvents";
import { ChunkErrorBoundary } from "./lib/chunkRecovery";

// Route screens other than the login/setup/home shells load on demand, so a
// first paint (and a Member, who can never open /admin) does not download and
// parse every screen. Each import() becomes its own chunk.
const ProfileScreen = lazy(() => import("./screens/ProfileScreen"));
const LinkScreen = lazy(() => import("./screens/LinkScreen"));
const AdminScreen = lazy(() => import("./screens/AdminScreen"));
const LibraryListScreen = lazy(() => import("./browse/LibraryListScreen"));
const LibraryGridScreen = lazy(() => import("./browse/LibraryGridScreen"));
const CollectionsScreen = lazy(() => import("./browse/CollectionsScreen"));
const CollectionDetailScreen = lazy(() => import("./browse/CollectionDetailScreen"));
const PlaylistsScreen = lazy(() => import("./browse/PlaylistsScreen"));
const PlaylistDetailScreen = lazy(() => import("./browse/PlaylistDetailScreen"));
const OnlineSourceScreen = lazy(() => import("./screens/OnlineSourceScreen"));
const ShowDetailScreen = lazy(() => import("./browse/ShowDetailScreen"));
const TitleDetailScreen = lazy(() => import("./browse/TitleDetailScreen"));
const MusicLibraryScreen = lazy(() => import("./music/MusicLibraryScreen"));
const ArtistDetailScreen = lazy(() => import("./music/ArtistDetailScreen"));
const AlbumDetailScreen = lazy(() => import("./music/AlbumDetailScreen"));
const TrackDetailScreen = lazy(() => import("./music/TrackDetailScreen"));

// App is the router root (issue 02): auth/first-run + the role gate + a minimal
// authed landing. Browse, player, home rows, and admin screens are later issues.
//
// Routes:
//   /setup   first-run only (handshake setupRequired) → create first Admin
//   /login   credentials → session
//   /sign-in/callback  a redirect-flow Sign-in provider's return (ADR-0063)
//   /link/:code?  approve a TV's sign-in code (RequireAuth) — ADR-0036
//   /profile the caller's own sign-ins, and attaching another (RequireAuth)
//   /        authed landing (RequireAuth)
//   /admin   admin-only placeholder (RequireAdmin)
//   *        unknown client routes fall back to the landing (which redirects to
//            login when unauthenticated), so deep links/refresh behave.
//
// BrowserRouter is fine because the Go server serves index.html for non-API
// paths (SPA fallback, PRD user story 37), so a refreshed deep link still loads
// the app.

export default function App() {
  return (
    <BrowserRouter>
      {/* Reset scroll to the top on each forward navigation so a new screen
          never inherits the previous screen's scroll offset (they share the one
          document scroll). Back/Forward keep the browser's restored position. */}
      <ScrollToTop />
      {/* The server handshake runs once here, above the auth scope, so the
          first-run gates (Setup/Login) and every authed screen read the one same
          result — and so `feature(name)` gating (Apple TV → Web parity §4) is a
          one-liner anywhere in the tree. */}
      <ServerInfoProvider>
      <AuthProvider>
        {/* Libraries load once inside the auth scope so the header's media nav
            (Music / Movies / TV) is derived from a single fetch shared across
            every screen rather than refetched on each navigation. */}
        <LibrariesProvider>
        {/* The Queue store lives inside the auth scope (it is scoped to the
            logged-in user and cleared on logout) and wraps every route so the
            player and the play affordances share one Queue. */}
        <QueueProvider>
        {/* The playback transport (play/pause state of the one media element) is
            shared so affordances outside the bar — e.g. an album track row's
            play/pause toggle — reflect and drive the current song. */}
        <PlaybackTransportProvider>
        <RouteBoundary>
        <Suspense fallback={<RouteLoading />}>
        <Routes>
          <Route path="/setup" element={<SetupGate />} />
          <Route path="/login" element={<LoginGate />} />
          {/* Where a redirect-flow Sign-in provider sends the browser back to
              (ADR-0063). Outside RequireAuth: nobody is signed in yet. */}
          <Route path="/sign-in/callback" element={<SignInCallbackScreen />} />
          {/* /link is where a TV's QR code lands (ADR-0036). RequireAuth is what
              implements "ask for a password first if you aren't signed in" — it
              bounces to /login and comes back — so this route needs no auth
              logic of its own. The code is a PATH param because the login bounce
              preserves only pathname, not search: /link?code=X would return from
              /login with the code gone. The bare /link form is the fallback for
              a hand-typed code. */}
          <Route
            path="/link/:code?"
            element={
              <RequireAuth>
                <LinkScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/"
            element={
              <RequireAuth>
                <HomeScreen />
              </RequireAuth>
            }
          />
          {/* Browse (issue 03), all behind RequireAuth: the library list, a
              library's poster grid, and a Title's detail. */}
          <Route
            path="/libraries"
            element={
              <RequireAuth>
                <LibraryListScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/libraries/:libraryId"
            element={
              <RequireAuth>
                <LibraryGridScreen />
              </RequireAuth>
            }
          />
          {/* Collections browse (collections-playlists-ui issue 01), behind
              RequireAuth: the Collections list and a Collection's member grid.
              Read-only for everyone; issue 02 adds inline Admin curation. */}
          <Route
            path="/collections"
            element={
              <RequireAuth>
                <CollectionsScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/collections/:id"
            element={
              <RequireAuth>
                <CollectionDetailScreen />
              </RequireAuth>
            }
          />
          {/* Playlists (collections-playlists-ui issue 03), behind RequireAuth:
              the caller's own playlists list and a playlist's ordered member
              grid. Owner-private — NOT role-gated; a non-owned playlist 404s
              (rendered as "not found"). Reorder (04) / play-through (05) extend
              the detail. */}
          {/* The caller's own profile: the sign-ins attached to their account,
              and attaching another (ADR-0063 decision 3). */}
          <Route
            path="/profile"
            element={
              <RequireAuth>
                <ProfileScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/playlists"
            element={
              <RequireAuth>
                <PlaylistsScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/playlists/:id"
            element={
              <RequireAuth>
                <PlaylistDetailScreen />
              </RequireAuth>
            }
          />
          {/* TV Show detail (tv-music issue 01): Seasons & Episodes for a Show. */}
          <Route
            path="/shows/:showId"
            element={
              <RequireAuth>
                <ShowDetailScreen />
              </RequireAuth>
            }
          />
          {/* Music experience (separate-music-ui): a self-contained section under
              /music with its own shell/header/theme. A music library lands on the
              Artist list, then Artist → Album → Track, all within /music. Playback
              still uses the shared player route below. */}
          <Route
            path="/music/libraries/:libraryId"
            element={
              <RequireAuth>
                <MusicLibraryScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/music/artists/:artistId"
            element={
              <RequireAuth>
                <ArtistDetailScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/music/albums/:albumId"
            element={
              <RequireAuth>
                <AlbumDetailScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/music/tracks/:titleId"
            element={
              <RequireAuth>
                <TrackDetailScreen />
              </RequireAuth>
            }
          />
          {/* An Online source's page (ADR-0068): reached from its Home tile. */}
          <Route
            path="/online/:sourceId"
            element={
              <RequireAuth>
                <OnlineSourceScreen />
              </RequireAuth>
            }
          />
          <Route
            path="/titles/:titleId"
            element={
              <RequireAuth>
                <TitleDetailScreen />
              </RequireAuth>
            }
          />
          {/* Admin hub (issues 06–07): a tabbed area — libraries/scanning,
              the attention surfaces, and devices — so the trailing path is
              matched by AdminScreen's own nested <Routes>. */}
          <Route
            path="/admin/*"
            element={
              <RequireAdmin>
                <AdminScreen />
              </RequireAdmin>
            }
          />
          {/* Unknown client routes → the landing, which itself redirects to
              /login when not authenticated. Keeps bookmarks/deep links sane. */}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
        </Suspense>
        </RouteBoundary>
        {/* The persistent player (ADR-0018 / now-playing-bar/01): mounted ONCE
            OUTSIDE <Routes> so it survives navigation — playback keeps going as
            the user browses. It renders nothing until a Queue is active. */}
        <NowPlayingBar />
        {/* Holds the shared /events stream open for the whole signed-in session.
            Each screen renders its own AppHeader (a listener), so without this the
            stream would close and reopen on every navigation, losing any event in
            the gap. */}
        <EventsKeepAlive />
        {/* Media Session bridge (appletv-parity/11): mounted once, inside the
            Queue + Transport providers, so OS media keys / the lock screen /
            the browser media hub reflect and drive MUSIC playback. Renders
            nothing; music-only, and a graceful no-op where the API is absent. */}
        <MediaSessionBridge />
        {/* First-run Enrichment consent prompt (ADR-0032): mounted once in the
            authed scope; shows only to an Admin who has not yet answered. */}
        <EnrichmentConsentGate />
        </PlaybackTransportProvider>
        </QueueProvider>
        </LibrariesProvider>
      </AuthProvider>
      </ServerInfoProvider>
    </BrowserRouter>
  );
}

// A failed lazy chunk (or a render error) shows an inline reload prompt here, so
// the NowPlayingBar and the other app-shell siblings stay mounted; navigating to
// another route clears it.
function RouteBoundary({ children }: { children: ReactNode }) {
  const { pathname } = useLocation();
  return <ChunkErrorBoundary resetKey={pathname}>{children}</ChunkErrorBoundary>;
}

export function EventsKeepAlive() {
  const { isAuthenticated } = useAuth();
  useEffect(() => {
    if (!isAuthenticated) return;
    return appEvents.subscribe(() => {});
  }, [isAuthenticated]);
  return null;
}

// SetupGate guards the setup screen with the live handshake: setup is only
// reachable on a fresh server (setupRequired). Once an Admin exists, /setup
// redirects to /login so the screen is never shown post-bootstrap.
function SetupGate() {
  const { state } = useServerInfoContext();
  if (state.status === "loading") return <Connecting />;
  if (state.status === "unreachable" || state.status === "error") {
    return <Unreachable message={describe(state)} />;
  }
  if (!state.info.setupRequired) {
    return <Navigate to="/login" replace />;
  }
  return <SetupScreen />;
}

// LoginGate sends a fresh server to /setup and an already-authenticated user to
// Home; otherwise it shows the login form.
function LoginGate() {
  const { isAuthenticated, ready } = useAuth();
  const { state } = useServerInfoContext();

  if (!ready || state.status === "loading") return <Connecting />;
  if (state.status === "unreachable" || state.status === "error") {
    return <Unreachable message={describe(state)} />;
  }
  if (state.info.setupRequired) {
    return <Navigate to="/setup" replace />;
  }
  if (isAuthenticated) {
    return <Navigate to="/" replace />;
  }
  return <LoginScreen />;
}

function describe(
  state: { status: "unreachable"; message: string } | { status: "error"; code: string; message: string },
): string {
  return state.status === "error" ? `${state.code}: ${state.message}` : state.message;
}

function RouteLoading() {
  return (
    <div className="app-shell" data-testid="route-loading">
      <main className="app-main">
        <p className="status status-loading">Loading&hellip;</p>
      </main>
    </div>
  );
}

function Connecting() {
  return (
    <div className="app-shell" data-testid="boot-connecting">
      <main className="app-main">
        <p className="status status-loading">Connecting to the server&hellip;</p>
      </main>
    </div>
  );
}

function Unreachable({ message }: { message: string }) {
  return (
    <div className="app-shell" data-testid="boot-unreachable">
      <main className="app-main">
        <p className="status status-error">
          <span className="dot dot-error" aria-hidden="true" />
          Server not reachable: {message}
        </p>
      </main>
    </div>
  );
}
