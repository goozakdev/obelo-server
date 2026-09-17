import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { EventSinksView, MetadataProvider } from "../api/types";

// The events control (ADR-0057 decision 6, plugin-system/05): the one control that
// belongs to the Event sink Extension point. Two things are asserted here, because
// they are the two that could quietly go wrong:
//
//  1. it offers exactly what the SERVER says this build can derive — never a
//     hard-coded list, because promising an Admin an event nothing produces leaves
//     them unable to tell "unimplemented" from "my receiver is broken";
//  2. it appears for the Webhook sink and NOT on a provider surface — a Metadata
//     or Subtitle provider is asked questions and has nothing to subscribe to.

const { getEventSinks, updateEventSinks } = vi.hoisted(() => ({
  getEventSinks: vi.fn(),
  updateEventSinks: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getEventSinks: (...a: unknown[]) => getEventSinks(...a),
      updateEventSinks: (...a: unknown[]) => updateEventSinks(...a),
    },
  };
});

import EventsControl from "./EventsControl";
import AdminEventSinksScreen from "./AdminEventSinksScreen";
import ProviderConfigDialog from "./ProviderConfigDialog";

function view(over: Partial<EventSinksView> = {}): EventSinksView {
  return {
    sinks: [
      {
        slug: "webhook",
        name: "Webhook",
        requiresSecret: true,
        enabled: false,
        hasSecret: false,
        url: "",
        events: [],
        description: "POST one signed JSON document per event to a URL you choose.",
        docsURL: "",
      },
    ],
    availableEvents: ["scan.completed"],
    ...over,
  };
}

beforeEach(() => {
  getEventSinks.mockReset();
  updateEventSinks.mockReset();
});

describe("EventsControl", () => {
  it("offers exactly the events the server says it can derive", () => {
    render(
      <EventsControl
        slug="webhook"
        available={["scan.completed"]}
        selected={[]}
        onChange={() => {}}
      />,
    );
    expect(screen.getByTestId("sink-event-webhook-scan.completed")).not.toBeChecked();
    // The other four curated types exist in the contract but nothing derives them
    // yet, so they must not be offered.
    for (const absent of [
      "enrich.completed",
      "playback.started",
      "playback.stopped",
      "library.changed",
    ]) {
      expect(screen.queryByTestId(`sink-event-webhook-${absent}`)).toBeNull();
    }
  });

  it("grows with the server rather than with a client release", () => {
    render(
      <EventsControl
        slug="webhook"
        available={["scan.completed", "playback.started"]}
        selected={["playback.started"]}
        onChange={() => {}}
      />,
    );
    expect(screen.getByTestId("sink-event-webhook-scan.completed")).not.toBeChecked();
    expect(screen.getByTestId("sink-event-webhook-playback.started")).toBeChecked();
  });

  it("emits the next subscription in the server's order, not click order", async () => {
    const onChange = vi.fn();
    render(
      <EventsControl
        slug="webhook"
        available={["scan.completed", "playback.started"]}
        selected={["playback.started"]}
        onChange={onChange}
      />,
    );
    await userEvent.click(screen.getByTestId("sink-event-webhook-scan.completed"));
    expect(onChange).toHaveBeenCalledWith(["scan.completed", "playback.started"]);
  });

  it("unticking an event removes just that one", async () => {
    const onChange = vi.fn();
    render(
      <EventsControl
        slug="webhook"
        available={["scan.completed", "playback.started"]}
        selected={["scan.completed", "playback.started"]}
        onChange={onChange}
      />,
    );
    await userEvent.click(screen.getByTestId("sink-event-webhook-scan.completed"));
    expect(onChange).toHaveBeenCalledWith(["playback.started"]);
  });

  it("says so rather than rendering an empty question when nothing is emitted", () => {
    render(
      <EventsControl slug="webhook" available={[]} selected={[]} onChange={() => {}} />,
    );
    expect(screen.getByTestId("sink-events-empty-webhook")).toBeInTheDocument();
  });
});

describe("the events control on the Event Sinks screen", () => {
  it("shows for the Webhook sink and saves the subscription", async () => {
    getEventSinks.mockResolvedValue(view());
    updateEventSinks.mockResolvedValue(
      view({
        sinks: [
          {
            ...view().sinks[0],
            enabled: true,
            hasSecret: true,
            url: "https://automation.local/obelo",
            events: ["scan.completed"],
          },
        ],
      }),
    );

    render(<AdminEventSinksScreen />);
    await screen.findByTestId("event-sinks-screen");

    // The control is part of the sink's card.
    expect(screen.getByTestId("sink-events-webhook")).toBeInTheDocument();

    await userEvent.click(screen.getByTestId("sink-event-webhook-scan.completed"));
    await userEvent.type(
      screen.getByTestId("event-sink-url-webhook"),
      "https://automation.local/obelo",
    );
    await userEvent.click(screen.getByTestId("event-sink-enable-webhook"));
    await userEvent.click(screen.getByTestId("event-sinks-save"));

    await waitFor(() => expect(updateEventSinks).toHaveBeenCalledTimes(1));
    expect(updateEventSinks).toHaveBeenCalledWith({
      sinks: [
        {
          slug: "webhook",
          enabled: true,
          url: "https://automation.local/obelo",
          events: ["scan.completed"],
        },
      ],
    });
    await screen.findByTestId("event-sinks-saved");
  });

  it("sends only what changed", async () => {
    getEventSinks.mockResolvedValue(
      view({
        sinks: [
          {
            ...view().sinks[0],
            enabled: true,
            hasSecret: true,
            url: "https://automation.local/obelo",
            events: ["scan.completed"],
          },
        ],
      }),
    );
    updateEventSinks.mockResolvedValue(view());

    render(<AdminEventSinksScreen />);
    await screen.findByTestId("event-sinks-screen");

    // Unsubscribe and save: the URL and the secret are untouched, so neither is in
    // the payload — an omitted secret is an UNCHANGED secret.
    await userEvent.click(screen.getByTestId("sink-event-webhook-scan.completed"));
    await userEvent.click(screen.getByTestId("event-sinks-save"));

    await waitFor(() => expect(updateEventSinks).toHaveBeenCalledTimes(1));
    expect(updateEventSinks).toHaveBeenCalledWith({
      sinks: [{ slug: "webhook", events: [] }],
    });
  });
});

describe("the events control is a sink's, not a provider's", () => {
  it("does not appear in the metadata provider dialog", () => {
    const provider: MetadataProvider = {
      slug: "tmdb",
      name: "The Movie Database (TMDB)",
      kinds: ["video"],
      role: "authoritative",
      requiresKey: true,
      enabled: true,
      hasKey: true,
      baseURL: "https://api.themoviedb.org/3",
      imageBaseURL: "https://image.tmdb.org/t/p",
      description: "The Movie Database.",
      docsURL: "https://www.themoviedb.org/settings/api",
    };
    render(
      <ProviderConfigDialog
        provider={provider}
        musicBrainzRateLimitMs={1000}
        onSaved={() => {}}
        onClose={() => {}}
      />,
    );
    // A provider is ASKED questions; it has nothing to subscribe to, so there is no
    // events control anywhere on its configuration surface.
    expect(screen.queryByTestId("sink-events-tmdb")).toBeNull();
    expect(screen.queryByTestId("sink-event-tmdb-scan.completed")).toBeNull();
  });
});
