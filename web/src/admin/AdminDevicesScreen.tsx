import { useCallback, useEffect, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import { formatDateTime } from "../time";
import type { Device } from "../api/types";

// The Devices surface (issue 07), behind RequireAdmin (App.tsx). Lists the
// signed-in User's registered Devices (name / platform / last-seen) and lets the
// Admin revoke one — DELETE /devices/{id} invalidates that Device's token
// immediately (a subsequent call with it is rejected). On a successful revoke we
// drop the row locally (the server is the source of truth, but a refetch would
// risk a flash; the 204 confirms removal).
//
// A small reloadable loader (not the load-once useAsync) because this screen
// mutates the very list it shows.

type ListState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; devices: Device[] };

export default function AdminDevicesScreen() {
  const [state, setState] = useState<ListState>({ status: "loading" });

  const load = useCallback(async (signal?: AbortSignal) => {
    setState({ status: "loading" });
    try {
      const devices = await apiClient.listDevices(signal);
      if (signal?.aborted) return;
      setState({ status: "ready", devices });
    } catch (err) {
      if (signal?.aborted) return;
      setState({ status: "error", message: errorMessage(err) });
    }
  }, []);

  useEffect(() => {
    const ctrl = new AbortController();
    void load(ctrl.signal);
    return () => ctrl.abort();
  }, [load]);

  return (
    <section className="admin-devices" data-testid="admin-devices">
      <h2 className="section-title">Devices</h2>

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="devices-loading">
          Loading devices&hellip;
        </p>
      )}
      {state.status === "error" && (
        <p className="status status-error" data-testid="devices-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}
      {state.status === "ready" && state.devices.length === 0 && (
        <p className="status status-empty" data-testid="devices-empty">
          No devices registered.
        </p>
      )}
      {state.status === "ready" && state.devices.length > 0 && (
        <ul className="devices-list" data-testid="devices-list">
          {state.devices.map((d) => (
            <DeviceRow
              key={d.id}
              device={d}
              onRevoked={() =>
                setState((cur) =>
                  cur.status === "ready"
                    ? { ...cur, devices: cur.devices.filter((x) => x.id !== d.id) }
                    : cur,
                )
              }
            />
          ))}
        </ul>
      )}
    </section>
  );
}

function DeviceRow({
  device,
  onRevoked,
}: {
  device: Device;
  onRevoked: () => void;
}) {
  const [revoking, setRevoking] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onRevoke() {
    if (revoking) return;
    setRevoking(true);
    setError(null);
    try {
      await apiClient.deleteDevice(device.id);
      onRevoked();
    } catch (err) {
      setError(errorMessage(err));
      setRevoking(false);
    }
  }

  const lastSeen = formatDateTime(device.lastSeenAt);

  return (
    <li className="device-item card" data-testid="device-item" data-device-id={device.id}>
      <span className="device-name" data-testid="device-name">
        {device.name}
      </span>
      <span className="device-platform" data-testid="device-platform">
        {device.platform}
      </span>
      {lastSeen && (
        <span className="device-last-seen" data-testid="device-last-seen">
          last seen {lastSeen}
        </span>
      )}
      <button
        className="nav-link nav-logout"
        type="button"
        data-testid="device-revoke"
        onClick={onRevoke}
        disabled={revoking}
      >
        {revoking ? "Revoking…" : "Revoke"}
      </button>
      {error && (
        <p className="status status-error" data-testid="device-revoke-error" role="alert">
          <span className="dot dot-error" aria-hidden="true" />
          {error}
        </p>
      )}
    </li>
  );
}
