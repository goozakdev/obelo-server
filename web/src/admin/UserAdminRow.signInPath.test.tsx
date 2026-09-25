import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import UserAdminRow from "./UserAdminRow";
import type { AdminUser } from "../api/types";

// The Users page's flag for a User left with no working way to sign in
// (ADR-0063 decision 9): every Sign-in provider they hold is disabled or
// failing, and they have no Local password. Their sessions are kept, so the row
// is the only place the Admin learns it.

function row(user: AdminUser) {
  render(
    <ul>
      <UserAdminRow user={user} onEdit={() => {}} onDelete={() => {}} />
    </ul>,
  );
}

describe("UserAdminRow — no working sign-in path", () => {
  it("flags a User the server says has no working sign-in path", () => {
    row({ id: "u1", username: "ada", role: "member", noWorkingSignInPath: true });
    expect(screen.getByTestId("admin-user-no-sign-in-path")).toHaveTextContent(
      "No working sign-in path",
    );
  });

  it("flags nobody else", () => {
    row({ id: "u2", username: "brandon", role: "admin" });
    expect(screen.queryByTestId("admin-user-no-sign-in-path")).not.toBeInTheDocument();
  });
});
