import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";

// A redirect attach end to end in the browser (ADR-0063 decision 3), through the
// real API client: the profile starts the round trip as the signed-in User, the
// browser leaves for the provider, and a fresh page load at /sign-in/callback
// finishes it — carrying the same bearer token, to the attach callback rather
// than the sign-in one — and returns to the profile. Only the network is a stub.

const network = vi.hoisted(() => {
  const fetch = vi.fn();
  globalThis.fetch = fetch as unknown as typeof globalThis.fetch;
  return { fetch };
});

import ProfileScreen from "./ProfileScreen";
import SignInCallbackScreen from "./SignInCallbackScreen";

const identities = {
  identities: [{ provider: "directory", providerName: "Home directory", username: "bj" }],
  hasPassword: false,
  password: [{ id: "directory", name: "Home directory" }],
  redirect: [{ id: "oidc", name: "OpenID Connect" }],
};

interface Sent {
  path: string;
  method: string;
  authorization: string | null;
  body: unknown;
}

let sent: Sent[] = [];

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const assign = vi.fn();
const realLocation = window.location;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  sent = [];
  assign.mockReset();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...realLocation, assign },
  });
  network.fetch.mockReset();
  network.fetch.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    const headers = new Headers(init?.headers);
    sent.push({
      path,
      method: init?.method ?? "GET",
      authorization: headers.get("Authorization"),
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    });
    if (path === "/api/v1/auth/external-identities") return json(200, identities);
    if (path === "/api/v1/auth/redirect/attach/start") {
      return json(200, { url: "https://auth.example.test/authorize?state=st-1" });
    }
    if (path === "/api/v1/auth/redirect/attach/callback") {
      return json(200, { identity: { provider: "oidc", providerName: "OpenID Connect", username: "bj" } });
    }
    return json(200, {});
  });
});

afterEach(() => {
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

function app(path: string) {
  return renderWithAuth(
    <Routes>
      <Route path="/profile" element={<ProfileScreen />} />
      <Route path="/sign-in/callback" element={<SignInCallbackScreen />} />
    </Routes>,
    { initialEntries: [path], user: { id: "u-bj", username: "bj", role: "member" } },
  );
}

describe("a redirect attach, round trip", () => {
  it("carries the session's bearer token out and back, and attaches rather than signing in", async () => {
    window.sessionStorage.setItem("obelo.reauthGrant", JSON.stringify({ grant: "g-1", expiresAt: Date.now() + 60_000 }));

    // Out: the profile starts the attach as the signed-in User.
    const out = app("/profile");
    await userEvent.setup().click(await screen.findByTestId("profile-attach-redirect-oidc"));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://auth.example.test/authorize?state=st-1"));
    const start = sent.find((s) => s.path === "/api/v1/auth/redirect/attach/start");
    expect(start).toMatchObject({
      method: "POST",
      authorization: "Bearer fake-token",
      body: { provider: "oidc", reauthGrant: "g-1" },
    });
    out.unmount();

    // Back: a fresh page load where the provider sent the browser.
    sent = [];
    app("/sign-in/callback?state=st-1&code=code-1");
    expect(await screen.findByTestId("profile-screen")).toBeInTheDocument();
    const callback = sent.find((s) => s.path === "/api/v1/auth/redirect/attach/callback");
    expect(callback).toMatchObject({
      method: "POST",
      authorization: "Bearer fake-token",
      body: { state: "st-1", code: "code-1" },
    });
    expect(sent.some((s) => s.path === "/api/v1/auth/redirect/callback")).toBe(false);
    expect(window.sessionStorage.getItem("obelo.signIn.redirect")).toBeNull();
  });
});
