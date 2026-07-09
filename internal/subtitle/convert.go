package subtitle

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// WebVTT delivery (ADR-0020): text Subtitle tracks are served to the client as
// WebVTT, the one text format a browser <track> renders natively. This file is
// the pure, binary-free conversion path shared by sidecar delivery (a subtitle
// file on disk) and — after ffmpeg extracts an embedded Stream to its source
// text — anything that arrives as SRT/ASS text rather than already-WebVTT.
//
// Three input formats are handled, matching the codec/format tokens the scanner
// records (sidecarCodec) and ffprobe reports:
//   - SubRip (srt/subrip): the common case — reheadered, comma→dot timestamps.
//   - WebVTT (vtt/webvtt): passed through (only a WEBVTT header is guaranteed).
//   - ASS/SSA (ass/ssa): DOWNGRADED to plain cues — dialogue kept, all styling
//     (override tags, positioning, karaoke) dropped, since a <track> can't render
//     it. Preserving ASS styling is explicitly future work (PRD "Out of Scope").
//
// Image formats (PGS/VOBSUB/DVD) are NOT here: they cannot become text and are
// burned in on a later slice. IsTextConvertible gates that at the call site.

// vttHeader is the mandatory first line of a WebVTT file. A body missing it is
// not a valid track, so every converter emits it.
const vttHeader = "WEBVTT"

// timingRe matches a SubRip/WebVTT cue-timing line: two H:MM:SS timestamps with
// a 2- or 3-digit fractional part separated by ",", "." (or, defensively, ":")
// around the "-->" arrow. Trailing cue-position settings (SRT coordinates or VTT
// cue settings) after the end timestamp are captured separately and dropped —
// a <track> ignores SRT coordinates and we emit no positioning. Hours are
// optional in some SRT dialects, so the hour group is optional and defaulted.
var timingRe = regexp.MustCompile(
	`^\s*(?:(\d+):)?(\d{1,2}):(\d{2})[,.:](\d{1,3})\s*-->\s*(?:(\d+):)?(\d{1,2}):(\d{2})[,.:](\d{1,3})`,
)

// indexRe matches a bare SubRip cue counter (a line that is only digits): the
// numeric index preceding a timing line, which WebVTT does not use.
var indexRe = regexp.MustCompile(`^\s*\d+\s*$`)

// cueEntityRe matches an already-valid HTML/XML character reference so we don't
// double-escape an "&amp;"/"&#233;" a source file already encoded.
var cueEntityRe = regexp.MustCompile(`^&(?:[a-zA-Z][a-zA-Z0-9]*|#[0-9]+|#[xX][0-9a-fA-F]+);`)

// cueTagRe matches an inline markup tag that WebVTT cue text understands and that
// SubRip commonly carries (<i>/<b>/<u>/<c>/<v>/<lang>/<ruby>/<rt>, with optional
// classes/annotations, plus their closing forms). A "<" that starts one of these
// is kept verbatim; any other "<" is escaped so stray angle brackets in dialogue
// don't get parsed as (broken) markup.
var cueTagRe = regexp.MustCompile(`^</?(?:c|i|b|u|v|lang|ruby|rt)(?:[.:][^\s>]+)*(?:[ \t][^>]*)?>`)

// escapeCueText makes a line of subtitle dialogue safe as WebVTT cue text: a raw
// "&" becomes "&amp;" (unless it already begins a character reference) and a raw
// "<" becomes "&lt;" (unless it begins a recognized inline tag). Without this a
// cue like "if x < y" or "Tom & Jerry" is parsed as cue markup and silently
// truncated by the browser. ">" needs no escaping in WebVTT text, so it is kept.
func escapeCueText(line string) string {
	// Fast path: the overwhelming majority of cues have neither character.
	if !strings.ContainsAny(line, "<&") {
		return line
	}
	var b strings.Builder
	b.Grow(len(line) + 8)
	for i := 0; i < len(line); {
		switch line[i] {
		case '&':
			if m := cueEntityRe.FindString(line[i:]); m != "" {
				b.WriteString(m)
				i += len(m)
				continue
			}
			b.WriteString("&amp;")
			i++
		case '<':
			if m := cueTagRe.FindString(line[i:]); m != "" {
				b.WriteString(m)
				i += len(m)
				continue
			}
			b.WriteString("&lt;")
			i++
		default:
			b.WriteByte(line[i])
			i++
		}
	}
	return b.String()
}

// TextFormat folds a codec/format token (from the scanner's sidecarCodec or an
// ffprobe codec_name) to the canonical converter format this package handles:
// "srt", "vtt", or "ass". A token that is a known text format but one we don't
// convert (e.g. MicroDVD, which needs a frame rate), or a bitmap/unknown token,
// returns "" — the caller treats "" as "not convertible to WebVTT here".
func TextFormat(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "srt", "subrip":
		return "srt"
	case "vtt", "webvtt":
		return "vtt"
	case "ass", "ssa":
		return "ass"
	default:
		return ""
	}
}

// IsTextConvertible reports whether a track with this codec/format token can be
// delivered as WebVTT through ToWebVTT (i.e. TextFormat recognizes it). Used to
// decide whether a text track gets an out-of-band delivery URL.
func IsTextConvertible(codec string) bool {
	return TextFormat(codec) != ""
}

// ToWebVTT converts subtitle bytes in the given format ("srt"|"vtt"|"ass", as
// returned by TextFormat) to valid WebVTT. An unrecognized/empty format is an
// error (the caller should have gated on IsTextConvertible). The output always
// begins with the WEBVTT header and ends with a trailing newline.
func ToWebVTT(data []byte, format string) ([]byte, error) {
	switch TextFormat(format) {
	case "srt":
		return srtToVTT(data), nil
	case "vtt":
		return passthroughVTT(data), nil
	case "ass":
		return assToVTT(data), nil
	default:
		return nil, fmt.Errorf("subtitle: cannot convert format %q to WebVTT", format)
	}
}

// srtToVTT rewrites SubRip into WebVTT: prepend the header, drop the numeric cue
// counters SRT uses but WebVTT does not, and normalize each timing line's
// comma-milliseconds to the dotted form (trimming any trailing SRT coordinates).
// Cue text is copied verbatim (SubRip inline tags like <i>/<b> are valid in
// WebVTT too), so the only loss is the counter and any position coordinates.
func srtToVTT(data []byte) []byte {
	lines := splitLines(data)
	var b strings.Builder
	b.WriteString(vttHeader)
	b.WriteString("\n\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// Drop a numeric cue counter, but only when it directly precedes a timing
		// line (so a line of dialogue that happens to be all digits is kept).
		if indexRe.MatchString(line) && i+1 < len(lines) && timingRe.MatchString(lines[i+1]) {
			continue
		}
		if timingRe.MatchString(line) {
			b.WriteString(normalizeTiming(line))
			b.WriteByte('\n')
			continue
		}
		// Cue text: escape raw & and < so a literal "x < y" or "Tom & Jerry" is not
		// parsed as (broken) WebVTT cue markup and silently truncated. Legitimate
		// SubRip inline tags (<i>/<b>/<u>/…) that WebVTT also understands are kept.
		b.WriteString(escapeCueText(line))
		b.WriteByte('\n')
	}
	return []byte(ensureTrailingNewline(collapseBlankRun(b.String())))
}

// passthroughVTT returns already-WebVTT bytes essentially unchanged, guaranteeing
// only that the WEBVTT header is present (some tools emit a headerless body, or a
// BOM before it). Timing/cue text is left exactly as authored.
func passthroughVTT(data []byte) []byte {
	s := strings.TrimPrefix(string(data), "\ufeff") // strip a leading UTF-8 BOM
	// A valid WebVTT file MUST begin with the WEBVTT signature (after the optional
	// BOM) \u2014 any leading whitespace/blank lines before it make the browser reject
	// the whole track, so strip them before serving.
	s = strings.TrimLeft(s, " \t\r\n")
	if strings.HasPrefix(s, vttHeader) {
		return []byte(ensureTrailingNewline(s))
	}
	return []byte(vttHeader + "\n\n" + ensureTrailingNewline(strings.TrimRight(s, "\r\n")))
}

// --- ASS/SSA ---------------------------------------------------------------

// assOverrideRe matches an ASS override block ({\pos(…)}, {\i1}, {\k20}, …): all
// styling/positioning/karaoke, which a <track> cannot render, so it is stripped.
var assOverrideRe = regexp.MustCompile(`\{[^}]*\}`)

// assToVTT downgrades ASS/SSA to plain WebVTT cues: it reads the [Events] section
// header to locate the Start/End/Text fields (their column order is declared, not
// fixed), then emits one cue per Dialogue line with the timings converted and the
// text stripped of all override tags (\N/\h escapes normalized to a line break /
// space). Everything else — [Script Info], [V4+ Styles], Comment lines, layer,
// style, actor, margins, effects — is discarded: this is a deliberate styling
// downgrade (PRD), not a faithful render.
func assToVTT(data []byte) []byte {
	lines := splitLines(data)
	// Field indices within a Dialogue line, discovered from the Format: line of the
	// [Events] section. ASS defaults are well-known, but the Format line is
	// authoritative, so we honor it when present.
	startIdx, endIdx, textIdx, fieldCount := 1, 2, 9, 10
	inEvents := false

	var b strings.Builder
	b.WriteString(vttHeader)
	b.WriteString("\n\n")
	wrote := false

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inEvents = strings.EqualFold(line, "[Events]")
			continue
		}
		if !inEvents {
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "format:") {
			startIdx, endIdx, textIdx, fieldCount = parseASSFormat(line[len("format:"):])
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), "dialogue:") {
			continue // Comment: lines and anything else are dropped
		}
		fields := splitASSFields(line[len("dialogue:"):], fieldCount)
		if startIdx >= len(fields) || endIdx >= len(fields) || textIdx >= len(fields) {
			continue
		}
		start := parseASSTime(fields[startIdx])
		end := parseASSTime(fields[endIdx])
		if start == "" || end == "" {
			continue
		}
		text := escapeCueText(cleanASSText(fields[textIdx]))
		if text == "" {
			continue
		}
		if wrote {
			b.WriteByte('\n')
		}
		b.WriteString(start)
		b.WriteString(" --> ")
		b.WriteString(end)
		b.WriteByte('\n')
		b.WriteString(text)
		b.WriteByte('\n')
		wrote = true
	}
	return []byte(ensureTrailingNewline(b.String()))
}

// parseASSFormat reads an [Events] "Format:" field list and returns the 0-based
// column indices of Start, End, and Text plus the total field count. Text is
// always the last field in ASS (it can contain commas), so its index is
// len(fields)-1 regardless of where it's named. Falls back to the ASS defaults
// for any field the Format line omits.
func parseASSFormat(list string) (startIdx, endIdx, textIdx, count int) {
	parts := strings.Split(list, ",")
	startIdx, endIdx, textIdx, count = 1, 2, len(parts)-1, len(parts)
	for i, p := range parts {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "start":
			startIdx = i
		case "end":
			endIdx = i
		case "text":
			textIdx = i
		}
	}
	if count < 1 {
		return 1, 2, 9, 10
	}
	return startIdx, endIdx, textIdx, count
}

// splitASSFields splits a Dialogue line body into exactly count fields: the first
// count-1 are comma-delimited, and the final field (Text) keeps every remaining
// comma verbatim (ASS dialogue frequently contains commas). SplitN with count
// does exactly this.
func splitASSFields(body string, count int) []string {
	if count < 1 {
		count = 10
	}
	fields := strings.SplitN(body, ",", count)
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	return fields
}

// parseASSTime converts an ASS timestamp (H:MM:SS.cc, centiseconds) to a WebVTT
// timestamp (HH:MM:SS.mmm, milliseconds). Returns "" on a malformed value so the
// caller drops the cue rather than emit a broken one.
func parseASSTime(v string) string {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return ""
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	secParts := strings.SplitN(parts[2], ".", 2)
	s, err3 := strconv.Atoi(secParts[0])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	ms := 0
	if len(secParts) == 2 {
		// Centiseconds (2 digits) in ASS; pad/truncate to milliseconds.
		frac := secParts[1]
		for len(frac) < 3 {
			frac += "0"
		}
		frac = frac[:3]
		ms, _ = strconv.Atoi(frac)
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// cleanASSText strips ASS inline styling to leave plain cue text: remove every
// {…} override block, turn the hard-break escapes \N and \n into newlines and the
// non-breaking-space escape \h into a space, then trim. The result is the
// dialogue only — the deliberate styling downgrade.
func cleanASSText(t string) string {
	t = assOverrideRe.ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, `\N`, "\n")
	t = strings.ReplaceAll(t, `\n`, "\n")
	t = strings.ReplaceAll(t, `\h`, " ")
	// Collapse a run of spaces a stripped tag may have left, per line.
	outLines := make([]string, 0)
	for _, ln := range strings.Split(t, "\n") {
		outLines = append(outLines, strings.TrimSpace(ln))
	}
	return strings.TrimSpace(strings.Join(outLines, "\n"))
}

// --- shared helpers --------------------------------------------------------

// normalizeTiming reformats a SubRip/WebVTT timing line to the canonical WebVTT
// "HH:MM:SS.mmm --> HH:MM:SS.mmm", dropping any trailing SRT coordinates / cue
// settings. The line is known to match timingRe.
func normalizeTiming(line string) string {
	m := timingRe.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	return vttTimestamp(m[1], m[2], m[3], m[4]) + " --> " + vttTimestamp(m[5], m[6], m[7], m[8])
}

// vttTimestamp assembles a WebVTT timestamp from optional-hour, minute, second,
// and fractional groups, zero-padding hours (defaulted to 0) and the fraction to
// milliseconds.
func vttTimestamp(h, m, s, frac string) string {
	if h == "" {
		h = "0"
	}
	for len(frac) < 3 {
		frac += "0"
	}
	frac = frac[:3]
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	si, _ := strconv.Atoi(s)
	return fmt.Sprintf("%02d:%02d:%02d.%s", hi, mi, si, frac)
}

// splitLines splits input into lines on \n, stripping a trailing \r (CRLF) and a
// leading UTF-8 BOM on the first line, so Windows-authored and BOM-prefixed files
// parse cleanly.
func splitLines(data []byte) []string {
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long cue lines
	for sc.Scan() {
		out = append(out, strings.TrimRight(sc.Text(), "\r"))
	}
	return out
}

// collapseBlankRun collapses any run of 2+ blank lines to a single blank line, so
// dropping SRT counters never leaves a widening gap between cues.
func collapseBlankRun(s string) string {
	var b strings.Builder
	blank := 0
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// ensureTrailingNewline guarantees exactly one trailing newline.
func ensureTrailingNewline(s string) string {
	return strings.TrimRight(s, "\n") + "\n"
}
