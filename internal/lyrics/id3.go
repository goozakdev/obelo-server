package lyrics

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ID3v2 lyrics frames. ffprobe does not surface SYLT at all, and hands a USLT
// back only as a flattened "lyrics-<lang>" tag, so the two frames are read here
// straight from the tag at the head of the file. Only what these two frames need
// is parsed: ID3v2.2, v2.3 and v2.4 headers (v2.2's SLT and ULT are the same
// frames under three-character ids), extended headers, tag- and frame-level
// unsynchronisation and v2.4 data-length indicators. A compressed or encrypted
// frame or tag, a SYLT timed in MPEG frames rather than milliseconds, or a UTF-16
// frame holding a lone surrogate, a UTF-8 frame holding invalid bytes, or a frame
// whose text-encoding byte is not one ID3 defines is skipped — it reads as "no
// lyrics there", never as an error.

// maxID3Frame bounds one lyrics frame read. The words to a song are kilobytes;
// the cap only stops a corrupt size field from allocating the file.
const maxID3Frame = 1 << 20

// maxID3UnsyncTag bounds the one case that reads a whole tag into memory: a v2.3
// tag unsynchronised as a unit, which has to be decoded before its frames can be
// found. Embedded cover art makes such tags large, so past this the file's
// lyrics frames are not looked for.
const maxID3UnsyncTag = 32 << 20

// readID3File returns the SYLT lines and USLT text of the ID3v2 tag at the head
// of the file at path. Both are empty when the file has no tag, no such frame, or
// cannot be read.
func readID3File(path string) ([]Line, string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	return readID3(f)
}

// readID3 is readID3File over any random-access reader.
func readID3(r io.ReaderAt) (sylt []Line, uslt string) {
	var hdr [10]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil || string(hdr[:3]) != "ID3" {
		return nil, ""
	}
	major, flags := hdr[3], hdr[5]
	if major < 2 || major > 4 {
		return nil, ""
	}
	// v2.2's second flag marks the whole tag compressed, in a scheme it never
	// defined.
	if major == 2 && flags&0x40 != 0 {
		return nil, ""
	}
	size := int64(syncsafe(hdr[6:10]))

	// A v2.2 or v2.3 tag unsynchronised as a whole is decoded first; v2.4 marks
	// it on each frame instead.
	var tag io.ReadSeeker = io.NewSectionReader(r, 10, size)
	if major < 4 && flags&0x80 != 0 {
		if size > maxID3UnsyncTag {
			return nil, ""
		}
		raw := make([]byte, size)
		if _, err := io.ReadFull(tag, raw); err != nil {
			return nil, ""
		}
		tag = bytes.NewReader(unsync(raw))
		size = int64(tag.(*bytes.Reader).Len())
	}

	if major > 2 && flags&0x40 != 0 {
		var ext [4]byte
		if _, err := io.ReadFull(tag, ext[:]); err != nil {
			return nil, ""
		}
		// v2.3's size excludes its own four bytes; v2.4's includes them.
		skip := int64(binary.BigEndian.Uint32(ext[:]))
		if major == 4 {
			skip = int64(syncsafe(ext[:])) - 4
		}
		if _, err := tag.Seek(skip, io.SeekCurrent); err != nil {
			return nil, ""
		}
	}

	headerLen := int64(10)
	if major == 2 {
		headerLen = 6
	}
	for {
		pos, _ := tag.Seek(0, io.SeekCurrent)
		if pos+headerLen > size {
			break
		}
		fh := make([]byte, headerLen)
		if _, err := io.ReadFull(tag, fh); err != nil || fh[0] == 0 {
			break // padding, or the end of what could be read
		}
		id, n, format := frameHeader(major, fh)
		if n < 0 || pos+headerLen+n > size {
			break
		}
		wanted := (id == "SYLT" && sylt == nil) || (id == "USLT" && uslt == "")
		if !wanted || n > maxID3Frame {
			if _, err := tag.Seek(n, io.SeekCurrent); err != nil {
				break
			}
			continue
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(tag, body); err != nil {
			break
		}
		body, ok := frameBody(major, format, body)
		if !ok {
			continue
		}
		switch id {
		case "SYLT":
			sylt = decodeSYLT(body)
		case "USLT":
			uslt = decodeUSLT(body)
		}
		if sylt != nil && uslt != "" {
			break
		}
	}
	return sylt, uslt
}

// v22IDs names the v2.2 lyrics frames by their v2.3 ids.
var v22IDs = map[string]string{"SLT": "SYLT", "ULT": "USLT"}

// frameHeader reads a frame header: the frame's id as v2.3 and v2.4 name it, its
// body's size and its format flags. A v2.2 header is a three-character id and a
// three-byte size, with no flags.
func frameHeader(major byte, fh []byte) (string, int64, byte) {
	switch major {
	case 2:
		return v22IDs[string(fh[:3])], int64(fh[3])<<16 | int64(fh[4])<<8 | int64(fh[5]), 0
	case 3:
		return string(fh[:4]), int64(binary.BigEndian.Uint32(fh[4:8])), fh[9]
	}
	return string(fh[:4]), int64(syncsafe(fh[4:8])), fh[9]
}

// frameBody undoes what a frame's format flags did to its body, or reports false
// for a frame that cannot be read here (compressed, encrypted).
func frameBody(major, format byte, body []byte) ([]byte, bool) {
	if major == 3 {
		if format&0xc0 != 0 { // compression, encryption
			return nil, false
		}
		if format&0x20 != 0 { // grouping identity byte
			if len(body) < 1 {
				return nil, false
			}
			body = body[1:]
		}
		return body, true
	}
	if format&0x0c != 0 { // compression, encryption
		return nil, false
	}
	if format&0x40 != 0 { // grouping identity byte
		if len(body) < 1 {
			return nil, false
		}
		body = body[1:]
	}
	if format&0x02 != 0 {
		body = unsync(body)
	}
	if format&0x01 != 0 { // data length indicator
		if len(body) < 4 {
			return nil, false
		}
		body = body[4:]
	}
	return body, true
}

// decodeUSLT reads a USLT body: encoding, language, a terminated descriptor,
// then the text. A frame holding text that does not decode is not read.
func decodeUSLT(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	enc := b[0]
	rest := b[4:]
	desc, rest, ok := cutString(enc, rest, nil)
	if !ok {
		return ""
	}
	if _, _, ok := decodeText(enc, desc, nil); !ok {
		return ""
	}
	text, _, ok := decodeText(enc, trimTerminator(enc, rest), nil)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

// decodeSYLT reads a SYLT body — encoding, language, timestamp format, content
// type, a terminated descriptor, then (text, 32-bit time) pairs — into Lines.
//
// Taggers write SYLT two ways: one entry per line, or one entry per syllable
// with each new line's first syllable starting with a newline. Either way the
// result is one Line per line of the song, starting when its first entry does,
// in time order whatever order the frame lists them in; a line starting past
// maxLineMs is dropped. A frame holding text that does not decode is not read.
func decodeSYLT(b []byte) []Line {
	if len(b) < 6 {
		return nil
	}
	enc, format, content := b[0], b[4], b[5]
	// Only millisecond timing can be followed without decoding the audio, and only
	// the lyric-bearing content types (other, lyrics, transcription) are words.
	if format != 2 || content > 2 {
		return nil
	}
	var bom []byte
	desc, rest, ok := cutString(enc, b[6:], &bom)
	if !ok {
		return nil
	}
	if _, _, ok := decodeText(enc, desc, nil); !ok {
		return nil
	}

	type entry struct {
		ms   int64
		text string
	}
	var entries []entry
	syllables := false
	for len(rest) > 0 {
		var raw []byte
		raw, rest, ok = cutString(enc, rest, nil)
		if !ok || len(rest) < 4 {
			break
		}
		text, next, good := decodeText(enc, raw, bom)
		if !good {
			return nil
		}
		bom = next
		ms := int64(binary.BigEndian.Uint32(rest[:4]))
		rest = rest[4:]
		if strings.HasPrefix(text, "\n") || strings.HasPrefix(text, "\r") {
			syllables = true
		}
		entries = append(entries, entry{ms: ms, text: text})
	}

	var lines []Line
	for i, e := range entries {
		startsLine := !syllables || i == 0 || strings.HasPrefix(e.text, "\n") || strings.HasPrefix(e.text, "\r")
		words := strings.Trim(e.text, "\r\n")
		if startsLine {
			lines = append(lines, Line{StartMs: e.ms, Text: words})
			continue
		}
		lines[len(lines)-1].Text += words
	}
	kept := lines[:0]
	for _, l := range lines {
		if l.StartMs <= maxLineMs {
			l.Text = strings.TrimSpace(l.Text)
			kept = append(kept, l)
		}
	}
	lines = kept
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].StartMs < lines[j].StartMs })
	if len(lines) == 0 {
		return nil
	}
	return lines
}

// cutString splits a terminated string off the front of b in encoding enc,
// returning the raw string bytes and what follows the terminator. When bom is
// non-nil and the string opens with a UTF-16 byte-order mark, the mark is
// recorded there for strings that omit their own.
func cutString(enc byte, b []byte, bom *[]byte) ([]byte, []byte, bool) {
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(b); i += 2 {
			if b[i] == 0 && b[i+1] == 0 {
				s := b[:i]
				if bom != nil && len(s) >= 2 && (s[0] == 0xff && s[1] == 0xfe || s[0] == 0xfe && s[1] == 0xff) {
					*bom = s[:2]
				}
				return s, b[i+2:], true
			}
		}
		return nil, nil, false
	}
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return nil, nil, false
	}
	return b[:i], b[i+1:], true
}

// trimTerminator drops a trailing terminator from a string that runs to the end
// of its frame, where the terminator is optional.
func trimTerminator(enc byte, b []byte) []byte {
	if enc == 1 || enc == 2 {
		for len(b) >= 2 && b[len(b)-2] == 0 && b[len(b)-1] == 0 {
			b = b[:len(b)-2]
		}
		return b
	}
	return bytes.TrimRight(b, "\x00")
}

// decodeText decodes one string in an ID3 text encoding: 0 Latin-1, 1 UTF-16
// with a byte-order mark, 2 UTF-16BE, 3 UTF-8. A UTF-16 string without its own
// mark uses bom, the last one seen; the mark in force is returned. UTF-16
// holding a lone surrogate, UTF-8 holding invalid bytes, or an encoding byte
// ID3 does not define reports false: it is damaged, and decoding it would put
// U+FFFD or guessed text in the words.
func decodeText(enc byte, b []byte, bom []byte) (string, []byte, bool) {
	switch enc {
	case 0:
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r), bom, true
	case 1, 2:
		bigEndian := enc == 2
		if enc == 1 {
			if len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff) {
				bom = b[:2]
				b = b[2:]
			}
			bigEndian = len(bom) == 2 && bom[0] == 0xfe
		}
		if !wellFormedUTF16(b, bigEndian) {
			return "", bom, false
		}
		u := make([]uint16, len(b)/2)
		for i := range u {
			if bigEndian {
				u[i] = binary.BigEndian.Uint16(b[2*i:])
			} else {
				u[i] = binary.LittleEndian.Uint16(b[2*i:])
			}
		}
		return string(utf16.Decode(u)), bom, true
	case 3:
		if !utf8.Valid(b) {
			return "", bom, false
		}
		return string(b), bom, true
	default:
		return "", bom, false
	}
}

// syncsafe reads a 28-bit ID3 syncsafe integer (7 bits per byte).
func syncsafe(b []byte) uint32 {
	return uint32(b[0]&0x7f)<<21 | uint32(b[1]&0x7f)<<14 | uint32(b[2]&0x7f)<<7 | uint32(b[3]&0x7f)
}

// unsync reverses ID3 unsynchronisation: every 0xFF 0x00 pair was a lone 0xFF.
func unsync(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{0xff, 0x00}, []byte{0xff})
}
