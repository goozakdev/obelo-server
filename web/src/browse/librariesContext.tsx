import { createContext, useContext, type ReactNode } from "react";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import type { Library } from "../api/types";
import { useAsync, type AsyncState } from "./useAsync";

// The caller's Libraries, loaded once per session and shared app-wide.
//
// AppHeader renders on every authed screen and derives its media nav (Music /
// Movies / TV) from the Library list. Fetching in the header itself would re-hit
// GET /libraries on every navigation (the header remounts per screen) and make
// the nav links flicker in and out. Loading here — keyed on the session token —
// gives a single fetch that stays warm across navigations and refetches on a
// re-login.

const LibrariesContext = createContext<AsyncState<Library[]> | null>(null);

export function LibrariesProvider({ children }: { children: ReactNode }) {
  const { session } = useAuth();
  const token = session?.token ?? null;
  const state = useAsync<Library[]>(
    (signal) => (token ? apiClient.listLibraries(signal) : Promise.resolve([])),
    [token],
  );
  return (
    <LibrariesContext.Provider value={state}>
      {children}
    </LibrariesContext.Provider>
  );
}

/** Read the shared Libraries state. Throws outside a LibrariesProvider. */
export function useLibraries(): AsyncState<Library[]> {
  const ctx = useContext(LibrariesContext);
  if (!ctx)
    throw new Error("useLibraries must be used within a LibrariesProvider");
  return ctx;
}
