import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../api/errors";
import type { InstalledPlugin, PluginUpgradeStaged } from "../api/types";

// The Plugins screen's side of an in-place upgrade (ADR-0069, issue 08): the preview
// dialog an upload that widens answers with, its confirm and cancel, the refusals,
// and the "Upload new version" / "Update available" entry points.

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  confirmPluginUpgrade: vi.fn(),
  cancelPluginUpgrade: vi.fn(),
  enablePlugin: vi.fn(),
  disablePlugin: vi.fn(),
  reenablePlugin: vi.fn(),
  uninstallPlugin: vi.fn(),
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
    version: "1.0.0",
    apiVersion: 1,
    provides: ["event-sink"],
    enabled: true,
    disabledByFailure: false,
    source: "upload",
    origin: "admin",
    ...over,
  };
}

function staged(over: Partial<PluginUpgradeStaged["preview"]> = {}): PluginUpgradeStaged {
  return {
    id: "example-sink",
    name: "Example Sink",
    staged: "tok-123",
    expiresAt: "2026-10-08T12:10:00Z",
    preview: {
      from: "1.0.0",
      to: "1.1.0",
      claimedPublisher: "Example Publisher",
      keyId: "7e25d4a0a63758b7",
      authorUnconfirmed: false,
      hostsAdded: ["api.example.test"],
      hostsRemoved: ["old.example.test"],
      extensionPointsAdded: ["lyric-provider"],
      extensionPointsRemoved: ["event-sink"],
      socketGrantAdded: true,
      settingsDropped: [{ key: "mode", reason: "its type changed from string to integer" }],
      settingsDeleted: ["legacy"],
      ...over,
    },
  };
}

const zip = () => new File([new Uint8Array([80, 75, 3, 4])], "p.zip", { type: "application/zip" });

async function uploadGeneral() {
  await userEvent.upload(screen.getByTestId("plugin-package-file"), zip());
  await userEvent.click(screen.getByTestId("plugin-upload"));
}

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
  client.getPlugins.mockResolvedValue({ plugins: [plugin()] });
  client.getPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
  client.getPluginPublishers.mockResolvedValue({ publishers: [] });
});

describe("the upgrade preview", () => {
  it("lists every widening item, dropped and deleted settings, and applies nothing before Confirm", async () => {
    client.installPlugin.mockResolvedValue(staged());
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();

    const dialog = await screen.findByTestId("plugin-upgrade-dialog");
    expect(within(dialog).getByTestId("plugin-upgrade-versions").textContent).toContain("1.0.0 to 1.1.0");
    expect(within(dialog).getByTestId("plugin-upgrade-hosts-added").textContent).toContain("api.example.test");
    expect(within(dialog).getByTestId("plugin-upgrade-hosts-removed").textContent).toContain("old.example.test");
    expect(within(dialog).getByTestId("plugin-upgrade-points-added").textContent).toContain("Lyric provider");
    expect(within(dialog).getByTestId("plugin-upgrade-points-removed").textContent).toContain("Event sink");
    expect(within(dialog).getByTestId("plugin-upgrade-socket")).toBeTruthy();
    expect(within(dialog).getByTestId("plugin-upgrade-settings-dropped").textContent).toContain("mode");
    expect(within(dialog).getByTestId("plugin-upgrade-settings-dropped").textContent).toContain("type changed");
    expect(within(dialog).getByTestId("plugin-upgrade-settings-deleted").textContent).toContain("legacy");
    // An unpinned name is a claim, never shown as a fact.
    expect(within(dialog).getByTestId("plugin-upgrade-author").textContent).toContain("Claims to be Example Publisher");
    expect(within(dialog).getByTestId("plugin-upgrade-author").textContent).toContain("7e25d4a0a63758b7");

    expect(client.confirmPluginUpgrade).not.toHaveBeenCalled();
    expect(client.getPlugins).toHaveBeenCalledTimes(1);
    expect(screen.getByTestId("plugin-version-example-sink").textContent).toContain("1.0.0");
  });

  it("shows the author-cannot-be-confirmed warning only when the server says so", async () => {
    client.installPlugin.mockResolvedValue(staged({ authorUnconfirmed: true }));
    const first = render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();
    expect((await screen.findByTestId("plugin-upgrade-unconfirmed")).textContent).toContain(
      "author of this upgrade cannot be confirmed",
    );
    first.unmount();

    client.installPlugin.mockResolvedValue(staged({ authorUnconfirmed: false }));
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();
    await screen.findByTestId("plugin-upgrade-dialog");
    expect(screen.queryByTestId("plugin-upgrade-unconfirmed")).toBeNull();
  });

  it("confirms with the staged token only, and Cancel calls the cancel route", async () => {
    client.installPlugin.mockResolvedValue(staged());
    client.confirmPluginUpgrade.mockResolvedValue({
      ...plugin({ version: "1.1.0" }),
      upgrade: {
        from: "1.0.0",
        to: "1.1.0",
        settings: { kept: ["url"], added: ["token"], dropped: [{ key: "mode", reason: "type changed" }], deleted: ["legacy"], needsValue: [] },
      },
    });
    client.cancelPluginUpgrade.mockResolvedValue({ plugins: [plugin()] });
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");

    await uploadGeneral();
    await userEvent.click(await screen.findByTestId("plugin-upgrade-cancel"));
    expect(client.cancelPluginUpgrade).toHaveBeenCalledWith("example-sink", "tok-123");
    expect(client.confirmPluginUpgrade).not.toHaveBeenCalled();
    expect(screen.queryByTestId("plugin-upgrade-dialog")).toBeNull();

    await uploadGeneral();
    client.getPlugins.mockResolvedValue({ plugins: [plugin({ version: "1.1.0" })] });
    await userEvent.click(await screen.findByTestId("plugin-upgrade-confirm"));
    expect(client.confirmPluginUpgrade).toHaveBeenCalledWith("example-sink", "tok-123");
    expect(client.confirmPluginUpgrade.mock.calls[0]).toHaveLength(2);
    const notice = (await screen.findByTestId("plugins-notice")).textContent ?? "";
    expect(notice).toContain("Upgraded Example Sink from 1.0.0 to 1.1.0.");
    expect(notice).toContain("kept url");
    expect(notice).toContain("added token");
    expect(notice).toContain("dropped mode (type changed)");
    expect(notice).toContain("deleted legacy");
    expect(screen.queryByTestId("plugin-upgrade-dialog")).toBeNull();
    expect(screen.getByTestId("plugin-version-example-sink").textContent).toContain("1.1.0");
  });

  it("offers no Confirm once the server says the staged upgrade is gone", async () => {
    client.installPlugin.mockResolvedValue(staged());
    client.confirmPluginUpgrade.mockRejectedValue(
      new ApiError(409, "PLUGIN_UPGRADE_STAGED", "that staged upgrade has expired; upload the package again"),
    );
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();
    await userEvent.click(await screen.findByTestId("plugin-upgrade-confirm"));

    expect((await screen.findByTestId("plugin-upgrade-error")).textContent).toContain("expired");
    expect(screen.queryByTestId("plugin-upgrade-confirm")).toBeNull();
  });
});

describe("confirm-time refusals", () => {
  it("shows a non-STAGED refusal, offers no Confirm, and Cancel still calls the cancel route", async () => {
    client.installPlugin.mockResolvedValue(staged());
    client.confirmPluginUpgrade.mockRejectedValue(
      new ApiError(409, "PLUGIN_UPGRADE_PUBLISHER", "signed by another key; no key rotation"),
    );
    client.cancelPluginUpgrade.mockResolvedValue({ plugins: [plugin()] });
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();
    await userEvent.click(await screen.findByTestId("plugin-upgrade-confirm"));

    expect((await screen.findByTestId("plugin-upgrade-error")).textContent).toContain("no key rotation");
    expect(screen.queryByTestId("plugin-upgrade-confirm")).toBeNull();
    expect(screen.getByTestId("plugin-upgrade-expiry").textContent).not.toContain("no longer waiting");
    await userEvent.click(screen.getByTestId("plugin-upgrade-cancel"));
    expect(client.cancelPluginUpgrade).toHaveBeenCalledWith("example-sink", "tok-123");
  });
});

describe("a refused upgrade", () => {
  it("shows the dependent-state counts and offers no Confirm", async () => {
    client.installPlugin.mockRejectedValue(
      new ApiError(409, "PLUGIN_UPGRADE_DEPENDENTS", "this version drops sign-in-provider; uninstall example-sink to remove it", {
        identities: 3,
        users: 2,
      }),
    );
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();

    const shown = (await screen.findByTestId("plugins-action-error")).textContent ?? "";
    expect(shown).toContain("uninstall example-sink to remove it");
    expect(shown).toContain("3 sign-in identities");
    expect(shown).toContain("2 users");
    expect(screen.queryByTestId("plugin-upgrade-dialog")).toBeNull();
    expect(screen.queryByTestId("plugin-upgrade-confirm")).toBeNull();
  });

  it("shows a version refusal as the server's sentence", async () => {
    client.installPlugin.mockRejectedValue(new ApiError(409, "PLUGIN_UPGRADE_VERSION", "1.0.0 is not newer than 1.0.0"));
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await uploadGeneral();
    expect((await screen.findByTestId("plugins-action-error")).textContent).toContain("not newer");
    expect(screen.queryByTestId("plugin-upgrade-confirm")).toBeNull();
  });
});

describe("Upload new version", () => {
  it("is offered for admin and bundled origin and hidden on a declined row", async () => {
    client.getPlugins.mockResolvedValue({
      plugins: [
        plugin({ id: "mine", name: "Mine", origin: "admin" }),
        plugin({ id: "shipped", name: "Shipped", origin: "bundled" }),
        { id: "gone", name: "Gone", apiVersion: 1, provides: [], enabled: false, disabledByFailure: false, source: "", origin: "bundled", state: "declined" },
      ],
    });
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-mine");

    await userEvent.click(screen.getByTestId("plugin-edit-mine"));
    expect(screen.getByTestId("plugin-upgrade-mine")).toBeTruthy();
    await userEvent.click(screen.getByTestId("plugin-dialog-close-x-mine"));
    await userEvent.click(screen.getByTestId("plugin-edit-shipped"));
    expect(screen.getByTestId("plugin-upgrade-shipped")).toBeTruthy();
    await userEvent.click(screen.getByTestId("plugin-dialog-close-x-shipped"));
    await userEvent.click(screen.getByTestId("plugin-edit-gone"));
    expect(screen.queryByTestId("plugin-upgrade-gone")).toBeNull();
  });

  it("installs the chosen package through the ordinary upload", async () => {
    client.installPlugin.mockResolvedValue(staged());
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    const pkg = zip();
    await userEvent.upload(screen.getByTestId("plugin-upgrade-file-example-sink"), pkg);

    expect(client.installPlugin).toHaveBeenCalledWith(pkg);
    expect(await screen.findByTestId("plugin-upgrade-dialog")).toBeTruthy();
  });
});

describe("Upload new version of a different plugin", () => {
  it("says so when the package installed as a new plugin", async () => {
    client.getPlugins.mockResolvedValue({ plugins: [plugin()] });
    client.installPlugin.mockResolvedValue({
      plugins: [plugin(), plugin({ id: "other", name: "Other Sink" })],
    });
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugin-example-sink");
    await userEvent.click(screen.getByTestId("plugin-edit-example-sink"));
    await userEvent.upload(screen.getByTestId("plugin-upgrade-file-example-sink"), zip());

    expect((await screen.findByTestId("plugins-notice")).textContent).toContain(
      "This package is Other Sink (other), not Example Sink; it was installed as a new plugin.",
    );
  });
});

describe("the catalog", () => {
  const entry = (id: string, over = {}) => ({
    id,
    name: id,
    version: "2.0.0",
    provides: ["event-sink"],
    packageUrl: `https://plugins.example.test/${id}.zip`,
    ...over,
  });

  it("marks only flagged entries Update available and updates through from-url", async () => {
    client.getPlugins.mockResolvedValue({ plugins: [plugin({ id: "old" }), plugin({ id: "same" })] });
    client.getPluginCatalog.mockResolvedValue({
      url: "https://plugins.example.test/index.json",
      error: "",
      entries: [entry("old", { updateAvailable: true, installedVersion: "1.0.0" }), entry("same"), entry("fresh")],
    });
    client.installPluginFromURL.mockResolvedValue(staged());
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));

    expect(screen.getByTestId("catalog-update-available-old").textContent).toContain("Update available");
    expect(screen.queryByTestId("catalog-update-available-same")).toBeNull();
    expect(screen.queryByTestId("catalog-update-available-fresh")).toBeNull();
    expect(screen.queryByTestId("catalog-update-same")).toBeNull();
    expect(screen.getByTestId("catalog-installed-same")).toBeTruthy();

    await userEvent.click(screen.getByTestId("catalog-update-old"));
    expect(client.installPluginFromURL).toHaveBeenCalledWith({ url: "https://plugins.example.test/old.zip" });
    expect(await screen.findByTestId("plugin-upgrade-dialog")).toBeTruthy();
  });
});
