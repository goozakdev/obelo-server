import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { AuthProvider } from "../auth/session";
import { RequireAdmin } from "../auth/guards";
import { ApiError } from "../api/errors";
import type { ApiClient } from "../api/client";
import type { User } from "../api/types";

// AdminUsersScreen end-to-end through the faked API client (the one seam — exactly
// as AdminLibrariesScreen.test.tsx fakes apiClient): the list renders Users with
// their role; creating a Member calls createUser with the chosen fields and the
// new User appears (list refetched); an admin role is selectable; a USERNAME_TAKEN
// create shows a readable inline error and preserves the typed input (no crash);
// delete asks to confirm, calls deleteUser, and drops the row; a LAST_ADMIN
// rejection shows a readable inline error and keeps the User; in-flight create and
// delete show pending/disabled states. A separate block exercises the tab in the
// Admin hub (it renders beside the existing tabs) and the RequireAdmin gate.

const { listUsers, createUser, deleteUser } = vi.hoisted(() => ({
  listUsers: vi.fn(),
  createUser: vi.fn(),
  deleteUser: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listUsers: (...a: unknown[]) => listUsers(...a),
      createUser: (...a: unknown[]) => createUser(...a),
      deleteUser: (...a: unknown[]) => deleteUser(...a),
    },
  };
});

import AdminUsersScreen from "./AdminUsersScreen";
import AdminScreen from "../screens/AdminScreen";

function usr(over: Partial<User>): User {
  return { id: "u1", username: "ada", role: "member", ...over };
}

// A deferred promise so a test can hold a call "in flight" and assert pending UI.
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  listUsers.mockReset();
  createUser.mockReset();
  deleteUser.mockReset();
});

describe("AdminUsersScreen", () => {
  it("renders every user with username and role", async () => {
    listUsers.mockResolvedValue([
      usr({ id: "u1", username: "operator", role: "admin" }),
      usr({ id: "u2", username: "ada", role: "member" }),
    ]);
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });

    await waitFor(() =>
      expect(screen.getByTestId("admin-user-list")).toBeInTheDocument(),
    );
    const rows = screen.getAllByTestId("admin-user-row");
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByTestId("admin-user-username")).toHaveTextContent(
      "operator",
    );
    expect(within(rows[0]).getByTestId("admin-user-role")).toHaveTextContent("admin");
    expect(within(rows[1]).getByTestId("admin-user-username")).toHaveTextContent("ada");
    expect(within(rows[1]).getByTestId("admin-user-role")).toHaveTextContent("member");
  });

  it("shows a clean empty state with no users", async () => {
    listUsers.mockResolvedValue([]);
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );
  });

  it("creates a Member (role defaults to member) and shows it after reload", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([])
      .mockResolvedValue([usr({ id: "u2", username: "ada", role: "member" })]);
    createUser.mockResolvedValue(usr({ id: "u2", username: "ada", role: "member" }));

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "s3cret!");
    // Role left untouched → defaults to member.
    await user.click(screen.getByTestId("create-user-submit"));

    await waitFor(() =>
      expect(createUser).toHaveBeenCalledWith({
        username: "ada",
        password: "s3cret!",
        role: "member",
      }),
    );
    await waitFor(() => expect(screen.getByText("ada")).toBeInTheDocument());
  });

  it("creates an Admin when the role is selected", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([])
      .mockResolvedValue([usr({ id: "u3", username: "boss", role: "admin" })]);
    createUser.mockResolvedValue(usr({ id: "u3", username: "boss", role: "admin" }));

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("user-username-input"), "boss");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.selectOptions(screen.getByTestId("user-role-select"), "admin");
    await user.click(screen.getByTestId("create-user-submit"));

    await waitFor(() =>
      expect(createUser).toHaveBeenCalledWith({
        username: "boss",
        password: "pw",
        role: "admin",
      }),
    );
  });

  it("renders a readable inline error on USERNAME_TAKEN and preserves input (no crash)", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    createUser.mockImplementation(() =>
      Promise.reject(
        new ApiError(409, "USERNAME_TAKEN", "username ada is already taken"),
      ),
    );

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    const err = await screen.findByTestId("create-user-error");
    expect(err).toHaveTextContent(/already taken/i);
    expect(err).toHaveAttribute("data-taken", "true");
    // Form still mounted and the typed username is preserved (not cleared).
    expect(screen.getByTestId("create-user-form")).toBeInTheDocument();
    expect(screen.getByTestId("user-username-input")).toHaveValue("ada");
    expect(screen.getByTestId("user-password-input")).toHaveValue("pw");
  });

  it("disables the create button while the create is in flight", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    const pending = deferred<User>();
    createUser.mockReturnValue(pending.promise);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    // The submit shows a pending label and is disabled until the call settles.
    await waitFor(() =>
      expect(screen.getByTestId("create-user-submit")).toBeDisabled(),
    );
    expect(screen.getByTestId("create-user-submit")).toHaveTextContent(/creating/i);

    pending.resolve(usr({ id: "u2", username: "ada" }));
    await waitFor(() =>
      expect(screen.getByTestId("create-user-submit")).toBeEnabled(),
    );
  });

  it("deletes a user after confirmation and removes the row", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([
        usr({ id: "u1", username: "operator", role: "admin" }),
        usr({ id: "u2", username: "ada", role: "member" }),
      ])
      .mockResolvedValue([usr({ id: "u1", username: "operator", role: "admin" })]);
    deleteUser.mockResolvedValue(undefined);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getAllByTestId("admin-user-row")).toHaveLength(2),
    );

    const adaRow = screen
      .getAllByTestId("admin-user-row")
      .find((r) => within(r).queryByText("ada"))!;

    // First click reveals the confirm step; no call yet.
    await user.click(within(adaRow).getByTestId("delete-user-button"));
    expect(deleteUser).not.toHaveBeenCalled();
    expect(within(adaRow).getByTestId("delete-user-confirm")).toBeInTheDocument();

    await user.click(within(adaRow).getByTestId("delete-user-confirm-button"));
    await waitFor(() => expect(deleteUser).toHaveBeenCalledWith("u2"));
    await waitFor(() =>
      expect(screen.getAllByTestId("admin-user-row")).toHaveLength(1),
    );
    expect(screen.queryByText("ada")).not.toBeInTheDocument();
    expect(screen.getByText("operator")).toBeInTheDocument();
  });

  it("can cancel the delete confirmation without calling delete", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([usr({ id: "u2", username: "ada" })]);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("delete-user-cancel-button"));
    expect(deleteUser).not.toHaveBeenCalled();
    expect(screen.getByTestId("delete-user-button")).toBeInTheDocument();
  });

  it("keeps the user and shows a readable error on LAST_ADMIN", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([
      usr({ id: "u1", username: "operator", role: "admin" }),
    ]);
    deleteUser.mockImplementation(() =>
      Promise.reject(
        new ApiError(409, "LAST_ADMIN", "cannot delete the last admin"),
      ),
    );

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("delete-user-confirm-button"));

    const err = await screen.findByTestId("delete-user-error");
    expect(err).toHaveTextContent(/last admin/i);
    // The User survives a refused delete.
    expect(screen.getByTestId("admin-user-row")).toBeInTheDocument();
    expect(screen.getByText("operator")).toBeInTheDocument();
  });

  it("disables the confirm-delete button while the delete is in flight", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([usr({ id: "u2", username: "ada" })]);
    const pending = deferred<void>();
    deleteUser.mockReturnValue(pending.promise);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("delete-user-confirm-button"));

    await waitFor(() =>
      expect(screen.getByTestId("delete-user-confirm-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("delete-user-confirm-button")).toHaveTextContent(
      /deleting/i,
    );

    pending.resolve();
  });
});

describe("Users tab in the Admin hub", () => {
  it("renders the Users tab beside the existing tabs and mounts the screen", async () => {
    listUsers.mockResolvedValue([]);
    // AdminScreen's nested <Routes> are relative to its /admin/* mount point, so
    // mount it under that parent route (as App.tsx does) for the path to resolve.
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/users"] },
    );

    // The new tab plus the existing tabs are all present (no regression).
    expect(screen.getByTestId("admin-tab-users")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-libraries")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-attention")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-devices")).toBeInTheDocument();

    // /admin/users mounts AdminUsersScreen.
    await waitFor(() =>
      expect(screen.getByTestId("admin-users")).toBeInTheDocument(),
    );
    expect(listUsers).toHaveBeenCalled();
  });

  it("redirects a Member away from /admin/users (RequireAdmin gate)", async () => {
    // Seed a Member session (renderWithAuth hardcodes an Admin, so wire it by hand).
    window.localStorage.setItem("juicebox.token", "fake-token");
    window.localStorage.setItem(
      "juicebox.user",
      JSON.stringify({ id: "m1", username: "kid", role: "member" }),
    );
    const stub = {
      token: "fake-token",
      setToken: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
    } as unknown as ApiClient;

    render(
      <MemoryRouter initialEntries={["/admin/users"]}>
        <AuthProvider client={stub}>
          <Routes>
            <Route path="/" element={<div data-testid="landing" />} />
            <Route
              path="/admin/*"
              element={
                <RequireAdmin>
                  <AdminScreen />
                </RequireAdmin>
              }
            />
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    );

    // The gate bounces the Member to the landing; the Users tab never mounts.
    await waitFor(() => expect(screen.getByTestId("landing")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-users")).not.toBeInTheDocument();
    expect(listUsers).not.toHaveBeenCalled();
  });
});
