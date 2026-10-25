package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// An Edition rung is still Direct play or Direct stream (ADR-0071 decision 1): a
// transcode of one Edition is never its own rung.
func TestStepDownEditionRungsNeverTranscode(t *testing.T) {
	svc := stepService(true,
		edition("uhd", "2160p", 2160, 40_000_000),
		edition("hd", "1080p", 1080, 8_000_000),
	)
	base := stepReq()
	base.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}}
	dec := negotiateOK(t, svc, base)
	for _, r := range dec.StepDown {
		if r.EditionID == "uhd" {
			t.Errorf("rung %q names the Edition the device cannot play as it is: %v", r.Label, labelsOf(dec.StepDown))
		}
		if r.EditionID != "" {
			if got := negotiateOK(t, svc, askRung(base, r)); got.Tier == TierTranscode {
				t.Errorf("Edition rung %q plays as a transcode", r.Label)
			}
		}
	}
	if !hasEditionRung(dec.StepDown, "hd") {
		t.Errorf("want the 1080p direct-play Edition listed: %v", labelsOf(dec.StepDown))
	}
}

func hasEditionRung(rungs []StepRung, id string) bool {
	for _, r := range rungs {
		if r.EditionID == id {
			return true
		}
	}
	return false
}

// A music force-encode of an unknown-bitrate Track is for audio-scale caps only; a
// video-scale cap leaves it as it is.
func TestMusicOfUnknownBitrateIsForcedToEncodeOnlyUnderAnAudioScaleCap(t *testing.T) {
	svc := stepService(true, store.Edition{ID: "e1", Files: []store.File{flacTrack(0, "flac")}})
	base := stepReq()
	base.Profile = musicProfile()

	base.Constraints.MaxBitrate = 8_000_000
	if got := negotiateOK(t, svc, base); got.Tier != TierDirectPlay {
		t.Errorf("an 8 Mbps cap plays as %s, want direct play", got.Tier)
	}

	base.Constraints.MaxBitrate = 256_000
	got := negotiateOK(t, svc, base)
	if got.Tier != TierTranscode {
		t.Fatalf("a 256 kbps cap plays as %s, want an AAC transcode", got.Tier)
	}
	if b, args := audioEncodeBitrate(t, base.Profile, base.Constraints, got); b == 0 || b > 256_000 {
		t.Errorf("a 256 kbps cap encodes at %d, want at most 256000; args %v", b, args)
	}
}
