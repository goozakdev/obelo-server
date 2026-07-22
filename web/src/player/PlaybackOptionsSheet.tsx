import { useEffect, useRef, useState } from "react";
import type {
  AudioStream,
  MediaFile,
  SubtitleTrack,
  TitleDetail,
  VideoStream,
} from "../api/types";
import {
  AUTO_PREFERENCE,
  loadPreferenceForTitle,
  preferenceScopeForTitle,
  savePreference,
  type PlaybackPreference,
  type SubtitlePreference,
} from "./playbackPreference";
import {
  availableRungs,
  sourceHeightForSelection,
  type QualityCapId,
} from "./qualityLadder";
import { orderedAudioStreams, preferredAudioLang } from "./audio";
import { orderedVideoStreams } from "./video";

/** The pre-play Audio + Video Stream picks the sheet's Play carries (issue 04).
 * Distinct from the persisted {@link PlaybackPreference}: these are the server's
 * per-user Remembered audio / Remembered video (server ADR-0023/0025), NOT a local
 * axis (client ADR-0011) — so they NEVER touch the preference store. `null` = Auto
 * (omit the id → the server applies its memory). They flow straight to Play as this
 * play's `audioStreamId` / `videoStreamId`; the server records them as Remembered
 * via the progress write-back, and an in-player switch later supersedes them without
 * any stale sheet value resurrecting. */
export interface StreamSelection {
  audioStreamId: string | null;
  videoStreamId: string | null;
}

/** The File the Audio / Video sections are built from: the first present (non-
 * missing) File of the drafted Edition (whose stream ids are what a play of that
 * Edition negotiates), falling back to the first present File across all Editions
 * when the draft is Auto (or the drafted Edition has no present File). undefined for
 * a Title with no present File at all. */
function selectableFile(
  editions: TitleDetail["editions"],
  editionName: string | null,
): MediaFile | undefined {
  const scoped = editionName ? editions.filter((e) => e.name === editionName) : [];
  const pool = scoped.length > 0 ? scoped : editions;
  return (
    pool.flatMap((e) => e.files).find((f) => !f.missing) ??
    editions.flatMap((e) => e.files).find((f) => !f.missing)
  );
}

// The Playback Options sheet (appletv-web-parity §1/§2) — the pre-play
// configuration surface, opened from Movie / Episode detail. It is built PURELY
// from the `GET /titles/{id}` payload ALREADY IN HAND (`title`): no negotiation, no
// network call, no provider-quota spend on open (the TV's "Model B").
//
// COMMIT MODEL: edits are a local DRAFT. **Play** commits the draft as the saved
// Playback preference (savePreference) AND starts playback (onPlay → the detail's
// existing play(), which Continues at the resume position when in progress, else
// from the start). Backing out (Cancel / Esc / backdrop) DISCARDS the draft — the
// saved preference is untouched.
//
// This slice carries four axes — the Edition, the Quality cap, and (issue 04) the
// Audio + Video Stream. Edition rows: **Auto** (omit `editionId`, let the server pick
// the best direct-play Edition) + one row per `title.editions`, persisted BY EDITION
// NAME (playbackPreference) so it ports across a Show's Episodes; the resolver maps the
// name back to an id per-Episode. Quality rows: **Direct Play** (uncapped — the
// viewport-derived resolution) + the downscale rungs STRICTLY BELOW the selected
// Edition's source height (re-derived when the Edition changes), each sending a paired
// `maxResolution` + `maxBitrate`.
//
// AUDIO + VIDEO are DELIBERATELY NOT persisted (client ADR-0011). Unlike Edition /
// Quality they are the server's per-user Remembered audio / Remembered video (server
// ADR-0023/0025), so the sheet holds them as DRAFT-ONLY {@link StreamSelection} state
// (never written to the preference store — duplicating them would drift the moment the
// viewer switches audio in-player) that flows STRAIGHT to Play as this play's
// `audioStreamId` / `videoStreamId`. **Auto** omits the id (→ server memory); an
// explicit pick sends it over the wire (web has no local mpv `aid` pre-seed). The
// Video section renders ONLY when the File carries >1 selectable Video Stream.
//
// SUBTITLE (appletv-web-parity §1, ADR-0020) IS a persisted axis, so — unlike audio /
// video — it rides the pref DRAFT like Edition / Quality and commits on Play. It is
// SELECTION ONLY: **Off** + one source-tagged row per `subtitles[]` track, with NO
// search / fetch (that stays in the in-player captions menu, so the sheet takes no
// network on open or selection). The choice persists BY LANGUAGE (+ forced), not id
// (playbackPreference), so a Show's choice ports across Episodes; the resolver decides
// delivery from the resulting tier (an image track burns in only on transcode / remux,
// via burnSubtitleId — a text track and a direct-play image track render locally).
//
// AUDIO DELIVERY — the "Transcode to AAC (Stereo)" toggle (appletv-web-parity §7,
// issue 06) — is a persisted axis on the pref draft like Edition / Quality / Subtitle.
// There is NO contract field for it: on Play the resolver maps the flag to a
// capability-profile narrowing (`audioCodecs: ["aac"], maxAudioChannels: 2`) merged
// over the sent `deviceProfile`. The draft flag (`draft.aacStereo`) is the state issue
// 07's rule reads to disable Force Remux while AAC is on (AAC moves the session off
// pure direct play). Force-Remux is a later section bolted onto this same skeleton.

/** A native <dialog> sheet, opened with showModal() (Esc-to-close, top layer, focus
 * containment, ::backdrop for free — mirroring EditItemDialog). Controlled by the
 * detail screen: `open` drives showModal/close, `onClose` fires on any dismissal. */
export default function PlaybackOptionsSheet({
  title,
  userId,
  open,
  onClose,
  onPlay,
}: {
  /** The already-loaded Title detail — the sole source the sheet is built from. */
  title: TitleDetail;
  /** The Active user (localStorage keying); null for an anon bucket. */
  userId: string | null;
  open: boolean;
  onClose: () => void;
  /** Start playback of the committed configuration — the detail's existing play(),
   * handed this play's Audio / Video Stream picks (Continue at the resume position
   * when in progress, else from the start). */
  onPlay: (streams: StreamSelection) => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  // The DRAFT Edition name + Quality cap (null = Auto), seeded from the SAVED
  // preference each time the sheet opens so it always reflects the committed choice,
  // and discarded on a back-out (we only persist on Play).
  const [draft, setDraft] = useState<PlaybackPreference>(AUTO_PREFERENCE);
  // The DRAFT Audio / Video Stream picks (null = Auto). NOT part of `draft` because
  // they are NEVER persisted (client ADR-0011) — the server owns their memory. The
  // sheet always OPENS at Auto: the client can't read the server's Remembered pick, so
  // Auto ("use server memory") is the honest initial state, re-seeded to null on each
  // open. They flow to Play, never to the store (see commitAndPlay).
  const [audioStreamId, setAudioStreamId] = useState<string | null>(null);
  const [videoStreamId, setVideoStreamId] = useState<string | null>(null);

  // Drive the native dialog imperatively; keep React's `open` in sync via its own
  // close event (Esc / backdrop). Re-seed the draft from the store on each open, and
  // reset the (unpersisted) Audio / Video picks to Auto.
  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (open && !dialog.open) {
      setDraft(loadPreferenceForTitle(window.localStorage, userId, title));
      setAudioStreamId(null);
      setVideoStreamId(null);
      dialog.showModal();
    }
    if (!open && dialog.open) dialog.close();
  }, [open, userId, title]);

  const scope = preferenceScopeForTitle(title);
  const resuming = !title.watched && title.resumePositionMs > 0;

  // The File the Audio / Video sections read their Streams from, re-derived from the
  // drafted Edition (its stream ids are per-File, so switching Edition changes the set
  // — hence the Audio / Video picks reset when the Edition draft changes, below).
  const streamFile = selectableFile(title.editions, draft.editionName);
  // Only send a pick that still belongs to the current File (an Edition switch left a
  // stale id from a different File's Streams) — otherwise degrade to Auto, mirroring
  // the Quality axis's degrade-to-Direct-Play. Auto is always the safe omission.
  const audioIds = new Set((streamFile?.audioStreams ?? []).map((s) => s.id));
  const videoIds = new Set((streamFile?.videoStreams ?? []).map((s) => s.id));
  const activeAudioId = audioStreamId && audioIds.has(audioStreamId) ? audioStreamId : null;
  const activeVideoId = videoStreamId && videoIds.has(videoStreamId) ? videoStreamId : null;

  // Changing the Edition draft re-points the negotiated File, whose Stream ids differ,
  // so the Audio / Video picks reset to Auto (a stale id would be sent to the wrong
  // File). Quality re-derives its rungs but stays a draft change like before.
  function selectEdition(editionName: string | null) {
    setDraft((d) => ({ ...d, editionName }));
    setAudioStreamId(null);
    setVideoStreamId(null);
  }

  // Commit the draft as the saved preference, then start playback. Persisting only
  // happens here (never on a row click), and persists ONLY the Edition + Quality axes
  // — the Audio / Video picks are handed to Play instead (client ADR-0011), so they
  // never enter the store and can't drift against an in-player switch. A pick that no
  // longer matches the negotiated File degrades to Auto (activeAudioId/activeVideoId).
  function commitAndPlay() {
    if (scope) savePreference(window.localStorage, userId, scope, draft);
    onClose();
    onPlay({ audioStreamId: activeAudioId, videoStreamId: activeVideoId });
  }

  return (
    <dialog
      ref={dialogRef}
      className="edit-item-dialog playback-options-sheet"
      data-testid="playback-options-sheet"
      // Native close (Esc / dialog.close()) → keep the parent's `open` in sync.
      onClose={onClose}
      // Clicking the ::backdrop (the dialog element itself, outside the panel)
      // dismisses without committing.
      onClick={(e) => {
        if (e.target === dialogRef.current) onClose();
      }}
    >
      <div className="edit-item-panel playback-options-panel">
        <div className="edit-item-header">
          <h2 className="edit-item-title">Playback options</h2>
          <button
            className="nav-link edit-item-close"
            type="button"
            data-testid="playback-options-close"
            aria-label="Close"
            onClick={onClose}
          >
            ✕
          </button>
        </div>

        <div className="playback-options-body">
          <EditionSection
            editions={title.editions}
            selected={draft.editionName}
            onSelect={selectEdition}
          />
          <QualitySection
            editions={title.editions}
            editionName={draft.editionName}
            selected={draft.qualityCap}
            onSelect={(qualityCap) => setDraft((d) => ({ ...d, qualityCap }))}
          />
          <AudioSection
            streams={streamFile?.audioStreams ?? []}
            selected={activeAudioId}
            onSelect={setAudioStreamId}
          />
          <AudioDeliverySection
            enabled={draft.aacStereo}
            onToggle={(aacStereo) => setDraft((d) => ({ ...d, aacStereo }))}
          />
          <VideoSection
            streams={streamFile?.videoStreams ?? []}
            selected={activeVideoId}
            onSelect={setVideoStreamId}
          />
          <SubtitlesSection
            subtitles={title.subtitles ?? []}
            selected={draft.subtitle}
            onSelect={(subtitle) => setDraft((d) => ({ ...d, subtitle }))}
          />
        </div>

        <div className="playback-options-footer">
          <button
            className="auth-submit play-button"
            type="button"
            data-testid="playback-options-play"
            onClick={commitAndPlay}
          >
            {resuming ? "Continue" : "Play"}
          </button>
          <button
            className="nav-link"
            type="button"
            data-testid="playback-options-cancel"
            onClick={onClose}
          >
            Cancel
          </button>
        </div>
      </div>
    </dialog>
  );
}

// The Edition section: an **Auto** row (omit editionId) + one row per Edition. The
// active row is marked (radio semantics — aria-checked + data-active). Selecting a
// row only updates the DRAFT; nothing persists until Play. Persisted by NAME, so the
// row's stored value is `edition.name` (Auto → null).
function EditionSection({
  editions,
  selected,
  onSelect,
}: {
  editions: TitleDetail["editions"];
  /** The draft Edition name, or null for Auto. */
  selected: string | null;
  onSelect: (editionName: string | null) => void;
}) {
  return (
    <section className="playback-options-section" data-testid="edition-section">
      <h3 className="section-title playback-options-section-title">Edition</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Edition">
        <OptionRow
          label="Auto"
          hint="Best available — the server picks the direct-play edition."
          active={selected === null}
          testId="edition-option-auto"
          onSelect={() => onSelect(null)}
        />
        {editions.map((ed) => (
          <OptionRow
            key={ed.id}
            label={ed.name || "Default"}
            active={selected === ed.name}
            testId="edition-option"
            dataName={ed.name}
            onSelect={() => onSelect(ed.name)}
          />
        ))}
      </ul>
    </section>
  );
}

// The Quality-cap section (appletv-web-parity §3): a **Direct Play** row (uncapped —
// the viewport-derived resolution, no manual bitrate cap) + one row per downscale rung
// STRICTLY BELOW the selected Edition's source height (availableRungs — never a rung ≥
// source, since the scale filter never upscales). Changing the Edition re-derives the
// source height, hence the offered rungs. Selecting a row only updates the DRAFT
// (persisted BY RUNG ID); nothing persists until Play. Each rung sends both a
// `maxResolution` and a paired `maxBitrate` — the resolver builds those from the id.
function QualitySection({
  editions,
  editionName,
  selected,
  onSelect,
}: {
  editions: TitleDetail["editions"];
  /** The draft Edition name (null = Auto) — governs which rungs are below source. */
  editionName: string | null;
  /** The draft Quality-cap rung id, or null for Direct Play. */
  selected: QualityCapId | null;
  onSelect: (qualityCap: QualityCapId | null) => void;
}) {
  const rungs = availableRungs(sourceHeightForSelection(editions, editionName));
  // A rung no longer below source (the Edition shrank) is no longer offered → treat the
  // draft as Direct Play so the active mark stays honest (the resolver degrades it too).
  const activeRung = rungs.some((r) => r.id === selected) ? selected : null;
  return (
    <section className="playback-options-section" data-testid="quality-section">
      <h3 className="section-title playback-options-section-title">Quality cap</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Quality cap">
        <OptionRow
          label="Direct Play"
          hint="Uncapped — sized to your screen."
          active={activeRung === null}
          testId="quality-option-direct"
          onSelect={() => onSelect(null)}
        />
        {rungs.map((rung) => (
          <OptionRow
            key={rung.id}
            label={rung.label}
            hint={`Up to ${Math.round(rung.maxBitrate / 1_000_000)} Mbps`}
            active={activeRung === rung.id}
            testId="quality-option"
            dataName={rung.id}
            onSelect={() => onSelect(rung.id)}
          />
        ))}
      </ul>
    </section>
  );
}

// The Audio section (appletv-web-parity §1, issue 04): an **Auto** row (omit
// `audioStreamId` → the server applies its Remembered audio) + one row per audio
// Stream of the negotiated File, ordered like the in-player Audio menu (preferred
// language, then default disposition, then label). A pick is DRAFT-ONLY and is handed
// to Play as `audioStreamId` — it is NEVER written to the preference store (client
// ADR-0011): the server records it as Remembered via the progress write-back. Renders
// nothing for a silent File (no audio Streams) — there's no meaningful choice. Per
// CONTEXT.md these are audio Streams, never a coined "Audio track".
function AudioSection({
  streams,
  selected,
  onSelect,
}: {
  streams: AudioStream[];
  /** The draft audio Stream id, or null for Auto. */
  selected: string | null;
  onSelect: (audioStreamId: string | null) => void;
}) {
  if (streams.length === 0) return null;
  const ordered = orderedAudioStreams(streams, preferredAudioLang());
  return (
    <section className="playback-options-section" data-testid="audio-section">
      <h3 className="section-title playback-options-section-title">Audio</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Audio">
        <OptionRow
          label="Auto"
          hint="Use your remembered audio."
          active={selected === null}
          testId="audio-option-auto"
          onSelect={() => onSelect(null)}
        />
        {ordered.map((s) => (
          <OptionRow
            key={s.id}
            label={s.label}
            active={selected === s.id}
            testId="audio-option"
            dataName={s.id}
            onSelect={() => onSelect(s.id)}
          />
        ))}
      </ul>
    </section>
  );
}

// The Audio-delivery section (appletv-web-parity §7, issue 06): the single
// "Transcode to AAC (Stereo)" toggle. NOT a request field — a capability-profile
// narrowing (the resolver maps the flag to `audioCodecs: ["aac"], maxAudioChannels:
// 2` over the sent `deviceProfile`). A PERSISTED axis riding the pref draft like
// Edition / Quality / Subtitle (this axis has no server memory), so a toggle here is
// draft-only until Play commits it. Checkbox semantics (on/off), not a radio row —
// there is no "Auto" third state. Issue 07 reads the same draft flag to disable
// Force Remux while this is on (AAC moves the session off pure direct play).
function AudioDeliverySection({
  enabled,
  onToggle,
}: {
  /** The draft AAC-stereo flag (persisted on Play). */
  enabled: boolean;
  onToggle: (aacStereo: boolean) => void;
}) {
  return (
    <section className="playback-options-section" data-testid="audio-delivery-section">
      <h3 className="section-title playback-options-section-title">Audio delivery</h3>
      <ul className="playback-options-list">
        <li className="playback-options-item">
          <button
            type="button"
            role="checkbox"
            aria-checked={enabled}
            className={`playback-options-row${enabled ? " is-active" : ""}`}
            data-testid="aac-stereo-toggle"
            data-active={enabled ? "1" : undefined}
            onClick={() => onToggle(!enabled)}
          >
            <span className="playback-options-mark" aria-hidden="true">
              {enabled ? "☑" : "☐"}
            </span>
            <span className="playback-options-row-text">
              <span className="playback-options-row-label">Transcode to AAC (Stereo)</span>
              <span className="playback-options-row-hint">
                Deliver stereo AAC audio — for devices that can't decode surround sound.
              </span>
            </span>
          </button>
        </li>
      </ul>
    </section>
  );
}

// The Video section (appletv-web-parity §1, issue 04): an **Auto** row (omit
// `videoStreamId` → the server applies its Remembered video) + one row per selectable
// Video Stream (e.g. Black & White / Colour cuts). Rendered ONLY when the File carries
// >1 selectable Video Stream — a lone Video Stream is not a choice (mirrors the detail
// screen's ≥2 chip gate and the in-player Video menu). A pick is DRAFT-ONLY and handed
// to Play as `videoStreamId`, never persisted (client ADR-0011). Per CONTEXT.md these
// are Video Streams, never a coined "Video track".
function VideoSection({
  streams,
  selected,
  onSelect,
}: {
  streams: VideoStream[];
  /** The draft video Stream id, or null for Auto. */
  selected: string | null;
  onSelect: (videoStreamId: string | null) => void;
}) {
  // Gate at >1 — the whole point of the section is choosing between alternates.
  if (streams.length < 2) return null;
  const ordered = orderedVideoStreams(streams);
  return (
    <section className="playback-options-section" data-testid="video-section">
      <h3 className="section-title playback-options-section-title">Video</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Video">
        <OptionRow
          label="Auto"
          hint="Use your remembered video."
          active={selected === null}
          testId="video-option-auto"
          onSelect={() => onSelect(null)}
        />
        {ordered.map((s) => (
          <OptionRow
            key={s.id}
            label={s.label}
            active={selected === s.id}
            testId="video-option"
            dataName={s.id}
            onSelect={() => onSelect(s.id)}
          />
        ))}
      </ul>
    </section>
  );
}

// The Subtitles section (appletv-web-parity §1, ADR-0020): an **Off** row (no
// subtitle) + one row per `subtitles[]` Subtitle track, each SOURCE-TAGGED (Embedded /
// Sidecar / Fetched — the source decides delivery, so it must be visible). SELECTION
// ONLY: no search / fetch happens here (that lives in the in-player captions menu), so
// the section builds purely from the in-hand `subtitles` with no network. Unlike
// Audio / Video this IS persisted (client ADR-0011: subtitle choice has no server
// memory) — stored BY LANGUAGE (+ forced), never the track id, so a Show's choice
// ports across Episodes whose tracks carry different ids. Renders nothing when the
// Title carries no Subtitle track (Off alone is no choice). Per CONTEXT.md these are
// "Subtitle track"s, always source-tagged — never a bare "Track".
function SubtitlesSection({
  subtitles,
  selected,
  onSelect,
}: {
  subtitles: SubtitleTrack[];
  /** The draft Subtitle choice (language + forced), or null for Off. */
  selected: SubtitlePreference | null;
  onSelect: (subtitle: SubtitlePreference | null) => void;
}) {
  if (subtitles.length === 0) return null;
  return (
    <section className="playback-options-section" data-testid="subtitles-section">
      <h3 className="section-title playback-options-section-title">Subtitles</h3>
      <ul className="playback-options-list" role="radiogroup" aria-label="Subtitle track">
        <OptionRow
          label="Off"
          hint="No subtitles."
          active={selected === null}
          testId="subtitle-option-off"
          onSelect={() => onSelect(null)}
        />
        {subtitles.map((sub) => (
          <OptionRow
            key={sub.id}
            label={sub.label}
            hint={subtitleSourceLabel(sub.source)}
            // Stored by language (+ forced), so the ACTIVE mark is by that key, not id
            // — a persisted choice highlights every same-language track, exactly what
            // ports across Episodes.
            active={
              selected !== null &&
              (sub.language ?? "").toLowerCase() === selected.language.toLowerCase() &&
              sub.forced === selected.forced
            }
            testId="subtitle-option"
            dataName={sub.source}
            onSelect={() =>
              onSelect({ language: sub.language ?? "", forced: sub.forced })
            }
          />
        ))}
      </ul>
    </section>
  );
}

/** The human source tag for a Subtitle track (ADR-0020's three sources) — the
 * source decides delivery, so the row must show it. An unknown source falls back to
 * a capitalized form rather than a bare "Track". */
function subtitleSourceLabel(source: string): string {
  switch (source) {
    case "embedded":
      return "Embedded";
    case "sidecar":
      return "Sidecar";
    case "fetched":
      return "Fetched";
    default:
      return source ? source[0].toUpperCase() + source.slice(1) : "";
  }
}

// One selectable option row (a radio in a list). The active row shows a mark and
// carries aria-checked + data-active for tests / styling. `dataName` is the row's
// stored value (an Edition name or a Quality rung id), exposed as data-option-value.
function OptionRow({
  label,
  hint,
  active,
  testId,
  dataName,
  onSelect,
}: {
  label: string;
  hint?: string;
  active: boolean;
  testId: string;
  dataName?: string;
  onSelect: () => void;
}) {
  return (
    <li className="playback-options-item">
      <button
        type="button"
        role="radio"
        aria-checked={active}
        className={`playback-options-row${active ? " is-active" : ""}`}
        data-testid={testId}
        data-option-value={dataName}
        data-active={active ? "1" : undefined}
        onClick={onSelect}
      >
        <span className="playback-options-mark" aria-hidden="true">
          {active ? "●" : "○"}
        </span>
        <span className="playback-options-row-text">
          <span className="playback-options-row-label">{label}</span>
          {hint && <span className="playback-options-row-hint">{hint}</span>}
        </span>
      </button>
    </li>
  );
}
