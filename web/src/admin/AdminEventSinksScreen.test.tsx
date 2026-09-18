import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { EventSinksView } from "../api/types";

// The delivery counters on the Event Sinks screen (ADR-0057 decision 6,
// plugin-system/06). They are the surface that answers the one question a sink
// cannot answer any other way: a sink has no Test button, because its secret is
// this server's own signing key and there is nobody to ask whether it works.
//
// So what is asserted here is that the three numbers reach the screen, and that the
// screen SAYS SOMETHING DIFFERENT when they are all zero — "nothing has been sent
// yet" is the sentence that stops an Admin debugging a receiver that is fine.

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

import AdminEventSinksScreen from "./AdminEventSinksScreen";

function view(counters = { delivered: 0, dropped: 0, failed: 0 }): EventSinksView {
  return {
    sinks: [
      {
        slug: "webhook",
        name: "Webhook",
        requiresSecret: true,
        enabled: true,
        hasSecret: true,
        url: "https://automation.local/obelo",
        events: ["scan.completed", "playback.started"],
        description: "POST one signed JSON document per event to a URL you choose.",
        docsURL: "",
        counters,
      },
    ],
    // All five, the way the server answers once every translation exists.
    availableEvents: [
      "scan.completed",
      "enrich.completed",
      "playback.started",
      "playback.stopped",
      "library.changed",
    ],
  };
}

beforeEach(() => {
  getEventSinks.mockReset();
  updateEventSinks.mockReset();
});

describe("the Event Sinks screen reports what has been delivered", () => {
  it("shows delivered, dropped and failure counts for the sink", async () => {
    getEventSinks.mockResolvedValue(view({ delivered: 12, dropped: 3, failed: 4 }));

    render(<AdminEventSinksScreen />);
    await screen.findByTestId("event-sinks-screen");

    expect(screen.getByTestId("sink-counter-delivered-webhook")).toHaveTextContent("12");
    expect(screen.getByTestId("sink-counter-dropped-webhook")).toHaveTextContent("3");
    expect(screen.getByTestId("sink-counter-failed-webhook")).toHaveTextContent("4");
    // With failures on the board, the hint explains what a failure IS — that is
    // the moment the Admin needs to know a failure means the target, not the
    // server.
    expect(screen.getByTestId("sink-counters-webhook").parentElement).toHaveTextContent(
      /target refused or could not be reached/,
    );
  });

  it("says nothing has been sent yet rather than showing three bare zeroes", async () => {
    getEventSinks.mockResolvedValue(view());

    render(<AdminEventSinksScreen />);
    await screen.findByTestId("event-sinks-screen");

    expect(screen.getByTestId("sink-counter-delivered-webhook")).toHaveTextContent("0");
    expect(screen.getByTestId("sink-counters-webhook").parentElement).toHaveTextContent(
      /Nothing has been sent yet/,
    );
  });

  it("offers every event the server says it can derive", async () => {
    getEventSinks.mockResolvedValue(view());

    render(<AdminEventSinksScreen />);
    await screen.findByTestId("event-sinks-screen");

    for (const ev of [
      "scan.completed",
      "enrich.completed",
      "playback.started",
      "playback.stopped",
      "library.changed",
    ]) {
      expect(screen.getByTestId(`sink-event-webhook-${ev}`)).toBeInTheDocument();
    }
    // And the two the sink is actually subscribed to are the two that are ticked.
    expect(screen.getByTestId("sink-event-webhook-scan.completed")).toBeChecked();
    expect(screen.getByTestId("sink-event-webhook-playback.started")).toBeChecked();
    expect(screen.getByTestId("sink-event-webhook-library.changed")).not.toBeChecked();
  });
});
