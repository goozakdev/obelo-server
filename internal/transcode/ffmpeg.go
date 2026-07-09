// Package transcode is the FFmpeg seam for the remux/transcode playback tiers
// (ADR-0003 tiers 2–3, ADR-0004 HLS delivery). It mirrors the scanner's Prober
// seam (internal/scanner/ffprobe.go): a Runner interface that the real
// implementation satisfies by shelling out to the bundled `ffmpeg` binary, and
// that unit tests fake without spawning a process.
//
// Command construction is kept PURE and separately unit-testable (RemuxArgs,
// TranscodeArgs): given a source File path, an output directory, and (for
// transcode) the per-track encode plan, each returns the exact ffmpeg argument
// vector with no process execution. The Runner then runs that vector.
//
// Scope:
//   - slice 1 — RemuxArgs: the remux/direct-stream tier — `-c copy` repackaging
//     into HLS.
//   - slice 2 — TranscodeArgs: the transcode tier — re-encode video (libx264,
//     CPU) and/or audio (aac, downmix) to a single rendition fitting the client,
//     or copy an already-HLS-compatible track, into the same HLS container.
//
// The seam is shaped so these slot in as sibling arg-builders sharing the same
// HLS muxer flags and the same Runner. Seek realignment landed (SeekOffset), and
// the video re-encode backend is a per-backend descriptor (see backend /
// videoBackend): the CPU libx264 path plus all four HW backends — VideoToolbox
// (the reference, end-to-end testable on macOS), NVENC, VAAPI, and QSV (Linux GPU
// backends, arg-verified here and end-to-end tested only where the device exists).
package transcode

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// HLS output filenames written into a session's scratch dir. The playlist is
// served at the stable name index.m3u8; segments are numbered ts files the
// playlist references by relative name (so the api layer can serve them from the
// same directory without rewriting the playlist).
const (
	// PlaylistName is the media-playlist file ffmpeg writes (and we serve as
	// .../hls/index.m3u8).
	PlaylistName = "index.m3u8"
	// SegmentPattern is ffmpeg's hls_segment_filename template, relative to the
	// output dir. %d is the zero-based segment index, e.g. segment000.ts.
	SegmentPattern = "segment%03d.ts"
	// SegmentPatternFMP4 is the fMP4 (CMAF) segment template used when the video is
	// stream-copied as a codec MPEG-TS cannot carry for Safari — HEVC (ADR-0024).
	// fragmented-MP4 segments are .m4s and reference a shared init segment.
	SegmentPatternFMP4 = "segment%03d.m4s"
	// InitSegmentName is the fMP4 initialization segment ffmpeg writes once
	// (-hls_fmp4_init_filename) and the media playlist references via #EXT-X-MAP. The
	// api serves it by name like any other segment (ADR-0024).
	InitSegmentName = "init.mp4"
	// SegmentSeconds is the target HLS segment duration. ~4s balances startup
	// latency against playlist/segment count for a short clip.
	SegmentSeconds = 4
)

// RemuxJob describes one remux (direct-stream) operation: copy the source File's
// streams unchanged into an HLS media playlist + TS segments under OutputDir.
type RemuxJob struct {
	// SourcePath is the absolute path of the File to repackage.
	SourcePath string
	// OutputDir is the session scratch directory ffmpeg writes the playlist +
	// segments into. The caller has already created it.
	OutputDir string

	// Seek is the seek-realignment offset (ADR-0004): the HLS runtime sets it to
	// reposition a restarted job near a requested timestamp. The zero value (the
	// default) starts at the beginning, exactly as the first launch does. Remux
	// seeking is cheap, but the field exists on both jobs so the runtime can drive
	// the two tiers through one realignment path.
	Seek SeekOffset

	// AudioStreamIndex, when non-nil, EXPLICITLY maps the negotiated audio Stream by
	// its audio-relative index (the Nth audio stream, 0-based) — `-map 0:v:0 -map
	// 0:a:N` — so the copied output carries exactly the Stream the playback Decision
	// reports (audio-streams/02). Without it ffmpeg's implicit selection picks the
	// "best" audio (most channels), which on a multi-audio File is NOT necessarily
	// the negotiated one — the reported-vs-audible divergence this closes. It is nil
	// for a single-audio File so those args stay byte-for-byte unchanged; the caller
	// sets it only when the File carries more than one audio Stream.
	AudioStreamIndex *int

	// VideoStreamIndex, when non-nil, EXPLICITLY maps the negotiated video Stream by
	// its video-relative index (the Nth video stream, 0-based) — `-map 0:v:N` — so a
	// multi-video File (two cuts sharing one audio track) copies exactly the Stream the
	// playback Decision resolved, not ffmpeg's implicit "first" video
	// (selectable-video/01, ADR-0025). It generalizes the hardcoded `0:v:0`; nil for a
	// single-video File keeps the args byte-for-byte unchanged. Set alongside
	// AudioStreamIndex on the muxed path and on the VideoOnly demuxed variant; ffmpeg's
	// `0:v:N` ordinal counts ALL video streams, so the index comes from videoRelIndex
	// (which likewise counts cover art), not the cover-art-filtered selectable set.
	VideoStreamIndex *int

	// VideoOnly copies ONLY the video track (`-map 0:v:N -an`) — the demuxed
	// multi-audio layout (audio-streams/03, ADR-0022): the video variant carries no
	// audio, and every audio Stream rides as its own in-band `#EXT-X-MEDIA:TYPE=AUDIO`
	// rendition (AudioRenditionArgs) so an HLS player switches tracks natively. It is
	// mutually exclusive with AudioStreamIndex (a demuxed video variant maps no audio
	// at all); the caller sets it only for a multi-audio HLS session, so single-audio
	// remux stays byte-for-byte the muxed copy-everything args.
	VideoOnly bool

	// FMP4 delivers fragmented-MP4 (.m4s + an init segment) instead of MPEG-TS
	// (ADR-0024). It is set when the COPIED video codec is one MPEG-TS cannot carry
	// for Safari — HEVC — so remuxing a HEVC mkv produces an fMP4 HLS Safari can play
	// natively rather than an unplayable HEVC-in-TS. false keeps the MPEG-TS output
	// byte-for-byte unchanged (h264 rides TS everywhere).
	FMP4 bool

	// SegmentTimes, when non-empty, DICTATES the segment cut times (seconds on the
	// output timeline) instead of letting the HLS muxer pick them: the job uses
	// ffmpeg's segment muxer with -segment_times, so the produced segments match the
	// server-synthesized playlist BY CONSTRUCTION — including after a seek
	// realignment, where the hls muxer would re-anchor its hls_time grid at the seek
	// point and silently diverge from the playlist (the post-seek A/V desync bug).
	// Set for a stream-copied TS video (whose cuts fall on source keyframes the
	// server has already computed); nil keeps the plain hls-muxer output unchanged.
	SegmentTimes []float64
}

// SeekOffset is a seek-realignment position for an HLS job: the segment index the
// realigned ffmpeg should begin producing and the matching input-seek time. The
// two travel together because they MUST agree — ffmpeg seeks the input to
// StartSeconds and numbers the first emitted segment StartNumber, and the HLS
// runtime derives both from the same target segment index (StartSeconds =
// StartNumber * SegmentSeconds) so the produced file lands at exactly the name
// the server-owned playlist lists. A zero value (StartNumber 0, StartSeconds 0)
// is the initial, from-the-top launch and adds no seek flag.
type SeekOffset struct {
	// StartNumber is the zero-based index of the first segment the job emits
	// (ffmpeg -start_number). It keeps numbering monotonic across a realignment so
	// the realigned job writes/extends from the seek point under the same names the
	// playlist already references.
	StartNumber int
	// StartSeconds is the input-seek time (ffmpeg -ss, placed BEFORE -i for a fast,
	// keyframe-accurate input seek): StartNumber * SegmentSeconds for a UNIFORM
	// (re-encode / audio-rendition) job, or the target segment's exact keyframe
	// boundary for a video-COPY job (whose segments fall on the source's irregular
	// keyframes — fractional, hence float; a whole number renders identically to the
	// old integer form).
	StartSeconds float64
}

// inputSeekArgs is the `-ss <seconds>` input-seek prefix for a realigned job,
// placed BEFORE -i so ffmpeg fast-seeks the INPUT (decoding only from the nearest
// keyframe at/just before the offset) rather than decoding-then-discarding from
// the top. It returns nil for the zero offset so the initial launch is byte-for-
// byte unchanged from slices 1/2.
func (s SeekOffset) inputSeekArgs() []string {
	if s.StartSeconds <= 0 {
		return nil
	}
	return []string{"-ss", strconv.FormatFloat(s.StartSeconds, 'f', -1, 64)}
}

// outputOffsetArgs keeps a REALIGNED job's output timestamps CONTINUOUS with the
// session timeline: ffmpeg rebases output pts to ~0 after an input -ss, so without
// this a post-seek segment's timestamps jump BACKWARD relative to the segments a
// browser already buffered — Chrome's MSE demuxer rejects the append ("Parsed
// buffers not in DTS sequence") and playback dies. -output_ts_offset shifts the
// realigned output by the seek position so segment N's timestamps land where the
// playlist says they are, exactly like the from-the-top run. Empty for the initial
// launch (no offset, byte-for-byte unchanged).
func (s SeekOffset) outputOffsetArgs() []string {
	if s.StartSeconds <= 0 {
		return nil
	}
	return []string{"-output_ts_offset", strconv.FormatFloat(s.StartSeconds, 'f', -1, 64)}
}

// RemuxArgs builds the PURE ffmpeg argument vector for a remux job — no process
// is spawned. It is the unit-testable command builder: it asserts the
// direct-stream contract (`-c copy`, no re-encode) and the HLS muxer flags that
// write a VOD playlist + segments on demand into the scratch dir.
//
// The args, in order:
//   - -nostdin -y: never block on stdin; overwrite stale scratch from a prior run.
//   - -i <source>: the input File.
//   - -c copy: COPY every stream (the direct-stream tier — no re-encode).
//   - -f hls + hls_time/hls_playlist_type vod/hls_segment_filename: emit an HLS
//     VOD playlist that grows as segments are written, so the api layer can serve
//     segments as they appear (segments listed before they exist are waited for).
//   - -hls_flags independent_segments + -start_number 0: stable, self-contained
//     segments numbered from 0, matching SegmentPattern.
//
// OutputDir must be absolute (the api/session layer guarantees this); the
// playlist and segment templates are joined onto it so ffmpeg writes everything
// inside the session scratch (ADR-0007).
func RemuxArgs(job RemuxJob) []string {
	args := []string{"-nostdin", "-y"}
	// Input seek (realignment) goes BEFORE -i; empty for the from-the-top launch.
	args = append(args, job.Seek.inputSeekArgs()...)
	args = append(args, "-i", job.SourcePath)
	switch {
	case job.VideoOnly:
		// Demuxed multi-audio session (audio-streams/03): copy ONLY the video track;
		// each audio Stream is delivered as its own in-band rendition. -an drops audio
		// entirely, and the explicit video map keeps the video after -an disables the
		// implicit stream selection — the negotiated video Stream (0:v:N) on a multi-
		// video File, else the first (0:v:0), byte-for-byte unchanged.
		args = append(args, "-map", videoMapSpec(job.VideoStreamIndex), "-an")
	default:
		// Explicitly select the negotiated video and/or audio Stream on a multi-video or
		// multi-audio File: any -map disables ffmpeg's implicit single-stream selection,
		// so once either track is pinned BOTH are mapped. Subtitles are delivered
		// out-of-band / as in-band renditions (ADR-0020), never muxed into the remux TS,
		// so they are deliberately not mapped here. A plain single-video/single-audio
		// File pins neither and emits no -map — byte-for-byte the copy-everything args.
		args = append(args, remuxMapArgs(job.VideoStreamIndex, job.AudioStreamIndex)...)
	}
	args = append(args, "-c", "copy")
	args = append(args, job.Seek.outputOffsetArgs()...)
	return append(args, videoHlsOutput(job.OutputDir, job.FMP4, job.SegmentTimes, job.Seek.StartNumber)...)
}

// videoHlsOutput is the muxer tail for the VIDEO variant (remux or transcode):
//   - segmentTimes non-empty (a TS video COPY) → ffmpeg's SEGMENT muxer with the
//     cut times DICTATED, so the segments match the server-synthesized playlist by
//     construction, including across a seek realignment (the hls muxer would
//     re-anchor its grid at the seek point and diverge).
//   - fmp4 (a copied HEVC, ADR-0024) → the hls muxer in fragmented-MP4 mode.
//   - otherwise → the plain MPEG-TS hls muxer, byte-for-byte unchanged.
func videoHlsOutput(outputDir string, fmp4 bool, segmentTimes []float64, startNumber int) []string {
	if fmp4 {
		// Apple HLS requires the hvc1 sample-entry brand for HEVC (not the hev1 form
		// ffmpeg may otherwise emit), and the master's CODECS must match it — force it
		// so Safari plays the copied HEVC (ADR-0024). It is a no-op on a stream that is
		// not HEVC (the only non-h264 codec that reaches the fMP4 path here).
		args := []string{"-tag:v", "hvc1"}
		return append(args, hlsOutputArgsTyped(outputDir, PlaylistName, SegmentPatternFMP4, InitSegmentName, startNumber)...)
	}
	if len(segmentTimes) > 0 {
		return segmentMuxerArgs(outputDir, segmentTimes, startNumber)
	}
	return hlsOutputArgs(outputDir, startNumber)
}

// segmentMuxerArgs emits the `-f segment` tail that cuts at EXPLICIT times. The
// times are seconds on the OUTPUT timeline (a realigned job restarts at ~0), each
// nudged 2ms EARLY so float rounding can never push a cut past its intended
// keyframe (the muxer splits at the first keyframe whose pts reaches the time).
// The muxer also writes an incremental m3u8 list — unused for serving (the server
// owns the playlist) but it feeds the synth-playlist self-check and debugging.
func segmentMuxerArgs(outputDir string, times []float64, startNumber int) []string {
	parts := make([]string, 0, len(times))
	for _, t := range times {
		if t <= 0 {
			continue
		}
		parts = append(parts, strconv.FormatFloat(t-0.002, 'f', 3, 64))
	}
	args := []string{
		"-f", "segment",
		"-segment_format", "mpegts",
		"-segment_list", join(outputDir, PlaylistName),
		"-segment_list_type", "m3u8",
		"-segment_list_size", "0",
		"-segment_start_number", itoa(startNumber),
	}
	if len(parts) > 0 {
		args = append(args, "-segment_times", strings.Join(parts, ","))
	}
	return append(args, join(outputDir, SegmentPattern))
}

// videoMapSpec returns the `0:v:N` stream selector for an explicitly-mapped video
// track: the negotiated Stream's video-relative index when idx is set (a multi-video
// File whose chosen Stream is not the first), else `0:v:0` — the first (and, on a
// single-video File, only) video track. Returning `0:v:0` for a nil index keeps every
// existing single-video arg byte-for-byte unchanged (selectable-video/01).
func videoMapSpec(idx *int) string {
	n := 0
	if idx != nil {
		n = *idx
	}
	return "0:v:" + itoa(n)
}

// remuxMapArgs builds the explicit `-map` flags for a MUXED remux (the video and its
// audio copied together into one HLS variant). It emits maps only when a specific
// video or audio Stream must be pinned — a multi-video or multi-audio File; a plain
// single-video/single-audio File pins neither and returns nil, so `-c copy` carries
// everything byte-for-byte unchanged. Because ANY -map disables ffmpeg's implicit
// selection, once either track is pinned BOTH are mapped: the chosen video (0:v:N,
// else 0:v:0) plus either the chosen audio (0:a:N) or, when only the video was pinned,
// ALL audio (0:a) so a co-packaged shared audio track is not dropped.
func remuxMapArgs(videoIdx, audioIdx *int) []string {
	if videoIdx == nil && audioIdx == nil {
		return nil
	}
	args := []string{"-map", videoMapSpec(videoIdx)}
	if audioIdx != nil {
		return append(args, "-map", "0:a:"+itoa(*audioIdx))
	}
	return append(args, "-map", "0:a")
}

// hlsOutputArgs is the shared HLS muxer tail used by BOTH RemuxArgs and
// TranscodeArgs: -f hls + a VOD playlist that grows as segments are written into
// the session scratch dir, with self-contained, zero-based segments matching
// SegmentPattern. Factored out so the remux (`-c copy`) and transcode (re-encode)
// builders differ ONLY in their codec flags — the delivery contract (on-demand
// playlist + numbered .ts segments the api layer serves) is identical, which is
// exactly why the slice-1 HLS runtime can drive either job unchanged.
// hlsOutputArgs takes the segment index the muxer should number its first segment
// (startNumber): 0 for the initial from-the-top launch, the realignment target for
// a repositioned job, so segment names stay monotonic and coherent with the
// server-owned playlist across a realignment.
func hlsOutputArgs(outputDir string, startNumber int) []string {
	return hlsOutputArgsNamed(outputDir, PlaylistName, SegmentPattern, startNumber)
}

// hlsOutputArgsNamed is hlsOutputArgs with the playlist + segment names taken as
// parameters, so a demuxed audio rendition (audio-streams/03) can write its own
// namespaced files (audio_<id>.m3u8 / audio_<id>_%03d.ts) into the SHARED session
// scratch dir next to the video variant's index.m3u8 + segment%03d.ts. The muxer
// flags are otherwise identical to the video path, so a rendition is served by the
// exact same on-demand playlist/segment machinery as the video.
func hlsOutputArgsNamed(outputDir, playlistName, segmentPattern string, startNumber int) []string {
	return hlsOutputArgsTyped(outputDir, playlistName, segmentPattern, "", startNumber)
}

// hlsOutputArgsTyped is hlsOutputArgsNamed with the fMP4 initialization-segment
// name as an extra parameter: an empty fmp4Init keeps the MPEG-TS output
// (byte-for-byte the pre-fMP4 args), a non-empty one switches the muxer to
// fragmented-MP4 (`-hls_segment_type fmp4`) and names the init segment ffmpeg
// writes + the playlist's #EXT-X-MAP references (ADR-0024). fMP4 is used only when
// a stream-copied video codec (HEVC) cannot ride MPEG-TS for Safari; every existing
// caller passes "" and is unchanged.
func hlsOutputArgsTyped(outputDir, playlistName, segmentPattern, fmp4Init string, startNumber int) []string {
	args := []string{
		"-f", "hls",
		"-hls_time", itoa(SegmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments+temp_file",
	}
	if fmp4Init != "" {
		args = append(args,
			"-hls_segment_type", "fmp4",
			"-hls_fmp4_init_filename", fmp4Init,
			// No edit lists in the init segment (CMAF forbids them). ffmpeg's mov
			// muxer otherwise expresses a stream's start offset / b-frame delay as a
			// two-entry elst (an empty edit + a media-time shift), and Safari's
			// MASTER-playlist pipeline rejects such an init with a bare "failed to
			// decode" the moment it arrives — while, confusingly, playing the same
			// stream fine when its media playlist is the <video> src directly. The
			// mov muxer shifts timestamps instead when edit lists are off.
			"-hls_segment_options", "use_editlist=0",
		)
	} else {
		args = append(args, "-hls_segment_type", "mpegts")
	}
	return append(args,
		"-hls_segment_filename", join(outputDir, segmentPattern),
		"-start_number", itoa(startNumber),
		join(outputDir, playlistName),
	)
}

// AudioRenditionPlaylist / AudioRenditionSegmentPattern name a demuxed audio
// rendition's HLS artifacts within the session scratch dir (audio-streams/03),
// namespaced by the audio Stream id so every rendition coexists flatly alongside
// the video variant (index.m3u8 / segment%03d.ts) and the other renditions. A
// Stream id is a UUID (hyphens, no underscore), so the trailing _%03d segment index
// is unambiguously separable from the id by the LAST underscore — the same
// convention the in-band subtitle renditions use.
func AudioRenditionPlaylist(streamID string) string {
	return "audio_" + streamID + ".m3u8"
}

func AudioRenditionSegmentPattern(streamID string) string {
	return "audio_" + streamID + "_%03d.ts"
}

// AudioRenditionSegmentPatternFMP4 / AudioRenditionInit are the fragmented-MP4
// analogues used when the session delivers fMP4 (a copied-HEVC video variant,
// ADR-0024): the alternate audio renditions must share the variant's container, so
// they emit .m4s segments referencing a per-rendition init segment. Both stay
// namespaced by the audio Stream id so every rendition's files remain distinct in
// the shared scratch dir.
func AudioRenditionSegmentPatternFMP4(streamID string) string {
	return "audio_" + streamID + "_%03d.m4s"
}

func AudioRenditionInit(streamID string) string {
	return "audio_" + streamID + "_init.mp4"
}

// HLS-friendly target codecs for the transcode tier. A single-rendition HLS
// stream the browser/hls.js (and Safari native HLS) reliably plays is h264 video
// + aac audio in mpegts segments, so re-encodes always target these and a track
// is only copied when it is ALREADY one of them (see TranscodeArgs).
const (
	// VideoCodecH264 is the libx264 output / the only video codec safe to copy.
	VideoCodecH264 = "h264"
	// AudioCodecAAC is the aac output / the only audio codec safe to copy.
	AudioCodecAAC = "aac"

	// videoEncoderLibx264 is the CPU H.264 encoder — the always-available
	// software fallback (ADR-0009: HW accel is opt-in with a guaranteed CPU path).
	// videoBackend(job.Accel) maps the chosen Accel to a concrete backend
	// descriptor; the CPU path is the guaranteed fallback for every value.
	videoEncoderLibx264 = "libx264"
	// videoEncoderVideoToolbox is Apple's VideoToolbox H.264 encoder (macOS) — the
	// first real HW backend wired behind the descriptor seam. It encodes from
	// system-memory frames (no device-init flags, no frame upload), so it reuses
	// the CPU `scale=` filter and the shared `-b:v` rate control; only the `-c:v`
	// encoder and preset flags differ from the CPU path.
	videoEncoderVideoToolbox = "h264_videotoolbox"
	// audioEncoderAAC is ffmpeg's built-in AAC encoder.
	audioEncoderAAC = "aac"

	// x264Preset trades encode speed for compression. "veryfast" keeps real-time
	// on-demand transcoding responsive on the CPU path without a visible quality
	// hit at our single rendition.
	x264Preset = "veryfast"

	// nvencPreset is NVENC's rate-controlled quality/speed preset (the p1 fastest …
	// p7 slowest scale). p4 is the balanced midpoint — real-time-friendly for
	// on-demand transcoding without a visible quality hit at our single rendition,
	// the HW analogue of the CPU path's x264Preset "veryfast".
	nvencPreset = "p4"
	// vaapiRenderNode is the default DRM render node VAAPI initializes against.
	// renderD128 is the conventional first render node on a Linux box with a single
	// GPU. A CONFIGURABLE device node (for multi-GPU hosts) is deferred: config
	// exposes no device knob today, so this sane default covers the common
	// single-GPU case and can later be promoted to a config field without reshaping
	// the backend seam.
	vaapiRenderNode = "/dev/dri/renderD128"
)

// Accel selects the video encode backend for the transcode tier (ADR-0009). It
// is a config-driven knob threaded down to the args builder so the encoder
// choice lives in ONE place. AccelCPU is the zero value, so a TranscodeJob with
// no Accel set always gets the safe software path; the named HW backends below
// are honored by videoBackend, which maps each to a backend descriptor and falls
// back to CPU for any value it cannot serve (so the SERVER_BUSY governance and
// the always-available CPU guarantee are unchanged regardless of the value).
type Accel string

const (
	// AccelCPU is the software libx264 path — the always-available default
	// (HardwareAccel off). It is the zero value, so an un-set Accel is CPU.
	AccelCPU Accel = ""
	// AccelAuto asks the server to pick the best available hardware encoder
	// (HardwareAccel auto). Detection/validation is a setup-time concern that
	// resolves auto to a concrete backend BEFORE it reaches the args builder
	// (ADR-0009); that detector is a later slice, so an un-resolved AccelAuto here
	// still safely maps to CPU in videoBackend.
	AccelAuto Accel = "auto"
	// AccelNVENC selects NVIDIA's h264_nvenc encoder, wired to a real backend
	// descriptor (nvencBackend): CPU-decode → NVENC encode with the CPU scale
	// filter and a rate-controlled -preset pN (the robust baseline, no full-CUDA
	// upload). The detector validates + resolves it on a Linux/NVIDIA host.
	AccelNVENC Accel = "nvenc"
	// AccelVAAPI selects the VAAPI h264_vaapi encoder (Intel/AMD on Linux), wired to
	// vaapiBackend: a VAAPI-surface decode/scale/encode pipeline (device-init flags,
	// scale_vaapi, and an nv12 hwupload for system-memory-origin frames).
	AccelVAAPI Accel = "vaapi"
	// AccelQSV selects Intel Quick Sync's h264_qsv encoder, wired to qsvBackend: the
	// QSV-surface analogue of VAAPI (QSV device-init, scale_qsv, hwupload).
	AccelQSV Accel = "qsv"
	// AccelVideoToolbox selects Apple's h264_videotoolbox encoder (macOS) — the
	// reference HW backend and the only one this dev host can end-to-end test.
	AccelVideoToolbox Accel = "videotoolbox"
)

// backend describes how one video-encode backend contributes to the ffmpeg arg
// vector. CPU is just the descriptor with empty init flags + libx264. The seam
// generalizes the old videoEncoder(Accel) string: a real backend needs more than
// a different -c:v value (device-init flags before -i, its own preset/rate
// control, and a HW-domain scale filter), so each backend contributes its
// fragments while TranscodeArgs stays a pure, unit-testable assembler.
type backend struct {
	// initArgs go BEFORE -i (device init + decode hwaccel + output format),
	// e.g. VAAPI: ["-hwaccel","vaapi","-hwaccel_device",dev,"-hwaccel_output_format","vaapi"].
	// Empty ONLY for CPU (system-memory software decode). Every HW backend carries a
	// decode -hwaccel here so the heavy decode is offloaded to the device too, not
	// just the encode (issue 05): VideoToolbox/NVENC use the PLAIN form (-hwaccel
	// videotoolbox / -hwaccel cuda, no -hwaccel_output_format) so ffmpeg auto-
	// downloads decoded frames back to system memory and the CPU-domain scale +
	// yuv420p + encoder chain is unchanged; VAAPI/QSV add -hwaccel_output_format so
	// decoded frames STAY on HW surfaces for the HW scale/encode. These are emitted
	// ONLY on the re-encode path (TranscodeArgs), never on -c:v copy / audio-only.
	initArgs []string
	// encoder is the -c:v value: libx264 | h264_videotoolbox | h264_nvenc | ...
	encoder string
	// presetArgs are the encoder-specific quality/speed + pixel-format flags that
	// follow -c:v. CPU: -preset veryfast -pix_fmt yuv420p ; VideoToolbox: just
	// -pix_fmt yuv420p (it has no libx264-style -preset).
	presetArgs []string
	// scaleFilter renders the scale-down for a target height in this backend's
	// domain: CPU/VideoToolbox "scale=-2:'min(H,ih)'" vs "scale_vaapi=-2:H" /
	// "scale_qsv". It is only invoked when a height cap binds.
	scaleFilter func(maxHeight int) string
	// uploadFilter, when non-empty, moves CPU-decoded frames into HW surfaces
	// before HW scale/encode (VAAPI/QSV: "format=nv12,hwupload"). Empty for CPU and
	// VideoToolbox, which encode directly from system-memory frames.
	uploadFilter string
}

// videoFilterChain builds the -vf value for a backend's re-encode: the optional
// HW frame-upload (VAAPI/QSV move system-memory frames onto HW surfaces) followed
// by the scale-down when a height cap binds, joined in the backend's filter
// domain. It returns "" when neither applies — an un-capped CPU/VideoToolbox
// encode carries no -vf at all — so the CPU path stays byte-for-byte unchanged.
func videoFilterChain(be backend, maxHeight int) string {
	var filters []string
	if be.uploadFilter != "" {
		filters = append(filters, be.uploadFilter)
	}
	if maxHeight > 0 && be.scaleFilter != nil {
		filters = append(filters, be.scaleFilter(maxHeight))
	}
	return strings.Join(filters, ",")
}

// cpuScaleFilter is the system-memory scale-down shared by the CPU and
// VideoToolbox backends: scale to height H with an even width (-2) that preserves
// the aspect ratio, guarded by min(H,ih) so a source already at/below H is left
// untouched (never upscale). ih is the input height.
func cpuScaleFilter(maxHeight int) string {
	return "scale=-2:'min(" + itoa(maxHeight) + ",ih)'"
}

// videoBackend maps the chosen Accel to its backend descriptor. It is the single
// seam HW encoders hook into (ADR-0009): each named hardware value returns its real
// descriptor, and AccelCPU plus an un-resolved AccelAuto fall back to the CPU
// libx264 descriptor, guaranteeing the always-available software path. The CPU
// descriptor is byte-for-byte the flags the old inline CPU branch emitted, so the
// off path never changes. Every HW descriptor preserves the shared bitrate cap and
// the -force_key_frames segment alignment (both added by TranscodeArgs), so seek
// realignment and the bitrate contract hold on every backend.
func videoBackend(a Accel) backend {
	switch a {
	case AccelVideoToolbox:
		return videoToolboxBackend()
	case AccelNVENC:
		return nvencBackend()
	case AccelVAAPI:
		return vaapiBackend()
	case AccelQSV:
		return qsvBackend()
	default:
		return cpuBackend()
	}
}

// videoToolboxBackend is Apple's VideoToolbox descriptor (h264_videotoolbox,
// macOS) — the reference HW backend. DECODE and encode both run on the media
// engine: initArgs ask ffmpeg to hardware-decode with VideoToolbox (-hwaccel
// videotoolbox) in the PLAIN form — no -hwaccel_output_format — so the decoder
// auto-downloads frames back to system memory. That keeps the rest of the pipeline
// in the CPU domain (the cpuScaleFilter scale-down, -pix_fmt yuv420p, and the
// h264_videotoolbox encode from system-memory frames), so only the heavy decode
// moves off the CPU while everything downstream is byte-for-byte as before. This
// is the issue-05 fix for the motivating bug: a 4K HEVC source pegged the CPU
// because the DECODE was software even though the encode was on the media engine.
// The shared -b:v rate control and segment-boundary keyframe forcing (TranscodeArgs)
// are unchanged. Only -c:v and the preset flags differ from CPU besides this decode
// hwaccel.
func videoToolboxBackend() backend {
	return backend{
		initArgs:    []string{"-hwaccel", "videotoolbox"},
		encoder:     videoEncoderVideoToolbox,
		presetArgs:  []string{"-pix_fmt", "yuv420p"},
		scaleFilter: cpuScaleFilter,
	}
}

// nvencBackend is the NVIDIA NVENC descriptor (h264_nvenc). DECODE is now hardware
// too (issue 05): initArgs ask ffmpeg to decode on the GPU with CUDA (-hwaccel cuda)
// in the PLAIN form — no -hwaccel_output_format — so the decoder auto-downloads
// frames back to system memory and the rest of the pipeline stays in the CPU domain
// (the cpuScaleFilter scale-down, then -c:v h264_nvenc, whose encoder re-uploads to
// the GPU automatically). So the baseline is now HW-decode → CPU-scale → NVENC
// encode: there is still NO -init_hw_device and NO hwupload filter, and no full-CUDA
// scale_npp surface chain. Keeping the auto-download baseline (rather than a zero-
// copy scale_npp pipeline) trades a little memory bandwidth for robustness across
// driver/container combinations (PRD "robust baseline"), while still moving the
// heavy decode off the CPU. Quality/speed is the rate-controlled -preset pN
// (nvencPreset); the shared -b:v/-maxrate/-bufsize cap (TranscodeArgs) drives NVENC's
// native rate control, and yuv420p keeps the output broadly decodable.
//
// NOTE (arg-verified, not CI-run here): the macOS dev host has no NVIDIA hardware,
// so -hwaccel cuda is pinned by the exact-arg-vector unit test only; the gated real
// e2e self-skips on this box.
func nvencBackend() backend {
	return backend{
		initArgs:    []string{"-hwaccel", "cuda"},
		encoder:     videoEncoderNVENC,
		presetArgs:  []string{"-preset", nvencPreset, "-pix_fmt", "yuv420p"},
		scaleFilter: cpuScaleFilter,
	}
}

// vaapiBackend is the VAAPI descriptor (h264_vaapi; Intel/AMD on Linux). Decode,
// scale, and encode run in the VAAPI surface domain: initArgs init the device and
// ask ffmpeg to decode into VAAPI surfaces (-hwaccel vaapi -hwaccel_device <node>
// -hwaccel_output_format vaapi), scaleFilter is the HW scale_vaapi, and
// uploadFilter (format=nv12,hwupload) moves any system-memory-origin frames onto
// VAAPI surfaces before the HW scale/encode. The shared -b:v cap drives VAAPI's
// native rate control. vaapiRenderNode is the default DRM render node (a
// configurable device is deferred — see its doc).
//
// NOTE (arg-verified, not CI-run here): with -hwaccel_output_format vaapi the
// decoder already lands frames on VAAPI surfaces, so the hwupload covers the
// CPU-decoded-origin case (a redundant nv12 round-trip when the decode is HW). The
// macOS dev host has no VAAPI device, so this descriptor's confidence comes from
// the exact-arg-vector unit test plus the gated real e2e that self-skips here.
func vaapiBackend() backend {
	return backend{
		initArgs: []string{
			"-hwaccel", "vaapi",
			"-hwaccel_device", vaapiRenderNode,
			"-hwaccel_output_format", "vaapi",
		},
		encoder:      videoEncoderVAAPI,
		scaleFilter:  vaapiScaleFilter,
		uploadFilter: "format=nv12,hwupload",
	}
}

// qsvBackend is the Intel Quick Sync descriptor (h264_qsv). It mirrors VAAPI in the
// QSV surface domain: initArgs create the QSV device and decode into QSV surfaces
// (-init_hw_device qsv=hw -hwaccel qsv -hwaccel_output_format qsv), scaleFilter is
// scale_qsv, and the hwupload filter moves system-memory-origin frames onto QSV
// surfaces. The shared -b:v cap drives QSV's native rate control. Like VAAPI, this
// is arg-verified on the macOS dev host (no QSV device) and exercised end-to-end
// only by the gated real e2e where a working device validates.
func qsvBackend() backend {
	return backend{
		initArgs: []string{
			"-init_hw_device", "qsv=hw",
			"-hwaccel", "qsv",
			"-hwaccel_output_format", "qsv",
		},
		encoder:      videoEncoderQSV,
		scaleFilter:  qsvScaleFilter,
		uploadFilter: "format=nv12,hwupload",
	}
}

// vaapiScaleFilter renders the VAAPI-domain scale-down to height H with an even
// width (-2) that preserves the aspect ratio. The frames are already on VAAPI
// surfaces (decode hwaccel / hwupload), so the scale runs on the GPU. PlanVideo
// only sets a height cap when the source is TALLER, so no min() upscale guard is
// needed (the CPU path keeps one as belt-and-suspenders for its expression form).
func vaapiScaleFilter(maxHeight int) string {
	return "scale_vaapi=-2:" + itoa(maxHeight)
}

// qsvScaleFilter is vaapiScaleFilter's QSV-domain analogue (scale_qsv).
func qsvScaleFilter(maxHeight int) string {
	return "scale_qsv=-2:" + itoa(maxHeight)
}

// IsHardware reports whether Accel a selects a hardware video encoder rather than
// the always-available CPU libx264 path. It is derived from the SAME backend
// descriptor the args builder uses (videoBackend), so it stays correct as backends
// are wired: any value videoBackend maps to libx264 — AccelCPU and an un-resolved
// AccelAuto — reports false, while a genuinely HW-encoded backend (NVENC, VAAPI,
// QSV, or VideoToolbox) reports true.
//
// The playback layer uses it to arm the per-session hardware→CPU launch fallback
// (ADR-0009) ONLY for a transcode that is actually hardware-encoded: a CPU
// transcode (or a remux) has nothing to fall back FROM, so a launch failure there
// is an honest playback error, not a retry.
func IsHardware(a Accel) bool {
	return videoBackend(a).encoder != videoEncoderLibx264
}

// cpuBackend is the always-available software libx264 descriptor: no device-init
// flags, -preset veryfast + yuv420p, and the system-memory scale filter. These
// are exactly the flags the CPU re-encode branch emitted before the seam was
// generalized, so the knob-off path is unchanged.
func cpuBackend() backend {
	return backend{
		encoder:     videoEncoderLibx264,
		presetArgs:  []string{"-preset", x264Preset, "-pix_fmt", "yuv420p"},
		scaleFilter: cpuScaleFilter,
	}
}

// forceKeyFramesExpr forces an IDR keyframe at every SegmentSeconds boundary
// (t = 0, SegmentSeconds, 2*SegmentSeconds, …). n_forced is ffmpeg's count of
// keyframes already forced, so the expression fires once per interval. Uniform
// keyframe-aligned segments are the precondition for seek realignment: segment N
// then starts exactly at N*SegmentSeconds, so a job restarted at that input offset
// regenerates exactly that segment under the same name. It is a var (not a const)
// because it is derived from SegmentSeconds via itoa.
var forceKeyFramesExpr = "expr:gte(t,n_forced*" + itoa(SegmentSeconds) + ")"

// TranscodeJob describes one transcode operation: re-encode (or selectively
// copy) the source File's video + audio into a single HLS rendition fitting the
// client, written into OutputDir. The playback layer fills it from the
// negotiated Decision + the client's capability profile; TranscodeArgs turns it
// into the ffmpeg argument vector.
//
// The per-track Copy flags are the conservative copy-vs-re-encode decision made
// by the caller (PlanVideo/PlanAudio): copy only an already-HLS-friendly track,
// otherwise re-encode. TranscodeArgs trusts those flags — it does not re-derive
// the policy — so the decision stays in one pure, unit-tested place.
type TranscodeJob struct {
	// SourcePath is the absolute path of the File to transcode.
	SourcePath string
	// OutputDir is the session scratch directory (already created by the caller).
	OutputDir string

	// Video is the video-track plan.
	Video VideoPlan
	// Audio is the audio-track plan. HasAudio is false for a silent File (no audio
	// stream is mapped/encoded).
	Audio    AudioPlan
	HasAudio bool
	// AudioOnly is true for a Music Track / audio-only File: no video stream
	// exists, so TranscodeArgs maps NO video (-vn) and emits only the audio encode
	// (e.g. FLAC/ALAC → AAC), delivered over the same HLS path — an audio-only HLS
	// rendition is valid (issue tv-music/03, additive). Video is ignored when set.
	AudioOnly bool

	// VideoOnly drops the audio track (`-an`) from a video transcode — the demuxed
	// multi-audio layout (audio-streams/03, ADR-0022): the re-encoded/copied video
	// variant carries no audio, and every audio Stream rides as its own in-band
	// rendition (AudioRenditionArgs). It suppresses the audio section (HasAudio /
	// AudioStreamIndex are ignored) exactly as -vn does for AudioOnly, but keeps the
	// video. The caller sets it only for a multi-audio HLS session; single-audio
	// transcode stays the byte-for-byte muxed args. Ignored when AudioOnly is set (an
	// audio-only File has no video to keep).
	VideoOnly bool

	// Accel selects the video encode backend (ADR-0009 HW-accel knob). The zero
	// value (AccelCPU) is the software libx264 path — the always-available
	// fallback. VideoToolbox is wired to a real encoder (h264_videotoolbox); the
	// other named backends and an un-resolved AccelAuto still fall back to CPU
	// (videoBackend). The playback layer sets it from the server's HardwareAccel
	// config; with that off it stays CPU.
	Accel Accel
	// Burn, when non-nil, burns an IMAGE Subtitle track into the video frames
	// (ADR-0020, subtitles/04) via a `-filter_complex …overlay…` graph. Setting it
	// FORCES a video re-encode (Video.Copy is ignored — you cannot overlay onto a
	// copied stream) on the system-memory (CPU) filter domain, so the encode backend
	// is pinned to CPU libx264 for a burn-in job regardless of Accel: the overlay
	// runs in system memory, and a burned-in frame must be re-encoded anyway. Nil for
	// every non-burn job (byte-for-byte unchanged). AudioOnly wins over Burn (an
	// audio-only File has no video to burn onto).
	Burn *BurnSubtitle
	// Seek is the seek-realignment offset (ADR-0004): the HLS runtime sets it to
	// restart the encode near a requested timestamp so a seek-ahead segment is
	// produced without re-encoding everything before it. The zero value starts the
	// encode at the beginning, exactly as the first launch does.
	Seek SeekOffset

	// AudioStreamIndex, when non-nil, EXPLICITLY maps the negotiated audio Stream by
	// its audio-relative index (the Nth audio stream, 0-based) so the re-encoded /
	// copied output carries exactly the Stream the Decision reports, closing the
	// reported-vs-audible divergence on a multi-audio File (audio-streams/02). It is
	// nil for a single-audio File, keeping those args byte-for-byte unchanged. On the
	// video-bearing paths a set index also forces an explicit video -map (0:v:0),
	// since any -map disables ffmpeg's implicit selection; the burn path already maps
	// its filter output as the video and uses this to select the source audio. The
	// caller sets it only when the File carries more than one audio Stream.
	AudioStreamIndex *int

	// VideoStreamIndex, when non-nil, EXPLICITLY maps the negotiated video Stream by
	// its video-relative index (`-map 0:v:N`) so a multi-video File re-encodes/copies
	// the Stream the Decision resolved rather than ffmpeg's implicit first video
	// (selectable-video/01, ADR-0025). Like AudioStreamIndex, setting it forces an
	// explicit video -map on the video-bearing paths (and drives the burn overlay's
	// `[0:v:N]` input). nil for a single-video File keeps the args byte-for-byte
	// unchanged; the value counts ALL video streams (videoRelIndex), matching ffmpeg's
	// 0:v:N ordinal.
	VideoStreamIndex *int

	// FMP4 delivers fragmented-MP4 (.m4s + init segment) instead of MPEG-TS
	// (ADR-0024): set when the video is STREAM-COPIED as HEVC (Video.Copy true, a
	// codec MPEG-TS cannot carry for Safari), so a copy-video + transcode-audio
	// session plays natively in Safari. false keeps the MPEG-TS output unchanged — the
	// h264 re-encode and h264 copy both ride TS.
	FMP4 bool

	// SegmentTimes dictates the cut times via the segment muxer for a TS video-COPY
	// transcode, exactly as on RemuxJob (see there): the copied stream cannot be
	// re-cut, so the cuts must match the server-synthesized playlist even across a
	// seek realignment. nil keeps the hls-muxer output unchanged (all re-encodes).
	SegmentTimes []float64
}

// VideoPlan is the resolved video-track decision.
type VideoPlan struct {
	// Copy re-packages the source video stream unchanged (`-c:v copy`) instead of
	// re-encoding. Set when the client can decode the SOURCE codec within the
	// resolution/bitrate limits (PlanVideo enforces this) — h264 as before, and now
	// any client-supported codec such as HEVC (ADR-0024); otherwise the stream is
	// re-encoded with libx264 to h264.
	Copy bool
	// Codec is the effective OUTPUT video codec: the source codec when Copy is set
	// (e.g. "hevc"), else "h264" (the re-encode target). The caller reads it to pick
	// the delivery container (fMP4 for a copied HEVC MPEG-TS cannot carry for Safari)
	// and to advertise CODECS in the master playlist.
	Codec string
	// MaxHeight, when > 0 and the source is taller, scales the video DOWN to this
	// height (preserving aspect ratio, never upscaling). Ignored when Copy is set.
	MaxHeight int
	// MaxBitrate, when > 0, caps the encoded video bitrate (bits/sec) via -b:v and
	// a matching -maxrate/-bufsize. Ignored when Copy is set.
	MaxBitrate int64
}

// AudioPlan is the resolved audio-track decision.
type AudioPlan struct {
	// Copy re-packages the source audio stream unchanged (`-c:a copy`). Set ONLY
	// when the source is already aac within the client's channel limit; otherwise
	// the stream is re-encoded to aac.
	Copy bool
	// MaxChannels, when > 0 and the source has more channels, downmixes to this
	// many channels (e.g. 5.1 → 2 = stereo). Ignored when Copy is set.
	MaxChannels int
}

// BurnSubtitle names the image Subtitle track to burn into the video frames
// (subtitles/04). Bitmap subtitles (PGS/VOBSUB/DVD) are rasterized onto the frame
// with the `overlay` filter — the portable, core-ffmpeg path that needs no libass
// (the `subtitles`/libass filter is a text-rendering path that many builds omit;
// overlay + the bitmap decoders are always present). TranscodeArgs turns it into a
// `-filter_complex …overlay… -map "[v]"` graph on the CPU re-encode path.
type BurnSubtitle struct {
	// SidecarPath, when non-empty, is a SIDECAR subtitle file (.sup, .idx) added as a
	// SECOND ffmpeg input so its subtitle stream can be overlaid; empty for an
	// EMBEDDED track, which is overlaid from the source container itself (input 0).
	SidecarPath string
	// StreamIndex is the SUBTITLE-relative stream index of an EMBEDDED track within
	// the source container (the Nth subtitle stream, 0-based) — overlaid as
	// `[0:v][0:s:N]overlay`. It is -1 for a sidecar track, whose single subtitle is
	// overlaid from the second input as `[0:v][1:s]overlay`.
	StreamIndex int
}

// isSidecar reports whether the burn target is a sidecar file (a second input)
// rather than an embedded stream of the source container.
func (b BurnSubtitle) isSidecar() bool { return b.SidecarPath != "" }

// overlayGraph builds the -filter_complex value that burns the subtitle onto the
// video: overlay the chosen subtitle stream onto the source video, then (when a
// height cap binds) scale the COMPOSITE down so the sub scales with the picture and
// stays aligned. The subtitle stream is `[0:s:N]` for an embedded track (same
// container as the video) or `[1:s]` for a sidecar (the second input). The output
// is labeled `[v]`, which TranscodeArgs -maps as the video track.
func overlayGraph(b BurnSubtitle, maxHeight int, videoLabel string) string {
	sub := "[1:s]"
	if !b.isSidecar() {
		sub = "[0:s:" + itoa(b.StreamIndex) + "]"
	}
	graph := videoLabel + sub + "overlay"
	if maxHeight > 0 {
		graph += "," + cpuScaleFilter(maxHeight)
	}
	return graph + "[v]"
}

// overlayVideoLabel is the filter-graph input label for the video the burn overlays
// onto: the negotiated Stream (`[0:v:N]`) on a multi-video File, else the bare `[0:v]`
// — ffmpeg's implicit first video — so a single-video burn stays byte-for-byte
// unchanged (selectable-video/01). Note the nil form is `[0:v]`, not `[0:v:0]`: the
// overlay graph and the plain video -map use different existing conventions, each
// preserved.
func overlayVideoLabel(idx *int) string {
	if idx == nil {
		return "[0:v]"
	}
	return "[0:v:" + itoa(*idx) + "]"
}

// TranscodeArgs builds the PURE ffmpeg argument vector for a transcode job — no
// process is spawned. It is the unit-testable command builder for the re-encode
// tier, the sibling of RemuxArgs:
//
//   - -nostdin -y -i <source>: same input handling as remux.
//   - Video: -c:v copy when the plan says the source is already HLS-friendly,
//     else -c:v libx264 (CPU) with -preset, an optional scale-down filter
//     (-vf scale=-2:H, even width via -2, never upscaling), and an optional
//     bitrate cap (-b:v/-maxrate/-bufsize). yuv420p + the baseline-friendly
//     defaults keep the output broadly decodable.
//   - Audio (when present): -c:a copy when already aac within the channel cap,
//     else -c:a aac with an optional -ac downmix.
//   - The shared hlsOutputArgs tail emits the same on-demand HLS playlist +
//     segments as remux, so the slice-1 runtime serves a transcode session
//     identically.
//
// OutputDir must be absolute (the session layer guarantees this).
func TranscodeArgs(job TranscodeJob) []string {
	args := []string{"-nostdin", "-y"}

	// Resolve the video encode backend up front: a HW re-encode contributes
	// device-init / decode-hwaccel flags that MUST precede -i, so they are emitted
	// ahead of the input. The copy and audio-only paths re-encode no video, so they
	// add no init flags and stay backend-independent (byte-for-byte unchanged). A
	// BURN-IN job always re-encodes (you cannot overlay onto a copied stream) and
	// runs the subtitles filter in system memory, so it is pinned to the CPU backend
	// regardless of Accel (subtitles/04) — HW-surface burn-in is a future refinement.
	burning := !job.AudioOnly && job.Burn != nil
	videoOnly := job.VideoOnly && !job.AudioOnly
	reencodeVideo := !job.AudioOnly && (!job.Video.Copy || burning)
	var be backend
	if reencodeVideo {
		if burning {
			be = cpuBackend()
		} else {
			be = videoBackend(job.Accel)
		}
		args = append(args, be.initArgs...)
	}

	// Input seek (realignment) goes BEFORE -i for a fast keyframe-accurate seek of
	// the INPUT, so the encode resumes near the target without decoding from 0;
	// empty for the from-the-top launch.
	args = append(args, job.Seek.inputSeekArgs()...)
	args = append(args, "-i", job.SourcePath)

	// A SIDECAR burn adds the subtitle file as a SECOND input, overlaid via
	// [1:s] (subtitles/04). The same input seek is applied to it so its cues stay
	// aligned with the video after a realignment.
	if burning && job.Burn.isSidecar() {
		args = append(args, job.Seek.inputSeekArgs()...)
		args = append(args, "-i", job.Burn.SidecarPath)
	}

	// --- video ---
	if job.AudioOnly {
		// Audio-only Track: drop video entirely (-vn). The audio branch below emits
		// the AAC encode; the HLS tail packages an audio-only rendition (valid HLS).
		args = append(args, "-vn")
	} else if job.Video.Copy && !burning {
		args = append(args, "-c:v", "copy")
	} else {
		// Assemble the re-encode from the backend descriptor (videoBackend): the
		// -c:v encoder, its preset/pixel-format flags, the segment-boundary keyframe
		// forcing, an optional upload+scale -vf chain in the backend's filter domain,
		// and the shared bitrate cap. The CPU descriptor reproduces the original
		// libx264 flags exactly, so the knob-off path is unchanged; a HW backend
		// (VideoToolbox here) swaps only the encoder + preset and keeps everything
		// else — critically the keyframe forcing — identical.
		args = append(args, "-c:v", be.encoder)
		args = append(args, be.presetArgs...)
		args = append(args,
			// Force a keyframe at every SegmentSeconds boundary so the HLS muxer cuts
			// segments at EXACT, uniform durations. This is what makes seek
			// realignment precise and the server-owned playlist exact: segment index N
			// always begins at N*SegmentSeconds, so restarting at -ss N*SegmentSeconds
			// with -start_number N reproduces exactly segmentN.ts (ADR-0004). It is
			// load-bearing on EVERY backend — a HW encoder that cannot honor it is not
			// acceptable.
			"-force_key_frames", forceKeyFramesExpr,
		)
		// Video filters. A BURN-IN job uses a -filter_complex overlay graph (the
		// subtitle rasterized onto the frame, then scaled with it) whose output MUST be
		// -mapped explicitly as the video track; any scale-down lives inside that graph.
		// A non-burn job uses the backend's -vf chain (optional HW upload + scale-down),
		// empty for an un-capped CPU/VideoToolbox encode so no stray -vf is emitted.
		if burning {
			args = append(args, "-filter_complex", overlayGraph(*job.Burn, job.Video.MaxHeight, overlayVideoLabel(job.VideoStreamIndex)), "-map", "[v]")
		} else if vf := videoFilterChain(be, job.Video.MaxHeight); vf != "" {
			args = append(args, "-vf", vf)
		}
		if job.Video.MaxBitrate > 0 {
			// Bitrate cap as -b:v + -maxrate at the cap, -bufsize at 2x. Every wired
			// backend accepts -b:v (VideoToolbox honors it as the target bitrate); the
			// descriptor could override these per backend if one ever needed to.
			b := itoa64(job.Video.MaxBitrate)
			args = append(args,
				"-b:v", b,
				"-maxrate", b,
				"-bufsize", itoa64(job.Video.MaxBitrate*2),
			)
		}
	}

	// On a multi-audio OR multi-video File the negotiated track is -mapped explicitly
	// (the audio section below maps audio); any -map disables ffmpeg's implicit
	// selection, so the video track must be -mapped too — the chosen video Stream
	// (0:v:N) on a multi-video File, else 0:v:0. The burn path already maps its overlay
	// output ([v]); the audio-only path has no video (-vn). Both indices nil
	// (single-video single-audio) adds nothing, so those args stay byte-for-byte
	// unchanged.
	if !job.AudioOnly && !videoOnly && !burning && (job.AudioStreamIndex != nil || job.VideoStreamIndex != nil) {
		args = append(args, "-map", videoMapSpec(job.VideoStreamIndex))
	}

	// Demuxed video variant (audio-streams/03): drop audio entirely. -an removes the
	// implicitly-selected audio so the variant carries only video; each audio Stream
	// is delivered as its own in-band rendition. It suppresses the audio section below.
	// A pinned video Stream (0:v:N, multi-video File) is mapped alongside -an so the
	// variant carries the negotiated cut; a nil index leaves ffmpeg's implicit first
	// video, byte-for-byte unchanged.
	if videoOnly {
		if job.VideoStreamIndex != nil {
			args = append(args, "-map", videoMapSpec(job.VideoStreamIndex))
		}
		args = append(args, "-an")
	}

	// --- audio ---
	if job.HasAudio && !videoOnly {
		// Map the negotiated audio Stream explicitly so the delivered audio is the one
		// the Decision reports (audio-streams/02): implicit selection would pick the
		// most-channels track, not the chosen one. A burn-in job already runs
		// -filter_complex/-map (implicit selection off), so it maps the source audio
		// unconditionally — the chosen Stream when known, else all source audio (0:a,
		// never the sidecar). A non-burn job adds a -map only when a specific Stream is
		// selected (multi-audio); single-audio keeps ffmpeg's implicit selection.
		if idx := job.AudioStreamIndex; idx != nil {
			args = append(args, "-map", "0:a:"+itoa(*idx))
		} else if burning || job.VideoStreamIndex != nil {
			// The video was mapped explicitly (a burn's [v], or a pinned multi-video
			// Stream), which disables implicit audio selection — so map all source audio
			// (0:a) to keep the co-packaged shared audio track rather than drop it.
			args = append(args, "-map", "0:a")
		}
		if job.Audio.Copy {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", audioEncoderAAC)
			if job.Audio.MaxChannels > 0 {
				args = append(args, "-ac", itoa(job.Audio.MaxChannels))
			}
		}
	}

	args = append(args, job.Seek.outputOffsetArgs()...)
	return append(args, videoHlsOutput(job.OutputDir, job.FMP4, job.SegmentTimes, job.Seek.StartNumber)...)
}

// AudioRenditionJob describes one demuxed in-band audio rendition (audio-streams/03,
// ADR-0022): an audio-only HLS stream carrying exactly ONE of a multi-audio File's
// audio Streams, produced LAZILY on first request and served next to the video
// variant + the other renditions in the shared session scratch dir. It is the audio
// analogue of RemuxJob/TranscodeJob — a pure arg description the Runner executes.
type AudioRenditionJob struct {
	// SourcePath is the absolute path of the multi-audio File.
	SourcePath string
	// OutputDir is the session scratch directory (shared with the video variant and
	// the other renditions); PlaylistName/SegmentPattern namespace this rendition's
	// files within it.
	OutputDir string
	// AudioStreamIndex is the audio-relative index (the Nth audio stream, 0-based) of
	// the Stream this rendition delivers — mapped as `-map 0:a:N`.
	AudioStreamIndex int
	// Copy stream-copies the source audio unchanged (`-c:a copy`) when the client's
	// capability profile accepts the codec AND MPEG-TS can carry it; false re-encodes
	// to AAC (an incompatible codec — DTS, TrueHD, FLAC, …). The caller (the playback
	// layer) makes this decision so the builder stays a pure honor-the-flag assembler.
	Copy bool
	// MaxChannels, when > 0 and re-encoding, downmixes the AAC output to this many
	// channels (the client's channel cap). Ignored when Copy is set.
	MaxChannels int
	// PlaylistName / SegmentPattern are this rendition's output filenames within
	// OutputDir (AudioRenditionPlaylist/AudioRenditionSegmentPattern), so the flat
	// scratch dir holds one distinctly-named playlist + segment set per rendition.
	PlaylistName   string
	SegmentPattern string
	// InitName, when non-empty, delivers the rendition as fragmented-MP4 (.m4s +
	// this init segment) instead of MPEG-TS (ADR-0024): set when the session's video
	// variant is a copied-HEVC fMP4 stream, so every rendition in the master shares
	// the fMP4 container. Empty keeps the rendition MPEG-TS, unchanged. The caller
	// pairs it with the fMP4 SegmentPattern (AudioRenditionSegmentPatternFMP4).
	InitName string
	// Seek is the seek-realignment offset (ADR-0004), carried for symmetry with the
	// video jobs. An audio encode is far faster than realtime, so the whole rendition
	// is produced up front and a seek reads an already-written segment — the runtime
	// does not realign renditions — but the field lets a from-a-position (re)launch
	// number its first segment consistently if ever needed.
	Seek SeekOffset
}

// AudioRenditionArgs builds the PURE ffmpeg argument vector for one demuxed audio
// rendition (audio-streams/03): drop the video (`-vn`), map exactly the chosen audio
// Stream (`-map 0:a:N`), stream-copy it when the client accepts the codec else
// re-encode to AAC (with an optional downmix), and emit the same on-demand HLS VOD
// playlist + mpegts segments as the video path — under this rendition's namespaced
// filenames so it coexists with the video variant in the shared scratch dir. No
// process is spawned; the Runner executes the vector.
func AudioRenditionArgs(job AudioRenditionJob) []string {
	args := []string{"-nostdin", "-y"}
	// Input seek (realignment) goes BEFORE -i; empty for the from-the-top launch.
	args = append(args, job.Seek.inputSeekArgs()...)
	args = append(args, "-i", job.SourcePath)
	// Audio-only: drop video, map exactly the chosen audio Stream.
	args = append(args, "-vn", "-map", "0:a:"+itoa(job.AudioStreamIndex))
	if job.Copy {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", audioEncoderAAC)
		if job.MaxChannels > 0 {
			args = append(args, "-ac", itoa(job.MaxChannels))
		}
	}
	args = append(args, job.Seek.outputOffsetArgs()...)
	return append(args, hlsOutputArgsTyped(job.OutputDir, job.PlaylistName, job.SegmentPattern, job.InitName, job.Seek.StartNumber)...)
}

// PlanVideo decides the video-track plan (ADR-0024): COPY the source stream when
// the client can decode the SOURCE codec and it fits without a scale-down or a
// bitrate reduction; otherwise re-encode to h264, scaled/capped to fit. It returns
// the effective output Codec so the caller can pick the delivery container (fMP4 for
// a copied HEVC) and advertise CODECS. clientSupportsSource is whether the profile
// declares the source codec; copyMaxHeight is the height cap that binds a COPY (the
// source codec's device ceiling ∩ the session cap); reencodeMaxHeight is the cap for
// the h264 RE-ENCODE (the h264 ceiling ∩ the session cap); maxBitrate caps both
// (0 = no cap). When in doubt (client can't take the source, or any cap binds), it
// re-encodes.
func PlanVideo(sourceCodec string, sourceHeight int, sourceBitrate int64, clientSupportsSource bool, copyMaxHeight, reencodeMaxHeight int, maxBitrate int64) VideoPlan {
	needBitrate := maxBitrate > 0 && sourceBitrate > 0 && sourceBitrate > maxBitrate
	// COPY the source stream when the client can decode its codec and it fits without
	// a scale-down or a bitrate reduction (a copy can do neither). This now admits any
	// client-supported codec — h264 rides MPEG-TS as before, a copied HEVC rides fMP4
	// (ADR-0024) — so a 4K HEVC that the client can play is delivered untouched instead
	// of being re-encoded (and downscaled) to h264 in real time, which a self-hosted
	// host cannot sustain.
	copyScale := copyMaxHeight > 0 && sourceHeight > copyMaxHeight
	if clientSupportsSource && !copyScale && !needBitrate {
		return VideoPlan{Copy: true, Codec: normCodec(sourceCodec)}
	}
	// Re-encode to h264, scaled/capped to the h264 ceiling (the re-encode target is
	// always h264 — the universally-playable HLS video codec).
	p := VideoPlan{Codec: VideoCodecH264}
	if reencodeMaxHeight > 0 && sourceHeight > reencodeMaxHeight {
		p.MaxHeight = reencodeMaxHeight
	}
	if needBitrate {
		p.MaxBitrate = maxBitrate
	}
	return p
}

// PlanAudio decides the audio-track plan conservatively: copy the source ONLY
// when it is already aac, the client supports aac, AND no downmix is needed;
// otherwise re-encode to aac (downmixing when the source exceeds the channel
// cap). maxChannels is the client's audio-channel cap (0 = no cap).
func PlanAudio(sourceCodec string, sourceChannels int, clientSupportsAAC bool, maxChannels int) AudioPlan {
	needDownmix := maxChannels > 0 && sourceChannels > 0 && sourceChannels > maxChannels
	if normCodec(sourceCodec) == AudioCodecAAC && clientSupportsAAC && !needDownmix {
		return AudioPlan{Copy: true}
	}
	p := AudioPlan{}
	if needDownmix {
		p.MaxChannels = maxChannels
	}
	return p
}

// normCodec lowercases/trims a codec name for the copy-vs-encode comparison
// (ffprobe reports e.g. "h264", "aac" already, but be defensive about casing).
func normCodec(c string) string {
	out := make([]byte, 0, len(c))
	for i := 0; i < len(c); i++ {
		ch := c[i]
		if ch == ' ' || ch == '\t' {
			continue
		}
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		out = append(out, ch)
	}
	return string(out)
}

// Runner runs an ffmpeg job to completion (or until its context is cancelled).
// It is the seam: the real FFmpeg shells out to the binary; unit tests fake it.
// Start launches the process and returns a handle the caller can wait on or
// kill — the session owns that handle so DELETE/reaping can terminate it.
type Runner interface {
	// Start launches ffmpeg with the given args (from RemuxArgs) and returns a
	// running Job. The returned Job's process is already started; Wait blocks for
	// completion and Kill terminates it. ctx cancellation also terminates it.
	Start(ctx context.Context, args []string) (Job, error)
}

// Job is one running ffmpeg process. Wait blocks until it exits (returning its
// error, nil on a clean exit or a context-cancellation/kill — a remux that is
// killed once its segments are produced is not a failure). Kill terminates it
// promptly and is idempotent.
type Job interface {
	Wait() error
	Kill() error
}

// FFmpeg is the production Runner: it invokes the `ffmpeg` binary on PATH.
// Binary names the executable so a test or deployment can point at an absolute
// path; empty defaults to "ffmpeg". This mirrors scanner.FFprobe.
type FFmpeg struct {
	Binary string
}

// Start launches the ffmpeg process bound to ctx and returns the running Job. It
// captures the process's stderr (the tail) so a FAILED job can report WHY it died —
// without it a broken/slow source or an unsupported copy silently produced no
// output and the HLS request just 404'd with no diagnostic.
func (f FFmpeg) Start(ctx context.Context, args []string) (Job, error) {
	bin := f.Binary
	if bin == "" {
		bin = "ffmpeg"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	j := &ffmpegJob{cmd: cmd}
	cmd.Stderr = &j.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transcode: starting ffmpeg: %w", err)
	}
	return j, nil
}

// ffmpegJob wraps a started exec.Cmd as a Job. It records whether it was
// deliberately Killed (a teardown/realign, which is expected and not a failure) and
// captures the stderr tail so an UNEXPECTED exit can be diagnosed.
type ffmpegJob struct {
	cmd    *exec.Cmd
	stderr tailBuffer
	mu     sync.Mutex
	killed bool
}

func (j *ffmpegJob) Wait() error {
	err := j.cmd.Wait()
	j.mu.Lock()
	killed := j.killed
	j.mu.Unlock()
	// A deliberate Kill (teardown / seek realignment) exits non-zero but is expected,
	// so it is NOT a failure — report nil so callers (the launch probe, the runtime's
	// exit watcher) do not treat it as a crash.
	if killed || err == nil {
		return nil
	}
	// A genuine failure: surface the ffmpeg stderr tail so the operator sees the cause
	// (a source the copy can't handle, a slow/unreadable mount, a codec issue) instead
	// of a silent 404.
	if tail := strings.TrimSpace(j.stderr.String()); tail != "" {
		return fmt.Errorf("transcode: ffmpeg exited: %w\nffmpeg stderr (tail):\n%s", err, tail)
	}
	return fmt.Errorf("transcode: ffmpeg exited: %w", err)
}

func (j *ffmpegJob) Kill() error {
	j.mu.Lock()
	j.killed = true
	j.mu.Unlock()
	if j.cmd.Process == nil {
		return nil
	}
	// Killing an already-exited process returns an error we can ignore — Kill is
	// idempotent from the caller's perspective.
	_ = j.cmd.Process.Kill()
	return nil
}

// tailBuffer is a bounded io.Writer that retains only the LAST tailBufferMax bytes
// written — enough of ffmpeg's stderr to diagnose a failure without unbounded memory
// for a chatty (e.g. -loglevel debug) run. Safe for concurrent Write/String.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailBufferMax = 8192

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBufferMax {
		t.buf = t.buf[len(t.buf)-tailBufferMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// join concatenates a directory and a name with a single separator, kept local
// so RemuxArgs stays a pure, import-light builder (avoids pulling filepath into
// the hot arg path; the caller passes an absolute OutputDir).
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	if dir[len(dir)-1] == '/' {
		return dir + name
	}
	return dir + "/" + name
}

// itoa is a tiny int→string for the arg builder (avoids importing strconv just
// for the segment-duration flag).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// itoa64 is itoa for int64 bitrate flags (always non-negative here).
func itoa64(n int64) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
