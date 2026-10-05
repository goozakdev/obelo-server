package api

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/markers"
)

// TestMarkerAutoSkipKindsAreOrdered: the stored list is the same for the same
// toggles on every save (it used to come out of a map range, so in random order).
func TestMarkerAutoSkipKindsAreOrdered(t *testing.T) {
	t.Parallel()
	all := markerAutoSkipJSON{Intro: true, Recap: true, Credits: true, Preview: true}
	want := []string{markers.KindIntro, markers.KindRecap, markers.KindCredits, markers.KindPreview}
	for i := 0; i < 50; i++ {
		if got := all.kinds(); !reflect.DeepEqual(got, want) {
			t.Fatalf("kinds() = %v, want the fixed order %v", got, want)
		}
	}
	if got := (markerAutoSkipJSON{Credits: true, Intro: true}).kinds(); !reflect.DeepEqual(got, []string{markers.KindIntro, markers.KindCredits}) {
		t.Errorf("subset kinds() = %v, want [intro credits]", got)
	}
}
