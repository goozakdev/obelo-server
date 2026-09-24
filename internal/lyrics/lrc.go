package lyrics

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// lrcTimeRe matches one LRC line timestamp: [mm:ss], [mm:ss.x], [mm:ss.xx] or
// [mm:ss.xxx], with ':' accepted in place of '.' as some writers use it.
var lrcTimeRe = regexp.MustCompile(`^\[(\d+):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// lrcTagRe matches an LRC header tag line ([ar:Artist], [offset:+250]). It is
// metadata about the file, never a line of the words. Only the tags the format
// defines match, so a plain "[Chorus: both]" stays a line of the song.
var lrcTagRe = regexp.MustCompile(`(?i)^\[(ar|al|ti|au|by|length|offset|re|ve|tool|#):([^\]]*)\]\s*$`)

// lrcWordTimeRe matches the per-word timestamps of "enhanced" LRC
// (<mm:ss.xx>). The line keeps its own time; the word timings are dropped.
var lrcWordTimeRe = regexp.MustCompile(`<\d+:\d{1,2}(?:[.:]\d{1,3})?>`)

// ParseLRC reads text that may be LRC. When at least one line carries a
// timestamp the result is Synced: one Line per timestamp (a line stamped twice
// appears twice), ordered by time, with the [offset:] header applied. Otherwise
// it is Plain: the text with any LRC header tags removed. A line stamped past
// maxLineMs is dropped, and an offset beyond it ignored. Text with no words at
// all is not lyrics and reports false.
func ParseLRC(text string) (Lyrics, bool) {
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var offsetMs int64
	var lines []Line
	var plain []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if m := lrcTagRe.FindStringSubmatch(line); m != nil {
			if strings.EqualFold(m[1], "offset") {
				if n, err := strconv.ParseInt(strings.TrimSpace(m[2]), 10, 64); err == nil && n >= -maxLineMs && n <= maxLineMs {
					offsetMs = n
				}
			}
			continue
		}
		var stamps []int64
		for {
			m := lrcTimeRe.FindStringSubmatch(line)
			if m == nil {
				break
			}
			stamps = append(stamps, lrcMs(m[1], m[2], m[3]))
			line = strings.TrimSpace(line[len(m[0]):])
		}
		if len(stamps) == 0 {
			plain = append(plain, strings.TrimRightFunc(raw, isSpace))
			continue
		}
		words := strings.TrimSpace(lrcWordTimeRe.ReplaceAllString(line, ""))
		for _, ms := range stamps {
			lines = append(lines, Line{StartMs: ms, Text: words})
		}
	}

	// A positive offset shows the words sooner (the LRC convention).
	kept := lines[:0]
	for _, l := range lines {
		if l.StartMs > maxLineMs {
			continue
		}
		l.StartMs = max(l.StartMs-offsetMs, 0)
		if l.StartMs <= maxLineMs {
			kept = append(kept, l)
		}
	}
	lines = kept
	if len(lines) > 0 {
		sort.SliceStable(lines, func(i, j int) bool { return lines[i].StartMs < lines[j].StartMs })
		return Lyrics{Kind: Synced, Lines: lines}, true
	}
	body := strings.Trim(strings.Join(plain, "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		return Lyrics{}, false
	}
	return Lyrics{Kind: Plain, Text: body}, true
}

// lrcMs converts a timestamp's minute, second and fraction fields to
// milliseconds. The fraction is read as decimal digits of a second, so ".5",
// ".50" and ".500" are all half a second. A minute count too large to be a
// timestamp reads as just past maxLineMs rather than overflowing.
func lrcMs(min, sec, frac string) int64 {
	m, err := strconv.ParseInt(min, 10, 64)
	if err != nil || m > maxLineMs/60_000 {
		return maxLineMs + 1
	}
	s, _ := strconv.ParseInt(sec, 10, 64)
	var f int64
	if frac != "" {
		f, _ = strconv.ParseInt((frac + "00")[:3], 10, 64)
	}
	return m*60_000 + s*1000 + f
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' }
