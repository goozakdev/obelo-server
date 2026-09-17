// Vitest setup: jest-dom matchers + a couple of jsdom shims the browse UI uses.
import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

// Give the tests back JSDOM's localStorage/sessionStorage, which a modern Node
// steals from them.
//
// Node itself now ships experimental `localStorage`/`sessionStorage` globals
// (on by default from Node 24; this machine is on 26). Vitest's jsdom
// environment installs a jsdom global only for a key it does NOT already find
// on the Node global — `getWindowKeys()` in vitest/dist/chunks: `if (k in
// global) return KEYS.includes(k)` — and neither storage is in that hardcoded
// KEYS list. So on such a Node, Node's globals win and jsdom's are never
// installed. `window` IS globalThis under vitest, so `window.localStorage`
// resolves to Node's too and there is nowhere left to get the real one.
//
// The damage: Node's `localStorage` throws away its contents unless the process
// was started with --localstorage-file, and evaluates to `undefined` without it
// (that is the "ExperimentalWarning: localStorage is not available because
// --localstorage-file was not provided" on every run). 595 of 1234 tests died
// on `localStorage.clear()` with "Cannot read properties of undefined". Node's
// `sessionStorage` is worse because it is quieter: it exists, so those tests
// passed, but it is one process-wide store rather than the per-test-file one a
// fresh JSDOM gives us, so state leaks between files sharing a worker.
//
// --localstorage-file is not a fix: one backing file shared by parallel vitest
// workers makes them collide (7 failures without --no-file-parallelism), and it
// would still leave sessionStorage as Node's.
//
// So take both storages straight off the JSDOM instance vitest hands us as
// `globalThis.jsdom` and pin them onto the global. On a Node without these
// globals (22, or 24 with them off) jsdom's are what is already there and this
// re-pins the same objects — a no-op, which is the point: the suite behaves
// identically whatever Node a contributor has.
//
// Note it never READS globalThis.localStorage to decide whether it needs to act.
// Reading it is what invokes Node's getter and prints that ExperimentalWarning,
// so an unconditional define leaves the run clean and nobody has to wonder
// whether the warning means this is still broken.
const jsdomWindow = (globalThis as { jsdom?: { window: Window } }).jsdom?.window;
if (jsdomWindow) {
  for (const key of ["localStorage", "sessionStorage"] as const) {
    Object.defineProperty(globalThis, key, {
      value: jsdomWindow[key],
      configurable: true,
      writable: true,
    });
  }
} else if (!globalThis.localStorage || !globalThis.sessionStorage) {
  // Fail loudly here rather than as hundreds of unreadable TypeErrors later:
  // storage is shadowed or missing and vitest no longer hands us the JSDOM
  // instance as `globalThis.jsdom` to repair it from.
  throw new Error(
    "Test setup: no usable localStorage/sessionStorage, and no globalThis.jsdom " +
      "to restore jsdom's from. See the comment above — a Node-provided storage " +
      "global has almost certainly shadowed jsdom's.",
  );
}

// IntersectionObserver isn't implemented in jsdom; the grid uses it for the
// infinite-scroll sentinel. A no-op stub lets the component mount; tests drive
// pagination by calling the exposed loadMore directly (the hook is tested as a
// unit) or by clicking a fallback, so we don't need it to actually fire.
class MockIntersectionObserver implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = "";
  readonly thresholds = [];
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}
vi.stubGlobal("IntersectionObserver", MockIntersectionObserver);

// jsdom implements neither the Fullscreen API on Element nor document.exitFullscreen.
// The Now Playing bar's immersive stage takes the video fullscreen by calling
// requestFullscreen on the STAGE WRAPPER element (not the bare <video>). Provide
// prototype-level no-ops so components can call them and tests can spy on the
// target (same minimal, generic style as the IntersectionObserver stub above).
if (!("requestFullscreen" in Element.prototype)) {
  Element.prototype.requestFullscreen = function () {
    return Promise.resolve();
  };
}
if (!("exitFullscreen" in Document.prototype)) {
  Document.prototype.exitFullscreen = function () {
    return Promise.resolve();
  };
}

// jsdom implements <dialog> markup but not showModal()/close() (nor the `open`
// reflection they drive). The Edit-item dialog opens itself imperatively via
// showModal(), so provide prototype-level shims that flip `open` — enough for
// component tests to open the dialog and interact with the active tab (same minimal,
// generic style as the stubs above).
if (!HTMLDialogElement.prototype.showModal) {
  HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
  };
}
if (!HTMLDialogElement.prototype.close) {
  HTMLDialogElement.prototype.close = function () {
    this.open = false;
    this.dispatchEvent(new Event("close"));
  };
}

// Unmount React trees between tests so each test gets a clean DOM.
afterEach(() => {
  cleanup();
});
