package playback

import (
	"strconv"
	"strings"

	"github.com/marioquake/juicebox/internal/audio"
	"github.com/marioquake/juicebox/internal/store"
	"github.com/marioquake/juicebox/internal/subtitle"
)

// Tier is the chosen playback tier (ADR-0003). Negotiation picks the cheapest
// tier that works: directPlay (bytes unchanged) → directStream (remux, no
// re-encode) → transcode (re-encode). All three are now delivered: a transcode
// outcome yields an HLS session whose ffmpeg job re-encodes to fit the client
// (slice 2). Only a structural impossibility (no video stream / no present file)
// remains an honest error.
type Tier string

const (
	// TierDirectPlay streams the File's bytes unchanged (CONTEXT.md "Direct play").
	TierDirectPlay Tier = "directPlay"
	// TierDirectStream remuxes the File on the fly (`-c copy`) into HLS — the
	// codecs are fine but the container is not (CONTEXT.md "Direct stream").
	TierDirectStream Tier = "directStream"
	// TierTranscode re-encodes video and/or audio to fit the client. Chosen when a
	// codec/bitrate/resolution/channel reason blocks both direct play and remux;
	// delivered as HLS by a re-encoding ffmpeg job (slice 2).
	TierTranscode Tier = "transcode"
)

// TierForReason classifies a direct-play failure into the fallback tier that
// could satisfy it (ADR-0003):
//   - container alone → directStream (remux): the codecs decode, only the
//     wrapping container is wrong, so repackaging without re-encoding suffices.
//   - any codec/bitrate/resolution/channel reason → transcode: the bytes
//     themselves must change.
//   - a structural impossibility (noVideo) or "nothing to play" (noFile) is not a
//     tier — it stays an honest error, classified as transcode so callers that
//     switch on the tier never mistake it for a remux.
//
// SelectEdition uses negotiateRemux to PROVE a container-only mismatch (so it
// only ever returns a directStream Decision when a remux genuinely suffices);
// this helper exposes the same classification for callers (and tests) that hold
// a Reason directly.
func TierForReason(r Reason) Tier {
	if r == ReasonContainer {
		return TierDirectStream
	}
	return TierTranscode
}

// Decision is the negotiation outcome for one playable File: the tier, the
// chosen Edition/File and the selected video+audio Streams, plus the estimated
// bitrate the stream will sustain. The api layer adds the sessionId and the
// session-scoped streamUrl (which it owns) on top of this.
type Decision struct {
	Tier        Tier
	Edition     store.Edition
	File        store.File
	VideoStream store.Stream
	AudioStream store.Stream
	// Subtitles is every selectable Subtitle track the played File offers, from all
	// sources in one list (ADR-0020): the File's embedded subtitle Streams plus the
	// Title's Sidecar/Fetched tracks. The api layer maps it to the decision's
	// `subtitles:[...]`, attaching the out-of-band delivery URL for text tracks. It
	// is populated by the Service after edition selection (SelectEdition leaves it
	// nil); the pure negotiation helpers don't touch it.
	Subtitles []SubtitleTrack
	// AudioOnly is true for a Music Track / any audio-only File (no video Stream):
	// negotiation skips the video gates entirely, and the transcode plan emits an
	// audio-only encode (FLAC/ALAC → AAC), reusing the same tier machinery. The
	// VideoStream is the zero value in that case (issue tv-music/03, additive).
	AudioOnly bool
	// Burn, when non-nil, is the image Subtitle track to burn into the video frames
	// (ADR-0020, subtitles/04). It is only ever set on a TierTranscode Decision (a
	// burn-in ESCALATES to transcode — you cannot overlay onto direct-play bytes or a
	// remux copy), and only when the request carried a burnSubtitleId that resolved
	// to an image track of the played File. The Service sets it after forcing the
	// transcode tier; the args builder (transcodeJobPlan) turns it into the ffmpeg
	// -vf subtitles= burn filter. Nil for every non-burn Decision.
	Burn *BurnSubtitle
	// EstimatedBitrate is the bits/sec the client should expect. For direct play
	// it is simply the File's own bitrate (we send the bytes unchanged).
	EstimatedBitrate int64
	// VideoCopy marks a TierTranscode Decision that STREAM-COPIES the video and
	// transcodes only the audio (ADR-0024): the client can decode the source video
	// codec at its resolution (e.g. a 4K HEVC on a HEVC-capable Safari), so the video
	// rides untouched and only the audio the client can't decode is re-encoded. It
	// makes the session behave like remux — an ffmpeg-owned playlist (a copied stream
	// has no forced-keyframe grid) and UNMETERED governance (no video encode) — and,
	// for a non-h264 copied codec, selects fMP4 delivery (UsesFMP4). Only ever set on a
	// video-bearing, non-burn transcode; false for direct play, remux, a re-encode, and
	// a burn (which must re-encode the video).
	VideoCopy bool
	// HevcInMpegTS mirrors DeviceProfile.HevcInMpegTS onto the Decision (the Service
	// stamps it where it holds the profile), so UsesFMP4 can route a copied HEVC to
	// the client's preferred HLS segment container: MPEG-TS for an hls.js client
	// (exact dictated-cut playlists), fMP4 only for the native Apple player that
	// requires it.
	HevcInMpegTS bool
}

// UsesFMP4 reports whether the HLS session is delivered as fragmented-MP4 (.m4s +
// an init segment) rather than MPEG-TS (ADR-0024). fMP4 is required when a COPIED
// video codec is one the CLIENT cannot take in MPEG-TS — HEVC on Apple's native
// player: a remux (directStream) always copies the video, and a VideoCopy transcode
// copies it too. An hls.js client (HevcInMpegTS) takes copied HEVC over the MPEG-TS
// pipeline instead — its dictated cuts make the synthesized playlist exact, which
// strict MSE playback needs. Direct play (no HLS), an audio-only File, an h264
// video, and any re-encode stay MPEG-TS.
func (d Decision) UsesFMP4() bool {
	if d.Tier == TierDirectPlay || d.AudioOnly || d.HevcInMpegTS {
		return false
	}
	copied := d.Tier == TierDirectStream || d.VideoCopy
	return copied && needsFMP4Codec(firstNonEmpty(d.VideoStream.Codec, d.File.VideoCodec))
}

// needsFMP4Codec reports whether a COPIED video codec requires fMP4 delivery instead
// of MPEG-TS (ADR-0024). Only HEVC does: Safari plays HEVC over HLS in fMP4/CMAF, not
// in MPEG-TS. Every other codec we copy — h264, mpeg4, and the like — rides MPEG-TS as
// before, so the check is a positive allowlist (a codec we don't recognize stays on the
// unchanged TS path rather than being pushed onto the newer fMP4 surface).
func needsFMP4Codec(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "hevc", "h265":
		return true
	default:
		return false
	}
}

// Negotiate decides whether the client described by (profile, constraints) can
// direct-play the given File as-is. The merge order matches the contract: the
// static device profile gates codecs/container, then the dynamic constraints
// (network + quality cap) layer their bitrate/resolution caps on top.
//
// A File direct-plays when ALL of these hold:
//   - the device can demux the File's container;
//   - the device can decode the video codec, within both that codec's per-codec
//     resolution ceiling and the session's maxResolution;
//   - the device can decode the audio codec (the chosen audio Stream's codec);
//   - the File's bitrate is within constraints.maxBitrate.
//
// On success it returns a directPlay Decision with the selected Streams; on any
// failure it returns a *Unsupported describing the first blocking reason, which
// the api layer renders as the structured TRANSCODE_REQUIRED error. A File with
// no video Stream (e.g. an audio-only container) is unsupported for this Movie
// slice.
func Negotiate(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File) (Decision, *Unsupported) {
	if !profile.supportsContainer(f.Container) {
		return Decision{}, &Unsupported{
			Reason: ReasonContainer,
			Detail: "container " + NormalizeContainer(f.Container) + " not in device profile",
		}
	}
	return negotiateStreams(profile, constraints, ed, f, TierDirectPlay)
}

// negotiateRemux decides whether the File qualifies for the directStream (remux)
// tier: the container is unsupported, but EVERY codec/bitrate/resolution/channel
// check the direct-play path runs would otherwise pass. It deliberately skips the
// container gate (a remux fixes exactly that) and runs the identical downstream
// checks, so "container is the ONLY reason" is decided by construction rather
// than by inspecting the first reason. On success it returns a directStream
// Decision; if any non-container reason also blocks, it returns that reason so
// the caller classifies the File as transcode instead.
func negotiateRemux(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File) (Decision, *Unsupported) {
	return negotiateStreams(profile, constraints, ed, f, TierDirectStream)
}

// negotiateTranscode builds a transcode Decision for a File that neither direct-
// plays nor remuxes. The transcode tier can fix ANY codec/container/bitrate/
// resolution/channel mismatch by re-encoding, so it runs none of those gates — it
// only requires a video Stream to exist (a structural impossibility a transcode
// cannot conjure). It picks the same video+audio Streams as the other paths so
// the encode plan and the decision response describe the right tracks, and sets
// EstimatedBitrate to the lower of the File's bitrate and any constraint cap
// (what the re-encoded rendition will sustain).
func negotiateTranscode(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File) (Decision, *Unsupported) {
	est := f.Bitrate
	if constraints.MaxBitrate > 0 && (est == 0 || est > constraints.MaxBitrate) {
		est = constraints.MaxBitrate
	}

	video, ok := defaultVideoStream(profile, constraints, f)
	if !ok {
		// Audio-only File (a Music Track): a transcode re-encodes the audio (e.g.
		// FLAC/ALAC → AAC) with no video — reuse the same tier, AudioOnly set. A File
		// with no audio either is genuinely unplayable.
		audio, audioOK := pickAudioStream(f, constraints.PreferredAudioLang)
		if !audioOK && f.AudioCodec == "" {
			return Decision{}, &Unsupported{Reason: ReasonNoVideo, Detail: "file has no video or audio stream"}
		}
		return Decision{
			Tier:             TierTranscode,
			Edition:          ed,
			File:             f,
			AudioStream:      audio,
			AudioOnly:        true,
			EstimatedBitrate: est,
		}, nil
	}
	audio, _ := pickAudioStream(f, constraints.PreferredAudioLang)

	return Decision{
		Tier:             TierTranscode,
		Edition:          ed,
		File:             f,
		VideoStream:      video,
		AudioStream:      audio,
		EstimatedBitrate: est,
	}, nil
}

// negotiateStreams runs the codec/resolution/bitrate/channel checks shared by the
// direct-play and remux paths (the container gate is the caller's concern) and
// builds a Decision at the given tier. Factored out so the remux path can reuse
// the exact direct-play checks, guaranteeing a directStream decision only when
// the container is the sole mismatch.
func negotiateStreams(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File, tier Tier) (Decision, *Unsupported) {
	video, ok := defaultVideoStream(profile, constraints, f)
	if !ok {
		// Audio-only File (a Music Track): there is no video to gate on, so this is
		// NOT an unplayable error — negotiate on the container + audio codec alone,
		// reusing the same direct-play/remux tier classification (issue tv-music/03).
		return negotiateAudioStreams(profile, constraints, ed, f, tier)
	}

	// Gate on the CHOSEN video Stream's own codec/dimensions, falling back to the
	// File-level attributes when a Stream value is missing. On a multi-video File the
	// capability-then-quality default may be a co-packaged Stream that is NOT the
	// File's primary (e.g. a 1080p H.264 chosen over an undecodable 4K HEVC), so the
	// gate must judge the Stream actually being delivered — not the File-level codec,
	// which describes the primary (selectable-video/01). For a single-video File the
	// Stream and File attributes agree, so this is byte-for-byte the prior behavior.
	videoCodec := firstNonEmpty(video.Codec, f.VideoCodec)
	support, ok := profile.videoCodec(videoCodec)
	if !ok {
		return Decision{}, &Unsupported{
			Reason: ReasonVideoCodec,
			Detail: "video codec " + videoCodec + " not in device profile",
		}
	}

	height := video.Height
	if height == 0 {
		height = f.Height
	}
	// Per-codec ceiling from the device profile (e.g. h264 capped at 1080p).
	if cap := resolutionHeight(support.MaxResolution); cap > 0 && height > cap {
		return Decision{}, &Unsupported{
			Reason: ReasonResolution,
			Detail: "video height " + itoa(height) + " exceeds device " + videoCodec + " ceiling " + support.MaxResolution,
		}
	}
	// Session resolution cap (the per-request network/quality limit).
	if cap := resolutionHeight(constraints.MaxResolution); cap > 0 && height > cap {
		return Decision{}, &Unsupported{
			Reason: ReasonResolution,
			Detail: "video height " + itoa(height) + " exceeds constraint maxResolution " + constraints.MaxResolution,
		}
	}

	audio, audioOK := pickAudioStream(f, constraints.PreferredAudioLang)
	// The audio codec to check: the chosen Stream's codec when present, else the
	// File-level audioCodec. A File with no audio Stream still direct-plays if the
	// (silent) video is otherwise supported — but if there IS audio it must be
	// decodable.
	if audioOK {
		audioCodec := firstNonEmpty(audio.Codec, f.AudioCodec)
		if audioCodec != "" && !profile.supportsAudio(audioCodec) {
			return Decision{}, &Unsupported{
				Reason: ReasonAudioCodec,
				Detail: "audio codec " + audioCodec + " not in device profile",
			}
		}
		if support := profile.MaxAudioChannels; support > 0 && audio.Channels > support {
			return Decision{}, &Unsupported{
				Reason: ReasonAudioChannels,
				Detail: "audio channels exceed device maxAudioChannels",
			}
		}
	} else if f.AudioCodec != "" && !profile.supportsAudio(f.AudioCodec) {
		return Decision{}, &Unsupported{
			Reason: ReasonAudioCodec,
			Detail: "audio codec " + f.AudioCodec + " not in device profile",
		}
	}

	// Remux (directStream) copies streams verbatim into the HLS segment container
	// (MPEG-TS), so a remux is valid ONLY when the audio codec is one MPEG-TS can
	// actually carry. A codec the client decodes but MPEG-TS cannot hold (FLAC,
	// Opus, Vorbis, ALAC, …) muxes as an unrecognized private data stream that no
	// player can decode (the silent-music bug), so it must transcode instead. Direct
	// play streams the original bytes (no MPEG-TS) and never runs this gate.
	if tier == TierDirectStream {
		if ac := firstNonEmpty(audio.Codec, f.AudioCodec); ac != "" && !hlsRemuxableAudio(ac) {
			return Decision{}, &Unsupported{
				Reason: ReasonAudioCodec,
				Detail: "audio codec " + ac + " cannot be remuxed into HLS (MPEG-TS); a transcode is required",
			}
		}
	}

	if constraints.MaxBitrate > 0 && f.Bitrate > 0 && f.Bitrate > constraints.MaxBitrate {
		return Decision{}, &Unsupported{
			Reason: ReasonBitrate,
			Detail: "file bitrate exceeds constraint maxBitrate",
		}
	}

	return Decision{
		Tier:             tier,
		Edition:          ed,
		File:             f,
		VideoStream:      video,
		AudioStream:      audio,
		EstimatedBitrate: f.Bitrate,
	}, nil
}

// negotiateAudioStreams runs the codec/bitrate checks for an AUDIO-ONLY File (a
// Music Track) at the given tier, mirroring negotiateStreams without the video
// gates. A File with neither a video nor an audio Stream is genuinely unplayable
// (ReasonNoVideo, the structural-impossibility sentinel SelectEdition keys on);
// otherwise the audio codec must be decodable (direct play / remux) and within
// the channel cap, and the bitrate within the constraint. The transcode path
// (negotiateTranscode) handles a codec the client lacks by re-encoding to AAC.
func negotiateAudioStreams(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File, tier Tier) (Decision, *Unsupported) {
	audio, audioOK := pickAudioStream(f, constraints.PreferredAudioLang)
	if !audioOK && f.AudioCodec == "" {
		// No video AND no audio — nothing to play; reuse the structural sentinel so
		// SelectEdition treats it as unplayable (not a transcode hint).
		return Decision{}, &Unsupported{Reason: ReasonNoVideo, Detail: "file has no video or audio stream"}
	}

	audioCodec := firstNonEmpty(audio.Codec, f.AudioCodec)
	if audioCodec != "" && !profile.supportsAudio(audioCodec) {
		return Decision{}, &Unsupported{
			Reason: ReasonAudioCodec,
			Detail: "audio codec " + audioCodec + " not in device profile",
		}
	}
	if support := profile.MaxAudioChannels; support > 0 && audio.Channels > support {
		return Decision{}, &Unsupported{
			Reason: ReasonAudioChannels,
			Detail: "audio channels exceed device maxAudioChannels",
		}
	}
	// A remux (directStream) of a Music Track copies the audio verbatim into the HLS
	// MPEG-TS container; only a codec MPEG-TS can carry is remux-able. FLAC/Opus/
	// Vorbis/ALAC etc. would mux as an unplayable private data stream (the silent-
	// music bug), so force them to transcode (FLAC→AAC) even though the client can
	// decode them. Direct play (raw bytes) is unaffected — only directStream gates.
	if tier == TierDirectStream && !hlsRemuxableAudio(audioCodec) {
		return Decision{}, &Unsupported{
			Reason: ReasonAudioCodec,
			Detail: "audio codec " + audioCodec + " cannot be remuxed into HLS (MPEG-TS); a transcode is required",
		}
	}
	if constraints.MaxBitrate > 0 && f.Bitrate > 0 && f.Bitrate > constraints.MaxBitrate {
		return Decision{}, &Unsupported{
			Reason: ReasonBitrate,
			Detail: "file bitrate exceeds constraint maxBitrate",
		}
	}

	return Decision{
		Tier:             tier,
		Edition:          ed,
		File:             f,
		AudioStream:      audio,
		AudioOnly:        true,
		EstimatedBitrate: f.Bitrate,
	}, nil
}

// SelectEdition chooses the Edition+File to play from a Title's editions, given
// the constraints and an optional explicit editionId.
//
// Selection rule (cheapest tier that works, ADR-0003):
//   - If editionId is given, only that Edition is considered (no silent fallback
//     to an Edition the user did not ask for).
//   - The server prefers the highest-quality DIRECT-PLAY Edition (most pixels,
//     then highest bitrate). When none direct-plays, it falls back to the best
//     DIRECT-STREAM (remux) Edition — one whose ONLY direct-play blocker is its
//     container. Direct play always wins over remux.
//   - If no Edition direct-plays or remuxes, it returns the first blocking
//     Unsupported reason; the api layer classifies it (a codec/bitrate/resolution
//     reason → transcode, surfaced as TRANSCODE_REQUIRED this slice).
//
// Missing Files (soft-deleted, ADR-0008) are skipped: an Edition whose File is
// absent from disk cannot be streamed. Multi-part Editions are out of scope this
// slice, so only the first present File of an Edition is considered.
func SelectEdition(profile DeviceProfile, constraints Constraints, editions []store.Edition, editionID string) (Decision, *Unsupported) {
	var firstReason *Unsupported
	var best *Decision          // best direct-play Decision
	var bestRemux *Decision     // best directStream (remux) fallback
	var bestTranscode *Decision // best transcode fallback (re-encode fits any mismatch)

	for _, ed := range editions {
		if editionID != "" && ed.ID != editionID {
			continue
		}
		f, ok := firstPresentFile(ed)
		if !ok {
			if firstReason == nil {
				firstReason = &Unsupported{Reason: ReasonNoFile, Detail: "edition has no present file"}
			}
			continue
		}
		dec, unsup := Negotiate(profile, constraints, ed, f)
		if unsup == nil {
			// Keep the highest-resolution direct-play Edition (bitrate breaks ties).
			if best == nil || better(dec, *best) {
				d := dec
				best = &d
			}
			continue
		}
		// The container is the thing the direct-play gate rejected first — see if a
		// remux would otherwise play it. negotiateRemux re-runs the identical
		// codec/bitrate/resolution checks (skipping only the container), so it
		// returns a directStream Decision exactly when the container is the SOLE
		// mismatch. When it ALSO fails, its reason is the real blocker (a codec/
		// bitrate/resolution issue that classifies as transcode) — surface THAT,
		// not the misleading "container", so a client/tier sees the honest cause.
		if unsup.Reason == ReasonContainer {
			rdec, rUnsup := negotiateRemux(profile, constraints, ed, f)
			if rUnsup == nil {
				if bestRemux == nil || better(rdec, *bestRemux) {
					d := rdec
					bestRemux = &d
				}
				continue
			}
			unsup = rUnsup // the deeper, transcode-classified reason
		}
		// Neither direct-play nor remux: a transcode can re-encode to fit ANY
		// codec/bitrate/resolution/channel mismatch — so fall back to it unless the
		// failure is structural (no video / no file), which a transcode cannot fix.
		if tier := TierForReason(unsup.Reason); tier == TierTranscode && unsup.Reason != ReasonNoVideo && unsup.Reason != ReasonNoFile {
			tdec, tUnsup := negotiateTranscode(profile, constraints, ed, f)
			if tUnsup == nil {
				if bestTranscode == nil || better(tdec, *bestTranscode) {
					d := tdec
					bestTranscode = &d
				}
				continue
			}
			unsup = tUnsup
		}
		if firstReason == nil {
			firstReason = unsup
		}
	}

	if best != nil {
		return *best, nil
	}
	if bestRemux != nil {
		return *bestRemux, nil
	}
	if bestTranscode != nil {
		return *bestTranscode, nil
	}
	if editionID != "" && firstReason == nil {
		// The requested editionId did not match any Edition of this Title.
		return Decision{}, &Unsupported{Reason: ReasonNoFile, Detail: "edition not found on title"}
	}
	if firstReason == nil {
		firstReason = &Unsupported{Reason: ReasonNoFile, Detail: "title has no playable file"}
	}
	return Decision{}, firstReason
}

// better reports whether candidate a is a higher-quality direct-play choice than
// b: more pixels first, then higher bitrate.
func better(a, b Decision) bool {
	ah, bh := a.File.Height, b.File.Height
	if ah != bh {
		return ah > bh
	}
	return a.File.Bitrate > b.File.Bitrate
}

// firstPresentFile returns the first on-disk File of an Edition (Missing Files
// are skipped — they cannot be streamed).
func firstPresentFile(ed store.Edition) (store.File, bool) {
	for _, f := range ed.Files {
		if f.Present {
			return f, true
		}
	}
	return store.File{}, false
}

// pickVideoStream returns the File's real video Stream — the default one if
// flagged, else the first. EMBEDDED COVER ART is skipped: ffprobe reports an
// attached picture (the album/track artwork baked into an MP3/FLAC/M4A, very
// common) as a "video" Stream whose codec is a still-image codec. It is not
// playable video, so a Music Track that carries cover art must NOT be mistaken for
// a video File — otherwise it skips the audio-only transcode path and the
// transcoder tries to encode a single still as an h264 track, producing a broken
// HLS the player hangs on (the cover-art-MP3 playback bug). With the cover art
// skipped the File correctly negotiates as audio-only (FLAC/MP3 → AAC).
func pickVideoStream(f store.File) (store.Stream, bool) {
	var first *store.Stream
	for i := range f.Streams {
		if f.Streams[i].Kind != "video" || isCoverArtStream(f.Streams[i]) {
			continue
		}
		if f.Streams[i].IsDefault {
			return f.Streams[i], true
		}
		if first == nil {
			first = &f.Streams[i]
		}
	}
	if first != nil {
		return *first, true
	}
	return store.Stream{}, false
}

// isCoverArtStream reports whether a "video" Stream is actually an embedded still
// image (attached cover art / thumbnail) rather than real playable video. ffprobe
// flags such a stream with disposition attached_pic=1; that flag is not persisted,
// but its codec is always a still-image codec, which is an equally reliable proxy
// and — unlike a new stored field — needs no rescan of an existing library. No
// real movie/episode/track video uses these codecs, so skipping them is safe.
func isCoverArtStream(s store.Stream) bool {
	switch strings.ToLower(strings.TrimSpace(s.Codec)) {
	case "png", "mjpeg", "mjpg", "jpeg", "jpg", "gif", "bmp", "webp", "tiff", "ppm":
		return true
	default:
		return false
	}
}

// SelectableVideoStreams returns a File's client-facing video Streams — every
// non-cover-art video Stream, in container order (ADR-0025, selectable-video/01). It
// is the set the player's Video menu is built from and the set defaultVideoStream
// ranks; embedded cover-art/thumbnail Streams (isCoverArtStream) are excluded so a
// music file's album art never masquerades as an alternate video. A single-video
// File yields a one-element slice; an audio-only File yields an empty one. Always
// non-nil. Exported so the api layer projects the same set the negotiation ranks.
func SelectableVideoStreams(f store.File) []store.Stream {
	out := make([]store.Stream, 0, len(f.Streams))
	for i := range f.Streams {
		if f.Streams[i].Kind == "video" && !isCoverArtStream(f.Streams[i]) {
			out = append(out, f.Streams[i])
		}
	}
	return out
}

// videoStreamDecodable reports whether the client described by (profile, constraints)
// can DECODE this video Stream as-is: its codec is in the device profile, and its
// height is within both that codec's per-codec ceiling and the session resolution
// cap. It is the per-Stream "cheapest playable tier" proxy defaultVideoStream ranks
// on — a decodable Stream direct-plays/-streams (the cheap tiers), an undecodable one
// forces a transcode (the expensive tier). Container and bitrate are File-level
// (shared by every video Stream of the File), so they never DIFFERENTIATE the set and
// are left to the tier machinery, not this per-Stream ranking.
func videoStreamDecodable(profile DeviceProfile, constraints Constraints, s store.Stream) bool {
	support, ok := profile.videoCodec(s.Codec)
	if !ok {
		return false
	}
	if cap := resolutionHeight(support.MaxResolution); cap > 0 && s.Height > cap {
		return false
	}
	if cap := resolutionHeight(constraints.MaxResolution); cap > 0 && s.Height > cap {
		return false
	}
	return true
}

// defaultVideoStream picks a File's default video Stream from its selectable set by
// the ADR-0025 ranking — the same shape as the Edition heuristic: cheapest playable
// tier first (a Stream the client can decode outranks one it cannot), then most
// pixels, then — since per-Stream bitrate is not persisted (no streams-table change)
// — the container is_default disposition breaks the tie. So a client that cannot
// decode a co-packaged 4K HEVC Stream defaults to a 1080p H.264 Stream it can play
// directly, chosen once at negotiation (no ABR); a fully-capable client gets the
// highest-resolution Stream. ok=false for an audio-only File (no non-cover-art video
// Stream), exactly like the pre-selectable pickVideoStream it replaces in the gates.
func defaultVideoStream(profile DeviceProfile, constraints Constraints, f store.File) (store.Stream, bool) {
	var best *store.Stream
	var bestDecodable bool
	for i := range f.Streams {
		s := &f.Streams[i]
		if s.Kind != "video" || isCoverArtStream(*s) {
			continue
		}
		d := videoStreamDecodable(profile, constraints, *s)
		if best == nil || videoStreamBetter(*s, d, *best, bestDecodable) {
			best = s
			bestDecodable = d
		}
	}
	if best == nil {
		return store.Stream{}, false
	}
	return *best, true
}

// videoStreamBetter reports whether video Stream a is a better DEFAULT pick than the
// current best b, given each Stream's decodability: a decodable Stream (a cheaper
// tier) beats an undecodable one; among equally-decodable Streams the taller wins
// (most pixels — height is the codebase's resolution proxy, as in the Edition
// better()); a remaining tie goes to the container is_default disposition. Total and
// deterministic so the scan order never changes the pick.
func videoStreamBetter(a store.Stream, aDecodable bool, b store.Stream, bDecodable bool) bool {
	if aDecodable != bDecodable {
		return aDecodable
	}
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	if a.IsDefault != b.IsDefault {
		return a.IsDefault
	}
	return false
}

// videoRelIndex returns the video-relative index of the Stream with id streamID
// within the File (the Nth video Stream, 0-based — the `-map 0:v:N` selector) and the
// File's total video-Stream count. found is false when the id names no video Stream.
// It counts ALL video Streams, INCLUDING any embedded cover art, because ffmpeg's
// 0:v:N ordinal does too — the map index must match ffmpeg's numbering, not the
// (cover-art-filtered) selectable set. It mirrors audioRelIndex.
func videoRelIndex(f store.File, streamID string) (idx, total int, found bool) {
	idx = -1
	for _, s := range f.Streams {
		if s.Kind != "video" {
			continue
		}
		if streamID != "" && s.ID == streamID {
			idx = total
			found = true
		}
		total++
	}
	return idx, total, found
}

// videoMapIndex returns the `-map 0:v:N` selector for the negotiated video Stream, or
// nil when no explicit map should be emitted. It returns nil for a single-video File
// (total <= 1) so the remux/transcode args stay byte-for-byte unchanged, and nil when
// the Stream can't be located — in both cases ffmpeg's implicit video selection (the
// only/first video Stream) is left in place. The video parallel of audioMapIndex: the
// one place the "map only when it matters" rule lives for video.
func videoMapIndex(f store.File, chosen store.Stream) *int {
	idx, total, found := videoRelIndex(f, chosen.ID)
	if !found || total <= 1 {
		return nil
	}
	return &idx
}

// resolveVideoStream finds the video Stream identified by streamID within the
// Title (the video parallel of resolveAudioStream), returning the owning
// Edition+File so an explicit videoStreamId plays exactly the File that carries the
// Stream. Embedded cover-art/thumbnail Streams are excluded — they are never in the
// client-facing selectable set, so a cover-art id is treated as no match. ok=false
// when streamID matches no non-cover-art video Stream of the Title — an unknown id,
// or an id belonging to an audio/subtitle Stream — which the caller surfaces as a
// structured error rather than a silent default.
func resolveVideoStream(detail store.TitleDetail, streamID string) (store.Edition, store.File, store.Stream, bool) {
	if streamID == "" {
		return store.Edition{}, store.File{}, store.Stream{}, false
	}
	for _, ed := range detail.Editions {
		for _, f := range ed.Files {
			for _, s := range f.Streams {
				if s.Kind == "video" && !isCoverArtStream(s) && s.ID == streamID {
					return ed, f, s, true
				}
			}
		}
	}
	return store.Edition{}, store.File{}, store.Stream{}, false
}

// videoSelectionFloor returns the minimum tier that can DELIVER the explicitly chosen
// video Stream of File f — the value the caller combines (maxTier) with the tier
// already resolved for the audio/subtitle picks. It mirrors audioSelectionTier from
// the video side (ADR-0025):
//
//   - The default Stream imposes no floor: direct play carries the default video, so
//     selecting it forces nothing (TierDirectPlay).
//   - A Stream the client cannot decode must be re-encoded → transcode.
//   - A decodable NON-DEFAULT Stream can be stream-COPIED, but direct play carries only
//     the default video, so it escalates at least to remux — and only when a remux is
//     actually viable (the File's audio/container otherwise pass the shared gates);
//     when even the default can't remux (e.g. an audio codec MPEG-TS can't carry) the
//     floor is transcode.
//
// The container/audio verdict reuses Negotiate/negotiateRemux (which evaluate the
// File's DEFAULT video+audio); that is exact whenever the default is at least as
// deliverable as the chosen Stream — true for the common co-packaged case — and at
// worst over-escalates to transcode, which still delivers the chosen Stream correctly.
func videoSelectionFloor(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File, chosen, def store.Stream) Tier {
	if chosen.ID == def.ID {
		return TierDirectPlay
	}
	if !videoStreamDecodable(profile, constraints, chosen) {
		return TierTranscode
	}
	if _, unsup := Negotiate(profile, constraints, ed, f); unsup == nil {
		return TierDirectStream
	}
	if _, rUnsup := negotiateRemux(profile, constraints, ed, f); rUnsup == nil {
		return TierDirectStream
	}
	return TierTranscode
}

// maxTier returns the more expensive of two tiers (directPlay < directStream <
// transcode). A video switch composes with the audio/burn picks already baked into a
// Decision: the delivered tier must satisfy BOTH, so it is the max of each pick's
// floor — a non-default video (remux) over a direct-play base escalates to remux, and
// an undecodable video (transcode) over any base escalates to transcode, while a
// default video pick over a burn transcode leaves the transcode standing.
func maxTier(a, b Tier) Tier {
	if tierRank(a) >= tierRank(b) {
		return a
	}
	return b
}

// tierRank orders the tiers by cost so maxTier can compare them: directPlay (cheapest,
// bytes unchanged) < directStream (remux copy) < transcode (re-encode).
func tierRank(t Tier) int {
	switch t {
	case TierDirectPlay:
		return 0
	case TierDirectStream:
		return 1
	default:
		return 2 // transcode (and any unknown) is the most expensive
	}
}

// pickAudioStream selects the audio Stream by preferred language when present,
// else the default audio Stream, else the first. Returns false when the File has
// no audio Stream at all. The preferred-language match is ISO-639 normalized on
// BOTH sides (reusing the audio read-path machinery), so a client hint of "ja",
// "jpn", or "Japanese" all match a Stream tagged "jpn" (audio-streams/01–02) —
// making preferredAudioLang actually select the delivered audio.
func pickAudioStream(f store.File, preferredLang string) (store.Stream, bool) {
	var first, def *store.Stream
	pref := audio.NormalizeLang(preferredLang)
	for i := range f.Streams {
		if f.Streams[i].Kind != "audio" {
			continue
		}
		if first == nil {
			first = &f.Streams[i]
		}
		if f.Streams[i].IsDefault && def == nil {
			def = &f.Streams[i]
		}
		if pref != "" && audio.NormalizeLang(f.Streams[i].Language) == pref {
			return f.Streams[i], true
		}
	}
	switch {
	case def != nil:
		return *def, true
	case first != nil:
		return *first, true
	default:
		return store.Stream{}, false
	}
}

// resolveAudioStream finds the audio Stream identified by streamID anywhere in the
// Title (the parallel of resolveBurnTarget for audio), returning the owning
// Edition+File so an explicit audioStreamId plays exactly the File that carries the
// Stream. ok=false when streamID matches no audio Stream of the Title — an unknown
// id, or an id that belongs to a video/subtitle Stream or another File — which the
// caller surfaces as a structured error rather than a silent default.
func resolveAudioStream(detail store.TitleDetail, streamID string) (store.Edition, store.File, store.Stream, bool) {
	if streamID == "" {
		return store.Edition{}, store.File{}, store.Stream{}, false
	}
	for _, ed := range detail.Editions {
		for _, f := range ed.Files {
			for _, s := range f.Streams {
				if s.Kind == "audio" && s.ID == streamID {
					return ed, f, s, true
				}
			}
		}
	}
	return store.Edition{}, store.File{}, store.Stream{}, false
}

// audioRelIndex returns the audio-relative index of the Stream with id streamID
// within the File (the Nth audio Stream, 0-based — the `-map 0:a:N` selector) and
// the File's total audio-Stream count. found is false when the id names no audio
// Stream of the File. It mirrors resolveBurnTarget's subtitle-relative index count.
func audioRelIndex(f store.File, streamID string) (idx, total int, found bool) {
	idx = -1
	for _, s := range f.Streams {
		if s.Kind != "audio" {
			continue
		}
		if streamID != "" && s.ID == streamID {
			idx = total
			found = true
		}
		total++
	}
	return idx, total, found
}

// audioMapIndex returns the `-map 0:a:N` selector for the negotiated audio Stream,
// or nil when no explicit map should be emitted. It returns nil for a single-audio
// File (total <= 1) so the remux/transcode args stay byte-for-byte unchanged, and
// nil when the Stream can't be located — in both cases ffmpeg's implicit selection
// (which, being the only audio Stream, is correct) is left in place. It is the one
// place the "map only when it matters" rule lives, keeping the args builder a pure
// honor-the-field assembler (audio-streams/02).
func audioMapIndex(f store.File, chosen store.Stream) *int {
	idx, total, found := audioRelIndex(f, chosen.ID)
	if !found || total <= 1 {
		return nil
	}
	return &idx
}

// audioSelectionTier resolves the cheapest playback tier that can DELIVER a
// specific, explicitly-chosen audio Stream of a video File (audio-streams/02,
// ADR-0022). It layers the audio verdict onto the File's video/container verdict:
//
//   - The audio must be re-encoded — the client cannot decode its codec, it exceeds
//     the channel cap, or MPEG-TS cannot carry it — → transcode (re-encode to AAC).
//   - Otherwise the audio is copy-deliverable, so the video/container decides:
//     a codec/resolution/bitrate blocker → transcode; a container-only blocker →
//     remux. And a NON-DEFAULT Stream can never direct-play (direct play carries
//     only the File's default audio), so it escalates a would-be direct play to
//     remux; the default Stream direct-plays unchanged.
//
// The video/container verdict reuses the shared Negotiate/negotiateRemux gates
// (which evaluate the File's DEFAULT audio); that is exact whenever the default
// audio is at least as deliverable as the chosen one — true for the common case of
// a maximally-compatible default track. A pathological File whose default audio is
// LESS deliverable than the pick would at worst over-escalate to transcode, which
// still delivers the chosen Stream correctly.
func audioSelectionTier(profile DeviceProfile, constraints Constraints, ed store.Edition, f store.File, chosen store.Stream) Tier {
	audioCodec := firstNonEmpty(chosen.Codec, f.AudioCodec)
	if audioCodec != "" {
		if !profile.supportsAudio(audioCodec) {
			return TierTranscode
		}
		if cap := profile.MaxAudioChannels; cap > 0 && chosen.Channels > cap {
			return TierTranscode
		}
		if !hlsRemuxableAudio(audioCodec) {
			return TierTranscode
		}
	}
	// Audio is copy-deliverable; the video/container verdict (against the default
	// audio) now decides.
	if _, unsup := Negotiate(profile, constraints, ed, f); unsup == nil {
		// Fully direct-playable. Only the DEFAULT audio may direct-play; a non-default
		// selection escalates to remux so the server can -map the chosen Stream.
		if chosen.IsDefault {
			return TierDirectPlay
		}
		return TierDirectStream
	}
	if _, rUnsup := negotiateRemux(profile, constraints, ed, f); rUnsup == nil {
		return TierDirectStream
	}
	return TierTranscode
}

// estimatedBitrateFor mirrors the per-tier EstimatedBitrate the negotiate helpers
// set: the File's own bitrate for direct play / remux (bytes are copied), and the
// lower of the File bitrate and any constraint cap for a transcode (what the
// re-encoded rendition will sustain). Used by the audioStreamId escalation, which
// builds its Decision outside the shared helpers.
func estimatedBitrateFor(f store.File, constraints Constraints, tier Tier) int64 {
	if tier != TierTranscode {
		return f.Bitrate
	}
	est := f.Bitrate
	if constraints.MaxBitrate > 0 && (est == 0 || est > constraints.MaxBitrate) {
		est = constraints.MaxBitrate
	}
	return est
}

// hlsRemuxableAudio reports whether an audio codec can be COPIED unchanged into
// the HLS segment container (MPEG-TS) that the remux/transcode tiers emit. MPEG-TS
// carries a fixed set of audio codecs as recognized elementary streams; a codec
// outside that set (FLAC, Opus, Vorbis, ALAC, PCM, …) muxes as an unrecognized
// "private data stream" that no browser/hls.js can decode — the root of the
// silent-music bug. It is the orthogonal "can the wire format hold it" gate the
// directStream (remux) tier applies ON TOP OF client decode support: a File whose
// audio the client can decode but MPEG-TS cannot carry must take the transcode
// tier (re-encode to AAC) instead of a broken remux. The set mirrors what ffmpeg's
// mpegts muxer accepts as a real audio stream; anything else returns false so the
// safe default is "transcode".
func hlsRemuxableAudio(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "mp3", "mp2", "ac3", "eac3":
		return true
	default:
		return false
	}
}

// SubtitleTrack is one selectable Subtitle track offered on a playback Decision
// (ADR-0020): the union of every source a viewer can turn on for the played File.
// ID selects the track (a Stream id for embedded, a subtitle-row id for
// sidecar/fetched); Source is embedded|sidecar|fetched; Kind is text|image;
// Language is ISO-639-1 ("" = Unknown). Delivery (a text track's out-of-band
// WebVTT URL, an image track's burn-in) is the api/later-slice concern —
// Convertible flags whether a text track can actually be served as WebVTT, so a
// text format we can't convert (or an image track) isn't given a delivery URL.
type SubtitleTrack struct {
	ID          string
	Source      string
	Kind        string
	Language    string
	Forced      bool
	Convertible bool
	// Format is the track's canonical ORIGINAL text format ("srt"|"vtt"|"ass" per
	// subtitle.TextFormat; "" when unknown or not one we can serve raw, e.g. an
	// embedded mov_text). It lets the api layer offer the original bytes — styling
	// intact — to a client whose Capability profile declares support for the format
	// (libmpv renders ASS natively), with the WebVTT conversion as the fallback.
	Format string
}

// buildSubtitleTracks assembles the played File's full Subtitle-track list from
// all sources, in one order the client treats uniformly (ADR-0020): the File's
// embedded subtitle Streams first (deduped by the observable kind|language|forced,
// so an identical track across parts collapses to one), then the Title's
// Sidecar/Fetched tracks. It mirrors the catalog's toSubtitleTracks so a Title's
// browse list and its playback decision offer the same set — the difference is
// this is scoped to the one negotiated File, and it marks each track's
// convertibility for delivery. Always non-nil.
func buildSubtitleTracks(f store.File, subs []store.Subtitle) []SubtitleTrack {
	out := make([]SubtitleTrack, 0, len(f.Streams)+len(subs))
	seen := map[string]bool{}
	for _, s := range f.Streams {
		if s.Kind != "subtitle" {
			continue
		}
		kind := subtitle.KindForCodec(s.Codec)
		lang := subtitle.NormalizeLang(s.Language)
		key := kind + "|" + lang + "|" + strconv.FormatBool(s.Forced)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, SubtitleTrack{
			ID:       s.ID,
			Source:   "embedded",
			Kind:     kind,
			Language: lang,
			Forced:   s.Forced,
			// Embedded text is extracted to WebVTT via ffmpeg regardless of its source
			// codec (mov_text/subrip/ass all convert), so any embedded text track is
			// deliverable; an image track is not.
			Convertible: kind == "text",
			// The original format is servable raw only for codecs TextFormat knows
			// (subrip/ass); mov_text folds to "" and stays WebVTT-only.
			Format: subtitle.TextFormat(s.Codec),
		})
	}
	for _, sub := range subs {
		lang := subtitle.NormalizeLang(sub.Language)
		out = append(out, SubtitleTrack{
			ID:          sub.ID,
			Source:      sub.Source,
			Kind:        sub.Kind,
			Language:    lang,
			Forced:      sub.Forced,
			Convertible: sub.Kind == "text" && subtitle.IsTextConvertible(sub.Codec),
			Format:      subtitle.TextFormat(sub.Codec),
		})
	}
	return out
}

// BurnSubtitle carries what the transcode args builder needs to burn one image
// Subtitle track into the video frames (subtitles/04): the file ffmpeg reads the
// subtitle from and, for an embedded track, its subtitle-relative stream index.
// It mirrors transcode.BurnSubtitle but lives here so the playback negotiation
// (which resolves the track from the Title detail) does not depend on the caller
// mapping ids to paths.
type BurnSubtitle struct {
	// Path is the subtitle source file: the played video File's path for an EMBEDDED
	// track (the same container carries the sub), or the sidecar subtitle file for a
	// SIDECAR/Fetched image track.
	Path string
	// StreamIndex is the SUBTITLE-relative index of an EMBEDDED track within its File
	// (the Nth subtitle Stream, 0-based) — the ffmpeg `subtitles=…:si=N` selector. It
	// is -1 for a sidecar file, whose single subtitle needs no selector.
	StreamIndex int
	// EditionID/FileID pin the Edition+File the burn track belongs to for an EMBEDDED
	// track, so the escalated transcode plays exactly that File. Empty for a sidecar
	// track, which is Title-scoped and burns over whichever File is chosen.
	EditionID string
	FileID    string
}

// resolveBurnTarget finds the image Subtitle track identified by subID within a
// Title detail and returns the BurnSubtitle describing how to burn it (subtitles/
// 04). It searches the Title's Sidecar/Fetched rows first, then every File's
// embedded subtitle Streams. It returns ok=false when subID matches no track OR
// matches a TEXT track — only an image track burns in (text is delivered
// selectably and never burned), so the caller treats a text/unknown id the same:
// there is no image sub to burn. For an embedded match it also computes the
// subtitle-relative stream index and pins the owning Edition/File.
func resolveBurnTarget(detail store.TitleDetail, subID string) (BurnSubtitle, bool) {
	// Sidecar/Fetched rows: an on-disk subtitle file. Only an image track burns.
	for _, sub := range detail.Subtitles {
		if sub.ID != subID {
			continue
		}
		if sub.Kind != "image" {
			return BurnSubtitle{}, false
		}
		return BurnSubtitle{Path: sub.Path, StreamIndex: -1}, true
	}
	// Embedded Streams: rasterized from the container by their subtitle-relative
	// index. si counts subtitle streams only, so track the ordinal as we scan.
	for _, ed := range detail.Editions {
		for _, f := range ed.Files {
			si := 0
			for _, s := range f.Streams {
				if s.Kind != "subtitle" {
					continue
				}
				if s.ID == subID {
					if subtitle.KindForCodec(s.Codec) != "image" {
						return BurnSubtitle{}, false
					}
					return BurnSubtitle{
						Path:        f.Path,
						StreamIndex: si,
						EditionID:   ed.ID,
						FileID:      f.ID,
					}, true
				}
				si++
			}
		}
	}
	return BurnSubtitle{}, false
}

// editionFileByID finds the Edition+File a burn target pins (an embedded track's
// owning File). Returns ok=false when the ids no longer resolve (a File that
// vanished between the catalog read and negotiation).
func editionFileByID(detail store.TitleDetail, editionID, fileID string) (store.Edition, store.File, bool) {
	for _, ed := range detail.Editions {
		if ed.ID != editionID {
			continue
		}
		for _, f := range ed.Files {
			if f.ID == fileID {
				return ed, f, true
			}
		}
	}
	return store.Edition{}, store.File{}, false
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// itoa is a tiny local int→string for error details (avoids importing strconv
// just for messages).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
