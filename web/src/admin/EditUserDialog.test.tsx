import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type {
  Library,
  TailnetSettingsView,
  TailnetStatus,
  User,
  UserDetail,
} from "../api/types";

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

const {
  getUser,
  listLibraries,
  setLibraryAccess,
  setRatingCeiling,
  setPlaybackCeiling,
  setPassword,
  getTailnet,
  createLinkInvite,
} = vi.hoisted(() => ({
  getUser: vi.fn(),
  listLibraries: vi.fn(),
  setLibraryAccess: vi.fn(),
  setRatingCeiling: vi.fn(),
  setPlaybackCeiling: vi.fn(),
  setPassword: vi.fn(),
  getTailnet: vi.fn(),
  createLinkInvite: vi.fn(),
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
      setPlaybackCeiling: (...a: unknown[]) => setPlaybackCeiling(...a),
      setPassword: (...a: unknown[]) => setPassword(...a),
      getTailnet: (...a: unknown[]) => getTailnet(...a),
      createLinkInvite: (...a: unknown[]) => createLinkInvite(...a),
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
    maxResolution: "",
    maxBitrate: 0,
    maxStreams: 0,
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
  setPlaybackCeiling.mockReset();
  setPassword.mockReset();
  getTailnet.mockReset();
  createLinkInvite.mockReset();
  // No Tailnet unless a test says otherwise: the pre-fill is best-effort and a
  // build with the feature left out answers 503.
  getTailnet.mockRejectedValue(new ApiError(503, "SERVICE_UNAVAILABLE", "no tailnet"));
  listLibraries.mockResolvedValue(ALL_LIBS);
  setLibraryAccess.mockResolvedValue(undefined);
  setRatingCeiling.mockResolvedValue(undefined);
  setPlaybackCeiling.mockResolvedValue(undefined);
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

describe("EditUserDialog — a linked Library is never re-shared", () => {
  // linked-servers issue 11 / ADR-0054 §4: a Library that arrived over a Link is
  // not offered to a linked Server, and the absence is explained rather than
  // silent. Every other role is untouched — a mirror is an ordinary Library to
  // the household's own people (ADR-0056 §2).
  const WITH_MIRROR = [
    lib("l1", "Kids Movies"),
    { ...lib("l9", "Their Films"), linked: true, available: true },
  ];

  it("offers a linked server this Server's OWN libraries only, and says why", async () => {
    listLibraries.mockResolvedValue(WITH_MIRROR);
    getUser.mockResolvedValue(detail({ role: "remote", libraryIds: ["l1"] }));
    renderDialog(usr({ role: "remote" }));

    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("library-checkbox-l1")).toBeChecked();
    expect(screen.queryByTestId("library-checkbox-l9")).toBeNull();
    expect(screen.getByTestId("linked-not-grantable")).toHaveTextContent(
      /provided by another server can.t be shared onward/i,
    );
  });

  it("still offers a Member the linked Library, with no note", async () => {
    listLibraries.mockResolvedValue(WITH_MIRROR);
    getUser.mockResolvedValue(detail({ libraryIds: ["l9"] }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    expect(screen.getByTestId("library-checkbox-l9")).toBeChecked();
    expect(screen.queryByTestId("linked-not-grantable")).toBeNull();
  });

  it("names the sharing server beside a linked Library on a Member's grant list (issue 18)", async () => {
    const NAMED = [
      lib("l1", "Kids Movies"),
      {
        ...lib("l9", "Their Films"),
        linked: true,
        available: true,
        linkedServer: "Kate's Obelo",
      },
    ];
    listLibraries.mockResolvedValue(NAMED);
    getUser.mockResolvedValue(detail({ libraryIds: [] }));
    renderDialog(usr({}));

    await screen.findByTestId("library-checklist");
    // The linked shelf stays grantable AND names whose it is.
    expect(screen.getByTestId("library-checkbox-l9")).toBeInTheDocument();
    const badge = screen.getByTestId("linked-badge");
    expect(badge).toHaveTextContent("Kate's Obelo");
    expect(badge.querySelector("svg")).not.toBeNull();
    // Exactly one mark — the local shelf beside it wears none.
    expect(screen.getAllByTestId("linked-badge")).toHaveLength(1);
  });

  it("shows no note for a linked server when nothing here came over a Link", async () => {
    getUser.mockResolvedValue(detail({ role: "remote" }));
    renderDialog(usr({ role: "remote" }));

    await screen.findByTestId("library-checklist");
    expect(screen.queryByTestId("linked-not-grantable")).toBeNull();
    expect(screen.getByTestId("library-checkbox-l3")).toBeInTheDocument();
  });

  it("never sends a linked Library, even if one was granted behind the API", async () => {
    const user = userEvent.setup();
    listLibraries.mockResolvedValue(WITH_MIRROR);
    // A grant row from before the rule (or written straight into the database).
    getUser.mockResolvedValue(detail({ role: "remote", libraryIds: ["l1", "l9"] }));
    renderDialog(usr({ role: "remote" }));

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledTimes(1));
    expect(setLibraryAccess).toHaveBeenCalledWith("u2", ["l1"]);
  });

  it("says the server has none of its own when every Library is somebody else's", async () => {
    listLibraries.mockResolvedValue([
      { ...lib("l9", "Their Films"), linked: true, available: true },
    ]);
    getUser.mockResolvedValue(detail({ role: "remote" }));
    renderDialog(usr({ role: "remote" }));

    expect(await screen.findByTestId("library-access-empty")).toHaveTextContent(
      /own/i,
    );
    expect(screen.getByTestId("linked-not-grantable")).toBeInTheDocument();
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

describe("EditUserDialog — playback ceiling (non-Admin)", () => {
  it("offers 720p/1080p/2160p + No limit and preselects the stored ceiling", async () => {
    getUser.mockResolvedValue(
      detail({ maxResolution: "1080p", maxBitrate: 8_000_000, maxStreams: 2 }),
    );
    renderDialog(usr({}));

    const select = (await screen.findByTestId(
      "max-resolution-select",
    )) as HTMLSelectElement;
    expect([...select.options].map((o) => o.textContent)).toEqual([
      "No limit",
      "720p",
      "1080p",
      "2160p",
    ]);
    expect(select.value).toBe("1080p");
    // Bits/sec on the wire, Mbps in the field.
    expect(screen.getByTestId("max-bitrate-input")).toHaveValue(8);
    expect(screen.getByTestId("max-streams-input")).toHaveValue(2);
  });

  it("shows an uncapped User's fields empty, not zeroed", async () => {
    getUser.mockResolvedValue(detail({}));
    renderDialog(usr({}));

    expect(
      (await screen.findByTestId("max-resolution-select")) as HTMLSelectElement,
    ).toHaveValue("");
    expect(screen.getByTestId("max-bitrate-input")).toHaveValue(null);
    expect(screen.getByTestId("max-streams-input")).toHaveValue(null);
  });

  it("saves the WHOLE ceiling, converting Mbps to bits/sec", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({}));
    renderDialog(usr({}));

    await user.selectOptions(
      await screen.findByTestId("max-resolution-select"),
      "1080p",
    );
    await user.type(screen.getByTestId("max-bitrate-input"), "8");
    await user.type(screen.getByTestId("max-streams-input"), "2");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() =>
      expect(setPlaybackCeiling).toHaveBeenCalledWith("u2", {
        maxResolution: "1080p",
        maxBitrate: 8_000_000,
        maxStreams: 2,
      }),
    );
  });

  it("clears a dimension by emptying its field (the body is a replace)", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(
      detail({ maxResolution: "720p", maxBitrate: 4_000_000, maxStreams: 1 }),
    );
    renderDialog(usr({}));

    await user.clear(await screen.findByTestId("max-streams-input"));
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() =>
      expect(setPlaybackCeiling).toHaveBeenCalledWith("u2", {
        maxResolution: "720p",
        maxBitrate: 4_000_000,
        maxStreams: 0,
      }),
    );
  });

  it("sends nothing when the ceiling is untouched", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({ maxStreams: 2 }));
    renderDialog(usr({}));

    await screen.findByTestId("max-streams-input");
    await user.type(screen.getByTestId("new-password-input"), "hunter2");
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setPassword).toHaveBeenCalled());
    expect(setPlaybackCeiling).not.toHaveBeenCalled();
  });

  it("renders for the `remote` role — the case the ceiling exists for", async () => {
    getUser.mockResolvedValue(detail({ role: "remote" }));
    renderDialog(usr({ role: "remote" }));

    expect(await screen.findByTestId("playback-ceiling")).toBeInTheDocument();
    expect(screen.getByTestId("max-streams-input")).toBeInTheDocument();
  });

  it("offers no ceiling control for an Admin", async () => {
    renderDialog(usr({ role: "admin" }));

    expect(await screen.findByTestId("admin-all-libraries")).toBeInTheDocument();
    expect(screen.queryByTestId("playback-ceiling")).not.toBeInTheDocument();
  });

  it("surfaces an UNKNOWN_RESOLUTION refusal inline, keeping the dialog", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(detail({}));
    setPlaybackCeiling.mockRejectedValue(
      new ApiError(422, "UNKNOWN_RESOLUTION", "1440p is not a settable rung"),
    );
    renderDialog(usr({}));

    await user.selectOptions(
      await screen.findByTestId("max-resolution-select"),
      "720p",
    );
    await user.click(screen.getByTestId("edit-user-save"));

    expect(await screen.findByTestId("edit-user-error")).toHaveTextContent(
      /not a settable rung/i,
    );
    expect(screen.getByTestId("max-resolution-select")).toBeInTheDocument();
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

// --- The Link section (.scratch/linked-servers issue 04, ADR-0055 §2) -------
//
// A `remote` User is a linked Server, not a person. Its dialog is where the
// sharing Admin turns "I granted them two libraries" into the one string they
// send over iMessage.

/** A running Tailnet node serving plain HTTP on :80 — the common case, since
 * tailnet HTTPS additionally needs two things enabled in the Tailscale console. */
function tailnetView(over: Partial<TailnetStatus> = {}): TailnetSettingsView {
  return {
    enabled: true,
    hostname: "obelo",
    controlURL: "",
    httpsEnabled: false,
    status: {
      state: "running",
      fqdn: "obelo.tail1a2b.ts.net",
      keyExpiry: null,
      httpsBound: false,
      ...over,
    },
  };
}

const INVITE = {
  invite:
    "obelo-link:eyJ2IjoxLCJpZCI6InNydi0xIiwibmFtZSI6IkhvbWUiLCJvcmlnaW5z" +
    "IjpbImh0dHBzOi8vbWVkaWEuZXhhbXBsZS5vcmciXSwiY29kZSI6ImFiYyIsImV4cCI6" +
    "IjIwMjYtMDktMDNUMTI6MDA6MDBaIn0",
  expiresAt: "2026-09-03T12:00:00Z",
};

function remoteUser(): User {
  return { id: "u9", username: "Brandon's server", role: "remote" };
}
function remoteDetail(over: Partial<UserDetail> = {}): UserDetail {
  return detail({ id: "u9", username: "Brandon's server", role: "remote", ...over });
}

describe("EditUserDialog — a linked server's invite", () => {
  it("offers no password field, and says why", async () => {
    getUser.mockResolvedValue(remoteDetail());
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    // The role carries no password at all — the schema refuses to give it one —
    // so a "New password" box would be a field that cannot do anything.
    expect(screen.queryByTestId("new-password-input")).not.toBeInTheDocument();
    expect(screen.getByTestId("remote-no-password")).toBeInTheDocument();
    // The grants and the Playback ceiling are still here: they are what the
    // other household is actually allowed to see and how it may play it.
    expect(await screen.findByTestId("library-checklist")).toBeInTheDocument();
    expect(screen.getByTestId("playback-ceiling")).toBeInTheDocument();
  });

  it("shows no Link section for a Member or an Admin", async () => {
    getUser.mockResolvedValue(detail({}));
    const { unmount } = renderDialog(usr({}));
    await screen.findByTestId("library-checklist");
    expect(screen.queryByTestId("link-section")).not.toBeInTheDocument();
    expect(screen.getByTestId("new-password-input")).toBeInTheDocument();
    unmount();

    renderDialog(usr({ role: "admin" }));
    expect(screen.queryByTestId("link-section")).not.toBeInTheDocument();
    // And an Admin dialog never asks the Tailnet anything.
    expect(getTailnet).not.toHaveBeenCalled();
  });

  it("pre-fills the MagicDNS origin with the scheme the node ACHIEVED", async () => {
    getUser.mockResolvedValue(remoteDetail());
    getTailnet.mockResolvedValue(tailnetView());
    renderDialog(remoteUser());

    await waitFor(() =>
      expect(screen.getByTestId("origin-input-0")).toHaveValue(
        "http://obelo.tail1a2b.ts.net",
      ),
    );
    // Plus the free-text row for a public address, left blank.
    expect(screen.getByTestId("origin-input-1")).toHaveValue("");
  });

  it("uses https for the pre-filled origin only once :443 is actually bound", async () => {
    // httpsEnabled is the operator's REQUEST; httpsBound is what the node
    // achieved. Reading the request as the outcome is what put a confident
    // https:// address in front of an operator whose :443 refused connections.
    getUser.mockResolvedValue(remoteDetail());
    getTailnet.mockResolvedValue({
      ...tailnetView({ httpsBound: true }),
      httpsEnabled: true,
    });
    renderDialog(remoteUser());

    await waitFor(() =>
      expect(screen.getByTestId("origin-input-0")).toHaveValue(
        "https://obelo.tail1a2b.ts.net",
      ),
    );
  });

  it("leaves one blank row when there is no Tailnet, and says the server cannot know its public address", async () => {
    getUser.mockResolvedValue(remoteDetail());
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    expect(screen.getByTestId("origin-input-0")).toHaveValue("");
    expect(screen.queryByTestId("origin-input-1")).not.toBeInTheDocument();
    expect(screen.getByTestId("link-section")).toHaveTextContent(
      /cannot know its own public address/i,
    );
    // A 503 from a build with no Tailnet support is not an error worth showing.
    expect(screen.queryByTestId("invite-error")).not.toBeInTheDocument();
  });

  it("cannot generate an invite with no address (an invite with none could only fail on the other machine)", async () => {
    getUser.mockResolvedValue(remoteDetail());
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    expect(screen.getByTestId("generate-invite")).toBeDisabled();
  });

  it("mints an invite from the typed origins, in order, and shows the string, its QR and the expiry", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    getTailnet.mockResolvedValue(tailnetView());
    createLinkInvite.mockResolvedValue(INVITE);
    renderDialog(remoteUser());

    await waitFor(() =>
      expect(screen.getByTestId("origin-input-0")).toHaveValue(
        "http://obelo.tail1a2b.ts.net",
      ),
    );
    await user.type(
      screen.getByTestId("origin-input-1"),
      "  https://media.example.org  ",
    );
    await user.click(screen.getByTestId("generate-invite"));

    // Trimmed, blanks dropped, ORDER PRESERVED — the redeeming Server tries them
    // in the order given (ADR-0055 §2).
    await waitFor(() =>
      expect(createLinkInvite).toHaveBeenCalledWith("u9", [
        "http://obelo.tail1a2b.ts.net",
        "https://media.example.org",
      ]),
    );

    const field = await screen.findByTestId("invite-string");
    expect(field).toHaveValue(INVITE.invite);
    expect(field).toHaveAttribute("readonly");
    expect(screen.getByTestId("invite-qr")).toBeInTheDocument();
    expect(screen.getByTestId("invite-expiry")).toHaveTextContent(/Expires/);
    expect(screen.getByTestId("invite-warning")).toHaveTextContent(
      /link once, within 24 hours/i,
    );
  });

  it("encodes the QR from EXACTLY the string in the field", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    createLinkInvite.mockResolvedValue(INVITE);
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    await user.type(screen.getByTestId("origin-input-0"), "https://media.example.org");
    await user.click(screen.getByTestId("generate-invite"));

    const svg = await screen.findByTestId("invite-qr");
    // The encoder's own round-trip is pinned in src/lib/qr.test.ts; what matters
    // here is that the symbol drawn is the one for the string on screen, and
    // that the two cannot drift apart.
    const { encodeQr } = await import("../lib/qr");
    const expected = encodeQr(INVITE.invite);
    expect(svg.getAttribute("data-qr-modules")).toBe(String(expected.size));
    expect(svg).toHaveAttribute("viewBox", `0 0 ${expected.size + 8} ${expected.size + 8}`);
    expect(svg.querySelector("path")?.getAttribute("d")).toBe(
      qrPath(expected.modules),
    );
  });

  it("adds and removes origin rows", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    createLinkInvite.mockResolvedValue(INVITE);
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    // A lone row has no remove control — removing the only address would leave
    // an invite nothing could redeem.
    expect(screen.queryByTestId("origin-remove-0")).not.toBeInTheDocument();

    await user.click(screen.getByTestId("origin-add"));
    await user.type(screen.getByTestId("origin-input-0"), "https://a.example");
    await user.type(screen.getByTestId("origin-input-1"), "https://b.example");
    await user.click(screen.getByTestId("origin-remove-0"));

    expect(screen.getByTestId("origin-input-0")).toHaveValue("https://b.example");
    await user.click(screen.getByTestId("generate-invite"));
    await waitFor(() =>
      expect(createLinkInvite).toHaveBeenCalledWith("u9", ["https://b.example"]),
    );
  });

  it("replaces the shown invite when a new one is generated", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    createLinkInvite
      .mockResolvedValueOnce(INVITE)
      .mockResolvedValue({ invite: "obelo-link:second", expiresAt: INVITE.expiresAt });
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    await user.type(screen.getByTestId("origin-input-0"), "https://media.example.org");
    await user.click(screen.getByTestId("generate-invite"));
    expect(await screen.findByTestId("invite-string")).toHaveValue(INVITE.invite);

    // The button restates what pressing it again does: the previous invite dies
    // server-side the moment this one is minted.
    expect(screen.getByTestId("generate-invite")).toHaveTextContent(
      "Generate a new invite",
    );
    await user.click(screen.getByTestId("generate-invite"));
    await waitFor(() =>
      expect(screen.getByTestId("invite-string")).toHaveValue("obelo-link:second"),
    );
  });

  it("copies the string to the clipboard", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    getUser.mockResolvedValue(remoteDetail());
    createLinkInvite.mockResolvedValue(INVITE);
    renderDialog(remoteUser());

    await screen.findByTestId("link-section");
    await user.type(screen.getByTestId("origin-input-0"), "https://media.example.org");
    await user.click(screen.getByTestId("generate-invite"));
    await screen.findByTestId("invite-string");

    await user.click(screen.getByTestId("invite-copy"));
    expect(writeText).toHaveBeenCalledWith(INVITE.invite);
    await waitFor(() =>
      expect(screen.getByTestId("invite-copy")).toHaveTextContent("Copied"),
    );
  });

  it("surfaces a refused mint inline and keeps the dialog open", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    createLinkInvite.mockRejectedValue(
      new ApiError(422, "INVALID_ORIGIN", "origins must be absolute http(s) origins"),
    );
    const onClose = vi.fn();
    renderDialog(remoteUser(), { onClose });

    await screen.findByTestId("link-section");
    await user.type(screen.getByTestId("origin-input-0"), "media.example.org");
    await user.click(screen.getByTestId("generate-invite"));

    expect(await screen.findByTestId("invite-error")).toHaveTextContent(
      /absolute http\(s\) origins/,
    );
    expect(screen.queryByTestId("invite-result")).not.toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("does not mint as a side effect of saving the grants", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    renderDialog(remoteUser());

    await screen.findByTestId("library-checklist");
    await user.click(screen.getByTestId("library-checkbox-l1"));
    await user.click(screen.getByTestId("edit-user-save"));

    await waitFor(() => expect(setLibraryAccess).toHaveBeenCalledWith("u9", ["l1"]));
    expect(createLinkInvite).not.toHaveBeenCalled();
  });

  it("holds the dialog shut while a mint is in flight", async () => {
    const user = userEvent.setup();
    getUser.mockResolvedValue(remoteDetail());
    const pending = deferred<typeof INVITE>();
    createLinkInvite.mockReturnValue(pending.promise);
    const onClose = vi.fn();
    renderDialog(remoteUser(), { onClose });

    await screen.findByTestId("link-section");
    await user.type(screen.getByTestId("origin-input-0"), "https://media.example.org");
    await user.click(screen.getByTestId("generate-invite"));

    // The string exists only in that one response, and the invite it replaced is
    // already dead — closing now would strand both households.
    expect(screen.getByTestId("generate-invite")).toHaveTextContent("Generating…");
    expect(screen.getByTestId("edit-user-cancel")).toBeDisabled();
    expect(screen.getByTestId("edit-user-close-x")).toBeDisabled();

    pending.resolve(INVITE);
    expect(await screen.findByTestId("invite-string")).toHaveValue(INVITE.invite);
    expect(screen.getByTestId("edit-user-cancel")).not.toBeDisabled();
    expect(onClose).not.toHaveBeenCalled();
  });
});

/** The same one-path-of-1x1-boxes QrSvg draws, so the assertion above compares
 * the drawing to the encoder rather than to itself. */
function qrPath(modules: boolean[][]): string {
  let d = "";
  for (let y = 0; y < modules.length; y++) {
    for (let x = 0; x < modules.length; x++) {
      if (modules[y][x]) d += `M${x + 4} ${y + 4}h1v1h-1z`;
    }
  }
  return d;
}
