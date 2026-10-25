package playback

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The ladder is derived from the real planner (ADR-0071, DECISIONS after refuter-16):
// every rung's label, bitrate and identity are what its own request delivers. These
// tests ask the planner independently (the real Negotiate and the real ffmpeg plan) and
// hold every rung of a grid of Title sets, ceilings and profiles to it.

// plannedRung is what a rung's own request delivers, read from the real planner.
type plannedRung struct {
	key     string // the stream: two rungs with one key are one delivery
	height  int    // delivered picture height, 0 for audio or unknown
	bitrate int64  // honest delivered bitrate: -b:v / -b:a, else the File's; 0 unknown
	encode  bool
	audio   bool
}

func planned(t *testing.T, svc *Service, base Request, r StepRung) (plannedRung, Request) {
	t.Helper()
	req := askRung(base, r)
	dec := negotiateOK(t, svc, req)
	cl, _ := clampToCeiling(req.Constraints, req.Scope)
	srcH := dec.VideoStream.Height
	if srcH == 0 {
		srcH = dec.File.Height
	}
	if dec.Tier != TierTranscode {
		key := fmt.Sprintf("%s/%s", dec.Tier, dec.File.ID)
		if dec.Tier != TierDirectPlay {
			key += fmt.Sprintf("/%s/%s", dec.VideoStream.ID, dec.AudioStream.ID)
		} else if !dec.AudioOnly {
			// Direct play plays the File's default video track, not the recorded stream.
			if v, ok := pickVideoStream(dec.File); ok && v.Height > 0 {
				srcH = v.Height
			} else {
				srcH = dec.File.Height
			}
		}
		return plannedRung{key: key, height: srcH, bitrate: dec.File.Bitrate, audio: dec.AudioOnly}, req
	}
	job := transcodeJobPlan(req.Profile, cl, dec)
	if dec.AudioOnly {
		b, _ := audioEncodeBitrate(t, req.Profile, cl, dec)
		p := plannedRung{key: fmt.Sprintf("audio/%s/copy=%v/b=%d", dec.File.ID, job.Audio.Copy, b), bitrate: dec.File.Bitrate, audio: true}
		if !job.Audio.Copy {
			p.encode, p.bitrate = true, b
		}
		return p, req
	}
	v := job.Video
	if v.Copy {
		return plannedRung{key: fmt.Sprintf("copy/%s/%s/audioCopy=%v/%s", dec.File.ID, dec.VideoStream.ID, job.Audio.Copy, copiedAudio(job, dec)), height: srcH, bitrate: dec.File.Bitrate}, req
	}
	h := srcH
	if v.MaxHeight > 0 && (h == 0 || h > v.MaxHeight) {
		h = v.MaxHeight
	}
	p := plannedRung{key: fmt.Sprintf("enc/%s/%d/%d/audioCopy=%v", dec.File.ID, h, v.MaxBitrate, job.Audio.Copy), height: h, bitrate: v.MaxBitrate, encode: true}
	return p, req
}

// copiedAudio is the audio stream a plan copies, "" when it encodes the audio.
func copiedAudio(job transcode.TranscodeJob, dec Decision) string {
	if job.Audio.Copy {
		return dec.AudioStream.ID
	}
	return ""
}

// checkDerived holds a ladder to the five properties of the brief: (a) requesting a rung
// exactly returns the same ladder with that rung current, (b) its label and bitrate are
// what the planner delivers for it, (c) the delivery is within the ceiling and the
// device, (d) no two rungs share a delivery or a label, (e) stepping down and back up
// reaches the top.
func checkDerived(t *testing.T, name string, svc *Service, base Request) {
	t.Helper()
	first, _, unsup, busy, err := svc.Negotiate(base)
	if err != nil || unsup != nil || busy != nil {
		return
	}
	L := first.StepDown
	if currentIndex(L) < 0 {
		t.Errorf("%s: want exactly one current rung: %v", name, labelsOf(L))
		return
	}
	ceilH, ceilBR := resolutionHeight(base.Scope.MaxResolution), base.Scope.MaxBitrate
	keys, labels := map[string]string{}, map[string]bool{}
	prevH := 1 << 30
	for i, r := range L {
		p, req := planned(t, svc, base, r)
		// The height field is the delivered height.
		if r.Height != p.height {
			t.Errorf("%s: rung %q says height %d, the planner delivers %d: %v", name, r.Label, r.Height, p.height, labelsOf(L))
		}
		// (b)
		if r.Bitrate != p.bitrate {
			t.Errorf("%s: rung %q says bitrate %d, the planner delivers %d: %v", name, r.Label, r.Bitrate, p.bitrate, labelsOf(L))
		}
		switch {
		case p.audio && p.encode:
			if !strings.Contains(r.Label, fmt.Sprintf("%d kbps", p.bitrate/1000)) {
				t.Errorf("%s: rung %q, the planner encodes at %d", name, r.Label, p.bitrate)
			}
		case p.encode || strings.HasPrefix(p.key, "copy/"):
			if p.height > 0 && !strings.HasPrefix(r.Label, fmt.Sprintf("%dp", p.height)) && r.Kind == StepKindTranscode {
				t.Errorf("%s: rung %q, the planner delivers %dp", name, r.Label, p.height)
			}
		}
		// A label that names a height names the delivered video stream's, whatever the kind.
		if h, ok := labelOpensWithHeight(r.Label); ok && !p.audio && p.height > 0 && h != p.height {
			t.Errorf("%s: rung %q, the planner delivers %dp", name, r.Label, p.height)
		}
		// (c)
		if ceilBR > 0 && p.bitrate > ceilBR {
			t.Errorf("%s: rung %q delivers %d bps over the %d ceiling", name, r.Label, p.bitrate, ceilBR)
		}
		if ceilH > 0 && p.height > ceilH {
			t.Errorf("%s: rung %q delivers %dp over the %dp ceiling", name, r.Label, p.height, ceilH)
		}
		if cl, _ := clampToCeiling(req.Constraints, req.Scope); p.encode && !p.audio {
			if dev := reencodeCapHeight(req.Profile, cl); dev > 0 && p.height > dev {
				t.Errorf("%s: rung %q delivers %dp over the device's %dp", name, r.Label, p.height, dev)
			}
		}
		if p.height > prevH {
			t.Errorf("%s: rung %q delivers %dp, taller than the rung above it (%dp)", name, r.Label, p.height, prevH)
		}
		prevH = p.height
		// (d)
		if prev, dup := keys[p.key]; dup {
			t.Errorf("%s: rungs %q and %q deliver the same stream %s", name, prev, r.Label, p.key)
		}
		keys[p.key] = r.Label
		if labels[r.Label] {
			t.Errorf("%s: label %q listed twice: %v", name, r.Label, labelsOf(L))
		}
		labels[r.Label] = true
		// (a)
		got := negotiateOK(t, svc, req).StepDown
		same := len(got) == len(L) && currentIndex(got) == i
		for j := 0; same && j < len(got); j++ {
			same = got[j].Label == L[j].Label && got[j].Bitrate == L[j].Bitrate && rungKey(got[j]) == rungKey(L[j])
		}
		if !same {
			t.Errorf("%s: requesting rung %d %q gives %v, want the same ladder %v with it current", name, i, r.Label, labelsOf(got), labelsOf(L))
		}
	}
	// (e)
	checkWalk(t, svc, base, first)
	// The client's own constraints change which rung is current, never the rungs.
	for _, own := range []Constraints{{MaxBitrate: 20_000_000}, {MaxBitrate: 3_000_000}, {MaxResolution: "720p"}, {MaxResolution: "1080p", MaxBitrate: 6_000_000}, {MaxBitrate: 192_000}} {
		q := base
		q.Constraints = own
		q.Constraints.PreferredAudioLang = base.Constraints.PreferredAudioLang
		dq, _, u, b, e := svc.Negotiate(q)
		if e != nil || u != nil || b != nil {
			continue
		}
		if currentIndex(dq.StepDown) < 0 {
			t.Errorf("%s own %+v: want one current rung: %v", name, own, labelsOf(dq.StepDown))
		}
		have := map[string]bool{}
		for _, r := range dq.StepDown {
			have[rungKey(r)] = true
		}
		for _, r := range L {
			if !have[rungKey(r)] {
				t.Errorf("%s own %+v: lost rung %q: %v", name, own, r.Label, labelsOf(dq.StepDown))
			}
		}
		checkNoDuplicates(t, dq.StepDown)
	}
}

func mkvEdition(id string, h int, br int64) store.Edition {
	e := edition(id, "", h, br)
	e.Files[0].Container = "matroska,webm"
	return e
}

func TestStepDownVideoLadderIsDerivedFromThePlanner(t *testing.T) {
	all := access.Scope{AllLibraries: true}
	mp4only := uhdProfile()
	mp4only.Containers = []string{"mp4"}
	dev720 := uhdProfile()
	dev720.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}
	hevcOK := uhdProfile()
	hevcOK.VideoCodecs = append(hevcOK.VideoCodecs, VideoCodecSupport{Codec: "hevc", MaxResolution: "2160p"})
	type set struct {
		eds  []store.Edition
		prof *DeviceProfile
	}
	sets := map[string]set{
		"ladder":         {ladderEditions(), nil},
		"h264-4k":        {[]store.Edition{edition("uhd", "", 2160, 40_000_000)}, nil},
		"h264-4k-unk":    {[]store.Edition{edition("uhd", "", 2160, 0)}, nil},
		"h264-1080-6M":   {[]store.Edition{edition("hd", "", 1080, 6_000_000)}, nil},
		"h264-1080-unk":  {[]store.Edition{edition("hd", "", 1080, 0)}, nil},
		"hevc4k":         {[]store.Edition{hevcEdition("uhd", 2160, 40_000_000)}, nil},
		"hevc4k-unk":     {[]store.Edition{hevcEdition("uhd", 2160, 0)}, nil},
		"hevc720":        {[]store.Edition{hevcEdition("hd", 720, 3_000_000)}, nil},
		"hevc1080-5M":    {[]store.Edition{hevcEdition("hd", 1080, 5_000_000)}, nil},
		"dts1080":        {[]store.Edition{dtsEdition("hd", 1080, 8_000_000)}, nil},
		"dts1080-unk":    {[]store.Edition{dtsEdition("hd", 1080, 0)}, nil},
		"dts4k":          {[]store.Edition{dtsEdition("uhd", 2160, 40_000_000)}, nil},
		"mkv1080":        {[]store.Edition{mkvEdition("hd", 1080, 8_000_000)}, &mp4only},
		"hevc4k+hd":      {[]store.Edition{hevcEdition("uhd", 2160, 40_000_000), edition("hd", "", 1080, 8_000_000)}, nil},
		"hevc4k-copy":    {[]store.Edition{hevcEdition("uhd", 2160, 40_000_000)}, &hevcOK},
		"h264-4k-dev720": {[]store.Edition{edition("uhd", "", 2160, 40_000_000)}, &dev720},
		"two1080":        {[]store.Edition{edition("a", "", 1080, 8_000_000), edition("b", "", 1080, 8_000_000)}, nil},
	}
	scopes := map[string]access.Scope{
		"none": all, "1080": {AllLibraries: true, MaxResolution: "1080p"}, "720": {AllLibraries: true, MaxResolution: "720p"},
		"5M": {AllLibraries: true, MaxBitrate: 5_000_000}, "10M": {AllLibraries: true, MaxBitrate: 10_000_000},
		"1080-8M": {AllLibraries: true, MaxResolution: "1080p", MaxBitrate: 8_000_000}, "2M": {AllLibraries: true, MaxBitrate: 2_000_000},
	}
	for sn, s := range sets {
		for cn, scope := range scopes {
			svc := stepService(true, s.eds...)
			base := stepReq()
			base.Scope = scope
			if s.prof != nil {
				base.Profile = *s.prof
			}
			checkDerived(t, sn+"/"+cn, svc, base)
		}
	}
}

func TestStepDownMusicLadderIsDerivedFromThePlanner(t *testing.T) {
	type src struct {
		codec, container string
		br               int64
	}
	srcs := []src{{"flac", "flac", 2_000_000}, {"flac", "flac", 0}, {"mp3", "mp3", 320_000}, {"mp3", "mp3", 192_000}, {"mp3", "mp3", 100_000},
		{"aac", "mp4", 320_000}, {"aac", "matroska", 320_000}, {"aac", "matroska", 96_000}, {"alac", "mp4", 1_000_000}}
	profs := map[string]DeviceProfile{
		"aac0":  {Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}},
		"aac2":  {Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2},
		"aac6":  {Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 6},
		"flac2": musicProfile(),
		"flac8": {Containers: []string{"flac", "mp4", "aac"}, AudioCodecs: []string{"flac", "aac", "mp3"}, MaxAudioChannels: 8},
	}
	scopes := map[string]access.Scope{"none": {AllLibraries: true}, "200k": {AllLibraries: true, MaxBitrate: 200_000}, "128k": {AllLibraries: true, MaxBitrate: 128_000},
		"100k": {AllLibraries: true, MaxBitrate: 100_000}, "1M": {AllLibraries: true, MaxBitrate: 1_000_000}}
	for _, s := range srcs {
		for _, ch := range []int{0, 1, 2, 3, 6, 8} {
			for pn, p := range profs {
				for sn, sc := range scopes {
					f := flacTrack(s.br, s.codec)
					f.Container = s.container
					f.Streams[0].Channels = ch
					svc := stepService(true, store.Edition{ID: "e1", Files: []store.File{f}})
					base := stepReq()
					base.Profile, base.Scope = p, sc
					checkDerived(t, fmt.Sprintf("%s-%s-%d-ch%d-%s-%s", s.codec, s.container, s.br, ch, pn, sn), svc, base)
				}
			}
		}
	}
}

// MF1: a fixed rung is clamped to the ceiling like any request, not overwritten by it,
// so it is neither above the ceiling nor labelled for a delivery it does not make.
func TestStepDownFixedRungUnderACeilingIsLabelledForWhatItDelivers(t *testing.T) {
	svc := stepService(true, hevcEdition("hd", 720, 3_000_000)) // h264-only client
	base := stepReq()
	base.Scope = access.Scope{AllLibraries: true, MaxBitrate: 5_000_000}
	dec := negotiateOK(t, svc, base)
	for _, r := range dec.StepDown {
		if r.Bitrate > 5_000_000 || r.MaxBitrate > 5_000_000 {
			t.Errorf("rung %+v is above the 5M ceiling: %v", r, labelsOf(dec.StepDown))
		}
	}
	checkDerived(t, "hevc720/5M", svc, base)
	base.Scope = access.Scope{AllLibraries: true, MaxResolution: "720p"}
	for _, r := range negotiateOK(t, svc, base).StepDown {
		if resolutionHeight(r.MaxResolution) > 720 {
			t.Errorf("rung %+v is above the 720p ceiling", r)
		}
	}
}

// MF2: the device's own codec ceiling bounds the rungs: a constrained rung can never
// sit above the unconstrained one or be labelled taller than it delivers.
func TestStepDownRungsRespectTheDevicesCodecCeiling(t *testing.T) {
	svc := stepService(true, edition("uhd", "", 2160, 40_000_000))
	base := stepReq()
	base.Profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "720p"}}
	dec := negotiateOK(t, svc, base)
	for _, r := range dec.StepDown {
		if strings.HasPrefix(r.Label, "1080p") {
			t.Errorf("a 720p device is offered %q: %v", r.Label, labelsOf(dec.StepDown))
		}
	}
	if !dec.StepDown[0].Current || dec.StepDown[0].MaxResolution != "" {
		t.Errorf("want the unconstrained rung first and current: %v", labelsOf(dec.StepDown))
	}
	checkDerived(t, "dev720", svc, base)
}

// MF4: an audio-only source of unknown bitrate cannot be shown to fit a cap, so a cap
// forces the AAC encode, and the AAC rungs are really different streams.
func TestMusicOfUnknownBitrateEncodesUnderACap(t *testing.T) {
	svc := stepService(true, store.Edition{ID: "e1", Files: []store.File{flacTrack(0, "flac")}})
	base := stepReq()
	base.Profile = musicProfile()
	dec := negotiateOK(t, svc, base)
	if dec.Tier != TierDirectPlay || len(dec.StepDown) != 3 {
		t.Fatalf("tier %s, ladder %v: want direct play of the original and two AAC rungs", dec.Tier, labelsOf(dec.StepDown))
	}
	for _, r := range dec.StepDown[1:] {
		req := askRung(base, r)
		got := negotiateOK(t, svc, req)
		if got.Tier != TierTranscode {
			t.Fatalf("rung %q plays as %s, want an AAC transcode", r.Label, got.Tier)
		}
		if b, args := audioEncodeBitrate(t, req.Profile, req.Constraints, got); b != r.Bitrate {
			t.Errorf("rung %q encodes at %d, want %d; args %v", r.Label, b, r.Bitrate, args)
		}
	}
	checkDerived(t, "flac-unknown", svc, base)
}

// MF3: a sharer rung with neither editionId nor constraints is judged against the home
// ceiling by its bitrate, and rungs the home ceiling makes one request are one rung.
func TestRelayStepDownJudgesABareSharerRungByItsBitrate(t *testing.T) {
	sets := map[string][2][]store.Edition{
		"hevc4k": {{hevcEdition("re1", 2160, 40_000_000)}, {hevcEdition("e1", 2160, 40_000_000)}},
		"unk4k":  {{edition("re1", "", 2160, 0)}, {edition("e1", "", 2160, 0)}},
		"4k+hd":  {{edition("re1", "", 2160, 40_000_000), edition("re2", "", 1080, 8_000_000)}, {edition("e1", "", 2160, 40_000_000), edition("e2", "", 1080, 8_000_000)}},
	}
	ceilings := map[string]access.Scope{
		"res1080": {AllLibraries: true, MaxResolution: "1080p"},
		"res720":  {AllLibraries: true, MaxResolution: "720p"},
		"br5M":    {AllLibraries: true, MaxBitrate: 5_000_000},
		"1080-5M": {AllLibraries: true, MaxResolution: "1080p", MaxBitrate: 5_000_000},
		"none":    {AllLibraries: true},
	}
	for sn, set := range sets {
		for cn, home := range ceilings {
			sharer := stepService(true, set[0]...)
			svc := NewService(ladderRelayStore{ceilingStore{detail: titleWith(set[1]...)}}, nil, "", Governance{})
			ask := func(editionID string, c Constraints) []any {
				cl, _ := clampToCeiling(c, home)
				req := stepReq()
				req.Constraints = cl
				if req.EditionID = editionID; editionID != "" {
					req.EditionID = "r" + editionID
				}
				d := negotiateOK(t, sharer, req)
				return svc.relayStepDown(ladderToAny(d.StepDown), svc.store.(ladderRelayStore).detail, home, editionID, c)
			}
			askMap := func(m map[string]any) []any {
				id, _ := m["editionId"].(string)
				var c Constraints
				if cc, ok := m["constraints"].(map[string]any); ok {
					c.MaxResolution, _ = cc["maxResolution"].(string)
					c.MaxBitrate = jsonInt(cc["maxBitrate"])
				}
				return ask(id, c)
			}
			name := sn + "/" + cn
			base := ask("", Constraints{})
			labels := map[any]bool{}
			for i, e := range base {
				m := e.(map[string]any)
				if labels[m["label"]] {
					t.Errorf("%s: label %v listed twice: %s", name, m["label"], relayDump(base))
				}
				labels[m["label"]] = true
				if br := jsonInt(m["bitrate"]); home.MaxBitrate > 0 && br > home.MaxBitrate {
					t.Errorf("%s: rung %v bitrate %d is above the ceiling: %s", name, m["label"], br, relayDump(base))
				}
				got := askMap(m)
				n, cur := 0, -1
				for j, g := range got {
					if g.(map[string]any)["current"] == true {
						n, cur = n+1, j
					}
				}
				if len(got) != len(base) || cur != i || n != 1 {
					t.Errorf("%s: requesting %d %v gives %s, want the same %d rungs with it current: %s", name, i, m["label"], relayDump(got), len(base), relayDump(base))
				}
			}
		}
	}
}

func ladderToAny(rs []StepRung) []any {
	var out []any
	for _, r := range rs {
		m := map[string]any{"label": r.Label, "kind": r.Kind}
		if r.EditionID != "" {
			m["editionId"] = r.EditionID
		}
		if r.MaxResolution != "" || r.MaxBitrate > 0 {
			c := map[string]any{}
			if r.MaxResolution != "" {
				c["maxResolution"] = r.MaxResolution
			}
			if r.MaxBitrate > 0 {
				c["maxBitrate"] = float64(r.MaxBitrate)
			}
			m["constraints"] = c
		}
		if r.Bitrate > 0 {
			m["bitrate"] = float64(r.Bitrate)
		}
		if r.Current {
			m["current"] = true
		}
		out = append(out, m)
	}
	return out
}

func relayDump(l []any) string {
	s := ""
	for _, e := range l {
		m := e.(map[string]any)
		c := ""
		if m["current"] == true {
			c = "*"
		}
		s += fmt.Sprintf("[%v%s ed=%v c=%v br=%v] ", m["label"], c, m["editionId"], m["constraints"], m["bitrate"])
	}
	return s
}

// The Online ladder is derived the same way: each candidate request is planned and the
// rung describes what it plans.
func TestOnlineStepDownIsDerivedFromThePlanner(t *testing.T) {
	variantSets := map[string][]pluginapi.OnlineVariant{
		"1080":       {mp4("1080p")},
		"720":        {mp4("720p")},
		"480":        {mp4("480p")},
		"1080+720":   {mp4("1080p"), mp4("720p")},
		"unknown":    {{URL: "https://m.example/x.mp4", Container: "mp4", Codecs: []string{"h264", "aac"}}},
		"4k-webm":    {{URL: "https://m.example/x.webm", Container: "webm", Codecs: []string{"vp9", "opus"}, Resolution: "2160p"}, mp4("720p")},
		"only-vp9":   {{URL: "https://m.example/x.webm", Container: "webm", Codecs: []string{"vp9", "opus"}, Resolution: "1080p"}},
		"2160-mp4":   {mp4("2160p")},
		"1080-and-4": {mp4("1080p"), mp4("480p")},
	}
	ceilings := map[string]access.Scope{
		"none": {}, "720": {MaxResolution: "720p"}, "5M": {MaxBitrate: 5_000_000}, "1080-3M": {MaxResolution: "1080p", MaxBitrate: 3_000_000}, "480": {MaxResolution: "480p"},
	}
	for vn, variants := range variantSets {
		for cn, scope := range ceilings {
			for _, can := range []bool{true, false} {
				ceiling, _ := ClampToCeiling(Constraints{}, scope)
				ladder := func(asked Constraints) ([]StepRung, bool) {
					c, _ := ClampToCeiling(asked, scope)
					plan, unsup := PlanOnline(onlineProfile(), c, variants, can)
					if unsup != nil {
						return nil, false
					}
					return OnlineStepDown(onlineProfile(), ceiling, asked, c, variants, plan, can), true
				}
				name := fmt.Sprintf("%s/%s/ffmpeg=%v", vn, cn, can)
				L, ok := ladder(Constraints{})
				if !ok {
					continue
				}
				if currentIndex(L) < 0 {
					t.Errorf("%s: want one current rung: %v", name, labelsOf(L))
					continue
				}
				ceilH := resolutionHeight(scope.MaxResolution)
				labels := map[string]bool{}
				for i, r := range L {
					if labels[r.Label] {
						t.Errorf("%s: label %q listed twice: %v", name, r.Label, labelsOf(L))
					}
					labels[r.Label] = true
					asked := Constraints{MaxResolution: r.MaxResolution, MaxBitrate: r.MaxBitrate}
					c, _ := ClampToCeiling(asked, scope)
					plan, unsup := PlanOnline(onlineProfile(), c, variants, can)
					if unsup != nil {
						t.Errorf("%s: rung %q does not plan: %v", name, r.Label, unsup)
						continue
					}
					if plan.Transcode && r.Bitrate != plan.MaxBitrate || !plan.Transcode && r.Bitrate != 0 {
						t.Errorf("%s: rung %q says bitrate %d, the plan delivers transcode=%v at %d", name, r.Label, r.Bitrate, plan.Transcode, plan.MaxBitrate)
					}
					if h := onlineHeight(plan); ceilH > 0 && h > ceilH || scope.MaxBitrate > 0 && plan.Transcode && plan.MaxBitrate > scope.MaxBitrate {
						t.Errorf("%s: rung %q delivers %dp at %d, over the ceiling", name, r.Label, h, plan.MaxBitrate)
					}
					got, ok := ladder(asked)
					same := ok && len(got) == len(L) && currentIndex(got) == i
					for j := 0; same && j < len(got); j++ {
						same = got[j].Label == L[j].Label && rungKey(got[j]) == rungKey(L[j])
					}
					if !same {
						t.Errorf("%s: requesting rung %d %q gives %v, want %v with it current", name, i, r.Label, labelsOf(got), labelsOf(L))
					}
				}
				// Walk down to the bottom, then up to the top.
				cur := L
				for steps := 0; steps < len(L)+1; steps++ {
					c := currentIndex(cur)
					if c == len(cur)-1 {
						break
					}
					cur, _ = ladder(Constraints{MaxResolution: cur[c+1].MaxResolution, MaxBitrate: cur[c+1].MaxBitrate})
				}
				for steps := 0; steps < len(L)+1; steps++ {
					c := currentIndex(cur)
					if c <= 0 {
						break
					}
					cur, _ = ladder(Constraints{MaxResolution: cur[c-1].MaxResolution, MaxBitrate: cur[c-1].MaxBitrate})
				}
				if currentIndex(cur) != 0 || rungKey(cur[0]) != rungKey(L[0]) {
					t.Errorf("%s: walking up ended at %v, want the top of %v", name, labelsOf(cur), labelsOf(L))
				}
			}
		}
	}
}
