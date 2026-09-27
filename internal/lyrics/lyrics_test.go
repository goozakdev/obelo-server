package lyrics

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestParseLRC(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Lyrics
		ok   bool
	}{
		{
			name: "timed lines are synced, header tags are not lines",
			in:   "[ar:Band]\n[ti:Song]\n[00:01.50]One\n[00:03.25]Two\n",
			want: Lyrics{Kind: Synced, Lines: []Line{{1500, "One"}, {3250, "Two"}}},
			ok:   true,
		},
		{
			name: "fraction digits are decimal seconds; minutes roll over",
			in:   "[00:01.5]a\n[00:02.123]b\n[01:02]c",
			want: Lyrics{Kind: Synced, Lines: []Line{{1500, "a"}, {2123, "b"}, {62000, "c"}}},
			ok:   true,
		},
		{
			name: "a line stamped twice appears twice, in time order",
			in:   "[00:10.00][00:02.00]Chorus\n[00:05.00]Verse",
			want: Lyrics{Kind: Synced, Lines: []Line{{2000, "Chorus"}, {5000, "Verse"}, {10000, "Chorus"}}},
			ok:   true,
		},
		{
			name: "a positive offset shows the words sooner, never before zero",
			in:   "[offset:+500]\n[00:00.20]a\n[00:02.00]b",
			want: Lyrics{Kind: Synced, Lines: []Line{{0, "a"}, {1500, "b"}}},
			ok:   true,
		},
		{
			name: "enhanced word timings are dropped, an empty timed line is a gap",
			in:   "\ufeff[00:01.00]<00:01.00>Hello <00:01.50>there\r\n[00:04.00]\r\n",
			want: Lyrics{Kind: Synced, Lines: []Line{{1000, "Hello there"}, {4000, ""}}},
			ok:   true,
		},
		{
			name: "untimed text is plain, keeping its stanza breaks and bracketed cues",
			in:   "\n[Chorus: both]\nLine one\n\nLine two  \n\n",
			want: Lyrics{Kind: Plain, Text: "[Chorus: both]\nLine one\n\nLine two"},
			ok:   true,
		},
		{
			name: "header tags alone are not lyrics",
			in:   "[ar:Band]\n[ti:Song]\n",
			ok:   false,
		},
		{name: "blank is not lyrics", in: "  \n\t\n", ok: false},
		{
			name: "lines stamped past what a client holds exactly are dropped, never overflowing",
			in:   "[99999999999999999999:00.00]overflow\n[153722867280912:55.81]wraps\n[00:01.00]kept",
			want: Lyrics{Kind: Synced, Lines: []Line{{1000, "kept"}}},
			ok:   true,
		},
		{
			name: "a line stamped past a day is kept, as in an audiobook",
			in:   "[1441:00.00]late\n[3000:00.50]later\n[00:01.00]early",
			want: Lyrics{Kind: Synced, Lines: []Line{{1000, "early"}, {86_460_000, "late"}, {180_000_500, "later"}}},
			ok:   true,
		},
		{
			name: "bare CR line endings end lines",
			in:   "[00:01.00]One\r[00:02.00]Two\rThree",
			want: Lyrics{Kind: Synced, Lines: []Line{{1000, "One"}, {2000, "Two"}}},
			ok:   true,
		},
		{
			name: "an offset past what a client holds exactly is ignored",
			in:   "[offset:-9000000000000000000]\n[00:01.00]a",
			want: Lyrics{Kind: Synced, Lines: []Line{{1000, "a"}}},
			ok:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseLRC(tc.in)
			if ok != tc.ok || (ok && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("ParseLRC = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// --- ID3 fixtures -------------------------------------------------------------

func be32(n int) []byte { return binary.BigEndian.AppendUint32(nil, uint32(n)) }

func ss32(n int) []byte {
	return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
}

// tag builds an ID3v2 tag of the given major version and flags around frames.
func tag(major, flags byte, body []byte) []byte {
	return append(append([]byte{'I', 'D', '3', major, 0, flags}, ss32(len(body))...), body...)
}

// frame builds one frame; v2.4 sizes are syncsafe, v2.3 sizes are plain.
func frame(major byte, id string, format byte, body []byte) []byte {
	size := be32(len(body))
	if major == 4 {
		size = ss32(len(body))
	}
	f := append([]byte(id), size...)
	f = append(f, 0, format)
	return append(f, body...)
}

// frame22 builds one v2.2 frame: a three-character id and a three-byte size.
func frame22(id string, body []byte) []byte {
	return append(append([]byte(id), be32(len(body))[1:]...), body...)
}

func latin1SYLT(format, content byte, entries ...any) []byte {
	b := []byte{0, 'e', 'n', 'g', format, content, 0}
	for i := 0; i < len(entries); i += 2 {
		b = append(b, entries[i].(string)...)
		b = append(b, 0)
		b = append(b, be32(entries[i+1].(int))...)
	}
	return b
}

func utf16LE(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = []byte{0xff, 0xfe}
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

func utf16BE(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = []byte{0xfe, 0xff}
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

func TestReadID3(t *testing.T) {
	usltBody := append([]byte{0, 'e', 'n', 'g', 'd', 'e', 's', 'c', 0}, "Plain\nwords\x00"...)

	// UTF-16 SYLT in the syllable style: a newline opens each line, and only the
	// descriptor carries a byte-order mark.
	syllables := []byte{1, 'e', 'n', 'g', 2, 1}
	syllables = append(syllables, utf16LE("desc", true)...)
	syllables = append(syllables, 0, 0)
	for _, e := range []struct {
		s  string
		ms int
	}{{"Ca", 100}, {"fé ", 300}, {"\nnoir", 900}} {
		syllables = append(syllables, utf16LE(e.s, false)...)
		syllables = append(syllables, 0, 0)
		syllables = append(syllables, be32(e.ms)...)
	}

	// UTF-16 SYLT whose descriptor is little-endian and whose first entry opens
	// with a big-endian mark: the entry after it is big-endian too, though it
	// does not say so — the last mark seen is the one in force.
	beSYLT := []byte{1, 'e', 'n', 'g', 2, 1}
	beSYLT = append(beSYLT, utf16LE("desc", true)...)
	beSYLT = append(beSYLT, 0, 0)
	for _, e := range []struct {
		s   string
		ms  int
		bom bool
	}{{"Café", 500, true}, {"noir", 1500, false}} {
		beSYLT = append(beSYLT, utf16BE(e.s, e.bom)...)
		beSYLT = append(beSYLT, 0, 0)
		beSYLT = append(beSYLT, be32(e.ms)...)
	}

	cases := []struct {
		name     string
		data     []byte
		wantSylt []Line
		wantUslt string
	}{
		{
			name: "v2.3 SYLT and USLT beside other frames",
			data: tag(3, 0, bytes.Join([][]byte{
				frame(3, "TIT2", 0, []byte("\x00Song")),
				frame(3, "SYLT", 0, latin1SYLT(2, 1, "One", 500, "Two", 2750)),
				frame(3, "USLT", 0, usltBody),
			}, nil)),
			wantSylt: []Line{{500, "One"}, {2750, "Two"}},
			wantUslt: "Plain\nwords",
		},
		{
			name:     "v2.4 syncsafe frame sizes, UTF-16 syllables grouped into lines",
			data:     tag(4, 0, frame(4, "SYLT", 0, syllables)),
			wantSylt: []Line{{100, "Café"}, {900, "noir"}},
		},
		{
			name:     "SYLT entries out of order are read in time order",
			data:     tag(3, 0, frame(3, "SYLT", 0, latin1SYLT(2, 1, "Three", 3000, "One", 1000, "Two", 2000))),
			wantSylt: []Line{{1000, "One"}, {2000, "Two"}, {3000, "Three"}},
		},
		{
			name:     "a SYLT entry stamped past a day is kept",
			data:     tag(3, 0, frame(3, "SYLT", 0, latin1SYLT(2, 1, "Late", 0xffffffff, "Kept", 1000))),
			wantSylt: []Line{{1000, "Kept"}, {0xffffffff, "Late"}},
		},
		{
			name:     "a v2.3 grouping identity byte is stripped",
			data:     tag(3, 0, frame(3, "USLT", 0x20, append([]byte{0x02}, usltBody...))),
			wantUslt: "Plain\nwords",
		},
		{
			name:     "SYLT syllables opening their lines with a bare CR",
			data:     tag(3, 0, frame(3, "SYLT", 0, latin1SYLT(2, 1, "One", 100, " more", 200, "\rTwo", 900))),
			wantSylt: []Line{{100, "One more"}, {900, "Two"}},
		},
		{
			name:     "UTF-16 SYLT whose big-endian mark on one entry carries to the next",
			data:     tag(3, 0, frame(3, "SYLT", 0, beSYLT)),
			wantSylt: []Line{{500, "Café"}, {1500, "noir"}},
		},
		{
			// Sizes of 128 and over differ between syncsafe and plain big-endian
			// reads, so this pins both v2.4 readings: the extended header's own
			// size (which counts its four size bytes) and the frame sizes.
			name: "v2.4 extended header and frames larger than 128 bytes",
			data: tag(4, 0x40, bytes.Join([][]byte{
				append(append(ss32(134), 1, 0), make([]byte, 128)...),
				frame(4, "SYLT", 0, latin1SYLT(2, 1, strings.Repeat("a", 150), 500)),
				frame(4, "USLT", 0, usltBody),
			}, nil)),
			wantSylt: []Line{{500, strings.Repeat("a", 150)}},
			wantUslt: "Plain\nwords",
		},
		{
			name: "v2.4 frame-level unsynchronisation with a data length indicator",
			data: tag(4, 0, frame(4, "SYLT", 0x03,
				append(ss32(0), bytes.ReplaceAll(latin1SYLT(2, 1, "\xffy", 0x00ff00ff), []byte{0xff}, []byte{0xff, 0x00})...))),
			wantSylt: []Line{{0x00ff00ff, "ÿy"}},
		},
		{
			name:     "v2.3 whole-tag unsynchronisation",
			data:     tag(3, 0x80, bytes.ReplaceAll(frame(3, "SYLT", 0, latin1SYLT(2, 1, "\xffx", 255)), []byte{0xff}, []byte{0xff, 0x00})),
			wantSylt: []Line{{255, "ÿx"}},
		},
		{
			name:     "a v2.3 extended header is skipped",
			data:     tag(3, 0x40, append(append(be32(6), make([]byte, 6)...), frame(3, "USLT", 0, usltBody)...)),
			wantUslt: "Plain\nwords",
		},
		{
			name: "SYLT timed in MPEG frames or holding chords is not lyrics",
			data: tag(3, 0, bytes.Join([][]byte{
				frame(3, "SYLT", 0, latin1SYLT(1, 1, "frames", 10)),
				frame(3, "SYLT", 0, latin1SYLT(2, 5, "Am", 10)),
			}, nil)),
		},
		{
			name: "a compressed frame is skipped",
			data: tag(3, 0, frame(3, "USLT", 0x80, usltBody)),
		},
		{
			name: "a frame claiming more than the tag holds ends the read",
			data: tag(3, 0, append(frame(3, "TIT2", 0, []byte("\x00x")), []byte("USLT\x7f\xff\xff\xff\x00\x00")...)),
		},
		{name: "no tag", data: []byte("\xff\xfb\x90\x00 audio")},
		{
			name: "v2.2 SLT and ULT beside other frames",
			data: tag(2, 0, bytes.Join([][]byte{
				frame22("TT2", []byte("\x00Song")),
				frame22("SLT", latin1SYLT(2, 1, "One", 500, "Two", 2750)),
				frame22("ULT", usltBody),
			}, nil)),
			wantSylt: []Line{{500, "One"}, {2750, "Two"}},
			wantUslt: "Plain\nwords",
		},
		{
			name:     "v2.2 whole-tag unsynchronisation",
			data:     tag(2, 0x80, bytes.ReplaceAll(frame22("SLT", latin1SYLT(2, 1, "\xffx", 255)), []byte{0xff}, []byte{0xff, 0x00})),
			wantSylt: []Line{{255, "ÿx"}},
		},
		{
			name: "a v2.2 tag marked compressed is not read",
			data: tag(2, 0x40, frame22("ULT", usltBody)),
		},
		{
			name: "a UTF-16 USLT holding a lone surrogate is not read",
			data: tag(3, 0, frame(3, "USLT", 0, append([]byte{1, 'e', 'n', 'g', 0xff, 0xfe, 0, 0}, "\xff\xfeW\x00\x00\xd8x\x00"...))),
		},
		{
			name: "a UTF-16 USLT whose descriptor holds a lone surrogate is not read",
			data: tag(3, 0, frame(3, "USLT", 0, append([]byte{1, 'e', 'n', 'g', 0xff, 0xfe, 0x00, 0xdc, 0, 0}, utf16LE("Words", true)...))),
		},
		{
			name: "a UTF-16BE SYLT with one entry holding a lone surrogate is not read",
			data: tag(4, 0, frame(4, "SYLT", 0, bytes.Join([][]byte{
				{2, 'e', 'n', 'g', 2, 1, 0, 0},
				utf16BE("One", false), {0, 0}, be32(500),
				{0xd8, 0x00, 0x00, 'x'}, {0, 0}, be32(900),
			}, nil))),
		},
		{
			name:     "a rejected USLT leaves a later one to be read",
			data:     tag(3, 0, append(frame(3, "USLT", 0, append([]byte{1, 'e', 'n', 'g', 0, 0}, 0xff, 0xfe, 0x00, 0xd8)), frame(3, "USLT", 0, usltBody)...)),
			wantUslt: "Plain\nwords",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sylt, uslt := readID3(bytes.NewReader(append(tc.data, "\xff\xfbaudio"...)))
			if !reflect.DeepEqual(sylt, tc.wantSylt) || uslt != tc.wantUslt {
				t.Fatalf("readID3 = %+v, %q; want %+v, %q", sylt, uslt, tc.wantSylt, tc.wantUslt)
			}
		})
	}
}

// TestLocalPrecedence: Synced outranks Plain whatever its source, and within a
// shape the sidecar outranks the file's own tags.
func TestLocalPrecedence(t *testing.T) {
	syncedID3 := tag(3, 0, frame(3, "SYLT", 0, latin1SYLT(2, 1, "Embedded", 1000)))
	plainID3 := tag(3, 0, frame(3, "USLT", 0, append([]byte{0, 'e', 'n', 'g', 0}, "USLT words"...)))

	cases := []struct {
		name    string
		audio   []byte
		sidecar string
		tags    map[string]string
		want    Lyrics
		ok      bool
	}{
		{
			name:    "a timed sidecar outranks an embedded SYLT",
			audio:   syncedID3,
			sidecar: "[00:02.00]Sidecar",
			want:    Lyrics{Kind: Synced, Lines: []Line{{2000, "Sidecar"}}},
			ok:      true,
		},
		{
			name:    "an embedded SYLT outranks an untimed sidecar",
			audio:   syncedID3,
			sidecar: "Sidecar words",
			want:    Lyrics{Kind: Synced, Lines: []Line{{1000, "Embedded"}}},
			ok:      true,
		},
		{
			name:    "an untimed sidecar outranks embedded plain text",
			audio:   plainID3,
			sidecar: "Sidecar words",
			tags:    map[string]string{"lyrics": "Tag words"},
			want:    Lyrics{Kind: Plain, Text: "Sidecar words"},
			ok:      true,
		},
		{
			name:  "USLT outranks a LYRICS tag",
			audio: plainID3,
			tags:  map[string]string{"lyrics": "Tag words"},
			want:  Lyrics{Kind: Plain, Text: "USLT words"},
			ok:    true,
		},
		{
			name: "a plain tag holding timed LRC is synced",
			tags: map[string]string{"lyrics-eng": "[00:03.00]Timed in a tag"},
			want: Lyrics{Kind: Synced, Lines: []Line{{3000, "Timed in a tag"}}},
			ok:   true,
		},
		{
			name: "an unsynced lyrics comment is plain",
			tags: map[string]string{"unsyncedlyrics": "Vorbis words"},
			want: Lyrics{Kind: Plain, Text: "Vorbis words"},
			ok:   true,
		},
		{
			name:    "a UTF-16LE sidecar is decoded, and its timed lines outrank USLT",
			audio:   plainID3,
			sidecar: string(utf16LE("[ti:Song]\r\n[00:02.00]Sidecar\r\n", true)),
			want:    Lyrics{Kind: Synced, Lines: []Line{{2000, "Sidecar"}}},
			ok:      true,
		},
		{
			name:    "a UTF-16BE sidecar is decoded",
			audio:   plainID3,
			sidecar: string(utf16BE("Sidecar words", true)),
			want:    Lyrics{Kind: Plain, Text: "Sidecar words"},
			ok:      true,
		},
		{
			name:    "an unreadable sidecar is skipped for the USLT",
			audio:   plainID3,
			sidecar: "\x80\x81 not text \xfe",
			want:    Lyrics{Kind: Plain, Text: "USLT words"},
			ok:      true,
		},
		{
			name:    "a sidecar holding NUL bytes is skipped for the USLT",
			audio:   plainID3,
			sidecar: "W\x00o\x00r\x00d\x00s\x00",
			want:    Lyrics{Kind: Plain, Text: "USLT words"},
			ok:      true,
		},
		{
			name:    "a Windows-1252 sidecar is decoded",
			audio:   plainID3,
			sidecar: "[00:02.00]Caf\xe9 \x93au lait\x94 \x80",
			want:    Lyrics{Kind: Synced, Lines: []Line{{2000, "Café “au lait” €"}}},
			ok:      true,
		},
		{
			name:    "a UTF-16 sidecar holding a lone surrogate is skipped for the USLT",
			audio:   plainID3,
			sidecar: "\xff\xfeW\x00\x00\xd8x\x00",
			want:    Lyrics{Kind: Plain, Text: "USLT words"},
			ok:      true,
		},
		{
			name:  "USLT text with bare CR line endings",
			audio: tag(3, 0, frame(3, "USLT", 0, append([]byte{0, 'e', 'n', 'g', 0}, "Line one\rLine two"...))),
			want:  Lyrics{Kind: Plain, Text: "Line one\nLine two"},
			ok:    true,
		},
		{name: "nothing anywhere", tags: map[string]string{"title": "Song"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "01 - Song.mp3")
			if err := os.WriteFile(path, append(tc.audio, "\xff\xfbaudio"...), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.sidecar != "" {
				if err := os.WriteFile(filepath.Join(dir, "01 - Song.lrc"), []byte(tc.sidecar), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := Local(path, tc.tags)
			if ok != tc.ok || (ok && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("Local = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestAnUnreadableSidecarFallsBackToTheNextCandidate: the .lrc beside a track is
// not text, and the next name tried — the .LRC, a different file on a
// case-sensitive filesystem — is read instead of giving up on sidecars.
func TestAnUnreadableSidecarFallsBackToTheNextCandidate(t *testing.T) {
	dir := t.TempDir()
	unreadable, readable := filepath.Join(dir, "first.lrc"), filepath.Join(dir, "second.lrc")
	if err := os.WriteFile(unreadable, []byte("W\x00o\x00r\x00d\x00s\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readable, []byte("[00:02.00]Upper-case sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := firstSidecar(filepath.Join(dir, "missing.lrc"), unreadable, readable)
	want := Lyrics{Kind: Synced, Lines: []Line{{2000, "Upper-case sidecar"}}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("firstSidecar = %+v, %v; want %+v", got, ok, want)
	}
}

// TestLocalReadsTheLRCBesideAnUnreadableLrc is the same fallback through Local,
// on a filesystem where the two names are two files.
func TestLocalReadsTheLRCBesideAnUnreadableLrc(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01 - Song.mp3")
	if err := os.WriteFile(path, []byte("\xff\xfbaudio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01 - Song.lrc"), []byte("W\x00o\x00r\x00d\x00s\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01 - Song.LRC"), []byte("[00:02.00]Upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 3 {
		t.Skip("this filesystem is case-insensitive: .lrc and .LRC are one file")
	}
	got, ok := Local(path, nil)
	if want := (Lyrics{Kind: Synced, Lines: []Line{{2000, "Upper"}}}); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("Local = %+v, %v; want %+v", got, ok, want)
	}
}

// TestAnUndefinedWindows1252ByteIsNotText: the five bytes Windows-1252 leaves
// undefined are not decoded to anything — not to the C1 control Latin-1 puts
// there — so a sidecar holding one is not a lyrics file.
func TestAnUndefinedWindows1252ByteIsNotText(t *testing.T) {
	for _, b := range []byte{0x81, 0x8d, 0x8f, 0x90, 0x9d} {
		data := []byte("[00:02.00]Caf\xe9 ")
		data = append(data, b)
		if text, ok := decodeWindows1252(data); ok {
			t.Fatalf("decodeWindows1252 with 0x%02x = %q, want not text", b, text)
		}
		if text, ok := sidecarText(data); ok {
			t.Fatalf("sidecarText with 0x%02x = %q, want not text", b, text)
		}
	}
}

// TestTheLRCIsTriedAfterAnUnreadableLrc is TestLocalReadsTheLRCBesideAnUnreadableLrc
// on any filesystem: the files are the test's own, so .lrc and .LRC are two
// files whatever the disk underneath does with case.
func TestTheLRCIsTriedAfterAnUnreadableLrc(t *testing.T) {
	files := map[string]string{
		"/music/01 - Song.lrc": "W\x00o\x00r\x00d\x00s\x00",
		"/music/01 - Song.LRC": "[00:02.00]Upper",
	}
	orig := openSidecar
	t.Cleanup(func() { openSidecar = orig })
	openSidecar = func(name string) (io.ReadCloser, error) {
		body, ok := files[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}
	got, ok := readSidecar("/music/01 - Song.mp3")
	if want := (Lyrics{Kind: Synced, Lines: []Line{{2000, "Upper"}}}); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("readSidecar = %+v, %v; want %+v", got, ok, want)
	}
}
