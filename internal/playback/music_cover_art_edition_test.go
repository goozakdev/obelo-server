package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// withArt gives a Track an embedded cover-art "video" Stream of the given height.
func withArt(f store.File, height int) store.File {
	f.Height = height
	f.Streams = append(append([]store.Stream{}, f.Streams...), store.Stream{Kind: "video", Codec: "mjpeg", Height: height})
	return f
}

func artEditions() (store.Edition, store.Edition) {
	flac := withArt(flacTrack(900_000, "flac"), 600)
	mp3 := withArt(mp3Track("tr2", 192_000, 0), 1400)
	return store.Edition{ID: "e1", Files: []store.File{flac}}, store.Edition{ID: "e2", Files: []store.File{mp3}}
}

// Cover art never ranks a Track: the lossless FLAC beats the MP3 whose art is larger.
func TestCoverArtDoesNotRankMusicEditions(t *testing.T) {
	e1, e2 := artEditions()
	svc := stepService(true, e1, e2)
	req := stepReq()
	req.Profile = portableProfile()
	dec := negotiateOK(t, svc, req)
	if dec.Edition.ID != "e1" {
		t.Fatalf("bare play chose %s, want the FLAC e1", dec.Edition.ID)
	}
	var got []string
	for _, r := range dec.StepDown {
		got = append(got, r.Label)
	}
	if len(dec.StepDown) != 3 || dec.StepDown[0].EditionID != "e1" || dec.StepDown[1].EditionID != "e2" || dec.StepDown[2].MaxBitrate != 128_000 {
		t.Errorf("ladder %v, want [Original FLAC, MP3, AAC 128]", got)
	}
	req.Constraints.MaxBitrate = 900_000
	dec = negotiateOK(t, svc, req)
	if cur := currentOf(t, dec.StepDown); cur.EditionID != "e1" {
		t.Errorf("cap-only 900000: current %+v, want the FLAC", cur)
	}
}

func TestBetterRanksAudioOnlyByQuality(t *testing.T) {
	d := func(codec string, br int64, art int) Decision {
		return Decision{AudioOnly: true, File: withArt(flacTrack(br, codec), art)}
	}
	for name, c := range map[string]struct{ a, b Decision }{
		"lossless over lossy": {d("flac", 900_000, 600), d("mp3", 192_000, 1400)},
		"lossless, bitrate":   {d("flac", 900_000, 0), d("alac", 700_000, 0)},
		"lossy, bitrate":      {d("mp3", 320_000, 100), d("aac", 192_000, 900)},
		"known over unknown":  {d("mp3", 128_000, 0), d("mp3", 0, 900)},
	} {
		if !better(c.a, c.b) || better(c.b, c.a) {
			t.Errorf("%s: want a better than b only", name)
		}
	}
}

// A relayed music Edition rung has no height: the mirrored File's cover art is not one.
func TestRelayMusicRungKeepsNoHeightUnderACeiling(t *testing.T) {
	e1, e2 := artEditions()
	home, hst := relayHome(e1, e2)
	scope := access.Scope{AllLibraries: true, MaxResolution: "720p"}
	music := func(id, label string, bitrate int64, current bool) map[string]any {
		m := map[string]any{"kind": StepKindEdition, "editionId": id, "label": label, "bitrate": float64(bitrate)}
		if current {
			m["current"] = true
		}
		return m
	}
	raw := []any{music("re1", "FLAC", 900_000, true), music("re2", "MP3", 192_000, false)}
	got := home.relayStepDown(raw, hst.detail, scope, "", Constraints{})
	if len(got) != 2 {
		t.Fatalf("ladder %v, want both music rungs", got)
	}
	for _, g := range got {
		m := g.(map[string]any)
		if _, has := m["height"]; has || labelHeight(labelOfRung(g)) != 0 {
			t.Errorf("rung %v carries a height", g)
		}
	}
	if m := got[0].(map[string]any); m["editionId"] != "e1" || m["current"] != true {
		t.Errorf("first rung %v, want the e1 Original, current", m)
	}
}

// Two relayed rungs never share a label.
func TestRelayLabelsAreUnique(t *testing.T) {
	taller := edition("e1", "", 1080, 8_000_000)
	home, hst := relayHome(taller)
	scope := access.Scope{AllLibraries: true, MaxResolution: "1080p", MaxBitrate: 5_000_000}
	tr := map[string]any{"kind": StepKindTranscode, "label": "720p 5 Mbps", "constraints": map[string]any{"maxResolution": "720p", "maxBitrate": float64(5_000_000)}, "bitrate": float64(5_000_000), "height": float64(720)}
	ed := relayRung("re1", "720p 5 Mbps", 720, 5_000_000, true)
	for _, req := range []struct {
		id string
		c  Constraints
	}{{"", Constraints{MaxResolution: "720p"}}, {"e1", Constraints{}}} {
		got := home.relayStepDown([]any{ed, tr}, hst.detail, scope, req.id, req.c)
		seen := map[string]bool{}
		for _, g := range got {
			if l := labelOfRung(g); seen[l] {
				t.Errorf("request %+v: duplicate label %q in %v", req, l, got)
			} else {
				seen[l] = true
			}
		}
	}
}
