package audio

import (
	"fmt"
	"strings"

	"github.com/marioquake/juicebox/internal/subtitle"
)

// In-band HLS AUDIO rendition delivery (ADR-0022, audio-streams/03). On the HLS
// tiers a multi-audio File is DEMUXED: a single video-only variant plus every audio
// Stream advertised as an `#EXT-X-MEDIA:TYPE=AUDIO` rendition in the master
// playlist, so any HLS player's native audio menu switches tracks instantly with no
// session restart. This file holds the PURE master-playlist builder — the audio
// group lines + the single video #EXT-X-STREAM-INF that references the AUDIO group
// (and, when present, the SUBTITLES group). The per-rendition media playlists +
// segments are ffmpeg's own output, served by the api/session layer.
//
// The builder composes with the existing in-band subtitle renditions (ADR-0020):
// one master carries BOTH an AUDIO group and a SUBTITLES group, and the video
// variant references whichever groups exist. It reuses the subtitle package's
// rendition-line format (subtitle.RenditionLines) so the two blocks stay
// consistent, and emits byte-identical output to subtitle.MasterPlaylist when there
// are no audio renditions — so a subtitle-only session is unchanged.

// HLSAudioGroupID is the GROUP-ID tying the master's `#EXT-X-MEDIA:TYPE=AUDIO`
// renditions to the single video `#EXT-X-STREAM-INF`. A fixed id is fine: there is
// exactly one audio group (no ABR, one video variant), mirroring the subtitle group.
const HLSAudioGroupID = "aud"

// Rendition describes one `#EXT-X-MEDIA:TYPE=AUDIO` entry for the master playlist:
// a single selectable in-band audio track. URI is the session-relative rendition
// media-playlist name (audio_<streamId>.m3u8). Name is the human menu label
// ("English 5.1", "English Director's Commentary"); Language is the ISO-639-1 code
// ("" omits the LANGUAGE attribute); Default marks the rendition an HLS player
// selects unless the viewer picks another (the resolved default audio Stream).
type Rendition struct {
	URI      string
	Name     string
	Language string
	Default  bool
}

// Variant describes the single video `#EXT-X-STREAM-INF` of the master. Apple's
// native player validates these attributes against the ACTUAL stream, so they must
// be honest — and for HDR content they are load-bearing (see VideoRange):
//
//   - Bandwidth: the variant's peak bits/sec; 0 falls back to the legacy nominal
//     (subtitle.HLSMasterBandwidth).
//   - Codecs: the RFC 6381 CODECS attribute (video + audio-group codecs), REQUIRED
//     for Safari to accept an HEVC fMP4 variant (ADR-0024); "" omits it.
//   - Width/Height: the RESOLUTION attribute when both are > 0.
//   - FrameRate: the FRAME-RATE attribute when > 0.
//   - VideoRange: "PQ" / "HLG" for HDR content. Safari HARD-FAILS an HDR stream
//     under a variant with no VIDEO-RANGE (implicitly SDR — "failed to decode" the
//     moment the init segment arrives), and it will not even LOAD a PQ variant that
//     lacks RESOLUTION/FRAME-RATE, so the three travel together. "" (SDR) omits it.
type Variant struct {
	Bandwidth  int64
	Codecs     string
	Width      int
	Height     int
	FrameRate  float64
	VideoRange string
}

// MasterPlaylist builds the HLS master playlist for a DEMUXED multi-audio session
// (audio-streams/03): the AUDIO group's `#EXT-X-MEDIA` lines, then the in-band
// SUBTITLES group's lines (when any), then the single video `#EXT-X-STREAM-INF`
// (referencing videoURI — the video-only variant's media playlist, described by v)
// bound to the AUDIO group and, when present, the SUBTITLES group. It stays single
// video rendition — no ABR (ADR-0004 amendment). Every URI is a session-relative
// name the /hls route serves.
//
// With no audio renditions and a zero Variant the output is byte-identical to
// subtitle.MasterPlaylist, so a subtitle-only (or bare) master is unchanged. A
// master with neither group is still valid (the caller only points at the master
// when there is a group to carry).
func MasterPlaylist(videoURI string, v Variant, audioRenditions []Rendition, subtitleRenditions []subtitle.Rendition) []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	for _, r := range audioRenditions {
		b.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="`)
		b.WriteString(HLSAudioGroupID)
		b.WriteString(`",NAME="`)
		b.WriteString(subtitle.HLSAttrEscape(r.Name))
		b.WriteString(`"`)
		if r.Language != "" {
			b.WriteString(`,LANGUAGE="`)
			b.WriteString(subtitle.HLSAttrEscape(r.Language))
			b.WriteString(`"`)
		}
		// The resolved default audio is the rendition a native HLS player turns on
		// unprompted; AUTOSELECT=YES lets the player also pick it by language preference.
		if r.Default {
			b.WriteString(`,DEFAULT=YES,AUTOSELECT=YES`)
		} else {
			b.WriteString(`,DEFAULT=NO,AUTOSELECT=YES`)
		}
		b.WriteString(`,URI="`)
		b.WriteString(subtitle.HLSAttrEscape(r.URI))
		b.WriteString("\"\n")
	}
	// Reuse the subtitle package's rendition-line format so the two blocks are
	// consistent and the subtitle-only output is unchanged.
	b.WriteString(subtitle.RenditionLines(subtitleRenditions))
	// The single video variant (no ABR). BANDWIDTH is the only required attr; the
	// rest are added when known (and, for HDR, REQUIRED — see Variant). It
	// references each group that exists so the player exposes the tracks alongside it.
	bw := v.Bandwidth
	if bw <= 0 {
		bw = subtitle.HLSMasterBandwidth
	}
	fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", bw)
	if v.Codecs != "" {
		fmt.Fprintf(&b, `,CODECS="%s"`, subtitle.HLSAttrEscape(v.Codecs))
	}
	if v.Width > 0 && v.Height > 0 {
		fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
	}
	if v.FrameRate > 0 {
		fmt.Fprintf(&b, ",FRAME-RATE=%.3f", v.FrameRate)
	}
	if v.VideoRange != "" {
		fmt.Fprintf(&b, ",VIDEO-RANGE=%s", v.VideoRange)
	}
	if len(audioRenditions) > 0 {
		fmt.Fprintf(&b, `,AUDIO="%s"`, HLSAudioGroupID)
	}
	if len(subtitleRenditions) > 0 {
		fmt.Fprintf(&b, `,SUBTITLES="%s"`, subtitle.HLSSubtitleGroupID)
	}
	b.WriteByte('\n')
	b.WriteString(videoURI)
	b.WriteByte('\n')
	return []byte(b.String())
}
