package transcode

import (
	"strings"
	"testing"
)

const onlineWhitelist = "https,tls,tcp,crypto"

// inputsOf returns, for each -i in args, the flags that precede it since the
// previous input (its own input options) and its value.
func inputsOf(args []string) (opts [][]string, urls []string) {
	var cur []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-i" && i+1 < len(args) {
			opts = append(opts, cur)
			urls = append(urls, args[i+1])
			cur = nil
			i++
			continue
		}
		cur = append(cur, args[i])
	}
	return opts, urls
}

func hasPairIn(args []string, k, v string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

// TestEveryOnlineInputCarriesTheProtocolWhitelist: whichever shape an Online
// variant has, EACH input ffmpeg is given (a split variant has two) is preceded by
// -protocol_whitelist https,tls,tcp,crypto, so no input option is applied to
// another input and no input can name a file:, plain http, udp or concat target.
func TestEveryOnlineInputCarriesTheProtocolWhitelist(t *testing.T) {
	for name, job := range map[string]OnlineJob{
		"muxed":    {Inputs: []string{"https://m.example/v.mp4"}, OutputDir: "/scratch/s1"},
		"split":    {Inputs: []string{"https://m.example/v.mp4", "https://m.example/a.m4a"}, OutputDir: "/scratch/s1"},
		"manifest": {Inputs: []string{"https://m.example/master.m3u8"}, OutputDir: "/scratch/s1"},
		"capped":   {Inputs: []string{"https://m.example/v.mp4"}, OutputDir: "/scratch/s1", MaxHeight: 480, MaxBitrate: 1_000_000},
		"headers":  {Inputs: []string{"https://m.example/v.mp4"}, OutputDir: "/scratch/s1", Headers: map[string]string{"Referer": "https://tube.example/"}},
	} {
		args := OnlineArgs(job)
		opts, urls := inputsOf(args)
		if len(urls) != len(job.Inputs) {
			t.Errorf("%s: args name %d inputs, want %d: %v", name, len(urls), len(job.Inputs), args)
			continue
		}
		for i, u := range urls {
			if !hasPairIn(opts[i], "-tls_verify", "1") {
				t.Errorf("%s: input %d (%s) does not verify the peer certificate; its options: %v", name, i, u, opts[i])
			}
			if u != job.Inputs[i] || !hasPairIn(opts[i], "-protocol_whitelist", onlineWhitelist) {
				t.Errorf("%s: input %d (%s) is not preceded by -protocol_whitelist %s; its options: %v", name, i, u, onlineWhitelist, opts[i])
			}
		}
		if n := strings.Count(strings.Join(args, " "), "-protocol_whitelist"); n != len(job.Inputs) {
			t.Errorf("%s: %d -protocol_whitelist flags for %d inputs: %v", name, n, len(job.Inputs), args)
		}
	}
}

// TestNoOtherRunGainsTheProtocolWhitelist: a Title's remux, transcode and audio
// rendition are byte-for-byte what they were; the flag marks an Online run.
func TestNoOtherRunGainsTheProtocolWhitelist(t *testing.T) {
	runs := map[string][]string{
		"remux":     RemuxArgs(RemuxJob{SourcePath: "/movies/a.mkv", OutputDir: "/scratch/s"}),
		"transcode": TranscodeArgs(TranscodeJob{SourcePath: "/movies/a.mkv", OutputDir: "/scratch/s", HasAudio: true}),
		"rendition": AudioRenditionArgs(AudioRenditionJob{SourcePath: "/movies/a.mkv", OutputDir: "/scratch/s"}),
	}
	for name, args := range runs {
		if strings.Contains(strings.Join(args, " "), "protocol_whitelist") {
			t.Errorf("%s args gained -protocol_whitelist: %v", name, args)
		}
	}
}

// TestOnlineArgsPassTheVariantsHeaders: the media host's required headers reach
// ffmpeg as input options of the input they belong to, and a header cannot smuggle
// another one in through a line break.
func TestOnlineArgsPassTheVariantsHeaders(t *testing.T) {
	args := OnlineArgs(OnlineJob{
		Inputs:    []string{"https://m.example/v.mp4"},
		OutputDir: "/scratch/s1",
		Headers: map[string]string{
			"Referer":    "https://tube.example/",
			"User-Agent": "TubeClient/1.0",
			"Origin":     "https://tube.example\r\nX-Evil: 1",
		},
	})
	opts, _ := inputsOf(args)
	if !hasPairIn(opts[0], "-user_agent", "TubeClient/1.0") {
		t.Errorf("User-Agent was not passed as -user_agent: %v", args)
	}
	var headers string
	for i := 0; i+1 < len(opts[0]); i++ {
		if opts[0][i] == "-headers" {
			headers = opts[0][i+1]
		}
	}
	if !strings.Contains(headers, "Referer: https://tube.example/\r\n") {
		t.Errorf("-headers = %q, want the Referer", headers)
	}
	if strings.Contains(headers, "X-Evil") && strings.Contains(headers, "\r\nX-Evil") {
		t.Errorf("-headers = %q: a line break in a value smuggled in a header", headers)
	}
	if strings.Contains(headers, "User-Agent") {
		t.Errorf("-headers = %q: the User-Agent travels as -user_agent", headers)
	}
}

// TestOnlineArgsMapAndEncode: a split variant takes its picture from the first
// input and its sound from the second; the encode is capped to the ceiling and
// segmented into the scratch dir as an event playlist, which is readable while the
// encode is still running.
func TestOnlineArgsMapAndEncode(t *testing.T) {
	split := OnlineArgs(OnlineJob{
		Inputs: []string{"https://m.example/v.mp4", "https://m.example/a.m4a"}, OutputDir: "/scratch/s1",
		MaxHeight: 480, MaxBitrate: 1_000_000,
	})
	if !hasPairIn(split, "-map", "0:v:0") || !hasPairIn(split, "-map", "1:a:0?") {
		t.Errorf("split variant maps: %v", split)
	}
	joined := strings.Join(split, " ")
	for _, want := range []string{"-c:v libx264", "min(480,ih)", "-maxrate 1000000", "-c:a aac", "-hls_playlist_type event", "-f hls"} {
		if !strings.Contains(joined, want) {
			t.Errorf("split args lack %q: %v", want, split)
		}
	}
	if last := split[len(split)-1]; last != "/scratch/s1/"+PlaylistName {
		t.Errorf("output = %q, want the scratch playlist", last)
	}
	if !hasPairIn(split, "-hls_segment_filename", "/scratch/s1/"+SegmentPattern) {
		t.Errorf("segments are not written into the scratch dir: %v", split)
	}

	muxed := OnlineArgs(OnlineJob{Inputs: []string{"https://m.example/v.mp4"}, OutputDir: "/scratch/s1"})
	if !hasPairIn(muxed, "-map", "0:v:0") || !hasPairIn(muxed, "-map", "0:a:0?") {
		t.Errorf("muxed variant maps: %v", muxed)
	}
	if strings.Contains(strings.Join(muxed, " "), "-vf") {
		t.Errorf("an uncapped encode carries a -vf: %v", muxed)
	}
}
