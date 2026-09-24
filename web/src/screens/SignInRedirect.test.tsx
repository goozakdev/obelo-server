import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import { ApiError, type ApiClient } from "../api/client";

// The redirect flow of a Sign-in provider on the web (ADR-0063 decisions 2 and
// 8): the login screen's buttons, and the callback screen the provider sends the
// browser back to. The server owns the round trip; these screens only carry what
// they were handed to it and show its answer.

const calls = vi.hoisted(() => ({
  list: vi.fn(),
  start: vi.fn(),
  complete: vi.fn(),
  login: vi.fn(),
}));

function stubClient(): ApiClient {
  return {
    token: null,
    setToken: () => {},
    setTokenDurable: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
    serverInfo: () =>
      Promise.resolve({ version: "test", supportedVersions: [1], features: {}, setupRequired: false }),
    login: (...a: unknown[]) => calls.login(...a),
    listRedirectSignInProviders: (...a: unknown[]) => calls.list(...a),
    startRedirectSignIn: (...a: unknown[]) => calls.start(...a),
    completeRedirectSignIn: (...a: unknown[]) => calls.complete(...a),
  } as unknown as ApiClient;
}

import LoginScreen from "./LoginScreen";
import SignInCallbackScreen from "./SignInCallbackScreen";

function renderAt(path: string, state?: unknown) {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <MemoryRouter initialEntries={[{ pathname: path.split("?")[0], search: path.includes("?") ? path.slice(path.indexOf("?")) : "", state }]}>
        <AuthProvider client={stubClient()}>{children}</AuthProvider>
      </MemoryRouter>
    );
  }
  return render(
    <Routes>
      <Route path="/login" element={<LoginScreen />} />
      <Route path="/sign-in/callback" element={<SignInCallbackScreen />} />
      <Route path="/" element={<div data-testid="home" />} />
      <Route path="/link/:code" element={<div data-testid="link" />} />
    </Routes>,
    { wrapper: Wrapper },
  );
}

const signedIn = {
  token: "t",
  user: { id: "u-1", username: "ada", role: "member" },
  device: { id: "d-1", name: "Browser", platform: "web", clientId: "c" },
};

const assign = vi.fn();
const realLocation = window.location;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  for (const fn of Object.values(calls)) fn.mockReset();
  assign.mockReset();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...realLocation, assign },
  });
});

afterEach(() => {
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

describe("the login screen's redirect sign-in", () => {
  it("offers no button when the server has no redirect provider", async () => {
    calls.list.mockResolvedValue({ providers: [] });
    renderAt("/login");
    await screen.findByTestId("login-submit");
    await Promise.resolve();
    expect(calls.list).toHaveBeenCalled();
    expect(screen.queryByTestId("login-redirect-oidc")).not.toBeInTheDocument();
  });

  it("sends the browser to the URL the server started", async () => {
    calls.list.mockResolvedValue({ providers: [{ id: "oidc", name: "OpenID Connect" }] });
    calls.start.mockResolvedValue("https://auth.example.test/authorize?state=st");
    renderAt("/login");

    const button = await screen.findByTestId("login-redirect-oidc");
    expect(button).toHaveTextContent("Sign in with OpenID Connect");
    await userEvent.setup().click(button);

    expect(calls.start).toHaveBeenCalledWith("oidc");
    expect(assign).toHaveBeenCalledWith("https://auth.example.test/authorize?state=st");
    expect(JSON.parse(window.sessionStorage.getItem("obelo.signIn.redirect") ?? "{}")).toEqual({
      from: "/",
      remember: true,
    });
  });
});

describe("the sign-in callback screen", () => {
  it("posts the state and code and returns to where the login was headed", async () => {
    window.sessionStorage.setItem("obelo.signIn.redirect", JSON.stringify({ from: "/link/ABCD", remember: false }));
    calls.complete.mockResolvedValue({
      token: "t",
      user: { id: "u-1", username: "ada", role: "member" },
      device: { id: "d-1", name: "Browser", platform: "web", clientId: "c" },
    });
    renderAt("/sign-in/callback?state=st-1&code=code-1");

    expect(await screen.findByTestId("link")).toBeInTheDocument();
    expect(calls.complete).toHaveBeenCalledTimes(1);
    const [req] = calls.complete.mock.calls[0] as [{ state: string; code: string; device: { clientId: string } }];
    expect(req.state).toBe("st-1");
    expect(req.code).toBe("code-1");
    expect(req.device.clientId).toBeTruthy();
  });

  it("shows the server's refusal and links back to the login screen", async () => {
    calls.complete.mockRejectedValue(
      new ApiError(401, "SIGN_IN_REFUSED", "the sign-in could not be completed; try again"),
    );
    renderAt("/sign-in/callback?state=st-1&code=code-1");

    expect(await screen.findByTestId("sign-in-callback-error")).toHaveTextContent(
      "the sign-in could not be completed; try again",
    );
    expect(screen.getByTestId("sign-in-callback-back")).toHaveAttribute("href", "/login");
    expect(screen.queryByTestId("home")).not.toBeInTheDocument();
  });

  it.each(["//evil.example", "/\\evil.example"])("returns Home rather than to %s", async (from) => {
    window.sessionStorage.setItem("obelo.signIn.redirect", JSON.stringify({ from, remember: true }));
    calls.complete.mockResolvedValue(signedIn);
    renderAt("/sign-in/callback?state=st-1&code=code-1");

    expect(await screen.findByTestId("home")).toBeInTheDocument();
  });

  it("posts nothing when the provider sent the browser back with an error", async () => {
    renderAt("/sign-in/callback?error=access_denied&state=st-1");

    expect(await screen.findByTestId("sign-in-callback-error")).toBeInTheDocument();
    expect(calls.complete).not.toHaveBeenCalled();
  });
});

describe("the login screen's password sign-in", () => {
  it.each(["//evil.example", "/\\evil.example"])("returns Home rather than to %s", async (from) => {
    calls.list.mockResolvedValue({ providers: [] });
    calls.login.mockResolvedValue(signedIn);
    renderAt("/login", { from: { pathname: from } });

    const user = userEvent.setup();
    await user.type(await screen.findByTestId("login-username"), "ada");
    await user.type(screen.getByTestId("login-password"), "pw");
    await user.click(screen.getByTestId("login-submit"));

    expect(await screen.findByTestId("home")).toBeInTheDocument();
  });
});
