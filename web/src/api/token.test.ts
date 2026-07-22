import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { webTokenStore } from "./token";

// The web token store is the retention seam for "Remember me" (appletv-parity/10).
// It persists the bearer token in EITHER localStorage (durable — survives a tab
// close) or sessionStorage (session-only — gone when the tab closes). NO password
// is ever stored, only the opaque token. These tests pin the retention behaviour
// at the storage seam without a DOM/React.

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

const KEY = "juicebox.token";

describe("webTokenStore retention", () => {
  it("defaults to durable: a set token lands in localStorage (Remember me on)", () => {
    const store = webTokenStore();
    store.set("tok-1");
    expect(window.localStorage.getItem(KEY)).toBe("tok-1");
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
    expect(store.get()).toBe("tok-1");
    expect(store.isDurable()).toBe(true);
  });

  it("session-only: setDurable(false) writes to sessionStorage and NOT localStorage", () => {
    const store = webTokenStore();
    store.setDurable(false);
    store.set("tok-2");
    // The core Remember-me-off guarantee: no token in localStorage.
    expect(window.localStorage.getItem(KEY)).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBe("tok-2");
    expect(store.get()).toBe("tok-2");
    expect(store.isDurable()).toBe(false);
  });

  it("re-homes the current token when the retention choice flips", () => {
    const store = webTokenStore();
    store.set("tok-3"); // durable
    store.setDurable(false); // → session-only
    expect(window.localStorage.getItem(KEY)).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBe("tok-3");
    store.setDurable(true); // → durable again
    expect(window.localStorage.getItem(KEY)).toBe("tok-3");
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
  });

  it("clearing (null) removes the token from BOTH tiers", () => {
    const store = webTokenStore();
    store.set("tok-4");
    store.setDurable(false);
    store.set("tok-5");
    store.set(null);
    expect(window.localStorage.getItem(KEY)).toBeNull();
    expect(window.sessionStorage.getItem(KEY)).toBeNull();
    expect(store.get()).toBeNull();
  });

  it("reads a durably-restored token (localStorage wins over an empty session)", () => {
    window.localStorage.setItem(KEY, "restored");
    const store = webTokenStore();
    expect(store.get()).toBe("restored");
    expect(store.isDurable()).toBe(true);
  });

  it("reads a session-only restored token when only sessionStorage holds it", () => {
    window.sessionStorage.setItem(KEY, "sess-restored");
    const store = webTokenStore();
    expect(store.get()).toBe("sess-restored");
    expect(store.isDurable()).toBe(false);
  });
});
