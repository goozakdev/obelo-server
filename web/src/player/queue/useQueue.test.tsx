import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider, useAuth } from "../../auth/session";
import type { ApiClient } from "../../api/client";
import type { TitleSummary } from "../../api/types";
import { QueueProvider, useQueue } from "./useQueue";
import { entryFromTitle } from "./model";
import { loadQueue } from "./persist";

// The QueueProvider WIRING (the pure model/persistence are covered by model.test
// / persist.test): the Queue hydrates from sessionStorage on mount scoped to the
// logged-in user (reload survival + scoping), and is cleared from memory AND
// storage on logout.

function title(id: string): TitleSummary {
  return {
    id,
    kind: "movie",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

// A stub client satisfying the surface AuthProvider touches on mount + logout.
function authStub(): ApiClient {
  return {
    token: "fake-token",
    setToken: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
    logout: () => Promise.resolve(),
  } as unknown as ApiClient;
}

function Harness() {
  const queue = useQueue();
  const { logout } = useAuth();
  return (
    <div>
      <div data-testid="current">{queue.current?.title.id ?? "none"}</div>
      <div data-testid="length">{queue.length}</div>
      <div data-testid="shuffle">{queue.shuffle ? "on" : "off"}</div>
      <div data-testid="repeat">{queue.repeat}</div>
      <button
        data-testid="play"
        onClick={() => queue.playNow([entryFromTitle(title("a")), entryFromTitle(title("b"))], 0)}
      >
        play
      </button>
      <button data-testid="shuffle-toggle" onClick={() => queue.setShuffle(!queue.shuffle)}>
        shuffle
      </button>
      <button data-testid="repeat-cycle" onClick={() => queue.cycleRepeat()}>
        repeat
      </button>
      <button data-testid="advance" onClick={() => queue.advance()}>
        advance
      </button>
      <button data-testid="logout" onClick={() => void logout()}>
        logout
      </button>
    </div>
  );
}

function renderProvider() {
  return render(
    <MemoryRouter>
      <AuthProvider client={authStub()}>
        <QueueProvider>
          <Harness />
        </QueueProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  // Seed a logged-in user (u1) the way the app persists it.
  window.localStorage.setItem("juicebox.token", "fake-token");
  window.localStorage.setItem(
    "juicebox.user",
    JSON.stringify({ id: "u1", username: "operator", role: "admin" }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe("QueueProvider", () => {
  it("persists the Queue to sessionStorage and restores it on a fresh mount (reload survival, scoped to the user)", async () => {
    const first = renderProvider();
    await act(async () => {
      screen.getByTestId("play").click();
    });
    expect(screen.getByTestId("current")).toHaveTextContent("a");
    // The Queue was persisted under the logged-in user's key.
    expect(loadQueue(window.sessionStorage, "u1").entries.map((e) => e.title.id)).toEqual([
      "a",
      "b",
    ]);
    expect(loadQueue(window.sessionStorage, "u2").entries).toEqual([]); // scoped per user
    first.unmount();

    // A fresh mount (a page reload in the same browser session) restores it.
    renderProvider();
    expect(await screen.findByTestId("current")).toHaveTextContent("a");
    expect(screen.getByTestId("length")).toHaveTextContent("2");
  });

  it("drives Shuffle mode, Repeat mode, and the natural-end advance through the store", async () => {
    renderProvider();
    await act(async () => {
      screen.getByTestId("play").click(); // [a (current), b]
    });
    expect(screen.getByTestId("shuffle")).toHaveTextContent("off");
    expect(screen.getByTestId("repeat")).toHaveTextContent("off");

    // setShuffle drives the derived `shuffle` flag (non-null authored snapshot).
    await act(async () => {
      screen.getByTestId("shuffle-toggle").click();
    });
    expect(screen.getByTestId("shuffle")).toHaveTextContent("on");
    await act(async () => {
      screen.getByTestId("shuffle-toggle").click();
    });
    expect(screen.getByTestId("shuffle")).toHaveTextContent("off");

    // cycleRepeat walks off → all → one → off.
    await act(async () => {
      screen.getByTestId("repeat-cycle").click();
    });
    expect(screen.getByTestId("repeat")).toHaveTextContent("all");
    await act(async () => {
      screen.getByTestId("repeat-cycle").click();
    });
    expect(screen.getByTestId("repeat")).toHaveTextContent("one");

    // advance under repeat-one stays put (the same entry replays); switching it off
    // then advancing moves to the next entry.
    await act(async () => {
      screen.getByTestId("advance").click();
    });
    expect(screen.getByTestId("current")).toHaveTextContent("a"); // repeat-one → no move
    await act(async () => {
      screen.getByTestId("repeat-cycle").click(); // one → off
      screen.getByTestId("advance").click();
    });
    expect(screen.getByTestId("current")).toHaveTextContent("b"); // advanced
  });

  it("clears the Queue (memory + storage) on logout", async () => {
    renderProvider();
    await act(async () => {
      screen.getByTestId("play").click();
    });
    expect(screen.getByTestId("current")).toHaveTextContent("a");

    await act(async () => {
      screen.getByTestId("logout").click();
    });

    expect(screen.getByTestId("current")).toHaveTextContent("none");
    expect(screen.getByTestId("length")).toHaveTextContent("0");
    expect(loadQueue(window.sessionStorage, "u1").entries).toEqual([]);
  });
});
