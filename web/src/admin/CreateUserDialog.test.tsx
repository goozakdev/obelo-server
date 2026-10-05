import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";

// Once the create succeeded the User exists, so EVERY way of dismissing the dialog
// has to report it (R02-04). Otherwise the hub never reloads and the new user is
// missing from the list.

const { createUser, listLibraries, setLibraryAccess } = vi.hoisted(() => ({
  createUser: vi.fn(),
  listLibraries: vi.fn(),
  setLibraryAccess: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      createUser: (...a: unknown[]) => createUser(...a),
      listLibraries: (...a: unknown[]) => listLibraries(...a),
      setLibraryAccess: (...a: unknown[]) => setLibraryAccess(...a),
    },
  };
});

import CreateUserDialog from "./CreateUserDialog";

beforeEach(() => {
  createUser.mockReset();
  listLibraries.mockReset();
  setLibraryAccess.mockReset();
});

async function createWithFailedGrant(onCreated: () => void, onClose: () => void) {
  createUser.mockResolvedValue({ id: "u2", username: "ada", role: "member" });
  listLibraries.mockResolvedValue([{ id: "l1", name: "Movies", kind: "movie", rootFolders: [] }]);
  setLibraryAccess.mockRejectedValue(new ApiError(422, "UNKNOWN_LIBRARY", "nope"));
  render(<CreateUserDialog onCreated={onCreated} onClose={onClose} />);
  await userEvent.type(screen.getByTestId("user-username-input"), "ada");
  await userEvent.type(screen.getByTestId("user-password-input"), "pw");
  await userEvent.click(screen.getByTestId("create-user-submit"));
  await screen.findByTestId("create-user-error");
}

describe("CreateUserDialog — dismissing after a partial create", () => {
  it("reports the created user when dismissed with the X", async () => {
    const onCreated = vi.fn();
    const onClose = vi.fn();
    await createWithFailedGrant(onCreated, onClose);

    await userEvent.click(screen.getByTestId("create-user-close-x"));

    expect(onCreated).toHaveBeenCalledTimes(1);
    expect(onClose).not.toHaveBeenCalled();
  });

  it("reports the created user when dismissed with ESC", async () => {
    const onCreated = vi.fn();
    const onClose = vi.fn();
    await createWithFailedGrant(onCreated, onClose);

    await userEvent.keyboard("{Escape}");
    // jsdom does not turn ESC into a native cancel, so fire it the way a browser does.
    screen
      .getByTestId("create-user-dialog")
      .dispatchEvent(new Event("cancel", { bubbles: false, cancelable: true }));

    expect(onCreated).toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("still plain-closes when nothing was created", async () => {
    const onCreated = vi.fn();
    const onClose = vi.fn();
    render(<CreateUserDialog onCreated={onCreated} onClose={onClose} />);

    await userEvent.click(screen.getByTestId("create-user-close-x"));

    expect(onClose).toHaveBeenCalledTimes(1);
    expect(onCreated).not.toHaveBeenCalled();
  });
});
