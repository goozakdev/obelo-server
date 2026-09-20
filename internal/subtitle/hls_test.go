package subtitle

import (
	"fmt"
	"strings"
	"testing"
)

// The whole-file WebVTT the api layer produces (from a sidecar convert or an
// embedded ffmpeg extraction) that these HLS builders segment. Absolute media
// time, three cues at 1s, 5s, 9s so they fall in distinct 4s segments.
const sampleVTT = `WEBVTT

1
00:00:01.000 --> 00:00:03.000
First cue

2
00:00:05.000 --> 00:00:07.500 align:start
Second cue

3
00:00:09.000 --> 00:00:11.000
Third cue
`

func TestSegmentVTTHeaderAndTimestampMap(t *testing.T) {
	seg := string(SegmentVTT([]byte(sampleVTT), 0, 4))
	if !strings.HasPrefix(seg, "WEBVTT\n") {
		t.Fatalf("segment does not start with WEBVTT header:\n%s", seg)
	}
	wantMap := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:00:00:00.000", HLSTimestampMapMPEGTS)
	if !strings.Contains(seg, wantMap) {
		t.Errorf("segment missing timestamp map %q:\n%s", wantMap, seg)
	}
}

func TestSegmentVTTPartitionsCuesByWindow(t *testing.T) {
	// Segment 0 = [0s,4s): only the first cue (1s–3s).
	seg0 := string(SegmentVTT([]byte(sampleVTT), 0, 4))
	if !strings.Contains(seg0, "First cue") {
		t.Errorf("segment 0 missing the 1s cue:\n%s", seg0)
	}
	if strings.Contains(seg0, "Second cue") || strings.Contains(seg0, "Third cue") {
		t.Errorf("segment 0 leaked a later cue:\n%s", seg0)
	}

	// Segment 1 = [4s,8s): the second cue (5s–7.5s), keeping its absolute time and
	// cue settings.
	seg1 := string(SegmentVTT([]byte(sampleVTT), 1, 4))
	if !strings.Contains(seg1, "Second cue") {
		t.Errorf("segment 1 missing the 5s cue:\n%s", seg1)
	}
	if !strings.Contains(seg1, "00:00:05.000 --> 00:00:07.500 align:start") {
		t.Errorf("segment 1 did not preserve the absolute time + cue settings:\n%s", seg1)
	}
	if strings.Contains(seg1, "First cue") || strings.Contains(seg1, "Third cue") {
		t.Errorf("segment 1 leaked another segment's cue:\n%s", seg1)
	}

	// Segment 2 = [8s,12s): the third cue (9s–11s) — the "later segment stays
	// time-mapped" case the acceptance test leans on.
	seg2 := string(SegmentVTT([]byte(sampleVTT), 2, 4))
	if !strings.Contains(seg2, "Third cue") || !strings.Contains(seg2, "00:00:09.000 --> 00:00:11.000") {
		t.Errorf("segment 2 missing the time-mapped 9s cue:\n%s", seg2)
	}
}

func TestSegmentVTTRepeatsBoundarySpanningCue(t *testing.T) {
	// A cue from 3s–6s straddles the [0,4)/[4,8) boundary → it must appear in BOTH
	// segments so a seek into either shows it (standard HLS WebVTT behavior).
	vtt := "WEBVTT\n\n00:00:03.000 --> 00:00:06.000\nSpanning\n"
	if !strings.Contains(string(SegmentVTT([]byte(vtt), 0, 4)), "Spanning") {
		t.Error("boundary-spanning cue missing from segment 0")
	}
	if !strings.Contains(string(SegmentVTT([]byte(vtt), 1, 4)), "Spanning") {
		t.Error("boundary-spanning cue missing from segment 1")
	}
	// But not in an unrelated later segment.
	if strings.Contains(string(SegmentVTT([]byte(vtt), 3, 4)), "Spanning") {
		t.Error("boundary-spanning cue leaked into a non-overlapping segment")
	}
}

func TestSegmentVTTEmptyWindowIsValid(t *testing.T) {
	// A window past every cue is still a valid (header-only) WebVTT document.
	seg := string(SegmentVTT([]byte(sampleVTT), 100, 4))
	if !strings.HasPrefix(seg, "WEBVTT\n") {
		t.Errorf("empty segment is not valid WebVTT:\n%s", seg)
	}
	if strings.Contains(seg, "-->") {
		t.Errorf("empty segment unexpectedly carries a cue:\n%s", seg)
	}
}

func TestSubtitleMediaPlaylistShape(t *testing.T) {
	pl := string(SubtitleMediaPlaylist(3, 4, func(i int) string {
		return fmt.Sprintf("subs_x_%03d.vtt", i)
	}))
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXT-X-TARGETDURATION:4",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXTINF:4.000000,",
		"subs_x_000.vtt",
		"subs_x_001.vtt",
		"subs_x_002.vtt",
		"#EXT-X-ENDLIST",
	} {
		if !strings.Contains(pl, want) {
			t.Errorf("subtitle media playlist missing %q:\n%s", want, pl)
		}
	}
	if got := strings.Count(pl, "#EXTINF"); got != 3 {
		t.Errorf("segment count = %d EXTINF lines, want 3:\n%s", got, pl)
	}
}

func TestRenditionLinesCarriesSubtitleRendition(t *testing.T) {
	lines := RenditionLines([]Rendition{
		{URI: "subs_en.m3u8", Name: "English", Language: "en", Forced: false},
		{URI: "subs_es.m3u8", Name: "Spanish (Forced)", Language: "es", Forced: true},
	})
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs"`,
		`NAME="English"`,
		`LANGUAGE="en"`,
		`URI="subs_en.m3u8"`,
		`URI="subs_es.m3u8"`,
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("rendition lines missing %q:\n%s", want, lines)
		}
	}
	// The forced track is the auto-display default; the plain one is off.
	if !strings.Contains(lines, `NAME="Spanish (Forced)",LANGUAGE="es",DEFAULT=YES,AUTOSELECT=YES,FORCED=YES`) {
		t.Errorf("forced rendition not marked DEFAULT/FORCED:\n%s", lines)
	}
	if !strings.Contains(lines, `NAME="English",LANGUAGE="en",DEFAULT=NO,AUTOSELECT=YES,FORCED=NO`) {
		t.Errorf("non-forced rendition not marked DEFAULT=NO:\n%s", lines)
	}
}

func TestRenditionLinesEmpty(t *testing.T) {
	// Defensive: no subtitle renditions yields no EXT-X-MEDIA lines at all.
	if lines := RenditionLines(nil); lines != "" {
		t.Errorf("RenditionLines(nil) = %q, want empty", lines)
	}
}
