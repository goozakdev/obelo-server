// The re-auth grant a User without a Local password earns by confirming it is
// them (ADR-0063 decision 3), kept in sessionStorage for the profile's next
// attach — across a redirect round trip too. It is good once, briefly, and only
// for the User and session that confirmed, so it is read fresh whenever it is
// used, and the session forgets it on sign-out and on a switch to another User.

const REAUTH_GRANT_KEY = "obelo.reauthGrant";

/** Keep a re-auth grant for the profile's next attach, until it expires. */
export function keepReauthGrant(grant: string, expiresIn: number): void {
  try {
    window.sessionStorage.setItem(REAUTH_GRANT_KEY, JSON.stringify({ grant, expiresAt: Date.now() + expiresIn * 1000 }));
  } catch {
    // Storage unavailable: the profile asks the User to confirm again.
  }
}

/** The kept re-auth grant, if it is still live. */
export function readReauthGrant(): string | null {
  return readLiveGrant()?.grant ?? null;
}

/** When the kept re-auth grant expires, in Date.now() milliseconds, if it is
 * still live. */
export function reauthGrantExpiresAt(): number | null {
  return readLiveGrant()?.expiresAt ?? null;
}

function readLiveGrant(): { grant: string; expiresAt: number } | null {
  try {
    const raw = window.sessionStorage.getItem(REAUTH_GRANT_KEY);
    if (raw) {
      const g = JSON.parse(raw) as { grant?: unknown; expiresAt?: unknown };
      if (typeof g.grant === "string" && typeof g.expiresAt === "number" && g.expiresAt > Date.now()) {
        return { grant: g.grant, expiresAt: g.expiresAt };
      }
    }
  } catch {
    /* fall through */
  }
  return null;
}

/** Forget the kept re-auth grant: it is spent once an attach presents it, and
 * nobody else's once the session changes hands. */
export function forgetReauthGrant(): void {
  try {
    window.sessionStorage.removeItem(REAUTH_GRANT_KEY);
  } catch {
    /* nothing kept */
  }
}
