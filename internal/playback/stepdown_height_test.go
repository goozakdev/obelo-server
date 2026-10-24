package playback

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// A music rung's codec tag names the delivered codec: two FLAC/ALAC originals that both come
// out as AAC are told apart by the count, not by the source codec.
func TestMusicCodecTagNamesTheDeliveredCodec(t *testing.T) {
	e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	alac := flacTrack(800_000, "alac")
	alac.ID = "tr2"
	e2 := store.Edition{ID: "e2", Files: []store.File{alac}}
	req := stepReq()
	req.Profile = DeviceProfile{Containers: []string{"mp4", "aac"}, AudioCodecs: []string{"aac"}, MaxAudioChannels: 2}
	req.EditionID = "e2"
	d := negotiateOK(t, stepService(true, e1, e2), req)
	labels := labelsOf(d.StepDown)
	has := func(want string) bool {
		for _, l := range labels {
			if strings.TrimSuffix(l, "*") == want {
				return true
			}
		}
		return false
	}
	if !has("AAC 256 kbps") || !has("AAC 256 kbps (2)") {
		t.Errorf("labels %v, want AAC 256 kbps and AAC 256 kbps (2)", labels)
	}
	for _, l := range labels {
		if strings.Contains(l, "flac") || strings.Contains(l, "alac") {
			t.Errorf("label %q names a source codec", l)
		}
	}
}

// An Online variant that states no height, encoded under a cap, has no height: the cap is
// not one, so no label names it and no rung carries a height.
func TestOnlineHeightlessVariantUnderACapClaimsNoHeight(t *testing.T) {
	got := onlineLadder(t, Constraints{MaxResolution: "1080p", MaxBitrate: 6_000_000}, true, mp4(""))
	if len(got) == 0 {
		t.Fatal("no rungs")
	}
	for _, r := range got {
		if r.Height != 0 || labelHeight(r.Label) != 0 || strings.Contains(r.Label, "1080p") {
			t.Errorf("rung %+v claims a height the variant never stated", r)
		}
	}
}

// A relay stand-in for a named top Edition keeps the Name in front of its height.
func TestRelayStandInKeepsTheEditionName(t *testing.T) {
	top := edition("re1", "Director's Cut", 2160, 40_000_000)
	sharer := stepService(true, top, edition("re2", "", 1080, 8_000_000))
	hst := ladderRelayStore{ceilingStore{detail: titleWith(edition("e1", "Director's Cut", 2160, 40_000_000), edition("e2", "", 1080, 8_000_000))}}
	home := NewService(hst, nil, "", Governance{})
	scope := access.Scope{AllLibraries: true, MaxBitrate: 5_000_000}
	// The sharer plays under the home ceiling, so its top rung is over it and dropped.
	req := stepReq()
	req.Constraints, _ = clampToCeiling(Constraints{}, scope)
	var raw []any
	for _, r := range stepRungsAsJSON(negotiateOK(t, sharer, req).StepDown) {
		raw = append(raw, r)
	}
	if l, _ := raw[0].(map[string]any)["label"].(string); !strings.HasPrefix(l, "Director's Cut") {
		t.Fatalf("sharer's top label %q does not carry the Name", l)
	}
	got := home.relayStepDown(raw, hst.detail, scope, "", Constraints{})
	l, _ := got[0].(map[string]any)["label"].(string)
	if !strings.HasPrefix(l, "Director's Cut · ") {
		t.Errorf("stand-in label %q, want the Name kept: %v", l, got)
	}
}

// A sharer's height passes through the relay untouched; a stand-in the ceiling clamps takes
// the ceiling's, and one the sharer gave none is unknown.
func TestRelayPassesTheSharersHeightThrough(t *testing.T) {
	sharer := stepService(true, edition("re1", "", 2160, 40_000_000), edition("re2", "", 1080, 8_000_000), edition("re3", "", 720, 4_000_000))
	hst := ladderRelayStore{ceilingStore{detail: titleWith(edition("e1", "", 2160, 40_000_000), edition("e2", "", 1080, 8_000_000), edition("e3", "", 720, 4_000_000))}}
	home := NewService(hst, nil, "", Governance{})
	d := negotiateOK(t, sharer, stepReq())
	raw := withHeights(d.StepDown)
	got := home.relayStepDown(raw, hst.detail, access.Scope{AllLibraries: true}, "", Constraints{})
	if len(got) != len(d.StepDown) {
		t.Fatalf("relay ladder %v", got)
	}
	for i, g := range got {
		if h := jsonInt(g.(map[string]any)["height"]); h != int64(d.StepDown[i].Height) || h == 0 {
			t.Errorf("rung %d height %d, want the sharer's %d", i, h, d.StepDown[i].Height)
		}
	}
	// Under a ceiling that drops the top, the stand-in's height is not above the ceiling's.
	scope := access.Scope{AllLibraries: true, MaxResolution: "720p", MaxBitrate: 3_000_000}
	req := stepReq()
	req.Constraints, _ = clampToCeiling(Constraints{}, scope)
	under := withHeights(negotiateOK(t, sharer, req).StepDown)
	stand := home.relayStepDown(under, hst.detail, scope, "", Constraints{})
	if h := jsonInt(stand[0].(map[string]any)["height"]); h > 720 {
		t.Errorf("stand-in height %d over the 720p ceiling: %v", h, stand)
	}
}

// withHeights is the sharer's ladder as decoded JSON, each rung carrying its height.
func withHeights(rungs []StepRung) []any {
	raw := ladderToAny(rungs)
	for i, m := range raw {
		if rungs[i].Height > 0 {
			m.(map[string]any)["height"] = float64(rungs[i].Height)
		}
	}
	return raw
}

// Every video rung kind carries the height it delivers; music has none.
func TestStepRungHeightIsTheDeliveredHeightForEveryKind(t *testing.T) {
	// Encode and Edition (direct play) rungs.
	d := negotiateOK(t, stepService(true, ladderEditions()...), stepReq())
	for _, r := range d.StepDown {
		if r.Height == 0 || labelHeight(r.Label) != 0 && labelHeight(r.Label) != r.Height {
			t.Errorf("rung %+v: height must be set and match its label", r)
		}
	}
	if d.StepDown[len(d.StepDown)-1].Height != 480 {
		t.Errorf("480p encode rung height %d: %v", d.StepDown[len(d.StepDown)-1].Height, d.StepDown)
	}
	// Remux (mkv in an mp4-only client) and video copy.
	mkv := mkvEdition("m", 1080, 8_000_000)
	q := stepReq()
	q.Profile.Containers = []string{"mp4"}
	for _, r := range negotiateOK(t, stepService(true, mkv), q).StepDown {
		if r.Height == 0 {
			t.Errorf("remux ladder rung %+v has no height", r)
		}
	}
	// Online.
	for _, r := range onlineLadder(t, Constraints{}, true, mp4("1080p")) {
		if r.Height == 0 {
			t.Errorf("online rung %+v has no height", r)
		}
	}
	// Music.
	m := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	mq := stepReq()
	mq.Profile = musicProfile()
	for _, r := range negotiateOK(t, stepService(true, m), mq).StepDown {
		if r.Height != 0 {
			t.Errorf("music rung %+v has a height", r)
		}
	}
}
