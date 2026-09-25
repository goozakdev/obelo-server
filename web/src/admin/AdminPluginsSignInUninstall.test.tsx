import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type { InstalledPlugin, InstalledPluginsView } from "../api/types";

// Uninstalling a Sign-in provider (ADR-0063 decision 10) deletes every User left
// with no other way to sign in. The screen must never do that on one click: it
// asks the server who would be deleted, says plainly that they will be, lists
// them by name, and uninstalls only when the Admin confirms — carrying back
// exactly the Users it showed.

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  enablePlugin: vi.fn(),
  disablePlugin: vi.fn(),
  reenablePlugin: vi.fn(),
  uninstallPlugin: vi.fn(),
  getPluginUninstallPreview: vi.fn(),
  uninstallSignInProvider: vi.fn(),
  getPluginCatalog: vi.fn(),
  getPluginPublishers: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import AdminPluginsScreen from "./AdminPluginsScreen";

function plugin(over: Partial<InstalledPlugin> = {}): InstalledPlugin {
  return {
    id: "directory",
    name: "Directory",
    version: "1.0.0",
    apiVersion: 1,
    provides: ["sign-in-provider"],
    enabled: true,
    disabledByFailure: false,
    source: "upload",
    installedAt: "2026-09-25T10:00:00Z",
    ...over,
  };
}

function view(...plugins: InstalledPlugin[]): InstalledPluginsView {
  return { plugins };
}

const adaAndBob = {
  signInProvider: true,
  usersToDelete: [
    { id: "u-ada", username: "ada" },
    { id: "u-bob", username: "bob" },
  ],
};

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
  client.getPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
  client.getPluginPublishers.mockResolvedValue({ publishers: [] });
});

async function openConfirmation() {
  render(<AdminPluginsScreen />);
  await screen.findByTestId("plugins-screen");
  await userEvent.click(screen.getByTestId("plugin-uninstall-directory"));
  return screen.findByTestId("plugin-uninstall-confirmation-directory");
}

describe("uninstalling a Sign-in provider", () => {
  it("lists by name the users it will delete, says so plainly, and uninstalls nothing yet", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue(adaAndBob);

    const panel = await openConfirmation();

    expect(client.getPluginUninstallPreview).toHaveBeenCalledWith("directory");
    expect(panel.textContent).toMatch(/will be deleted/i);
    const names = screen
      .getAllByTestId("plugin-uninstall-user")
      .map((li) => li.textContent);
    expect(names).toEqual(["ada", "bob"]);
    expect(client.uninstallPlugin).not.toHaveBeenCalled();
    expect(client.uninstallSignInProvider).not.toHaveBeenCalled();
  });

  it("uninstalls nothing when the Admin cancels", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue(adaAndBob);

    await openConfirmation();
    await userEvent.click(screen.getByTestId("plugin-uninstall-cancel-directory"));

    expect(screen.queryByTestId("plugin-uninstall-confirmation-directory")).toBeNull();
    expect(screen.getByTestId("plugin-directory")).toBeTruthy();
    expect(client.uninstallPlugin).not.toHaveBeenCalled();
    expect(client.uninstallSignInProvider).not.toHaveBeenCalled();
  });

  it("confirms exactly the users it listed", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue(adaAndBob);
    client.uninstallSignInProvider.mockResolvedValue(view());

    await openConfirmation();
    await userEvent.click(screen.getByTestId("plugin-uninstall-confirm-directory"));

    expect(client.uninstallSignInProvider).toHaveBeenCalledWith("directory", ["u-ada", "u-bob"]);
    expect(await screen.findByTestId("plugins-empty")).toBeTruthy();
  });

  it("says nobody is deleted when everyone has another way in", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue({ signInProvider: true, usersToDelete: [] });

    const panel = await openConfirmation();

    expect(panel.textContent).toMatch(/no user will be deleted/i);
    expect(screen.queryAllByTestId("plugin-uninstall-user")).toHaveLength(0);
  });

  it("shows the new list when the server says it changed, and confirms that one", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue({
      signInProvider: true,
      usersToDelete: [{ id: "u-ada", username: "ada" }],
    });
    client.uninstallSignInProvider
      .mockRejectedValueOnce(
        new ApiError(409, "UNINSTALL_NOT_CONFIRMED", "confirm exactly the users listed", {
          usersToDelete: adaAndBob.usersToDelete,
        }),
      )
      .mockResolvedValueOnce(view());

    await openConfirmation();
    await userEvent.click(screen.getByTestId("plugin-uninstall-confirm-directory"));

    await screen.findByText("bob");
    expect(screen.getByTestId("plugin-directory")).toBeTruthy();
    await userEvent.click(screen.getByTestId("plugin-uninstall-confirm-directory"));
    expect(client.uninstallSignInProvider).toHaveBeenLastCalledWith("directory", ["u-ada", "u-bob"]);
  });

  it("leaves a plugin that is not a Sign-in provider to uninstall at once", async () => {
    client.getPlugins.mockResolvedValue(
      view(plugin({ id: "example-sink", name: "Example Sink", provides: ["event-sink"] })),
    );
    client.uninstallPlugin.mockResolvedValue(view());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-uninstall-example-sink"));

    expect(client.uninstallPlugin).toHaveBeenCalledWith("example-sink");
    expect(client.getPluginUninstallPreview).not.toHaveBeenCalled();
  });
});
