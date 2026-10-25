package playback

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Rung identity follows the stream a request actually delivers, never an estimate
// (ADR-0071, DECISIONS after refuter-14): two rungs are one when they are copies of the
// same File, transcodes of one height at one -b:v, or one audio encode.

// deliveryKeys lists, per rung, the stream the rung's own request delivers, so two
// rungs that play the same thing show up as equal strings.
func deliveryKeys(t *testing.T, svc *Service, base Request, rungs []StepRung) []string {
	t.Helper()
	var out []string
	for _, r := range rungs {
		req := askRung(base, r)
		dec := negotiateOK(t, svc, req)
		cl, _ := clampToCeiling(req.Constraints, req.Scope)
		req.Constraints = cl
		if dec.Tier != TierTranscode {
			out = append(out, fmt.Sprintf("%s/%s", dec.Tier, dec.Edition.ID))
			continue
		}
		d, _, _ := deliveryOf(req.Profile, req.Constraints, dec)
		out = append(out, fmt.Sprintf("transcode copy=%v file=%s h=%d b:v=%d", d.videoCopy, d.source, d.height, d.videoBitrate))
	}
	return out
}

func checkDistinctDeliveries(t *testing.T, svc *Service, base Request, rungs []StepRung) {
	t.Helper()
	seen := map[string]string{}
	for i, k := range deliveryKeys(t, svc, base, rungs) {
		if prev, dup := seen[k]; dup {
			t.Errorf("rungs %q and %q both deliver %s: %v", prev, rungs[i].Label, k, labelsOf(rungs))
		}
		seen[k] = rungs[i].Label
	}
}

func TestStepDownVideoCopyOfUnknownBitrateIsListedOnceWhateverTheCap(t *testing.T) {
	for _, cap := range []int64{20_000_000, 8_000_000} {
		svc := stepService(true, dtsEdition("hd", 1080, 0)) // DTS audio: the video is copied
		base := stepReq()
		asked := base
		asked.Constraints = Constraints{MaxBitrate: cap}
		dec := negotiateOK(t, svc, asked)
		if !dec.VideoCopy {
			t.Fatalf("cap %d: decision is not a video copy: %+v", cap, dec)
		}
		checkNoDuplicates(t, dec.StepDown)
		checkDistinctDeliveries(t, svc, base, dec.StepDown)
		if currentIndex(dec.StepDown) < 0 {
			t.Errorf("cap %d: ladder %v has no single current rung", cap, labelsOf(dec.StepDown))
		}
		for _, r := range dec.StepDown {
			if r.Bitrate == cap {
				t.Errorf("cap %d: rung %+v carries the request's cap as its bitrate", cap, r)
			}
		}
		checkWalk(t, svc, base, negotiateOK(t, svc, base))
	}
}

func TestStepDownResolutionOnlyCeilingKeepsTheUnconstrainedAndFixedRungsApart(t *testing.T) {
	for name, tc := range map[string]struct {
		ed    store.Edition
		scope access.Scope
		fixed string
	}{
		"h264 2160p 40M under a 1080p ceiling": {edition("uhd", "", 2160, 40_000_000), access.Scope{AllLibraries: true, MaxResolution: "1080p"}, "1080p"},
		"DTS 1080p under a 720p ceiling":       {dtsEdition("hd", 1080, 8_000_000), access.Scope{AllLibraries: true, MaxResolution: "720p"}, "720p"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := stepService(true, tc.ed)
			base := stepReq()
			base.Scope = tc.scope
			dec := negotiateOK(t, svc, base)
			checkNoDuplicates(t, dec.StepDown)
			checkDistinctDeliveries(t, svc, base, dec.StepDown)
			top := dec.StepDown[0]
			if !top.Current || top.MaxResolution != "" || top.MaxBitrate != 0 {
				t.Errorf("ladder %v: want the unconstrained rung first and current, got %+v", labelsOf(dec.StepDown), top)
			}
			// A re-encode with no -b:v has no bitrate we know (DECISIONS, after refuter-19).
			if top.Bitrate != 0 {
				t.Errorf("unconstrained rung bitrate %d, want it unknown (the encode has no -b:v)", top.Bitrate)
			}
			var fixed *StepRung
			for i, r := range dec.StepDown {
				if r.MaxResolution == tc.fixed && r.MaxBitrate > 0 {
					fixed = &dec.StepDown[i]
				}
			}
			if fixed == nil || fixed.Current || fixed.Bitrate != fixed.MaxBitrate {
				t.Errorf("ladder %+v: want a separate, not-current %s rung at its own bitrate", dec.StepDown, tc.fixed)
			}
			checkWalk(t, svc, base, dec)
		})
	}
}

func TestStepDownLabelsAreUnique(t *testing.T) {
	both := uhdProfile()
	both.VideoCodecs = append(both.VideoCodecs, VideoCodecSupport{Codec: "hevc", MaxResolution: "2160p"})
	for name, tc := range map[string]struct {
		eds     []store.Edition
		profile DeviceProfile
		want    []string // substrings the disambiguated labels must contain, any order
	}{
		"same height, different bitrate": {[]store.Edition{edition("a", "", 1080, 8_000_000), edition("b", "", 1080, 6_000_000)}, uhdProfile(), []string{"8 Mbps", "6 Mbps"}},
		"same height, different codec":   {[]store.Edition{edition("a", "", 1080, 8_000_000), hevcEdition("b", 1080, 8_000_000)}, both, []string{"h264", "hevc"}},
		"identical":                      {[]store.Edition{edition("a", "", 1080, 8_000_000), edition("b", "", 1080, 8_000_000)}, uhdProfile(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			svc := stepService(true, tc.eds...)
			base := stepReq()
			base.Profile = tc.profile
			dec := negotiateOK(t, svc, base)
			checkNoDuplicates(t, dec.StepDown)
			joined := strings.Join(labelsOf(dec.StepDown), "|")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("labels %v: want one naming %q", labelsOf(dec.StepDown), w)
				}
			}
			checkWalk(t, svc, base, dec)
		})
	}
}

func TestStepDownAudioEditionLabelsAreUnique(t *testing.T) {
	a := store.Edition{ID: "a", Files: []store.File{flacTrack(900_000, "flac")}}
	b := store.Edition{ID: "b", Files: []store.File{flacTrack(1_200_000, "flac")}}
	b.Files[0].ID = "tr2"
	c := store.Edition{ID: "c", Files: []store.File{flacTrack(320_000, "aac")}}
	c.Files[0].ID = "tr3"
	svc := stepService(true, a, b, c)
	base := stepReq()
	base.Profile = musicProfile()
	dec := negotiateOK(t, svc, base)
	checkNoDuplicates(t, dec.StepDown)
	n := 0
	for _, r := range dec.StepDown {
		if strings.HasPrefix(r.Label, "Original") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("labels %v: want three Original rungs, each told apart", labelsOf(dec.StepDown))
	}
}

// musicRungsAgreeWithTheEncode requests every rung and wants the -b:a its ffmpeg args
// carry to be the rung's bitrate, and never above the cap or the source.
func musicRungsAgreeWithTheEncode(t *testing.T, svc *Service, base Request, source int64) {
	t.Helper()
	dec := negotiateOK(t, svc, base)
	for _, r := range dec.StepDown {
		req := askRung(base, r)
		d := negotiateOK(t, svc, req)
		if d.Tier != TierTranscode {
			continue
		}
		cl, _ := clampToCeiling(req.Constraints, req.Scope)
		b, args := audioEncodeBitrate(t, req.Profile, cl, d)
		if b != r.Bitrate {
			t.Errorf("rung %q says bitrate %d but encodes -b:a %d; args %v", r.Label, r.Bitrate, b, args)
		}
		if cl.MaxBitrate > 0 && b > cl.MaxBitrate {
			t.Errorf("rung %q encodes %d above the cap %d", r.Label, b, cl.MaxBitrate)
		}
		if source > 0 && b > source {
			t.Errorf("rung %q encodes %d above the source's %d", r.Label, b, source)
		}
	}
	checkWalk(t, svc, base, dec)
}

func TestMusicEncodeAndRungAgreeAndStayUnderTheSource(t *testing.T) {
	aac := DeviceProfile{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 6}
	for name, tc := range map[string]struct {
		file    store.File
		profile DeviceProfile
		scope   access.Scope
		want    int64 // the unconstrained rung's bitrate
	}{
		"mp3 192k":           {surroundFile("mp3", 192_000, 2), aac, access.Scope{AllLibraries: true}, 192_000},
		"mp3 100k":           {surroundFile("mp3", 100_000, 2), aac, access.Scope{AllLibraries: true}, 100_000},
		"flac 3 channels":    {surroundFile("flac", 2_000_000, 3), aac, access.Scope{AllLibraries: true}, 144_000},
		"flac 4 channels":    {surroundFile("flac", 2_000_000, 4), aac, access.Scope{AllLibraries: true}, 256_000},
		"flac mono":          {surroundFile("flac", 900_000, 1), aac, access.Scope{AllLibraries: true}, 69_000},
		"flac 5.1":           {surroundFile("flac", 2_000_000, 6), aac, access.Scope{AllLibraries: true}, 256_000},
		"flac ceiling 200k":  {surroundFile("flac", 900_000, 2), musicProfile(), access.Scope{AllLibraries: true, MaxBitrate: 200_000}, 200_000},
		"flac 5.1 ceil 300k": {surroundFile("flac", 2_000_000, 6), aac, access.Scope{AllLibraries: true, MaxBitrate: 300_000}, 300_000},
		"aac 320k":           {surroundFile("aac", 320_000, 2), musicProfile(), access.Scope{AllLibraries: true}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			svc := stepService(true, store.Edition{ID: "e1", Files: []store.File{tc.file}})
			base := stepReq()
			base.Profile = tc.profile
			base.Scope = tc.scope
			dec := negotiateOK(t, svc, base)
			if tc.want != 0 && dec.Tier == TierTranscode {
				if cur := dec.StepDown[currentIndex(dec.StepDown)]; cur.Bitrate != tc.want {
					t.Errorf("current rung %+v, want bitrate %d", cur, tc.want)
				}
			}
			musicRungsAgreeWithTheEncode(t, svc, base, tc.file.Bitrate)
		})
	}
}

func surroundFile(codec string, bitrate int64, channels int) store.File {
	f := flacTrack(bitrate, codec)
	f.Streams[0].Channels = channels
	return f
}

// The defaults ffmpeg 9.0.1's AAC encoder produced for each layout (see
// audioEncodeDefaults); a count with no measured layout reads as the one below it.
func TestAudioEncodeDefaultsAreTheMeasuredOnesRoundedDown(t *testing.T) {
	for ch, want := range map[int]int64{0: 128_000, 1: 69_000, 2: 128_000, 3: 144_000, 4: 266_000, 5: 325_000, 6: 341_000, 7: 341_000, 8: 469_000, 12: 469_000} {
		if got := audioEncodeDefaultFor(ch); got != want {
			t.Errorf("audioEncodeDefaultFor(%d) = %d, want %d", ch, got, want)
		}
	}
	for ch, want := range map[int]int64{1: 69_000, 2: 256_000, 3: 144_000, 4: 256_000, 6: 256_000, 8: 256_000} {
		if got := audioEncodeUncapped(ch); got != want {
			t.Errorf("audioEncodeUncapped(%d) = %d, want %d", ch, got, want)
		}
	}
}

// relayWalk plays a sharer with three Editions behind a home Server with the given
// ceiling, asking for every rung of the home ladder in turn.
func relayWalk(t *testing.T, homeScope access.Scope) []any {
	t.Helper()
	sharer := stepService(true, edition("re1", "", 2160, 40_000_000), edition("re2", "", 1080, 8_000_000), edition("re3", "", 720, 4_000_000))
	hst := ladderRelayStore{ceilingStore{detail: titleWith(edition("e1", "", 2160, 40_000_000), edition("e2", "", 1080, 8_000_000), edition("e3", "", 720, 4_000_000))}}
	home := NewService(hst, nil, "", Governance{})
	ask := func(editionID string, c Constraints) []any {
		cl, _ := clampToCeiling(c, homeScope)
		req := stepReq()
		req.Constraints = cl
		if editionID != "" {
			req.EditionID = "r" + editionID
		}
		var raw []any
		for _, r := range stepRungsAsJSON(negotiateOK(t, sharer, req).StepDown) {
			raw = append(raw, r)
		}
		return home.relayStepDown(raw, hst.detail, homeScope, editionID, c)
	}
	base := ask("", Constraints{})
	for i, e := range base {
		m := e.(map[string]any)
		id, _ := m["editionId"].(string)
		var c Constraints
		if cc, ok := m["constraints"].(map[string]any); ok {
			c.MaxResolution, _ = cc["maxResolution"].(string)
			c.MaxBitrate = jsonInt(cc["maxBitrate"])
		}
		got := ask(id, c)
		cur, n := -1, 0
		for j, g := range got {
			if g.(map[string]any)["current"] == true {
				cur = j
				n++
			}
		}
		if len(got) != len(base) || cur != i || n != 1 {
			t.Errorf("requesting rung %d %v: ladder %v, want %d rungs with that one current", i, m["label"], got, len(base))
		}
	}
	return base
}

func TestRelayStepDownUnderAResolutionCeilingKeepsTheRequestedRungCurrent(t *testing.T) {
	base := relayWalk(t, access.Scope{AllLibraries: true, MaxResolution: "1080p"})
	for _, e := range base {
		m := e.(map[string]any)
		if strings.Contains(m["label"].(string), "2160") || jsonInt(m["bitrate"]) > 8_000_000 {
			t.Errorf("rung %v is above the 1080p ceiling: %v", m["label"], base)
		}
	}
	if top := base[0].(map[string]any); top["editionId"] != "e2" || top["current"] != true {
		t.Errorf("top rung %v, want the 1080p Edition e2, current", top)
	}
}

func TestRelayStepDownUnderABitrateCeilingStartsAtWhatTheSharerPlays(t *testing.T) {
	for name, tc := range map[string]struct {
		ceil int64
		top  string
	}{"5M": {5_000_000, "e3"}, "10M": {10_000_000, "e2"}} {
		t.Run(name, func(t *testing.T) {
			base := relayWalk(t, access.Scope{AllLibraries: true, MaxBitrate: tc.ceil})
			top := base[0].(map[string]any)
			if top["editionId"] != tc.top || top["current"] != true || jsonInt(top["bitrate"]) > tc.ceil {
				t.Errorf("top rung %v, want %s current within the ceiling", top, tc.top)
			}
		})
	}
}

// A cap that changes nothing about the stream (the source's bitrate is unknown, so there
// is no -b:v) is the unconstrained delivery, so it is that rung, listed once.
func TestStepDownCapThatAddsNoBitrateArgumentIsTheUnconstrainedRung(t *testing.T) {
	svc := stepService(true, hevcEdition("uhd", 2160, 0)) // h264-only client
	base := stepReq()
	asked := base
	asked.Constraints = Constraints{MaxBitrate: 20_000_000}
	dec := negotiateOK(t, svc, asked)
	if c := currentIndex(dec.StepDown); c != 0 || dec.StepDown[0].MaxBitrate != 0 {
		t.Errorf("ladder %v: want the unconstrained rung first and current", labelsOf(dec.StepDown))
	}
	checkNoDuplicates(t, dec.StepDown)
	checkDistinctDeliveries(t, svc, base, dec.StepDown)
}
