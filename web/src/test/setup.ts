// Vitest setup: jest-dom matchers + a couple of jsdom shims the browse UI uses.
import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

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
