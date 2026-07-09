import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type { Library, User, UserDetail } from "../api/types";

// UserAdminRow's library-access grant editor through the faked API client (the
// one seam — exactly as AdminUsersScreen/AdminLibrariesScreen fake apiClient).
// Covers every acceptance criterion of issue 02: a Member's row shows its current
// grants (from getUser); opening the editor shows a checklist of ALL Libraries
// with current grants pre-ticked; saving sends the FULL chosen set (replace-set)
// and the row reflects it; an empty tick set saves as "sees no catalog"; an Admin
// row reads "all Libraries" with no editable control; a defensive ADMIN_GRANT /
// UNKNOWN_LIBRARY save surfaces a readable inline error without crashing; and the
// save shows a pending state and recovers on error.

const {
  getUser,
  setLibraryAccess,
  listLibraries,
  deleteUser,
  setRatingCeiling,
  setPassword,
} = vi.hoisted(() => ({
  getUser: vi.fn(),
  setLibraryAccess: vi.fn(),
  listLibraries: vi.fn(),
  deleteUser: vi.fn(),
  setRatingCeiling: vi.fn(),
  setPassword: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getUser: (...a: unknown[]) => getUser(...a),
      setLibraryAccess: (...a: unknown[]) => setLibraryAccess(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      deleteUser: (...a: unknown[]) => deleteUser(...a),
      setRatingCeiling: (...a: unknown[]) => setRatingCeiling(...a),
      setPassword: (...a: unknown[]) => setPassword(...a),
    },
  };
});

import UserAdminRow from "./UserAdminRow";

function lib(id: string, name: string): Library {
  return { id, name, kind: "movie", rootFolders: [] };
}
function usr(over: Partial<User>): User {
  return { id: "u2", username: "ada", role: "member", ...over };
}
function detail(over: Partial<UserDetail>): UserDetail {
  return {
    id: "u2",
    username: "ada",
    role: "member",
    libraryIds: [],
    ratingCeiling: "",
    ...over,
  };
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const ALL_LIBS = [
  lib("l1", "Kids Movies"),
  lib("l2", "Family TV"),
  lib("l3", "My Library"),
];

function renderRow(user: User) {
  return render(
    <ul>
      <UserAdminRow user={user} onDeleted={() => {}} />
    </ul>,
  );
}

beforeEach(() => {
  getUser.mockReset();
  setLibraryAccess.mockReset();
  listLibraries.mockReset();
  deleteUser.mockReset();
  setRatingCeiling.mockReset();
  setPassword.mockReset();
});

describe("UserAdminRow — library grants (Member)", () => {
  it("shows the Member's current granted Libraries after opening the editor", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: ["l1", "l2"] }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    await waitFor(() => expect(getUser).toHaveBeenCalledWith("u2"));
    const summary = await screen.findByTestId("granted-libraries");
    expect(summary).toHaveTextContent("Kids Movies");
    expect(summary).toHaveTextContent("Family TV");
    expect(summary).not.toHaveTextContent("My Library");
  });

  it("opens a checklist of all Libraries with current grants pre-ticked", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: ["l2"] }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("library-checkbox-l1")).not.toBeChecked();
    expect(screen.getByTestId("library-checkbox-l2")).toBeChecked();
    expect(screen.getByTestId("library-checkbox-l3")).not.toBeChecked();
  });

  it("saves the FULL chosen set (replace-set) and the row reflects the new grants", async () => {
    const user = userEvent.setup();
    getUser
      .mockResolvedValueOnce(detail({ libraryIds: ["l2"] }))
      .mockResolvedValue(detail({ libraryIds: ["l1", "l2"] }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setLibraryAccess.mockResolvedValue(undefined);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    await screen.findByTestId("library-checklist");

    // l2 was pre-ticked; add l1 → the saved set is the full {l1, l2}.
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("save-library-access-button"));

    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledTimes(1));
    const [id, ids] = setLibraryAccess.mock.calls[0];
    expect(id).toBe("u2");
    expect([...(ids as string[])].sort()).toEqual(["l1", "l2"]);

    // The granted summary refetches and reflects the new set.
    await waitFor(() =>
      expect(screen.getByTestId("granted-libraries")).toHaveTextContent(
        "Kids Movies",
      ),
    );
    expect(screen.getByTestId("granted-libraries")).toHaveTextContent("Family TV");
  });

  it("saves an empty set (sees no catalog) when nothing is ticked", async () => {
    const user = userEvent.setup();
    getUser
      .mockResolvedValueOnce(detail({ libraryIds: ["l1"] }))
      .mockResolvedValue(detail({ libraryIds: [] }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setLibraryAccess.mockResolvedValue(undefined);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    await screen.findByTestId("library-checklist");

    // Untick the only pre-ticked Library, then save → empty replace-set.
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("save-library-access-button"));

    await waitFor(() =>
      expect(setLibraryAccess).toHaveBeenCalledWith("u2", []),
    );
    await waitFor(() =>
      expect(screen.getByTestId("granted-libraries")).toHaveTextContent(
        /no libraries/i,
      ),
    );
  });

  it("surfaces a readable inline error on UNKNOWN_LIBRARY without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setLibraryAccess.mockRejectedValue(
      new ApiError(422, "UNKNOWN_LIBRARY", "library l9 does not exist"),
    );

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    await screen.findByTestId("library-checklist");

    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("save-library-access-button"));

    const err = await screen.findByTestId("library-access-error");
    expect(err).toHaveTextContent(/does not exist/i);
    // The editor survives a refused save.
    expect(screen.getByTestId("library-checklist")).toBeInTheDocument();
  });

  it("surfaces a defensive ADMIN_GRANT error without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setLibraryAccess.mockRejectedValue(
      new ApiError(422, "ADMIN_GRANT", "cannot grant libraries to an admin"),
    );

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("save-library-access-button"));

    const err = await screen.findByTestId("library-access-error");
    expect(err).toHaveTextContent(/cannot grant libraries to an admin/i);
    expect(screen.getByTestId("library-checklist")).toBeInTheDocument();
  });

  it("shows a pending state during save and recovers on error", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    const pending = deferred<void>();
    setLibraryAccess.mockReturnValue(pending.promise);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("save-library-access-button"));

    // Pending: the save button disables + shows a saving label; boxes lock.
    await waitFor(() =>
      expect(screen.getByTestId("save-library-access-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("save-library-access-button")).toHaveTextContent(
      /saving/i,
    );
    expect(screen.getByTestId("library-checkbox-l1")).toBeDisabled();

    // Reject → recovers (button re-enabled, error shown).
    pending.reject(new ApiError(500, "INTERNAL", "boom"));
    await waitFor(() =>
      expect(screen.getByTestId("save-library-access-button")).toBeEnabled(),
    );
    expect(screen.getByTestId("library-access-error")).toHaveTextContent(/boom/i);
  });

  it("surfaces a load failure with a retry, then recovers", async () => {
    const user = userEvent.setup();
    getUser.mockRejectedValueOnce(
      new ApiError(404, "NOT_FOUND", "user not found"),
    );
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    const loadErr = await screen.findByTestId("library-access-load-error");
    expect(loadErr).toHaveTextContent(/user not found/i);

    // Retry succeeds → the checklist renders.
    getUser.mockResolvedValue(detail({ libraryIds: ["l1"] }));
    await user.click(screen.getByTestId("library-access-retry"));
    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("library-checkbox-l1")).toBeChecked();
  });
});

describe("UserAdminRow — library grants (Admin guard)", () => {
  it("reads 'All libraries' and exposes no editable grant control", () => {
    renderRow(usr({ id: "u1", username: "operator", role: "admin" }));

    expect(screen.getByTestId("admin-all-libraries")).toHaveTextContent(
      /all libraries/i,
    );
    expect(
      screen.queryByTestId("manage-libraries-button"),
    ).not.toBeInTheDocument();
    expect(screen.queryByTestId("library-checklist")).not.toBeInTheDocument();
    // No detail/library fetch for an Admin row.
    expect(getUser).not.toHaveBeenCalled();
    expect(listLibraries).not.toHaveBeenCalled();
  });

  it("still offers delete on an Admin row (issue 01 behavior intact)", async () => {
    const user = userEvent.setup();
    renderRow(usr({ id: "u1", username: "operator", role: "admin" }));

    const row = screen.getByTestId("admin-user-row");
    expect(within(row).getByTestId("delete-user-button")).toBeInTheDocument();
    await user.click(within(row).getByTestId("delete-user-button"));
    expect(within(row).getByTestId("delete-user-confirm")).toBeInTheDocument();
  });
});

// Issue 03: the Rating-ceiling dropdown lives in the same Member access panel.
// The detail (which carries `ratingCeiling`) loads lazily when the panel opens —
// just like the grant editor — so every test opens "Manage libraries" first.
describe("UserAdminRow — rating ceiling (Member)", () => {
  it("shows the Member's ceiling when capped", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "PG-13" }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    const summary = await screen.findByTestId("rating-ceiling-summary");
    expect(summary).toHaveTextContent("PG-13");
  });

  it("shows 'No limit' when uncapped", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    const summary = await screen.findByTestId("rating-ceiling-summary");
    expect(summary).toHaveTextContent(/no limit/i);
  });

  it("offers G/PG/PG-13/R/NC-17 + No limit, preselecting the current ceiling", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "R" }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    const select = (await screen.findByTestId(
      "rating-ceiling-select",
    )) as HTMLSelectElement;
    const labels = [...select.options].map((o) => o.textContent);
    expect(labels).toEqual(["No limit", "G", "PG", "PG-13", "R", "NC-17"]);
    // The loaded ceiling is pre-selected.
    expect(select.value).toBe("R");
  });

  it("preselects 'No limit' (empty value) when uncapped", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));

    const select = (await screen.findByTestId(
      "rating-ceiling-select",
    )) as HTMLSelectElement;
    expect(select.value).toBe("");
  });

  it("choosing a rung calls setRatingCeiling with that label and the row reflects it", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setRatingCeiling.mockResolvedValue(undefined);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    const select = await screen.findByTestId("rating-ceiling-select");

    await user.selectOptions(select, "PG-13");

    await waitFor(() =>
      expect(setRatingCeiling).toHaveBeenCalledWith("u2", "PG-13"),
    );
    await waitFor(() =>
      expect(screen.getByTestId("rating-ceiling-summary")).toHaveTextContent(
        "PG-13",
      ),
    );
  });

  it("choosing 'No limit' calls setRatingCeiling with null and the row reflects it", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "R" }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setRatingCeiling.mockResolvedValue(undefined);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    const select = await screen.findByTestId("rating-ceiling-select");

    await user.selectOptions(select, "No limit");

    await waitFor(() =>
      expect(setRatingCeiling).toHaveBeenCalledWith("u2", null),
    );
    await waitFor(() =>
      expect(screen.getByTestId("rating-ceiling-summary")).toHaveTextContent(
        /no limit/i,
      ),
    );
  });

  it("surfaces a defensive ADMIN_CEILING error without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setRatingCeiling.mockRejectedValue(
      new ApiError(422, "ADMIN_CEILING", "cannot cap an admin"),
    );

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    const select = await screen.findByTestId("rating-ceiling-select");
    await user.selectOptions(select, "R");

    const err = await screen.findByTestId("rating-ceiling-error");
    expect(err).toHaveTextContent(/cannot cap an admin/i);
    // The panel survives a refused save.
    expect(screen.getByTestId("rating-ceiling-select")).toBeInTheDocument();
  });

  it("surfaces a defensive UNKNOWN_RATING error without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    setRatingCeiling.mockRejectedValue(
      new ApiError(422, "UNKNOWN_RATING", "TV-MA is not a known rating"),
    );

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    const select = await screen.findByTestId("rating-ceiling-select");
    await user.selectOptions(select, "G");

    const err = await screen.findByTestId("rating-ceiling-error");
    expect(err).toHaveTextContent(/not a known rating/i);
    expect(screen.getByTestId("rating-ceiling-select")).toBeInTheDocument();
  });

  it("shows a pending state during a ceiling save and recovers on error", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    listLibraries.mockResolvedValue(ALL_LIBS);
    const pending = deferred<void>();
    setRatingCeiling.mockReturnValue(pending.promise);

    renderRow(usr({}));
    await user.click(screen.getByTestId("manage-libraries-button"));
    const select = await screen.findByTestId("rating-ceiling-select");
    await user.selectOptions(select, "PG");

    // Pending: the select locks and a saving affordance shows.
    await waitFor(() =>
      expect(screen.getByTestId("rating-ceiling-select")).toBeDisabled(),
    );
    expect(screen.getByTestId("rating-ceiling-saving")).toBeInTheDocument();

    // Reject → recovers (select re-enabled, error shown).
    pending.reject(new ApiError(500, "INTERNAL", "boom"));
    await waitFor(() =>
      expect(screen.getByTestId("rating-ceiling-select")).toBeEnabled(),
    );
    expect(screen.getByTestId("rating-ceiling-error")).toHaveTextContent(/boom/i);
  });
});

describe("UserAdminRow — rating ceiling (Admin guard)", () => {
  it("reads 'no cap' and exposes no editable ceiling control", () => {
    renderRow(usr({ id: "u1", username: "operator", role: "admin" }));

    expect(screen.getByTestId("admin-no-cap")).toHaveTextContent(/no cap/i);
    expect(
      screen.queryByTestId("rating-ceiling-select"),
    ).not.toBeInTheDocument();
    expect(setRatingCeiling).not.toHaveBeenCalled();
  });
});

// Issue 03: password reset is available for ANY User (incl. an Admin) and does
// not depend on opening the access panel.
describe("UserAdminRow — password reset", () => {
  it("resets a Member's password and shows success", async () => {
    const user = userEvent.setup();
    setPassword.mockResolvedValue(undefined);

    renderRow(usr({}));
    await user.click(screen.getByTestId("reset-password-button"));
    await user.type(screen.getByTestId("new-password-input"), "hunter2");
    await user.click(screen.getByTestId("save-password-button"));

    await waitFor(() =>
      expect(setPassword).toHaveBeenCalledWith("u2", "hunter2"),
    );
    expect(
      await screen.findByTestId("password-reset-success"),
    ).toBeInTheDocument();
  });

  it("is available on an Admin row too", async () => {
    const user = userEvent.setup();
    setPassword.mockResolvedValue(undefined);

    renderRow(usr({ id: "u1", username: "operator", role: "admin" }));
    await user.click(screen.getByTestId("reset-password-button"));
    await user.type(screen.getByTestId("new-password-input"), "newpass");
    await user.click(screen.getByTestId("save-password-button"));

    await waitFor(() =>
      expect(setPassword).toHaveBeenCalledWith("u1", "newpass"),
    );
    expect(
      await screen.findByTestId("password-reset-success"),
    ).toBeInTheDocument();
  });

  it("disables Save until a password is typed", async () => {
    const user = userEvent.setup();

    renderRow(usr({}));
    await user.click(screen.getByTestId("reset-password-button"));
    expect(screen.getByTestId("save-password-button")).toBeDisabled();

    await user.type(screen.getByTestId("new-password-input"), "x");
    expect(screen.getByTestId("save-password-button")).toBeEnabled();
  });

  it("shows a pending state during the save and recovers on error", async () => {
    const user = userEvent.setup();
    const pending = deferred<void>();
    setPassword.mockReturnValue(pending.promise);

    renderRow(usr({}));
    await user.click(screen.getByTestId("reset-password-button"));
    await user.type(screen.getByTestId("new-password-input"), "hunter2");
    await user.click(screen.getByTestId("save-password-button"));

    await waitFor(() =>
      expect(screen.getByTestId("save-password-button")).toBeDisabled(),
    );
    expect(screen.getByTestId("save-password-button")).toHaveTextContent(
      /saving/i,
    );

    pending.reject(new ApiError(500, "INTERNAL", "kaboom"));
    await waitFor(() =>
      expect(screen.getByTestId("password-reset-error")).toHaveTextContent(
        /kaboom/i,
      ),
    );
    // The form stays open so the Admin can retry.
    expect(screen.getByTestId("new-password-input")).toBeInTheDocument();
  });
});
