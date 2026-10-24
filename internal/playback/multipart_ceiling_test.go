package playback

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// A multi-part Edition is delivered through the concat demuxer, whose `-map 0:v:N`
// (or implicit map) picks whatever track each LATER part has at that index. So every
// part's track at the mapped index must pass the checks part 1's does: decodable,
// within the resolution ceiling. When one fails the Decision is a video ENCODE scaled
// to the ceiling, never a copy or remux that could carry the part's track
// (directplay-ceiling-bypass).

type partVideo struct {
	height int
	def    bool
}

func partOf(id string, ord int, vs ...partVideo) store.File {
	f := store.File{ID: id, Path: "/movies/x - part" + string(rune('0'+ord)) + ".mkv", Container: "matroska,webm",
		VideoCodec: "h264", AudioCodec: "aac", Present: true, PartOrdinal: ord, DurationMs: 60_000}
	for i, v := range vs {
		f.Streams = append(f.Streams, store.Stream{ID: id + "v" + string(rune('0'+i)), Index: i, Kind: "video", Codec: "h264", Height: v.height, IsDefault: v.def})
		if v.height > f.Height {
			f.Height = v.height
		}
	}
	f.Streams = append(f.Streams, store.Stream{ID: id + "a0", Index: len(vs), Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true})
	return f
}

func multiPartNegotiate(t *testing.T, p1, p2 store.File) Decision {
	t.Helper()
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{p1, p2}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	return negotiateOK(t, svc, d35Request())
}

func TestMultiPartLaterPartTrackAboveCeilingIsEncodedAndScaled(t *testing.T) {
	cases := []struct {
		name   string
		p1, p2 store.File
	}{
		{"delta", partOf("a", 1, partVideo{2160, false}, partVideo{720, true}), partOf("b", 2, partVideo{720, true}, partVideo{2160, false})},
		{"bravo", partOf("a", 1, partVideo{720, true}, partVideo{480, false}), partOf("b", 2, partVideo{2160, false}, partVideo{720, true})},
		{"charlie", partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{2160, true})},
		{"alpha", partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{2160, false}, partVideo{720, true})},
		{"no track at the index", partOf("a", 1, partVideo{2160, false}, partVideo{720, true}), partOf("b", 2, partVideo{720, true})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := multiPartNegotiate(t, c.p1, c.p2)
			if dec.Tier != TierTranscode || dec.VideoCopy {
				t.Fatalf("tier %s videoCopy %v, want a video encode (transcode, not a copy)", dec.Tier, dec.VideoCopy)
			}
			ctr := Constraints{MaxResolution: "1080p"}
			v := transcodeJobPlan(d35Request().Profile, ctr, dec).Video
			if v.Copy || v.MaxHeight != 720 {
				t.Errorf("video plan %+v, want an encode capped at part 1's 720 (under the 1080 ceiling)", v)
			}
		})
	}
}

// The scale is the tighter of the ceiling and the profile's cap for the encode codec.
func TestMultiPartEncodeIsScaledToTheProfileWhenTighter(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{2160, true})
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{p1, p2}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}
	dec := negotiateOK(t, svc, req)
	ctr := Constraints{MaxResolution: "720p"}
	if v := transcodeJobPlan(req.Profile, ctr, dec).Video; dec.Tier != TierTranscode || v.Copy || v.MaxHeight != 720 {
		t.Errorf("tier %s plan %+v, want an encode scaled to 720", dec.Tier, v)
	}
}

// Echo: the same layout in both parts approves every part, so it stays a remux.
func TestMultiPartSameLayoutStaysARemux(t *testing.T) {
	dec := multiPartNegotiate(t,
		partOf("a", 1, partVideo{2160, false}, partVideo{720, true}),
		partOf("b", 2, partVideo{2160, false}, partVideo{720, true}))
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "av1" {
		t.Errorf("tier %s video %s, want directStream av1", dec.Tier, dec.VideoStream.ID)
	}
}

// A copy that is not a remux (an audio transcode copying the video) is held to the
// same check: a part that would carry an over-ceiling track forces the video encode.
func TestMultiPartAudioTranscodeDoesNotCopyAHiddenLaterTrack(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{2160, true})
	for _, f := range []*store.File{&p1, &p2} {
		for i := range f.Streams {
			if f.Streams[i].Kind == "audio" {
				f.Streams[i].Codec = "flac"
			}
		}
		f.AudioCodec = "flac"
	}
	dec := multiPartNegotiate(t, p1, p2)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
}

// Without a ceiling the check still holds a part to decodability.
func TestMultiPartUndecodableLaterPartIsEncodedWithoutACeiling(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{720, true})
	p2.Streams[0].Codec, p2.VideoCodec = "hevc", "hevc"
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{p1, p2}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.Scope.MaxResolution = ""
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode for an undecodable later part", dec.Tier, dec.VideoCopy)
	}
}

// absLayoutPart is a part whose streams are [audio, video 720p default] at absolute
// indexes 0 and 1, so its mapped video is the RELATIVE 0:v:0 but the ABSOLUTE index 1.
func absLayoutParts() (store.File, store.File) {
	p1 := store.File{ID: "a", Path: "/movies/x - part1.mkv", Container: "matroska,webm", VideoCodec: "h264", AudioCodec: "aac",
		Present: true, PartOrdinal: 1, DurationMs: 60_000, Height: 720,
		Streams: []store.Stream{
			{ID: "aa0", Index: 0, Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
			{ID: "av0", Index: 1, Kind: "video", Codec: "h264", Height: 720, IsDefault: true},
		}}
	// [video 720p default, video 2160p, audio]: the concat demuxer, mapping part 1's
	// absolute index 1, reaches the 2160p track.
	p2 := partOf("b", 2, partVideo{720, true}, partVideo{2160, false})
	return p1, p2
}

// The concat demuxer maps by ABSOLUTE stream index, so a part with a different layout
// than part 1 cannot be proven to carry the approved track and is encoded.
func TestMultiPartDifferentAbsoluteLayoutIsEncoded(t *testing.T) {
	p1, p2 := absLayoutParts()
	dec := multiPartNegotiate(t, p1, p2)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Fatalf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
	if v := transcodeJobPlan(d35Request().Profile, Constraints{MaxResolution: "1080p"}, dec).Video; v.Copy || v.MaxHeight != 720 {
		t.Errorf("video plan %+v, want an encode capped at part 1's 720 (under the 1080 ceiling)", v)
	}
}

// A part with no probed Streams cannot be shown identical to part 1.
func TestMultiPartUnprobedLaterPartIsEncoded(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{720, true})
	p2.Streams = nil
	dec := multiPartNegotiate(t, p1, p2)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
}

// The same layout with a different video codec at the mapped index is not identical.
func TestMultiPartDifferentMappedCodecIsEncoded(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{720, true})
	p2.Streams[0].Codec = "mpeg4"
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{p1, p2}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "2160p"}, {Codec: "mpeg4", MaxResolution: "2160p"}}
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
}

// The multi-part encode fallback never upscales part 1 on a hardware backend: the
// fixed-height scale filters would otherwise stretch a 480p part to the 1080p ceiling.
func TestMultiPartEncodeDoesNotUpscalePartOneOnHardware(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{480, true}), partOf("b", 2, partVideo{2160, true})
	dec := multiPartNegotiate(t, p1, p2)
	ctr := Constraints{MaxResolution: "1080p", CeilingResolution: "1080p"}
	job := transcodeJobPlan(d35Request().Profile, ctr, dec)
	if job.Video.Copy || job.Video.MaxHeight != 480 {
		t.Fatalf("video plan %+v, want an encode capped at part 1's 480", job.Video)
	}
	for _, accel := range []transcode.Accel{transcode.AccelVideoToolbox, transcode.AccelVAAPI} {
		args := strings.Join(transcodeArgsBuilder(job, accel, nil, nil)("/out", transcode.SeekOffset{}), " ")
		for _, h := range []string{"1080", "1440", "2160"} {
			if strings.Contains(args, ":"+h) {
				t.Errorf("%s args scale to %s: %s", accel, h, args)
			}
		}
		if !strings.Contains(args, "-2:480") {
			t.Errorf("%s args do not scale to 480: %s", accel, args)
		}
	}
}

// The applyPicks tail: an explicit audio pick rebuilds the Decision as a remux AFTER
// negotiation judged the parts, so the tail must judge them again.
func TestMultiPartAudioPickRemuxIsEncodedWhenAPartIsNotApproved(t *testing.T) {
	p1 := partOf("a", 1, partVideo{720, false}, partVideo{480, true})
	p2 := partOf("b", 2, partVideo{2160, false}, partVideo{480, true})
	for _, f := range []*store.File{&p1, &p2} {
		f.Streams = append(f.Streams, store.Stream{ID: f.ID + "a1", Index: len(f.Streams), Kind: "audio", Codec: "aac", Channels: 2, Language: "fra"})
	}
	svc := NewService(ceilingStore{detail: titleWith(store.Edition{ID: "e1", Files: []store.File{p1, p2}})}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.AudioStreamID = "aa1"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
}

// Each check of the layout rule is pinned by its own test: the parts below differ from
// part 1 in exactly one way and carry an approved 720p track, so only the check named
// can send them to an encode.
func multiPartLayoutDecision(t *testing.T, mutate func(p2 *store.File)) Decision {
	t.Helper()
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{720, true})
	mutate(&p2)
	return multiPartNegotiate(t, p1, p2)
}

func wantVideoEncode(t *testing.T, dec Decision) {
	t.Helper()
	if dec.Tier != TierTranscode || dec.VideoCopy {
		t.Errorf("tier %s videoCopy %v, want a video encode", dec.Tier, dec.VideoCopy)
	}
}

func TestMultiPartStreamCountMismatchIsEncoded(t *testing.T) {
	wantVideoEncode(t, multiPartLayoutDecision(t, func(p2 *store.File) {
		p2.Streams = append(p2.Streams, store.Stream{ID: "bx", Index: 2, Kind: "audio", Codec: "aac", Channels: 2})
	}))
}

func TestMultiPartKindMismatchAtAnEqualPositionIsEncoded(t *testing.T) {
	wantVideoEncode(t, multiPartLayoutDecision(t, func(p2 *store.File) {
		p2.Streams[1].Kind = "subtitle"
	}))
}

func TestMultiPartIndexMismatchWithEqualKindsIsEncoded(t *testing.T) {
	wantVideoEncode(t, multiPartLayoutDecision(t, func(p2 *store.File) {
		p2.Streams[1].Index = 5
	}))
}

// MPEG-TS parts: part 1 is [0 data (not stored), 1 h264 720, 2 h264 480, 3 aac] and
// part 2 is [0 h264 720, 1 h264 2160, 2 aac]. The stored sequences have the same length
// and the same kinds by position, but ffmpeg index 1 is the 720p track in part 1 and
// the 2160p track in part 2.
func TestMultiPartShiftedDataStreamIsEncoded(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}, partVideo{480, false}), partOf("b", 2, partVideo{720, true}, partVideo{2160, false})
	for i := range p1.Streams {
		p1.Streams[i].Index++
	}
	dec := multiPartNegotiate(t, p1, p2)
	wantVideoEncode(t, dec)
	if v := transcodeJobPlan(d35Request().Profile, Constraints{MaxResolution: "1080p"}, dec).Video; v.Copy || v.MaxHeight != 720 {
		t.Errorf("video plan %+v, want an encode capped at part 1's 720", v)
	}
}

// Identical layouts remux even when a data stream took an Index (the stored Indexes then
// start above 0 and are the same in both parts).
func TestMultiPartSameLayoutWithADroppedDataStreamStaysARemux(t *testing.T) {
	p1, p2 := partOf("a", 1, partVideo{720, true}), partOf("b", 2, partVideo{720, true})
	for _, f := range []*store.File{&p1, &p2} {
		for i := range f.Streams {
			f.Streams[i].Index++
		}
	}
	if dec := multiPartNegotiate(t, p1, p2); dec.Tier != TierDirectStream {
		t.Errorf("tier %s, want directStream", dec.Tier)
	}
}
