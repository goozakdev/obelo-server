import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import type { ApiClient } from "../api/client";

// EnrichmentConsentGate — the catch-up consent prompt (ADR-0032), which exists for
// servers the first-run setup step never reached.
//
// The case that matters most here is the one it must NOT handle: while the setup
// wizard is on screen. The wizard auto-logs-in as the Admin it just created, so
// this component's precondition (authed Admin + consent unset) goes true while the
// operator is reading the wizard's own copy of the question — and a native
// <dialog> opened then sits on top of the wizard and eats the clicks meant for it.
// jsdom did not catch that originally because nothing rendered both; driving the
// real browser did. Hence these tests.

const { getEnrichmentConsent, setEnrichmentConsent } = vi.hoisted(() => ({
  getEnrichmentConsent: vi.fn(),
  setEnrichmentConsent: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getEnrichmentConsent: (...a: unknown[]) => getEnrichmentConsent(...a),
      setEnrichmentConsent: (...a: unknown[]) => setEnrichmentConsent(...a),
    },
  };
});

import EnrichmentConsentGate from "./EnrichmentConsentGate";

function authStubClient(): ApiClient {
  return {
    token: "fake-token",
    setToken: () => {},
    setTokenDurable: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
  } as unknown as ApiClient;
}

// Renders the gate at `path` with a seeded logged-in session, exactly as App.tsx
// mounts it: outside <Routes>, so the current route is the only thing that varies.
function renderGateAt(path: string, role = "admin") {
  window.localStorage.setItem("obelo.token", "fake-token");
  window.localStorage.setItem(
    "obelo.user",
    JSON.stringify({ id: "u1", username: "operator", role }),
  );
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <MemoryRouter initialEntries={[path]}>
        <AuthProvider client={authStubClient()}>{children}</AuthProvider>
      </MemoryRouter>
    );
  }
  return render(<EnrichmentConsentGate />, { wrapper: Wrapper });
}

beforeEach(() => {
  window.localStorage.clear();
  getEnrichmentConsent.mockReset().mockResolvedValue({ state: "unset", credentialSource: "bootstrap" });
  setEnrichmentConsent.mockReset().mockResolvedValue({ state: "granted" });
});

describe("EnrichmentConsentGate", () => {
  it("prompts an Admin whose server never answered the question", async () => {
    renderGateAt("/");
    await waitFor(() =>
      expect(screen.getByTestId("enrichment-consent-dialog")).toBeInTheDocument(),
    );
  });

  it("says whose API key the decision covers", async () => {
    renderGateAt("/");
    const note = await screen.findByTestId("enrichment-consent-credentials");
    expect(note).toHaveAttribute("data-source", "bootstrap");
    expect(note.textContent).toMatch(/your own/i);
  });

  it("stays out of the way while the setup wizard is asking", async () => {
    renderGateAt("/setup");

    // Nothing rendered, and — the actual failure mode — no modal over the wizard.
    await waitFor(() => expect(getEnrichmentConsent).not.toHaveBeenCalled());
    expect(screen.queryByTestId("enrichment-consent-dialog")).not.toBeInTheDocument();
  });

  it("re-reads the decision on leaving setup, so an answer just given is not re-asked", async () => {
    const { unmount } = renderGateAt("/setup");
    unmount();

    // The wizard recorded a grant; navigating into the app must not re-open the
    // question against a state fetched before the answer existed.
    getEnrichmentConsent.mockResolvedValue({ state: "granted", credentialSource: "bootstrap" });
    renderGateAt("/");

    await waitFor(() => expect(getEnrichmentConsent).toHaveBeenCalled());
    expect(screen.queryByTestId("enrichment-consent-dialog")).not.toBeInTheDocument();
  });

  it("never asks a Member — the endpoint is Admin-only", async () => {
    renderGateAt("/", "member");
    await waitFor(() => expect(getEnrichmentConsent).not.toHaveBeenCalled());
    expect(screen.queryByTestId("enrichment-consent-dialog")).not.toBeInTheDocument();
  });
});
