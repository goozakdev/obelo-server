package playback

import (
	"fmt"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Ladder order and the way back up (ADR-0071): within a height the unconstrained rung is
// first, whatever its bitrate, and a delivery is never listed twice.

func dtsEdition(id string, height int, bitrate int64) store.Edition {
	e := edition(id, "", height, bitrate)
	e.Files[0].AudioCodec = "dts"
	e.Files[0].Streams[1].Codec = "dts"
	return e
}

func rungKey(r StepRung) string {
	return fmt.Sprintf("%s|%s|%s|%d", r.Kind, r.EditionID, r.MaxResolution, r.MaxBitrate)
}

func currentIndex(rungs []StepRung) int {
	idx := -1
	for i, r := range rungs {
		if r.Current {
			if idx >= 0 {
				return -2
			}
			idx = i
		}
	}
	return idx
}

// checkNoDuplicates fails when two rungs share a label or a request.
func checkNoDuplicates(t *testing.T, rungs []StepRung) {
	t.Helper()
	labels, keys := map[string]bool{}, map[string]bool{}
	for _, r := range rungs {
		if labels[r.Label] || keys[rungKey(r)] {
			t.Errorf("ladder lists %q twice: %v", r.Label, labelsOf(rungs))
		}
		labels[r.Label], keys[rungKey(r)] = true, true
	}
}

// checkWalk steps down the ladder one rung at a time, then back up, and wants the top
// rung again, with every ladder on the way a single-current list of distinct rungs.
func checkWalk(t *testing.T, svc *Service, base Request, first Decision) {
	t.Helper()
	lad := first.StepDown
	top := rungKey(lad[0])
	for i := 0; i < len(first.StepDown)+1; i++ {
		c := currentIndex(lad)
		if c < 0 || c == len(lad)-1 {
			break
		}
		lad = negotiateOK(t, svc, askRung(base, lad[c+1])).StepDown
		checkNoDuplicates(t, lad)
	}
	for i := 0; i < len(first.StepDown)+1; i++ {
		c := currentIndex(lad)
		if c < 0 {
			t.Fatalf("ladder %v has no single current rung", labelsOf(lad))
		}
		if c == 0 {
			break
		}
		lad = negotiateOK(t, svc, askRung(base, lad[c-1])).StepDown
		checkNoDuplicates(t, lad)
	}
	if got := currentIndex(lad); got != 0 || rungKey(lad[0]) != top {
		t.Errorf("walking up ended at %v, want the top rung %q", labelsOf(lad), first.StepDown[0].Label)
	}
}

func TestStepDownMusicOriginalOfUnknownBitrateSortsFirstUnderNoConstraints(t *testing.T) {
	svc := stepService(true, store.Edition{ID: "e1", Files: []store.File{flacTrack(0, "flac")}})
	base := stepReq()
	base.Profile = musicProfile()
	dec := negotiateOK(t, svc, base)
	wantKeys(t, dec.StepDown, "edition:e1*", "transcode:", "transcode:")
	if dec.StepDown[0].Label != "Original" {
		t.Errorf("top rung = %q, want Original", dec.StepDown[0].Label)
	}
	checkWalk(t, svc, base, dec)
}

func TestStepDownUnconstrainedRungIsAboveTheCurrentOneWhenACapChangesTheStream(t *testing.T) {
	svc := stepService(true, hevcEdition("uhd", 2160, 40_000_000)) // h264-only client
	base := stepReq()
	asked := base
	asked.Constraints = Constraints{MaxBitrate: 20_000_000}
	dec := negotiateOK(t, svc, asked)
	top := dec.StepDown[0]
	if top.Current || top.EditionID != "" || top.MaxResolution != "" || top.MaxBitrate != 0 {
		t.Errorf("top rung = %+v in %v, want the unconstrained rung, not current", top, labelsOf(dec.StepDown))
	}
	if c := currentIndex(dec.StepDown); c < 1 {
		t.Errorf("ladder %v: current index %d, want it below the unconstrained rung", labelsOf(dec.StepDown), c)
	}
	checkNoDuplicates(t, dec.StepDown)
	checkWalk(t, svc, base, dec)
}

func TestStepDownVideoCopyIsListedOnceWithItsDeliveredBitrate(t *testing.T) {
	svc := stepService(true, dtsEdition("hd", 1080, 8_000_000)) // DTS audio: the video is copied
	base := stepReq()
	asked := base
	asked.Constraints = Constraints{MaxBitrate: 20_000_000}
	dec := negotiateOK(t, svc, asked)
	checkNoDuplicates(t, dec.StepDown)
	n := 0
	for _, r := range dec.StepDown {
		if r.Bitrate == 8_000_000 {
			n++
			if !r.Current {
				t.Errorf("rung %q delivers the copy but is not current: %v", r.Label, labelsOf(dec.StepDown))
			}
		}
		if r.Bitrate == 20_000_000 {
			t.Errorf("rung %q carries the request's cap as its bitrate", r.Label)
		}
	}
	if n != 1 {
		t.Errorf("%d rungs at the copy's 8 Mbps in %v, want 1", n, labelsOf(dec.StepDown))
	}
	checkWalk(t, svc, base, negotiateOK(t, svc, base))
}

func TestStepDownCeilingClampedRequestMatchesTheUnconstrainedRung(t *testing.T) {
	svc := stepService(true, edition("uhd", "", 2160, 40_000_000))
	base := stepReq()
	base.Scope = access.Scope{AllLibraries: true, MaxBitrate: 5_000_000}
	asked := base
	asked.Constraints = Constraints{MaxBitrate: 8_000_000}
	dec := negotiateOK(t, svc, asked)
	checkNoDuplicates(t, dec.StepDown)
	if c := currentIndex(dec.StepDown); c != 0 || dec.StepDown[0].MaxBitrate != 0 {
		t.Errorf("ladder %v: want the unconstrained rung on top and current", labelsOf(dec.StepDown))
	}
	checkWalk(t, svc, base, negotiateOK(t, svc, base))
}

// A sharer whose only Edition is over the home ceiling: after a step to 720p the home
// ladder still has the unconstrained rung above, and `{}` marks it current.
func TestRelayStepDownKeepsAnUnconstrainedRungAboveTheHomeCeiling(t *testing.T) {
	sharer := stepService(true, edition("re1", "", 1080, 8_000_000))
	home := NewService(ladderRelayStore{ceilingStore{detail: titleWith(edition("e1", "", 1080, 8_000_000))}}, nil, "", Governance{})
	homeScope := access.Scope{AllLibraries: true, MaxBitrate: 5_000_000}
	ask := func(editionID string, c Constraints) []map[string]any {
		cl, _ := clampToCeiling(c, homeScope)
		req := stepReq()
		req.Constraints = cl
		req.EditionID = editionID
		if editionID == "e1" {
			req.EditionID = "re1"
		}
		var raw []any
		for _, r := range stepRungsAsJSON(negotiateOK(t, sharer, req).StepDown) {
			raw = append(raw, r)
		}
		var out []map[string]any
		for _, e := range home.relayStepDown(raw, home.store.(ladderRelayStore).detail, homeScope, editionID, c) {
			out = append(out, e.(map[string]any))
		}
		return out
	}
	isTop := func(m map[string]any) bool {
		_, hasC := m["constraints"]
		return m["editionId"] == "e1" && !hasC
	}
	l0 := ask("", Constraints{})
	if !isTop(l0[0]) || l0[0]["current"] != true {
		t.Errorf("{}: ladder %v, want the unconstrained rung first and current", l0)
	}
	l1 := ask("", Constraints{MaxResolution: "720p", MaxBitrate: 4_000_000})
	if !isTop(l1[0]) || l1[0]["current"] == true {
		t.Errorf("720p: ladder %v, want the unconstrained rung first and not current", l1)
	}
	cur := 0
	for _, m := range l1 {
		if m["current"] == true {
			cur++
		}
	}
	if cur != 1 {
		t.Errorf("720p: %d current rungs in %v, want 1", cur, l1)
	}
	// Requesting the unconstrained rung (home sends editionId e1, clamped) is current again.
	l2 := ask("e1", Constraints{})
	if !isTop(l2[0]) || l2[0]["current"] != true || len(l2) != len(l0) {
		t.Errorf("e1 rung: ladder %v, want %d rungs with the unconstrained one current", l2, len(l0))
	}
}

func stepRungsAsJSON(rs []StepRung) []map[string]any {
	var out []map[string]any
	for _, r := range rs {
		m := map[string]any{"label": r.Label, "kind": r.Kind}
		if r.EditionID != "" {
			m["editionId"] = r.EditionID
		}
		if r.MaxResolution != "" || r.MaxBitrate > 0 {
			m["constraints"] = map[string]any{"maxResolution": r.MaxResolution, "maxBitrate": float64(r.MaxBitrate)}
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
