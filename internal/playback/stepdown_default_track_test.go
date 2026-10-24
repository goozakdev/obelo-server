package playback

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// twoCutEdition is an Edition whose File has a default video track of height h0 and a
// second, non-default one of height h1.
func twoCutEdition(id string, h0, h1 int, br int64) store.Edition {
	e := edition(id, "", h0, br)
	e.Files[0].Streams[0] = store.Stream{ID: id + "-v0", Kind: "video", Codec: "h264", Height: h0, IsDefault: true}
	e.Files[0].Streams = append(e.Files[0].Streams, store.Stream{ID: id + "-v1", Kind: "video", Codec: "h264", Height: h1})
	return e
}

// Under a resolution cap SelectEdition records a lower, non-default video track, which
// plays as a remux of that track. The rung describes what plays: label, height and codec
// are the remuxed track's.
func TestStepDownRemuxRungDescribesTheRemuxedTrack(t *testing.T) {
	svc := stepService(true, twoCutEdition("uhd", 2160, 1080, 40_000_000), twoCutEdition("hd", 1080, 720, 8_000_000))
	req := stepReq()
	req.Scope.MaxResolution = "720p"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierDirectStream || dec.File.ID != "f-hd" || dec.VideoStream.Height != 720 {
		t.Fatalf("input did not arise: %s %s recorded height %d", dec.Tier, dec.File.ID, dec.VideoStream.Height)
	}
	cur := dec.StepDown[currentIndex(dec.StepDown)]
	if cur.Height != 720 || !strings.HasPrefix(cur.Label, "720p") {
		t.Errorf("current remux rung = %q height %d, want the remuxed track's 720p", cur.Label, cur.Height)
	}
}
