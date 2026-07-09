package scanner

import "testing"

// TestParseFFprobe checks the JSON-mapping seam against a representative
// ffprobe payload, without invoking the binary (the integration test in the api
// package exercises the real ffprobe).
func TestParseFFprobe(t *testing.T) {
	data := []byte(`{
	  "streams": [
	    {"index":0,"codec_type":"video","codec_name":"h264","width":320,"height":240,
	     "disposition":{"default":1}},
	    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2,
	     "tags":{"language":"eng"},"disposition":{"default":1}},
	    {"index":2,"codec_type":"audio","codec_name":"dts","channels":2,
	     "tags":{"language":"eng","title":"Director's Commentary"},
	     "disposition":{"comment":1,"hearing_impaired":1}},
	    {"index":3,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}},
	    {"index":4,"codec_type":"data","codec_name":"bin_data"}
	  ],
	  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.000000",
	             "bit_rate":"125000","size":"15625"}
	}`)

	info, err := parseFFprobe(data)
	if err != nil {
		t.Fatalf("parseFFprobe: %v", err)
	}
	if info.Container != "mov" {
		t.Errorf("container = %q, want mov (first of the comma list)", info.Container)
	}
	if info.DurationMs != 1000 {
		t.Errorf("durationMs = %d, want 1000", info.DurationMs)
	}
	if info.Bitrate != 125000 {
		t.Errorf("bitrate = %d, want 125000", info.Bitrate)
	}
	if info.SizeBytes != 15625 {
		t.Errorf("sizeBytes = %d, want 15625", info.SizeBytes)
	}
	// data stream dropped → only the playable elementary kinds remain (2 audio,
	// 1 video, 1 subtitle).
	if len(info.Streams) != 4 {
		t.Fatalf("streams = %d, want 4 (data dropped)", len(info.Streams))
	}

	// The commentary audio Stream (index 2) captures its title tag and the
	// comment/hearing-impaired dispositions ffprobe exposes (audio-streams/01).
	var comment StreamInfo
	for _, s := range info.Streams {
		if s.Index == 2 {
			comment = s
		}
	}
	if comment.Title != "Director's Commentary" {
		t.Errorf("commentary title = %q, want Director's Commentary", comment.Title)
	}
	if !comment.Commentary {
		t.Errorf("commentary stream: Commentary = false, want true")
	}
	if !comment.HearingImpaired {
		t.Errorf("commentary stream: HearingImpaired = false, want true")
	}
	// The ordinary audio Stream (index 1) carries neither a title nor a
	// commentary/HI disposition.
	if a2, _ := info.PrimaryAudio(); a2.Title != "" || a2.Commentary || a2.HearingImpaired {
		t.Errorf("primary audio picked up spurious label/dispositions: %+v", a2)
	}

	v, ok := info.PrimaryVideo()
	if !ok || v.Codec != "h264" || v.Width != 320 || v.Height != 240 {
		t.Errorf("primary video = %+v (ok=%v), want h264 320x240", v, ok)
	}
	a, ok := info.PrimaryAudio()
	if !ok || a.Codec != "aac" || a.Channels != 2 || a.Language != "eng" {
		t.Errorf("primary audio = %+v (ok=%v), want aac 2ch eng", a, ok)
	}
	if !v.IsDefault {
		t.Errorf("video stream should be default")
	}
}
