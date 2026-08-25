import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider } from "../auth/session";
import type { ApiClient } from "../api/client";

// SetupScreen — first run (ADR-0013) plus the metadata-services decision that is
// now a step of it (ADR-0032). The apiClient is the one seam; the router and the
// real AuthProvider are live, so the auto-login-then-ask ordering is exercised
// rather than assumed (the consent endpoint is Admin-only — asking before the
// login lands would 403).
//
// The behavior under test is the CONTROL guarantee: on a build that ships default
// TMDB / fanart.tv credentials, the operator is asked before this server can use
// them, is told the keys are not theirs, and is told they can supply their own.

const { setup, getEnrichmentConsent, setEnrichmentConsent, login } = vi.hoisted(() => ({
  setup: vi.fn(),
  getEnrichmentConsent: vi.fn(),
  setEnrichmentConsent: vi.fn(),
  login: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      setup: (...a: unknown[]) => setup(...a),
      getEnrichmentConsent: (...a: unknown[]) => getEnrichmentConsent(...a),
      setEnrichmentConsent: (...a: unknown[]) => setEnrichmentConsent(...a),
    },
  };
});

import SetupScreen from "./SetupScreen";

// The AuthProvider's client seam: enough for the provider to mount unauthenticated
// and to complete the auto-login SetupScreen performs after creating the admin.
function authStubClient(): ApiClient {
  return {
    token: null,
    setToken: () => {},
    setTokenDurable: () => {},
    setUnauthorizedHandler: () => {},
    verifySession: () => Promise.resolve({}),
    serverInfo: () => Promise.resolve({ version: "test", supportedVersions: [1], features: {}, setupRequired: false }),
    login: (...a: unknown[]) => login(...a),
  } as unknown as ApiClient;
}

function renderSetup() {
  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <MemoryRouter initialEntries={["/setup"]}>
        <AuthProvider client={authStubClient()}>{children}</AuthProvider>
      </MemoryRouter>
    );
  }
  return render(
    <Routes>
      <Route path="/setup" element={<SetupScreen />} />
      <Route path="/" element={<div data-testid="home" />} />
    </Routes>,
    { wrapper: Wrapper },
  );
}

// Fill in step 1 and submit it — the precondition for every metadata-step case.
async function createAdmin(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByTestId("setup-claim-token"), "CLAIM-123");
  await user.type(screen.getByTestId("setup-username"), "operator");
  await user.type(screen.getByTestId("setup-password"), "hunter2hunter2");
  await user.click(screen.getByTestId("setup-submit"));
}

beforeEach(() => {
  window.localStorage.clear();
  setup.mockReset().mockResolvedValue({ user: { id: "u1", username: "operator", role: "admin" } });
  login.mockReset().mockResolvedValue({
    token: "t1",
    user: { id: "u1", username: "operator", role: "admin" },
  });
  getEnrichmentConsent.mockReset().mockResolvedValue({ state: "unset", credentialSource: "bootstrap" });
  setEnrichmentConsent.mockReset().mockResolvedValue({ state: "granted" });
});

describe("SetupScreen first-run metadata decision", () => {
  it("asks about metadata services after creating the admin, before anything is enabled", async () => {
    const user = userEvent.setup();
    renderSetup();

    await createAdmin(user);

    // The wizard does not end at "admin created": it asks the question first.
    await waitFor(() => expect(screen.getByTestId("setup-metadata-step")).toBeInTheDocument());
    expect(screen.queryByTestId("home")).not.toBeInTheDocument();
    // And it has asked NOTHING of the metadata providers yet — the decision is
    // recorded only when the operator answers.
    expect(setEnrichmentConsent).not.toHaveBeenCalled();

    // The consent read happens after the login, or it would 403 on the Admin-only
    // endpoint. Ordering is the contract here, so assert it directly.
    expect(login).toHaveBeenCalled();
    expect(login.mock.invocationCallOrder[0]).toBeLessThan(
      getEnrichmentConsent.mock.invocationCallOrder[0],
    );
  });

  it("tells an official-build operator the bundled keys are not theirs, and where to use their own", async () => {
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    const note = await screen.findByTestId("setup-metadata-credentials");
    expect(note).toHaveAttribute("data-source", "bootstrap");
    // The two facts an operator of a pre-compiled binary / Docker image cannot
    // otherwise know: the credentials came with the build and are shared, and
    // their own keys are accepted instead.
    expect(note.textContent).toMatch(/bundled|comes with/i);
    expect(note.textContent).toMatch(/your own/i);
    expect(note.textContent).toMatch(/Metadata Providers/i);
  });

  it("says the opposite on a build that ships no keys", async () => {
    getEnrichmentConsent.mockResolvedValue({ state: "unset", credentialSource: "none" });
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    const note = await screen.findByTestId("setup-metadata-credentials");
    expect(note).toHaveAttribute("data-source", "none");
    expect(note.textContent).toMatch(/no metadata API keys/i);
  });

  it("records a grant and lands on Home", async () => {
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    await user.click(await screen.findByTestId("setup-metadata-enable"));

    await waitFor(() => expect(setEnrichmentConsent).toHaveBeenCalledWith(true));
    await waitFor(() => expect(screen.getByTestId("home")).toBeInTheDocument());
  });

  it("records a decline and still lands on Home", async () => {
    setEnrichmentConsent.mockResolvedValue({ state: "declined" });
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    await user.click(await screen.findByTestId("setup-metadata-decline"));

    // Declining is a RECORDED decision, not a skip: the server must be told, so
    // the catch-up gate does not re-ask a question already answered.
    await waitFor(() => expect(setEnrichmentConsent).toHaveBeenCalledWith(false));
    await waitFor(() => expect(screen.getByTestId("home")).toBeInTheDocument());
  });

  it("does not re-ask when the decision was pre-seeded (headless deploy)", async () => {
    getEnrichmentConsent.mockResolvedValue({ state: "granted", credentialSource: "operator" });
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    await waitFor(() => expect(screen.getByTestId("home")).toBeInTheDocument());
    expect(screen.queryByTestId("setup-metadata-step")).not.toBeInTheDocument();
    expect(setEnrichmentConsent).not.toHaveBeenCalled();
  });

  it("never traps the operator in setup when the consent read fails", async () => {
    getEnrichmentConsent.mockRejectedValue(new Error("boom"));
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    // The admin exists; a failure on a secondary concern must not strand them on a
    // form they already completed. Enrichment stays off — the server gate, not this
    // screen, is what holds that line.
    await waitFor(() => expect(screen.getByTestId("home")).toBeInTheDocument());
    expect(setEnrichmentConsent).not.toHaveBeenCalled();
  });

  it("offers a way past a failed save rather than blocking setup", async () => {
    setEnrichmentConsent.mockRejectedValue(new Error("boom"));
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    await user.click(await screen.findByTestId("setup-metadata-enable"));
    expect(await screen.findByTestId("setup-error")).toBeInTheDocument();

    await user.click(screen.getByTestId("setup-metadata-skip"));
    await waitFor(() => expect(screen.getByTestId("home")).toBeInTheDocument());
  });

  it("keeps the admin creation failure on step 1", async () => {
    setup.mockRejectedValue(new Error("bad claim token"));
    const user = userEvent.setup();
    renderSetup();
    await createAdmin(user);

    expect(await screen.findByTestId("setup-error")).toBeInTheDocument();
    expect(screen.queryByTestId("setup-metadata-step")).not.toBeInTheDocument();
    expect(getEnrichmentConsent).not.toHaveBeenCalled();
  });
});
