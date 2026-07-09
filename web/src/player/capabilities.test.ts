import { describe, it, expect, vi, afterEach } from "vitest";
import { deriveCapabilityProfile } from "./capabilities";

// The capability profile must be derived from the ACTUAL browser, so a browser
// that can play mp4/h264/aac reports those (→ directPlay), and one that cannot
// play matroska/mpeg4 omits them (→ the server's TRANSCODE_REQUIRED). We drive
// canPlayType to assert exactly that mapping.

afterEach(() => vi.restoreAllMocks());

function stubCanPlay(fn: (mime: string) => string) {
  vi.spyOn(HTMLMediaElement.prototype, "canPlayType").mockImplementation(fn);
}

describe("deriveCapabilityProfile", () => {
  it("reports mp4/h264/aac when the browser can play them", () => {
    stubCanPlay((mime) => (/mp4|avc1|mp4a/.test(mime) ? "probably" : ""));
    const { deviceProfile, constraints } = deriveCapabilityProfile();
    expect(deviceProfile.containers).toContain("mp4");
    expect(deviceProfile.videoCodecs.map((c) => c.codec)).toContain("h264");
    expect(deviceProfile.audioCodecs).toContain("aac");
    // A generous bitrate + a resolution cap are always present.
    expect(constraints.maxBitrate).toBeGreaterThan(0);
    expect(constraints.maxResolution).toMatch(/\d+p/);
  });

  it("omits matroska/mpeg4 when the browser cannot play them", () => {
    // An mp4-only browser: matroska and mpeg4 are NOT claimed, so the server will
    // return TRANSCODE_REQUIRED for an .mkv/mpeg4 file (the honest outcome).
    stubCanPlay((mime) => (/mp4|avc1|mp4a/.test(mime) ? "probably" : ""));
    const { deviceProfile } = deriveCapabilityProfile();
    expect(deviceProfile.containers).not.toContain("matroska");
    expect(deviceProfile.containers).not.toContain("mkv");
    expect(deviceProfile.videoCodecs.map((c) => c.codec)).not.toContain("mpeg4");
  });

  it("claims nothing when the browser can play nothing", () => {
    stubCanPlay(() => "");
    const { deviceProfile } = deriveCapabilityProfile();
    expect(deviceProfile.containers).toHaveLength(0);
    expect(deviceProfile.videoCodecs).toHaveLength(0);
    expect(deviceProfile.audioCodecs).toHaveLength(0);
  });

  it("accepts a 'maybe' verdict (honest answer for many codecs)", () => {
    stubCanPlay((mime) => (/mp4/.test(mime) ? "maybe" : ""));
    const { deviceProfile } = deriveCapabilityProfile();
    expect(deviceProfile.containers).toContain("mp4");
  });

  it("sets hevcInMpegts on the hls.js path (no native HLS)", () => {
    // An MSE browser (Chrome/Firefox): hls.js ≥ 1.6 demuxes HEVC-in-TS itself, and
    // the TS pipeline's dictated cuts give the exact playlists strict MSE playback
    // needs — so the profile asks for copied HEVC over MPEG-TS.
    stubCanPlay((mime) => (/mp4|avc1|mp4a/.test(mime) ? "probably" : ""));
    const { deviceProfile } = deriveCapabilityProfile();
    expect(deviceProfile.hevcInMpegts).toBe(true);
  });

  it("does NOT set hevcInMpegts on the native-HLS path (Safari needs HEVC in fMP4)", () => {
    stubCanPlay((mime) => (/mp4|avc1|mp4a|mpegurl/.test(mime) ? "maybe" : ""));
    // Native-HLS shape: Safari also exposes video.audioTracks (the second half of
    // the canPlayHlsNatively feature check).
    Object.defineProperty(HTMLMediaElement.prototype, "audioTracks", {
      value: { length: 0 },
      configurable: true,
    });
    try {
      const { deviceProfile } = deriveCapabilityProfile();
      expect(deviceProfile.hevcInMpegts).toBe(false);
    } finally {
      delete (HTMLMediaElement.prototype as { audioTracks?: unknown }).audioTracks;
    }
  });

  it("never advertises ac3/eac3, even when the browser claims it (Safari's false positive)", () => {
    // Safari returns "maybe" for `ac-3`/`ec-3` (macOS has OS decoders for AirPlay/
    // passthrough) but cannot decode AC3/E-AC3 in an in-page <video>. Advertising it
    // would make the server COPY that audio verbatim (direct play / remux) — a broken,
    // silent stream — instead of transcoding to AAC. So the profile must omit ac3/eac3
    // regardless of canPlayType, and still keep the codecs the browser truly decodes.
    stubCanPlay((mime) => (/mp4|webm|avc1|mp4a|ac-3|ec-3|opus|vorbis|flac/.test(mime) ? "maybe" : ""));
    const { deviceProfile } = deriveCapabilityProfile();
    expect(deviceProfile.audioCodecs).not.toContain("ac3");
    expect(deviceProfile.audioCodecs).not.toContain("eac3");
    // The genuinely in-page-decodable codecs are still advertised.
    expect(deviceProfile.audioCodecs).toContain("aac");
    expect(deviceProfile.audioCodecs).toEqual(
      expect.arrayContaining(["aac", "mp3", "opus", "vorbis", "flac"]),
    );
  });
});
