package playback

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The Step-down ladder (ADR-0071): every successful decision lists the rungs below
// the quality it delivers, Edition rungs first, then fixed Transcode rungs.

func edition(id, name string, height int, bitrate int64) store.Edition {
	f := mp4File(height, bitrate)
	f.ID = "f-" + id
	return store.Edition{ID: id, Name: name, Files: []store.File{f}}
}

// rungKeys flattens a ladder to "edition:<id>" / "transcode:<res>" strings, with a
// trailing "*" on the rung marked current.
func rungKeys(rungs []StepRung) []string {
	out := []string{}
	for _, r := range rungs {
		k := "transcode:" + r.MaxResolution
		if r.Kind == StepKindEdition {
			k = "edition:" + r.EditionID
		}
		if r.Current {
			k += "*"
		}
		out = append(out, k)
	}
	return out
}

func wantKeys(t *testing.T, got []StepRung, want ...string) {
	t.Helper()
	if got == nil {
		t.Fatal("stepDown is nil, want a non-nil list")
	}
	g := rungKeys(got)
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Errorf("stepDown = %v, want %v", g, want)
	}
}

func stepService(canTranscode bool, eds ...store.Edition) *Service {
	svc := NewService(ceilingStore{detail: titleWith(eds...)}, nil, "", Governance{})
	svc.SetCanTranscode(canTranscode)
	return svc
}

func negotiateOK(t *testing.T, svc *Service, req Request) Decision {
	t.Helper()
	dec, _, unsup, busy, err := svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate: err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	return dec
}

func stepReq() Request {
	return Request{UserID: "u1", TitleID: "t1", Profile: uhdProfile(), Scope: access.Scope{AllLibraries: true}}
}

func ladderEditions() []store.Edition {
	return []store.Edition{
		edition("uhd", "2160p", 2160, 40_000_000),
		edition("hd", "1080p", 1080, 8_000_000),
		edition("sd", "720p", 720, 4_000_000),
	}
}

func TestStepDownIsTheFullLadderWithThePlayingRungMarked(t *testing.T) {
	svc := stepService(true, ladderEditions()...)
	dec := negotiateOK(t, svc, stepReq())
	if dec.Edition.ID != "uhd" || dec.Tier != TierDirectPlay {
		t.Fatalf("decision = %s/%s, want the 4K Edition direct playing", dec.Edition.ID, dec.Tier)
	}
	wantKeys(t, dec.StepDown, "edition:uhd*", "edition:hd", "edition:sd", "transcode:480p")
	if dec.StepDown[1].Label != "1080p" {
		t.Errorf("edition rung label = %q, want the Edition's name", dec.StepDown[1].Label)
	}
	if r := dec.StepDown[3]; r.MaxBitrate != 1_500_000 || r.Kind != StepKindTranscode {
		t.Errorf("480p rung = %+v, want transcode at 1.5 Mbps", r)
	}
}

func TestStepDownPinnedEditionKeepsTheRungsAboveIt(t *testing.T) {
	svc := stepService(true, ladderEditions()...)
	req := stepReq()
	req.EditionID = "sd"
	dec := negotiateOK(t, svc, req)
	wantKeys(t, dec.StepDown, "edition:uhd", "edition:hd", "edition:sd*", "transcode:480p")
}

func TestStepDownRequestBitrateDoesNotHideTheEditionRungs(t *testing.T) {
	svc := stepService(true, ladderEditions()...)
	req := stepReq()
	req.Constraints.MaxBitrate = 3_000_000 // below even the 720p Edition
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode {
		t.Fatalf("tier = %s, want transcode under a 3 Mbps cap", dec.Tier)
	}
	// The delivered target matches no fixed rung, so it is inserted in sorted place.
	var cur StepRung
	for _, r := range dec.StepDown {
		if r.Current {
			cur = r
		}
	}
	if cur.Kind != StepKindTranscode || cur.MaxBitrate != 3_000_000 || cur.Label == "" {
		t.Errorf("current rung = %+v, want an inserted transcode at 3 Mbps", cur)
	}
	wantKeys(t, dec.StepDown, "edition:uhd", "transcode:*", "edition:hd", "edition:sd", "transcode:480p")
}

func TestStepDownNonLadderBitrateIsInsertedInSortedPlace(t *testing.T) {
	svc := stepService(true, edition("uhd", "", 2160, 40_000_000))
	req := stepReq()
	req.Constraints.MaxBitrate = 6_000_000
	dec := negotiateOK(t, svc, req)
	var order []int64
	for _, r := range dec.StepDown {
		if r.Kind == StepKindEdition {
			continue
		}
		order = append(order, r.MaxBitrate)
		if r.Current && r.MaxBitrate != 6_000_000 {
			t.Errorf("current rung %+v, want the 6 Mbps one", r)
		}
	}
	if fmt.Sprint(order) != fmt.Sprint([]int64{6_000_000, 8_000_000, 4_000_000, 1_500_000}) {
		t.Errorf("rung bitrates = %v, want 6M(current, still 2160p), 8M, 4M, 1.5M", order)
	}
}

func TestStepDownSingleEditionGetsAllTranscodeRungs(t *testing.T) {
	ed := edition("uhd", "", 2160, 40_000_000)
	dec := negotiateOK(t, stepService(true, ed), stepReq())
	wantKeys(t, dec.StepDown, "edition:uhd*", "transcode:1080p", "transcode:720p", "transcode:480p")
	if dec.StepDown[1].Label == "" {
		t.Error("transcode rung has no label")
	}

	dec = negotiateOK(t, stepService(false, ed), stepReq())
	wantKeys(t, dec.StepDown, "edition:uhd*") // no ffmpeg → no Transcode rungs
}

func TestStepDownNeverOffersARungAboveTheUserCeiling(t *testing.T) {
	req := stepReq()
	req.Scope.MaxResolution = "1080p"
	dec := negotiateOK(t, stepService(true, ladderEditions()...), req)
	wantKeys(t, dec.StepDown, "edition:hd*", "edition:sd", "transcode:480p") // the 4K Edition alone, transcoded down to the ceiling, is not a rung (brief 18)

	// Even when the client pins the 4K Edition: the current rung is within the ceiling.
	req.EditionID = "uhd"
	dec = negotiateOK(t, stepService(true, ladderEditions()...), req)
	for _, r := range dec.StepDown {
		if r.Kind == StepKindEdition && r.EditionID == "uhd" {
			t.Errorf("4K rung offered under a 1080p ceiling: %+v", r)
		}
	}
	if n := countCurrent(dec.StepDown); n != 1 {
		t.Errorf("%d current rungs, want 1", n)
	}
	req.EditionID = ""

	// A 4K-only Title transcoded down to the ceiling: the unconstrained play (no -b:v) is
	// the delivery, and the fixed 1080p rung, which carries a -b:v, is a different stream.
	dec = negotiateOK(t, stepService(true, edition("uhd", "", 2160, 40_000_000)), req)
	if dec.Tier != TierTranscode {
		t.Fatalf("tier = %s, want transcode under the ceiling", dec.Tier)
	}
	wantKeys(t, dec.StepDown, "transcode:*", "transcode:1080p", "transcode:720p", "transcode:480p")

	req.Scope.MaxBitrate = 5_000_000
	dec = negotiateOK(t, stepService(true, edition("uhd", "", 2160, 40_000_000)), req)
	for _, r := range dec.StepDown {
		if r.MaxBitrate > 5_000_000 {
			t.Errorf("rung %+v is above the 5 Mbps ceiling", r)
		}
	}
	if n := countCurrent(dec.StepDown); n != 1 {
		t.Errorf("%d current rungs, want 1", n)
	}
}

func countCurrent(rungs []StepRung) int {
	n := 0
	for _, r := range rungs {
		if r.Current {
			n++
		}
	}
	return n
}

func TestStepDownEveryRungNegotiatesAndIsMarkedCurrent(t *testing.T) {
	for name, eds := range map[string][]store.Edition{
		"editions": ladderEditions(),
		"single":   {edition("uhd", "", 2160, 40_000_000)},
	} {
		t.Run(name, func(t *testing.T) {
			svc := stepService(true, eds...)
			dec := negotiateOK(t, svc, stepReq())
			if len(dec.StepDown) < 2 {
				t.Fatal("no rungs to request")
			}
			if n := countCurrent(dec.StepDown); n != 1 {
				t.Fatalf("%d current rungs, want 1", n)
			}
			for _, rung := range dec.StepDown {
				req := stepReq()
				if rung.Kind == StepKindEdition {
					req.EditionID = rung.EditionID
				} else {
					req.Constraints.MaxResolution = rung.MaxResolution
					req.Constraints.MaxBitrate = rung.MaxBitrate
				}
				got := negotiateOK(t, svc, req)
				if rung.Kind == StepKindEdition && got.Tier != TierDirectPlay {
					t.Errorf("edition rung %s plays as %s, want directPlay", rung.EditionID, got.Tier)
				}
				if n := countCurrent(got.StepDown); n != 1 {
					t.Errorf("rung %s: %d current rungs, want 1", rung.Label, n)
				}
				for _, g := range got.StepDown {
					if g.Current && g.Label != rung.Label {
						t.Errorf("requesting %q marks %q current", rung.Label, g.Label)
					}
				}
			}
		})
	}
}

func flacTrack(bitrate int64, codec string) store.File {
	return store.File{
		ID: "tr1", Path: "/music/a." + codec, Container: codec, AudioCodec: codec,
		Bitrate: bitrate, Present: true,
		Streams: []store.Stream{{Kind: "audio", Codec: codec, Channels: 2, IsDefault: true}},
	}
}

func musicProfile() DeviceProfile {
	return DeviceProfile{Containers: []string{"flac", "mp4", "aac"}, AudioCodecs: []string{"flac", "aac"}, MaxAudioChannels: 2}
}

// audioEncodeBitrate reads the -b:a the real ffmpeg args for dec carry, or 0 when none.
func audioEncodeBitrate(t *testing.T, p DeviceProfile, c Constraints, dec Decision) (int64, []string) {
	t.Helper()
	job := transcodeJobPlan(p, c, dec)
	job.OutputDir = "/tmp/obelo-stepdown-test"
	args := transcode.TranscodeArgs(job)
	for i, a := range args {
		if a == "-b:a" && i+1 < len(args) {
			n, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				t.Fatalf("-b:a %q: %v", args[i+1], err)
			}
			return n, args
		}
	}
	return 0, args
}

func TestStepDownMusicOffersAAC256ThenAAC128(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	svc := stepService(true, ed)
	req := stepReq()
	req.Profile = musicProfile()
	dec := negotiateOK(t, svc, req)
	if !dec.AudioOnly || dec.Tier != TierDirectPlay {
		t.Fatalf("decision = audioOnly %v tier %s, want audio direct play", dec.AudioOnly, dec.Tier)
	}
	wantKeys(t, dec.StepDown, "edition:e1*", "transcode:", "transcode:")
	if dec.StepDown[1].MaxBitrate != 256_000 || dec.StepDown[2].MaxBitrate != 128_000 {
		t.Errorf("music rungs = %+v, want 256000 then 128000", dec.StepDown)
	}

	// Requesting the 128k rung yields an AAC encode held to that bitrate.
	req.Constraints.MaxBitrate = 128_000
	step := negotiateOK(t, svc, req)
	if step.Tier != TierTranscode || !step.AudioOnly {
		t.Fatalf("128k rung plays as %s audioOnly=%v, want an audio transcode", step.Tier, step.AudioOnly)
	}
	got, args := audioEncodeBitrate(t, req.Profile, req.Constraints, step)
	if got == 0 || got > 128_000 {
		t.Errorf("encode bitrate = %d, want 1..128000; args: %v", got, args)
	}
	wantKeys(t, step.StepDown, "edition:e1", "transcode:", "transcode:*")
	if !step.StepDown[2].Current || step.StepDown[2].MaxBitrate != 128_000 {
		t.Errorf("128k request: rungs %+v, want the 128k rung current", step.StepDown)
	}

	// A 256k-capped play has only the 128k rung left.
	req.Constraints.MaxBitrate = 256_000
	wantKeys(t, negotiateOK(t, svc, req).StepDown, "edition:e1", "transcode:*", "transcode:")

	// A client that cannot decode FLAC gets the 256k AAC encode (ADR-0071).
	req.Constraints = Constraints{}
	req.Profile.AudioCodecs = []string{"aac"}
	req.Profile.Containers = []string{"mp4"}
	enc := negotiateOK(t, svc, req)
	if enc.Tier != TierTranscode {
		t.Fatalf("no-FLAC client tier = %s, want transcode", enc.Tier)
	}
	wantKeys(t, enc.StepDown, "transcode:*", "transcode:") // its 256k encode IS the 256k rung, the unconstrained one, so it ranks first
	if enc.StepDown[0].Bitrate != 256_000 || enc.StepDown[0].MaxBitrate != 0 || enc.StepDown[1].Bitrate != 128_000 {
		t.Errorf("no-FLAC ladder %+v, want AAC 256 then AAC 128", enc.StepDown)
	}
}

func TestMusicUncappedTranscodeLadderMarksEachRungCurrent(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	svc := stepService(true, ed)
	req := stepReq()
	req.Profile = DeviceProfile{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2}
	for _, tc := range []struct {
		maxBR int64
		cur   int
	}{{0, 0}, {256_000, 0}, {128_000, 1}} {
		req.Constraints = Constraints{MaxBitrate: tc.maxBR}
		dec := negotiateOK(t, svc, req)
		if len(dec.StepDown) != 2 || dec.StepDown[0].Bitrate != 256_000 || dec.StepDown[1].Bitrate != 128_000 {
			t.Fatalf("maxBitrate %d: ladder %+v, want [AAC 256, AAC 128]", tc.maxBR, dec.StepDown)
		}
		for i, r := range dec.StepDown {
			if r.Current != (i == tc.cur) {
				t.Errorf("maxBitrate %d: ladder %+v, want rung %d current", tc.maxBR, dec.StepDown, tc.cur)
			}
		}
	}
}

func TestMusicUncappedTranscodeEncodesAt256kStereoAndNoMoreThan69kMono(t *testing.T) {
	for channels, want := range map[int]int64{1: 69_000, 2: 256_000, 6: 256_000} {
		ed := store.Edition{ID: "e1", Files: []store.File{surroundTrack(2_000_000, channels)}}
		req := stepReq()
		req.Profile = DeviceProfile{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 6}
		dec := negotiateOK(t, stepService(true, ed), req)
		if dec.Tier != TierTranscode {
			t.Fatalf("%d ch: tier = %s, want transcode", channels, dec.Tier)
		}
		got, args := audioEncodeBitrate(t, req.Profile, req.Constraints, dec)
		if channels == 1 && got > 69_000 {
			t.Errorf("mono -b:a = %d, want no more than 69000; args: %v", got, args)
		}
		if got != want && !(channels == 1 && got == 0) {
			t.Errorf("%d ch: -b:a = %d, want %d; args: %v", channels, got, want, args)
		}
	}
}

func TestVideoTranscodeAudioArgsCarryNoAudioBitrate(t *testing.T) {
	f := store.File{ID: "v1", Path: "/m/a.mkv", Container: "mkv", VideoCodec: "hevc", AudioCodec: "ac3", Height: 1080, Bitrate: 8_000_000, Present: true,
		Streams: []store.Stream{{Kind: "audio", Codec: "ac3", Channels: 2, IsDefault: true}}}
	dec := Decision{File: f, Tier: TierTranscode, AudioStream: f.Streams[0]}
	profile := DeviceProfile{Containers: []string{"mp4"}, VideoCodecs: []VideoCodecSupport{{Codec: "h264"}}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2}
	if got, args := audioEncodeBitrate(t, profile, Constraints{}, dec); got != 0 {
		t.Errorf("video transcode -b:a = %d, want none; args: %v", got, args)
	}
}

func TestMusicTranscodeNeverCopiesAnAACSourceOverTheCap(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{flacTrack(320_000, "aac")}}
	svc := stepService(true, ed)
	req := stepReq()
	req.Profile = musicProfile()
	req.Constraints.MaxBitrate = 128_000
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode {
		t.Fatalf("tier = %s, want transcode (320k source over a 128k cap)", dec.Tier)
	}
	got, args := audioEncodeBitrate(t, req.Profile, req.Constraints, dec)
	if got == 0 || got > 128_000 {
		t.Errorf("encode bitrate = %d, want 1..128000; args: %v", got, args)
	}
	for i, a := range args {
		if a == "-c:a" && args[i+1] == "copy" {
			t.Errorf("AAC source copied over the cap; args: %v", args)
		}
	}
}

// --- relay -------------------------------------------------------------------

// ladderRelayStore maps the sharer's Edition ids (re1..re4) onto the local e1..e4.
type ladderRelayStore struct{ ceilingStore }

func (ladderRelayStore) LocalIDForRemote(_, remoteID string) (string, error) {
	if len(remoteID) == 3 && remoteID[:2] == "re" && remoteID[2] >= '1' && remoteID[2] <= '4' {
		return "e" + remoteID[2:], nil
	}
	return "", store.ErrNotFound
}

// ladderRelay is a sharer whose decision carries a Step-down ladder, shaped as JSON
// decodes it (numbers are float64).
type ladderRelay struct {
	markerRelay
	stepDown any
}

func (r *ladderRelay) RelayNegotiate(context.Context, RelayRequest) (RelayAnswer, error) {
	d := map[string]any{}
	if r.stepDown != nil {
		d["stepDown"] = r.stepDown
	}
	return RelayAnswer{LinkID: "l1", RemoteSessionID: "rs1", RemoteTitleID: "rt1", RemoteEditionID: "re1", Tier: TierDirectPlay, Decision: d}, nil
}

func rung(kind, editionID, res string, bitrate float64) map[string]any {
	m := map[string]any{"label": kind + editionID + res, "kind": kind}
	if editionID != "" {
		m["editionId"] = editionID
	}
	if res != "" {
		m["constraints"] = map[string]any{"maxResolution": res, "maxBitrate": bitrate}
		if bitrate > 0 {
			m["bitrate"] = bitrate // a sharer states the rung's delivered bitrate when it knows it
		}
	}
	return m
}

func withCurrent(m map[string]any) map[string]any {
	m["current"] = true
	return m
}

func relayLadder(t *testing.T, sharer any, scope access.Scope) []any {
	t.Helper()
	f := func(id string, h int, br int64) store.Edition {
		e := edition(id, "", h, br)
		e.Files[0].Path = ""
		return e
	}
	st := ladderRelayStore{ceilingStore{detail: titleWith(f("e1", 2160, 40_000_000), f("e2", 1080, 8_000_000), f("e3", 720, 4_000_000), f("e4", 480, 1_500_000))}}
	svc := NewService(st, nil, "", Governance{})
	svc.SetRelay(&ladderRelay{stepDown: sharer})
	req := stepReq()
	req.Scope = scope
	dec := negotiateOK(t, svc, req)
	got, ok := dec.Relay.Decision["stepDown"].([]any)
	if !ok {
		t.Fatalf("relay decision stepDown = %#v, want a list", dec.Relay.Decision["stepDown"])
	}
	return got
}

func TestRelayStepDownIsLocalisedAndTrimmedToTheHomeCeiling(t *testing.T) {
	sharer := []any{
		rung("edition", "re1", "", 0),              // 2160p: above the home ceiling
		rung("edition", "re2", "", 0),              // 1080p: above the home ceiling
		withCurrent(rung("edition", "re3", "", 0)), // 720p: what the sharer delivers
		rung("edition", "re9", "", 0),              // the mirror has not seen it: cannot be requested here
		rung("transcode", "", "1080p", 8_000_000),
		rung("transcode", "", "720p", 6_000_000), // under the resolution ceiling, over the bitrate one
		rung("transcode", "", "720p", 4_000_000),
		rung("transcode", "", "480p", 1_500_000),
	}
	got := relayLadder(t, sharer, access.Scope{AllLibraries: true, MaxResolution: "720p", MaxBitrate: 5_000_000})
	var keys []string
	for _, e := range got {
		m := e.(map[string]any)
		star := ""
		if m["current"] == true {
			star = "*"
		}
		if id, ok := m["editionId"].(string); ok {
			keys = append(keys, "edition:"+id+star)
			continue
		}
		c := m["constraints"].(map[string]any)
		keys = append(keys, fmt.Sprintf("transcode:%s/%v%s", c["maxResolution"], c["maxBitrate"], star))
	}
	// The ceiling drops the sharer's top rungs, and {} gets what the sharer plays under it
	// (e3), so that Edition is the unconstrained rung and no stand-in is made.
	want := []string{"edition:e3*", "transcode:720p/4e+06", "transcode:480p/1.5e+06"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("relayed stepDown = %v, want %v", keys, want)
	}
}

func TestRelayStepDownIsAnEmptyListWhenTheSharerSendsNone(t *testing.T) {
	got := relayLadder(t, nil, access.Scope{AllLibraries: true})
	if got == nil || len(got) != 0 {
		t.Errorf("stepDown = %#v, want an empty non-nil list", got)
	}
}

// onlineLadder plays variants under the request constraints c (the ceiling is none) and
// returns the plan and its ladder.
func onlineLadder(t *testing.T, c Constraints, canTranscode bool, variants ...pluginapi.OnlineVariant) []StepRung {
	t.Helper()
	plan, unsup := PlanOnline(onlineProfile(), c, variants, canTranscode)
	if unsup != nil {
		t.Fatal(unsup)
	}
	return OnlineStepDown(onlineProfile(), Constraints{}, c, c, variants, plan, canTranscode)
}

func TestOnlineStepDownListsOriginalThenTheTranscodeRungs(t *testing.T) {
	got := onlineLadder(t, Constraints{}, true, mp4("1080p"))
	wantKeys(t, got, "edition:*", "transcode:720p", "transcode:480p")
	if got[0].Label != "Original · 1080p" || got[0].EditionID != "" || got[0].MaxResolution != "" || got[0].MaxBitrate != 0 {
		t.Errorf("top rung = %+v, want a bare Original", got[0])
	}
}

func TestOnlineStepDownMarksTheRungAResolutionCapDelivers(t *testing.T) {
	wantKeys(t, onlineLadder(t, Constraints{MaxResolution: "720p"}, true, mp4("1080p")),
		"edition:", "transcode:720p", "transcode:720p*", "transcode:480p") // the fixed 720p rung has a -b:v, a bare 720p cap has none
}

func TestOnlineStepDownInsertsTheDeliveredRungWhenItMatchesNone(t *testing.T) {
	// A split variant is always encoded; 3 Mbps is on no fixed rung.
	got := onlineLadder(t, Constraints{MaxBitrate: 3_000_000}, true, split("1080p"))
	wantKeys(t, got, "edition:", "transcode:1080p", "transcode:*", "transcode:720p", "transcode:480p")
	if got[2].MaxBitrate != 3_000_000 || got[2].Label != "1080p 3 Mbps" {
		t.Errorf("inserted rung = %+v", got[2])
	}
	// A client-only bitrate on a relayable variant changes nothing: still the original.
	wantKeys(t, onlineLadder(t, Constraints{MaxBitrate: 3_000_000}, true, mp4("1080p")),
		"edition:*", "transcode:720p", "transcode:480p")
}

func TestOnlineStepDownWithoutFFmpegIsJustOriginal(t *testing.T) {
	wantKeys(t, onlineLadder(t, Constraints{}, false, mp4("1080p")), "edition:*")
}

func TestOnlineStepDownEveryListedRungPlansAndIsCurrentWhenRequested(t *testing.T) {
	variants := []pluginapi.OnlineVariant{mp4("1080p")}
	for _, v := range [][]pluginapi.OnlineVariant{variants, {split("1080p")}} {
		base := onlineLadder(t, Constraints{}, true, v...)
		for i, r := range base {
			c := Constraints{MaxResolution: r.MaxResolution, MaxBitrate: r.MaxBitrate}
			got := onlineLadder(t, c, true, v...) // fails the test if the rung does not plan
			if len(got) != len(base) {
				t.Fatalf("requesting rung %d: ladder %v, want %v", i, rungKeys(got), rungKeys(base))
			}
			for j := range got {
				if got[j].Current != (j == i) {
					t.Errorf("requesting rung %d (%s): ladder %v marks the wrong rung", i, r.Label, rungKeys(got))
				}
			}
		}
	}
}

func TestRelayStepDownKeepsAnUnmappedCurrentRungWithoutItsEditionId(t *testing.T) {
	sharer := []any{
		rung("edition", "re9", "", 0), // unmapped, not current: dropped
		withCurrent(rung("edition", "re8", "", 0)),
		rung("transcode", "", "480p", 1_500_000),
	}
	got := relayLadder(t, sharer, access.Scope{AllLibraries: true})
	if len(got) != 2 {
		t.Fatalf("relayed stepDown = %v, want the current rung and the 480p one", got)
	}
	cur := got[0].(map[string]any)
	if _, has := cur["editionId"]; cur["current"] != true || has {
		t.Errorf("first rung = %v, want current with no editionId", cur)
	}
	if got[1].(map[string]any)["current"] == true {
		t.Errorf("second rung is also current: %v", got[1])
	}
}

// --- brief 09: order, always-present unconstrained rung, replace-not-merge ---------

func labelsOf(rungs []StepRung) []string {
	out := []string{}
	for _, r := range rungs {
		l := r.Label
		if r.Current {
			l += "*"
		}
		out = append(out, l)
	}
	return out
}

func hevcEdition(id string, height int, bitrate int64) store.Edition {
	e := edition(id, "", height, bitrate)
	e.Files[0].VideoCodec = "hevc"
	e.Files[0].Streams[0].Codec = "hevc"
	return e
}

// askRung is the request a client sends to move to rung: the rung's editionId and
// constraints, nothing of its own (its preferredAudioLang is a preference, not a cap, and
// stays).
func askRung(base Request, r StepRung) Request {
	req := base
	req.EditionID = r.EditionID
	req.Constraints = Constraints{MaxResolution: r.MaxResolution, MaxBitrate: r.MaxBitrate, PreferredAudioLang: base.Constraints.PreferredAudioLang}
	return req
}

func TestStepDownKeepsTheUnconstrainedRungWhenAConstraintIsRequested(t *testing.T) {
	svc := stepService(true, hevcEdition("uhd", 2160, 40_000_000)) // h264-only client
	base := negotiateOK(t, svc, stepReq())
	if base.Tier != TierTranscode {
		t.Fatalf("tier = %s, want a transcode of the HEVC 4K file", base.Tier)
	}
	if len(base.StepDown) != 4 || !base.StepDown[0].Current {
		t.Fatalf("ladder = %v, want the 4K transcode current above 1080p/720p/480p", labelsOf(base.StepDown))
	}
	req := stepReq()
	req.Constraints = Constraints{MaxResolution: "1080p", MaxBitrate: 8_000_000}
	got := negotiateOK(t, svc, req)
	want := labelsOf(base.StepDown)
	want[0] = strings.TrimSuffix(want[0], "*")
	want[1] += "*"
	if fmt.Sprint(labelsOf(got.StepDown)) != fmt.Sprint(want) {
		t.Errorf("1080p request: ladder %v, want %v", labelsOf(got.StepDown), want)
	}
}

func TestStepDownEveryRungRequestedByItselfComesBackCurrent(t *testing.T) {
	music := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	cases := map[string]struct {
		eds     []store.Edition
		profile DeviceProfile
	}{
		"editions": {ladderEditions(), uhdProfile()},
		"single":   {[]store.Edition{edition("uhd", "", 2160, 40_000_000)}, uhdProfile()},
		"hevc4k":   {[]store.Edition{hevcEdition("uhd", 2160, 40_000_000)}, uhdProfile()},
		"music":    {[]store.Edition{music}, musicProfile()},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc := stepService(true, c.eds...)
			base := stepReq()
			base.Profile = c.profile
			first := negotiateOK(t, svc, base)
			if len(first.StepDown) < 2 {
				t.Fatalf("ladder %v has no rungs to request", labelsOf(first.StepDown))
			}
			for i, rung := range first.StepDown {
				got := negotiateOK(t, svc, askRung(base, rung))
				if n := countCurrent(got.StepDown); n != 1 {
					t.Fatalf("rung %q: %d current rungs, want 1 (%v)", rung.Label, n, labelsOf(got.StepDown))
				}
				if len(got.StepDown) != len(first.StepDown) {
					t.Errorf("rung %q: ladder %v, want the same %d rungs as %v", rung.Label, labelsOf(got.StepDown), len(first.StepDown), labelsOf(first.StepDown))
					continue
				}
				for j, g := range got.StepDown {
					if g.Label != first.StepDown[j].Label || g.Current != (j == i) {
						t.Errorf("rung %q: ladder %v, want the same ladder with rung %d current", rung.Label, labelsOf(got.StepDown), i)
						break
					}
				}
			}
		})
	}
}

func TestStepDownListsAnInsertedRungDirectlyUnderItsHeightNotByBitrate(t *testing.T) {
	svc := stepService(true, edition("uhd", "", 2160, 40_000_000))
	req := stepReq()
	req.Constraints.MaxBitrate = 3_000_000
	dec := negotiateOK(t, svc, req)
	got := labelsOf(dec.StepDown)
	want := []string{"2160p", "2160p 3 Mbps*", "1080p 8 Mbps", "720p 4 Mbps", "480p 1.5 Mbps"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ladder = %v, want %v", got, want)
	}
}

func TestStepDownOrdersByHeightThenBitrateUnknownLast(t *testing.T) {
	eds := []store.Edition{
		edition("a", "A", 1080, 0),
		edition("b", "B", 1080, 8_000_000),
		edition("c", "C", 1080, 12_000_000),
		edition("d", "D", 720, 20_000_000),
	}
	dec := negotiateOK(t, stepService(true, eds...), stepReq())
	var got []string
	for _, r := range dec.StepDown {
		if r.Kind == StepKindEdition {
			got = append(got, r.Label)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"C · 1080p", "B · 1080p", "A · 1080p", "D · 720p"}) {
		t.Errorf("edition order = %v, want C B A D, each named with its height (height, then bitrate, unknown last)", got)
	}
}

func TestStepDownRungsCarryABitrate(t *testing.T) {
	dec := negotiateOK(t, stepService(true, ladderEditions()...), stepReq())
	for _, r := range dec.StepDown {
		want := map[string]int64{"2160p": 40_000_000, "1080p": 8_000_000, "720p": 4_000_000, "480p 1.5 Mbps": 1_500_000}[r.Label]
		if r.Bitrate != want || want == 0 {
			t.Errorf("rung %q bitrate = %d, want %d", r.Label, r.Bitrate, want)
		}
	}

	music := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	req := stepReq()
	req.Profile = musicProfile()
	m := negotiateOK(t, stepService(true, music), req)
	if len(m.StepDown) != 3 || m.StepDown[0].Bitrate != 900_000 || m.StepDown[1].Bitrate != 256_000 || m.StepDown[2].Bitrate != 128_000 {
		t.Errorf("music rungs = %+v, want bitrates 900000, 256000, 128000", m.StepDown)
	}
}

func surroundTrack(bitrate int64, channels int) store.File {
	f := flacTrack(bitrate, "flac")
	f.Streams[0].Channels = channels
	return f
}

func TestMusicTranscodeNeverEncodesAboveItsCap(t *testing.T) {
	ed := store.Edition{ID: "e1", Files: []store.File{surroundTrack(2_000_000, 6)}}
	svc := stepService(true, ed)
	req := stepReq()
	req.Profile = musicProfile()
	req.Profile.MaxAudioChannels = 6
	req.Constraints.MaxBitrate = 330_000 // between the 320k limit and the 341k 5.1 default
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierTranscode {
		t.Fatalf("tier = %s, want an audio transcode", dec.Tier)
	}
	got, args := audioEncodeBitrate(t, req.Profile, req.Constraints, dec)
	if got == 0 || got > 330_000 {
		t.Errorf("-b:a = %d, want 1..330000; args: %v", got, args)
	}
}

func TestMusicCurrentRungIsLabelledWithTheChannelAwareEncodeTarget(t *testing.T) {
	for channels, want := range map[int]int64{1: 69_000, 2: 256_000, 6: 256_000} {
		ed := store.Edition{ID: "e1", Files: []store.File{surroundTrack(2_000_000, channels)}}
		req := stepReq()
		req.Profile = DeviceProfile{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 6}
		dec := negotiateOK(t, stepService(true, ed), req)
		if dec.Tier != TierTranscode {
			t.Fatalf("%d ch: tier = %s, want transcode", channels, dec.Tier)
		}
		var cur StepRung
		for _, r := range dec.StepDown {
			if r.Current {
				cur = r
			}
		}
		if cur.Bitrate != want {
			t.Errorf("%d ch: current rung %+v, want bitrate %d", channels, cur, want)
		}
	}
}

func TestOnlineStepDownRequestingAListedRungMarksThatRungWhateverTheVariant(t *testing.T) {
	// 720p is a variant of its own: a request for the 720p rung relays it untranscoded.
	got := onlineLadder(t, Constraints{MaxResolution: "720p", MaxBitrate: 4_000_000}, true, mp4("1080p"), mp4("720p"))
	wantKeys(t, got, "edition:", "transcode:720p*", "transcode:480p")
}

func TestOnlineStepDownRoundTripsEveryRungOfAMultiVariantItem(t *testing.T) {
	v := []pluginapi.OnlineVariant{mp4("1080p"), mp4("720p")}
	base := onlineLadder(t, Constraints{}, true, v...)
	for i, r := range base {
		got := onlineLadder(t, Constraints{MaxResolution: r.MaxResolution, MaxBitrate: r.MaxBitrate}, true, v...)
		if len(got) != len(base) {
			t.Fatalf("requesting rung %d: ladder %v, want %v", i, rungKeys(got), rungKeys(base))
		}
		for j := range got {
			if got[j].Current != (j == i) {
				t.Errorf("requesting rung %d (%s): ladder %v marks the wrong rung", i, r.Label, rungKeys(got))
			}
		}
	}
}

func TestMusicLadderListsNoAACRungAtOrAboveTheUnconstrainedDelivery(t *testing.T) {
	noFLAC := DeviceProfile{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2}
	for _, tc := range []struct {
		name    string
		profile DeviceProfile
		file    store.File
		want    []string
	}{
		{"mono, no FLAC", noFLAC, surroundTrack(2_000_000, 1), []string{"transcode:*"}},
		{"stereo, no FLAC", noFLAC, surroundTrack(2_000_000, 2), []string{"transcode:*", "transcode:"}},
		{"FLAC client", musicProfile(), flacTrack(900_000, "flac"), []string{"edition:e1*", "transcode:", "transcode:"}},
	} {
		req := stepReq()
		req.Profile = tc.profile
		dec := negotiateOK(t, stepService(true, store.Edition{ID: "e1", Files: []store.File{tc.file}}), req)
		wantKeys(t, dec.StepDown, tc.want...)
		if tc.name == "mono, no FLAC" && dec.StepDown[0].Bitrate != 69_000 {
			t.Errorf("%s: ladder %+v, want the 69 kbps rung", tc.name, dec.StepDown)
		}
	}
}
