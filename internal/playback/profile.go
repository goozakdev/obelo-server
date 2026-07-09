// Package playback is the direct-play tier of the three-tier playback model
// (ADR-0003 tier 1, ADR-0004 progressive HTTP byte-range). It merges a client's
// Capability profile against a File's ffprobed technical attributes to make a
// binary negotiation decision — directPlay or TRANSCODE_REQUIRED — selects the
// best Edition for the client, and owns the ephemeral in-memory Playback
// sessions that back the progressive stream URL.
//
// Scope (issue 07): direct play only. There is no remux/transcode tier here, no
// HLS, and no SERVER_BUSY governance — an unplayable File yields an honest
// TRANSCODE_REQUIRED rather than a fake stream. The package is transport-
// agnostic: it speaks profiles, decisions, and sessions, not HTTP; the api
// package wraps it in thin handlers.
//
// Deferred (documented choices, not omissions):
//   - The api-contract's "register a device profile once on the Device, reference
//     it by clientId" optimization is DEFERRED. This slice accepts the full
//     capability profile inline on every playback request, which keeps the
//     negotiation pure and self-contained.
//   - Progress reporting, keepalive, and timeout-reaping of idle sessions are
//     issue 08. The session Manager is built so issue 08 can add a last-progress
//     timestamp and a reaper without reshaping the session record.
//   - Subtitle delivery (sidecar SRT→WebVTT) and burn-in are out of scope; the
//     decision reports subtitle mode "none".
package playback

import "strings"

// DeviceProfile is the static half of a Capability profile (CONTEXT.md): what a
// client's hardware/software can play, independent of the current network. It is
// the device-profile shape from docs/api-contract.md, decoded as-is.
type DeviceProfile struct {
	// Containers the client can demux (e.g. "mp4", "mkv"). Compared against the
	// File's container. ffprobe reports "matroska" for .mkv; NormalizeContainer
	// folds the common aliases so a profile listing "mkv" matches.
	Containers []string
	// VideoCodecs the client can decode, each carrying its own resolution/HDR
	// ceiling (per-codec, not a flat list — a device may do h264@1080p but
	// hevc@2160p).
	VideoCodecs []VideoCodecSupport
	// AudioCodecs the client can decode (e.g. "aac", "ac3").
	AudioCodecs []string
	// MaxAudioChannels caps channel count (0 = unspecified, treated as no cap).
	MaxAudioChannels int
	// TextSubtitleFormats the client can render as selectable tracks. Unused this
	// slice (subtitle mode is always "none") but decoded for forward-compat.
	TextSubtitleFormats []string
	// HevcInMpegTS declares the client can play a COPIED HEVC video inside MPEG-TS
	// HLS segments. Apple's native HLS player requires HEVC in fMP4/CMAF, but an
	// hls.js (MSE) client demuxes HEVC-in-TS itself (hls.js ≥ 1.6) — and the TS
	// pipeline is the one with dictated cuts and exact synthesized playlists, which
	// strict MSE playback needs (the fMP4 hls-muxer's cut grid can drift from the
	// synthesized playlist, stalling hls.js). False (a native/unknown client) keeps
	// HEVC on fMP4.
	HevcInMpegTS bool
}

// VideoCodecSupport is one decodable video codec plus its ceiling on this device.
type VideoCodecSupport struct {
	Codec string
	// MaxLevel is the codec level cap (e.g. "4.2"); recorded but not enforced this
	// slice (ffprobe level parsing is deferred — resolution is the binding limit).
	MaxLevel string
	// MaxResolution caps the playable resolution for this codec (e.g. "1080p").
	// Empty means no per-codec cap (the session constraint still applies).
	MaxResolution string
	// HDR formats supported for this codec; recorded, not yet enforced.
	HDR []string
}

// Constraints is the dynamic half of a Capability profile (CONTEXT.md): the
// per-request limits that reflect the current network and user quality cap. This
// is the field set most likely to flip a direct-play-capable File into a
// transcode (api-contract.md), so it is merged on top of the device profile.
type Constraints struct {
	// MaxBitrate caps the File's overall bitrate in bits/sec (0 = no cap).
	MaxBitrate int64
	// MaxResolution caps the playable resolution across all codecs (e.g. "1080p").
	// Empty = no cap.
	MaxResolution string
	// PreferredAudioLang selects the audio Stream by language (ISO code, e.g.
	// "en"); when no Stream matches, the default/first audio Stream is used.
	PreferredAudioLang string
	// PreferredSubtitleLang is recorded for forward-compat; subtitles are "none".
	PreferredSubtitleLang string
}

// supportsContainer reports whether the device can demux the given container,
// folding ffprobe's aliases (e.g. "matroska" ↔ "mkv") so a human-written profile
// matches the probed value.
func (p DeviceProfile) supportsContainer(container string) bool {
	want := NormalizeContainer(container)
	for _, c := range p.Containers {
		if NormalizeContainer(c) == want {
			return true
		}
	}
	return false
}

// videoCodec returns the device's support entry for the given video codec, or
// (zero, false) when the device cannot decode it at all.
func (p DeviceProfile) videoCodec(codec string) (VideoCodecSupport, bool) {
	want := strings.ToLower(strings.TrimSpace(codec))
	for _, v := range p.VideoCodecs {
		if strings.ToLower(strings.TrimSpace(v.Codec)) == want {
			return v, true
		}
	}
	return VideoCodecSupport{}, false
}

// supportsAudio reports whether the device can decode the given audio codec.
func (p DeviceProfile) supportsAudio(codec string) bool {
	want := strings.ToLower(strings.TrimSpace(codec))
	for _, c := range p.AudioCodecs {
		if strings.ToLower(strings.TrimSpace(c)) == want {
			return true
		}
	}
	return false
}

// NormalizeContainer folds ffprobe's container naming to the short tokens a
// client profile uses. ffprobe reports a comma-joined format list (e.g.
// "mov,mp4,m4a,3gp,3g2,mj2") for the MP4 family and "matroska,webm" for MKV; we
// reduce both to the canonical client token ("mp4", "mkv").
func NormalizeContainer(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case c == "":
		return ""
	case strings.Contains(c, "matroska"), c == "mkv":
		return "mkv"
	case strings.Contains(c, "mp4"), strings.Contains(c, "mov"), c == "m4v":
		return "mp4"
	case strings.Contains(c, "webm"):
		return "webm"
	default:
		// A single clean token (e.g. "mp4", "avi") passes through; a comma list we
		// don't recognize collapses to its first element.
		if i := strings.IndexByte(c, ','); i >= 0 {
			return c[:i]
		}
		return c
	}
}

// resolutionHeight maps a client resolution token ("720p", "1080p", "2160p"/"4k")
// to its pixel height. Unrecognized or empty tokens return 0, meaning "no cap" —
// a forgiving posture so an odd token never silently blocks direct play.
func resolutionHeight(token string) int {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "":
		return 0
	case "144p":
		return 144
	case "240p":
		return 240
	case "360p":
		return 360
	case "480p", "sd":
		return 480
	case "576p":
		return 576
	case "720p", "hd":
		return 720
	case "1080p", "fhd":
		return 1080
	case "1440p", "2k":
		return 1440
	case "2160p", "4k", "uhd":
		return 2160
	case "4320p", "8k":
		return 4320
	default:
		return 0
	}
}
