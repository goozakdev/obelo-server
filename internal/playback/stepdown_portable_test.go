package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

func portableProfile() DeviceProfile {
	return DeviceProfile{Containers: []string{"flac", "mp3", "mp4", "aac"}, AudioCodecs: []string{"flac", "aac", "mp3"}, MaxAudioChannels: 2}
}

func mp3Track(id string, bitrate int64, coverHeight int) store.File {
	f := flacTrack(bitrate, "mp3")
	f.ID, f.Height = id, coverHeight
	return f
}

// A track's cover art is not a picture: an audio-only rung has no height, and the sort
// never reads one.
func TestMusicCoverArtGivesNoRungAHeight(t *testing.T) {
	flac := flacTrack(900_000, "flac")
	flac.Height = 600
	e1 := store.Edition{ID: "e1", Files: []store.File{flac}}
	e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", 192_000, 300)}}
	req := stepReq()
	req.Profile = portableProfile()
	dec := negotiateOK(t, stepService(true, e1, e2), req)
	if len(dec.StepDown) == 0 || dec.StepDown[0].EditionID != "e1" {
		t.Fatalf("ladder %v, want the Original first", labelsOf(dec.StepDown))
	}
	for _, r := range dec.StepDown {
		if r.Height != 0 {
			t.Errorf("rung %+v carries a height from cover art", r)
		}
	}
}

// A music Edition rung keeps its editionId and also carries the cap of the fixed AAC
// request it merged with, so it means the same on any Track; either request marks it current.
func TestMusicEditionRungCarriesTheMergedCap(t *testing.T) {
	e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", 192_000, 0)}}
	svc := stepService(true, e1, e2)
	req := stepReq()
	req.Profile = portableProfile()
	var mp3 *StepRung
	dec := negotiateOK(t, svc, req)
	for i := range dec.StepDown {
		if dec.StepDown[i].EditionID == "e2" {
			mp3 = &dec.StepDown[i]
		}
	}
	if mp3 == nil || mp3.MaxBitrate != 256_000 {
		t.Fatalf("ladder %+v, want the e2 rung carrying maxBitrate 256000", dec.StepDown)
	}
	if dec.StepDown[0].MaxBitrate != 0 {
		t.Errorf("the unconstrained Original carries %+v", dec.StepDown[0])
	}
	for name, ask := range map[string]func(*Request){
		"bare editionId": func(q *Request) { q.EditionID = "e2" },
		"cap alone":      func(q *Request) { q.Constraints.MaxBitrate = 256_000 },
		"rung exactly":   func(q *Request) { q.EditionID, q.Constraints.MaxBitrate = "e2", 256_000 },
	} {
		q := req
		ask(&q)
		cur := currentOf(t, negotiateOK(t, svc, q).StepDown)
		if cur.EditionID != "e2" || cur.MaxBitrate != 256_000 {
			t.Errorf("%s: current rung %+v, want the e2 rung", name, cur)
		}
	}
}

// An Edition rung no fixed AAC request merged with carries its own delivered bitrate.
func TestMusicEditionRungWithoutAMergeCarriesItsBitrate(t *testing.T) {
	e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", 320_000, 0)}}
	svc := stepService(true, e1, e2)
	req := stepReq()
	req.Profile = portableProfile()
	for _, r := range negotiateOK(t, svc, req).StepDown {
		if r.EditionID == "e2" {
			if r.MaxBitrate != 320_000 {
				t.Errorf("rung %+v, want maxBitrate 320000", r)
			}
			return
		}
	}
	t.Error("no e2 rung")
}

func currentOf(t *testing.T, rungs []StepRung) StepRung {
	t.Helper()
	var cur []StepRung
	for _, r := range rungs {
		if r.Current {
			cur = append(cur, r)
		}
	}
	if len(cur) != 1 {
		t.Fatalf("%d current rungs in %+v", len(cur), rungs)
	}
	return cur[0]
}

func relayRung(id, label string, height int, bitrate int64, current bool) map[string]any {
	m := map[string]any{"kind": StepKindEdition, "editionId": id, "label": label, "height": float64(height), "bitrate": float64(bitrate)}
	if current {
		m["current"] = true
	}
	return m
}

func relayHome(eds ...store.Edition) (*Service, ladderRelayStore) {
	hst := ladderRelayStore{ceilingStore{detail: titleWith(eds...)}}
	return NewService(hst, nil, "", Governance{}), hst
}

func labelOfRung(m any) string { l, _ := m.(map[string]any)["label"].(string); return l }

// A sharer's rung can deliver a track other than the File's primary: its own height, not
// the mirror's primary one, decides the ceiling check.
func TestRelayJudgesAnEditionRungByTheSharersHeight(t *testing.T) {
	home, hst := relayHome(edition("e1", "", 2160, 30_000_000), edition("e2", "", 1080, 6_000_000))
	scope := access.Scope{AllLibraries: true, MaxResolution: "1080p"}
	// (a) the mirror's primary is 2160 but the sharer's rung plays its 720 track.
	raw := []any{relayRung("re1", "720p", 720, 30_000_000, false), relayRung("re2", "1080p", 1080, 6_000_000, true)}
	got := home.relayStepDown(raw, hst.detail, scope, "", Constraints{})
	if len(got) != 2 || labelOfRung(got[0]) != "720p" || jsonInt(got[0].(map[string]any)["height"]) != 720 {
		t.Errorf("(a) ladder %v, want the 720p rung kept as the sharer named it", got)
	}
	// (c) the mirror's primary is 1080 but the sharer's rung plays 2160: over the ceiling.
	raw = []any{relayRung("re2", "2160p", 2160, 6_000_000, false), relayRung("re1", "1080p", 1080, 6_000_000, true)}
	for _, g := range home.relayStepDown(raw, hst.detail, scope, "", Constraints{}) {
		if jsonInt(g.(map[string]any)["height"]) > 1080 {
			t.Errorf("(c) rung %v is above the 1080p ceiling", g)
		}
	}
}

// The stand-in for a dropped top rung takes the sharer's height, not the mirror's.
func TestRelayStandInTakesTheSharersHeight(t *testing.T) {
	home, hst := relayHome(edition("e1", "", 720, 6_000_000))
	scope := access.Scope{AllLibraries: true, MaxResolution: "1080p", MaxBitrate: 5_000_000}
	low := map[string]any{"kind": StepKindTranscode, "label": "480p", "current": true, "constraints": map[string]any{"maxResolution": "480p", "maxBitrate": float64(1_500_000)}}
	raw := []any{relayRung("re1", "1080p 6 Mbps", 1080, 6_000_000, false), low}
	got := home.relayStepDown(raw, hst.detail, scope, "", Constraints{})
	if len(got) == 0 {
		t.Fatal("no rungs")
	}
	if l := labelOfRung(got[0]); l != "1080p 5 Mbps" {
		t.Errorf("stand-in label %q, want 1080p 5 Mbps: %v", l, got)
	}
}

// Whatever tier plays it, an audio-only delivery has no height.
func TestAudioOnlyDeliveryHasNoHeight(t *testing.T) {
	f := flacTrack(900_000, "flac")
	f.Height = 600
	for _, tier := range []Tier{TierDirectPlay, TierDirectStream, TierTranscode} {
		dec := Decision{Tier: tier, AudioOnly: true, File: f}
		if _, h, _ := deliveryOf(portableProfile(), Constraints{}, dec); h != 0 {
			t.Errorf("%s: height %d from cover art", tier, h)
		}
	}
}

// A non-Original music Edition rung whose bitrate is unknown carries no maxBitrate.
func TestMusicEditionRungWithUnknownBitrateCarriesNoCap(t *testing.T) {
	e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(900_000, "flac")}}
	e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", 0, 0)}}
	req := stepReq()
	req.Profile = portableProfile()
	for _, r := range negotiateOK(t, stepService(true, e1, e2), req).StepDown {
		if r.EditionID == "e2" && r.MaxBitrate != 0 {
			t.Errorf("rung %+v carries a cap for an unknown bitrate", r)
		}
	}
}

// A music Edition rung whose known bitrate is at or above the Original's is not listed: a
// step down never raises the bitrate, and a cap-only request names one rung.
func TestMusicFloorDropsALossyEditionAtOrAboveTheOriginal(t *testing.T) {
	e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(250_000, "flac")}}
	e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", 320_000, 0)}}
	svc := stepService(true, e1, e2)
	req := stepReq()
	req.Profile = portableProfile()
	dec := negotiateOK(t, svc, req)
	if got := labelsOf(dec.StepDown); len(got) != 2 || dec.StepDown[0].EditionID != "e1" || !dec.StepDown[0].Current || dec.StepDown[1].MaxBitrate != 128_000 {
		t.Fatalf("ladder %v (%+v), want [Original*, AAC 128]", got, dec.StepDown)
	}
	req.Constraints.MaxBitrate = 320_000
	if cur := currentOf(t, negotiateOK(t, svc, req).StepDown); cur.EditionID != "e1" {
		t.Errorf("cap-only request marks %+v current, want the Original", cur)
	}
	// The rung a request names stays, inserted as current.
	req.Constraints.MaxBitrate, req.EditionID = 0, "e2"
	dec = negotiateOK(t, svc, req)
	if cur := currentOf(t, dec.StepDown); cur.EditionID != "e2" {
		t.Errorf("request naming e2: ladder %+v, want e2 current", dec.StepDown)
	}
}

// With the Original's bitrate unknown, or the Edition's, nothing is dropped.
func TestMusicFloorNeedsBothBitratesKnown(t *testing.T) {
	for name, br := range map[string][2]int64{"original unknown": {0, 320_000}, "edition unknown": {250_000, 0}} {
		e1 := store.Edition{ID: "e1", Files: []store.File{flacTrack(br[0], "flac")}}
		e2 := store.Edition{ID: "e2", Files: []store.File{mp3Track("tr2", br[1], 0)}}
		req := stepReq()
		req.Profile = portableProfile()
		found := false
		for _, r := range negotiateOK(t, stepService(true, e1, e2), req).StepDown {
			found = found || r.EditionID == "e2"
		}
		if !found {
			t.Errorf("%s: e2 rung dropped", name)
		}
	}
}
