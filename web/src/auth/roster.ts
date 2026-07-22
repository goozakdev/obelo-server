import type { Role } from "../api/types";

// The per-Server remembered-Users roster (appletv-parity/10) — a lightweight
// switch-user surface bringing part of the TV's multi-user support to web.
//
// Keyed by the Server identity id (ADR-0034) so two servers reached from the same
// browser keep separate rosters. Each entry is a User this browser has seen. An
// entry that retained a DURABLE bearer token is "Signed-in" (an instant, auth-free
// switch); one without a token is "Known" (a switch routes through re-auth). NO
// password is ever stored — only the opaque bearer token, and only when Remember
// me was on. Admin seeding (GET /users) pre-populates Known entries.
//
// These are pure, storage-injected helpers (the provider passes `localStorage`;
// tests pass a fake/real Storage), so the round-trip is unit-testable without
// React — the same seam philosophy as the Queue's persist.ts / usePlaybackPrefs.

const STORAGE_PREFIX = "juicebox.roster";
const SERVER_ID_KEY = "juicebox.serverId";

/** A stored roster entry. The `token`, when present, is a retained DURABLE bearer
 * token that makes this a Signed-in entry; its absence makes it Known. */
export interface RosterEntry {
  userId: string;
  username: string;
  role: Role;
  /** A retained durable bearer token → Signed-in (instant, auth-free switch).
   * Absent → Known (re-auth). NEVER a password. */
  token?: string;
}

/** The sanitized view the UI consumes: the token is deliberately withheld from the
 * React tree (switchTo reads it from storage by id), so a token never rides in a
 * component prop or the DOM. */
export interface RosterUser {
  userId: string;
  username: string;
  role: Role;
  /** True when a durable token is retained → an instant, auth-free switch; false
   * for a Known entry that must re-authenticate. */
  signedIn: boolean;
}

/** The per-server storage key (a server with no advertised id buckets under
 * "unknown", so the roster still works against a pre-ADR-0034 server). */
export function rosterStorageKey(serverId: string | null): string {
  return `${STORAGE_PREFIX}.${serverId ?? "unknown"}`;
}

function isEntry(x: unknown): x is RosterEntry {
  if (!x || typeof x !== "object") return false;
  const e = x as RosterEntry;
  return (
    typeof e.userId === "string" &&
    typeof e.username === "string" &&
    typeof e.role === "string" &&
    (e.token === undefined || typeof e.token === "string")
  );
}

/** Load a server's roster, defensively. Any malformed payload degrades to an empty
 * roster rather than throwing — a corrupt roster must never break login. */
export function loadRoster(storage: Storage, serverId: string | null): RosterEntry[] {
  try {
    const raw = storage.getItem(rosterStorageKey(serverId));
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(isEntry);
  } catch {
    return [];
  }
}

/** Persist a server's roster. An empty roster removes the key. Storage failures
 * (quota/private mode) are swallowed: the switch-user surface simply won't persist. */
export function saveRoster(
  storage: Storage,
  serverId: string | null,
  entries: RosterEntry[],
): void {
  try {
    if (entries.length === 0) storage.removeItem(rosterStorageKey(serverId));
    else storage.setItem(rosterStorageKey(serverId), JSON.stringify(entries));
  } catch {
    // ignore — persistence is best-effort.
  }
}

/** The minimal identity a login/seed carries. */
export interface RosterIdentity {
  id: string;
  username: string;
  role: Role;
}

/** Remember a User who just signed in. `token` is the retained durable token when
 * Remember me was on (→ Signed-in), or `null` to record a Known entry (Remember me
 * off, or an explicit demotion). The username/role are refreshed either way. */
export function rememberUser(
  storage: Storage,
  serverId: string | null,
  user: RosterIdentity,
  token: string | null,
): void {
  const entries = loadRoster(storage, serverId);
  const next: RosterEntry = {
    userId: user.id,
    username: user.username,
    role: user.role,
    ...(token ? { token } : {}),
  };
  const idx = entries.findIndex((e) => e.userId === user.id);
  if (idx >= 0) entries[idx] = next;
  else entries.push(next);
  saveRoster(storage, serverId, entries);
}

/** Add Known entries for server Users not already remembered (Admin seeding via
 * GET /users). Never touches an existing entry, so it can neither clobber a
 * Signed-in entry's token nor demote a remembered user. */
export function seedKnownUsers(
  storage: Storage,
  serverId: string | null,
  users: RosterIdentity[],
): void {
  const entries = loadRoster(storage, serverId);
  const known = new Set(entries.map((e) => e.userId));
  let changed = false;
  for (const u of users) {
    if (known.has(u.id)) continue;
    entries.push({ userId: u.id, username: u.username, role: u.role });
    known.add(u.id);
    changed = true;
  }
  if (changed) saveRoster(storage, serverId, entries);
}

/** Demote a Signed-in entry to Known by dropping its retained token, keeping the
 * user remembered. Used on sign-out and when an adopted token turns out dead. */
export function demoteUser(
  storage: Storage,
  serverId: string | null,
  userId: string,
): void {
  const entries = loadRoster(storage, serverId);
  const idx = entries.findIndex((e) => e.userId === userId);
  if (idx < 0 || entries[idx].token === undefined) return;
  const { token: _drop, ...rest } = entries[idx];
  void _drop;
  entries[idx] = rest;
  saveRoster(storage, serverId, entries);
}

/** Forget a User entirely (remove from the roster). */
export function forgetUser(
  storage: Storage,
  serverId: string | null,
  userId: string,
): void {
  const entries = loadRoster(storage, serverId);
  const next = entries.filter((e) => e.userId !== userId);
  if (next.length !== entries.length) saveRoster(storage, serverId, next);
}

/** The full stored entry for a User (carries the token) — used by switchTo to
 * adopt a Signed-in entry's retained token. Null when not remembered. */
export function getRosterEntry(
  storage: Storage,
  serverId: string | null,
  userId: string,
): RosterEntry | null {
  return loadRoster(storage, serverId).find((e) => e.userId === userId) ?? null;
}

/** The sanitized, token-free roster the switch-user UI renders, sorted by username. */
export function rosterUsers(storage: Storage, serverId: string | null): RosterUser[] {
  return loadRoster(storage, serverId)
    .map((e) => ({
      userId: e.userId,
      username: e.username,
      role: e.role,
      signedIn: e.token !== undefined,
    }))
    .sort((a, b) => a.username.localeCompare(b.username));
}

// --- Server identity id (ADR-0034), the roster's key ------------------------
//
// Persisted app-wide so the auth scope can key the roster synchronously without a
// handshake round-trip. Written when the handshake resolves; read on mount.

/** The last-seen Server identity id, or null. */
export function loadServerId(): string | null {
  try {
    const id = window.localStorage.getItem(SERVER_ID_KEY);
    return id && id.length > 0 ? id : null;
  } catch {
    return null;
  }
}

/** Record the Server identity id (from the `GET /server` handshake). A null/blank
 * id (a pre-ADR-0034 server) is ignored so a prior good id isn't wiped. */
export function saveServerId(id: string | null | undefined): void {
  if (!id) return;
  try {
    window.localStorage.setItem(SERVER_ID_KEY, id);
  } catch {
    // ignore — best-effort.
  }
}
