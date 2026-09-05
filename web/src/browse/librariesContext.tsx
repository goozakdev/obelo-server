import {
  createContext,
  useContext,
  useEffect,
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

// libraryId → the name of the Server providing it, for the linked ones. Empty on
// the overwhelmingly common server that has never linked anything.
const ProvidersContext = createContext<Record<string, string>>({});

export function LibrariesProvider({ children }: { children: ReactNode }) {
  const { session, isAdmin } = useAuth();
  const token = session?.token ?? null;
  const state = useAsync<Library[]>(
    (signal) => (token ? apiClient.listLibraries(signal) : Promise.resolve([])),
    [token],
  );
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
    if (!isAdmin || !libraries || !libraries.some((l) => l.linked)) return;
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
      <ProvidersContext.Provider value={providers}>
        {children}
      </ProvidersContext.Provider>
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
