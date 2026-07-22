import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, act, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider, useAuth } from "./session";
import { ApiClient } from "../api/client";
import { ServerInfoStateProvider } from "../serverInfoContext";
import type { ServerState } from "../useServerInfo";
import { QueueProvider, useQueue } from "../player/queue/useQueue";
import { entryFromTitle } from "../player/queue/model";
import type { TitleSummary } from "../api/types";

// The AuthProvider WIRING for Remember me + the remembered-Users roster
// (appletv-parity/10), driven through a REAL ApiClient (so the real webTokenStore
// makes the retention assertions genuine) with an injected fetch. The pure token
// / roster stores are covered by token.test / roster.test; this proves the
// end-to-end behaviour: retention tier on login, instant auth-free switch for a
// Signed-in entry, playback teardown + client-state re-key across a switch, and
// Admin GET /users seeding.

const TOKEN_KEY = "juicebox.token";

function title(id: string): TitleSummary {
  return {
    id,
    kind: "music",
    title: id,
    year: 0,
    needsReview: false,
    ambiguous: false,
    resumePositionMs: 0,
    watched: false,
    genres: [],
  };
}

interface Counts {
  login: number;
  /** POST /auth/media-cookie hits — the re-issue on an instant switch
   * (appletv-parity/12). Optional so the older tests need not thread it. */
  mediaCookie?: number;
}

function makeFetch(counts: Counts): typeof fetch {
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : (input as Request).url ?? String(input);
    if (url.endsWith("/api/v1/server")) {
      return json({
        id: "srv-1",
        name: "Test",
        version: "t",
        supportedVersions: [1],
        features: {},
        setupRequired: false,
      });
    }
    if (url.endsWith("/api/v1/auth/login")) {
      counts.login++;
      const body = JSON.parse(String(init?.body ?? "{}")) as { username: string };
      const username = body.username;
      return json({
        token: `tok-${username}`,
        user: { id: `u-${username}`, username, role: username === "ana" ? "admin" : "member" },
        device: { id: "d", name: "n", platform: "web", clientId: "c" },
      });
    }
    if (url.endsWith("/api/v1/auth/media-cookie")) {
      counts.mediaCookie = (counts.mediaCookie ?? 0) + 1;
      return new Response(null, { status: 204 });
    }
    if (url.endsWith("/api/v1/auth/logout")) return new Response(null, { status: 204 });
    if (url.endsWith("/api/v1/devices")) return json({ devices: [] });
    if (url.endsWith("/api/v1/users")) {
      return json({
        users: [
          { id: "u-ana", username: "ana", role: "admin" },
          { id: "u-ben", username: "ben", role: "member" },
          { id: "u-cy", username: "cy", role: "member" },
        ],
      });
    }
    return json({});
  }) as unknown as typeof fetch;
}

function Harness() {
  const { session, login, switchTo, roster } = useAuth();
  const queue = useQueue();
  return (
    <div>
      <div data-testid="user">{session?.user.username ?? "none"}</div>
      <div data-testid="queue-current">{queue.current?.title.id ?? "none"}</div>
      <div data-testid="roster">
        {roster.map((u) => `${u.username}:${u.signedIn ? "in" : "known"}`).join(",")}
      </div>
      <button data-testid="login-ana-remember" onClick={() => void login("ana", "pw", true)}>
        x
      </button>
      <button data-testid="login-ana-nosave" onClick={() => void login("ana", "pw", false)}>
        x
      </button>
      <button data-testid="login-ben-remember" onClick={() => void login("ben", "pw", true)}>
        x
      </button>
      <button
        data-testid="play"
        onClick={() => queue.playNow([entryFromTitle(title("song-a"))], 0)}
      >
        x
      </button>
      <button data-testid="switch-ben" onClick={() => void switchTo("u-ben")}>
        x
      </button>
    </div>
  );
}

function renderApp(client: ApiClient) {
  return render(
    <MemoryRouter>
      <AuthProvider client={client}>
        <QueueProvider>
          <Harness />
        </QueueProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

// Like renderApp, but wraps AuthProvider in a ServerInfoStateProvider carrying a
// ready handshake with the given feature flags — the seam the media-cookie
// re-issue (appletv-parity/12) gates on. The default renderApp mounts AuthProvider
// bare (no provider), which is itself the "server too old to advertise the flag"
// case: useOptionalFeature reads false and the switch skips the refresh.
function renderAppWithFeatures(client: ApiClient, features: Record<string, boolean>) {
  const state: ServerState = {
    status: "ready",
    info: {
      id: "srv-1",
      name: "Test",
      version: "t",
      supportedVersions: [1],
      features,
      setupRequired: false,
    },
  };
  return render(
    <MemoryRouter>
      <ServerInfoStateProvider state={state}>
        <AuthProvider client={client}>
          <QueueProvider>
            <Harness />
          </QueueProvider>
        </AuthProvider>
      </ServerInfoStateProvider>
    </MemoryRouter>,
  );
}

async function click(testid: string) {
  await act(async () => {
    screen.getByTestId(testid).click();
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe("AuthProvider — Remember me retention", () => {
  it("keeps the token session-only (NOT in localStorage) when Remember me is off", async () => {
    const client = new ApiClient({ fetchImpl: makeFetch({ login: 0 }) });
    renderApp(client);
    await click("login-ana-nosave");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    // The core guarantee: no durable token, only a session-only one.
    expect(window.localStorage.getItem(TOKEN_KEY)).toBeNull();
    expect(window.sessionStorage.getItem(TOKEN_KEY)).toBe("tok-ana");
    // The user hint follows the same tier (no durable user hint either).
    expect(window.localStorage.getItem("juicebox.user")).toBeNull();
    expect(window.sessionStorage.getItem("juicebox.user")).not.toBeNull();
  });

  it("persists the token to localStorage when Remember me is on", async () => {
    const client = new ApiClient({ fetchImpl: makeFetch({ login: 0 }) });
    renderApp(client);
    await click("login-ana-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    expect(window.localStorage.getItem(TOKEN_KEY)).toBe("tok-ana");
    expect(window.sessionStorage.getItem(TOKEN_KEY)).toBeNull();
  });
});

describe("AuthProvider — roster switch-user", () => {
  it("switches instantly and auth-free to a Signed-in entry, tearing down playback and re-keying the queue", async () => {
    const counts = { login: 0 };
    const client = new ApiClient({ fetchImpl: makeFetch(counts) });
    renderApp(client);

    // ben signs in (Remember me → a Signed-in roster entry), then ana becomes active.
    await click("login-ben-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));
    await click("login-ana-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    // ana starts playback → the queue holds ana's Title.
    await click("play");
    expect(screen.getByTestId("queue-current")).toHaveTextContent("song-a");

    const loginsBefore = counts.login; // ben + ana = 2

    // Switch to ben's Signed-in entry: instant, no re-auth.
    await click("switch-ben");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));

    // No new /auth/login call — the switch adopted the retained token.
    expect(counts.login).toBe(loginsBefore);
    // Hard teardown: ana's active playback is gone and the queue re-keyed to ben
    // (his empty queue), not ana's — no cross-user leak.
    expect(screen.getByTestId("queue-current")).toHaveTextContent("none");
    // The active token is now ben's, retained durably.
    expect(window.localStorage.getItem(TOKEN_KEY)).toBe("tok-ben");
  });

  it("re-issues the media cookie on an instant switch when the flag is on (appletv-parity/12)", async () => {
    const counts: Counts = { login: 0, mediaCookie: 0 };
    const client = new ApiClient({ fetchImpl: makeFetch(counts) });
    renderAppWithFeatures(client, { mediaCookieRefresh: true });

    // ben signs in (→ a Signed-in roster entry), then ana becomes active.
    await click("login-ben-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));
    await click("login-ana-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    // Login must NOT hit the re-issue endpoint (the server sets the cookie inline).
    expect(counts.mediaCookie).toBe(0);

    // Instant switch to ben: the HttpOnly media cookie can't be swapped from JS, so
    // the switch calls POST /auth/media-cookie to flip byte-serving to ben.
    await click("switch-ben");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));
    await waitFor(() => expect(counts.mediaCookie).toBe(1));
  });

  it("does NOT re-issue the media cookie (and does not throw) when the flag is absent", async () => {
    const counts: Counts = { login: 0, mediaCookie: 0 };
    const client = new ApiClient({ fetchImpl: makeFetch(counts) });
    // Server advertises no flags → the switch must degrade gracefully.
    renderAppWithFeatures(client, {});

    await click("login-ben-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));
    await click("login-ana-remember");
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    await click("switch-ben");
    // The switch itself still works…
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ben"));
    // …but with no flag the re-issue endpoint is never called.
    expect(counts.mediaCookie).toBe(0);
  });

  it("seeds Known entries from GET /users for an Admin", async () => {
    const client = new ApiClient({ fetchImpl: makeFetch({ login: 0 }) });
    renderApp(client);
    await click("login-ana-remember"); // ana is an Admin
    await waitFor(() => expect(screen.getByTestId("user")).toHaveTextContent("ana"));

    // Seeding is best-effort/async; the other server Users appear as Known switch
    // targets (ana herself is excluded as the active user).
    await waitFor(() => {
      const roster = screen.getByTestId("roster").textContent ?? "";
      expect(roster).toContain("ben:known");
      expect(roster).toContain("cy:known");
    });
  });
});
