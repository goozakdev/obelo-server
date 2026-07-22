import { createContext, useContext, useMemo, type ReactNode } from "react";
import { apiClient, type ApiClient } from "./api/client";
import { useServerInfo, type ServerState } from "./useServerInfo";
import type { ServerInfo } from "./api/types";

// The shared handshake context (Apple TV → Web parity §4).
//
// The GET /server handshake advertises a `features` map — the server's own
// statement of which capabilities it exposes. A client MUST gate on those flags,
// NEVER on the server version: an older server simply omits a flag, and the
// correct behaviour is to keep the feature off (the TV client does exactly this
// via `serverInfo.feature("…")`). This provider runs the handshake once, above
// the auth scope, and hands the whole tree a `feature(name)` gate plus the raw
// handshake state the first-run gates render from.

interface ServerInfoContextValue {
  /** The handshake state (loading/ready/unreachable/error) — the first-run gates
   * (Setup/Login) render their connecting/unreachable screens from it. */
  state: ServerState;
  /** True ONLY when `name` is advertised present-and-true in the handshake's
   * `features` map. An absent flag, a missing/empty map, or a not-yet-ready
   * handshake all resolve to false. Gate capabilities on this, never on the
   * server version. */
  feature(name: string): boolean;
}

const ServerInfoContext = createContext<ServerInfoContextValue | null>(null);

/** Pure flag read: present-and-true wins; everything else — an absent flag, a
 * missing or empty `features` map, an undefined handshake — is false. Kept pure
 * (no React, no fetch) so it is trivially unit-testable and reused by the context
 * helper below. */
export function readFeature(
  info: ServerInfo | undefined,
  name: string,
): boolean {
  return info?.features?.[name] === true;
}

export interface ServerInfoProviderProps {
  children: ReactNode;
  /** Injectable client for tests; defaults to the app singleton. */
  client?: ApiClient;
}

/** App-level provider: runs the GET /server handshake once (via {@link
 * useServerInfo}) and shares both the handshake state and the `feature()` gate
 * with the whole tree. Mounted above the auth scope so the first-run gates and
 * every authed screen read the one same result. */
export function ServerInfoProvider({
  children,
  client = apiClient,
}: ServerInfoProviderProps) {
  const state = useServerInfo(client);
  return (
    <ServerInfoStateProvider state={state}>{children}</ServerInfoStateProvider>
  );
}

/** Provider from an explicit, already-resolved state — the seam tests and stories
 * use to supply a handshake synchronously (no fetch, no async settle). The
 * fetching {@link ServerInfoProvider} is a thin wrapper over this. */
export function ServerInfoStateProvider({
  state,
  children,
}: {
  state: ServerState;
  children: ReactNode;
}) {
  const value = useMemo<ServerInfoContextValue>(
    () => ({
      state,
      feature: (name) =>
        readFeature(state.status === "ready" ? state.info : undefined, name),
    }),
    [state],
  );
  return (
    <ServerInfoContext.Provider value={value}>
      {children}
    </ServerInfoContext.Provider>
  );
}

/** Read the shared handshake context. Throws outside a provider (a wiring bug). */
export function useServerInfoContext(): ServerInfoContextValue {
  const ctx = useContext(ServerInfoContext);
  if (!ctx)
    throw new Error(
      "useServerInfoContext must be used within a ServerInfoProvider",
    );
  return ctx;
}

/** Convenience gate on a single named feature flag, e.g.
 * `useFeature("remuxSelectedOnly")`. Returns true only when the flag is
 * advertised present-and-true. */
export function useFeature(name: string): boolean {
  return useServerInfoContext().feature(name);
}
