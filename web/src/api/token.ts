// TokenStore is the seam for where the bearer token lives. Login populates it
// from `POST /auth/login`; the client reads it to attach `Authorization: Bearer
// <token>` when a token is present. It is intentionally a tiny, swappable object
// — the app default is {@link webTokenStore} (durable/session-only retention for
// Remember me); tests substitute an in-memory store.
export interface TokenStore {
  get(): string | null;
  set(token: string | null): void;
}

const STORAGE_KEY = "juicebox.token";

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

// A token store that additionally carries a runtime RETENTION choice — the seam
// behind "Remember me" (appletv-parity/10). NO password is ever stored, only the
// opaque bearer token; retention governs *where* that token lives.
export interface RetainableTokenStore extends TokenStore {
  /** Choose where a subsequently-set token is persisted, re-homing any current
   * token so a post-sign-in choice takes effect. `true` → localStorage (durable,
   * survives a tab close — Remember me on); `false` → sessionStorage (session-only,
   * gone when the tab closes — Remember me off). */
  setDurable(durable: boolean): void;
  /** Whether the currently-held token is durable (persisted to localStorage). */
  isDurable(): boolean;
}

function safeGet(storage: Storage, key: string): string | null {
  try {
    return storage.getItem(key);
  } catch {
    return null;
  }
}

function safeSet(storage: Storage, key: string, value: string | null): void {
  try {
    if (value === null) storage.removeItem(key);
    else storage.setItem(key, value);
  } catch {
    // Storage unavailable; the token simply won't persist this session.
  }
}

// webTokenStore is the app's DEFAULT token store. It reads the token from whichever
// tier holds one — localStorage (durable) wins over sessionStorage — so a restored
// session resumes from either after a reload. Writes go to the ACTIVE tier chosen
// by the last Remember-me decision (durable by default, matching the historical
// "always persist" behaviour); the other tier is kept clear so exactly one copy
// exists. Degrades to a no-op where web storage is unavailable.
export function webTokenStore(): RetainableTokenStore {
  const local = (): Storage | null => {
    try {
      return window.localStorage;
    } catch {
      return null;
    }
  };
  const session = (): Storage | null => {
    try {
      return window.sessionStorage;
    } catch {
      return null;
    }
  };

  const readLocal = (): string | null => {
    const s = local();
    return s ? safeGet(s, STORAGE_KEY) : null;
  };
  const readSession = (): string | null => {
    const s = session();
    return s ? safeGet(s, STORAGE_KEY) : null;
  };

  // Derive the initial retention from where a token currently lives: a durably
  // restored token (localStorage) is durable; a session-only one is not; with no
  // token, default to durable (the historical behaviour) until a login chooses.
  let durable = readSession() !== null && readLocal() === null ? false : true;

  const write = (token: string | null): void => {
    const l = local();
    const s = session();
    if (token === null) {
      if (l) safeSet(l, STORAGE_KEY, null);
      if (s) safeSet(s, STORAGE_KEY, null);
      return;
    }
    if (durable) {
      if (l) safeSet(l, STORAGE_KEY, token);
      if (s) safeSet(s, STORAGE_KEY, null);
    } else {
      if (s) safeSet(s, STORAGE_KEY, token);
      if (l) safeSet(l, STORAGE_KEY, null);
    }
  };

  return {
    get(): string | null {
      // Durable wins so a Remember-me session takes precedence over a stale
      // session-only copy in the same tab.
      return readLocal() ?? readSession();
    },
    set(token: string | null): void {
      write(token);
    },
    setDurable(next: boolean): void {
      if (next === durable) return;
      const token = readLocal() ?? readSession();
      durable = next;
      write(token);
    },
    isDurable(): boolean {
      return durable;
    },
  };
}

/** Narrow a TokenStore to a RetainableTokenStore (the web store) when it supports
 * a retention choice; a plain/memory store simply lacks the methods (tests). */
export function supportsRetention(
  store: TokenStore,
): store is RetainableTokenStore {
  return (
    typeof (store as RetainableTokenStore).setDurable === "function" &&
    typeof (store as RetainableTokenStore).isDurable === "function"
  );
}
