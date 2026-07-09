package subtitle

import (
	"strings"
	"testing"
)

// The WebVTT conversion is a pure seam (PRD "existing unit seams, no binaries"):
// input text → expected cues, with ASS styling dropped and dialogue kept.

func TestToWebVTT_SRT(t *testing.T) {
	srt := "1\r\n" +
		"00:00:01,000 --> 00:00:02,500\r\n" +
		"Hello, world\r\n" +
		"\r\n" +
		"2\r\n" +
		"00:00:03,000 --> 00:00:04,000\r\n" +
		"Second line\r\n"

	out, err := ToWebVTT([]byte(srt), "srt")
	if err != nil {
		t.Fatalf("ToWebVTT srt: %v", err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "WEBVTT\n") {
		t.Errorf("output missing WEBVTT header:\n%s", got)
	}
	// Milliseconds use a dot, not a comma, in WebVTT.
	if !strings.Contains(got, "00:00:01.000 --> 00:00:02.500") {
		t.Errorf("first cue timing not normalized to dotted ms:\n%s", got)
	}
	if !strings.Contains(got, "Hello, world") || !strings.Contains(got, "Second line") {
		t.Errorf("cue text lost:\n%s", got)
	}
	// The SubRip numeric counters must be gone (a bare "1"/"2" line).
	for _, ln := range strings.Split(got, "\n") {
		if strings.TrimSpace(ln) == "1" || strings.TrimSpace(ln) == "2" {
			t.Errorf("SubRip cue counter leaked into WebVTT: %q\n%s", ln, got)
		}
	}
}

func TestToWebVTT_VTTPassthrough(t *testing.T) {
	vtt := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nAlready VTT\n"
	out, err := ToWebVTT([]byte(vtt), "webvtt")
	if err != nil {
		t.Fatalf("ToWebVTT vtt: %v", err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "WEBVTT") || !strings.Contains(got, "Already VTT") {
		t.Errorf("passthrough mangled the VTT:\n%s", got)
	}
	if !strings.Contains(got, "00:00:00.000 --> 00:00:01.000") {
		t.Errorf("passthrough dropped the cue timing:\n%s", got)
	}
}

func TestToWebVTT_VTTHeaderlessGetsHeader(t *testing.T) {
	// Defensive: a body missing the header still comes out valid.
	body := "00:00:00.000 --> 00:00:01.000\nNo header\n"
	out, _ := ToWebVTT([]byte(body), "vtt")
	if !strings.HasPrefix(string(out), "WEBVTT") {
		t.Errorf("headerless VTT did not gain a WEBVTT header:\n%s", out)
	}
}

func TestToWebVTT_ASSDowngrade(t *testing.T) {
	ass := strings.Join([]string{
		"[Script Info]",
		"Title: Example",
		"ScriptType: v4.00+",
		"",
		"[V4+ Styles]",
		"Format: Name, Fontname, Fontsize",
		"Style: Default,Arial,20",
		"",
		"[Events]",
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text",
		`Dialogue: 0,0:00:01.00,0:00:03.50,Default,,0,0,0,,{\i1}Hello{\i0}, {\pos(200,300)}world`,
		`Comment: 0,0:00:04.00,0:00:05.00,Default,,0,0,0,,not shown`,
		`Dialogue: 0,0:00:06.00,0:00:07.00,Default,,0,0,0,,Line one\NLine two`,
	}, "\n") + "\n"

	out, err := ToWebVTT([]byte(ass), "ass")
	if err != nil {
		t.Fatalf("ToWebVTT ass: %v", err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "WEBVTT") {
		t.Errorf("ASS output missing WEBVTT header:\n%s", got)
	}
	// ASS centiseconds → WebVTT milliseconds.
	if !strings.Contains(got, "00:00:01.000 --> 00:00:03.500") {
		t.Errorf("ASS timing not converted:\n%s", got)
	}
	// Dialogue kept…
	if !strings.Contains(got, "Hello, world") {
		t.Errorf("ASS dialogue lost:\n%s", got)
	}
	// …styling dropped (no override braces survive).
	if strings.Contains(got, "{") || strings.Contains(got, `\i1`) || strings.Contains(got, "pos(") {
		t.Errorf("ASS styling leaked into WebVTT:\n%s", got)
	}
	// A Comment: line is not a cue.
	if strings.Contains(got, "not shown") {
		t.Errorf("ASS Comment line leaked as a cue:\n%s", got)
	}
	// \N becomes a real line break.
	if !strings.Contains(got, "Line one\nLine two") {
		t.Errorf("ASS \\N not converted to a newline:\n%s", got)
	}
}

func TestTextFormatAndConvertible(t *testing.T) {
	cases := map[string]string{
		"srt": "srt", "subrip": "srt",
		"vtt": "vtt", "webvtt": "vtt",
		"ass": "ass", "ssa": "ass",
		"mov_text":          "", // embedded text: extracted via ffmpeg, not this path
		"hdmv_pgs_subtitle": "", // image
		"microdvd":          "", // text but frame-rate dependent — not handled here
		"":                  "",
	}
	for in, want := range cases {
		if got := TextFormat(in); got != want {
			t.Errorf("TextFormat(%q) = %q, want %q", in, got, want)
		}
		if got := IsTextConvertible(in); got != (want != "") {
			t.Errorf("IsTextConvertible(%q) = %v, want %v", in, got, want != "")
		}
	}
}

func TestToWebVTT_SRTEscapesMarkup(t *testing.T) {
	// Raw < and & in dialogue must be escaped so the browser doesn't parse them as
	// (broken) cue markup and drop the rest of the line; legit inline tags stay.
	srt := "1\n00:00:00,000 --> 00:00:01,000\nif x < y & z then <i>run</i>\n"
	out, err := ToWebVTT([]byte(srt), "srt")
	if err != nil {
		t.Fatalf("ToWebVTT: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "if x &lt; y &amp; z then <i>run</i>") {
		t.Errorf("cue text not correctly escaped (< , & escaped; <i> kept):\n%s", got)
	}
	// A pre-encoded entity must not be double-escaped.
	srt2 := "1\n00:00:00,000 --> 00:00:01,000\nTom &amp; Jerry\n"
	out2, _ := ToWebVTT([]byte(srt2), "srt")
	if !strings.Contains(string(out2), "Tom &amp; Jerry") || strings.Contains(string(out2), "&amp;amp;") {
		t.Errorf("existing entity was double-escaped:\n%s", out2)
	}
}

func TestToWebVTT_ASSEscapesMarkup(t *testing.T) {
	ass := strings.Join([]string{
		"[Events]",
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text",
		`Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,5 < 6 & 7`,
	}, "\n") + "\n"
	out, _ := ToWebVTT([]byte(ass), "ass")
	if !strings.Contains(string(out), "5 &lt; 6 &amp; 7") {
		t.Errorf("ASS cue text not escaped:\n%s", out)
	}
}

func TestToWebVTT_VTTLeadingWhitespaceTrimmed(t *testing.T) {
	// A .vtt whose body has blank lines before WEBVTT must be served starting at
	// the signature, else the browser rejects the whole track.
	vtt := "\n\n   WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nHi\n"
	out, _ := ToWebVTT([]byte(vtt), "vtt")
	if !strings.HasPrefix(string(out), "WEBVTT") {
		t.Errorf("leading whitespace not trimmed before WEBVTT:\n%q", string(out))
	}
}

func TestToWebVTT_UnknownFormatErrors(t *testing.T) {
	if _, err := ToWebVTT([]byte("x"), "hdmv_pgs_subtitle"); err == nil {
		t.Error("expected an error converting an image format to WebVTT")
	}
}
