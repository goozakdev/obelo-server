// TokenStore is the seam for where the bearer token lives. Login (issue 02)
// will populate it from `POST /auth/login`; this slice only reads it so the
// client can attach `Authorization: Bearer <token>` when a token is present.
//
// The default implementation persists to localStorage so a session survives a
// page reload (PRD user story 4). It is intentionally a tiny, swappable object
// — tests and later auth work can substitute an in-memory store.
export interface TokenStore {
  get(): string | null;
  set(token: string | null): void;
}

const STORAGE_KEY = "juicebox.token";

// localStorageTokenStore persists the token across reloads. It degrades to a
// no-op read (null) if localStorage is unavailable (e.g. SSR, privacy mode),
// so the client still works for anonymous endpoints like the handshake.
export const localStorageTokenStore: TokenStore = {
  get(): string | null {
    try {
      return window.localStorage.getItem(STORAGE_KEY);
    } catch {
      return null;
    }
  },
  set(token: string | null): void {
    try {
      if (token === null) {
        window.localStorage.removeItem(STORAGE_KEY);
      } else {
        window.localStorage.setItem(STORAGE_KEY, token);
      }
    } catch {
      // Storage unavailable; the token simply won't persist this session.
    }
  },
};

// memoryTokenStore keeps the token in a closure — useful for tests and any
// context without a DOM.
export function memoryTokenStore(initial: string | null = null): TokenStore {
  let token = initial;
  return {
    get: () => token,
    set: (t) => {
      token = t;
    },
  };
}
