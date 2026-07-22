import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import AppHeader from "./AppHeader";
import { AuthProvider } from "../auth/session";
import { LibrariesProvider } from "./librariesContext";
import { rememberUser } from "../auth/roster";
import type { ApiClient } from "../api/client";

// AppHeader's switch-user surface (appletv-parity/10): the account menu lists the
// remembered-Users roster for the current server. A Signed-in entry switches
// instantly (adopting its retained token); a Known entry routes to /login with the
// username pre-filled. The retention/roster mechanics are covered in session.test;
// this proves the header wiring end-to-end.

// A stub client covering the surface AuthProvider touches on mount + switch.
function stubClient(): ApiClient {
  return {
    token: "fake-token",
    setToken: () => {},
    setTokenDurable: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
    logout: () => Promise.resolve(),
  } as unknown as ApiClient;
}

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="location">{`${loc.pathname}${loc.search}`}</div>;
}

function renderHeader() {
  // Seed an active Admin session (ana) and a two-entry roster under server srv-1:
  // ben is Signed-in (a retained token), cy is Known.
  window.localStorage.setItem("juicebox.token", "fake-token");
  window.localStorage.setItem(
    "juicebox.user",
    JSON.stringify({ id: "u-ana", username: "ana", role: "admin" }),
  );
  window.localStorage.setItem("juicebox.serverId", "srv-1");
  rememberUser(window.localStorage, "srv-1", { id: "u-ben", username: "ben", role: "member" }, "tok-ben");
  rememberUser(window.localStorage, "srv-1", { id: "u-cy", username: "cy", role: "member" }, null);

  return render(
    <MemoryRouter initialEntries={["/titles/x"]}>
      <AuthProvider client={stubClient()}>
        <LibrariesProvider>
          <AppHeader />
          <LocationProbe />
        </LibrariesProvider>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.restoreAllMocks();
});

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe("AppHeader switch-user roster", () => {
  it("lists the remembered Users with Signed-in vs Sign-in affordances", async () => {
    renderHeader();
    await userEvent.click(screen.getByTestId("user-menu-toggle"));

    const entries = screen.getAllByTestId("switch-user");
    const byId = Object.fromEntries(
      entries.map((e) => [e.getAttribute("data-user-id"), e.getAttribute("data-signed-in")]),
    );
    // ana (the active user) is not offered as a switch target.
    expect(byId).toEqual({ "u-ben": "true", "u-cy": "false" });
  });

  it("routes a Known entry to /login with the username pre-filled", async () => {
    renderHeader();
    await userEvent.click(screen.getByTestId("user-menu-toggle"));
    const cy = screen
      .getAllByTestId("switch-user")
      .find((e) => e.getAttribute("data-user-id") === "u-cy")!;
    await userEvent.click(cy);

    await waitFor(() =>
      expect(screen.getByTestId("location")).toHaveTextContent("/login?user=cy"),
    );
  });

  it("switches instantly to a Signed-in entry (adopts its token, lands on Home)", async () => {
    renderHeader();
    await userEvent.click(screen.getByTestId("user-menu-toggle"));
    const ben = screen
      .getAllByTestId("switch-user")
      .find((e) => e.getAttribute("data-user-id") === "u-ben")!;
    await userEvent.click(ben);

    // The active identity is now ben, and the app navigated to Home.
    await waitFor(() => expect(screen.getByTestId("current-user")).toHaveTextContent("ben"));
    expect(screen.getByTestId("location")).toHaveTextContent("/");
  });
});
