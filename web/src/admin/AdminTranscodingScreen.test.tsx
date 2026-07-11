import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import { AuthProvider } from "../auth/session";
import { RequireAdmin } from "../auth/guards";
import type { ApiClient } from "../api/client";
import type { TranscodingSnapshot } from "../api/types";

// AdminTranscodingScreen through the faked API client (the one seam): the backend
// block renders the active/requested/reason projection, the Degraded badge is the
// one loud pixel and shows only when degraded, the screen polls while mounted, and
// the tab is wired into the admin hub behind RequireAdmin.

const { getTranscoding } = vi.hoisted(() => ({ getTranscoding: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual =
    await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      getTranscoding: (...a: unknown[]) => getTranscoding(...a),
    },
  };
});

import AdminTranscodingScreen from "./AdminTranscodingScreen";
import AdminScreen from "../screens/AdminScreen";

function snapshot(
  over: Partial<TranscodingSnapshot["backend"]> = {},
  loadOver: Partial<TranscodingSnapshot["load"]> = {},
  gpu: TranscodingSnapshot["gpu"] = null,
): TranscodingSnapshot {
  return {
    backend: {
      requested: "cpu",
      active: "cpu",
      degraded: false,
      reason: "hardware acceleration off; using CPU libx264",
      ...over,
    },
    load: {
      active: 0,
      cap: 4,
      atCapacity: false,
      ...loadOver,
    },
    gpu,
  };
}

beforeEach(() => {
  getTranscoding.mockReset();
});

describe("AdminTranscodingScreen", () => {
  it("renders the active backend, requested, and reason", async () => {
    getTranscoding.mockResolvedValue(
      snapshot({
        requested: "auto",
        active: "cpu",
        reason: "auto-detect found no working hardware encoder; using CPU",
      }),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    await waitFor(() =>
      expect(screen.getByTestId("transcoding-backend")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("transcoding-active")).toHaveTextContent(/CPU/);
    expect(screen.getByTestId("transcoding-active")).toHaveAttribute(
      "data-backend",
      "cpu",
    );
    expect(screen.getByTestId("transcoding-requested")).toHaveTextContent(/auto/);
    expect(screen.getByTestId("transcoding-reason")).toHaveTextContent(
      /no working hardware encoder/,
    );
    // Not degraded → no badge.
    expect(
      screen.queryByTestId("transcoding-degraded-badge"),
    ).not.toBeInTheDocument();
  });

  it("renders transcode load as active / cap without an at-capacity badge below the cap", async () => {
    getTranscoding.mockResolvedValue(snapshot({}, { active: 2, cap: 4 }));
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    await waitFor(() =>
      expect(screen.getByTestId("transcoding-load")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("transcoding-load-active")).toHaveTextContent("2");
    expect(screen.getByTestId("transcoding-load-cap")).toHaveTextContent("4");
    expect(
      screen.queryByTestId("transcoding-at-capacity-badge"),
    ).not.toBeInTheDocument();
  });

  it("renders an unlimited cap as 'unlimited', never 0", async () => {
    getTranscoding.mockResolvedValue(
      snapshot({}, { active: 3, cap: 0, atCapacity: false }),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    await waitFor(() =>
      expect(screen.getByTestId("transcoding-load-cap")).toHaveTextContent(
        "unlimited",
      ),
    );
  });

  it("shows an at-capacity badge when the server is at its cap", async () => {
    getTranscoding.mockResolvedValue(
      snapshot({}, { active: 4, cap: 4, atCapacity: true }),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    const badge = await screen.findByTestId("transcoding-at-capacity-badge");
    expect(badge).toHaveTextContent(/at capacity/i);
  });

  it("shows the Degraded badge when a hardware backend fell back to CPU", async () => {
    getTranscoding.mockResolvedValue(
      snapshot({
        requested: "nvenc",
        active: "cpu",
        degraded: true,
        reason:
          "configured backend nvenc did not validate; falling back to CPU libx264",
      }),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    const badge = await screen.findByTestId("transcoding-degraded-badge");
    expect(badge).toHaveTextContent(/degraded/i);
    expect(screen.getByTestId("transcoding-requested")).toHaveTextContent(/NVENC/);
  });

  it("renders the GPU telemetry readout with a freshness stamp", async () => {
    getTranscoding.mockResolvedValue(
      snapshot(
        { active: "nvenc", requested: "nvenc" },
        { active: 1, cap: 4 },
        {
          utilizationPct: 37,
          vramUsedMb: 1240,
          vramTotalMb: 8192,
          encoderSessions: 2,
          driverVersion: "550.90.07",
          sampledAt: new Date(Date.now() - 3000).toISOString(),
        },
      ),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    await waitFor(() =>
      expect(screen.getByTestId("transcoding-gpu")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("transcoding-gpu-utilization")).toHaveTextContent(
      "37%",
    );
    expect(screen.getByTestId("transcoding-gpu-vram")).toHaveTextContent(
      /1240 \/ 8192 MB/,
    );
    expect(screen.getByTestId("transcoding-gpu-sessions")).toHaveTextContent("2");
    expect(screen.getByTestId("transcoding-gpu-driver")).toHaveTextContent(
      "550.90.07",
    );
    expect(screen.getByTestId("transcoding-gpu-freshness")).toHaveTextContent(
      /as of \d+s ago/,
    );
    expect(
      screen.queryByTestId("transcoding-gpu-unavailable"),
    ).not.toBeInTheDocument();
  });

  it("collapses GPU telemetry to an 'unavailable' line when gpu is null", async () => {
    getTranscoding.mockResolvedValue(snapshot({}, {}, null));
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });

    const line = await screen.findByTestId("transcoding-gpu-unavailable");
    expect(line).toHaveTextContent(/gpu telemetry unavailable/i);
    expect(
      screen.queryByTestId("transcoding-gpu-freshness"),
    ).not.toBeInTheDocument();
  });

  it("surfaces a readable error when the snapshot read fails", async () => {
    getTranscoding.mockRejectedValue(
      new ApiError(500, "INTERNAL", "could not read transcoding status"),
    );
    renderWithAuth(<AdminTranscodingScreen />, {
      initialEntries: ["/admin/transcoding"],
    });
    const err = await screen.findByTestId("transcoding-error");
    expect(err).toHaveTextContent(/could not read transcoding status/i);
  });

  it("polls the endpoint repeatedly while mounted and stops on unmount", async () => {
    getTranscoding.mockResolvedValue(snapshot());
    const { unmount } = renderWithAuth(
      <AdminTranscodingScreen intervalMs={20} />,
      { initialEntries: ["/admin/transcoding"] },
    );

    // The immediate mount read plus interval ticks accumulate multiple calls.
    await waitFor(() =>
      expect(getTranscoding.mock.calls.length).toBeGreaterThanOrEqual(3),
    );

    unmount();
    const after = getTranscoding.mock.calls.length;
    // Give the (now-cleared) interval a few periods; no further calls land.
    await new Promise((r) => setTimeout(r, 80));
    expect(getTranscoding.mock.calls.length).toBe(after);
  });
});

describe("Transcoding tab in the Admin hub", () => {
  it("renders the tab beside the existing tabs and mounts the screen", async () => {
    getTranscoding.mockResolvedValue(snapshot());
    renderWithAuth(
      <Routes>
        <Route path="/admin/*" element={<AdminScreen />} />
      </Routes>,
      { initialEntries: ["/admin/transcoding"] },
    );

    expect(screen.getByTestId("admin-tab-transcoding")).toBeInTheDocument();
    expect(screen.getByTestId("admin-tab-libraries")).toBeInTheDocument();

    await waitFor(() =>
      expect(screen.getByTestId("admin-transcoding")).toBeInTheDocument(),
    );
    expect(getTranscoding).toHaveBeenCalled();
  });

  it("redirects a Member away from /admin/transcoding (RequireAdmin gate)", async () => {
    window.localStorage.setItem("juicebox.token", "fake-token");
    window.localStorage.setItem(
      "juicebox.user",
      JSON.stringify({ id: "m1", username: "kid", role: "member" }),
    );
    const stub = {
      token: "fake-token",
      setToken: () => {},
      setUnauthorizedHandler: () => {},
      verifySession: () => Promise.resolve({}),
    } as unknown as ApiClient;

    render(
      <MemoryRouter initialEntries={["/admin/transcoding"]}>
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

    await waitFor(() =>
      expect(screen.getByTestId("landing")).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("admin-transcoding")).not.toBeInTheDocument();
    expect(getTranscoding).not.toHaveBeenCalled();
  });
});
