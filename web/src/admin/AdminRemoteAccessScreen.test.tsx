import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { AuthProvider } from "../auth/session";
import { RequireAdmin } from "../auth/guards";
import { ApiError } from "../api/errors";
import { formatDate } from "../time";
import type { ApiClient } from "../api/client";
import type { TailnetSettingsView, TailnetStatus } from "../api/types";

// AdminRemoteAccessScreen end-to-end through the faked API client (the one seam
// — exactly as AdminProvidersScreen.test.tsx fakes apiClient). This screen is a
// state machine's face, so the spine of this suite is one test per state the
// operator can sit in: off, connecting, waiting-for-login, connected,
// connected-with-expiry-approaching, expired, and failed.
//
// The two that carry the product weight get more than a render assertion:
//   - the login link must appear with NO RELOAD, driven by the `tailscaleState`
//     SSE nudge (the event is a nudge; the GET is the truth), and
//   - the server's failure message must reach the screen VERBATIM.
//
// A second block covers the verbs (Connect / Disconnect with no dialog / Forget
// behind ConfirmDialog), the hostname warning at the point of editing, and the
// feature gate — where absent and false must be indistinguishable, right down to
// no fetch being attempted.

const {
  getTailnet,
  updateTailnet,
  connectTailnet,
  disconnectTailnet,
  forgetTailnet,
  subscribeEvents,
} = vi.hoisted(() => ({
  getTailnet: vi.fn(),
  updateTailnet: vi.fn(),
  connectTailnet: vi.fn(),
  disconnectTailnet: vi.fn(),
  forgetTailnet: vi.fn(),
  subscribeEvents: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTailnet: (...a: unknown[]) => getTailnet(...a),
      updateTailnet: (...a: unknown[]) => updateTailnet(...a),
      connectTailnet: (...a: unknown[]) => connectTailnet(...a),
      disconnectTailnet: (...a: unknown[]) => disconnectTailnet(...a),
      forgetTailnet: (...a: unknown[]) => forgetTailnet(...a),
      subscribeEvents: (...a: unknown[]) => subscribeEvents(...a),
    },
  };
});

import AdminRemoteAccessScreen from "./AdminRemoteAccessScreen";
import AdminScreen from "../screens/AdminScreen";

/** The live SSE listener the events hub registered, so a test can deliver a
 * `tailscaleState` nudge the way the server would. */
let emit: ((type: string, data: unknown) => void) | null = null;

function view(
  status: Partial<TailnetStatus> = {},
  over: Partial<TailnetSettingsView> = {},
): TailnetSettingsView {
  return {
    enabled: false,
    hostname: "obelo",
    controlURL: "",
    httpsEnabled: false,
    // httpsBound defaults to FALSE — the server's own default, and the state of
    // every node that has not bound tailnet :443. A fixture that has to opt into
    // it is a fixture that cannot accidentally assert HTTPS is up.
    status: { state: "stopped", keyExpiry: null, httpsBound: false, ...status },
    ...over,
  };
}

/** An RFC3339 stamp `days` from now (negative = in the past). */
function daysFromNow(days: number): string {
  return new Date(Date.now() + days * 86_400_000).toISOString();
}

beforeEach(() => {
  getTailnet.mockReset();
  updateTailnet.mockReset();
  connectTailnet.mockReset();
  disconnectTailnet.mockReset();
  forgetTailnet.mockReset();
  subscribeEvents.mockReset();
  emit = null;
  subscribeEvents.mockImplementation((fn: (type: string, data: unknown) => void) => {
    emit = fn;
    return () => {
      emit = null;
    };
  });
});

describe("AdminRemoteAccessScreen — the six states", () => {
  it("off: names the cost (Tailscale on every device) and offers Connect", async () => {
    getTailnet.mockResolvedValue(view());
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "off");
    // The honest cost belongs HERE, where it can still change somebody's mind.
    expect(screen.getByTestId("tailnet-cost")).toHaveTextContent(
      /every device you watch on must join the tailnet/i,
    );
    expect(screen.getByTestId("tailnet-cost")).toHaveTextContent(/tailscale/i);
    expect(screen.getByTestId("tailnet-connect")).toBeInTheDocument();
    // No address to show, and no Disconnect for a node that is not up.
    expect(screen.queryByTestId("tailnet-address")).toBeNull();
    expect(screen.queryByTestId("tailnet-disconnect")).toBeNull();
  });

  it("connecting: says a link is coming rather than spinning silently", async () => {
    getTailnet.mockResolvedValue(view({ state: "starting" }, { enabled: true }));
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "connecting");
    expect(panel).toHaveTextContent(/a login link will appear here/i);
  });

  it("waiting for login: the link is the content, prominent and clickable", async () => {
    getTailnet.mockResolvedValue(
      view(
        { state: "needsLogin", loginURL: "https://login.tailscale.com/a/1234abcd" },
        { enabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "waitingForLogin");
    const link = screen.getByTestId("tailnet-login-link");
    expect(link).toHaveAttribute("href", "https://login.tailscale.com/a/1234abcd");
    // The URL is shown in full — an operator on another machine has to be able to
    // read it, not just click it.
    expect(link).toHaveTextContent("https://login.tailscale.com/a/1234abcd");
    expect(panel).toHaveTextContent(/approve this server in your tailscale account/i);
  });

  it("connected: the address is the primary object, and it is copyable", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
      writable: true,
    });
    getTailnet.mockResolvedValue(
      view(
        {
          state: "running",
          fqdn: "obelo.tail1a2b.ts.net",
          addresses: ["100.101.102.103", "fd7a:115c:a1e0::1"],
          keyExpiry: daysFromNow(120),
          // Asked for AND achieved — the only combination that earns `https://`.
          httpsBound: true,
        },
        { enabled: true, httpsEnabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "connected");
    const address = screen.getByTestId("tailnet-address");
    expect(address).toHaveTextContent("https://obelo.tail1a2b.ts.net");
    // Visual dominance is CSS (`.tailnet-address` is the largest type on the
    // panel, in the accent colour); jsdom loads no stylesheet, so the class is
    // what can be asserted here — and it is the class the size hangs off.
    expect(address).toHaveClass("tailnet-address");

    await user.click(screen.getByTestId("tailnet-copy"));
    expect(writeText).toHaveBeenCalledWith("https://obelo.tail1a2b.ts.net");
    expect(await screen.findByTestId("tailnet-copy")).toHaveTextContent(/copied/i);

    // The raw Tailnet addresses are there for an operator with MagicDNS off.
    expect(screen.getByTestId("tailnet-addresses")).toHaveTextContent("100.101.102.103");
    // A far-off expiry is a fact, not a warning.
    expect(screen.queryByTestId("tailnet-expiry-warning")).toBeNull();
  });

  it("connected, expiry approaching: warns with the date and where the fix lives", async () => {
    const expiry = daysFromNow(9);
    getTailnet.mockResolvedValue(
      view(
        { state: "running", fqdn: "obelo.tail1a2b.ts.net", keyExpiry: expiry },
        { enabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const warning = await screen.findByTestId("tailnet-expiry-warning");
    // The date, named.
    expect(warning).toHaveTextContent(formatDate(expiry));
    expect(warning).toHaveTextContent(/9 days/);
    // And where the fix lives — which is not here.
    expect(warning).toHaveTextContent(/tailscale console/i);
    expect(warning).toHaveTextContent(/disable key expiry/i);
    // Still connected: the address is not hidden behind the warning.
    expect(screen.getByTestId("tailnet-address")).toHaveTextContent(
      "http://obelo.tail1a2b.ts.net",
    );
  });

  it("expired: a fresh login link framed as re-authorize, not as an error", async () => {
    const lapsed = daysFromNow(-3);
    getTailnet.mockResolvedValue(
      view(
        {
          // The SERVER says the key lapsed. The screen does not work this out
          // from the date — see the derivation test below.
          state: "keyExpired",
          loginURL: "https://login.tailscale.com/a/fresh999",
          keyExpiry: lapsed,
        },
        { enabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "expired");
    expect(panel).toHaveTextContent(/re-authorize/i);
    expect(screen.getByTestId("tailnet-expired-note")).toHaveTextContent(
      formatDate(lapsed),
    );
    // Nothing is broken — this is a lapse, not a fault.
    expect(screen.getByTestId("tailnet-expired-note")).toHaveTextContent(
      /nothing is broken/i,
    );
    expect(screen.getByTestId("tailnet-login-link")).toHaveAttribute(
      "href",
      "https://login.tailscale.com/a/fresh999",
    );
    expect(panel).toHaveTextContent(/disable key expiry/i);
  });

  it("does not invent 'expired' from the date — the server owns that call", async () => {
    // needsLogin carrying an already-past expiry. The screen once derived
    // "expired" from exactly this shape, before the server could say it; it must
    // not any more, or one distinction has two sources of truth that disagree
    // the first time a clock is wrong. The server checks expiry BEFORE its state
    // mapping, so this combination means "waiting for a first login", and a stale
    // date on the payload does not change that.
    getTailnet.mockResolvedValue(
      view(
        {
          state: "needsLogin",
          loginURL: "https://login.tailscale.com/a/first111",
          keyExpiry: daysFromNow(-3),
        },
        { enabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "waitingForLogin");
    expect(panel).not.toHaveTextContent(/re-authorize/i);
  });

  it("failed: the server's message renders verbatim", async () => {
    // The server's words name a console setting and a control URL; a paraphrase
    // would lose the only part the operator can act on.
    const verbatim =
      'tailnet: joining "https://headscale.example.net": dial tcp 10.0.0.9:443: connect: connection refused';
    getTailnet.mockResolvedValue(
      view({ state: "error", lastError: verbatim }, { enabled: true }),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const panel = await screen.findByTestId("remote-access-state");
    expect(panel).toHaveAttribute("data-state", "failed");
    expect(screen.getByTestId("tailnet-error")).toHaveTextContent(verbatim);
    // A failed node is a warning, not an outage: say so, and offer another go.
    expect(panel).toHaveTextContent(/lan is unaffected/i);
    expect(screen.getByTestId("tailnet-connect")).toHaveTextContent(/try again/i);
  });
});

describe("AdminRemoteAccessScreen — the address must not promise HTTPS the server never bound", () => {
  // `httpsEnabled` is what the operator ASKED FOR; `status.httpsBound` is what the
  // server ACHIEVED. They come apart in exactly the case this feature is most
  // likely to be misconfigured in — tailnet HTTPS needs MagicDNS and HTTPS
  // certificates enabled in the Tailscale console, which this server can neither
  // set nor detect — and the screen used to read the first as though it were the
  // second. The result: a saved toggle reporting success, a confident
  // `https://…` on screen, and an address that refuses connections while `:80`
  // served perfectly. Worse than a plain error, because it sends the operator off
  // to debug their client, their browser, or their tailnet.

  // Kept in the server's own words (cmd/obelo/tailnet_tls.go, failureMessage) —
  // this screen renders it VERBATIM, so a fixture that paraphrases would prove
  // nothing about the sentence an operator actually reads. It is deliberately
  // free of log-only phrasing: the message is written for both destinations, and
  // "the server is still running" reads like a non-sequitur on the settings panel
  // of a server you are looking at.
  const consoleFailure =
    'There is no HTTPS on the Tailnet for "obelo.tail1a2b.ts.net": tsnet: you must ' +
    "enable MagicDNS in the DNS page of the admin panel to proceed — plain HTTP on " +
    "the Tailnet at http://obelo.tail1a2b.ts.net is serving normally, and so is the " +
    "LAN listener on :8080. Tailnet HTTPS needs two settings in the Tailscale admin " +
    "console: DNS → MagicDNS enabled, and DNS → HTTPS Certificates enabled.";

  it("HTTPS off: the address is http:// and there is no panel beyond the toggle", async () => {
    getTailnet.mockResolvedValue(
      view(
        { state: "running", fqdn: "obelo.tail1a2b.ts.net" },
        { enabled: true, httpsEnabled: false },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    expect(await screen.findByTestId("tailnet-address")).toHaveTextContent(
      "http://obelo.tail1a2b.ts.net",
    );
    // Nothing was asked for, so there is nothing to report. A panel here would be
    // furniture explaining the absence of a feature nobody turned on.
    expect(screen.queryByTestId("tailnet-https-unbound")).toBeNull();
    expect(screen.queryByTestId("tailnet-https-bound")).toBeNull();
  });

  it("HTTPS asked for AND bound: the address is https:// and the panel confirms it", async () => {
    getTailnet.mockResolvedValue(
      view(
        { state: "running", fqdn: "obelo.tail1a2b.ts.net", httpsBound: true },
        { enabled: true, httpsEnabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    expect(await screen.findByTestId("tailnet-address")).toHaveTextContent(
      "https://obelo.tail1a2b.ts.net",
    );
    expect(screen.getByTestId("tailnet-https-bound")).toHaveTextContent(
      /https is serving on the tailnet/i,
    );
    expect(screen.queryByTestId("tailnet-https-unbound")).toBeNull();
  });

  it("HTTPS asked for and NOT bound: the address stays http:// and the server's reason is verbatim", async () => {
    // THE CRITERION THIS TICKET EXISTS FOR. The operator ticked the box and has
    // not done the console half; the setting is on and nothing is listening.
    getTailnet.mockResolvedValue(
      view(
        {
          state: "running",
          fqdn: "obelo.tail1a2b.ts.net",
          httpsBound: false,
          httpsError: consoleFailure,
        },
        { enabled: true, httpsEnabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    // The address is the one that WORKS.
    const address = await screen.findByTestId("tailnet-address");
    expect(address).toHaveTextContent("http://obelo.tail1a2b.ts.net");
    expect(address).not.toHaveTextContent("https://obelo.tail1a2b.ts.net");
    // The copy button hands over the same working address, not a decorated one.
    expect(screen.getByTestId("tailnet-copy")).toHaveAttribute(
      "aria-label",
      "Copy http://obelo.tail1a2b.ts.net",
    );

    // And the operator is told, where they asked for it, why — verbatim, because
    // the server's words name the two console settings that are the entire fix.
    const panel = screen.getByTestId("tailnet-https-unbound");
    expect(panel).toHaveTextContent(/https on the tailnet is not serving/i);
    expect(screen.getByTestId("tailnet-https-error")).toHaveTextContent(consoleFailure);
    // …together with the thing an operator who stops reading needs most: the
    // address above is fine and is still the one to use.
    expect(panel).toHaveTextContent("http://obelo.tail1a2b.ts.net");
    expect(panel).toHaveTextContent(/unaffected/i);
    expect(screen.queryByTestId("tailnet-https-bound")).toBeNull();
  });

  it("asked for, not bound, no reason yet: says it keeps trying rather than nothing", async () => {
    // The gap between the save and the first bind attempt returning. Rare and
    // brief, but a panel that renders an empty paragraph there looks broken.
    getTailnet.mockResolvedValue(
      view(
        { state: "running", fqdn: "obelo.tail1a2b.ts.net", httpsBound: false },
        { enabled: true, httpsEnabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    expect(await screen.findByTestId("tailnet-https-pending")).toHaveTextContent(
      /keeps trying on its own/i,
    );
    expect(screen.queryByTestId("tailnet-https-error")).toBeNull();
  });

  it("node not up: says nothing about HTTPS rather than reporting a failure nobody had", async () => {
    // HTTPS is enabled and the node is disconnected. Nothing is trying to bind
    // :443, so "HTTPS is not serving" would be this screen making exactly the
    // mistake it was just fixed for, in the other direction — asserting a failure
    // the server has not had. The node's own state is the panel above.
    getTailnet.mockResolvedValue(
      view({ state: "stopped" }, { enabled: false, httpsEnabled: true }),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    await screen.findByTestId("remote-access-state");
    expect(screen.queryByTestId("tailnet-https-unbound")).toBeNull();
    expect(screen.queryByTestId("tailnet-https-bound")).toBeNull();
    // The toggle itself is still there and still reflects the saved setting — the
    // operator's choice survives the node going down.
    expect(screen.getByTestId("tailnet-https-input")).toBeChecked();
  });

  it("the SSE nudge flips the address and the panel with no reload", async () => {
    // The fix happens in a browser tab somewhere else: the operator enables
    // MagicDNS in the Tailscale console, the server's retry timer binds :443, and
    // the supervisor's report fires the `tailscaleState` nudge. Nothing here polls
    // — the event says "look again" and the refetch says what is true.
    getTailnet.mockResolvedValueOnce(
      view(
        {
          state: "running",
          fqdn: "obelo.tail1a2b.ts.net",
          httpsBound: false,
          httpsError: consoleFailure,
        },
        { enabled: true, httpsEnabled: true },
      ),
    );
    getTailnet.mockResolvedValue(
      view(
        { state: "running", fqdn: "obelo.tail1a2b.ts.net", httpsBound: true },
        { enabled: true, httpsEnabled: true },
      ),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    expect(await screen.findByTestId("tailnet-address")).toHaveTextContent(
      "http://obelo.tail1a2b.ts.net",
    );
    expect(screen.getByTestId("tailnet-https-unbound")).toBeInTheDocument();

    expect(emit).not.toBeNull();
    await act(async () => {
      emit?.("tailscaleState", {});
    });

    await waitFor(() =>
      expect(screen.getByTestId("tailnet-address")).toHaveTextContent(
        "https://obelo.tail1a2b.ts.net",
      ),
    );
    expect(screen.getByTestId("tailnet-https-bound")).toBeInTheDocument();
    expect(screen.queryByTestId("tailnet-https-unbound")).toBeNull();
  });
});

describe("AdminRemoteAccessScreen — live updates", () => {
  it("shows the login link with no reload when the tailscaleState event arrives", async () => {
    const user = userEvent.setup();
    // Connect answers with a node that is up but has no link yet — the link is
    // seconds away and arrives over SSE. This is the whole reason the screen
    // cannot be a plain form.
    getTailnet.mockResolvedValueOnce(view());
    connectTailnet.mockResolvedValue(view({ state: "starting" }, { enabled: true }));
    getTailnet.mockResolvedValue(
      view(
        { state: "needsLogin", loginURL: "https://login.tailscale.com/a/later777" },
        { enabled: true },
      ),
    );

    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });
    await screen.findByTestId("tailnet-connect");

    await user.click(screen.getByTestId("tailnet-connect"));
    await waitFor(() =>
      expect(screen.getByTestId("remote-access-state")).toHaveAttribute(
        "data-state",
        "connecting",
      ),
    );
    expect(screen.queryByTestId("tailnet-login-link")).toBeNull();

    // The server nudges; the screen refetches; the link lands. No reload, and
    // the operator never has to wonder whether the button worked.
    expect(emit).not.toBeNull();
    await act(async () => {
      emit?.("tailscaleState", {});
    });

    const link = await screen.findByTestId("tailnet-login-link");
    expect(link).toHaveAttribute("href", "https://login.tailscale.com/a/later777");
    // The event carries nothing — the refetch is what told us anything.
    expect(getTailnet).toHaveBeenCalledTimes(2);
  });
});

describe("AdminRemoteAccessScreen — the verbs", () => {
  it("Connect calls the connect route", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(view());
    connectTailnet.mockResolvedValue(view({ state: "starting" }, { enabled: true }));
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    await user.click(await screen.findByTestId("tailnet-connect"));
    await waitFor(() => expect(connectTailnet).toHaveBeenCalled());
  });

  it("Disconnect does NOT ask for confirmation — it is reversible", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(
      view({ state: "running", fqdn: "obelo.tail1a2b.ts.net" }, { enabled: true }),
    );
    disconnectTailnet.mockResolvedValue(view());
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    await user.click(await screen.findByTestId("tailnet-disconnect"));

    await waitFor(() => expect(disconnectTailnet).toHaveBeenCalled());
    // A dialog on a reversible action trains people to dismiss dialogs.
    expect(screen.queryByTestId("confirm-dialog")).toBeNull();
  });

  it("Forget is confirmed, and the confirmation states both consequences", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(
      view({ state: "running", fqdn: "obelo.tail1a2b.ts.net" }, { enabled: true }),
    );
    forgetTailnet.mockResolvedValue(view());
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    await user.click(await screen.findByTestId("tailnet-forget"));

    const message = await screen.findByTestId("confirm-dialog-message");
    // 1. the next connect needs a fresh login…
    expect(message).toHaveTextContent(/fresh login/i);
    // 2. …and a dead node row stays in the Tailscale console, which only the
    //    operator can delete — nothing Obelo does can reach it.
    expect(message).toHaveTextContent(/tailscale console/i);
    expect(message).toHaveTextContent(/only you can delete it/i);
    // Nothing has happened yet.
    expect(forgetTailnet).not.toHaveBeenCalled();

    await user.click(screen.getByTestId("confirm-dialog-confirm"));
    await waitFor(() => expect(forgetTailnet).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByTestId("confirm-dialog")).toBeNull());
  });

  it("cancelling Forget leaves the node alone", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(
      view({ state: "running", fqdn: "obelo.tail1a2b.ts.net" }, { enabled: true }),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    await user.click(await screen.findByTestId("tailnet-forget"));
    await screen.findByTestId("confirm-dialog");
    await user.click(screen.getByTestId("confirm-dialog-cancel"));

    await waitFor(() => expect(screen.queryByTestId("confirm-dialog")).toBeNull());
    expect(forgetTailnet).not.toHaveBeenCalled();
  });
});

describe("AdminRemoteAccessScreen — settings", () => {
  it("warns about stored client addresses while the hostname is being edited, before any save", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(
      view({ state: "running", fqdn: "obelo.tail1a2b.ts.net" }, { enabled: true }),
    );
    updateTailnet.mockResolvedValue(
      view({ state: "starting" }, { enabled: true, hostname: "cinema" }),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const input = await screen.findByTestId("tailnet-hostname-input");
    // Nothing edited yet, so no warning shouting at a field nobody touched.
    expect(screen.queryByTestId("tailnet-hostname-warning")).toBeNull();

    await user.clear(input);
    await user.type(input, "cinema");

    // The warning is up BEFORE the save, which is the only moment it can change
    // the outcome.
    const warning = await screen.findByTestId("tailnet-hostname-warning");
    expect(warning).toHaveTextContent(/every client that stored the old one/i);
    expect(warning).toHaveTextContent(/tailscale console/i);
    expect(updateTailnet).not.toHaveBeenCalled();

    await user.click(screen.getByTestId("tailnet-save"));
    await waitFor(() =>
      expect(updateTailnet).toHaveBeenCalledWith({ hostname: "cinema" }),
    );
    expect(await screen.findByTestId("tailnet-save-success")).toBeInTheDocument();
  });

  it("frames the Control URL as the self-hosting escape hatch and saves the HTTPS toggle", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(view());
    updateTailnet.mockResolvedValue(view({}, { httpsEnabled: true }));
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const hint = await screen.findByTestId("tailnet-controlurl-hint");
    expect(hint).toHaveTextContent(/leave this empty to use tailscale/i);
    expect(hint).toHaveTextContent(/headscale/i);

    await user.click(screen.getByTestId("tailnet-https-input"));
    await user.click(screen.getByTestId("tailnet-save"));
    await waitFor(() =>
      expect(updateTailnet).toHaveBeenCalledWith({ httpsEnabled: true }),
    );
  });

  it("renders a refused save verbatim (the server names the field and the rule)", async () => {
    const user = userEvent.setup();
    getTailnet.mockResolvedValue(view());
    const refusal =
      'hostname must be a single DNS label: letters, digits and hyphens only, not starting or ending with a hyphen, at most 63 characters (e.g. "obelo")';
    updateTailnet.mockRejectedValue(
      new ApiError(422, "TAILNET_INVALID_HOSTNAME", refusal),
    );
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    const input = await screen.findByTestId("tailnet-hostname-input");
    await user.clear(input);
    await user.type(input, "-nope-");
    await user.click(screen.getByTestId("tailnet-save"));

    expect(await screen.findByTestId("tailnet-save-error")).toHaveTextContent(refusal);
  });

  it("renders a failed load verbatim, with a retry", async () => {
    const user = userEvent.setup();
    const message = "remote access is not available on this server";
    getTailnet.mockRejectedValueOnce(
      new ApiError(503, "SERVICE_UNAVAILABLE", message),
    );
    getTailnet.mockResolvedValue(view());
    renderWithAuth(<AdminRemoteAccessScreen />, {
      initialEntries: ["/admin/remote-access"],
      features: { tailscale: true },
    });

    expect(await screen.findByTestId("remote-access-load-error")).toHaveTextContent(
      message,
    );
    await user.click(screen.getByTestId("remote-access-retry"));
    expect(await screen.findByTestId("remote-access-state")).toHaveAttribute(
      "data-state",
      "off",
    );
  });
});

describe("Remote access tab in the Admin hub", () => {
  it("renders the tab and mounts the screen when the server advertises the feature", async () => {
    getTailnet.mockResolvedValue(view());
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/remote-access"], features: { tailscale: true } },
    );

    expect(screen.getByTestId("admin-tab-remote-access")).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByTestId("admin-remote-access")).toBeInTheDocument(),
    );
    expect(getTailnet).toHaveBeenCalled();
  });

  it("features.tailscale false → no tab, no screen, no fetch", async () => {
    getTailnet.mockResolvedValue(view());
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/remote-access"], features: { tailscale: false } },
    );

    await waitFor(() => expect(screen.getByTestId("admin-tabs")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-tab-remote-access")).toBeNull();
    // Not disabled, not an empty state: absent. And the route is gone with it,
    // so navigating straight at it renders nothing rather than a broken panel.
    expect(screen.queryByTestId("admin-remote-access")).toBeNull();
    expect(getTailnet).not.toHaveBeenCalled();
  });

  it("an ABSENT tailscale flag behaves exactly like false", async () => {
    getTailnet.mockResolvedValue(view());
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/remote-access"] }, // no tailscale key at all
    );

    await waitFor(() => expect(screen.getByTestId("admin-tabs")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-tab-remote-access")).toBeNull();
    expect(screen.queryByTestId("admin-remote-access")).toBeNull();
    expect(getTailnet).not.toHaveBeenCalled();
  });

  it("redirects a Member away from /admin/remote-access (RequireAdmin gate)", async () => {
    window.localStorage.setItem("obelo.token", "fake-token");
    window.localStorage.setItem(
      "obelo.user",
      JSON.stringify({ id: "m1", username: "kid", role: "member" }),
    );
    const stub = {
      token: "fake-token",
      setToken: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
    } as unknown as ApiClient;

    render(
      <MemoryRouter initialEntries={["/admin/remote-access"]}>
        <AuthProvider client={stub}>
          <Routes>
            <Route path="/" element={<div data-testid="landing" />} />
            <Route
              path="/admin/*"
              element={
                <RequireAdmin>
                  <AdminScreen />
                </RequireAdmin>
              }
            />
          </Routes>
        </AuthProvider>
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByTestId("landing")).toBeInTheDocument());
    expect(screen.queryByTestId("admin-remote-access")).not.toBeInTheDocument();
    expect(getTailnet).not.toHaveBeenCalled();
  });
});
