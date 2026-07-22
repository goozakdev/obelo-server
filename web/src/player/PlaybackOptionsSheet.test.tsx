import type { ReactElement } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import type {
  AudioStream,
  MediaFile,
  ServerInfo,
  SubtitleTrack,
  TitleDetail,
  VideoStream,
} from "../api/types";
import { loadPreference } from "./playbackPreference";
import { ServerInfoStateProvider } from "../serverInfoContext";
import PlaybackOptionsSheet from "./PlaybackOptionsSheet";

// The Playback Options sheet (appletv-web-parity §1/§2). The whole sheet is built
// from the in-hand `title` — assert it opens with ZERO network calls, shows the
// Edition section (Auto + a row per Edition, active row marked), and that Play
// COMMITS the draft to the preference store AND starts playback while backing out
// DISCARDS it.

// Spy the whole api client so we can prove the sheet never touches the network on
// open (it builds purely from the `title` prop it's handed).
const apiCalls = vi.hoisted(() => ({ calls: 0 }));
vi.mock("../api/client", () => {
  const trap = new Proxy(
    {},
    {
      get: () => (..._a: unknown[]) => {
        apiCalls.calls += 1;
        return Promise.resolve({});
      },
    },
  );
  return { apiClient: trap };
});

function movieDetail(): TitleDetail {
  return {
    id: "t1",
    libraryId: "lib1",
    kind: "movie",
    title: "Blade Runner",
    year: 1982,
    needsReview: false,
    ambiguous: false,
    hidden: false,
    resumePositionMs: 0,
    watched: false,
    editions: [
      { id: "ed-tc", name: "Theatrical Cut", files: [] },
      { id: "ed-fc", name: "Final Cut", files: [] },
    ],
    artwork: [],
    subtitles: [],
    overview: "",
    tagline: "",
    contentRating: "",
    releaseDate: "",
    runtimeMinutes: 0,
    studio: "",
    genres: [],
    cast: [],
    enrichmentStatus: "",
    lockedFields: [],
    displayTitle: "",
  };
}

/** A File carrying a source height, so the Quality section can read the resolution. */
function file(height: number): MediaFile {
  return {
    id: `f-${height}`,
    path: "",
    container: "mp4",
    width: Math.round((height * 16) / 9),
    height,
    bitrate: 0,
    durationMs: 0,
    sizeBytes: 0,
    missing: false,
    streams: [],
    audioStreams: [],
    videoStreams: [],
  };
}

/** A Movie whose single Edition is a 4K (2160-line) source — its downscale rungs are
 * 1080p / 720p / SD, and the 4K rung must NOT be offered (a rung ≥ source). */
function uhdDetail(): TitleDetail {
  return { ...movieDetail(), editions: [{ id: "ed-uhd", name: "4K", files: [file(2160)] }] };
}

beforeEach(() => {
  window.localStorage.clear();
  apiCalls.calls = 0;
});

describe("PlaybackOptionsSheet — Edition axis", () => {
  it("builds from the in-hand payload with no network call, showing Auto + each Edition", () => {
    render(
      <PlaybackOptionsSheet
        title={movieDetail()}
        userId="u1"
        open
        onClose={() => {}}
        onPlay={() => {}}
      />,
    );
    expect(apiCalls.calls).toBe(0);
    expect(screen.getByTestId("edition-option-auto")).toBeInTheDocument();
    const rows = screen.getAllByTestId("edition-option");
    expect(rows.map((r) => r.textContent)).toEqual(
      expect.arrayContaining([expect.stringContaining("Theatrical Cut"), expect.stringContaining("Final Cut")]),
    );
    // Nothing stored → Auto is the active row.
    expect(screen.getByTestId("edition-option-auto")).toHaveAttribute("aria-checked", "true");
  });

  it("Play commits the Edition draft as the saved preference AND starts playback", () => {
    const onPlay = vi.fn();
    const onClose = vi.fn();
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={onClose} onPlay={onPlay} />,
    );
    // Pick "Final Cut" (a draft change only — nothing persisted yet).
    fireEvent.click(screen.getByRole("radio", { name: /Final Cut/ }));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
    // Play commits + starts.
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBe("Final Cut");
    expect(onPlay).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("selecting Auto omits the Edition (stored as null)", () => {
    // Seed a prior pick, then commit Auto over it.
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: "Final Cut" }),
    );
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    // Opens reflecting the saved pick.
    expect(screen.getByRole("radio", { name: /Final Cut/ })).toHaveAttribute("aria-checked", "true");
    fireEvent.click(screen.getByTestId("edition-option-auto"));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
  });

  it("backing out discards the draft (preference untouched, no playback)", () => {
    const onPlay = vi.fn();
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /Final Cut/ }));
    fireEvent.click(screen.getByTestId("playback-options-cancel"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).editionName).toBeNull();
    expect(onPlay).not.toHaveBeenCalled();
  });
});

describe("PlaybackOptionsSheet — Quality cap axis", () => {
  it("lists Direct Play + only the rungs strictly below the 4K source (no 4K rung)", () => {
    render(
      <PlaybackOptionsSheet title={uhdDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.getByTestId("quality-option-direct")).toBeInTheDocument();
    const rungs = screen.getAllByTestId("quality-option").map((r) => r.getAttribute("data-option-value"));
    // 4K source → 1080p / 720p / sd below it; the 4K rung is NOT offered (≥ source).
    expect(rungs).toEqual(["1080p", "720p", "sd"]);
    // Nothing stored → Direct Play is the active row.
    expect(screen.getByTestId("quality-option-direct")).toHaveAttribute("aria-checked", "true");
  });

  it("offers no rung (Direct Play only) when the Edition carries no source dims", () => {
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.getByTestId("quality-option-direct")).toBeInTheDocument();
    expect(screen.queryAllByTestId("quality-option")).toHaveLength(0);
  });

  it("Play commits the chosen rung as the saved Quality cap", () => {
    const onPlay = vi.fn();
    render(
      <PlaybackOptionsSheet title={uhdDetail()} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /720p/ }));
    // Draft only — nothing persisted until Play.
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).qualityCap).toBeNull();
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).qualityCap).toBe("720p");
    expect(onPlay).toHaveBeenCalledTimes(1);
  });

  it("Direct Play commits a null cap (uncapped)", () => {
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: null, qualityCap: "720p" }),
    );
    render(
      <PlaybackOptionsSheet title={uhdDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    // Opens reflecting the saved rung.
    expect(screen.getByRole("radio", { name: /720p/ })).toHaveAttribute("aria-checked", "true");
    fireEvent.click(screen.getByTestId("quality-option-direct"));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).qualityCap).toBeNull();
  });
});

// ── Audio + Video Stream axes (issue 04) ─────────────────────────────────────────
// The two SERVER-owned axes: Auto omits the id (→ the server's Remembered pick), an
// explicit pick flows to Play (never to the store, client ADR-0011), and the Video
// section shows only when the File carries >1 selectable Video Stream.

function audio(id: string, label: string, extra?: Partial<AudioStream>): AudioStream {
  return { id, index: 0, codec: "eac3", isDefault: false, label, ...extra };
}
function videoStream(id: string, label: string, extra?: Partial<VideoStream>): VideoStream {
  return { id, index: 0, codec: "h264", isDefault: false, label, ...extra };
}

/** A File carrying explicit audio + video Streams for the Audio / Video sections. */
function streamsFile(audioStreams: AudioStream[], videoStreams: VideoStream[]): MediaFile {
  return { ...file(1080), audioStreams, videoStreams };
}

/** A Movie whose single Edition's File carries the given audio / video Streams. */
function streamsDetail(audioStreams: AudioStream[], videoStreams: VideoStream[]): TitleDetail {
  return {
    ...movieDetail(),
    editions: [{ id: "ed1", name: "Default", files: [streamsFile(audioStreams, videoStreams)] }],
  };
}

/** The last StreamSelection Play handed onPlay, for asserting the id reaches Play.
 * Unmounts before returning so a second call in the same test doesn't leave two
 * sheets (two radiogroups) in the DOM. */
function playAndCapture(detail: TitleDetail, pick: () => void) {
  const onPlay = vi.fn();
  const { unmount } = render(
    <PlaybackOptionsSheet title={detail} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
  );
  pick();
  fireEvent.click(screen.getByTestId("playback-options-play"));
  unmount();
  return onPlay;
}

describe("PlaybackOptionsSheet — Audio axis", () => {
  const streams = [
    audio("a-en", "English 5.1", { language: "en", isDefault: true }),
    audio("a-fr", "Français Stéréo", { language: "fr" }),
    audio("a-com", "Director's Commentary", { commentary: true }),
  ];

  it("shows Auto + one row per audio Stream, Auto active by default", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(streams, [])} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.getByTestId("audio-option-auto")).toBeInTheDocument();
    expect(screen.getAllByTestId("audio-option")).toHaveLength(3);
    // Auto (server Remembered audio) is the honest initial state — the client can't
    // read the server's memory, so no explicit row is pre-marked.
    expect(screen.getByTestId("audio-option-auto")).toHaveAttribute("aria-checked", "true");
  });

  it("Auto omits audioStreamId; an explicit pick sends it to Play", () => {
    // Auto → null (omit).
    let onPlay = playAndCapture(streamsDetail(streams, []), () => {});
    expect(onPlay).toHaveBeenCalledWith({ audioStreamId: null, videoStreamId: null });
    // Explicit pick → the chosen Stream id reaches Play.
    onPlay = playAndCapture(streamsDetail(streams, []), () =>
      fireEvent.click(screen.getByRole("radio", { name: /Director's Commentary/ })),
    );
    expect(onPlay).toHaveBeenCalledWith({ audioStreamId: "a-com", videoStreamId: null });
  });

  it("renders nothing for a silent File (no audio Streams)", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail([], [])} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.queryByTestId("audio-section")).not.toBeInTheDocument();
  });

  it("never writes the audio pick to the preference store (ADR-0011)", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(streams, [])} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /Français/ }));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    // The persisted preference carries only Edition + Quality — no audio field exists,
    // and the store's raw JSON must not smuggle one in.
    const raw = window.localStorage.getItem("juicebox.playback-pref.u1.title.t1");
    expect(raw).not.toBeNull();
    expect(raw).not.toContain("a-fr");
    expect(raw).not.toContain("audioStreamId");
  });

  it("copy says \"Audio\", never \"Audio track\"", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(streams, [])} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    const section = screen.getByTestId("audio-section");
    expect(section).toHaveTextContent("Audio");
    expect(section.textContent).not.toMatch(/Audio track/i);
  });
});

describe("PlaybackOptionsSheet — Video axis", () => {
  const audioOnly = [audio("a-en", "English", { isDefault: true })];
  const twoVideos = [
    videoStream("v-col", "Colour", { isDefault: true }),
    videoStream("v-bw", "Black & White"),
  ];

  it("is hidden when the File carries a single (or no) Video Stream", () => {
    render(
      <PlaybackOptionsSheet
        title={streamsDetail(audioOnly, [videoStream("v-only", "1080p", { isDefault: true })])}
        userId="u1"
        open
        onClose={() => {}}
        onPlay={() => {}}
      />,
    );
    expect(screen.queryByTestId("video-section")).not.toBeInTheDocument();
  });

  it("renders Auto + a row per variant when the File carries >1 Video Stream", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(audioOnly, twoVideos)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.getByTestId("video-option-auto")).toBeInTheDocument();
    expect(screen.getAllByTestId("video-option")).toHaveLength(2);
    expect(screen.getByTestId("video-option-auto")).toHaveAttribute("aria-checked", "true");
  });

  it("Auto omits videoStreamId; an explicit pick sends it to Play", () => {
    let onPlay = playAndCapture(streamsDetail(audioOnly, twoVideos), () => {});
    expect(onPlay).toHaveBeenCalledWith({ audioStreamId: null, videoStreamId: null });
    onPlay = playAndCapture(streamsDetail(audioOnly, twoVideos), () =>
      fireEvent.click(screen.getByRole("radio", { name: /Black & White/ })),
    );
    expect(onPlay).toHaveBeenCalledWith({ audioStreamId: null, videoStreamId: "v-bw" });
  });

  it("never writes the video pick to the preference store (ADR-0011)", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(audioOnly, twoVideos)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    fireEvent.click(screen.getByRole("radio", { name: /Black & White/ }));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    const raw = window.localStorage.getItem("juicebox.playback-pref.u1.title.t1");
    expect(raw).not.toContain("v-bw");
    expect(raw).not.toContain("videoStreamId");
  });

  it("copy says \"Video\", never \"Video track\"", () => {
    render(
      <PlaybackOptionsSheet title={streamsDetail(audioOnly, twoVideos)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    const section = screen.getByTestId("video-section");
    expect(section).toHaveTextContent("Video");
    expect(section.textContent).not.toMatch(/Video track/i);
  });
});

// ── Audio-delivery axis: the AAC-stereo toggle (issue 06, §7) ────────────────────
// A PERSISTED axis (no server memory, no contract field): a checkbox riding the pref
// draft like Edition / Quality / Subtitle. Play commits it; backing out discards it.
// The narrowing itself (deviceProfile → aac/2ch) is the resolver/session's job — the
// sheet only owns the flag.

describe("PlaybackOptionsSheet — AAC-stereo toggle", () => {
  it("shows the \"Transcode to AAC (Stereo)\" toggle, off by default", () => {
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    const toggle = screen.getByRole("checkbox", { name: /Transcode to AAC \(Stereo\)/ });
    expect(toggle).toHaveAttribute("aria-checked", "false");
  });

  it("Play commits the toggle to the preference store (draft-only until then)", () => {
    const onPlay = vi.fn();
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
    );
    fireEvent.click(screen.getByTestId("aac-stereo-toggle"));
    // A toggle is a draft change only — nothing persisted yet.
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).aacStereo).toBe(false);
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).aacStereo).toBe(true);
    expect(onPlay).toHaveBeenCalledTimes(1);
  });

  it("opens reflecting the saved toggle and commits off over it", () => {
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: null, qualityCap: null, subtitle: null, aacStereo: true }),
    );
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    const toggle = screen.getByTestId("aac-stereo-toggle");
    expect(toggle).toHaveAttribute("aria-checked", "true");
    fireEvent.click(toggle);
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).aacStereo).toBe(false);
  });

  it("backing out discards the draft toggle (preference untouched)", () => {
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    fireEvent.click(screen.getByTestId("aac-stereo-toggle"));
    fireEvent.click(screen.getByTestId("playback-options-cancel"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).aacStereo).toBe(false);
  });
});

// ── Subtitle axis (issue 05, ADR-0020) ───────────────────────────────────────────
// A PERSISTED axis (subtitle choice has no server memory): Off + one source-tagged
// row per Subtitle track, SELECTION ONLY (no network on open/selection), persisted BY
// LANGUAGE (+ forced) — never the track id — so a Show's choice ports across Episodes.

function sub(id: string, extra: Partial<SubtitleTrack>): SubtitleTrack {
  return { id, source: "embedded", kind: "text", forced: false, label: id, ...extra };
}

/** A Movie carrying the given Subtitle tracks on its detail payload. */
function subsDetail(subtitles: SubtitleTrack[]): TitleDetail {
  return { ...movieDetail(), subtitles };
}

const tracks = [
  sub("s-en", { source: "embedded", kind: "text", language: "en", label: "English" }),
  sub("s-en-img", { source: "sidecar", kind: "image", language: "en", label: "English (PGS)" }),
  sub("s-fr", { source: "fetched", kind: "text", language: "fr", label: "Français" }),
];

describe("PlaybackOptionsSheet — Subtitle axis", () => {
  it("shows Off + one source-tagged row per Subtitle track, Off active by default, no network", () => {
    render(
      <PlaybackOptionsSheet title={subsDetail(tracks)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(apiCalls.calls).toBe(0); // built purely from the in-hand payload
    expect(screen.getByTestId("subtitle-option-off")).toBeInTheDocument();
    expect(screen.getByTestId("subtitle-option-off")).toHaveAttribute("aria-checked", "true");
    const rows = screen.getAllByTestId("subtitle-option");
    expect(rows).toHaveLength(3);
    // Each row carries its SOURCE tag (Embedded / Sidecar / Fetched).
    const section = screen.getByTestId("subtitles-section");
    expect(section).toHaveTextContent("Embedded");
    expect(section).toHaveTextContent("Sidecar");
    expect(section).toHaveTextContent("Fetched");
  });

  it("renders nothing when the Title carries no Subtitle track (Off alone is no choice)", () => {
    render(
      <PlaybackOptionsSheet title={subsDetail([])} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.queryByTestId("subtitles-section")).not.toBeInTheDocument();
  });

  it("Play commits the pick BY LANGUAGE (+ forced), NOT by id", () => {
    const onPlay = vi.fn();
    render(
      <PlaybackOptionsSheet title={subsDetail(tracks)} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
    );
    // Pick the French track (a draft change only).
    fireEvent.click(screen.getByRole("radio", { name: /Français/ }));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).subtitle).toBeNull();
    // Play commits + starts.
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).subtitle).toEqual({
      language: "fr",
      forced: false,
    });
    expect(onPlay).toHaveBeenCalledTimes(1);
    // The stored value is the LANGUAGE key, not the track id.
    const raw = window.localStorage.getItem("juicebox.playback-pref.u1.title.t1");
    expect(raw).not.toContain("s-fr");
  });

  it("opens reflecting a saved pick and commits Off over it", () => {
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: null, qualityCap: null, subtitle: { language: "fr", forced: false } }),
    );
    render(
      <PlaybackOptionsSheet title={subsDetail(tracks)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.getByRole("radio", { name: /Français/ })).toHaveAttribute("aria-checked", "true");
    fireEvent.click(screen.getByTestId("subtitle-option-off"));
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" }).subtitle).toBeNull();
  });

  it("copy uses \"Subtitle track\", source-tagged, never a bare \"Track\"", () => {
    render(
      <PlaybackOptionsSheet title={subsDetail(tracks)} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    const group = screen.getByRole("radiogroup", { name: "Subtitle track" });
    expect(group).toBeInTheDocument();
    // No bare "Track" (a standalone word) leaks into the section copy.
    expect(screen.getByTestId("subtitles-section").textContent).not.toMatch(/\bTrack\b/);
  });
});

// ── Force Remux axis (issue 07, appletv-web-parity §10) ──────────────────────────
// A FLAG-GATED checkbox: hidden entirely unless the server advertises
// `features.remuxSelectedOnly`, and enabled ONLY while the draft would otherwise
// DIRECT PLAY — a Quality-cap downscale (issue 03) or AAC-stereo on (issue 06)
// disables it AND Play then commits the axis OFF, so the flag never persists under
// (and never rides) a transcoding draft.

/** Mount under a ready handshake advertising `remuxSelectedOnly` — the sheet reads
 * the flag through useOptionalFeature, so a bare mount (no provider, as every other
 * describe in this file uses) is exactly the absent-flag / older-server case. */
function withRemuxFlag(ui: ReactElement, on = true): ReactElement {
  const info: ServerInfo = {
    version: "test",
    supportedVersions: [1],
    features: { remuxSelectedOnly: on },
    setupRequired: false,
  };
  return (
    <ServerInfoStateProvider state={{ status: "ready", info }}>{ui}</ServerInfoStateProvider>
  );
}

describe("PlaybackOptionsSheet — Force Remux checkbox", () => {
  const prefT1 = () => loadPreference(window.localStorage, "u1", { kind: "title", id: "t1" });

  it("is hidden entirely without the feature flag (bare mount = older server)", () => {
    render(
      <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
    );
    expect(screen.queryByTestId("force-remux-section")).not.toBeInTheDocument();
  });

  it("is hidden when the handshake advertises the flag as false", () => {
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
        false,
      ),
    );
    expect(screen.queryByTestId("force-remux-section")).not.toBeInTheDocument();
  });

  it("shows enabled + unchecked on an otherwise-direct-play draft; Play commits true", () => {
    const onPlay = vi.fn();
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={onPlay} />,
      ),
    );
    const toggle = screen.getByRole("checkbox", { name: /Force Remux/ });
    expect(toggle).not.toBeDisabled();
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(toggle);
    // A toggle is a draft change only — nothing persisted yet.
    expect(prefT1().remuxSelectedOnly).toBe(false);
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(prefT1().remuxSelectedOnly).toBe(true);
    expect(onPlay).toHaveBeenCalledTimes(1);
  });

  it("a Quality-cap rung disables it (shown unchecked) and Play commits the axis OFF", () => {
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={uhdDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
      ),
    );
    // Check Force Remux while the draft still direct-plays…
    fireEvent.click(screen.getByTestId("force-remux-toggle"));
    expect(screen.getByTestId("force-remux-toggle")).toHaveAttribute("aria-checked", "true");
    // …then pick a downscale rung: the draft now transcodes, so the checkbox
    // disables and honestly reads unchecked (that is what Play will commit).
    fireEvent.click(screen.getByRole("radio", { name: /720p/ }));
    const toggle = screen.getByTestId("force-remux-toggle");
    expect(toggle).toBeDisabled();
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(prefT1().remuxSelectedOnly).toBe(false);
    expect(prefT1().qualityCap).toBe("720p");
  });

  it("AAC-stereo on disables it and Play commits the axis OFF", () => {
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
      ),
    );
    fireEvent.click(screen.getByTestId("force-remux-toggle"));
    fireEvent.click(screen.getByTestId("aac-stereo-toggle"));
    const toggle = screen.getByTestId("force-remux-toggle");
    expect(toggle).toBeDisabled();
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(prefT1().remuxSelectedOnly).toBe(false);
    expect(prefT1().aacStereo).toBe(true);
  });

  it("re-enables with the draft check preserved when the transcoding pick is reverted", () => {
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
      ),
    );
    fireEvent.click(screen.getByTestId("force-remux-toggle"));
    fireEvent.click(screen.getByTestId("aac-stereo-toggle")); // AAC on → disabled
    expect(screen.getByTestId("force-remux-toggle")).toBeDisabled();
    fireEvent.click(screen.getByTestId("aac-stereo-toggle")); // AAC off again
    const toggle = screen.getByTestId("force-remux-toggle");
    // The draft check survives the round-trip — only a COMMIT clears it.
    expect(toggle).not.toBeDisabled();
    expect(toggle).toHaveAttribute("aria-checked", "true");
  });

  it("opens reflecting the saved checkbox and commits off over it", () => {
    window.localStorage.setItem(
      "juicebox.playback-pref.u1.title.t1",
      JSON.stringify({ editionName: null, qualityCap: null, subtitle: null, remuxSelectedOnly: true }),
    );
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
      ),
    );
    const toggle = screen.getByTestId("force-remux-toggle");
    expect(toggle).toHaveAttribute("aria-checked", "true");
    fireEvent.click(toggle);
    fireEvent.click(screen.getByTestId("playback-options-play"));
    expect(prefT1().remuxSelectedOnly).toBe(false);
  });

  it("backing out discards the draft check (preference untouched)", () => {
    render(
      withRemuxFlag(
        <PlaybackOptionsSheet title={movieDetail()} userId="u1" open onClose={() => {}} onPlay={() => {}} />,
      ),
    );
    fireEvent.click(screen.getByTestId("force-remux-toggle"));
    fireEvent.click(screen.getByTestId("playback-options-cancel"));
    expect(prefT1().remuxSelectedOnly).toBe(false);
  });
});
