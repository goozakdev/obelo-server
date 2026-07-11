import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { errorMessage } from "../screens/errorMessage";
import type { TranscodingSnapshot } from "../api/types";

// The Transcoding surface (ADR-0029), behind RequireAdmin (App.tsx). A read-only
// status panel over GET /api/v1/transcoding: what Transcode backend the server
// actually resolved and validated at boot, with a prominent Degraded badge when a
// hardware backend was requested but the server silently fell back to CPU — the
// signal that today lives only in a boot log line. Later slices add live load and
// GPU telemetry to the same snapshot; they ride this same poll.
//
// There is no SSE in v1 (ADR-0016 deferred): the screen polls on a short interval
// while mounted and stops on unmount, so an idle server isn't sampled when no one
// is watching.

/** How often the snapshot is re-polled (ms) — ~4s per ADR-0029's polling note:
 * frequent enough to watch load/utilization move, light enough for a low-traffic
 * screen an admin holds open for a minute. */
export const TRANSCODING_POLL_INTERVAL_MS = 4000;

type SnapshotState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ready"; snapshot: TranscodingSnapshot };

/** Human labels for the backend vocabulary; unknown values fall through verbatim. */
const BACKEND_LABELS: Record<string, string> = {
  cpu: "CPU (libx264)",
  nvenc: "NVENC",
  vaapi: "VAAPI",
  qsv: "Quick Sync",
  videotoolbox: "VideoToolbox",
  auto: "auto",
  off: "off",
};

function backendLabel(v: string): string {
  return BACKEND_LABELS[v] ?? v;
}

export default function AdminTranscodingScreen({
  intervalMs = TRANSCODING_POLL_INTERVAL_MS,
}: {
  intervalMs?: number;
} = {}) {
  const [state, setState] = useState<SnapshotState>({ status: "loading" });
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;

    // One poll: on success show the snapshot (clearing any prior error); on
    // failure surface the message but keep polling, so a transient blip recovers
    // on the next tick rather than freezing the panel.
    const poll = async () => {
      try {
        const snapshot = await apiClient.getTranscoding();
        if (!mountedRef.current) return;
        setState({ status: "ready", snapshot });
      } catch (err) {
        if (!mountedRef.current) return;
        setState((cur) =>
          cur.status === "ready"
            ? cur
            : { status: "error", message: errorMessage(err) },
        );
      }
    };

    void poll();
    const timer = setInterval(() => void poll(), intervalMs);
    return () => {
      mountedRef.current = false;
      clearInterval(timer);
    };
  }, [intervalMs]);

  return (
    <section className="admin-transcoding" data-testid="admin-transcoding">
      <h2 className="section-title">Transcoding</h2>

      {state.status === "loading" && (
        <p className="status status-loading" data-testid="transcoding-loading">
          Loading transcoding status&hellip;
        </p>
      )}
      {state.status === "error" && (
        <p
          className="status status-error"
          data-testid="transcoding-error"
          role="alert"
        >
          <span className="dot dot-error" aria-hidden="true" />
          {state.message}
        </p>
      )}
      {state.status === "ready" && (
        <>
          <BackendPanel backend={state.snapshot.backend} />
          <LoadPanel load={state.snapshot.load} />
          <GpuPanel gpu={state.snapshot.gpu} />
        </>
      )}
    </section>
  );
}

function BackendPanel({
  backend,
}: {
  backend: TranscodingSnapshot["backend"];
}) {
  return (
    <div className="transcoding-panel card" data-testid="transcoding-backend">
      <div className="transcoding-headline">
        <span className="transcoding-label">Transcode backend</span>
        <span
          className="transcoding-backend-active"
          data-testid="transcoding-active"
          data-backend={backend.active}
        >
          {backendLabel(backend.active)}
        </span>
        {backend.degraded && (
          <span
            className="transcoding-badge transcoding-badge-degraded"
            data-testid="transcoding-degraded-badge"
          >
            Degraded
          </span>
        )}
      </div>

      <dl className="transcoding-detail">
        <div className="transcoding-detail-row">
          <dt>Requested</dt>
          <dd data-testid="transcoding-requested">
            {backendLabel(backend.requested)}
          </dd>
        </div>
        {backend.reason && (
          <div className="transcoding-detail-row">
            <dt>Reason</dt>
            <dd data-testid="transcoding-reason">{backend.reason}</dd>
          </div>
        )}
      </dl>
    </div>
  );
}

function LoadPanel({ load }: { load: TranscodingSnapshot["load"] }) {
  // A cap of 0 means the operator disabled the limit — render "unlimited", never
  // "0", so the readout is never misleading.
  const capLabel = load.cap === 0 ? "unlimited" : String(load.cap);
  return (
    <div className="transcoding-panel card" data-testid="transcoding-load">
      <div className="transcoding-headline">
        <span className="transcoding-label">Transcode load</span>
        <span className="transcoding-load-count" data-testid="transcoding-load-count">
          <span data-testid="transcoding-load-active">{load.active}</span>
          {" / "}
          <span data-testid="transcoding-load-cap">{capLabel}</span>
        </span>
        {load.atCapacity && (
          <span
            className="transcoding-badge transcoding-badge-degraded"
            data-testid="transcoding-at-capacity-badge"
          >
            At capacity
          </span>
        )}
      </div>
      <p className="transcoding-load-note">
        {load.atCapacity
          ? "The server is at its transcode cap and is rejecting new transcodes."
          : "Full transcodes running now. Direct play and direct stream carry no load."}
      </p>
    </div>
  );
}

/** A nullable numeric field: "37%", "1240 MB", etc., or an em-dash when the tool
 * didn't report that column. */
function num(value: number | null, suffix = ""): string {
  return value === null ? "—" : `${value}${suffix}`;
}

/** Whole seconds since an RFC3339 timestamp, floored at 0. */
function secondsAgo(rfc3339: string): number {
  const then = Date.parse(rfc3339);
  if (Number.isNaN(then)) return 0;
  return Math.max(0, Math.round((Date.now() - then) / 1000));
}

function GpuPanel({ gpu }: { gpu: TranscodingSnapshot["gpu"] }) {
  if (gpu === null) {
    // One honest, all-or-nothing unavailable line — non-NVENC backend, nvidia-smi
    // absent, or a probe error. Not an error state.
    return (
      <div className="transcoding-panel card" data-testid="transcoding-gpu">
        <div className="transcoding-headline">
          <span className="transcoding-label">GPU telemetry</span>
        </div>
        <p
          className="transcoding-load-note"
          data-testid="transcoding-gpu-unavailable"
        >
          GPU telemetry unavailable
        </p>
      </div>
    );
  }

  const ago = secondsAgo(gpu.sampledAt);
  return (
    <div className="transcoding-panel card" data-testid="transcoding-gpu">
      <div className="transcoding-headline">
        <span className="transcoding-label">GPU telemetry</span>
        <span
          className="transcoding-gpu-freshness"
          data-testid="transcoding-gpu-freshness"
        >
          as of {ago}s ago
        </span>
      </div>

      <dl className="transcoding-detail">
        <div className="transcoding-detail-row">
          <dt>Utilization</dt>
          <dd data-testid="transcoding-gpu-utilization">
            {num(gpu.utilizationPct, "%")}
          </dd>
        </div>
        <div className="transcoding-detail-row">
          <dt>VRAM</dt>
          <dd data-testid="transcoding-gpu-vram">
            {num(gpu.vramUsedMb)} / {num(gpu.vramTotalMb, " MB")}
          </dd>
        </div>
        <div className="transcoding-detail-row">
          <dt>Encoder sessions</dt>
          <dd data-testid="transcoding-gpu-sessions">
            {num(gpu.encoderSessions)}
          </dd>
        </div>
        <div className="transcoding-detail-row">
          <dt>Driver</dt>
          <dd data-testid="transcoding-gpu-driver">
            {gpu.driverVersion ?? "—"}
          </dd>
        </div>
      </dl>
    </div>
  );
}
