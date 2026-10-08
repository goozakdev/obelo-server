package transcode

import (
	"sort"
	"strconv"
	"strings"
)

// OnlineProtocolWhitelist is what ffmpeg may open for an Online item (ADR-0068
// decision 9): https and the three layers it needs to carry it, plus crypto for an
// encrypted HLS segment. It blocks file:, plain http, data:, rtmp, udp, concat,
// pipe and the rest, which is what stops a Plugin-supplied URL or a manifest it
// points at from naming a local file or a non-https target.
const OnlineProtocolWhitelist = "https,tls,tcp,crypto"

// OnlineJob describes one ffmpeg run for an Online item: read one or two https
// inputs and encode them for the client into an HLS event playlist in OutputDir.
// Unlike a Title's jobs it ALWAYS re-encodes the video, since a variant is only
// sent here when the client could not play it as is, and the source is remote and
// unprobed.
type OnlineJob struct {
	// Inputs are the https URLs ffmpeg reads: one for a muxed or manifest variant,
	// two (picture, then sound) for a split one. The host has already judged them.
	Inputs []string
	// Headers are the request headers the media host requires (referer, user agent),
	// sent with every input.
	Headers map[string]string
	// OutputDir is the session scratch directory (already created by the caller).
	OutputDir string
	// MaxHeight and MaxBitrate bound the encode; 0 is no bound.
	MaxHeight  int
	MaxBitrate int64
	// Accel is the video encode backend, as for a Title.
	Accel Accel
	// StartSeconds and Append continue an encode that stopped: input-seek to
	// StartSeconds and append to the playlist already in OutputDir after a
	// discontinuity, on the same timeline. ffmpeg numbers the new segments from that
	// playlist itself (a -start_number as well would count them twice). Zero and
	// false are a fresh run.
	StartSeconds float64
	Append       bool
}

// OnlineArgs builds the ffmpeg argument vector for an Online run. Every input is
// preceded by -protocol_whitelist (an input option applies to the next -i only, so
// it is repeated), then the variant's request headers.
func OnlineArgs(job OnlineJob) []string {
	// -nostats keeps the progress lines out of the stderr tail the host reads for a
	// refusal.
	args := []string{"-nostdin", "-nostats", "-y"}
	be := videoBackend(job.Accel)
	args = append(args, be.initArgs...)

	userAgent, headers := onlineHeaderOptions(job.Headers)
	for _, in := range job.Inputs {
		// Verify the media host's certificate: a build of ffmpeg whose TLS default is
		// off must not fetch from an impostor. The system CA store is used.
		args = append(args, "-protocol_whitelist", OnlineProtocolWhitelist, "-tls_verify", "1")
		if userAgent != "" {
			args = append(args, "-user_agent", userAgent)
		}
		if headers != "" {
			args = append(args, "-headers", headers)
		}
		if job.StartSeconds > 0 {
			args = append(args, "-ss", strconv.FormatFloat(job.StartSeconds, 'f', -1, 64))
		}
		args = append(args, "-i", in)
	}

	// A split variant's picture is the first input and its sound the second; a
	// muxed or manifest one has both in the first. The audio map is optional, so a
	// silent video plays rather than failing the run.
	if len(job.Inputs) > 1 {
		args = append(args, "-map", "0:v:0", "-map", "1:a:0?")
	} else {
		args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
	}

	args = append(args, "-c:v", be.encoder)
	args = append(args, be.presetArgs...)
	args = append(args, "-force_key_frames", forceKeyFramesExpr)
	if vf := videoFilterChain(be, job.MaxHeight); vf != "" {
		args = append(args, "-vf", vf)
	}
	if job.MaxBitrate > 0 {
		b := strconv.FormatInt(job.MaxBitrate, 10)
		args = append(args, "-b:v", b, "-maxrate", b, "-bufsize", strconv.FormatInt(job.MaxBitrate*2, 10))
	}
	args = append(args, "-c:a", audioEncoderAAC)

	// An EVENT playlist, not VOD: the muxer writes a VOD playlist only when it has
	// finished the whole input, and the player must start long before that. The
	// segments and playlist are the session's scratch and nothing else.
	//
	// ffmpeg never writes the playlist's ENDLIST (omit_endlist): it exits 0 after
	// skipping segments the media host refused, and a player must not be told the
	// stream is over before the host has judged why it stopped. The host adds
	// ENDLIST itself when an encode ends cleanly.
	hlsFlags := "independent_segments+temp_file+omit_endlist"
	if job.Append {
		hlsFlags += "+append_list"
	}
	if job.StartSeconds > 0 {
		// ffmpeg rebases the output to ~0 after an input seek; keep the session's timeline.
		args = append(args, "-output_ts_offset", strconv.FormatFloat(job.StartSeconds, 'f', -1, 64))
	}
	return append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(SegmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", hlsFlags,
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", join(job.OutputDir, SegmentPattern),
		join(job.OutputDir, PlaylistName),
	)
}

// onlineHeaderOptions splits a variant's headers into the -user_agent value and the
// -headers block (CRLF-terminated lines, in a fixed order) ffmpeg takes. A name or
// value holding a line break or control character is dropped whole, so one header
// can never carry another.
func onlineHeaderOptions(h map[string]string) (userAgent, block string) {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		v := h[k]
		if k == "" || hasControl(k) || hasControl(v) || strings.ContainsAny(k, ": ") {
			continue
		}
		if strings.EqualFold(k, "User-Agent") {
			userAgent = v
			continue
		}
		b.WriteString(k + ": " + v + "\r\n")
	}
	return userAgent, b.String()
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
