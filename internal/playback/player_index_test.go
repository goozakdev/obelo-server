package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Unit tests for HLSPlayerIndexes (ADR-0067): the index a player demuxing the HLS
// master sees for each Stream. The numbering is the master's playlist order, so
// these pin the rule; the api golden test pins it against the real master.

func playerIndexDecision(tier Tier, audioN int, subs []SubtitleTrack) Decision {
	streams := []store.Stream{{ID: "v1", Index: 0, Kind: "video", Codec: "h264"}}
	for i := 0; i < audioN; i++ {
		streams = append(streams, store.Stream{ID: "a" + string(rune('0'+i)), Index: i + 1, Kind: "audio", Codec: "aac"})
	}
	return Decision{
		Tier:        tier,
		File:        store.File{Streams: streams},
		VideoStream: streams[0],
		Subtitles:   subs,
	}
}

func TestHLSPlayerIndexesNumberAudioFirstThenVideoAfterTheTextSubtitles(t *testing.T) {
	t.Parallel()
	subs := []SubtitleTrack{
		{ID: "s1", Kind: "text", Convertible: true},
		{ID: "s2", Kind: "image"},                    // no in-band rendition
		{ID: "s3", Kind: "text", Convertible: false}, // unconvertible: none either
		{ID: "s4", Kind: "text", Convertible: true},
	}
	for _, tier := range []Tier{TierDirectStream, TierTranscode} {
		pi, ok := HLSPlayerIndexes(playerIndexDecision(tier, 3, subs))
		if !ok {
			t.Fatalf("%s: demuxed Decision reported no player indexes", tier)
		}
		for id, want := range map[string]int{"a0": 0, "a1": 1, "a2": 2} {
			if got, has := pi.Audio[id]; !has || got != want {
				t.Errorf("%s: audio %s player index = %d (present %v), want %d", tier, id, got, has, want)
			}
		}
		// 3 audio renditions + 2 deliverable text subtitle renditions precede the video.
		if pi.Video != 5 {
			t.Errorf("%s: video player index = %d, want 5", tier, pi.Video)
		}
	}
}

func TestHLSPlayerIndexesNumberAMuxedVariantAfterTheTextSubtitles(t *testing.T) {
	t.Parallel()
	subs := []SubtitleTrack{
		{ID: "s1", Kind: "text", Convertible: true},
		{ID: "s2", Kind: "image"},
		{ID: "s3", Kind: "text", Convertible: true},
	}
	// single audio, and a remux-selected multi-audio File whose played audio is a1.
	single := playerIndexDecision(TierDirectStream, 1, subs)
	single.AudioStream = single.File.Streams[1]
	selected := playerIndexDecision(TierDirectStream, 3, subs)
	selected.RemuxSelectedOnly = true
	selected.AudioStream = selected.File.Streams[2] // container index 2, played
	for name, c := range map[string]struct {
		dec       Decision
		audioID   string
		wantVideo int
		wantAudio int
	}{
		"single audio":   {single, "a0", 2, 3},
		"remux selected": {selected, "a1", 2, 3},
		"bare playlist":  {func() Decision { d := single; d.Subtitles = nil; return d }(), "a0", 0, 1},
	} {
		pi, ok := HLSPlayerIndexes(c.dec)
		if !ok {
			t.Fatalf("%s: no player indexes", name)
		}
		if pi.Video != c.wantVideo {
			t.Errorf("%s: video = %d, want %d", name, pi.Video, c.wantVideo)
		}
		if len(pi.Audio) != 1 || pi.Audio[c.audioID] != c.wantAudio {
			t.Errorf("%s: audio = %v, want only %s at %d (the played Stream)", name, pi.Audio, c.audioID, c.wantAudio)
		}
	}
}

func TestHLSPlayerIndexesAreOmittedWhereNoOrderIsClaimed(t *testing.T) {
	t.Parallel()
	cases := map[string]Decision{
		"directPlay":         playerIndexDecision(TierDirectPlay, 3, nil),
		"audio only (Track)": func() Decision { d := playerIndexDecision(TierDirectStream, 3, nil); d.AudioOnly = true; return d }(),
	}
	for name, dec := range cases {
		if pi, ok := HLSPlayerIndexes(dec); ok {
			t.Errorf("%s: got player indexes %+v, want none", name, pi)
		}
	}
}
