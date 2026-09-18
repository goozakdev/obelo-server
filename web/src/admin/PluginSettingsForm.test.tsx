import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type {
  InstalledPlugin,
  InstalledPluginsView,
  PluginSettingsField,
} from "../api/types";

// The schema-driven settings form (plugin-system/13).
//
// Three things are worth asserting and nothing else is. That a DECLARATION
// becomes the right control — an enum is a select of its options and not a text
// box, an integer is a number, a secret is masked. That what the form SENDS is
// the field's own JSON type — 7 and not "7", true and not "true", an array and
// not a comma list — because a guest reads it back in the shape it declared and a
// string-stuffed number would be a different value. And that a server's per-field
// refusal lands UNDER THE CONTROL that caused it, which is the entire reason
// validation is structured rather than one sentence.

const client = vi.hoisted(() => ({
  savePluginSettings: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return { ...actual, apiClient: client };
});

import { ApiError } from "../api/client";
import PluginSettingsForm from "./PluginSettingsForm";

const schema: PluginSettingsField[] = [
  { key: "account", type: "string", label: "Account name" },
  { key: "token", type: "secret", label: "Access token", required: true },
  { key: "endpoint", type: "url", label: "Mirror", help: "An absolute address." },
  { key: "adult", type: "bool", label: "Include adult titles" },
  { key: "region", type: "enum", label: "Region", options: ["eu", "us", "apac"], required: true },
  { key: "formats", type: "multi-select", label: "Formats", options: ["srt", "ass", "vtt"] },
  { key: "retries", type: "integer", label: "Retries", min: 1, max: 10 },
];

function plugin(over: Partial<InstalledPlugin> = {}): InstalledPlugin {
  return {
    id: "example-source",
    name: "Example Source",
    provides: ["metadata-provider"],
    enabled: true,
    disabledByFailure: false,
    settingsSchema: schema,
    settings: { values: {}, secrets: { token: false } },
    ...over,
  };
}

function view(p: InstalledPlugin): InstalledPluginsView {
  return { plugins: [p] };
}

beforeEach(() => {
  client.savePluginSettings.mockReset();
});

describe("a plugin's manifest-declared settings form", () => {
  it("renders one control of the right kind for every declared field type", () => {
    render(<PluginSettingsForm plugin={plugin()} onSaved={() => {}} />);

    expect(screen.getByTestId("plugin-settings-example-source")).toBeTruthy();
    // A string and a url are text boxes; an enum is a select carrying exactly the
    // declared options; an integer is a number input carrying the declared bounds.
    expect(screen.getByTestId("plugin-field-example-source-account").tagName).toBe("INPUT");
    const region = screen.getByTestId("plugin-field-example-source-region") as HTMLSelectElement;
    expect(region.tagName).toBe("SELECT");
    expect([...region.options].map((o) => o.value)).toEqual(["", "eu", "us", "apac"]);
    const retries = screen.getByTestId("plugin-field-example-source-retries") as HTMLInputElement;
    expect(retries.type).toBe("number");
    expect(retries.min).toBe("1");
    expect(retries.max).toBe("10");
    // A multi-select is one box per option, not a text field of comma-separated
    // values that an operator has to guess the separator of.
    expect(screen.getByTestId("plugin-field-example-source-formats-srt")).toBeTruthy();
    expect(screen.getByTestId("plugin-field-example-source-formats-vtt")).toBeTruthy();
    // A secret goes through the masked control the rest of this app uses, and the
    // server never sent a value for it to show.
    const secret = screen.getByTestId(
      "provider-key-input-example-source-token",
    ) as HTMLInputElement;
    expect(secret.type).toBe("password");
    expect(secret.value).toBe("");
    // The author's own help line reaches the operator.
    expect(screen.getByText("An absolute address.")).toBeTruthy();
  });

  it("shows a declared default for a field that has never been saved", () => {
    const p = plugin({
      settingsSchema: [
        { key: "endpoint", type: "url", label: "Mirror", default: "https://mirror.example.test" },
      ],
      settings: { values: {}, secrets: {} },
    });
    render(<PluginSettingsForm plugin={p} onSaved={() => {}} />);

    const box = screen.getByTestId("plugin-field-example-source-endpoint") as HTMLInputElement;
    expect(box.value).toBe("https://mirror.example.test");
  });

  it("sends each value in its field's own JSON type, and omits an untouched secret", async () => {
    const user = userEvent.setup();
    client.savePluginSettings.mockResolvedValue(view(plugin()));
    render(<PluginSettingsForm plugin={plugin()} onSaved={() => {}} />);

    await user.type(screen.getByTestId("plugin-field-example-source-account"), "ripley");
    await user.selectOptions(screen.getByTestId("plugin-field-example-source-region"), "us");
    await user.click(screen.getByTestId("plugin-field-example-source-adult"));
    await user.click(screen.getByTestId("plugin-field-example-source-formats-srt"));
    await user.click(screen.getByTestId("plugin-field-example-source-formats-vtt"));
    await user.type(screen.getByTestId("plugin-field-example-source-retries"), "4");
    await user.click(screen.getByTestId("plugin-settings-save-example-source"));

    expect(client.savePluginSettings).toHaveBeenCalledTimes(1);
    const [id, body] = client.savePluginSettings.mock.calls[0];
    expect(id).toBe("example-source");
    expect(body.values.account).toBe("ripley");
    expect(body.values.region).toBe("us");
    expect(body.values.adult).toBe(true);
    expect(body.values.formats).toEqual(["srt", "vtt"]);
    expect(body.values.retries).toBe(4);
    // The secret was not touched, so it is ABSENT rather than sent as "" — the
    // server never returned it, and sending an empty string would clear on every
    // save the one field the form cannot see.
    expect("token" in body.values).toBe(false);
  });

  it("clears a secret only when the Admin asks, as an explicit null", async () => {
    const user = userEvent.setup();
    client.savePluginSettings.mockResolvedValue(view(plugin()));
    const p = plugin({ settings: { values: {}, secrets: { token: true } } });
    render(<PluginSettingsForm plugin={p} onSaved={() => {}} />);

    // The form shows only that one is on file.
    expect(
      screen.getByTestId("provider-key-status-example-source-token").textContent,
    ).toBe("Configured");

    await user.click(screen.getByTestId("provider-key-clear-example-source-token"));
    await user.click(screen.getByTestId("plugin-settings-save-example-source"));

    const [, body] = client.savePluginSettings.mock.calls[0];
    expect(body.values.token).toBeNull();
  });

  it("puts the server's refusal under the control that caused it", async () => {
    const user = userEvent.setup();
    client.savePluginSettings.mockRejectedValue(
      new ApiError(400, "PLUGIN_INVALID_SETTINGS", "Region is required", {
        fields: [
          { key: "region", message: "Region is required" },
          { key: "retries", message: "Retries must be between 1 and 10" },
        ],
      }),
    );
    render(<PluginSettingsForm plugin={plugin()} onSaved={() => {}} />);

    await user.click(screen.getByTestId("plugin-settings-save-example-source"));

    expect(
      (await screen.findByTestId("plugin-field-example-source-region-error")).textContent,
    ).toBe("Region is required");
    expect(
      screen.getByTestId("plugin-field-example-source-retries-error").textContent,
    ).toBe("Retries must be between 1 and 10");
    // A field the server did not complain about carries no message at all.
    expect(screen.queryByTestId("plugin-field-example-source-account-error")).toBeNull();
    // And the envelope's own sentence is still shown, for a failing control that
    // has scrolled out of view.
    expect(screen.getByTestId("plugin-settings-error-example-source").textContent).toBe(
      "Region is required",
    );
  });

  it("hands the whole new list up after a save, and forgets the drafts", async () => {
    const user = userEvent.setup();
    const saved = plugin({ settings: { values: { account: "ripley" }, secrets: { token: true } } });
    client.savePluginSettings.mockResolvedValue(view(saved));
    const onSaved = vi.fn();
    render(<PluginSettingsForm plugin={plugin()} onSaved={onSaved} />);

    await user.click(screen.getByTestId("plugin-settings-save-example-source"));

    expect(onSaved).toHaveBeenCalledWith(view(saved));
    expect(
      (await screen.findByTestId("plugin-settings-notice-example-source")).textContent,
    ).toBe("Saved.");
  });

  it("renders nothing at all for a plugin that declares no settings of its own", () => {
    const { container } = render(
      <PluginSettingsForm
        plugin={plugin({ settingsSchema: undefined, settings: undefined })}
        onSaved={() => {}}
      />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("says what it cannot draw rather than drawing the wrong control", () => {
    const p = plugin({
      settingsSchema: [{ key: "palette", type: "colour-wheel", label: "Palette" }],
      settings: { values: {}, secrets: {} },
    });
    render(<PluginSettingsForm plugin={p} onSaved={() => {}} />);

    expect(screen.getByTestId("plugin-field-example-source-palette-unknown")).toBeTruthy();
  });
});
