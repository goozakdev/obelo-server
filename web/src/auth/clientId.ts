// A stable per-browser clientId (UUID). The server reuses/refreshes one Device
// per clientId on re-login instead of duplicating it (docs/api-contract.md,
// ADR-0015), so this value must persist across reloads — hence localStorage.
//
// It is independent of the bearer token: logging out clears the token but keeps
// the clientId, so the next login lands on the same Device.

const CLIENT_ID_KEY = "juicebox.clientId";

/** Returns this browser's stable clientId, generating and persisting one on
 * first use. Falls back to an ephemeral id if localStorage is unavailable
 * (privacy mode) — login still works; only Device dedup degrades. */
export function getClientId(): string {
  try {
    const existing = window.localStorage.getItem(CLIENT_ID_KEY);
    if (existing) return existing;
    const fresh = newUuid();
    window.localStorage.setItem(CLIENT_ID_KEY, fresh);
    return fresh;
  } catch {
    return newUuid();
  }
}

/** The Device descriptor this browser presents at login: a human label, the
 * "web" platform, and the stable clientId. */
export function browserDevice(): { name: string; platform: string; clientId: string } {
  return {
    name: browserName(),
    platform: "web",
    clientId: getClientId(),
  };
}

function newUuid(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  // Fallback for environments without crypto.randomUUID.
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    const v = c === "x" ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
}

function browserName(): string {
  // A friendly default Device name; the user has no UI to set one in this slice.
  if (typeof navigator !== "undefined" && navigator.userAgent) {
    if (/iPhone|iPad/.test(navigator.userAgent)) return "Web (iOS)";
    if (/Android/.test(navigator.userAgent)) return "Web (Android)";
    if (/Macintosh/.test(navigator.userAgent)) return "Web (Mac)";
    if (/Windows/.test(navigator.userAgent)) return "Web (Windows)";
  }
  return "Web Browser";
}
