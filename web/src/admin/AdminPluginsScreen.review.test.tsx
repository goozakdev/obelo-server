import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type { InstalledPlugin, InstalledPluginsView } from "../api/types";

// Review fixes on the Plugins screen: a refused install/pin keeps what the Admin
// typed (R02-03), a closed dialog forgets its uninstall confirmation (R02-13), the
// Lyric order card follows installs on the same screen (R02-14), and the three
// loads run side by side (R02-17).

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  disablePlugin: vi.fn(),
  getPluginUninstallPreview: vi.fn(),
  getPluginCatalog: vi.fn(),
  getPluginPublishers: vi.fn(),
  pinPluginPublisher: vi.fn(),
  getLyricProviders: vi.fn(),
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

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
  client.getPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
  client.getPluginPublishers.mockResolvedValue({ publishers: [] });
  client.getLyricProviders.mockResolvedValue({ providers: [] });
});

describe("the Plugins screen — review fixes", () => {
  it("keeps the pasted URL when the install is refused (R02-03)", async () => {
    client.getPlugins.mockResolvedValue(view());
    client.installPluginFromURL.mockRejectedValue(
      new ApiError(400, "URL_REFUSED", "that address is on this network"),
    );
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-url"), "http://10.0.0.5/p.zip");
    await userEvent.click(screen.getByTestId("plugin-install-from-url"));

    expect(await screen.findByTestId("plugins-action-error")).toBeTruthy();
    expect(screen.getByTestId("plugin-url")).toHaveValue("http://10.0.0.5/p.zip");
  });

  it("clears the pasted URL once the install succeeds (R02-03)", async () => {
    client.getPlugins.mockResolvedValue(view());
    client.installPluginFromURL.mockResolvedValue(view(plugin()));
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-url"), "https://example.com/p.zip");
    await userEvent.click(screen.getByTestId("plugin-install-from-url"));

    await screen.findByTestId("plugin-directory");
    expect(screen.getByTestId("plugin-url")).toHaveValue("");
  });

  it("keeps the publisher name and key when the pin is refused (R02-03)", async () => {
    client.getPlugins.mockResolvedValue(view());
    client.pinPluginPublisher.mockRejectedValue(
      new ApiError(400, "BAD_KEY", "that is not a valid key"),
    );
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-publisher-name"), "acme");
    await userEvent.type(screen.getByTestId("plugin-publisher-key"), "abc123");
    await userEvent.click(screen.getByTestId("plugin-publisher-pin"));

    expect(await screen.findByTestId("plugins-action-error")).toBeTruthy();
    expect(screen.getByTestId("plugin-publisher-name")).toHaveValue("acme");
    expect(screen.getByTestId("plugin-publisher-key")).toHaveValue("abc123");
  });

  it("forgets an open uninstall confirmation when the dialog closes (R02-13)", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.getPluginUninstallPreview.mockResolvedValue({
      signInProvider: true,
      usersToDelete: [{ id: "u-ada", username: "ada" }],
    });
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-directory"));
    await userEvent.click(screen.getByTestId("plugin-uninstall-directory"));
    await screen.findByTestId("plugin-uninstall-confirmation-directory");
    await userEvent.click(screen.getByTestId("plugin-dialog-close-x-directory"));

    await userEvent.click(screen.getByTestId("plugin-edit-directory"));
    expect(screen.queryByTestId("plugin-uninstall-confirmation-directory")).toBeNull();
  });

  it("refetches the Lyric provider order after a change on this screen (R02-14)", async () => {
    client.getPlugins.mockResolvedValue(view(plugin({ provides: ["lyric-provider"] })));
    client.disablePlugin.mockResolvedValue(
      view(plugin({ provides: ["lyric-provider"], enabled: false })),
    );
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await waitFor(() => expect(client.getLyricProviders).toHaveBeenCalledTimes(1));

    await userEvent.click(screen.getByTestId("plugin-edit-directory"));
    await userEvent.click(screen.getByTestId("plugin-disable-directory"));

    await waitFor(() => expect(client.getLyricProviders).toHaveBeenCalledTimes(2));
  });

  it("asks for the plugins, catalog and publishers without waiting on one another (R02-17)", async () => {
    client.getPlugins.mockReturnValue(new Promise(() => {}));
    render(<AdminPluginsScreen />);

    await waitFor(() => expect(client.getPluginCatalog).toHaveBeenCalled());
    expect(client.getPluginPublishers).toHaveBeenCalled();
  });
});
