import {
  createContext,
  useContext,
  useCallback,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { apiClient } from "../api/client";
import { useAuth } from "../auth/session";
import type { Library, LinkedMarks } from "../api/types";
import { useAsync, type AsyncState } from "./useAsync";

// The caller's Libraries, loaded once per session and shared app-wide.
//
// AppHeader renders on every authed screen and derives its media nav (Music /
// Movies / TV) from the Library list. Fetching in the header itself would re-hit
// GET /libraries on every navigation (the header remounts per screen) and make
// the nav links flicker in and out. Loading here — keyed on the session token —
// gives a single fetch that stays warm across navigations and refetches on a
// re-login.
//
// The same list is the app's answer to "is this thing a mirror?" for the ONE
// document that does not say so itself: `titleDetailJSON` carries no
// `linked`/`available` (issue 14 deviation 2), and the Title/Track detail screens
// read the pair off the Title's Library here rather than asking for a wire
// change (see useLibraryMarks).

const LibrariesContext = createContext<AsyncState<Library[]> | null>(null);

// Re-reads the list. A no-op outside a provider so an Admin screen that calls it
// still renders in isolation.
const RefreshContext = createContext<() => void>(() => {});

// libraryId → the name of the Server providing it, for the linked ones. Empty on
// the overwhelmingly common server that has never linked anything.
const ProvidersContext = createContext<Record<string, string>>({});

export function LibrariesProvider({ children }: { children: ReactNode }) {
  const { session, isAdmin } = useAuth();
  const token = session?.token ?? null;
  // Bumped by refresh() so a library created, renamed or deleted in Admin shows
  // without a reload. keepPreviousData keeps the media nav steady while it
  // re-reads; the list is tagged with the token it was read for so a DIFFERENT
  // session never sees the previous one's Libraries while its own load runs.
  const [version, setVersion] = useState(0);
  const refresh = useCallback(() => setVersion((n) => n + 1), []);
  const raw = useAsync<{ token: string | null; libraries: Library[] }>(
    async (signal) => ({
      token,
      libraries: token ? await apiClient.listLibraries(signal) : [],
    }),
    [token, version],
    { keepPreviousData: true },
  );
  const state = useMemo<AsyncState<Library[]>>(() => {
    if (raw.status !== "ready") return raw;
    return raw.data.token === token
      ? { status: "ready", data: raw.data.libraries }
      : { status: "loading" };
  }, [raw, token]);
  const [providers, setProviders] = useState<Record<string, string>>({});

  // The Library JSON says only that a shelf IS linked; it never says whose it is
  // (issue 07 deviation 8), so the name comes from GET /links joined by library
  // id — the same join the Libraries admin hub does. Two guards keep it cheap:
  // it runs ONLY when something in hand is actually a mirror (a household that
  // has never linked makes exactly the requests it always did), and ONLY for an
  // Admin, because /links is an Admin route and a Member would collect a 403 on
  // every login for a decoration. A read that fails degrades to a bare badge.
  const libraries = state.status === "ready" ? state.data : null;
  useEffect(() => {
    if (!isAdmin || !libraries || !libraries.some((l) => l.linked)) {
      // Drop names fetched under a previous token/role: /links is Admin-only, so
      // a Member signing in after an Admin must not keep seeing them.
      setProviders((prev) => (Object.keys(prev).length > 0 ? {} : prev));
      return;
    }
    const ctrl = new AbortController();
    void (async () => {
      try {
        const links = await apiClient.listLinks(ctrl.signal);
        if (ctrl.signal.aborted) return;
        const map: Record<string, string> = {};
        for (const link of links) {
          for (const lib of link.libraries) map[lib.id] = link.serverName;
        }
        setProviders(map);
      } catch {
        // The badge is the load-bearing part and it renders without this.
      }
    })();
    return () => ctrl.abort();
  }, [isAdmin, libraries]);

  return (
    <LibrariesContext.Provider value={state}>
      <RefreshContext.Provider value={refresh}>
        <ProvidersContext.Provider value={providers}>
          {children}
        </ProvidersContext.Provider>
      </RefreshContext.Provider>
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

/** Re-read the shared Libraries list; call it after changing the set of Libraries. */
export function useRefreshLibraries(): () => void {
  return useContext(RefreshContext);
}

/** The mirror pair of a Library, for the detail screens whose own document does
 * not carry it (`titleDetailJSON`, issue 14 deviation 2). Empty — neither field —
 * while the list is loading, for an unknown id, and for an ordinary local
 * Library, which are the same answer as far as the mark is concerned: no badge. */
export function useLibraryMarks(libraryId: string | undefined): LinkedMarks {
  const libraries = useLibraries();
  if (!libraryId || libraries.status !== "ready") return {};
  const found = libraries.data.find((l) => l.id === libraryId);
  if (!found?.linked) return {};
  return { linked: true, available: found.available };
}

/** The name of the Server providing a linked Library, or "" when it is not known
 * cheaply (a Member, a failed /links read, a local Library). Callers pass it
 * straight to `LinkedMark providedBy`, which omits the note when it is empty. */
export function useLibraryProvider(libraryId: string | undefined): string {
  const providers = useContext(ProvidersContext);
  return (libraryId && providers[libraryId]) || "";
}
