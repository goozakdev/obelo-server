import type { ReactElement, ReactNode } from "react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import { QueueProvider } from "../player/queue/useQueue";
import { LibrariesProvider } from "../browse/librariesContext";
import type { ApiClient } from "../api/client";

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
}

const ADMIN_USER: SeedUser = { id: "u1", username: "operator", role: "admin" };

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
      </MemoryRouter>
    );
  }

  return render(ui, { wrapper: Wrapper });
}
