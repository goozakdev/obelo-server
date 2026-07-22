import { useCallback, useEffect, useRef, useState } from "react";
import type { ApiClient } from "../api/client";
import { ApiError } from "../api/errors";
import type { PlaybackConstraints, PlaybackDecision, PlaybackState } from "../api/types";
import { errorMessage } from "../screens/errorMessage";
import { deriveCapabilityProfile } from "./capabilities";

// Owns one Playback session's whole lifecycle for the Now Playing bar's player:
//
//   1. NEGOTIATE — on mount, derive the capability profile from the browser and
//      POST /titles/{id}/playback. Outcomes:
//        - a decision (directPlay / directStream / transcode) → expose it; the
//          component renders <video> (progressive for directPlay, hls.js for the
//          HLS tiers — it branches on decision.tier).
//        - 503 SERVER_BUSY → expose a `busy` status carrying suggestedMaxBitrate.
//          The hook AUTO-RETRIES ONCE at that lower bitrate (a meaningful step
//          down that may drop a tier or shrink the transcode); if it's still
//          busy it stays in `busy` so the component offers a manual retry. No
//          infinite loop — exactly one automatic attempt.
//        - TRANSCODE_REQUIRED (501) → a genuinely unplayable Title (the server
//          can neither remux nor transcode it); expose `unsupported` (+ reason)
//          so the component shows the honest "can't play" state. With the HLS
//          tiers live this is now rare (the server remuxes/transcodes most
//          files), so it is reserved for true negotiation failures.
//        - any other error → a render-readable message.
//   2. PROGRESS — once playing, POST /sessions/{id}/progress with the RAW
//      position on a ~12s interval AND on pause/seek/ended (the component calls
//      report()). The SERVER applies the Watched threshold; we never compute
//      "watched". A page-unload (beforeunload) fires a best-effort final report.
//   3. STOP — DELETE /sessions/{id} on unmount / navigation away, after a final
//      progress report, so the session is cleanly ended (not just reaped).
//
// Progress/resume/stop are tier-agnostic: the client reports raw positionMs and
// the server owns the watched threshold, so the HLS path uses the exact same
// wiring as direct play.
//
// The hook is deliberately UI-free: it returns the decision/state plus
// imperative report()/stop() the <video> event handlers call, and a retry() the
// busy state's manual button calls. Tests drive those handlers directly without
// a real media element.

const PROGRESS_INTERVAL_MS = 12_000;

export type PlayerStatus =
  | { kind: "negotiating" }
  | { kind: "ready"; decision: PlaybackDecision }
  | { kind: "busy"; suggestedMaxBitrate?: number; retrying: boolean; message: string }
  | { kind: "unsupported"; reason: string; message: string }
  | { kind: "error"; message: string };

export interface PlayerSession {
  status: PlayerStatus;
  /** The IMAGE Subtitle track currently burned into the video (ADR-0020,
   * subtitles/04), or null when none is burned. Burning is a fresh negotiation —
   * the component reads this to mark the selected image track in the captions menu
   * and to know a burn is active. */
  burnSubtitleId: string | null;
  /** The audio Stream the LAST negotiation was asked to deliver (audio-streams/04),
   * or null to take the server-resolved default. On direct play a non-default pick
   * escalates via {@link selectAudioStream} (a fresh negotiation into remux); on the
   * HLS tiers an in-band pick is recorded via {@link noteAudioStream} without a
   * restart. Carried on every subsequent re-negotiation (a burn switch) so the audio
   * choice survives it. */
  audioStreamId: string | null;
  /** The video Stream the LAST negotiation was asked to deliver (selectable-video/03),
   * or null to take the server-resolved capability-then-quality default. A pick
   * escalates via {@link selectVideoStream} (a fresh negotiation — there is no in-band
   * video rendition). Carried on every subsequent re-negotiation (a burn/audio switch)
   * so the video choice survives it. */
  videoStreamId: string | null;
  /** Report the current raw position + play state. Debounced only by the caller
   * (we send whatever it passes). No-op before a session exists. */
  report: (positionMs: number, state: PlaybackState) => void;
  /** Begin the periodic progress interval (call once playback starts). */
  startReporting: (getPositionMs: () => number) => void;
  /** Stop the periodic interval (e.g. on pause), without ending the session. */
  stopReporting: () => void;
  /** Final report + DELETE the session. Idempotent; safe to call on unmount and
   * from an explicit Stop. */
  end: (finalPositionMs?: number) => void;
  /** Re-run negotiation from the `busy` state (the manual "Try again" button).
   * Retries at the last suggested lower bitrate so the request is a real step
   * down, not a repeat of the rejected one. */
  retry: () => void;
  /** Select an IMAGE Subtitle track to burn in (id), or clear the burn (null),
   * resuming near `positionMs` (ADR-0020, subtitles/04). It ENDS the current
   * session cleanly (final report + DELETE) and re-negotiates: selecting an image
   * sub escalates to a burned-in transcode (which may go `busy`); clearing it
   * restarts on the cheapest tier. The choice is remembered as `burnSubtitleId`.
   * A no-op when the id is already the active burn. */
  selectBurnSubtitle: (id: string | null, positionMs: number) => void;
  /** Select a non-default audio Stream on a DIRECT-PLAY session (audio-streams/04,
   * ADR-0022) — the one escalating switch. Direct play carries only the default
   * audio, so this ENDS the current session cleanly (final report + DELETE) and
   * re-negotiates with `audioStreamId`, escalating to remux (or a governed
   * transcode → the busy state's retry UX) and resuming near `positionMs`. After it
   * lands the session is demuxed HLS, so all further switches are in-band
   * ({@link noteAudioStream}). A no-op when the id is already the requested audio. */
  selectAudioStream: (id: string, positionMs: number) => void;
  /** Record an IN-BAND audio pick made client-side on the HLS tiers (no restart,
   * no server round-trip — the HLS player switches renditions instantly). It only
   * updates the remembered `audioStreamId` so a LATER re-negotiation (e.g. a burn
   * switch) carries the choice forward; it never itself re-negotiates. */
  noteAudioStream: (id: string) => void;
  /** Select a video Stream (selectable-video/03, ADR-0025) — always an escalating
   * switch, because HLS has no in-band video rendition (unlike audio). It ENDS the
   * current session cleanly (final report + DELETE) and re-negotiates with
   * `videoStreamId`, preserving the current `audioStreamId`, `burnSubtitleId`, and
   * resuming near `positionMs`. A non-default pick escalates to HLS remux
   * (`-map 0:v:N`); a Stream the browser can't decode escalates to a governed
   * transcode → the busy state's retry UX. Reads as a brief re-buffer (the image-
   * subtitle model), not the instant audio flip. A no-op when the id is already the
   * requested video. */
  selectVideoStream: (id: string, positionMs: number) => void;
  /** Recover a VANISHED session by re-negotiating a fresh one from `positionMs`.
   * The server ends idle sessions (reaped after a long pause) and drops them on a
   * restart; when the HLS layer sees the stream's playlist + segments all 404,
   * this rebuilds the session at the current position — the same negotiate path as
   * an escalating switch, but it changes neither the burn nor the audio pick (both
   * carry forward). The component captures the position + play state so the fresh
   * stream resumes where the viewer was (CONTEXT.md: a client whose session
   * vanished simply re-negotiates). */
  recover: (positionMs: number) => void;
}

/** Read a finite, positive `suggestedMaxBitrate` (bits/sec) off a SERVER_BUSY
 * error's details, or undefined when the server omitted/zeroed it. */
function suggestedBitrate(err: ApiError): number | undefined {
  const v = err.details?.suggestedMaxBitrate;
  return typeof v === "number" && Number.isFinite(v) && v > 0 ? v : undefined;
}

/** Copy for the SERVER_BUSY state. While the automatic retry is in flight we
 * say so; once it's exhausted we invite a manual retry. */
function busyMessage(retrying: boolean): string {
  return retrying
    ? "The server is busy transcoding right now — retrying at a lower quality…"
    : "The server is busy transcoding right now. Try again at a lower quality.";
}

/** Friendly, honest copy for a TRANSCODE_REQUIRED outcome. The server's
 * `details.reason` is a machine enum (container/videoCodec/audioCodec/…); we map
 * the common ones to a sentence and fall back to the server message otherwise.
 * With remux/transcode live, the server only returns this for a Title it genuinely
 * cannot play at all (e.g. no decodable video stream) — a true dead-end, not a
 * "the server doesn't transcode" message. */
function unsupportedMessage(err: ApiError): { reason: string; message: string } {
  const reason = typeof err.details?.reason === "string" ? err.details.reason : "unknown";
  const detail = typeof err.details?.detail === "string" ? err.details.detail : "";
  const base =
    "This title can't be played in the browser — the server couldn't produce a playable stream for it.";
  const byReason: Record<string, string> = {
    container: "The browser can't open this file's container.",
    videoCodec: "The browser can't decode this video codec.",
    audioCodec: "The browser can't decode this audio codec.",
    resolution: "This file's resolution is above the allowed ceiling.",
    bitrate: "This file's bitrate is above the allowed ceiling.",
  };
  const specific = byReason[reason];
  return {
    reason,
    message: specific ? `${specific} ${base}` : detail ? `${detail}. ${base}` : base,
  };
}

/** The pre-play Playback preference the session negotiates with (appletv-web-parity
 * §1/§3): the resolved `editionId` (undefined = Auto) and the resolved Quality-cap
 * `constraints` override (undefined = Direct Play), plus a `pending` flag while that
 * resolution is still in flight. Negotiation is DEFERRED until `pending` clears — but
 * ONLY the configured path waits: with no stored preference the caller passes
 * `pending: false` (or omits this) and playback starts immediately, exactly as before. */
export interface PlayerPreference {
  editionId?: string;
  /** The Quality-cap override merged OVER the capability-derived constraints
   * (`maxResolution` + `maxBitrate` from the ladder). Undefined = Direct Play: keep
   * the viewport-derived resolution + the generous Direct-Play bitrate default. */
  constraints?: Pick<PlaybackConstraints, "maxResolution" | "maxBitrate">;
  /** The pre-play Audio Stream pick (appletv-web-parity §1, issue 04) that SEEDS the
   * initial negotiation's `audioStreamId`. Unlike `editionId` / `constraints` this is
   * NOT a persisted preference — it's the server's Remembered audio (server ADR-0023),
   * handed in once from the transient Queue entry (client ADR-0011). It seeds the
   * audio ref ONCE (at mount); an in-player switch then owns the value and this prop
   * never re-seeds over it (no stale-value resurrection). Undefined = Auto. */
  audioStreamId?: string;
  /** The pre-play Video Stream pick (issue 04), seeding the initial `videoStreamId`
   * exactly like `audioStreamId` — seed-once, never persisted, superseded by an
   * in-player Video switch. Undefined = Auto (→ server Remembered video, ADR-0025). */
  videoStreamId?: string;
  /** The pre-play IMAGE Subtitle burn-in (appletv-web-parity §1, issue 05, ADR-0020),
   * seeding the initial `burnSubtitleId` so a committed image-subtitle choice on a
   * transcode/remux tier is burned from frame one. UNLIKE audio/video this axis IS a
   * persisted preference (subtitle choice has no server memory), resolved by the
   * playbackResolver from the stored language(+forced): it emits an id ONLY for an
   * image track on a transcoding tier — a text track and a direct-play image track
   * render locally and leave this undefined. Seeded ONCE (like audio/video), so an
   * in-player captions switch then owns the burn. Undefined = no pre-play burn. */
  burnSubtitleId?: string;
  pending?: boolean;
}

export function usePlayerSession(
  client: ApiClient,
  titleId: string,
  startPosition: number,
  preference?: PlayerPreference,
): PlayerSession {
  const [status, setStatus] = useState<PlayerStatus>({ kind: "negotiating" });
  // The resolved Edition the negotiation sends (appletv-web-parity §2), or undefined
  // for Auto (omit → server picks the best direct-play Edition). Held in a ref, kept
  // in sync each render, so negotiate reads it without being a dependency — and so it
  // carries through every re-negotiation (a burn/audio/video switch) unchanged.
  const editionRef = useRef<string | undefined>(preference?.editionId);
  editionRef.current = preference?.editionId;
  // The resolved Quality-cap constraints override (appletv-web-parity §3), or
  // undefined for Direct Play. Held in a ref, kept in sync each render (like the
  // Edition), so negotiate reads it without being a dependency AND it carries through
  // every re-negotiation (a burn/audio/video switch) unchanged.
  const constraintsRef = useRef<PlayerPreference["constraints"]>(preference?.constraints);
  constraintsRef.current = preference?.constraints;
  // While the preference is still resolving (its Edition NAME → this Title's id needs
  // the detail), hold off the FIRST negotiation so it goes out WITH the editionId
  // rather than re-negotiating after (which would re-buffer). Only the configured
  // path is `pending`; the no-preference path is never held.
  const pendingPreference = preference?.pending ?? false;
  // The image sub burned into the video (subtitles/04), or null. State (not a ref)
  // so the captions menu re-renders when a burn is selected/cleared; mirrored into
  // burnRef so negotiate reads it without being a dependency. SEEDED ONCE from the
  // pre-play preference (issue 05 — a committed image sub on a transcode/remux tier)
  // via the useState/useRef initializers, which run only on mount, so a later in-
  // player captions switch owns the burn and this never re-seeds over it.
  const [burnSubtitleId, setBurnSubtitleId] = useState<string | null>(
    preference?.burnSubtitleId ?? null,
  );
  const burnRef = useRef<string | null>(preference?.burnSubtitleId ?? null);
  // The audio Stream the last negotiation requested (audio-streams/04), or null for
  // the server-resolved default. State so the Audio menu re-renders on an escalation;
  // mirrored into audioRef so negotiate reads it without being a dependency. Carried
  // on every re-negotiation (a burn switch) so an audio pick survives it. SEEDED ONCE
  // from the pre-play pick (issue 04, the sheet's Auto-or-explicit choice on the entry)
  // via the useState/useRef initializers — which run only on mount, so a later render
  // (the same `preference` prop) NEVER re-seeds over an in-player switch. That seed-
  // once (contrast the every-render editionRef sync above) is what stops a stale sheet
  // value resurrecting after the viewer changes audio in-player.
  const [audioStreamId, setAudioStreamId] = useState<string | null>(
    preference?.audioStreamId ?? null,
  );
  const audioRef = useRef<string | null>(preference?.audioStreamId ?? null);
  // The video Stream the last negotiation requested (selectable-video/03), or null
  // for the server-resolved capability-then-quality default. State so the Video menu
  // re-renders on a switch; mirrored into videoStreamRef so negotiate reads it
  // without being a dependency. Carried on every re-negotiation (a burn/audio switch)
  // so a video pick survives it. SEEDED ONCE from the pre-play pick (issue 04), exactly
  // like audio — an in-player Video switch then owns it, never re-seeded.
  const [videoStreamId, setVideoStreamId] = useState<string | null>(
    preference?.videoStreamId ?? null,
  );
  const videoStreamRef = useRef<string | null>(preference?.videoStreamId ?? null);
  // The resume offset negotiation requests (and the position a burn re-negotiation
  // resumes near). Initialized to startPosition; a burn switch updates it to the
  // live position so the restarted stream resumes where the viewer was.
  const resumeRef = useRef<number>(startPosition);

  // The live session id (ref, not state, so report/end read it without being a
  // dependency and without a stale closure). Cleared once the session is ended.
  const sessionIdRef = useRef<string | null>(null);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const getPositionRef = useRef<() => number>(() => 0);
  const endedRef = useRef(false);
  // Aborts the in-flight negotiation when a retry supersedes it or we unmount.
  const negotiateCtrlRef = useRef<AbortController | null>(null);
  // True once we've spent our single automatic SERVER_BUSY retry; a further
  // busy response then waits for the user's manual retry (no infinite loop).
  const autoRetriedRef = useRef(false);

  // negotiate runs (or re-runs) the playback negotiation. `overrideMaxBitrate`
  // lowers the request's bitrate cap for a SERVER_BUSY retry. It is wrapped in a
  // useCallback so both the mount effect and the manual retry() share one path.
  const negotiate = useCallback(
    (overrideMaxBitrate?: number) => {
      // Supersede any in-flight negotiation (e.g. a manual retry mid-request).
      negotiateCtrlRef.current?.abort();
      const ctrl = new AbortController();
      negotiateCtrlRef.current = ctrl;
      endedRef.current = false;
      const { deviceProfile, constraints } = deriveCapabilityProfile();
      // Layer the constraints: the capability-derived defaults (viewport resolution +
      // the generous Direct-Play bitrate, ADR-0003) first, then the Quality-cap rung
      // OVER them (a manual override of both axes — appletv-web-parity §3), then a
      // SERVER_BUSY step-down last so a busy retry still lowers the bitrate further.
      let effectiveConstraints: PlaybackConstraints = {
        ...constraints,
        ...(constraintsRef.current ?? {}),
      };
      if (overrideMaxBitrate != null) {
        effectiveConstraints = { ...effectiveConstraints, maxBitrate: overrideMaxBitrate };
      }
      void (async () => {
        try {
          const decision = await client.startPlayback(
            titleId,
            {
              deviceProfile,
              constraints: effectiveConstraints,
              startPosition: resumeRef.current,
              editionId: editionRef.current,
              burnSubtitleId: burnRef.current ?? undefined,
              audioStreamId: audioRef.current ?? undefined,
              videoStreamId: videoStreamRef.current ?? undefined,
            },
            ctrl.signal,
          );
          if (ctrl.signal.aborted) return;
          sessionIdRef.current = decision.sessionId;
          setStatus({ kind: "ready", decision });
        } catch (err) {
          if (ctrl.signal.aborted || isAbort(err)) return;
          if (err instanceof ApiError && err.code === "SERVER_BUSY") {
            const suggested = suggestedBitrate(err);
            // Auto-retry ONCE at the suggested lower bitrate (where one was
            // given and we haven't already spent the automatic attempt).
            if (!autoRetriedRef.current && suggested != null) {
              autoRetriedRef.current = true;
              setStatus({
                kind: "busy",
                suggestedMaxBitrate: suggested,
                retrying: true,
                message: busyMessage(true),
              });
              negotiate(suggested);
              return;
            }
            // No suggestion, or the auto-retry was already used: park in busy and
            // let the user retry manually (carrying the suggestion forward).
            setStatus({
              kind: "busy",
              suggestedMaxBitrate: suggested,
              retrying: false,
              message: busyMessage(false),
            });
            return;
          }
          if (err instanceof ApiError && err.code === "TRANSCODE_REQUIRED") {
            const { reason, message } = unsupportedMessage(err);
            setStatus({ kind: "unsupported", reason, message });
            return;
          }
          setStatus({ kind: "error", message: errorMessage(err) });
        }
      })();
    },
    // startPosition is read once at negotiation; intentionally not a dep.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [client, titleId],
  );

  // Negotiate once per title, once the preference has resolved. While
  // `pendingPreference` is true (a stored preference whose Edition is still being
  // resolved against the detail) we hold off so the first request already carries the
  // editionId — no start-then-re-negotiate re-buffer. The no-preference path is never
  // pending, so it negotiates immediately (unchanged behaviour). Aborts on
  // unmount/title change.
  useEffect(() => {
    if (pendingPreference) return;
    autoRetriedRef.current = false;
    setStatus({ kind: "negotiating" });
    negotiate();
    return () => negotiateCtrlRef.current?.abort();
  }, [negotiate, pendingPreference]);

  // Manual retry from the busy state: re-negotiate at the last suggested lower
  // bitrate (if any) so the request is a genuine step down, not the rejected one.
  const retry = useCallback(() => {
    setStatus((prev) => {
      const suggested = prev.kind === "busy" ? prev.suggestedMaxBitrate : undefined;
      negotiate(suggested);
      return { kind: "negotiating" };
    });
  }, [negotiate]);

  const report = useCallback(
    (positionMs: number, state: PlaybackState) => {
      const sid = sessionIdRef.current;
      if (!sid || endedRef.current) return;
      // Fire-and-forget: a dropped keepalive is non-fatal (the next one or the
      // reaper covers it). Swallow rejections so a transient failure never
      // surfaces as an unhandled rejection mid-playback.
      void Promise.resolve(client.reportProgress(sid, { positionMs, state })).catch(() => {});
    },
    [client],
  );

  const stopReporting = useCallback(() => {
    if (intervalRef.current != null) {
      clearInterval(intervalRef.current);
      intervalRef.current = null;
    }
  }, []);

  const startReporting = useCallback(
    (getPositionMs: () => number) => {
      getPositionRef.current = getPositionMs;
      stopReporting();
      intervalRef.current = setInterval(() => {
        report(getPositionRef.current(), "playing");
      }, PROGRESS_INTERVAL_MS);
    },
    [report, stopReporting],
  );

  const end = useCallback(
    (finalPositionMs?: number) => {
      stopReporting();
      const sid = sessionIdRef.current;
      if (!sid || endedRef.current) return;
      endedRef.current = true;
      sessionIdRef.current = null;
      if (finalPositionMs != null) {
        void Promise.resolve(
          client.reportProgress(sid, { positionMs: finalPositionMs, state: "paused" }),
        ).catch(() => {});
      }
      void Promise.resolve(client.endSession(sid)).catch(() => {});
    },
    [client, stopReporting],
  );

  // Select/clear a burned-in IMAGE subtitle (subtitles/04). It is a FRESH
  // negotiation: end the current session cleanly (final report + DELETE), record
  // the new burn + resume position, and re-negotiate. Selecting an image sub
  // escalates to a burned-in transcode (which may return SERVER_BUSY → the busy
  // state's retry UX); clearing it (null) restarts on the cheapest tier. A no-op
  // when the requested id already matches the active burn.
  const selectBurnSubtitle = useCallback(
    (id: string | null, positionMs: number) => {
      if (burnRef.current === id) return;
      end(positionMs);
      burnRef.current = id;
      setBurnSubtitleId(id);
      resumeRef.current = Math.max(0, Math.floor(positionMs));
      // A burn switch is a fresh negotiation, so it gets its own automatic
      // SERVER_BUSY retry (the previous session's attempt is spent/irrelevant).
      autoRetriedRef.current = false;
      setStatus({ kind: "negotiating" });
      negotiate();
    },
    [end, negotiate],
  );

  // Select a non-default audio Stream on a DIRECT-PLAY session (audio-streams/04) —
  // the one escalating switch, mirroring selectBurnSubtitle. Direct play carries only
  // the default audio, so a non-default pick is a FRESH negotiation: end the current
  // session cleanly, record the audio + resume position, and re-negotiate. It
  // escalates to remux (or a governed transcode → SERVER_BUSY → the busy retry UX);
  // after it lands the session is demuxed HLS and further switches are in-band. A
  // no-op when the requested id already matches the last negotiated audio.
  const selectAudioStream = useCallback(
    (id: string, positionMs: number) => {
      if (audioRef.current === id) return;
      end(positionMs);
      audioRef.current = id;
      setAudioStreamId(id);
      resumeRef.current = Math.max(0, Math.floor(positionMs));
      // A fresh negotiation gets its own automatic SERVER_BUSY retry (the previous
      // session's attempt is spent/irrelevant).
      autoRetriedRef.current = false;
      setStatus({ kind: "negotiating" });
      negotiate();
    },
    [end, negotiate],
  );

  // Record an in-band audio pick made client-side on the HLS tiers (audio-streams/04).
  // In-band switching never re-negotiates, so this remembers the choice locally so a
  // LATER re-negotiation (a burn switch) carries it forward — AND reports it through
  // the progress surface so the server stores it as Remembered audio (audio-streams/05,
  // ADR-0023): the pick becomes the Title's default (and, for an Episode, the Show's)
  // on the next play. Best-effort — a dropped write just isn't remembered, and it never
  // affects the resume/watched threshold (that stays position-only).
  const noteAudioStream = useCallback(
    (id: string) => {
      audioRef.current = id;
      setAudioStreamId(id);
      const sid = sessionIdRef.current;
      if (!sid || endedRef.current) return;
      const positionMs = Math.max(0, Math.floor(getPositionRef.current()));
      void Promise.resolve(
        client.reportProgress(sid, { positionMs, state: "playing", audioStreamId: id }),
      ).catch(() => {});
    },
    [client],
  );

  // Select a video Stream (selectable-video/03, ADR-0025). Unlike audio there is NO
  // in-band video rendition, so EVERY pick is an escalating switch (the image-subtitle
  // model): end the current session cleanly, record the video + resume position, and
  // re-negotiate. The current audioStreamId + burnSubtitleId carry forward through
  // negotiate() unchanged, so the audio and subtitle choices survive the switch. A
  // non-default pick escalates to HLS remux (or a governed transcode → SERVER_BUSY →
  // the busy retry UX). A no-op when the requested id already matches the last
  // negotiated video.
  const selectVideoStream = useCallback(
    (id: string, positionMs: number) => {
      if (videoStreamRef.current === id) return;
      end(positionMs);
      videoStreamRef.current = id;
      setVideoStreamId(id);
      resumeRef.current = Math.max(0, Math.floor(positionMs));
      // A fresh negotiation gets its own automatic SERVER_BUSY retry (the previous
      // session's attempt is spent/irrelevant).
      autoRetriedRef.current = false;
      setStatus({ kind: "negotiating" });
      negotiate();
    },
    [end, negotiate],
  );

  // Recover a vanished session (reaped after a long pause, or dropped on a server
  // restart): re-negotiate a FRESH session from the current position and resume.
  // Same shape as an escalating switch (end the old session, update the resume
  // offset, re-negotiate) but it changes neither the burn nor the audio pick —
  // both carry through negotiate() unchanged. end() best-effort DELETEs the
  // already-gone session (a 404 it swallows); negotiate() re-arms endedRef and
  // mints the new session + streamUrl the component re-attaches to.
  const recover = useCallback(
    (positionMs: number) => {
      end(positionMs);
      resumeRef.current = Math.max(0, Math.floor(positionMs));
      // A fresh negotiation gets its own automatic SERVER_BUSY retry.
      autoRetriedRef.current = false;
      setStatus({ kind: "negotiating" });
      negotiate();
    },
    [end, negotiate],
  );

  // Best-effort final report on a hard page unload (close/refresh): the
  // component-driven report()/end() cover in-app navigation, but a tab close
  // skips React unmount, so we also report here. Uses the last known position
  // getter. We do NOT DELETE here (an async DELETE rarely survives unload); the
  // server's reaper collects an abandoned session.
  useEffect(() => {
    const onUnload = () => {
      const sid = sessionIdRef.current;
      if (!sid || endedRef.current) return;
      report(getPositionRef.current(), "paused");
    };
    window.addEventListener("beforeunload", onUnload);
    return () => window.removeEventListener("beforeunload", onUnload);
  }, [report]);

  // On unmount (route change away from the player), clear the interval. The
  // The Now Playing bar calls end() in its own unmount effect with the final position;
  // this is the safety net for the interval timer.
  useEffect(() => {
    return () => stopReporting();
  }, [stopReporting]);

  return {
    status,
    burnSubtitleId,
    audioStreamId,
    videoStreamId,
    report,
    startReporting,
    stopReporting,
    end,
    retry,
    selectBurnSubtitle,
    selectAudioStream,
    noteAudioStream,
    selectVideoStream,
    recover,
  };
}

function isAbort(err: unknown): boolean {
  return err instanceof DOMException && err.name === "AbortError";
}
