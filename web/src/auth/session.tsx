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
import { NetworkError } from "../api/errors";
import { useOptionalFeature } from "../serverInfoContext";
import { browserDevice } from "./clientId";
import type { LoginResult, Role, User } from "../api/types";
import {
  demoteUser,
  forgetUser,
  getRosterEntry,
  loadServerId,
  rememberUser,
  rosterUsers,
  saveServerId,
  seedKnownUsers,
  type RosterUser,
} from "./roster";

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
//
// Remember me + roster (appletv-parity/10). The user hint follows the token's
// retention tier: durable (localStorage) when Remember me is on, session-only
// (sessionStorage, gone on tab close) when off — the token store (webTokenStore)
// governs the token, this layer mirrors it for the user hint. A per-Server
// remembered-Users roster (keyed by the Server identity id) surfaces a lightweight
// switch-user affordance: a Signed-in entry (retained durable token) switches
// instantly and auth-free; a Known entry re-authenticates. A switch swaps the
// session, which the per-userId Queue/prefs stores already react to — tearing down
// the previous identity's playback (client ADR-0009) rather than swapping a header.

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
   * stored. `remember` governs token retention only (NO password is ever stored):
   * true (default) → durable localStorage; false → session-only, gone on tab
   * close. Throws ApiError on failure (the caller shows the message). */
  login(username: string, password: string, remember?: boolean): Promise<void>;
  /** Log out: revoke server-side, clear token + media cookie + local session. The
   * user stays a Known roster entry (demoted from Signed-in) for quick re-login. */
  logout(): Promise<void>;
  /** Adopt a fresh login result directly (e.g. an auto-login right after setup,
   * if a screen chooses to). The client has already stored the token. */
  adopt(result: LoginResult): void;
  /** The other remembered Users for this server (excludes the active user) — the
   * switch-user surface. Each carries whether it can switch instantly (`signedIn`)
   * or must re-authenticate. */
  roster: RosterUser[];
  /** Switch instantly to a Signed-in roster entry by adopting its retained durable
   * token — auth-free. A no-op for a Known entry (the caller routes to /login).
   * The session swap tears down the previous identity's playback. */
  switchTo(userId: string): Promise<void>;
  /** Tear down the active session LOCALLY — clear the active token + user hint,
   * WITHOUT revoking server-side and WITHOUT touching the roster — so the app can
   * step aside to /login and re-authenticate as a different (Known) User. The
   * current user's retained roster token (if any) survives, so they stay an
   * instant-switch target. */
  clearActiveSession(): void;
  /** Forget a remembered User (remove from the roster). */
  forgetRosterUser(userId: string): void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

// The user hint is read from whichever tier holds it — localStorage (durable)
// wins over sessionStorage (session-only) — mirroring the token store, so a
// restored session resumes from either after a reload.
function readUserFrom(storage: Storage): User | null {
  try {
    const raw = storage.getItem(USER_KEY);
    if (!raw) return null;
    const u = JSON.parse(raw) as User;
    if (u && typeof u.id === "string" && typeof u.role === "string") return u;
    return null;
  } catch {
    return null;
  }
}

function loadStoredUser(): User | null {
  try {
    return readUserFrom(window.localStorage) ?? readUserFrom(window.sessionStorage);
  } catch {
    return null;
  }
}

/** The stored user's id, read from EITHER tier — the synchronous seam the Queue
 * and playback-prefs stores key on so they load the right user on first render,
 * before auth hydrates. Exported so those stores share one notion of "who". */
export function persistedUserId(): string | null {
  return loadStoredUser()?.id ?? null;
}

// Persist the user hint in the tier matching the token's retention: durable
// (localStorage) or session-only (sessionStorage). Writing one tier clears the
// other so exactly one copy exists; null clears both (logout/teardown).
function writeUser(user: User | null, durable: boolean): void {
  const put = (storage: Storage, value: string | null) => {
    try {
      if (value === null) storage.removeItem(USER_KEY);
      else storage.setItem(USER_KEY, value);
    } catch {
      // Storage unavailable; the session simply won't survive a reload.
    }
  };
  if (user === null) {
    put(window.localStorage, null);
    put(window.sessionStorage, null);
    return;
  }
  const json = JSON.stringify(user);
  if (durable) {
    put(window.localStorage, json);
    put(window.sessionStorage, null);
  } else {
    put(window.sessionStorage, json);
    put(window.localStorage, null);
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
  // The Server identity id (ADR-0034) that keys the roster. Seeded synchronously
  // from storage (a prior handshake) and refreshed by a best-effort probe below.
  const [serverId, setServerId] = useState<string | null>(loadServerId);
  // Bumped whenever the persisted roster changes so the derived `roster` recomputes.
  const [rosterVersion, setRosterVersion] = useState(0);
  const bumpRoster = useCallback(() => setRosterVersion((v) => v + 1), []);
  // Whether the server advertises POST /auth/media-cookie (appletv-parity/12): the
  // bearer-authed re-issue of the HttpOnly ms_media cookie. Read via the OPTIONAL
  // gate so AuthProvider still mounts bare in unit tests (no ServerInfoProvider) —
  // there the flag simply reads false and the switch skips the refresh, exactly as
  // it should against a server too old to advertise the route.
  const canRefreshMediaCookie = useOptionalFeature("mediaCookieRefresh");

  const clearSession = useCallback(() => {
    client.setToken(null);
    writeUser(null, true);
    setSession(null);
  }, [client]);

  // Learn the Server identity id for roster keying. Best-effort and guarded: a
  // stub client (tests) has no getServerInfo, and an offline/pre-ADR-0034 server
  // simply leaves the roster on its stored key. Persisted so a later authed load
  // (which never renders the login gates) still keys the roster correctly.
  useEffect(() => {
    if (typeof client.getServerInfo !== "function") return;
    const controller = new AbortController();
    let active = true;
    client
      .getServerInfo(controller.signal)
      .then((info) => {
        if (active && info?.id) {
          saveServerId(info.id);
          setServerId(info.id);
        }
      })
      .catch(() => {
        /* offline / unreachable — keep the stored serverId */
      });
    return () => {
      active = false;
      controller.abort();
    };
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

  // Resolve the roster key (Server identity id), preferring live state but falling
  // back to storage and a one-off handshake so a login that races the mount probe
  // still keys the roster correctly rather than under the "unknown" bucket.
  const resolveServerId = useCallback(async (): Promise<string | null> => {
    if (serverId) return serverId;
    const stored = loadServerId();
    if (stored) {
      setServerId(stored);
      return stored;
    }
    if (typeof client.getServerInfo !== "function") return null;
    try {
      const info = await client.getServerInfo();
      if (info?.id) {
        saveServerId(info.id);
        setServerId(info.id);
        return info.id;
      }
    } catch {
      /* offline — fall through to the unknown bucket */
    }
    return null;
  }, [client, serverId]);

  const login = useCallback(
    async (username: string, password: string, remember = true) => {
      // Choose token retention BEFORE the client stores the token: durable
      // (localStorage) when Remember me is on, session-only otherwise. NO password
      // is stored either way — only the opaque bearer token.
      client.setTokenDurable(remember);
      const res = await client.login({
        username,
        password,
        device: browserDevice(),
      });
      writeUser(res.user, remember);
      setSession({ token: res.token, user: res.user });
      const sid = await resolveServerId();
      // Record in the per-server roster: Signed-in (retained durable token) when
      // Remember me is on, else Known.
      rememberUser(window.localStorage, sid, res.user, remember ? res.token : null);
      bumpRoster();
      // Admin roster seeding (best-effort): pre-populate Known entries for the
      // other server Users so they appear as switch targets (Admin scope; the web
      // app already has it). Guarded for stub clients in tests.
      if (hasRole(res.user.role, "admin") && typeof client.listUsers === "function") {
        void client
          .listUsers()
          .then((users) => {
            seedKnownUsers(window.localStorage, sid, users);
            bumpRoster();
          })
          .catch(() => {
            /* seeding is best-effort */
          });
      }
    },
    [client, resolveServerId, bumpRoster],
  );

  const logout = useCallback(async () => {
    const uid = session?.user.id;
    try {
      await client.logout();
    } finally {
      // logout() already dropped the token; clear the rest regardless of network.
      // The user stays remembered as a Known entry (its token was just revoked).
      if (uid) {
        demoteUser(window.localStorage, serverId, uid);
        bumpRoster();
      }
      writeUser(null, true);
      setSession(null);
    }
  }, [client, session, serverId, bumpRoster]);

  const adopt = useCallback(
    (result: LoginResult) => {
      // The client stored the token durably by default; record a Signed-in entry.
      writeUser(result.user, true);
      setSession({ token: result.token, user: result.user });
      rememberUser(window.localStorage, serverId, result.user, result.token);
      bumpRoster();
    },
    [serverId, bumpRoster],
  );

  // Instant, auth-free switch to a Signed-in roster entry: adopt its retained
  // durable token + identity. The session swap re-keys the per-userId Queue/prefs
  // stores, tearing down the previous identity's active playback (ADR-0009). A
  // Known entry (no token) is a no-op here — the switch-user UI routes it to login.
  const switchTo = useCallback(
    async (userId: string) => {
      const entry = getRosterEntry(window.localStorage, serverId, userId);
      if (!entry?.token) return;
      client.setTokenDurable(true);
      client.setToken(entry.token);
      const user: User = {
        id: entry.userId,
        username: entry.username,
        role: entry.role,
      };
      writeUser(user, true);
      setSession({ token: entry.token, user });
      // Re-issue the HttpOnly ms_media cookie so browser byte-serving (<video>/<img>/
      // HLS GETs, which can't send a bearer) flips to the switched-in identity BEFORE
      // any media resumes — the one thing the JS-side token swap above cannot do
      // itself (the cookie is HttpOnly). Gated on the feature flag: a server too old
      // to advertise the route simply doesn't get the refresh (media falls back to
      // today's behaviour until the next real login). Best-effort — a failed refresh
      // must never break the switch (the JSON/browse path already works).
      if (canRefreshMediaCookie && typeof client.refreshMediaCookie === "function") {
        void client.refreshMediaCookie().catch(() => {
          /* best-effort: leave the previous cookie rather than fail the switch */
        });
      }
      // Confirm the adopted token. A dead one 401s — the global handler clears the
      // session and the guard routes to /login — and we demote it to Known so it
      // stops presenting as an instant switch. An offline probe is left alone.
      if (typeof client.verifySession === "function") {
        void client.verifySession().catch((err: unknown) => {
          if (err instanceof NetworkError) return;
          demoteUser(window.localStorage, serverId, userId);
          bumpRoster();
        });
      }
    },
    [client, serverId, bumpRoster, canRefreshMediaCookie],
  );

  const forgetRosterUser = useCallback(
    (userId: string) => {
      forgetUser(window.localStorage, serverId, userId);
      bumpRoster();
    },
    [serverId, bumpRoster],
  );

  // The switch targets: every remembered User for this server except the active
  // one. Recomputed when the roster changes (`rosterVersion`), the server is
  // identified, or the active user changes.
  const roster = useMemo<RosterUser[]>(() => {
    const activeId = session?.user.id;
    void rosterVersion; // recompute trigger
    return rosterUsers(window.localStorage, serverId).filter(
      (u) => u.userId !== activeId,
    );
  }, [serverId, rosterVersion, session]);

  const value = useMemo<AuthContextValue>(
    () => ({
      session,
      ready,
      isAuthenticated: session !== null,
      isAdmin: hasRole(session?.user.role, "admin"),
      login,
      logout,
      adopt,
      roster,
      switchTo,
      clearActiveSession: clearSession,
      forgetRosterUser,
    }),
    [
      session,
      ready,
      login,
      logout,
      adopt,
      roster,
      switchTo,
      clearSession,
      forgetRosterUser,
    ],
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
