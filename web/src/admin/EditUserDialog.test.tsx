import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type { Library, User, UserDetail } from "../api/types";

// EditUserDialog through the faked API client (the one seam — exactly as
// AdminUsersScreen/AdminLibrariesScreen fake apiClient). This dialog is where all
// per-User editing moved when the roster became a plain list, so it inherits the
// access-control acceptance criteria the row used to carry:
//   - a Member's current grants pre-tick a checklist of ALL Libraries, and saving
//     sends the FULL ticked set (replace-set), an empty set included;
//   - the Rating-ceiling dropdown offers the MPAA rungs + "No limit", preselects
//     the current cap, and saves the label (or null);
//   - a password reset is available for ANY User, Admin included;
//   - an Admin has NO grant or ceiling control at all (the server refuses both);
//   - one Save applies only what changed, in one pass, and a refusal
//     (ADMIN_GRANT / UNKNOWN_LIBRARY / ADMIN_CEILING / UNKNOWN_RATING) surfaces
//     inline with the dialog and its edits intact;
//   - "Delete user" reports up to the hub rather than deleting here.

const { getUser, listLibraries, setLibraryAccess, setRatingCeiling, setPassword } =
  vi.hoisted(() => ({
    getUser: vi.fn(),
    listLibraries: vi.fn(),
    setLibraryAccess: vi.fn(),
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
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      setLibraryAccess: (...a: unknown[]) => setLibraryAccess(...a),
      setRatingCeiling: (...a: unknown[]) => setRatingCeiling(...a),
      setPassword: (...a: unknown[]) => setPassword(...a),
    },
  };
});

import EditUserDialog from "./EditUserDialog";

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

function renderDialog(
  user: User,
  handlers: { onClose?: () => void; onRequestDelete?: (u: User) => void } = {},
) {
  return render(
    <EditUserDialog
      user={user}
      onClose={handlers.onClose ?? (() => {})}
      onRequestDelete={handlers.onRequestDelete ?? (() => {})}
    />,
  );
}

beforeEach(() => {
  getUser.mockReset();
  listLibraries.mockReset();
  setLibraryAccess.mockReset();
  setRatingCeiling.mockReset();
  setPassword.mockReset();
  listLibraries.mockResolvedValue(ALL_LIBS);
  setLibraryAccess.mockResolvedValue(undefined);
  setRatingCeiling.mockResolvedValue(undefined);
  setPassword.mockResolvedValue(undefined);
});

describe("EditUserDialog — library grants (Member)", () => {
  it("pre-ticks the Member's current grants in a checklist of all Libraries", async () => {
    getUser.mockResolvedValue(detail({ libraryIds: ["l2"] }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    expect(getUser).toHaveBeenCalledWith("u2");
    expect(screen.getByTestId("library-checkbox-l1")).not.toBeChecked();
    expect(screen.getByTestId("library-checkbox-l2")).toBeChecked();
    expect(screen.getByTestId("library-checkbox-l3")).not.toBeChecked();
  });

  it("saves the FULL chosen set (replace-set) and closes", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    getUser.mockResolvedValue(detail({ libraryIds: ["l2"] }));
    renderDialog(usr({}), { onClose });

    await screen.findByTestId("library-checklist");
    // l2 was pre-ticked; add l1 → the saved set is the full {l1, l2}.
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledTimes(1));
    const [id, ids] = setLibraryAccess.mock.calls[0];
    expect(id).toBe("u2");
    expect([...(ids as string[])].sort()).toEqual(["l1", "l2"]);
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  it("saves an empty set (sees no catalog) when everything is unticked", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: ["l1"] }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    // The consequence is spelled out before saving.
    expect(screen.getByTestId("no-libraries-hint")).toHaveTextContent(
      /sees no catalog/i,
    );

    await user.click(screen.getByTestId("edit-user-save"));
    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledWith("u2", []));
  });

  it("sends nothing when nothing changed (Save stays disabled)", async () => {
    getUser.mockResolvedValue(detail({ libraryIds: ["l1"], ratingCeiling: "PG" }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("edit-user-save")).toBeDisabled();
  });

  it("surfaces a readable inline error on UNKNOWN_LIBRARY and keeps the edits", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    setLibraryAccess.mockRejectedValue(
      new ApiError(422, "UNKNOWN_LIBRARY", "library l9 does not exist"),
    );
    renderDialog(usr({}), { onClose });

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-save"));

    const err = await screen.findByTestId("edit-user-error");
    expect(err).toHaveTextContent(/does not exist/i);
    // The dialog survives a refused save, with the tick still made.
    expect(screen.getByTestId("library-checkbox-l1")).toBeChecked();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("surfaces a defensive ADMIN_GRANT error without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    setLibraryAccess.mockRejectedValue(
      new ApiError(422, "ADMIN_GRANT", "cannot grant libraries to an admin"),
    );
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-save"));

    expect(await screen.findByTestId("edit-user-error")).toHaveTextContent(
      /cannot grant libraries to an admin/i,
    );
    expect(screen.getByTestId("library-checklist")).toBeInTheDocument();
  });

  it("shows a pending state during save and recovers on error", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    const pending = deferred<void>();
    setLibraryAccess.mockReturnValue(pending.promise);
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(screen.getByTestId("edit-user-save")).toBeDisabled());
    expect(screen.getByTestId("edit-user-save")).toHaveTextContent(/saving/i);
    expect(screen.getByTestId("library-checkbox-l1")).toBeDisabled();

    pending.reject(new ApiError(500, "INTERNAL", "boom"));
    await waitFor(() => expect(screen.getByTestId("edit-user-save")).toBeEnabled());
    expect(screen.getByTestId("edit-user-error")).toHaveTextContent(/boom/i);
  });

  it("surfaces a load failure with a retry, then recovers", async () => {
    const user = userEvent.setup();
    getUser.mockRejectedValueOnce(new ApiError(404, "NOT_FOUND", "user not found"));
    renderDialog(usr({}));

    expect(await screen.findByTestId("library-access-load-error")).toHaveTextContent(
      /user not found/i,
    );

    getUser.mockResolvedValue(detail({ libraryIds: ["l1"] }));
    await user.click(screen.getByTestId("library-access-retry"));
    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("library-checkbox-l1")).toBeChecked();
  });

  it("says so when the server has no libraries yet", async () => {
    getUser.mockResolvedValue(detail({}));
    listLibraries.mockResolvedValue([]);
    renderDialog(usr({}));

    expect(await screen.findByTestId("library-access-empty")).toBeInTheDocument();
    // The ceiling control still renders — it doesn't depend on the Library list.
    expect(screen.getByTestId("rating-ceiling-select")).toBeInTheDocument();
  });
});

describe("EditUserDialog — rating ceiling (Member)", () => {
  it("offers G/PG/PG-13/R/NC-17 + No limit, preselecting the current ceiling", async () => {
    getUser.mockResolvedValue(detail({ ratingCeiling: "R" }));
    renderDialog(usr({}));

    const select = (await screen.findByTestId(
      "rating-ceiling-select",
    )) as HTMLSelectElement;
    const labels = [...select.options].map((o) => o.textContent);
    expect(labels).toEqual(["No limit", "G", "PG", "PG-13", "R", "NC-17"]);
    expect(select.value).toBe("R");
  });

  it("preselects 'No limit' (empty value) when uncapped", async () => {
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    renderDialog(usr({}));

    const select = (await screen.findByTestId(
      "rating-ceiling-select",
    )) as HTMLSelectElement;
    expect(select.value).toBe("");
  });

  it("saves a chosen rung as its label", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    renderDialog(usr({}));

    await user.selectOptions(
      await screen.findByTestId("rating-ceiling-select"),
      "PG-13",
    );
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() =>
      expect(setRatingCeiling).toHaveBeenCalledWith("u2", "PG-13"),
    );
  });

  it("saves 'No limit' as null (clearing the cap)", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "R" }));
    renderDialog(usr({}));

    await user.selectOptions(
      await screen.findByTestId("rating-ceiling-select"),
      "No limit",
    );
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setRatingCeiling).toHaveBeenCalledWith("u2", null));
  });

  it("surfaces a defensive UNKNOWN_RATING error without crashing", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ ratingCeiling: "" }));
    setRatingCeiling.mockRejectedValue(
      new ApiError(422, "UNKNOWN_RATING", "TV-MA is not a known rating"),
    );
    renderDialog(usr({}));

    await user.selectOptions(await screen.findByTestId("rating-ceiling-select"), "G");
    await user.click(screen.getByTestId("edit-user-save"));

    expect(await screen.findByTestId("edit-user-error")).toHaveTextContent(
      /not a known rating/i,
    );
    expect(screen.getByTestId("rating-ceiling-select")).toBeInTheDocument();
  });
});

describe("EditUserDialog — password reset", () => {
  it("resets a Member's password", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({}));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    await user.type(screen.getByTestId("new-password-input"), "hunter2");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setPassword).toHaveBeenCalledWith("u2", "hunter2"));
    // Only the password changed, so nothing else is sent.
    expect(setLibraryAccess).not.toHaveBeenCalled();
    expect(setRatingCeiling).not.toHaveBeenCalled();
  });

  it("is available on an Admin too", async () => {
    const user = userEvent.setup();
    renderDialog(usr({ id: "u1", username: "operator", role: "admin" }));

    await user.type(screen.getByTestId("new-password-input"), "newpass");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setPassword).toHaveBeenCalledWith("u1", "newpass"));
  });

  it("applies a password, grants, and a ceiling together in one save", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ libraryIds: [], ratingCeiling: "" }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    await user.type(screen.getByTestId("new-password-input"), "hunter2");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.selectOptions(screen.getByTestId("rating-ceiling-select"), "PG");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setPassword).toHaveBeenCalledWith("u2", "hunter2"));
    expect(setLibraryAccess).toHaveBeenCalledWith("u2", ["l1"]);
    expect(setRatingCeiling).toHaveBeenCalledWith("u2", "PG");
  });
});

describe("EditUserDialog — Admin guard", () => {
  it("exposes no grant or ceiling control, and fetches nothing", () => {
    renderDialog(usr({ id: "u1", username: "operator", role: "admin" }));

    expect(screen.getByTestId("admin-all-libraries")).toHaveTextContent(
      /all libraries/i,
    );
    expect(screen.getByTestId("admin-no-cap")).toHaveTextContent(
      /no rating ceiling/i,
    );
    expect(screen.queryByTestId("library-checklist")).not.toBeInTheDocument();
    expect(screen.queryByTestId("rating-ceiling-select")).not.toBeInTheDocument();
    // No detail/library fetch for an Admin.
    expect(getUser).not.toHaveBeenCalled();
    expect(listLibraries).not.toHaveBeenCalled();
  });
});

describe("EditUserDialog — delete and dismiss", () => {
  it("reports the delete up rather than deleting here", async () => {
    const user = userEvent.setup();
    const onRequestDelete = vi.fn();
    getUser.mockResolvedValue(detail({}));
    renderDialog(usr({}), { onRequestDelete });

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("edit-user-delete"));
    expect(onRequestDelete).toHaveBeenCalledWith(
      expect.objectContaining({ id: "u2" }),
    );
  });

  it("cancels without sending anything", async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    getUser.mockResolvedValue(detail({}));
    renderDialog(usr({}), { onClose });

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-cancel"));

    expect(onClose).toHaveBeenCalled();
    expect(setLibraryAccess).not.toHaveBeenCalled();
    expect(setPassword).not.toHaveBeenCalled();
  });
});
