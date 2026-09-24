// Package markers holds the Marker vocabulary (CONTEXT.md "Marker", ADR-0065)
// and the pure readers that turn a File's own chapters or its edit-decision
// sidecar (`.edl`) into Local markers. It touches no disk and no database: the
// Scanner hands it bytes and chapter titles, and stores what comes back.
//
// A Marker is a timed span of a File of one kind — Intro, Recap, Credits or
// Preview. There is deliberately no Commercial kind: nothing here could find one
// honestly, so a span that names nothing else is dropped, never guessed.
package markers

import (
	"bufio"
	"bytes"
	"math"
	"sort"
	"strconv"
	"strings"
)

// The closed set of Marker kinds, as stored and as sent on the wire.
const (
	KindIntro   = "intro"
	KindRecap   = "recap"
	KindCredits = "credits"
	KindPreview = "preview"
)

// The Marker sources, ranked Local > Detected > Fetched (ADR-0065 §1). Only
// Local is produced today; the other two are named so the column's vocabulary
// is settled before anything writes them.
const (
	SourceLocal    = "local"
	SourceDetected = "detected"
	SourceFetched  = "fetched"
)

// Span is one Marker as the readers produce it: a kind and a half-open
// [StartMs, EndMs) span on the File's own timeline.
type Span struct {
	Kind    string
	StartMs int64
	EndMs   int64
}

// Chapter is one embedded chapter as ffprobe reports it.
type Chapter struct {
	StartMs int64
	EndMs   int64
	Title   string
}

// Classify names the Marker kind a chapter title or `.edl` label describes, or
// "" when it describes none of the four. The rules are ordered: "Opening
// Credits" is an Intro, not Credits, and "Previously on…" is a Recap even though
// it mentions nothing about recaps.
func Classify(label string) string {
	words := strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	has := func(ws ...string) bool {
		for _, w := range words {
			for _, want := range ws {
				if w == want {
					return true
				}
			}
		}
		return false
	}
	// "OP"/"ED" are the anime chapter names for the opening and ending, but as a
	// word inside a longer title they are as likely to be somebody called Ed, so
	// they count only as the whole label.
	whole := strings.Join(words, " ")
	switch {
	case has("recap", "previously"):
		return KindRecap
	case has("preview", "previews") || has("next") && has("episode", "time", "week"):
		return KindPreview
	case has("intro", "introduction", "opening") || whole == "op":
		return KindIntro
	case has("credits", "ending", "outro", "endcredits") || whole == "ed":
		return KindCredits
	}
	return ""
}

// FromChapters returns the Local markers a File's chapters identify: every
// chapter whose title Classify recognizes. A chapter named "Chapter 3" names
// nothing and is skipped.
func FromChapters(chapters []Chapter, durationMs int64) []Span {
	var out []Span
	for _, c := range chapters {
		kind := Classify(c.Title)
		if kind == "" {
			continue
		}
		if s, ok := clamp(Span{Kind: kind, StartMs: c.StartMs, EndMs: c.EndMs}, durationMs); ok {
			out = append(out, s)
		}
	}
	return sorted(out)
}

// ParseEDL reads an edit-decision list: one span per line, `start end [action]
// [label]`, times in seconds. The classic format (MPlayer, Kodi) carries only an
// action number — cut, mute, scene, commercial — which says nothing about WHAT
// the span is, so a line is kept only when its label (anything after the action,
// with an optional leading `#`) names one of the four kinds:
//
//	0.0     84.5    0   Intro
//	1290.2  1380.0  3   # Credits
//
// A line with no label, a label naming nothing, or times that do not parse is
// skipped rather than failing the whole file.
func ParseEDL(data []byte, durationMs int64) []Span {
	var out []Span
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		start, errS := strconv.ParseFloat(fields[0], 64)
		end, errE := strconv.ParseFloat(fields[1], 64)
		if errS != nil || errE != nil || math.IsNaN(start) || math.IsNaN(end) {
			continue
		}
		rest := fields[2:]
		if _, err := strconv.Atoi(rest[0]); err == nil {
			rest = rest[1:] // the action number
		}
		kind := Classify(strings.TrimPrefix(strings.Join(rest, " "), "#"))
		if kind == "" {
			continue
		}
		span := Span{Kind: kind, StartMs: int64(start * 1000), EndMs: int64(end * 1000)}
		if s, ok := clamp(span, durationMs); ok {
			out = append(out, s)
		}
	}
	return sorted(out)
}

// clamp trims a span to the File and refuses one that is empty, inverted, or
// starts past the end. A durationMs of 0 (unknown) trims nothing.
func clamp(s Span, durationMs int64) (Span, bool) {
	if s.StartMs < 0 {
		s.StartMs = 0
	}
	if durationMs > 0 {
		if s.StartMs >= durationMs {
			return Span{}, false
		}
		if s.EndMs > durationMs {
			s.EndMs = durationMs
		}
	}
	if s.EndMs <= s.StartMs {
		return Span{}, false
	}
	return s, true
}

func sorted(spans []Span) []Span {
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartMs < spans[j].StartMs })
	return spans
}
