package subtitle

import (
	"fmt"
	"strconv"
	"strings"
)

// In-band HLS text-subtitle delivery (ADR-0020, subtitles/03). On the remux/
// transcode (HLS) tiers a text Subtitle track rides in-band as an
// #EXT-X-MEDIA:TYPE=SUBTITLES rendition referenced from a master playlist (one
// video rendition + a subtitle group — the scoped ADR-0004 master-playlist
// amendment; still single video rendition, no ABR). The WebVTT is SEGMENTED to
// the video segment cadence and each segment carries an X-TIMESTAMP-MAP so cues
// line up with the video PTS; seeking realigns the subtitle rendition exactly as
// the video segments realign (the player fetches only the segment covering the
// sought time).
//
// This file holds the PURE, dependency-free builders — segmentation and the two
// playlist writers — so the whole in-band shape is unit-testable without a
// server or ffmpeg. The api layer produces the whole-file WebVTT (reusing the
// out-of-band conversion path) and wires these into the session-scoped /hls
// route.

// HLSTimestampMapMPEGTS is the MPEGTS value written into every in-band subtitle
// segment's X-TIMESTAMP-MAP (with LOCAL:00:00:00.000), in a 90 kHz clock. It
// anchors WebVTT time 0 to the video's first presentation timestamp so cues,
// authored in absolute media time, line up with the video.
//
// Why 133500 (= 1.4833 s): our HLS video is muxed by ffmpeg's mpegts muxer,
// whose VOD output does NOT start at PTS 0 — it applies a small initial offset.
// Probing the exact flags this server uses (-f hls -hls_segment_type mpegts,
// SegmentSeconds=4), the first video packet lands at PTS 133500 for a from-the-
// top encode AND for a seek-realigned (-ss) restart, so the offset is stable
// across seeks. hls.js's WebVTT sync computes an offset of
// (X-TIMESTAMP-MAP.MPEGTS − mainTrackInitPTS)/90000 and adds it to each cue; when
// this value equals the video's init PTS that offset is zero and cues render at
// their absolute times. The offset does vary a little by codec/framerate and on
// the copy (remux) path (observed range ≈ 128250–133500, a spread under 0.06 s),
// so a single constant leaves a worst-case sub-60 ms skew — imperceptible for
// subtitles and well inside the segment-fetch tolerance. Deriving it exactly
// would mean ffprobing the live video segment per request; the fixed anchor is
// the deliberate simpler trade (a future refinement could probe it).
const HLSTimestampMapMPEGTS = 133500

// HLSSubtitleGroupID is the GROUP-ID that ties the master playlist's
// #EXT-X-MEDIA subtitle renditions to the single video #EXT-X-STREAM-INF. A
// fixed id is fine — there is exactly one subtitle group (no ABR, one video
// rendition). Exported so the unified master builder (audio-streams/03) can
// reference the same group when composing an AUDIO group alongside it.
const HLSSubtitleGroupID = "subs"

// HLSMasterBandwidth is the nominal BANDWIDTH advertised on the master
// playlist's single #EXT-X-STREAM-INF. BANDWIDTH is required by the HLS spec but,
// with exactly one video rendition and no ABR ladder (ADR-0004), the player never
// chooses between renditions on it — so a fixed nominal value is honest enough
// and avoids threading a per-session estimate through the master builder. Exported
// so the unified (audio + subtitle) master builder emits the identical value.
const HLSMasterBandwidth = 2_000_000

// Rendition describes one #EXT-X-MEDIA:TYPE=SUBTITLES entry for the master
// playlist: a single selectable in-band text track. URI is the (session-relative)
// subtitle media-playlist name. Name is the human label; Language is ISO-639-1
// ("" omits the LANGUAGE attribute). Forced marks a forced track — it is emitted
// FORCED=YES and, being the auto-display default (text-only forced auto-display,
// ADR-0020), DEFAULT=YES so a native-HLS player turns it on without user action;
// non-forced tracks are DEFAULT=NO (subtitles default off).
type Rendition struct {
	URI      string
	Name     string
	Language string
	Forced   bool
}

// MasterPlaylist builds the HLS master playlist for an HLS session carrying
// subtitles: one video #EXT-X-STREAM-INF (referencing videoURI, the existing
// media playlist) bound to a SUBTITLES group listing every renditions entry. It
// stays single video rendition — no ABR (ADR-0004 amendment). videoURI and each
// Rendition.URI are session-relative names the /hls route serves from the same
// directory. Always emits the video rendition; renditions may be empty (a master
// with no subtitle group is still valid, though the caller only points at the
// master when there is at least one deliverable text track).
func MasterPlaylist(videoURI string, renditions []Rendition) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(RenditionLines(renditions))
	// The single video rendition, tied to the subtitle group so the player exposes
	// the subtitles alongside it. No RESOLUTION/CODECS — the one rendition is
	// self-describing via its media playlist; BANDWIDTH is the only required attr.
	fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", HLSMasterBandwidth)
	if len(renditions) > 0 {
		fmt.Fprintf(&b, `,SUBTITLES="%s"`, HLSSubtitleGroupID)
	}
	b.WriteByte('\n')
	b.WriteString(videoURI)
	b.WriteByte('\n')
	return []byte(b.String())
}

// RenditionLines writes the `#EXT-X-MEDIA:TYPE=SUBTITLES` lines for a master
// playlist — one per rendition, in order — with no trailing STREAM-INF. It is
// exported so a UNIFIED master builder (audio-streams/03) that carries BOTH an AUDIO
// group and this SUBTITLES group can compose the two rendition blocks without
// duplicating the subtitle line format. MasterPlaylist itself uses it, so the
// subtitle-only output is unchanged.
func RenditionLines(renditions []Rendition) string {
	var b strings.Builder
	for _, r := range renditions {
		b.WriteString(`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="`)
		b.WriteString(HLSSubtitleGroupID)
		b.WriteString(`",NAME="`)
		b.WriteString(HLSAttrEscape(r.Name))
		b.WriteString(`"`)
		if r.Language != "" {
			b.WriteString(`,LANGUAGE="`)
			b.WriteString(HLSAttrEscape(r.Language))
			b.WriteString(`"`)
		}
		// A forced track is the text-only auto-display default (ADR-0020): DEFAULT=YES
		// so native HLS turns it on unprompted. Everything else defaults off.
		if r.Forced {
			b.WriteString(`,DEFAULT=YES,AUTOSELECT=YES,FORCED=YES`)
		} else {
			b.WriteString(`,DEFAULT=NO,AUTOSELECT=YES,FORCED=NO`)
		}
		b.WriteString(`,URI="`)
		b.WriteString(HLSAttrEscape(r.URI))
		b.WriteString("\"\n")
	}
	return b.String()
}

// SubtitleMediaPlaylist builds the media playlist for one in-band subtitle
// rendition: a VOD playlist of count uniform segSeconds-long WebVTT segments,
// named by nameFor(i), ending in EXT-X-ENDLIST. It mirrors the video media
// playlist's shape (same VOD/target-duration/segment cadence) so the player
// fetches subtitle segment N exactly as it fetches video segment N; the cues'
// absolute times (plus each segment's X-TIMESTAMP-MAP) carry the real timing, so
// a uniform segSeconds EXTINF is sufficient even when the video's own segments
// vary slightly (remux). A zero count yields an empty (but valid) VOD playlist.
func SubtitleMediaPlaylist(count, segSeconds int, nameFor func(i int) string) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", segSeconds)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "#EXTINF:%d.000000,\n", segSeconds)
		b.WriteString(nameFor(i))
		b.WriteByte('\n')
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return []byte(b.String())
}

// SegmentVTT returns the WebVTT for one subtitle segment: the cues of the whole-
// file WebVTT full that overlap the window [index*segSeconds, (index+1)*segSeconds)
// seconds, with their ABSOLUTE times preserved, under a header carrying the
// X-TIMESTAMP-MAP that anchors them to the video PTS. A cue spanning a segment
// boundary is repeated in every segment it overlaps (standard HLS WebVTT), so a
// seek into any segment shows the cue active at that moment. The output is always
// a valid WebVTT document even when no cue overlaps (header only).
func SegmentVTT(full []byte, index, segSeconds int) []byte {
	winStart := float64(index * segSeconds)
	winEnd := float64((index + 1) * segSeconds)

	var b strings.Builder
	b.WriteString("WEBVTT\n")
	fmt.Fprintf(&b, "X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000\n", HLSTimestampMapMPEGTS)
	for _, c := range parseCues(full) {
		// Half-open overlap: the cue is active somewhere inside the window.
		if c.end > winStart && c.start < winEnd {
			b.WriteString("\n")
			b.WriteString(c.block)
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// HLSAttrEscape sanitizes a value for an HLS quoted-string attribute. Per the HLS
// spec a quoted-string may contain neither a double quote nor a line break, so we
// strip both (our values — language codes and menu labels — never legitimately
// carry them; this only guards against a pathological label breaking the
// playlist). Exported so the unified master builder reuses the identical escaping
// for AUDIO rendition labels.
func HLSAttrEscape(s string) string {
	return strings.NewReplacer(`"`, "", "\n", " ", "\r", " ").Replace(s)
}

// cue is one parsed WebVTT cue: its start/end in seconds (for the segment-overlap
// test) and the verbatim block text (the timing line — with its absolute times
// and any cue settings — plus the payload lines, and any id line above it), so
// re-emitting a cue preserves it exactly.
type cue struct {
	start float64
	end   float64
	block string
}

// parseCues extracts the cues from a WebVTT document. It splits on blank lines
// into blocks, keeps only blocks whose lines include a `-->` timing line, and
// parses that line's start/end. Header (WEBVTT ...), NOTE and STYLE blocks carry
// no timing line and are dropped — a subtitle segment needs only cues. The block
// text is returned trimmed of a trailing newline so the caller controls spacing.
func parseCues(full []byte) []cue {
	var cues []cue
	blocks := strings.Split(strings.ReplaceAll(string(full), "\r\n", "\n"), "\n\n")
	for _, block := range blocks {
		block = strings.Trim(block, "\n")
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		timingIdx := -1
		for i, ln := range lines {
			if strings.Contains(ln, "-->") {
				timingIdx = i
				break
			}
		}
		if timingIdx < 0 {
			continue // header / NOTE / STYLE — no cue timing
		}
		start, end, ok := parseTimingLine(lines[timingIdx])
		if !ok {
			continue
		}
		cues = append(cues, cue{start: start, end: end, block: block})
	}
	return cues
}

// parseTimingLine parses a WebVTT cue timing line ("<start> --> <end> [settings]")
// into start/end seconds. It tolerates cue settings after the end timestamp. A
// line it cannot parse (both timestamps must be valid) returns ok=false so the
// block is skipped rather than mis-timed.
func parseTimingLine(line string) (start, end float64, ok bool) {
	i := strings.Index(line, "-->")
	if i < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(line[:i])
	rest := strings.TrimSpace(line[i+len("-->"):])
	// The end timestamp is the first whitespace-delimited token after -->; any cue
	// settings (align:, position:, …) follow it and are not part of the timestamp.
	endStr := rest
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		endStr = rest[:sp]
	}
	s, sok := parseVTTTimestamp(startStr)
	e, eok := parseVTTTimestamp(endStr)
	if !sok || !eok {
		return 0, 0, false
	}
	return s, e, true
}

// parseVTTTimestamp parses a WebVTT timestamp — "HH:MM:SS.mmm" or "MM:SS.mmm" —
// into seconds. Returns ok=false on any malformed component.
func parseVTTTimestamp(ts string) (float64, bool) {
	ts = strings.TrimSpace(ts)
	dot := strings.LastIndex(ts, ".")
	if dot < 0 {
		return 0, false
	}
	millis, err := strconv.Atoi(ts[dot+1:])
	if err != nil {
		return 0, false
	}
	parts := strings.Split(ts[:dot], ":")
	var h, m, s int
	var err2 error
	switch len(parts) {
	case 3:
		h, err2 = strconv.Atoi(parts[0])
		if err2 != nil {
			return 0, false
		}
		m, err2 = strconv.Atoi(parts[1])
		if err2 != nil {
			return 0, false
		}
		s, err2 = strconv.Atoi(parts[2])
	case 2:
		m, err2 = strconv.Atoi(parts[0])
		if err2 != nil {
			return 0, false
		}
		s, err2 = strconv.Atoi(parts[1])
	default:
		return 0, false
	}
	if err2 != nil {
		return 0, false
	}
	return float64(h)*3600 + float64(m)*60 + float64(s) + float64(millis)/1000, true
}
