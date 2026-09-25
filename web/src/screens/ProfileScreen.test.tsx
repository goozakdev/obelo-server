import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { AuthProvider, useAuth } from "../auth/session";
import { rememberUser } from "../auth/roster";
import { ApiError, type ApiClient } from "../api/client";

// Attaching an External identity from one's own profile (ADR-0063 decision 3):
// the profile lists what the caller holds and offers each Sign-in provider's
// flow; the callback screen finishes a redirect attach instead of a sign-in when
// the profile started it. The server owns every judgment; these screens carry
// what they were handed and show its answer.
//
// A session alone never attaches: a User with a Local password types it into the
// attach; a User without one first confirms it is them through a sign-in they
// already hold, and the grant that answers goes with the attach, once.

const api = vi.hoisted(() => ({
  list: vi.fn(),
  attachPassword: vi.fn(),
  startAttach: vi.fn(),
  completeAttach: vi.fn(),
  reauthPassword: vi.fn(),
  startReauth: vi.fn(),
  completeReauth: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listExternalIdentities: (...a: unknown[]) => api.list(...a),
      attachPasswordIdentity: (...a: unknown[]) => api.attachPassword(...a),
      startAttachRedirect: (...a: unknown[]) => api.startAttach(...a),
      completeAttachRedirect: (...a: unknown[]) => api.completeAttach(...a),
      reauthPassword: (...a: unknown[]) => api.reauthPassword(...a),
      startReauthRedirect: (...a: unknown[]) => api.startReauth(...a),
      completeReauthRedirect: (...a: unknown[]) => api.completeReauth(...a),
    },
  };
});

import ProfileScreen from "./ProfileScreen";
import SignInCallbackScreen from "./SignInCallbackScreen";

const assign = vi.fn();
const realLocation = window.location;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  for (const fn of Object.values(api)) fn.mockReset();
  assign.mockReset();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...realLocation, assign },
  });
});

afterEach(() => {
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

const view = {
  identities: [{ provider: "directory", providerName: "Home directory", username: "bj" }],
  hasPassword: true,
  password: [{ id: "directory", name: "Home directory" }],
  redirect: [{ id: "oidc", name: "OpenID Connect" }],
};

describe("the profile's sign-ins", () => {
  it("lists the caller's attached sign-ins", async () => {
    api.list.mockResolvedValue(view);
    renderWithAuth(<ProfileScreen />);

    const row = await screen.findByTestId("profile-identity-directory");
    expect(row).toHaveTextContent("Home directory");
    expect(row).toHaveTextContent("bj");
  });

  it("attaches through a password provider and shows it", async () => {
    api.list.mockResolvedValueOnce({ ...view, identities: [] }).mockResolvedValueOnce(view);
    api.attachPassword.mockResolvedValue(view.identities[0]);
    renderWithAuth(<ProfileScreen />);

    const user = userEvent.setup();
    await user.type(await screen.findByTestId("profile-current-password"), "mine");
    await user.type(screen.getByTestId("profile-attach-username-directory"), "bj");
    await user.type(screen.getByTestId("profile-attach-password-directory"), "bj-pw");
    await user.click(screen.getByTestId("profile-attach-submit-directory"));

    expect(api.attachPassword).toHaveBeenCalledWith("directory", "bj", "bj-pw", { currentPassword: "mine" });
    expect(await screen.findByTestId("profile-identity-directory")).toBeInTheDocument();
  });

  it("shows the server's refusal when the sign-in is somebody else's", async () => {
    api.list.mockResolvedValue({ ...view, identities: [] });
    api.attachPassword.mockRejectedValue(
      new ApiError(409, "EXTERNAL_IDENTITY_HELD", "That sign-in is already attached to another account on this server."),
    );
    renderWithAuth(<ProfileScreen />);

    const user = userEvent.setup();
    await user.type(await screen.findByTestId("profile-current-password"), "mine");
    await user.type(screen.getByTestId("profile-attach-username-directory"), "ada");
    await user.type(screen.getByTestId("profile-attach-password-directory"), "pw");
    await user.click(screen.getByTestId("profile-attach-submit-directory"));

    expect(await screen.findByTestId("profile-attach-error")).toHaveTextContent("already attached to another account");
    expect(screen.queryByTestId("profile-identity-directory")).not.toBeInTheDocument();
  });

  it("sends the browser to a redirect provider and remembers it is an attach", async () => {
    api.list.mockResolvedValue(view);
    api.startAttach.mockResolvedValue("https://auth.example.test/authorize?state=st");
    renderWithAuth(<ProfileScreen />);

    const user = userEvent.setup();
    await user.type(await screen.findByTestId("profile-current-password"), "mine");
    await user.click(screen.getByTestId("profile-attach-redirect-oidc"));

    expect(api.startAttach).toHaveBeenCalledWith("oidc", { currentPassword: "mine" });
    expect(assign).toHaveBeenCalledWith("https://auth.example.test/authorize?state=st");
    expect(JSON.parse(window.sessionStorage.getItem("obelo.signIn.redirect") ?? "{}")).toMatchObject({
      from: "/profile",
      attach: true,
    });
  });
});

describe("confirming it is you before an attach", () => {
  it("cannot attach without the Local password", async () => {
    api.list.mockResolvedValue(view);
    renderWithAuth(<ProfileScreen />);

    expect(await screen.findByTestId("profile-attach-submit-directory")).toBeDisabled();
    expect(screen.getByTestId("profile-attach-redirect-oidc")).toBeDisabled();
    expect(screen.queryByTestId("profile-reauth-submit-directory")).not.toBeInTheDocument();
  });

  const outsideOnly = { ...view, hasPassword: false };

  it("has a User with no Local password confirm through a sign-in they hold, then attaches once with it", async () => {
    api.list.mockResolvedValue(outsideOnly);
    api.reauthPassword.mockResolvedValue({ grant: "g-1", expiresIn: 300 });
    api.attachPassword.mockResolvedValue(view.identities[0]);
    renderWithAuth(<ProfileScreen />);

    const user = userEvent.setup();
    expect(await screen.findByTestId("profile-attach-submit-directory")).toBeDisabled();
    expect(screen.queryByTestId("profile-current-password")).not.toBeInTheDocument();
    await user.type(screen.getByTestId("profile-reauth-username-directory"), "bj");
    await user.type(screen.getByTestId("profile-reauth-password-directory"), "bj-pw");
    await user.click(screen.getByTestId("profile-reauth-submit-directory"));
    expect(api.reauthPassword).toHaveBeenCalledWith("directory", "bj", "bj-pw");

    await waitFor(() => expect(screen.getByTestId("profile-attach-submit-directory")).toBeEnabled());
    await user.type(screen.getByTestId("profile-attach-username-directory"), "bj2");
    await user.type(screen.getByTestId("profile-attach-password-directory"), "pw2");
    await user.click(screen.getByTestId("profile-attach-submit-directory"));

    expect(api.attachPassword).toHaveBeenCalledWith("directory", "bj2", "pw2", { reauthGrant: "g-1" });
    await waitFor(() => expect(screen.getByTestId("profile-attach-submit-directory")).toBeDisabled());
  });

  it("confirms through a redirect sign-in they hold by sending the browser there", async () => {
    api.list.mockResolvedValue({
      ...outsideOnly,
      identities: [{ provider: "oidc", providerName: "OpenID Connect", username: "bj" }],
    });
    api.startReauth.mockResolvedValue("https://auth.example.test/authorize?state=re");
    renderWithAuth(<ProfileScreen />);

    await userEvent.setup().click(await screen.findByTestId("profile-reauth-redirect-oidc"));

    expect(api.startReauth).toHaveBeenCalledWith("oidc");
    expect(assign).toHaveBeenCalledWith("https://auth.example.test/authorize?state=re");
    expect(JSON.parse(window.sessionStorage.getItem("obelo.signIn.redirect") ?? "{}")).toMatchObject({
      from: "/profile",
      reauth: true,
    });
  });

  it("uses the grant a redirect re-auth brought back", async () => {
    window.sessionStorage.setItem(
      "obelo.reauthGrant",
      JSON.stringify({ grant: "g-redirect", expiresAt: Date.now() + 60_000 }),
    );
    api.list.mockResolvedValue(outsideOnly);
    api.startAttach.mockResolvedValue("https://auth.example.test/authorize?state=st");
    renderWithAuth(<ProfileScreen />);

    await userEvent.setup().click(await screen.findByTestId("profile-attach-redirect-oidc"));

    expect(api.startAttach).toHaveBeenCalledWith("oidc", { reauthGrant: "g-redirect" });
    expect(window.sessionStorage.getItem("obelo.reauthGrant")).toBeNull();
  });

  it("re-checks the grant when it is used, not only at mount", async () => {
    const now = Date.now();
    window.sessionStorage.setItem("obelo.reauthGrant", JSON.stringify({ grant: "g-late", expiresAt: now + 60_000 }));
    api.list.mockResolvedValue(outsideOnly);
    renderWithAuth(<ProfileScreen />);
    expect(await screen.findByTestId("profile-reauth-confirmed")).toBeInTheDocument();

    const clock = vi.spyOn(Date, "now").mockReturnValue(now + 120_000);
    try {
      await userEvent.setup().click(screen.getByTestId("profile-attach-redirect-oidc"));
    } finally {
      clock.mockRestore();
    }

    expect(api.startAttach).not.toHaveBeenCalled();
    expect(await screen.findByTestId("profile-attach-error")).toHaveTextContent("Confirm it is you again");
    expect(screen.queryByTestId("profile-reauth-confirmed")).not.toBeInTheDocument();
    expect(screen.getByTestId("profile-attach-redirect-oidc")).toBeDisabled();
  });

  it("does not send a grant that was forgotten while the profile was open", async () => {
    window.sessionStorage.setItem("obelo.reauthGrant", JSON.stringify({ grant: "g-gone", expiresAt: Date.now() + 60_000 }));
    api.list.mockResolvedValue(outsideOnly);
    api.attachPassword.mockResolvedValue(view.identities[0]);
    renderWithAuth(<ProfileScreen />);
    expect(await screen.findByTestId("profile-reauth-confirmed")).toBeInTheDocument();

    window.sessionStorage.removeItem("obelo.reauthGrant");
    const user = userEvent.setup();
    await user.type(screen.getByTestId("profile-attach-username-directory"), "bj2");
    await user.type(screen.getByTestId("profile-attach-password-directory"), "pw2");
    await user.click(screen.getByTestId("profile-attach-submit-directory"));

    expect(api.attachPassword).not.toHaveBeenCalled();
    expect(await screen.findByTestId("profile-attach-error")).toHaveTextContent("Confirm it is you again");
  });

  it("ignores an expired grant", async () => {
    window.sessionStorage.setItem("obelo.reauthGrant", JSON.stringify({ grant: "old", expiresAt: Date.now() - 1 }));
    api.list.mockResolvedValue(outsideOnly);
    renderWithAuth(<ProfileScreen />);

    expect(await screen.findByTestId("profile-attach-redirect-oidc")).toBeDisabled();
  });
});

describe("the kept re-auth grant", () => {
  // It belongs to the User and session that confirmed; signing out or switching
  // to somebody else forgets it, so nobody inherits it.
  function Controls() {
    const { session, logout, switchTo } = useAuth();
    return (
      <>
        <div data-testid="who">{session?.user.username ?? "none"}</div>
        <button data-testid="sign-out" onClick={() => void logout()} />
        <button data-testid="switch-ben" onClick={() => void switchTo("u-ben")} />
      </>
    );
  }

  function renderSignedIn() {
    window.localStorage.setItem("obelo.token", "tok-ada");
    window.localStorage.setItem("obelo.user", JSON.stringify({ id: "u-ada", username: "ada", role: "member" }));
    window.sessionStorage.setItem("obelo.reauthGrant", JSON.stringify({ grant: "g-ada", expiresAt: Date.now() + 60_000 }));
    const client = {
      token: "tok-ada",
      setToken: () => {},
      setTokenDurable: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
      logout: () => Promise.resolve(),
    } as unknown as ApiClient;
    return render(
      <MemoryRouter>
        <AuthProvider client={client}>
          <Controls />
        </AuthProvider>
      </MemoryRouter>,
    );
  }

  it("is forgotten on sign-out", async () => {
    renderSignedIn();
    expect(await screen.findByTestId("who")).toHaveTextContent("ada");

    await userEvent.setup().click(screen.getByTestId("sign-out"));

    await waitFor(() => expect(screen.getByTestId("who")).toHaveTextContent("none"));
    expect(window.sessionStorage.getItem("obelo.reauthGrant")).toBeNull();
  });

  it("is forgotten on a switch to another User", async () => {
    rememberUser(window.localStorage, null, { id: "u-ben", username: "ben", role: "member" }, "tok-ben");
    renderSignedIn();
    expect(await screen.findByTestId("who")).toHaveTextContent("ada");

    await userEvent.setup().click(screen.getByTestId("switch-ben"));

    await waitFor(() => expect(screen.getByTestId("who")).toHaveTextContent("ben"));
    expect(window.sessionStorage.getItem("obelo.reauthGrant")).toBeNull();
  });
});

describe("the callback screen finishing an attach", () => {
  const completeSignIn = vi.fn();

  function renderCallback(path: string) {
    const client = {
      token: "t",
      setToken: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
      completeRedirectSignIn: (...a: unknown[]) => completeSignIn(...a),
    } as unknown as ApiClient;
    function Wrapper({ children }: { children: ReactNode }) {
      return (
        <MemoryRouter initialEntries={[path]}>
          <AuthProvider client={client}>{children}</AuthProvider>
        </MemoryRouter>
      );
    }
    return render(
      <Routes>
        <Route path="/sign-in/callback" element={<SignInCallbackScreen />} />
        <Route path="/profile" element={<div data-testid="profile" />} />
      </Routes>,
      { wrapper: Wrapper },
    );
  }

  beforeEach(() => completeSignIn.mockReset());

  it("attaches rather than signing in, and returns to the profile", async () => {
    window.sessionStorage.setItem("obelo.signIn.redirect", JSON.stringify({ from: "/profile", remember: true, attach: true }));
    api.completeAttach.mockResolvedValue(view.identities[0]);
    renderCallback("/sign-in/callback?state=st-1&code=code-1");

    expect(await screen.findByTestId("profile")).toBeInTheDocument();
    expect(api.completeAttach).toHaveBeenCalledWith("st-1", "code-1");
    expect(completeSignIn).not.toHaveBeenCalled();
  });

  it("finishes a re-auth, keeps its grant for the profile, and returns there", async () => {
    window.sessionStorage.setItem("obelo.signIn.redirect", JSON.stringify({ from: "/profile", remember: true, reauth: true }));
    api.completeReauth.mockResolvedValue({ grant: "g-back", expiresIn: 300 });
    renderCallback("/sign-in/callback?state=st-2&code=code-2");

    expect(await screen.findByTestId("profile")).toBeInTheDocument();
    expect(api.completeReauth).toHaveBeenCalledWith("st-2", "code-2");
    expect(api.completeAttach).not.toHaveBeenCalled();
    expect(completeSignIn).not.toHaveBeenCalled();
    expect(JSON.parse(window.sessionStorage.getItem("obelo.reauthGrant") ?? "{}")).toMatchObject({ grant: "g-back" });
  });

  it("shows a refused attach and links back to the profile", async () => {
    window.sessionStorage.setItem("obelo.signIn.redirect", JSON.stringify({ from: "/profile", remember: true, attach: true }));
    api.completeAttach.mockRejectedValue(
      new ApiError(409, "EXTERNAL_IDENTITY_HELD", "That sign-in is already attached to another account on this server."),
    );
    renderCallback("/sign-in/callback?state=st-1&code=code-1");

    expect(await screen.findByTestId("sign-in-callback-error")).toHaveTextContent("already attached");
    await waitFor(() => expect(screen.getByTestId("sign-in-callback-back")).toHaveAttribute("href", "/profile"));
    expect(completeSignIn).not.toHaveBeenCalled();
  });
});
