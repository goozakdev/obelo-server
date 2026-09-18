import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type {
  InstalledPlugin,
  PluginCatalogEntry,
  PluginCatalogView,
} from "../api/types";

// The two OPTIONAL halves of the Plugins screen (plugin-system/15): a catalog to
// browse, and publisher keys to pin.
//
// What is worth asserting is not that a list renders. It is the three things the
// screen would get wrong if nobody wrote them down:
//
//   1. The Browse tab keys off the catalog URL BEING SET, not off entries being
//      present. A configured catalog that is unreachable still has a tab — the
//      tab is where its note belongs — and a catalog that has never been set has
//      no tab bar at all, so a server that did not opt in looks exactly as it did.
//   2. An unreachable catalog is a NOTE, never an error. The upload and paste-URL
//      cards have nothing to do with the catalog and must survive its outage.
//   3. Installing an entry goes through the ORDINARY URL install, so the refusal
//      an entry gets for an address inside this network is the server's own
//      sentence, shown verbatim.

const client = vi.hoisted(() => ({
  getPlugins: vi.fn(),
  installPlugin: vi.fn(),
  installPluginFromURL: vi.fn(),
  enablePlugin: vi.fn(),
  disablePlugin: vi.fn(),
  reenablePlugin: vi.fn(),
  uninstallPlugin: vi.fn(),
  getPluginCatalog: vi.fn(),
  setPluginCatalog: vi.fn(),
  getPluginPublishers: vi.fn(),
  pinPluginPublisher: vi.fn(),
  unpinPluginPublisher: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import AdminPluginsScreen from "./AdminPluginsScreen";

const discordEntry: PluginCatalogEntry = {
  id: "discord",
  name: "Discord",
  version: "0.1.0",
  publisher: "Example Publisher",
  provides: ["event-sink"],
  manifestUrl: "https://plugins.example.test/discord/manifest.json",
  description: "Posts a message when something finishes.",
};

function catalog(over: Partial<PluginCatalogView> = {}): PluginCatalogView {
  return {
    url: "https://plugins.example.test/index.json",
    entries: [discordEntry],
    error: "",
    ...over,
  };
}

function installedDiscord(): InstalledPlugin {
  return {
    id: "discord",
    name: "Discord",
    version: "0.1.0",
    apiVersion: 1,
    provides: ["event-sink"],
    enabled: true,
    disabledByFailure: false,
    source: discordEntry.manifestUrl,
    publisher: "Example Publisher",
    keyId: "9f86d081884c7d65",
  };
}

beforeEach(() => {
  for (const fn of Object.values(client)) fn.mockReset();
  client.getPlugins.mockResolvedValue({ plugins: [] });
  client.getPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
  client.getPluginPublishers.mockResolvedValue({ publishers: [] });
});

describe("the catalog", () => {
  it("has no Browse tab at all until an address is set", async () => {
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.queryByTestId("plugin-tabs")).toBeNull();
    expect(screen.queryByTestId("plugin-tab-browse")).toBeNull();
    // ...and the two paths that have always been there are untouched.
    expect(screen.getByTestId("plugin-install-upload")).toBeTruthy();
    expect(screen.getByTestId("plugin-install-url")).toBeTruthy();
  });

  it("lists what a configured catalog offers, with what it says it provides", async () => {
    client.getPluginCatalog.mockResolvedValue(catalog());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));

    expect(screen.getByTestId("catalog-entry-discord")).toBeTruthy();
    expect(screen.getByTestId("catalog-version-discord").textContent).toContain("0.1.0");
    expect(screen.getByTestId("catalog-provides-discord").textContent).toContain(
      "Event sink",
    );
    expect(screen.getByTestId("catalog-manifest-discord").textContent).toBe(
      discordEntry.manifestUrl,
    );
    // The publisher is the index author's CLAIM and is labelled as one. Nothing
    // on this row has been verified by anything.
    expect(screen.getByTestId("catalog-publisher-discord").textContent).toBe(
      "Example Publisher",
    );
  });

  it("installs an entry through the ordinary URL install", async () => {
    client.getPluginCatalog.mockResolvedValue(catalog());
    client.installPluginFromURL.mockResolvedValue({ plugins: [installedDiscord()] });

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));
    await userEvent.click(screen.getByTestId("catalog-install-discord"));

    // The entry's manifest URL, and nothing catalog-specific: there is one install
    // path and the catalog only chooses an address for it.
    expect(client.installPluginFromURL).toHaveBeenCalledWith({
      url: discordEntry.manifestUrl,
    });
    expect(screen.getByTestId("plugins-notice").textContent).toContain("Discord");
  });

  it("passes an entry's own signature URL when it carries one", async () => {
    client.getPluginCatalog.mockResolvedValue(
      catalog({
        entries: [
          {
            ...discordEntry,
            signatureUrl: "https://plugins.example.test/discord/plugin.sig.json",
          },
        ],
      }),
    );
    client.installPluginFromURL.mockResolvedValue({ plugins: [installedDiscord()] });

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));
    await userEvent.click(screen.getByTestId("catalog-install-discord"));

    expect(client.installPluginFromURL).toHaveBeenCalledWith({
      url: discordEntry.manifestUrl,
      signatureUrl: "https://plugins.example.test/discord/plugin.sig.json",
    });
  });

  it("shows the server's own sentence when an entry is refused", async () => {
    client.getPluginCatalog.mockResolvedValue(catalog());
    client.installPluginFromURL.mockRejectedValue(
      new Error(
        "plugins.example.test resolves to an address on this server's own network, " +
          "and a plugin is code this server will run — upload the file instead",
      ),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));
    await userEvent.click(screen.getByTestId("catalog-install-discord"));

    expect(screen.getByTestId("plugins-action-error").textContent).toContain(
      "upload the file instead",
    );
  });

  it("keeps the tab, shows a note, and leaves the other paths working when the catalog is unreachable", async () => {
    client.getPluginCatalog.mockResolvedValue(
      catalog({
        entries: [],
        error: "The catalog at https://plugins.example.test/index.json could not be reached right now.",
      }),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    // The tab is there because the URL is SET, not because entries came back.
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));
    expect(screen.getByTestId("plugin-catalog-note").textContent).toContain(
      "could not be reached",
    );

    // And the rest of the screen is exactly where it was.
    await userEvent.click(screen.getByTestId("plugin-tab-installed"));
    expect(screen.getByTestId("plugin-install-upload")).toBeTruthy();
    expect(screen.getByTestId("plugin-install-url")).toBeTruthy();
  });

  it("marks an entry that is already installed instead of offering it again", async () => {
    client.getPlugins.mockResolvedValue({ plugins: [installedDiscord()] });
    client.getPluginCatalog.mockResolvedValue(catalog());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("plugin-tab-browse"));

    expect(screen.getByTestId("catalog-installed-discord")).toBeTruthy();
    expect(screen.queryByTestId("catalog-install-discord")).toBeNull();
  });

  it("saves an address and gains a tab, and clearing it loses one", async () => {
    client.setPluginCatalog.mockResolvedValue(catalog());

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    expect(screen.queryByTestId("plugin-tabs")).toBeNull();

    await userEvent.type(
      screen.getByTestId("plugin-catalog-url"),
      "https://plugins.example.test/index.json",
    );
    await userEvent.click(screen.getByTestId("plugin-catalog-save"));

    expect(client.setPluginCatalog).toHaveBeenCalledWith({
      url: "https://plugins.example.test/index.json",
    });
    expect(screen.getByTestId("plugin-tabs")).toBeTruthy();

    client.setPluginCatalog.mockResolvedValue({ url: "", entries: [], error: "" });
    await userEvent.clear(screen.getByTestId("plugin-catalog-url"));
    await userEvent.click(screen.getByTestId("plugin-catalog-save"));

    expect(screen.queryByTestId("plugin-tabs")).toBeNull();
  });
});

describe("pinned publisher keys", () => {
  it("says in words that an empty list means nothing is checked", async () => {
    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    // The whole point: an empty table with no sentence under it reads as "not set
    // up yet", which is the opposite of what it means.
    expect(screen.getByTestId("plugin-publishers-empty").textContent).toContain(
      "not checked",
    );
  });

  it("pins a key and lists it, unmasked", async () => {
    client.pinPluginPublisher.mockResolvedValue({
      publishers: [
        {
          publisher: "Example Publisher",
          publicKey: "3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
          keyId: "9f86d081884c7d65",
        },
      ],
    });

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-publisher-name"), "Example Publisher");
    await userEvent.type(
      screen.getByTestId("plugin-publisher-key"),
      "3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    );
    await userEvent.click(screen.getByTestId("plugin-publisher-pin"));

    expect(client.pinPluginPublisher).toHaveBeenCalledWith({
      publisher: "Example Publisher",
      publicKey: "3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    });
    // A PUBLIC key is shown in full. It is the one credential-shaped field in this
    // API that is not masked, because an operator has to compare it against what a
    // publisher advertises.
    expect(
      screen.getByTestId("publisher-key-Example Publisher").textContent,
    ).toContain("3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=");
    expect(screen.queryByTestId("plugin-publishers-empty")).toBeNull();
  });

  it("shows the server's sentence when a key is not a key", async () => {
    client.pinPluginPublisher.mockRejectedValue(
      new Error("that is not an ed25519 public key: an ed25519 public key is 32 bytes and this one is 4"),
    );

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    await userEvent.type(screen.getByTestId("plugin-publisher-name"), "Example Publisher");
    await userEvent.type(screen.getByTestId("plugin-publisher-key"), "bm9wZQ==");
    await userEvent.click(screen.getByTestId("plugin-publisher-pin"));

    expect(screen.getByTestId("plugins-action-error").textContent).toContain(
      "not an ed25519 public key",
    );
  });

  it("unpins a key", async () => {
    client.getPluginPublishers.mockResolvedValue({
      publishers: [
        {
          publisher: "Example Publisher",
          publicKey: "3q2+7wAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
          keyId: "9f86d081884c7d65",
        },
      ],
    });
    client.unpinPluginPublisher.mockResolvedValue({ publishers: [] });

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");
    await userEvent.click(screen.getByTestId("publisher-unpin-Example Publisher"));

    expect(client.unpinPluginPublisher).toHaveBeenCalledWith("Example Publisher");
    expect(screen.getByTestId("plugin-publishers-empty")).toBeTruthy();
  });
});

describe("a signed installed plugin", () => {
  it("says who signed it, and says nothing about one nobody verified", async () => {
    client.getPlugins.mockResolvedValue({
      plugins: [
        installedDiscord(),
        { ...installedDiscord(), id: "other", name: "Other", publisher: undefined, keyId: undefined },
      ],
    });

    render(<AdminPluginsScreen />);
    await screen.findByTestId("plugins-screen");

    expect(screen.getByTestId("plugin-publisher-discord").textContent).toContain(
      "Example Publisher",
    );
    // No "Unsigned" row: an empty publisher means nobody checked, which is not
    // the same claim and not one the server is in a position to make.
    expect(screen.queryByTestId("plugin-publisher-other")).toBeNull();
  });
});
