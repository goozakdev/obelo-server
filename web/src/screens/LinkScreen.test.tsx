import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Route, Routes, useNavigate } from "react-router-dom";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/client";

// LinkScreen — the phone's half of the Device authorization grant (ADR-0036).
// The apiClient is the one seam; everything else is real, including the router,
// so the /link/:code path parameter is exercised rather than assumed.

const { approveDeviceCode } = vi.hoisted(() => ({ approveDeviceCode: vi.fn() }));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      approveDeviceCode: (...a: unknown[]) => approveDeviceCode(...a),
    },
  };
});

import LinkScreen from "./LinkScreen";

function renderAt(path: string) {
  return renderWithAuth(
    <Routes>
      <Route path="/link/:code?" element={<LinkScreen />} />
    </Routes>,
    { initialEntries: [path] },
  );
}

beforeEach(() => {
  approveDeviceCode.mockReset();
});

describe("LinkScreen", () => {
  it("approves immediately when the code arrives in the URL (the scanned-QR path)", async () => {
    approveDeviceCode.mockResolvedValue({
      device: { name: "Living Room TV", platform: "tvos" },
    });

    renderAt("/link/K7R9");

    await waitFor(() => expect(screen.getByTestId("link-approved")).toBeInTheDocument());
    expect(approveDeviceCode).toHaveBeenCalledWith("K7R9");
    // The success state must NAME the device: with no confirmation step before
    // approval, this is the only place a user can notice they signed in a TV
    // they did not mean to.
    expect(screen.getByTestId("link-approved-device")).toHaveTextContent("Living Room TV");
  });

  it("approves a scanned code exactly once", async () => {
    approveDeviceCode.mockResolvedValue({
      device: { name: "Living Room TV", platform: "tvos" },
    });

    renderAt("/link/K7R9");

    await waitFor(() => expect(screen.getByTestId("link-approved")).toBeInTheDocument());
    // A second approval would hit an already-redeemed (one-shot) code and report
    // a spurious failure over the real success.
    expect(approveDeviceCode).toHaveBeenCalledTimes(1);
  });

  it("asks for the code when there isn't one in the URL", async () => {
    approveDeviceCode.mockResolvedValue({
      device: { name: "Bedroom TV", platform: "tvos" },
    });

    renderAt("/link");

    expect(screen.getByTestId("link-screen")).toBeInTheDocument();
    expect(approveDeviceCode).not.toHaveBeenCalled();

    await userEvent.type(screen.getByTestId("link-code"), "K7R9");
    await userEvent.click(screen.getByTestId("link-submit"));

    await waitFor(() => expect(screen.getByTestId("link-approved")).toBeInTheDocument());
    expect(approveDeviceCode).toHaveBeenCalledWith("K7R9");
  });

  it("shows the server's message when the code is rejected", async () => {
    approveDeviceCode.mockRejectedValue(
      new ApiError(404, "INVALID_USER_CODE", "that code is not valid; check the code on your TV"),
    );

    renderAt("/link/ZZZZ");

    await waitFor(() => expect(screen.getByTestId("link-error")).toBeInTheDocument());
    expect(screen.getByTestId("link-error")).toHaveTextContent("that code is not valid");
    // Failure falls back to the entry form rather than a dead end — the code on
    // the TV is still live, and retyping it is the whole recovery.
    expect(screen.getByTestId("link-code")).toBeInTheDocument();
  });

  it("surfaces the rate limit rather than looking broken", async () => {
    approveDeviceCode.mockRejectedValue(
      new ApiError(429, "TOO_MANY_ATTEMPTS", "too many incorrect codes; wait a few minutes and try again"),
    );

    renderAt("/link/ZZZZ");

    await waitFor(() => expect(screen.getByTestId("link-error")).toBeInTheDocument());
    expect(screen.getByTestId("link-error")).toHaveTextContent("too many incorrect codes");
  });
});

describe("LinkScreen code changes (R06-13)", () => {
  it("approves the new code when :code changes on the mounted screen", async () => {
    approveDeviceCode.mockResolvedValue({ device: { name: "TV", platform: "tvos" } });
    function Hop() {
      const nav = useNavigate();
      return <button data-testid="hop" onClick={() => nav("/link/NEW2")} />;
    }
    renderWithAuth(
      <>
        <Hop />
        <Routes>
          <Route path="/link/:code?" element={<LinkScreen />} />
        </Routes>
      </>,
      { initialEntries: ["/link/OLD1"] },
    );
    await waitFor(() => expect(approveDeviceCode).toHaveBeenCalledWith("OLD1"));
    await userEvent.click(screen.getByTestId("hop"));
    await waitFor(() => expect(approveDeviceCode).toHaveBeenCalledWith("NEW2"));
    expect(approveDeviceCode).toHaveBeenCalledTimes(2);
  });
  it("keeps the later code's result when an earlier approval settles last", async () => {
    let finishOld!: (v: unknown) => void;
    approveDeviceCode.mockImplementation((code: string) =>
      code === "OLD1"
        ? new Promise((resolve) => {
            finishOld = resolve;
          })
        : Promise.resolve({ device: { name: "New TV", platform: "tvos" } }),
    );
    function Hop() {
      const nav = useNavigate();
      return <button data-testid="hop" onClick={() => nav("/link/NEW2")} />;
    }
    renderWithAuth(
      <>
        <Hop />
        <Routes>
          <Route path="/link/:code?" element={<LinkScreen />} />
        </Routes>
      </>,
      { initialEntries: ["/link/OLD1"] },
    );
    await waitFor(() => expect(approveDeviceCode).toHaveBeenCalledWith("OLD1"));
    await userEvent.click(screen.getByTestId("hop"));
    await waitFor(() => expect(screen.getByTestId("link-approved-device")).toHaveTextContent("New TV"));

    await act(async () => {
      finishOld({ device: { name: "Old TV", platform: "tvos" } });
    });
    expect(screen.getByTestId("link-approved-device")).toHaveTextContent("New TV");
  });
  it("keeps the later code's result when an earlier approval fails last", async () => {
    let failOld!: (e: unknown) => void;
    approveDeviceCode.mockImplementation((code: string) =>
      code === "OLD1"
        ? new Promise((_resolve, reject) => {
            failOld = reject;
          })
        : Promise.resolve({ device: { name: "New TV", platform: "tvos" } }),
    );
    function Hop() {
      const nav = useNavigate();
      return <button data-testid="hop" onClick={() => nav("/link/NEW2")} />;
    }
    renderWithAuth(
      <>
        <Hop />
        <Routes>
          <Route path="/link/:code?" element={<LinkScreen />} />
        </Routes>
      </>,
      { initialEntries: ["/link/OLD1"] },
    );
    await waitFor(() => expect(approveDeviceCode).toHaveBeenCalledWith("OLD1"));
    await userEvent.click(screen.getByTestId("hop"));
    await waitFor(() => expect(screen.getByTestId("link-approved-device")).toHaveTextContent("New TV"));

    await act(async () => {
      failOld(new Error("late failure"));
    });
    expect(screen.queryByTestId("link-error")).not.toBeInTheDocument();
    expect(screen.getByTestId("link-approved-device")).toHaveTextContent("New TV");
  });
});
