package playback

import (
	"testing"
)

// A video rung's label always carries the delivered height: an Edition named for a
// resolution is labelled by the height alone, any other Name is a prefix to it.
func TestEditionRungLabelCarriesTheDeliveredHeight(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"2160p", []string{"2160p", "720p*", "480p 1.5 Mbps"}},
		{"4K", []string{"2160p", "720p*", "480p 1.5 Mbps"}},
		{"uhd", []string{"2160p", "720p*", "480p 1.5 Mbps"}},
		{"Director's Cut", []string{"Director's Cut · 2160p", "Director's Cut · 720p*", "480p 1.5 Mbps"}},
		{"", []string{"2160p", "720p*", "480p 1.5 Mbps"}},
	} {
		var reads int
		ed := memMultiVideo("u", "h264", 2160, "h264", 720, 3_000_000)
		ed.Name = tc.name
		ed.Files[0].Container = "matroska,webm"
		svc := memPicksSvc(nil, nil, &reads, ed)
		q := stepReq()
		q.Profile.Containers = []string{"mp4"}
		q.Constraints.MaxResolution, q.Constraints.MaxBitrate = "720p", 4_000_000
		d := negotiateOK(t, svc, q)
		got := labelsOf(d.StepDown)
		if len(got) != len(tc.want) {
			t.Errorf("Name %q: ladder %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Name %q: ladder %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}
