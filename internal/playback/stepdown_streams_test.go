package playback

import (
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Two remuxes of different video tracks in one File are different rungs, each labelled
// by its own track's height.
func TestRemuxesOfDifferentVideoStreamsAreDifferentRungs(t *testing.T) {
	cases := map[string]struct {
		container string
		remuxOnly bool
		br        int64
		cap       Constraints
	}{
		"mkv remux":         {"matroska,webm", false, 3_000_000, Constraints{MaxResolution: "720p", MaxBitrate: 4_000_000}},
		"mp4 remuxSelected": {"", true, 3_000_000, Constraints{MaxResolution: "720p", MaxBitrate: 5_000_000}},
	}
	for name, c := range cases {
		e := r26MV("uhd", c.container, 2160, "h264", 720, c.br)
		svc := memPicksSvc(nil, nil, new(int), e)
		q := stepReq()
		q.Profile.Containers = []string{"mp4"}
		q.Constraints = c.cap
		q.RemuxSelectedOnly = c.remuxOnly
		d := negotiateOK(t, svc, q)
		if d.VideoStream.Height != 720 {
			t.Fatalf("%s: delivers %dp, want the 720p track", name, d.VideoStream.Height)
		}
		ci := currentIndex(d.StepDown)
		if ci < 0 || !strings.HasPrefix(d.StepDown[ci].Label, "720p") {
			t.Errorf("%s: current rung in %v, want one labelled 720p", name, labelsOf(d.StepDown))
		}
		tops := 0
		for _, r := range d.StepDown {
			if strings.HasPrefix(r.Label, "2160p") {
				tops++
			}
		}
		if tops != 1 {
			t.Errorf("%s: want the default 2160p track kept as its own rung: %v", name, labelsOf(d.StepDown))
		}
		if ci >= 0 {
			again := negotiateOK(t, svc, askRung(q, d.StepDown[ci]))
			if again.VideoStream.ID != d.VideoStream.ID {
				t.Errorf("%s: asking the current rung delivers %s, want %s", name, again.VideoStream.ID, d.VideoStream.ID)
			}
		}
		checkDerived(t, name, svc, q)
	}
}

func r26MV(id, container string, h0 int, c1 string, h1 int, br int64) (e store.Edition) {
	e = memMultiVideo(id, "h264", h0, c1, h1, br)
	e.Files[0].ID = "f-" + id
	if container != "" {
		e.Files[0].Container = container
	}
	return e
}
