package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The ladder plans every candidate the way Negotiate does, with the request's own stream
// picks applied (DECISIONS, after refuter-19), and names a rung for what it delivers.

func hevcDtsEdition(id string, h int, br int64) store.Edition {
	e := edition(id, "", h, br)
	e.Files[0].VideoCodec = "hevc"
	e.Files[0].Streams[0].Codec = "hevc"
	e.Files[0].AudioCodec = "dts"
	e.Files[0].Streams[1].Codec = "dts"
	return e
}

func multiAudioEdition(id string, h int, br int64) store.Edition {
	e := edition(id, "", h, br)
	e.Files[0].Streams[1].ID = id + "-aac"
	e.Files[0].Streams = append(e.Files[0].Streams, store.Stream{ID: id + "-dts", Kind: "audio", Codec: "dts", Channels: 6, Language: "de"})
	return e
}

// An Edition that plays as a video copy (the audio transcoded) is still an Edition rung:
// only a re-encode of the video is not.
func TestStepDownVideoCopyEditionIsARung(t *testing.T) {
	hevcOK := uhdProfile()
	hevcOK.VideoCodecs = append(hevcOK.VideoCodecs, VideoCodecSupport{Codec: "hevc", MaxResolution: "2160p"})
	svc := stepService(true, hevcDtsEdition("uhd", 2160, 40_000_000), edition("hd", "", 1080, 8_000_000))
	base := stepReq()
	base.Profile = hevcOK
	q := base
	q.EditionID = "uhd"
	if d := negotiateOK(t, svc, q); d.Tier != TierTranscode || !d.VideoCopy {
		t.Fatalf("uhd plays as %s copy=%v, want a video-copy transcode", d.Tier, d.VideoCopy)
	}
	dec := negotiateOK(t, svc, base)
	if !hasEditionRung(dec.StepDown, "uhd") {
		t.Errorf("the video-copy Edition is not a rung: %v", labelsOf(dec.StepDown))
	}
	checkDerived(t, "videocopy-edition", svc, base)
}

// An explicit audioStreamId pins the File that carries it and ignores editionId, so two
// Editions that both land on that File are one rung, and a rung's request delivers what
// the rung says.
func TestStepDownRungsHonourAnExplicitAudioPick(t *testing.T) {
	svc := stepService(true, multiAudioEdition("uhd", 2160, 40_000_000), multiAudioEdition("hd", 1080, 8_000_000))
	for _, aid := range []string{"uhd-aac", "uhd-dts"} {
		base := stepReq()
		base.AudioStreamID = aid
		dec := negotiateOK(t, svc, base)
		for _, r := range dec.StepDown {
			if r.EditionID == "" {
				continue
			}
			if got := negotiateOK(t, svc, askRung(base, r)); got.Edition.ID != r.EditionID {
				t.Errorf("audio %s: Edition rung %q (%s) plays Edition %s", aid, r.Label, r.EditionID, got.Edition.ID)
			}
		}
		checkDerived(t, "audio-"+aid, svc, base)
	}
	// preferredAudioLang picks the DTS track on the default path, in every File.
	lang := stepReq()
	lang.Constraints.PreferredAudioLang = "de"
	checkDerived(t, "audio-lang-de", svc, lang)
}

// A burn forces a re-encode of the played File, and remuxSelectedOnly cannot undo it: no
// Edition is its own rung, and every rung's request still delivers what it says.
func TestStepDownBurnWithRemuxSelectedOnly(t *testing.T) {
	detail := titleWith(edition("uhd", "", 2160, 40_000_000), edition("hd", "", 1080, 8_000_000))
	detail.Subtitles = []store.Subtitle{{ID: "s1", Kind: "image", Codec: "hdmv_pgs_subtitle", Path: "/x.sup"}}
	svc := NewService(ceilingStore{detail: detail}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	base := stepReq()
	base.BurnSubtitleID = "s1"
	base.RemuxSelectedOnly = true
	dec := negotiateOK(t, svc, base)
	if dec.Tier != TierTranscode || dec.Burn == nil {
		t.Fatalf("decision = %s burn=%v, want a burning transcode", dec.Tier, dec.Burn)
	}
	for _, r := range dec.StepDown {
		if r.Kind == StepKindEdition {
			t.Errorf("Edition rung %q while burning re-encodes every Edition: %v", r.Label, labelsOf(dec.StepDown))
		}
	}
	checkDerived(t, "burn-remux", svc, base)
}

// A re-encode with no -b:v has an unknown bitrate: no bitrate on the rung, labelled by
// its height alone, never the source File's bitrate.
func TestStepDownReencodeWithoutBitrateIsUnknown(t *testing.T) {
	svc := stepService(true, hevcEdition("uhd", 2160, 40_000_000)) // h264-only client
	base := stepReq()
	base.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}
	dec := negotiateOK(t, svc, base)
	top := dec.StepDown[0]
	if !top.Current || top.Bitrate != 0 || top.Label != "720p" {
		t.Errorf("top rung = %+v, want the current 720p encode with no bitrate: %v", top, labelsOf(dec.StepDown))
	}
	checkDerived(t, "unknown-bitrate", svc, base)
}

// A fixed rung's constraints say what it delivers: under a 720p h264 device the 1080p
// rung does not claim 1080p.
func TestStepDownFixedRungIsClampedToTheDevice(t *testing.T) {
	svc := stepService(true, edition("uhd", "", 2160, 40_000_000))
	base := stepReq()
	base.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}
	dec := negotiateOK(t, svc, base)
	for _, r := range dec.StepDown {
		if resolutionHeight(r.MaxResolution) > 720 {
			t.Errorf("rung %+v claims more than the 720p device delivers: %v", r, labelsOf(dec.StepDown))
		}
	}
	checkDerived(t, "clamped-fixed", svc, base)
}
