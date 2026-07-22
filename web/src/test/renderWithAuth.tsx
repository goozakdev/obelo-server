import type { ReactElement, ReactNode } from "react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import { QueueProvider } from "../player/queue/useQueue";
import { LibrariesProvider } from "../browse/librariesContext";
import { ServerInfoStateProvider } from "../serverInfoContext";
import type { ApiClient } from "../api/client";
import type { ServerInfo } from "../api/types";

// Renders a browse screen inside a real AuthProvider seeded with a logged-in
// session, so the shared AppHeader (which calls useAuth) mounts. The provider's
// hydrate path reads a token + user from localStorage and runs a background
// verifySession; we give it a stub client that satisfies those calls, keeping
// the test focused on the screen's own data (mocked separately on apiClient).

/** The seeded session user. Defaults to an Admin; pass a `member` role (or a full
 * user) to assert the role-aware control split (collections-playlists-ui issue
 * 02): an Admin sees the curation controls, a Member sees the read-only view. */
export interface SeedUser {
  id: string;
  username: string;
  role: string;
}

interface Options {
  /** Initial router entries (defaults to ["/"]). */
  initialEntries?: string[];
  /** The logged-in user to seed (defaults to the Admin "operator"). */
  user?: SeedUser;
  /** Feature flags the seeded handshake advertises (Apple TV → Web parity §4).
   * Merged over the defaults, so a test that gates a surface on a flag can flip
   * just that one — e.g. `{ playlists: false }` to assert the hidden state. */
  features?: Record<string, boolean>;
}

const ADMIN_USER: SeedUser = { id: "u1", username: "operator", role: "admin" };

// The handshake a seeded session gets by default: every capability the current
// server advertises (internal/server/server.go) is on, so a screen's header
// links and flag-gated affordances render exactly as against a live server. A
// test flips a single flag through `opts.features` to exercise the hidden path.
const DEFAULT_FEATURES: Record<string, boolean> = {
  collections: true,
  playlists: true,
  realtimeEvents: true,
  deviceAuth: true,
  remuxSelectedOnly: true,
  transcode: false,
};

function seededServerInfo(features?: Record<string, boolean>): ServerInfo {
  return {
    version: "test",
    supportedVersions: [1],
    features: { ...DEFAULT_FEATURES, ...features },
    setupRequired: false,
  };
}

function authStubClient(): ApiClient {
  // Minimal surface AuthProvider touches on mount.
  return {
    token: "fake-token",
    setToken: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
  } as unknown as ApiClient;
}

export function renderWithAuth(ui: ReactElement, opts: Options = {}) {
  // Seed a session so the provider hydrates as authenticated. The role defaults
  // to Admin; a test asserting the role-aware split passes a member user.
  window.localStorage.setItem("juicebox.token", "fake-token");
  window.localStorage.setItem(
    "juicebox.user",
    JSON.stringify(opts.user ?? ADMIN_USER),
  );

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <MemoryRouter initialEntries={opts.initialEntries ?? ["/"]}>
        {/* The handshake context is part of the app spine (App.tsx mounts it above
            the auth scope) — the shared AppHeader reads it to gate the Playlists /
            Collections links on the server's feature flags. Seeded ready so screen
            tests get the links by default; `opts.features` flips a flag off. */}
        <ServerInfoStateProvider
          state={{ status: "ready", info: seededServerInfo(opts.features) }}
        >
          <AuthProvider client={authStubClient()}>
            {/* The Libraries store is part of the app spine (App.tsx mounts it in
                the auth scope) — the shared AppHeader reads it for its media nav. */}
            <LibrariesProvider>
              {/* The Queue store is part of the app spine (App.tsx mounts it inside
                  the auth scope), so screen tests get it too — inert for screens
                  that don't read it, and the seam the player/playlist queue tests
                  drive. */}
              <QueueProvider>{children}</QueueProvider>
            </LibrariesProvider>
          </AuthProvider>
        </ServerInfoStateProvider>
      </MemoryRouter>
    );
  }

  return render(ui, { wrapper: Wrapper });
}
