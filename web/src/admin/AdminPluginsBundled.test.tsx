import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { InstalledPlugin, InstalledPluginsView } from "../api/types";

// The Plugins screen and the plugins this server SHIPS (ADR-0059,
// bundled-plugins/04).
//
// Two things an Admin has to be able to tell, and neither is visible anywhere
// else on the screen: which of these sources they chose and which arrived with
// the server, and — once they have removed one of the server's — that it is gone
// on purpose and how to get it back. Everything else about a bundled plugin is
// deliberately identical to any other, so there is nothing else here to assert.

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  enablePlugin: vi.fn(),
  disablePlugin: vi.fn(),
  reenablePlugin: vi.fn(),
  reinstallShippedPlugin: vi.fn(),
  uninstallPlugin: vi.fn(),
  getPluginCatalog: vi.fn(),
  getPluginPublishers: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import AdminPluginsScreen from "./AdminPluginsScreen";

function shipped(over: Partial<InstalledPlugin> = {}): InstalledPlugin {
  return {
    id: "tmdb",
    name: "The Movie Database (TMDB)",
    version: "1.0.0",
    apiVersion: 1,
    provides: ["metadata-provider"],
    enabled: true,
    disabledByFailure: false,
    origin: "bundled",
    source: "shipped with obelo",
    installedAt: "2026-09-18T10:00:00Z",
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
});

describe("the Plugins screen and the plugins the server ships", () => {
  it("says a bundled plugin shipped with Obelo, where an uploaded one names its upload", async () => {
    client.getPlugins.mockResolvedValue(
      view(
        shipped(),
        shipped({
          id: "example-sink",
          name: "Example Sink",
          origin: "admin",
          source: "upload",
          provides: ["event-sink"],
        }),
      ),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-source-tmdb").textContent).toBe("Shipped with Obelo");
    expect(screen.getByTestId("plugin-source-example-sink").textContent).toBe("Uploaded");
    // And in every other respect it is an ordinary plugin: the same controls.
    expect(screen.getByTestId("plugin-disable-tmdb")).toBeTruthy();
    expect(screen.getByTestId("plugin-uninstall-tmdb")).toBeTruthy();
  });

  it("offers the shipped version back on a declined row, and nothing else", async () => {
    client.getPlugins.mockResolvedValue(
      view(shipped({ state: "declined", version: undefined, source: undefined })),
    );
    client.reinstallShippedPlugin.mockResolvedValue(view(shipped()));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-status-tmdb").textContent).toContain("you removed it");
    // A declined plugin has nothing to enable, disable or uninstall.
    expect(screen.queryByTestId("plugin-disable-tmdb")).toBeNull();
    expect(screen.queryByTestId("plugin-enable-tmdb")).toBeNull();
    expect(screen.queryByTestId("plugin-uninstall-tmdb")).toBeNull();

    await userEvent.click(screen.getByTestId("plugin-reinstall-shipped-tmdb"));

    expect(client.reinstallShippedPlugin).toHaveBeenCalledWith("tmdb");
    // The verb answers with the whole new truth, so the row becomes an ordinary
    // installed plugin without a second request.
    expect(await screen.findByTestId("plugin-uninstall-tmdb")).toBeTruthy();
    expect(screen.getByTestId("plugin-source-tmdb").textContent).toBe("Shipped with Obelo");
  });
});
