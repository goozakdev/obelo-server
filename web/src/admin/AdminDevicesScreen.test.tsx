import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithAuth } from "../test/renderWithAuth";
import { ApiError } from "../api/errors";
import type { Device } from "../api/types";

// AdminDevicesScreen through the faked API client (the one seam): the list
// renders the user's devices; revoking one calls DELETE /devices/{id} and drops
// the row; a failed revoke surfaces a readable error and keeps the row.

const { listDevices, deleteDevice } = vi.hoisted(() => ({
  listDevices: vi.fn(),
  deleteDevice: vi.fn(),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    ...actual,
    apiClient: {
      listDevices: (...a: unknown[]) => listDevices(...a),
      deleteDevice: (...a: unknown[]) => deleteDevice(...a),
    },
  };
});

import AdminDevicesScreen from "./AdminDevicesScreen";

function device(over: Partial<Device>): Device {
  return {
    id: "d1",
    name: "Living Room",
    platform: "web",
    clientId: "c1",
    lastSeenAt: "2026-06-23T10:00:00Z",
    ...over,
  };
}

beforeEach(() => {
  listDevices.mockReset();
  deleteDevice.mockReset();
});

describe("AdminDevicesScreen", () => {
  it("lists the user's devices with name/platform/last-seen", async () => {
    listDevices.mockResolvedValue([
      device({ id: "d1", name: "Living Room", platform: "web" }),
      device({ id: "d2", name: "Phone", platform: "android" }),
    ]);
    renderWithAuth(<AdminDevicesScreen />, { initialEntries: ["/admin/devices"] });

    await waitFor(() =>
      expect(screen.getByTestId("devices-list")).toBeInTheDocument(),
    );
    const items = screen.getAllByTestId("device-item");
    expect(items).toHaveLength(2);
    expect(within(items[0]).getByTestId("device-name")).toHaveTextContent("Living Room");
    expect(within(items[0]).getByTestId("device-platform")).toHaveTextContent("web");
    expect(within(items[0]).getByTestId("device-last-seen")).toHaveTextContent(/last seen/i);
  });

  it("shows an empty state with no devices", async () => {
    listDevices.mockResolvedValue([]);
    renderWithAuth(<AdminDevicesScreen />, { initialEntries: ["/admin/devices"] });
    await waitFor(() =>
      expect(screen.getByTestId("devices-empty")).toBeInTheDocument(),
    );
  });

  it("revokes a device and removes its row", async () => {
    const user = userEvent.setup();
    listDevices.mockResolvedValue([
      device({ id: "d1", name: "Living Room" }),
      device({ id: "d2", name: "Phone" }),
    ]);
    deleteDevice.mockResolvedValue(undefined);

    renderWithAuth(<AdminDevicesScreen />, { initialEntries: ["/admin/devices"] });
    await waitFor(() =>
      expect(screen.getAllByTestId("device-item")).toHaveLength(2),
    );

    const first = screen.getAllByTestId("device-item")[0];
    await user.click(within(first).getByTestId("device-revoke"));

    await waitFor(() => expect(deleteDevice).toHaveBeenCalledWith("d1"));
    await waitFor(() =>
      expect(screen.getAllByTestId("device-item")).toHaveLength(1),
    );
    expect(screen.getByText("Phone")).toBeInTheDocument();
    expect(screen.queryByText("Living Room")).not.toBeInTheDocument();
  });

  it("keeps the row and shows a readable error when revoke fails", async () => {
    const user = userEvent.setup();
    listDevices.mockResolvedValue([device({ id: "d1", name: "Living Room" })]);
    deleteDevice.mockImplementation(() =>
      Promise.reject(new ApiError(500, "INTERNAL", "could not revoke device")),
    );

    renderWithAuth(<AdminDevicesScreen />, { initialEntries: ["/admin/devices"] });
    await waitFor(() =>
      expect(screen.getByTestId("device-item")).toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("device-revoke"));
    const err = await screen.findByTestId("device-revoke-error");
    expect(err).toHaveTextContent(/could not revoke/i);
    // The row survives a failed revoke.
    expect(screen.getByTestId("device-item")).toBeInTheDocument();
  });
});
