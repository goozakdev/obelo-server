import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import AppHeader from "./AppHeader";

// R05-19: sign-out still lands on /login when the logout call itself rejects (the
// harness's stub client has no logout(), so session.logout() rethrows).
describe("AppHeader sign out", () => {
  it("navigates to /login even when logout() rejects", async () => {
    renderWithAuth(
      <Routes>
        <Route path="/login" element={<div data-testid="login-landing" />} />
        <Route path="/" element={<AppHeader />} />
      </Routes>,
    );
    await userEvent.click(await screen.findByTestId("user-menu-toggle"));
    await userEvent.click(await screen.findByTestId("logout-button"));
    await waitFor(() => expect(screen.getByTestId("login-landing")).toBeInTheDocument());
  });
});
