package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// ADR-0067: every HLS Decision reports `playerIndex` for the Streams the player
// sees, numbered the way a demuxer opening the playlist the Decision points at
// numbers them. This table holds each layout the server can serve to a real ffprobe
// of that playlist: demuxed (AUDIO renditions), a muxed variant behind the
// subtitle renditions (single audio, and remuxSelectedOnly on a multi-audio File,
// whose master still lists an AUDIO group that is never produced), a bare media
// playlist, TS and fMP4, transcode, and a File with two video Streams. The fixture's
// audio and subtitle Streams are laid out so the container `index` and the player's
// differ everywhere they can.

type layoutFixtureSpec struct {
	audio    int  // audio Streams: a0 AAC stereo eng, a1 AC3 5.1 jpn, then AAC mono
	videos   int  // video Streams (the second is smaller, so the first is played)
	hevc     bool // HEVC video instead of h264
	embedded bool // two embedded SubRip tracks (eng, fra)
	sidecar  bool // one sidecar SubRip (de)
}

// layoutFixture writes a ~12s mkv to a fresh library root and returns the root.
func layoutFixture(t *testing.T, f layoutFixtureSpec) string {
	t.Helper()
	root := t.TempDir()
	movieDir := filepath.Join(root, "Layout Movie (2005)")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srt := "1\n00:00:01,000 --> 00:00:03,000\nOne\n\n2\n00:00:05,000 --> 00:00:07,000\nTwo\n"
	args := []string{"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=12:size=320x240:rate=24",
		"-f", "lavfi", "-i", "aevalsrc=0.1*sin(1000*t):duration=12:channel_layout=5.1"}
	maps := []string{"-map", "0:v"}
	next := 2
	if f.videos > 1 {
		args = append(args, "-f", "lavfi", "-i", "testsrc=duration=12:size=160x120:rate=24")
		maps = append(maps, "-map", strconv.Itoa(next)+":v")
		next++
	}
	vcodec := []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p"}
	if f.hevc {
		vcodec = []string{"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-tag:v", "hvc1", "-x265-params", "log-level=none"}
	}
	opts := vcodec
	for i := 0; i < f.audio; i++ {
		maps = append(maps, "-map", "1:a")
		n := strconv.Itoa(i)
		switch i {
		case 0:
			opts = append(opts, "-c:a:0", "aac", "-ac:a:0", "2", "-metadata:s:a:0", "language=eng", "-disposition:a:0", "default")
		case 1:
			opts = append(opts, "-c:a:1", "ac3", "-ac:a:1", "6", "-metadata:s:a:1", "language=jpn", "-disposition:a:1", "0")
		default:
			opts = append(opts, "-c:a:"+n, "aac", "-ac:a:"+n, "1", "-disposition:a:"+n, "0")
		}
	}
	if f.embedded {
		for i, lang := range []string{"eng", "fra"} {
			p := filepath.Join(root, "emb"+strconv.Itoa(i)+".srt")
			if err := os.WriteFile(p, []byte(srt), 0o644); err != nil {
				t.Fatalf("write srt: %v", err)
			}
			args = append(args, "-f", "srt", "-i", p)
			maps = append(maps, "-map", strconv.Itoa(next)+":s")
			opts = append(opts, "-metadata:s:s:"+strconv.Itoa(i), "language="+lang)
			next++
		}
		opts = append(opts, "-c:s", "srt")
	}
	args = append(args, maps...)
	args = append(args, opts...)
	args = append(args, filepath.Join(movieDir, "Layout Movie (2005).mkv"))
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not build the layout fixture: %v\n%s", err, out)
	}
	if f.sidecar {
		if err := os.WriteFile(filepath.Join(movieDir, "Layout Movie (2005).de.srt"), []byte(srt), 0o644); err != nil {
			t.Fatalf("write sidecar: %v", err)
		}
	}
	return root
}

type layoutProbeStream struct {
	Index     int    `json:"index"`
	CodecType string `json:"codec_type"`
	Channels  int    `json:"channels"`
}

// probeStreams is what a demuxer opening url sees, in its own numbering.
func probeStreams(t *testing.T, srv *testharness.Server, token, url string) []layoutProbeStream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-headers", "Authorization: Bearer "+token+"\r\n",
		"-print_format", "json", "-show_streams", srv.URL(url)).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", url, err)
	}
	var p struct {
		Streams []layoutProbeStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("parsing ffprobe json: %v\n%s", err, out)
	}
	for i, s := range p.Streams {
		if s.Index != i {
			t.Fatalf("ffprobe stream %d reports index %d: %s", i, s.Index, out)
		}
	}
	return p.Streams
}

type layoutStream struct {
	ID          string `json:"id"`
	Index       int    `json:"index"`
	Channels    int    `json:"channels"`
	PlayerIndex *int   `json:"playerIndex"`
}

type layoutDecision struct {
	Tier         string         `json:"tier"`
	StreamURL    string         `json:"streamUrl"`
	VideoStream  *layoutStream  `json:"videoStream"`
	AudioStream  *layoutStream  `json:"audioStream"`
	AudioStreams []layoutStream `json:"audioStreams"`
	VideoStreams []layoutStream `json:"videoStreams"`
	Subtitles    []struct {
		Kind string `json:"kind"`
	} `json:"subtitles"`
}

func TestPlayerIndexesMatchWhatAPlayerSeesInEveryHLSLayout(t *testing.T) {
	t.Parallel()
	requireFFmpeg(t)
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx265") {
		t.Skip("ffmpeg has no libx265; the HEVC fMP4 layouts cannot be built")
	}
	subs := layoutFixtureSpec{embedded: true, sidecar: true}
	with := func(f layoutFixtureSpec, audio, videos int, hevc bool) layoutFixtureSpec {
		f.audio, f.videos, f.hevc = audio, videos, hevc
		return f
	}
	cases := []struct {
		name         string
		fixture      layoutFixtureSpec
		profile      func() map[string]any
		selectedOnly bool
		playSecond   bool   // ask for the second audio Stream (container index 2)
		wantMaster   string // "master" or "index"
		wantTier     string
		wantFMP4     bool
		wantAudio    int // audio Streams the player sees
		wantText     int // text subtitle renditions in the playlist
	}{
		{name: "demuxed directStream", fixture: with(subs, 2, 1, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 2, wantText: 3},
		{name: "demuxed transcode", fixture: with(subs, 2, 1, false), profile: transcodeAudioMovieProfile, wantMaster: "master", wantTier: "transcode", wantAudio: 2, wantText: 3},
		{name: "demuxed without subtitles", fixture: with(layoutFixtureSpec{}, 2, 1, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 2},
		{name: "demuxed three audio, two video", fixture: with(subs, 3, 2, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 3, wantText: 3},
		{name: "demuxed HEVC fMP4", fixture: with(subs, 2, 1, true), profile: hevcSafariProfileJSON, wantMaster: "master", wantTier: "directStream", wantFMP4: true, wantAudio: 2, wantText: 3},
		{name: "remuxSelectedOnly plays audio 2", fixture: with(subs, 2, 1, false), profile: mkvMultiAudioProfile, selectedOnly: true, playSecond: true, wantMaster: "master", wantTier: "directStream", wantAudio: 1, wantText: 3},
		{name: "single audio directStream", fixture: with(subs, 1, 1, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 1, wantText: 3},
		{name: "single audio transcode", fixture: with(subs, 1, 1, false), profile: transcodeAudioMovieProfile, wantMaster: "master", wantTier: "transcode", wantAudio: 1, wantText: 3},
		{name: "single audio two video", fixture: with(subs, 1, 2, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 1, wantText: 3},
		{name: "single audio sidecar only", fixture: with(layoutFixtureSpec{sidecar: true}, 1, 1, false), profile: remuxMultiAudioProfile, wantMaster: "master", wantTier: "directStream", wantAudio: 1, wantText: 1},
		// Sidecar only: with embedded subtitle Streams the muxed fMP4 session's ffmpeg
		// fails to start ("webvtt muxer supports only codec webvtt"), a separate defect.
		{name: "single audio HEVC fMP4", fixture: with(layoutFixtureSpec{sidecar: true}, 1, 1, true), profile: hevcSafariProfileJSON, wantMaster: "master", wantTier: "directStream", wantFMP4: true, wantAudio: 1, wantText: 1},
		{name: "bare media playlist", fixture: with(layoutFixtureSpec{}, 1, 1, false), profile: remuxMultiAudioProfile, wantMaster: "index", wantTier: "directStream", wantAudio: 1},
		{name: "bare media playlist remuxSelectedOnly audio 2", fixture: with(layoutFixtureSpec{}, 2, 1, false), profile: mkvMultiAudioProfile, selectedOnly: true, playSecond: true, wantMaster: "index", wantTier: "directStream", wantAudio: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := testharness.New(t)
			token := adminToken(t, srv)
			list := scanLibraryAt(t, srv, token, layoutFixture(t, c.fixture))
			id := findTitle(t, list, "Layout Movie")

			negotiate := func(extra map[string]any) layoutDecision {
				p := c.profile()
				if c.selectedOnly {
					p["remuxSelectedOnly"] = true
				}
				for k, v := range extra {
					p[k] = v
				}
				var dec layoutDecision
				if status, raw := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, p, &dec); status != http.StatusOK {
					t.Fatalf("playback status = %d; body: %s", status, raw)
				}
				return dec
			}
			dec := negotiate(nil)
			if c.playSecond {
				second := ""
				for _, a := range dec.AudioStreams {
					if a.Index == 2 {
						second = a.ID
					}
				}
				if second == "" {
					t.Fatalf("no audio Stream at container index 2: %+v", dec.AudioStreams)
				}
				dec = negotiate(map[string]any{"audioStreamId": second})
			}
			if dec.Tier != c.wantTier || !strings.HasSuffix(dec.StreamURL, "/hls/"+c.wantMaster+".m3u8") {
				t.Fatalf("tier %s url %s, want %s on %s.m3u8", dec.Tier, dec.StreamURL, c.wantTier, c.wantMaster)
			}
			if c.wantFMP4 {
				pl := fetchText(t, srv, dec.StreamURL[:strings.LastIndex(dec.StreamURL, "/")+1]+"index.m3u8", token)
				if !strings.Contains(pl, "#EXT-X-MAP") {
					t.Fatalf("the layout was meant to be fMP4 but index.m3u8 has no #EXT-X-MAP:\n%s", pl)
				}
			}

			probe := probeStreams(t, srv, token, dec.StreamURL)
			counts := map[string]int{}
			for _, s := range probe {
				counts[s.CodecType]++
			}
			if counts["video"] != 1 || counts["audio"] != c.wantAudio || counts["subtitle"] != c.wantText || len(probe) != 1+c.wantAudio+c.wantText {
				t.Fatalf("ffprobe sees %v, want 1 video, %d audio, %d subtitle: %+v", counts, c.wantAudio, c.wantText, probe)
			}
			if len(dec.Subtitles) != c.wantText {
				t.Fatalf("decision lists %d subtitles, the playlist carries %d", len(dec.Subtitles), c.wantText)
			}

			// The played video: its playerIndex is the video in the probe, on the single
			// video's every report; the container index stays 0.
			at := func(what string, s *layoutStream, wantType string) layoutProbeStream {
				t.Helper()
				if s == nil || s.PlayerIndex == nil {
					t.Fatalf("%s = %+v, want a playerIndex", what, s)
				}
				if *s.PlayerIndex < 0 || *s.PlayerIndex >= len(probe) || probe[*s.PlayerIndex].CodecType != wantType {
					t.Fatalf("%s playerIndex %d is not a %s in ffprobe: %+v", what, *s.PlayerIndex, wantType, probe)
				}
				return probe[*s.PlayerIndex]
			}
			at("videoStream", dec.VideoStream, "video")
			if dec.VideoStream.Index != 0 {
				t.Errorf("videoStream index = %d, want its container index 0", dec.VideoStream.Index)
			}
			played := 0
			for _, v := range dec.VideoStreams {
				if v.PlayerIndex != nil {
					played++
					if *v.PlayerIndex != *dec.VideoStream.PlayerIndex {
						t.Errorf("videoStreams playerIndex %d != videoStream's %d", *v.PlayerIndex, *dec.VideoStream.PlayerIndex)
					}
				}
			}
			if played != 1 {
				t.Errorf("%d videoStreams carry a playerIndex, want only the played one: %+v", played, dec.VideoStreams)
			}

			// Audio: every Stream with a playerIndex is the audio of that channel count at
			// that probe position, and exactly the audio the player sees carries one.
			// index stays the container's (video Streams first, then a0, a1, ...).
			withIndex := 0
			for k, a := range dec.AudioStreams {
				if want := c.fixture.videos + k; a.Index != want {
					t.Errorf("audioStreams[%d] index = %d, want its container index %d", k, a.Index, want)
				}
				if a.PlayerIndex == nil {
					continue
				}
				withIndex++
				if got := at("audioStreams["+strconv.Itoa(k)+"]", &a, "audio"); got.Channels != a.Channels {
					t.Errorf("audio %s (container index %d) playerIndex %d has %d channels in ffprobe, want %d", a.ID, a.Index, *a.PlayerIndex, got.Channels, a.Channels)
				}
			}
			if withIndex != c.wantAudio {
				t.Errorf("%d audioStreams carry a playerIndex, want %d", withIndex, c.wantAudio)
			}
			if got := at("audioStream", dec.AudioStream, "audio"); got.Channels != dec.AudioStream.Channels {
				t.Errorf("played audio playerIndex %d has %d channels in ffprobe, want %d", *dec.AudioStream.PlayerIndex, got.Channels, dec.AudioStream.Channels)
			}
			if c.playSecond && (dec.AudioStream.Index != 2 || *dec.AudioStream.PlayerIndex == dec.AudioStream.Index) {
				t.Errorf("played audio index %d playerIndex %d: want container index 2 and a different playerIndex (the case exists to tell them apart)", dec.AudioStream.Index, *dec.AudioStream.PlayerIndex)
			}
		})
	}
}
