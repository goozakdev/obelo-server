import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { AuthProvider } from "../auth/session";
import { RequireAdmin } from "../auth/guards";
import { ApiError } from "../api/errors";
import type { ApiClient } from "../api/client";
import type { Library, User } from "../api/types";

// AdminUsersScreen end-to-end through the faked API client (the one seam — exactly
// as AdminLibrariesScreen.test.tsx fakes apiClient). The redesigned hub is a list
// plus three dialogs, so the coverage follows those paths:
//   - the list renders one row per User with their role and no inline editors;
//   - "Add User" opens the create dialog; creating a Member sends the chosen
//     fields AND grants every Library (the default access set), an Admin skips the
//     grant, and a USERNAME_TAKEN create shows a readable inline error preserving
//     the typed input;
//   - a grant that fails after the create is stated plainly (the User exists) with
//     a Retry that re-runs only the grant;
//   - the row's delete icon confirms before deleting, drops the row on success,
//     and keeps the User with a readable message on LAST_ADMIN;
//   - the row's edit icon opens the Edit dialog, whose "Delete user" button routes
//     into the same one confirmation.
// A separate block exercises the tab in the Admin hub and the RequireAdmin gate.

const {
  listUsers,
  createUser,
  deleteUser,
  listLibraries,
  setLibraryAccess,
  getUser,
} = vi.hoisted(() => ({
  listUsers: vi.fn(),
  createUser: vi.fn(),
  deleteUser: vi.fn(),
  listLibraries: vi.fn(),
  setLibraryAccess: vi.fn(),
  getUser: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listUsers: (...a: unknown[]) => listUsers(...a),
      createUser: (...a: unknown[]) => createUser(...a),
      deleteUser: (...a: unknown[]) => deleteUser(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      setLibraryAccess: (...a: unknown[]) => setLibraryAccess(...a),
      getUser: (...a: unknown[]) => getUser(...a),
    },
  };
});

import AdminUsersScreen from "./AdminUsersScreen";
import AdminScreen from "../screens/AdminScreen";

function usr(over: Partial<User>): User {
  return { id: "u1", username: "ada", role: "member", ...over };
}

function lib(id: string, name: string): Library {
  return { id, name, kind: "movie", rootFolders: [] };
}

const ALL_LIBS = [lib("l1", "Movies"), lib("l2", "TV")];

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
  listLibraries.mockReset();
  setLibraryAccess.mockReset();
  getUser.mockReset();
  listLibraries.mockResolvedValue(ALL_LIBS);
  setLibraryAccess.mockResolvedValue(undefined);
});

describe("AdminUsersScreen — the roster list", () => {
  it("renders every user with username and role, and no inline editor", async () => {
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

    // Each row carries exactly the two icon actions; nothing is editable in place.
    expect(within(rows[1]).getByTestId("edit-user-button")).toBeInTheDocument();
    expect(within(rows[1]).getByTestId("delete-user-button")).toBeInTheDocument();
    expect(screen.queryByTestId("library-checklist")).not.toBeInTheDocument();
    expect(screen.queryByTestId("new-password-input")).not.toBeInTheDocument();

    expect(screen.getByTestId("admin-users-count")).toHaveTextContent("2 users");
  });

  it("shows a clean empty state with no users", async () => {
    listUsers.mockResolvedValue([]);
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );
    // The add action is still reachable from the empty state.
    expect(screen.getByTestId("add-user-button")).toBeInTheDocument();
  });

  it("surfaces a failed load as a readable error", async () => {
    listUsers.mockRejectedValue(new ApiError(403, "FORBIDDEN", "admin only"));
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    expect(await screen.findByTestId("admin-users-error")).toHaveTextContent(
      /admin only/i,
    );
  });
});

describe("AdminUsersScreen — adding a user", () => {
  it("opens the create dialog only when Add User is clicked", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument();
    await user.click(screen.getByTestId("add-user-button"));
    expect(screen.getByTestId("create-user-dialog")).toBeInTheDocument();
  });

  it("creates a Member (role defaults to member), grants ALL libraries, and shows it after reload", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([])
      .mockResolvedValue([usr({ id: "u2", username: "ada", role: "member" })]);
    createUser.mockResolvedValue(usr({ id: "u2", username: "ada", role: "member" }));

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
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
    // Every Library is granted by default, as the full replace-set.
    await waitFor(() =>
      expect(setLibraryAccess).toHaveBeenCalledWith("u2", ["l1", "l2"]),
    );
    // The dialog closes and the refetched list shows the new User.
    await waitFor(() =>
      expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument(),
    );
    expect(await screen.findByText("ada")).toBeInTheDocument();
  });

  it("does not grant libraries to a created Admin (they are all-access by role)", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([])
      .mockResolvedValue([usr({ id: "u3", username: "boss", role: "admin" })]);
    createUser.mockResolvedValue(usr({ id: "u3", username: "boss", role: "admin" }));

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
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
    await waitFor(() =>
      expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument(),
    );
    expect(setLibraryAccess).not.toHaveBeenCalled();
  });

  it("skips the grant when the server has no libraries yet", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    listLibraries.mockResolvedValue([]);
    createUser.mockResolvedValue(usr({ id: "u2", username: "ada" }));

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    await waitFor(() => expect(createUser).toHaveBeenCalled());
    await waitFor(() =>
      expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument(),
    );
    expect(setLibraryAccess).not.toHaveBeenCalled();
  });

  it("renders a readable inline error on USERNAME_TAKEN and preserves input (no crash)", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    createUser.mockRejectedValue(
      new ApiError(409, "USERNAME_TAKEN", "username ada is already taken"),
    );

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    const err = await screen.findByTestId("create-user-error");
    expect(err).toHaveTextContent(/already taken/i);
    expect(err).toHaveAttribute("data-taken", "true");
    // The dialog stays open and the typed values survive.
    expect(screen.getByTestId("create-user-dialog")).toBeInTheDocument();
    expect(screen.getByTestId("user-username-input")).toHaveValue("ada");
    expect(screen.getByTestId("user-password-input")).toHaveValue("pw");
  });

  it("states plainly when the User was created but the default grant failed, and retries only the grant", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    createUser.mockResolvedValue(usr({ id: "u2", username: "ada" }));
    setLibraryAccess.mockRejectedValueOnce(
      new ApiError(422, "UNKNOWN_LIBRARY", "library l2 does not exist"),
    );

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    const err = await screen.findByTestId("create-user-error");
    expect(err).toHaveTextContent(/was created/i);
    expect(err).toHaveTextContent(/does not exist/i);
    // The create is not offered again (the User exists) — Retry / Close instead.
    expect(screen.queryByTestId("create-user-submit")).not.toBeInTheDocument();

    // Retry re-runs only the grant.
    setLibraryAccess.mockResolvedValue(undefined);
    await user.click(screen.getByTestId("create-user-retry-grant"));
    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledTimes(2));
    expect(createUser).toHaveBeenCalledTimes(1);
    await waitFor(() =>
      expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument(),
    );
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

    await user.click(screen.getByTestId("add-user-button"));
    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.type(screen.getByTestId("user-password-input"), "pw");
    await user.click(screen.getByTestId("create-user-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("create-user-submit")).toBeDisabled(),
    );
    expect(screen.getByTestId("create-user-submit")).toHaveTextContent(/creating/i);

    pending.resolve(usr({ id: "u2", username: "ada" }));
    await waitFor(() =>
      expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument(),
    );
  });

  it("cancels the create dialog without calling the API", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([]);
    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("add-user-button"));
    await user.type(screen.getByTestId("user-username-input"), "ada");
    await user.click(screen.getByTestId("create-user-cancel"));

    expect(screen.queryByTestId("create-user-dialog")).not.toBeInTheDocument();
    expect(createUser).not.toHaveBeenCalled();
  });
});

describe("AdminUsersScreen — deleting a user", () => {
  it("asks to confirm, deletes, and removes the row", async () => {
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

    // The icon opens a confirmation; nothing is deleted yet.
    await user.click(within(adaRow).getByTestId("delete-user-button"));
    expect(deleteUser).not.toHaveBeenCalled();
    const dialog = screen.getByTestId("confirm-dialog");
    expect(within(dialog).getByTestId("confirm-dialog-message")).toHaveTextContent(
      "ada",
    );

    await user.click(screen.getByTestId("confirm-dialog-confirm"));
    await waitFor(() => expect(deleteUser).toHaveBeenCalledWith("u2"));
    await waitFor(() =>
      expect(screen.getAllByTestId("admin-user-row")).toHaveLength(1),
    );
    expect(screen.queryByText("ada")).not.toBeInTheDocument();
    expect(screen.getByText("operator")).toBeInTheDocument();
    expect(screen.queryByTestId("confirm-dialog")).not.toBeInTheDocument();
  });

  it("can cancel the confirmation without calling delete", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([usr({ id: "u2", username: "ada" })]);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("confirm-dialog-cancel"));
    expect(deleteUser).not.toHaveBeenCalled();
    expect(screen.queryByTestId("confirm-dialog")).not.toBeInTheDocument();
    expect(screen.getByTestId("admin-user-row")).toBeInTheDocument();
  });

  it("keeps the user and shows a readable error on LAST_ADMIN", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([
      usr({ id: "u1", username: "operator", role: "admin" }),
    ]);
    deleteUser.mockRejectedValue(
      new ApiError(409, "LAST_ADMIN", "cannot delete the last admin"),
    );

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("confirm-dialog-confirm"));

    const err = await screen.findByTestId("confirm-dialog-error");
    expect(err).toHaveTextContent(/last admin/i);
    // The dialog stays open and the User survives a refused delete.
    expect(screen.getByTestId("confirm-dialog")).toBeInTheDocument();
    expect(screen.getByText("operator")).toBeInTheDocument();
  });

  it("disables confirm while the delete is in flight", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([usr({ id: "u2", username: "ada" })]);
    const pending = deferred<void>();
    deleteUser.mockReturnValue(pending.promise);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("delete-user-button"));
    await user.click(screen.getByTestId("confirm-dialog-confirm"));

    await waitFor(() =>
      expect(screen.getByTestId("confirm-dialog-confirm")).toBeDisabled(),
    );
    expect(screen.getByTestId("confirm-dialog-confirm")).toHaveTextContent(
      /deleting/i,
    );

    pending.resolve();
  });
});

describe("AdminUsersScreen — editing a user", () => {
  it("opens the Edit dialog from the row's edit icon", async () => {
    const user = userEvent.setup();
    listUsers.mockResolvedValue([usr({ id: "u2", username: "ada" })]);
    getUser.mockResolvedValue({
      id: "u2",
      username: "ada",
      role: "member",
      libraryIds: ["l1"],
      ratingCeiling: "PG",
    });

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    expect(screen.queryByTestId("edit-user-dialog")).not.toBeInTheDocument();
    await user.click(screen.getByTestId("edit-user-button"));
    expect(screen.getByTestId("edit-user-dialog")).toBeInTheDocument();
    expect(screen.getByTestId("edit-user-username")).toHaveTextContent("ada");
    await waitFor(() => expect(getUser).toHaveBeenCalledWith("u2"));
  });

  it("routes the Edit dialog's Delete user button into the same confirmation", async () => {
    const user = userEvent.setup();
    listUsers
      .mockResolvedValueOnce([usr({ id: "u2", username: "ada" })])
      .mockResolvedValue([]);
    getUser.mockResolvedValue({
      id: "u2",
      username: "ada",
      role: "member",
      libraryIds: [],
      ratingCeiling: "",
    });
    deleteUser.mockResolvedValue(undefined);

    renderWithAuth(<AdminUsersScreen />, { initialEntries: ["/admin/users"] });
    await waitFor(() =>
      expect(screen.getByTestId("admin-user-row")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("edit-user-button"));
    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("edit-user-delete"));

    // The Edit dialog steps aside so the confirmation is the only modal open.
    expect(screen.queryByTestId("edit-user-dialog")).not.toBeInTheDocument();
    expect(screen.getByTestId("confirm-dialog")).toBeInTheDocument();
    expect(deleteUser).not.toHaveBeenCalled();

    await user.click(screen.getByTestId("confirm-dialog-confirm"));
    await waitFor(() => expect(deleteUser).toHaveBeenCalledWith("u2"));
    await waitFor(() =>
      expect(screen.getByTestId("admin-users-empty")).toBeInTheDocument(),
    );
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
    expect(screen.getByTestId("admin-tab-needs-fixing")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-devices")).toBeInTheDocument();

    // /admin/users mounts AdminUsersScreen.
    await waitFor(() =>
      expect(screen.getByTestId("admin-users")).toBeInTheDocument(),
    );
    expect(listUsers).toHaveBeenCalled();
  });

  it("redirects a Member away from /admin/users (RequireAdmin gate)", async () => {
    // Seed a Member session (renderWithAuth hardcodes an Admin, so wire it by hand).
    window.localStorage.setItem("obelo.token", "fake-token");
    window.localStorage.setItem(
      "obelo.user",
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
