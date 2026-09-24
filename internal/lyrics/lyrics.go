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

// maxLineMs bounds a line's start time. No song runs a day; a timestamp past it
// is a corrupt or hostile file, and dropping the line keeps every StartMs well
// inside what a JavaScript number holds exactly.
const maxLineMs = 24 * 60 * 60 * 1000

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
	for _, ext := range []string{".lrc", ".LRC"} {
		f, err := os.Open(stem + ext)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
		_ = f.Close()
		if err != nil || len(data) > maxSidecarBytes {
			return Lyrics{}, false
		}
		text, ok := sidecarText(data)
		if !ok {
			return Lyrics{}, false
		}
		return ParseLRC(text)
	}
	return Lyrics{}, false
}

// sidecarText decodes a sidecar's bytes: UTF-16 in either byte order when it
// opens with a byte-order mark (as Windows tools write it), UTF-8 otherwise.
// Text that is still not valid UTF-8, or holds a NUL, is not a lyrics file and
// reports false, so the track's own tags are read instead.
func sidecarText(data []byte) (string, bool) {
	text := string(data)
	if len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff) {
		text, _ = decodeText(1, data, nil)
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return "", false
	}
	return text, true
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
