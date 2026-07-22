import { describe, it, expect } from "vitest";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import {
  readFeature,
  useFeature,
  ServerInfoStateProvider,
} from "./serverInfoContext";
import type { ServerState } from "./useServerInfo";
import type { ServerInfo } from "./api/types";

// The handshake feature-gate (Apple TV → Web parity §4). A client gates
// capabilities on the advertised `features` flags, never on the server version:
// present-and-true is the only "on"; an absent flag, a missing/empty map, or a
// not-yet-ready handshake are all "off".

function info(features: ServerInfo["features"]): ServerInfo {
  return {
    version: "test",
    supportedVersions: [1],
    features,
    setupRequired: false,
  };
}

describe("readFeature", () => {
  it("is true only for an advertised present-and-true flag", () => {
    expect(readFeature(info({ remuxSelectedOnly: true }), "remuxSelectedOnly")).toBe(
      true,
    );
  });

  it("is false for an absent flag", () => {
    expect(readFeature(info({ playlists: true }), "remuxSelectedOnly")).toBe(false);
  });

  it("is false for a flag explicitly advertised false", () => {
    expect(readFeature(info({ transcode: false }), "transcode")).toBe(false);
  });

  it("tolerates an empty features map", () => {
    expect(readFeature(info({}), "remuxSelectedOnly")).toBe(false);
  });

  it("tolerates a missing features map", () => {
    // An older/partial handshake with no `features` key at all must not throw.
    const partial = { version: "old", supportedVersions: [1], setupRequired: false };
    expect(readFeature(partial as unknown as ServerInfo, "playlists")).toBe(false);
  });

  it("tolerates an undefined handshake (not yet fetched)", () => {
    expect(readFeature(undefined, "playlists")).toBe(false);
  });
});

function wrapper(state: ServerState) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <ServerInfoStateProvider state={state}>{children}</ServerInfoStateProvider>
    );
  };
}

describe("useFeature", () => {
  it("returns true for an advertised flag and false for an absent one", () => {
    const state: ServerState = {
      status: "ready",
      info: info({ playlists: true, transcode: false }),
    };
    const playlists = renderHook(() => useFeature("playlists"), {
      wrapper: wrapper(state),
    });
    const transcode = renderHook(() => useFeature("transcode"), {
      wrapper: wrapper(state),
    });
    const missing = renderHook(() => useFeature("remuxSelectedOnly"), {
      wrapper: wrapper(state),
    });
    expect(playlists.result.current).toBe(true);
    expect(transcode.result.current).toBe(false);
    expect(missing.result.current).toBe(false);
  });

  it("is false while the handshake is still loading", () => {
    const { result } = renderHook(() => useFeature("playlists"), {
      wrapper: wrapper({ status: "loading" }),
    });
    expect(result.current).toBe(false);
  });

  it("is false when the handshake is unreachable or errored", () => {
    const unreachable = renderHook(() => useFeature("playlists"), {
      wrapper: wrapper({ status: "unreachable", message: "offline" }),
    });
    const errored = renderHook(() => useFeature("playlists"), {
      wrapper: wrapper({ status: "error", code: "BOOM", message: "nope" }),
    });
    expect(unreachable.result.current).toBe(false);
    expect(errored.result.current).toBe(false);
  });
});
