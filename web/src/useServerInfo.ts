import { useEffect, useState } from "react";
import { apiClient, type ApiClient } from "./api/client";
import { ApiError, NetworkError } from "./api/errors";
import type { ServerInfo } from "./api/types";

export type ServerState =
  | { status: "loading" }
  | { status: "ready"; info: ServerInfo }
  | { status: "unreachable"; message: string }
  | { status: "error"; code: string; message: string };

// useServerInfo runs the handshake (GET /api/v1/server) on mount and exposes a
// discriminated state the shell renders from. It distinguishes a reachable
// server (ready) from an unreachable one (NetworkError) from a server-side
// error (ApiError) so the UI can say something honest in each case.
export function useServerInfo(client: ApiClient = apiClient): ServerState {
  const [state, setState] = useState<ServerState>({ status: "loading" });

  useEffect(() => {
    const controller = new AbortController();
    let active = true;

    client
      .getServerInfo(controller.signal)
      .then((info) => {
        if (active) setState({ status: "ready", info });
      })
      .catch((err: unknown) => {
        if (!active) return;
        if (err instanceof DOMException && err.name === "AbortError") return;
        if (err instanceof NetworkError) {
          setState({ status: "unreachable", message: err.message });
        } else if (err instanceof ApiError) {
          setState({ status: "error", code: err.code, message: err.message });
        } else {
          setState({
            status: "error",
            code: "UNKNOWN",
            message: err instanceof Error ? err.message : String(err),
          });
        }
      });

    return () => {
      active = false;
      controller.abort();
    };
  }, [client]);

  return state;
}
