import { useEffect, useRef, useState } from "react";
import { apiClient } from "../api/client";
import { ApiError, isAbort } from "../api/errors";
import type { OnlinePlayback } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { deriveCapabilityProfile } from "./capabilities";
import { attachHls } from "./hls";
import { useQueue } from "./queue/useQueue";
import type { OnlineQueueItem } from "./queue/model";

// The persistent player's core for an ONLINE item (ADR-0068, ADR-0018). The bar
// (NowPlayingBar) is the shell-owned player; this is what it mounts in place of
// the Title core when the current Queue entry is an Online item, keyed by the
// entry like that core so a new play ends this session before the next starts.
//
// An Online item is deliberately a smaller thing than a Title: no resume
// position, no watch state, no Markers, lyrics, subtitles or Up Next, and no
// stream picks — the server relays one variant the browser can play as-is, or
// encodes it with ffmpeg and serves HLS, or says it cannot. The stream URL carries
// a stream token, so the <video> needs no header or cookie. Progress reports exist only as the session keepalive; the
// server records nothing from them.

const KEEPALIVE_MS = 12_000;

type Status =
  | { kind: "negotiating" }
  | { kind: "ready"; playback: OnlinePlayback }
  | { kind: "unsupported"; message: string }
  | { kind: "error"; message: string };

export default function OnlinePlayer({ item }: { item: OnlineQueueItem }) {
  const queue = useQueue();
  const [status, setStatus] = useState<Status>({ kind: "negotiating" });
  const videoRef = useRef<HTMLVideoElement | null>(null);

  useEffect(() => {
    let cancelled = false;
    let sessionId: string | null = null;
    let timer: ReturnType<typeof setInterval> | null = null;

    void (async () => {
      try {
        const { deviceProfile, constraints } = deriveCapabilityProfile();
        const playback = await apiClient.startOnlinePlayback(
          item.sourceId,
          item.itemId,
          { deviceProfile, constraints },
          new AbortController().signal,
        );
        if (cancelled) {
          // Abandoned in flight: the server opened a session nobody will use.
          void Promise.resolve(apiClient.endSession(playback.sessionId)).catch(() => {});
          return;
        }
        sessionId = playback.sessionId;
        setStatus({ kind: "ready", playback });
        timer = setInterval(() => {
          const v = videoRef.current;
          void Promise.resolve(
            apiClient.reportProgress(playback.sessionId, {
              positionMs: v ? Math.floor(v.currentTime * 1000) : 0,
              state: v && !v.paused ? "playing" : "paused",
            }),
          ).catch(() => {});
        }, KEEPALIVE_MS);
      } catch (err) {
        if (cancelled || isAbort(err)) return;
        if (err instanceof ApiError && err.code === "TRANSCODE_REQUIRED") {
          setStatus({
            kind: "unsupported",
            message: `${item.sourceName} can't be played in this browser — it would need a transcode, which online sources don't support yet.`,
          });
          return;
        }
        if (err instanceof ApiError && err.code === "SERVER_BUSY") {
          setStatus({
            kind: "error",
            message: `${item.sourceName} can't be played right now — the server is busy transcoding. Try again in a moment.`,
          });
          return;
        }
        if (err instanceof ApiError && err.code === "SOURCE_UNAVAILABLE") {
          setStatus({ kind: "error", message: `${item.sourceName} isn't responding.` });
          return;
        }
        setStatus({ kind: "error", message: errorMessage(err) });
      }
    })();

    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
      if (sessionId) void Promise.resolve(apiClient.endSession(sessionId)).catch(() => {});
    };
  }, [item.sourceId, item.itemId, item.sourceName]);

  // An encoded item is an HLS playlist: hls.js (or native HLS) is attached to the
  // element, which then carries no src of its own.
  useEffect(() => {
    if (status.kind !== "ready" || status.playback.format !== "hls") return;
    const v = videoRef.current;
    if (!v) return;
    let cancelled = false;
    let attachment: { detach(): void } | null = null;
    void attachHls(v, status.playback.streamUrl, {
      onFatal: (reason) => setStatus({ kind: "error", message: `Playback stopped: ${reason}` }),
    })
      .then((a) => {
        if (cancelled) a.detach();
        else attachment = a;
      })
      .catch((err) => {
        if (!cancelled) setStatus({ kind: "error", message: errorMessage(err) });
      });
    return () => {
      cancelled = true;
      attachment?.detach();
    };
  }, [status]);

  return (
    <div className="now-playing-inner now-playing-online" data-testid="online-player">
      <div className="now-playing-left">
        <div className="now-playing-thumb">
          <img src={item.thumbnailUrl} alt="" className="poster-img" />
        </div>
        <div className="now-playing-labels">
          <span className="now-playing-title" data-testid="now-playing-title">
            {item.title}
          </span>
          <span className="now-playing-context" data-testid="now-playing-context">
            {item.sourceName}
          </span>
        </div>
      </div>
      <div className="now-playing-center">
        {status.kind === "negotiating" && (
          <span className="status status-loading" data-testid="online-player-loading">
            Starting&hellip;
          </span>
        )}
        {(status.kind === "unsupported" || status.kind === "error") && (
          <span className="status status-error" role="alert" data-testid="online-player-error">
            {status.message}
          </span>
        )}
        {status.kind === "ready" && (
          <video
            ref={videoRef}
            className="player-video now-playing-online-video"
            data-testid="player-video"
            data-stream-url={status.playback.streamUrl}
            src={status.playback.format === "hls" ? undefined : status.playback.streamUrl}
            controls
            autoPlay
            playsInline
          />
        )}
      </div>
      <div className="now-playing-right">
        <button
          type="button"
          className="now-playing-stop"
          data-testid="online-player-stop"
          onClick={() => queue.clear()}
        >
          Stop
        </button>
      </div>
    </div>
  );
}
