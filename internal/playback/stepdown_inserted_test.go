package playback

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// An inserted current rung carries the request as the client sent it, so asking for it
// again reproduces the delivery and it never repeats another rung's request.
func TestInsertedCurrentRungCarriesTheActualRequest(t *testing.T) {
	for _, mode := range []string{"remembered", "explicit"} {
		var reads int
		uhd := memMultiVideo("uhd", "h264", 2160, "h264", 1080, 40_000_000)
		hd := edition("hd", "", 1080, 8_000_000)
		var v *store.RememberedVideo
		if mode == "remembered" {
			v = &store.RememberedVideo{Label: "ALT", Codec: "h264", Height: 720}
		}
		svc := memPicksSvc(nil, v, &reads, uhd, hd)
		q := stepReq()
		q.Constraints.MaxResolution = "720p"
		if mode == "explicit" {
			q.VideoStreamID = "uhd-v1"
		}
		d := negotiateOK(t, svc, q)
		keys := map[[3]any]string{}
		for _, r := range d.StepDown {
			k := [3]any{r.EditionID, r.MaxResolution, r.MaxBitrate}
			if prev, dup := keys[k]; dup {
				t.Errorf("%s: rungs %q and %q repeat the request %v: %v", mode, prev, r.Label, k, labelsOf(d.StepDown))
			}
			keys[k] = r.Label
		}
		cur := d.StepDown[currentIndex(d.StepDown)]
		if cur.EditionID != "" || cur.MaxResolution != "720p" {
			t.Errorf("%s: current rung %+v, want the request as sent: no editionId, 720p", mode, cur)
		}
		again := negotiateOK(t, svc, askRung(q, cur))
		if again.File.ID != d.File.ID || again.VideoStream.ID != d.VideoStream.ID || currentIndex(again.StepDown) < 0 ||
			again.StepDown[currentIndex(again.StepDown)].Label != cur.Label {
			t.Errorf("%s: asking for the current rung gives %s/%s %v, want %s/%s with %q current", mode, again.File.ID, again.VideoStream.ID, labelsOf(again.StepDown), d.File.ID, d.VideoStream.ID, cur.Label)
		}
		checkDerived(t, "inserted-"+mode, svc, stepReq())
	}
}

// An Edition rung is labelled by the video stream it delivers, so a picked lower cut is
// not called by the Edition's top height.
func TestEditionRungLabelUsesTheDeliveredVideoHeight(t *testing.T) {
	var reads int
	v := &store.RememberedVideo{Label: "ALT", Codec: "h264", Height: 1080}
	svc := memPicksSvc(nil, v, &reads, memMultiVideo("uhd", "h264", 2160, "h264", 1080, 40_000_000))
	d := negotiateOK(t, svc, stepReq())
	if d.VideoStream.Height != 1080 {
		t.Fatalf("delivers %dp, want the picked 1080p cut", d.VideoStream.Height)
	}
	for _, r := range d.StepDown {
		if r.Current && !strings.HasPrefix(r.Label, "1080p") {
			t.Errorf("current rung %q, want it labelled by the delivered 1080p", r.Label)
		}
	}
	checkDerived(t, "label-lower-cut", svc, stepReq())
}

// labelOpensWithHeight is the picture height a label opens with ("1080p", "720p 3 Mbps"), if it does.
func labelOpensWithHeight(label string) (int, bool) {
	n := 0
	for i, c := range label {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			continue
		}
		return n, i > 0 && c == 'p'
	}
	return 0, false
}
