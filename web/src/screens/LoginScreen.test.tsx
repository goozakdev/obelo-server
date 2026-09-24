import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import { ApiError, type ApiClient } from "../api/client";

// LoginScreen — the unchanged login form, now also the password flow of a
// Sign-in provider (ADR-0063). The form does not know which path signed someone
// in or refused them; what it must do is show the server's own sentence, because
// the two answers are deliberately different: every refusal reads the same, and
// a username collision after a provider accepted reads as what it is.

const { login } = vi.hoisted(() => ({ login: vi.fn() }));

function authStubClient(): ApiClient {
  return {
    token: null,
    setToken: () => {},
    setTokenDurable: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
    serverInfo: () =>
      Promise.resolve({ version: "test", supportedVersions: [1], features: {}, setupRequired: false }),
    login: (...a: unknown[]) => login(...a),
  } as unknown as ApiClient;
}

import LoginScreen from "./LoginScreen";

function renderLogin() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <MemoryRouter initialEntries={["/login"]}>
        <AuthProvider client={authStubClient()}>{children}</AuthProvider>
      </MemoryRouter>
    );
  }
  return render(
    <Routes>
      <Route path="/login" element={<LoginScreen />} />
      <Route path="/" element={<div data-testid="home" />} />
    </Routes>,
    { wrapper: Wrapper },
  );
}

async function submit(username: string, password: string) {
  const user = userEvent.setup();
  await user.type(screen.getByTestId("login-username"), username);
  await user.type(screen.getByTestId("login-password"), password);
  await user.click(screen.getByTestId("login-submit"));
}

beforeEach(() => {
  window.localStorage.clear();
  login.mockReset();
});

describe("LoginScreen", () => {
  it("shows the username-collision sentence, pointing at the profile and an Admin", async () => {
    const message =
      "Your sign-in was accepted, but an account with that username already exists on this server. " +
      "If it is yours, sign in to it and attach this sign-in from your profile; otherwise ask an Admin.";
    login.mockRejectedValue(new ApiError(409, "SIGN_IN_USERNAME_TAKEN", message));
    renderLogin();

    await submit("brandon", "directory-pw");

    expect(await screen.findByTestId("login-error")).toHaveTextContent(message);
    expect(screen.queryByTestId("home")).not.toBeInTheDocument();
  });

  it("shows the one refusal sentence for a refusal", async () => {
    login.mockRejectedValue(new ApiError(401, "INVALID_CREDENTIALS", "invalid username or password"));
    renderLogin();

    await submit("nobody", "whatever");

    expect(await screen.findByTestId("login-error")).toHaveTextContent("invalid username or password");
  });
});
