import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { apiClient, type ApiClient } from "../api/client";
import { browserDevice } from "./clientId";
import type { LoginResult, Role, User } from "../api/types";

// The auth/session layer (PRD "First-run & auth").
//
// Holds the current user + token, hydrated from storage on load so a session
// survives a reload. It is the ONE place the app reads "who am I / am I logged
// in / what role." Login/logout flow through here so the token store and the
// in-memory session never drift. The single 401 handler is registered on the
// API client here: any unauthorized response clears the session, and the route
// guards then bounce to /login.
//
// Why persist the user (not just the token): the API has no "GET /me", so to
// restore the role on reload without a round-trip we store the user object next
// to the token. The token is still validated by the server on the first real
// request; a 401 clears everything. The persisted user is a hint, never the
// authority.

const USER_KEY = "juicebox.user";

export interface Session {
  user: User;
  token: string;
}

interface AuthContextValue {
  /** The current session, or null when logged out. */
  session: Session | null;
  /** True until the initial hydrate-from-storage completes (one tick). Guards
   * wait for this so a stored session isn't briefly treated as "logged out". */
  ready: boolean;
  /** Convenience: is someone logged in. */
  isAuthenticated: boolean;
  /** Convenience: is the current user an Admin. False when logged out. */
  isAdmin: boolean;
  /** Log in with credentials; on success the session is populated and the token
   * stored. Throws ApiError on failure (the caller shows the message). */
  login(username: string, password: string): Promise<void>;
  /** Log out: revoke server-side, clear token + media cookie + local session. */
  logout(): Promise<void>;
  /** Adopt a fresh login result directly (e.g. an auto-login right after setup,
   * if a screen chooses to). The client has already stored the token. */
  adopt(result: LoginResult): void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

function loadStoredUser(): User | null {
  try {
    const raw = window.localStorage.getItem(USER_KEY);
    if (!raw) return null;
    const u = JSON.parse(raw) as User;
    if (u && typeof u.id === "string" && typeof u.role === "string") return u;
    return null;
  } catch {
    return null;
  }
}

function storeUser(user: User | null): void {
  try {
    if (user === null) window.localStorage.removeItem(USER_KEY);
    else window.localStorage.setItem(USER_KEY, JSON.stringify(user));
  } catch {
    // Storage unavailable; the session simply won't survive a reload.
  }
}

export interface AuthProviderProps {
  children: ReactNode;
  /** Injectable client for tests; defaults to the app singleton. */
  client?: ApiClient;
}

export function AuthProvider({ children, client = apiClient }: AuthProviderProps) {
  const [session, setSession] = useState<Session | null>(null);
  const [ready, setReady] = useState(false);

  const clearSession = useCallback(() => {
    client.setToken(null);
    storeUser(null);
    setSession(null);
  }, [client]);

  // Hydrate from storage once on mount: if a token AND a stored user exist, we
  // resume the session optimistically, then fire a lightweight authenticated
  // probe to confirm the token is still valid. A revoked/garbage token returns
  // 401, which the unauthorized handler (below) turns into a cleared session —
  // so a stale restored session degrades to the login screen (PRD user story 5).
  useEffect(() => {
    const token = client.token;
    const user = loadStoredUser();
    if (token && user) {
      setSession({ token, user });
      // Confirm the restored token in the background. The 401 handler does the
      // teardown; other errors (offline) leave the optimistic session in place.
      void client.verifySession().catch(() => {
        /* handled by the global 401 handler / ignored when offline */
      });
    } else if (token || user) {
      // Half a session (one without the other) is no session — clear both.
      clearSession();
    }
    setReady(true);
  }, [client, clearSession]);

  // Register THE single 401 handler: any unauthorized response anywhere clears
  // the session. The route guards observe `session === null` and redirect to
  // /login. Re-registered if the client instance changes (tests).
  useEffect(() => {
    client.setUnauthorizedHandler(() => clearSession());
    return () => client.setUnauthorizedHandler(undefined);
  }, [client, clearSession]);

  const login = useCallback(
    async (username: string, password: string) => {
      const res = await client.login({
        username,
        password,
        device: browserDevice(),
      });
      storeUser(res.user);
      setSession({ token: res.token, user: res.user });
    },
    [client],
  );

  const logout = useCallback(async () => {
    try {
      await client.logout();
    } finally {
      // logout() already dropped the token; clear the rest regardless of network.
      storeUser(null);
      setSession(null);
    }
  }, [client]);

  const adopt = useCallback((result: LoginResult) => {
    storeUser(result.user);
    setSession({ token: result.token, user: result.user });
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({
      session,
      ready,
      isAuthenticated: session !== null,
      isAdmin: hasRole(session?.user.role, "admin"),
      login,
      logout,
      adopt,
    }),
    [session, ready, login, logout, adopt],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

/** Read the auth context. Throws if used outside an AuthProvider (a wiring bug). */
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within an AuthProvider");
  return ctx;
}

/** Role check kept in one place so the gate's notion of a role is consistent.
 * Built to extend: today only "admin" is meaningful, but Member support drops
 * in by adding cases without touching the guards. */
export function hasRole(role: Role | undefined, required: Role): boolean {
  if (!role) return false;
  return role === required;
}
