// Package lyrics reads a track's Local lyrics: the words the Scanner finds in
// the file itself or beside it, before any Lyric provider is asked. It knows
// three sources — a sidecar .lrc file, an embedded ID3 SYLT (synced) or USLT
// (plain) frame, and a LYRICS-style tag a container surfaces through ffprobe —
// and turns each into one of the two shapes CONTEXT.md defines: Synced lyrics
// (every line timed, so a player can follow along) or Plain lyrics (just the
// text).
//
// Everything here is local and offline: it reads bytes already on disk and
// decides nothing about identity. The Scanner calls Local once per audio file and
// stores what it returns against the Track.
package lyrics

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Kind says which of the two shapes a set of lyrics has.
type Kind string

const (
	// Synced lyrics are timed line by line (Lyrics.Lines).
	Synced Kind = "synced"
	// Plain lyrics are untimed text (Lyrics.Text).
	Plain Kind = "plain"
)

// Line is one timed line of Synced lyrics: the text shown from StartMs until the
// next line begins. An empty Text is a deliberate gap (an instrumental break).
type Line struct {
	StartMs int64
	Text    string
}

// Lyrics is one track's words in one shape: Lines when Kind is Synced, Text when
// it is Plain. The other field is always empty.
type Lyrics struct {
	Kind  Kind
	Lines []Line
	Text  string
}

// maxSidecarBytes bounds a sidecar .lrc read. A whole album's worth of timed
// lines is a few kilobytes; anything past this is not a lyrics file.
const maxSidecarBytes = 1 << 20

// maxLineMs bounds a line's start time: the largest integer a JavaScript number
// holds exactly, so every StartMs a client reads is the one stored. It is not a
// length any recording has — an audiobook runs past a day, and its lines are
// kept — so a timestamp past it is a corrupt or hostile file, and is dropped.
const maxLineMs = 1<<53 - 1

// MaxLineMs is maxLineMs for lines this package did not read: a Lyric
// provider's answer is held to the same bound.
const MaxLineMs = maxLineMs

// Local returns the Local lyrics for the audio file at path, whose ffprobe tags
// (lower-cased keys) are tags, and false when it has none.
//
// Synced outranks Plain whatever the source, because a timed line carries
// everything an untimed one does. Within a shape the sidecar outranks the file's
// own tags: it is what someone put beside the track on purpose. So the order is
// sidecar .lrc, embedded SYLT, then any plain source whose text turns out to be
// LRC-timed, and only then the first plain text found. A source that cannot be
// read is skipped, never fatal: a track without readable lyrics has none.
func Local(path string, tags map[string]string) (Lyrics, bool) {
	var found []Lyrics
	if l, ok := readSidecar(path); ok {
		found = append(found, l)
	}
	sylt, uslt := readID3File(path)
	if len(sylt) > 0 {
		found = append(found, Lyrics{Kind: Synced, Lines: sylt})
	}
	for _, text := range append([]string{uslt}, tagLyrics(tags)...) {
		if l, ok := ParseLRC(text); ok {
			found = append(found, l)
		}
	}
	for _, kind := range []Kind{Synced, Plain} {
		for _, l := range found {
			if l.Kind == kind {
				return l, true
			}
		}
	}
	return Lyrics{}, false
}

// readSidecar reads the .lrc beside path: the same name with the extension
// swapped, in either case.
func readSidecar(path string) (Lyrics, bool) {
	stem := strings.TrimSuffix(path, filepath.Ext(path))
	return firstSidecar(stem+".lrc", stem+".LRC")
}

// openSidecar opens a sidecar candidate for reading.
var openSidecar = func(name string) (io.ReadCloser, error) { return os.Open(name) }

// firstSidecar reads the first of names that holds lyrics. One that is missing,
// too large or not text is passed over for the next: on a case-sensitive
// filesystem the .LRC beside an unreadable .lrc is another file.
func firstSidecar(names ...string) (Lyrics, bool) {
	for _, name := range names {
		f, err := openSidecar(name)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
		_ = f.Close()
		if err != nil || len(data) > maxSidecarBytes {
			continue
		}
		text, ok := sidecarText(data)
		if !ok {
			continue
		}
		if l, ok := ParseLRC(text); ok {
			return l, true
		}
	}
	return Lyrics{}, false
}

// sidecarText decodes a sidecar's bytes: UTF-16 in either byte order when it
// opens with a byte-order mark (as Windows tools write it), UTF-8 when it is
// valid UTF-8, and Windows-1252 otherwise — what older collections were written
// in, and a superset of Latin-1. UTF-16 holding a lone surrogate, bytes
// Windows-1252 leaves undefined, or a NUL mean it is not a lyrics file, and
// report false, so the track's own tags are read instead.
func sidecarText(data []byte) (string, bool) {
	var text string
	switch {
	case len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff):
		if !wellFormedUTF16(data[2:], data[0] == 0xfe) {
			return "", false
		}
		text, _ = decodeText(1, data, nil)
	case utf8.Valid(data):
		text = string(data)
	default:
		var ok bool
		if text, ok = decodeWindows1252(data); !ok {
			return "", false
		}
	}
	if strings.ContainsRune(text, 0) {
		return "", false
	}
	return text, true
}

// wellFormedUTF16 reports whether b, as UTF-16 in the given byte order, pairs
// every surrogate. A lone one is a damaged file, not a character.
func wellFormedUTF16(b []byte, bigEndian bool) bool {
	pending := false // a high surrogate waiting for its low half
	for i := 0; i+1 < len(b); i += 2 {
		u := uint16(b[i]) | uint16(b[i+1])<<8
		if bigEndian {
			u = uint16(b[i])<<8 | uint16(b[i+1])
		}
		high, low := u >= 0xd800 && u < 0xdc00, u >= 0xdc00 && u < 0xe000
		if pending != low {
			return false
		}
		pending = high
	}
	return !pending
}

// windows1252 is what Windows-1252 puts at 0x80–0x9F, where it differs from
// Latin-1; 0 marks the five bytes it leaves undefined.
var windows1252 = [32]rune{
	'€', 0, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0, 'Ž', 0,
	0, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0, 'ž', 'Ÿ',
}

// decodeWindows1252 decodes b as Windows-1252, and reports false for a byte it
// leaves undefined: text holding one was not written in it.
func decodeWindows1252(b []byte) (string, bool) {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
		if c >= 0x80 && c < 0xa0 {
			if r[i] = windows1252[c-0x80]; r[i] == 0 {
				return "", false
			}
		}
	}
	return string(r), true
}

// tagLyrics is every lyrics-bearing tag ffprobe surfaced, in a stable order:
// "lyrics" (Vorbis LYRICS, MP4 ©lyr), "unsyncedlyrics" (the Vorbis spelling some
// taggers use), then ffprobe's "lyrics-<lang>" keys for a USLT it read itself.
func tagLyrics(tags map[string]string) []string {
	var out []string
	for _, k := range []string{"lyrics", "unsyncedlyrics"} {
		if v := tags[k]; v != "" {
			out = append(out, v)
		}
	}
	var langs []string
	for k := range tags {
		if strings.HasPrefix(k, "lyrics-") {
			langs = append(langs, k)
		}
	}
	sort.Strings(langs)
	for _, k := range langs {
		out = append(out, tags[k])
	}
	return out
}
