import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { InstalledPlugin, InstalledPluginsView } from "../api/types";

// The Plugins admin screen (ADR-0058, plugin-system/10).
//
// What is worth asserting here is not that a list renders. It is that the screen
// keeps apart the two things an Admin will otherwise conflate — THE SWITCH THEY
// FLIPPED and THIS SERVER REFUSING TO CALL THE PLUGIN — and that a refusal from
// the server reaches the screen as the server's own sentence rather than as
// "something went wrong". Those messages are the entire value of five distinct
// refusal codes on the way in.

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  enablePlugin: vi.fn(),
  disablePlugin: vi.fn(),
  reenablePlugin: vi.fn(),
  uninstallPlugin: vi.fn(),
  // The catalog and the pinned keys (plugin-system/15). Every test in THIS file
  // leaves them rejecting, which is the point: with neither configured the screen
  // must be exactly what it was before they existed, so nothing below changed
  // when they arrived. The Browse tab and the key pinning have their own file.
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
    id: "example-sink",
    name: "Example Sink",
    version: "1.2.0",
    apiVersion: 1,
    provides: ["event-sink"],
    enabled: true,
    disabledByFailure: false,
    source: "upload",
    installedAt: "2026-09-17T10:00:00Z",
    ...over,
  };
}

function view(...plugins: InstalledPlugin[]): InstalledPluginsView {
  return { plugins };
}

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
  // No catalog and no pinned keys — the shipped default.
  client.getPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
  client.getPluginPublishers.mockResolvedValue({ publishers: [] });
});

describe("the Plugins screen", () => {
  it("names what an installed plugin provides, and where it came from", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-example-sink")).toBeTruthy();
    expect(screen.getByTestId("plugin-version-example-sink").textContent).toContain("1.2.0");
    expect(screen.getByTestId("plugin-status-example-sink").textContent).toBe("Running");
    // The rest — provides, source — lives in the dialog, behind the edit button.
    expect(screen.queryByTestId("plugin-provides-example-sink")).toBeNull();
    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    // The contract's token becomes the operator's word for it.
    expect(screen.getByTestId("plugin-provides-example-sink").textContent).toContain(
      "Event sink",
    );
    expect(screen.getByTestId("plugin-source-example-sink").textContent).toBe("Uploaded");
  });

  it("says so when nothing is installed", async () => {
    client.getPlugins.mockResolvedValue(view());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugins-empty")).toBeTruthy();
  });

  // The distinction the whole screen exists to keep straight. A plugin that is
  // switched ON and that this server has STOPPED CALLING is not "off" — it is a
  // thing the Admin has to be told about, with the error and a way to retry.
  it("distinguishes a plugin the Admin switched off from one this server stopped", async () => {
    client.getPlugins.mockResolvedValue(
      view(
        plugin({ id: "off-by-admin", name: "Off By Admin", enabled: false }),
        plugin({
          id: "stopped",
          name: "Stopped",
          enabled: true,
          disabledByFailure: true,
          lastError: "the module trapped: unreachable",
        }),
      ),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-status-off-by-admin").textContent).toBe("Switched off");
    // Switched off offers Enable, and nothing to forgive.
    await userEvent.click(screen.getByTestId("plugin-edit-off-by-admin"));
    expect(screen.getByTestId("plugin-enable-off-by-admin")).toBeTruthy();
    expect(screen.queryByTestId("plugin-reenable-off-by-admin")).toBeNull();

    expect(screen.getByTestId("plugin-status-stopped").textContent).toBe(
      "Stopped by this server",
    );
    await userEvent.click(screen.getByTestId("plugin-edit-stopped"));
    expect(screen.getByTestId("plugin-error-stopped").textContent).toContain(
      "the module trapped",
    );
    // It is still switched on, so the switch still reads Disable — and there is a
    // Re-enable, because there is something to forgive.
    expect(screen.getByTestId("plugin-disable-stopped")).toBeTruthy();
    expect(screen.getByTestId("plugin-reenable-stopped")).toBeTruthy();
  });

  // The dialog sits in front of the row it belongs to, so an action's own outcome
  // has to land in front of it too — not on the page behind, where the Admin who
  // just clicked Disable would never see it.
  it("shows a dialog action's error inside the dialog, not behind it", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.disablePlugin.mockRejectedValue(new Error("the server refused to disable it"));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    await userEvent.click(screen.getByTestId("plugin-disable-example-sink"));

    const dialog = screen.getByTestId("plugin-dialog-example-sink");
    const shown = await screen.findByTestId("plugins-action-error");
    expect(dialog.contains(shown)).toBe(true);
    expect(shown.textContent).toContain("the server refused to disable it");
  });

  it("re-renders from the response a verb returns, with no second fetch", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.disablePlugin.mockResolvedValue(view(plugin({ enabled: false })));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    await userEvent.click(screen.getByTestId("plugin-disable-example-sink"));

    expect(client.disablePlugin).toHaveBeenCalledWith("example-sink");
    expect(client.getPlugins).toHaveBeenCalledTimes(1);
    await screen.findByTestId("plugin-enable-example-sink");
    expect(screen.getByTestId("plugin-status-example-sink").textContent).toBe("Switched off");
  });

  it("uninstalls the plugin the button belongs to", async () => {
    client.getPlugins.mockResolvedValue(view(plugin(), plugin({ id: "other", name: "Other" })));
    client.uninstallPlugin.mockResolvedValue(view(plugin({ id: "other", name: "Other" })));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    await userEvent.click(screen.getByTestId("plugin-uninstall-example-sink"));

    expect(client.uninstallPlugin).toHaveBeenCalledWith("example-sink");
    expect(screen.queryByTestId("plugin-example-sink")).toBeNull();
    // The dialog closes with the plugin it was open for (D4).
    expect(screen.queryByTestId("plugin-dialog-example-sink")).toBeNull();
    expect(screen.getByTestId("plugin-other")).toBeTruthy();
  });

  it("sends both files as one install", async () => {
    client.getPlugins.mockResolvedValue(view());
    client.installPlugin.mockResolvedValue(view(plugin()));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    const manifest = new File(['{"id":"example-sink"}'], "manifest.json", {
      type: "application/json",
    });
    const module = new File([new Uint8Array([0, 97, 115, 109])], "plugin.wasm", {
      type: "application/wasm",
    });
    await userEvent.upload(screen.getByTestId("plugin-manifest-file"), manifest);
    await userEvent.upload(screen.getByTestId("plugin-module-file"), module);
    await userEvent.click(screen.getByTestId("plugin-upload"));

    // The third argument is the OPTIONAL signature part (plugin-system/15), and
    // it is undefined here because this form does not insist on one: most plugins
    // are unsigned, and whether this server needs one is the server's question.
    expect(client.installPlugin).toHaveBeenCalledWith(manifest, module, undefined);
    await screen.findByTestId("plugin-example-sink");
  });

  // Half a plugin is not a plugin, and the screen says which half is missing
  // before it wastes a round trip on it.
  it("refuses to upload without both files", async () => {
    client.getPlugins.mockResolvedValue(view());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-upload"));

    expect(client.installPlugin).not.toHaveBeenCalled();
    expect(screen.getByTestId("plugins-action-error").textContent).toContain("both");
  });

  it("installs from a pasted manifest URL", async () => {
    client.getPlugins.mockResolvedValue(view());
    client.installPluginFromURL.mockResolvedValue(view(plugin()));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(
      screen.getByTestId("plugin-url"),
      "https://example.com/p/manifest.json",
    );
    await userEvent.click(screen.getByTestId("plugin-install-from-url"));

    expect(client.installPluginFromURL).toHaveBeenCalledWith({
      url: "https://example.com/p/manifest.json",
    });
    await screen.findByTestId("plugin-example-sink");
  });

  // The server distinguishes five refusals so that an Admin is told which one
  // happened. That is only worth anything if the sentence reaches the screen.
  it("shows the server's own refusal, word for word", async () => {
    client.getPlugins.mockResolvedValue(view());
    const { ApiError } = await vi.importActual<typeof import("../api/client")>(
      "../api/client",
    );
    client.installPluginFromURL.mockRejectedValue(
      new ApiError(
        422,
        "PLUGIN_API_VERSION",
        "this server speaks plugin API v1; the plugin needs v2 — upgrade the server",
      ),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-url"), "https://example.com/manifest.json");
    await userEvent.click(screen.getByTestId("plugin-install-from-url"));

    const shown = await screen.findByTestId("plugins-action-error");
    expect(shown.textContent).toContain("upgrade the server");
  });

  // plugin-system/13. The panel belongs to the plugin whose manifest declared it,
  // so it lives inside that card — and a plugin that declares nothing gets no
  // panel rather than an empty one.
  it("puts a plugin's own declared settings inside its card, and only when it has some", async () => {
    client.getPlugins.mockResolvedValue(
      view(
        plugin({
          settingsSchema: [{ key: "region", type: "enum", label: "Region", options: ["eu", "us"] }],
          settings: { values: { region: "eu" }, secrets: {} },
        }),
        plugin({ id: "plain-sink", name: "Plain Sink" }),
      ),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    const dialog = screen.getByTestId("plugin-dialog-example-sink");
    expect(dialog.querySelector('[data-testid="plugin-settings-example-sink"]')).toBeTruthy();
    expect(
      (screen.getByTestId("plugin-field-example-sink-region") as HTMLSelectElement).value,
    ).toBe("eu");

    await userEvent.click(screen.getByTestId("plugin-edit-plain-sink"));
    expect(screen.queryByTestId("plugin-settings-plain-sink")).toBeNull();
  });
});

// plugins-list-dialog: the Installed tab is a compact list — name, version,
// status, edit — with everything else (provides, source, the settings form,
// every action) behind the pencil, in a dialog.
describe("the Installed tab's list and dialog", () => {
  it("shows only name, version, status and an edit button until the dialog is opened", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-version-example-sink")).toBeTruthy();
    expect(screen.getByTestId("plugin-status-example-sink")).toBeTruthy();
    expect(screen.getByTestId("plugin-edit-example-sink")).toBeTruthy();
    expect(screen.queryByTestId("plugin-provides-example-sink")).toBeNull();
    expect(screen.queryByTestId("plugin-uninstall-example-sink")).toBeNull();

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));

    expect(screen.getByTestId("plugin-provides-example-sink")).toBeTruthy();
    expect(screen.getByTestId("plugin-uninstall-example-sink")).toBeTruthy();
  });

  it("shows a declined row as Removed, whose dialog offers only reinstall", async () => {
    client.getPlugins.mockResolvedValue(
      view(plugin({ state: "declined", version: undefined, source: undefined })),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-status-example-sink").textContent).toBe("Removed");
    expect(screen.queryByTestId("plugin-version-example-sink")).toBeNull();

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));

    expect(screen.getByTestId("plugin-reinstall-shipped-example-sink")).toBeTruthy();
    expect(screen.queryByTestId("plugin-enable-example-sink")).toBeNull();
    expect(screen.queryByTestId("plugin-disable-example-sink")).toBeNull();
    expect(screen.queryByTestId("plugin-uninstall-example-sink")).toBeNull();
  });

  it("closes the dialog once its plugin's uninstall succeeds", async () => {
    client.getPlugins.mockResolvedValue(view(plugin()));
    client.uninstallPlugin.mockResolvedValue(view());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    expect(screen.getByTestId("plugin-dialog-example-sink")).toBeTruthy();

    await userEvent.click(screen.getByTestId("plugin-uninstall-example-sink"));

    expect(screen.queryByTestId("plugin-dialog-example-sink")).toBeNull();
  });
});
