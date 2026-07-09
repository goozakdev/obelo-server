package playback

import (
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Regression for the "Safari won't play a non-AAC default audio" bug. Safari's
// canPlayType returns "maybe" for ac-3/ec-3, so the web profile USED to advertise
// ac3/eac3 — and the server, honoring that claim, DIRECT-PLAYED/REMUXED (copied)
// the AC3 audio Safari cannot actually decode. The client fix stops advertising
// ac3/eac3; this pins the server half of the contract that makes the fix work: an
// audio codec the client does NOT declare is TRANSCODED to AAC (tier=transcode),
// never copied verbatim — so every non-AAC default is delivered as playable AAC.
//
// This is the honest-browser profile (aac only) the fixed capabilities.ts sends;
// the assertion is that no non-AAC default escapes as a copy, regardless of
// container or channel count.
func TestBrowserProfileTranscodesUndeclaredAudioCodec(t *testing.T) {
	browser := DeviceProfile{
		Containers:       []string{"mp4"}, // Safari: mp4 yes, mkv no
		VideoCodecs:      []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}},
		AudioCodecs:      []string{"aac", "mp3"}, // honest: NO ac3/eac3
		MaxAudioChannels: 2,
	}
	cons := Constraints{MaxBitrate: 100_000_000, MaxResolution: "1080p"}
	const mp4 = "mov,mp4,m4a,3gp,3g2,mj2"
	const mkv = "matroska,webm"

	cases := []struct {
		name      string
		container string
		codec     string
		channels  int
	}{
		{"ac3 stereo mp4", mp4, "ac3", 2},
		{"eac3 stereo mp4", mp4, "eac3", 2},
		{"ac3 5.1 mkv", mkv, "ac3", 6},
		{"dts 5.1 mkv", mkv, "dts", 6},
		{"truehd 7.1 mkv", mkv, "truehd", 8},
	}
	for _, c := range cases {
		f := store.File{
			ID: "f", Path: "/movies/x", Container: c.container,
			VideoCodec: "h264", AudioCodec: c.codec, Height: 1080, Bitrate: 6_000_000, Present: true,
			Streams: []store.Stream{
				{ID: "v", Kind: "video", Codec: "h264", Height: 1080, IsDefault: true},
				{ID: "a", Kind: "audio", Codec: c.codec, Channels: c.channels, Language: "en", IsDefault: true},
			},
		}
		ed := store.Edition{ID: "e", Files: []store.File{f}}
		dec, unsup := SelectEdition(browser, cons, []store.Edition{ed}, "")
		if unsup != nil {
			t.Errorf("%s: unexpected unsupported %v", c.name, unsup)
			continue
		}
		if dec.Tier != TierTranscode {
			t.Errorf("%s: tier = %s (delivers %s copied verbatim); want transcode → AAC, else the browser can't decode it",
				c.name, dec.Tier, c.codec)
		}
	}

	// Control: an AAC default the browser DOES declare must still direct-play
	// (unchanged) — the fix must not force a needless transcode of playable audio.
	aac := store.File{
		ID: "f", Path: "/movies/a.mp4", Container: mp4,
		VideoCodec: "h264", AudioCodec: "aac", Height: 1080, Bitrate: 6_000_000, Present: true,
		Streams: []store.Stream{
			{ID: "v", Kind: "video", Codec: "h264", Height: 1080, IsDefault: true},
			{ID: "a", Kind: "audio", Codec: "aac", Channels: 2, Language: "en", IsDefault: true},
		},
	}
	dec, unsup := SelectEdition(browser, cons, []store.Edition{{ID: "e", Files: []store.File{aac}}}, "")
	if unsup != nil || dec.Tier != TierDirectPlay {
		t.Errorf("aac stereo mp4: tier = %s, unsup = %v; want directPlay (playable audio must not transcode)", dec.Tier, unsup)
	}
}
