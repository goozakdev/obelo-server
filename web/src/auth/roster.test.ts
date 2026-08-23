import { describe, it, expect, beforeEach } from "vitest";
import {
  demoteUser,
  forgetUser,
  getRosterEntry,
  loadRoster,
  rememberUser,
  rosterStorageKey,
  rosterUsers,
  syncKnownUsers,
} from "./roster";

// The pure roster store (appletv-parity/10): remembered Users per server, keyed by
// the Server identity id. Signed-in = a retained durable token; Known = none.

const SERVER = "srv-1";

function storage(): Storage {
  return window.localStorage;
}

beforeEach(() => {
  window.localStorage.clear();
});

describe("roster store", () => {
  it("keys the roster per server id", () => {
    expect(rosterStorageKey("srv-1")).toBe("obelo.roster.srv-1");
    // A server that advertises no id buckets under a stable fallback.
    expect(rosterStorageKey(null)).toBe("obelo.roster.unknown");
  });

  it("remembers a Remember-me login as a Signed-in entry (durable token retained)", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "admin" }, "tok-ana");
    const entry = getRosterEntry(storage(), SERVER, "u1");
    expect(entry).toMatchObject({ userId: "u1", username: "ana", token: "tok-ana" });
    expect(rosterUsers(storage(), SERVER)).toEqual([
      { userId: "u1", username: "ana", role: "admin", signedIn: true },
    ]);
  });

  it("remembers a Remember-me-OFF login as a Known entry (no token stored)", () => {
    rememberUser(storage(), SERVER, { id: "u2", username: "ben", role: "member" }, null);
    const entry = getRosterEntry(storage(), SERVER, "u2");
    expect(entry).toMatchObject({ userId: "u2", username: "ben" });
    expect(entry?.token).toBeUndefined();
    expect(rosterUsers(storage(), SERVER)[0].signedIn).toBe(false);
  });

  it("seeds Known entries for unknown server Users without clobbering a Signed-in token", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "admin" }, "tok-ana");
    syncKnownUsers(storage(), SERVER, [
      { id: "u1", username: "ana", role: "admin" }, // already present, untouched
      { id: "u3", username: "cy", role: "member" }, // new → Known
    ]);
    // u1 keeps its token (still Signed-in); u3 is added Known.
    expect(getRosterEntry(storage(), SERVER, "u1")?.token).toBe("tok-ana");
    const byId = Object.fromEntries(
      rosterUsers(storage(), SERVER).map((u) => [u.userId, u.signedIn]),
    );
    expect(byId).toEqual({ u1: true, u3: false });
  });

  it("prunes entries the server no longer knows, so a deleted User stops being a switch target", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "admin" }, "tok-ana");
    rememberUser(storage(), SERVER, { id: "u2", username: "ben", role: "member" }, "tok-ben");
    // ben was deleted server-side; the list is the authority on who exists.
    syncKnownUsers(storage(), SERVER, [{ id: "u1", username: "ana", role: "admin" }]);
    expect(rosterUsers(storage(), SERVER).map((u) => u.userId)).toEqual(["u1"]);
    expect(getRosterEntry(storage(), SERVER, "u2")).toBeNull();
    // Surviving entries keep their retained token — reconciling is not a demotion.
    expect(getRosterEntry(storage(), SERVER, "u1")?.token).toBe("tok-ana");
  });

  it("refreshes a surviving entry's username and role from the server list", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "member" }, "tok-ana");
    syncKnownUsers(storage(), SERVER, [{ id: "u1", username: "ana-renamed", role: "admin" }]);
    expect(rosterUsers(storage(), SERVER)).toEqual([
      { userId: "u1", username: "ana-renamed", role: "admin", signedIn: true },
    ]);
  });

  it("drops a dead twin sharing the username when that username signs in again", () => {
    // A deleted-then-recreated User: the stale entry carries the OLD id, so an
    // id-keyed upsert would leave two rows for one person — the dead one shadowing
    // the live one in the switch-user menu forever.
    rememberUser(storage(), SERVER, { id: "old-id", username: "ben", role: "member" }, null);
    rememberUser(storage(), SERVER, { id: "new-id", username: "ben", role: "member" }, "tok-ben");
    expect(rosterUsers(storage(), SERVER)).toEqual([
      { userId: "new-id", username: "ben", role: "member", signedIn: true },
    ]);
  });

  it("keeps distinct Users whose usernames differ only by case (the server's UNIQUE is exact)", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "Ben", role: "member" }, null);
    rememberUser(storage(), SERVER, { id: "u2", username: "ben", role: "member" }, null);
    expect(rosterUsers(storage(), SERVER).map((u) => u.userId).sort()).toEqual(["u1", "u2"]);
  });

  it("demotes a Signed-in entry to Known (drops the token, keeps the user)", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "admin" }, "tok-ana");
    demoteUser(storage(), SERVER, "u1");
    const entry = getRosterEntry(storage(), SERVER, "u1");
    expect(entry).toMatchObject({ userId: "u1", username: "ana" });
    expect(entry?.token).toBeUndefined();
  });

  it("forgets a User entirely", () => {
    rememberUser(storage(), SERVER, { id: "u1", username: "ana", role: "admin" }, "tok-ana");
    forgetUser(storage(), SERVER, "u1");
    expect(loadRoster(storage(), SERVER)).toEqual([]);
  });

  it("isolates rosters across servers", () => {
    rememberUser(storage(), "srv-A", { id: "u1", username: "ana", role: "admin" }, "tok");
    rememberUser(storage(), "srv-B", { id: "u9", username: "zed", role: "member" }, null);
    expect(rosterUsers(storage(), "srv-A").map((u) => u.userId)).toEqual(["u1"]);
    expect(rosterUsers(storage(), "srv-B").map((u) => u.userId)).toEqual(["u9"]);
  });

  it("degrades a corrupt roster to empty rather than throwing", () => {
    window.localStorage.setItem(rosterStorageKey(SERVER), "{not json");
    expect(loadRoster(storage(), SERVER)).toEqual([]);
  });
});
