package api

import (
	"encoding/json"
	"testing"

	"github.com/goozakdev/obelo-server/internal/playback"
)

// The wire shape of the Step-down ladder (ADR-0071): `[]` and never null when there
// are no rungs; `current` present only on the playing rung.
func TestToStepDownIsAnEmptyArrayNotNull(t *testing.T) {
	got, err := json.Marshal(toStepDown(nil))
	if err != nil || string(got) != "[]" {
		t.Errorf("toStepDown(nil) = %s (err %v), want []", got, err)
	}
}

func TestToStepDownMarksCurrentOnlyOnThePlayingRung(t *testing.T) {
	got, err := json.Marshal(toStepDown([]playback.StepRung{
		{Label: "4K", Kind: playback.StepKindEdition, EditionID: "e1", Current: true},
		{Label: "480p", Kind: playback.StepKindTranscode, MaxResolution: "480p", MaxBitrate: 1_500_000},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"label":"4K","kind":"edition","editionId":"e1","current":true},{"label":"480p","kind":"transcode","constraints":{"maxResolution":"480p","maxBitrate":1500000}}]`
	if string(got) != want {
		t.Errorf("toStepDown = %s\nwant %s", got, want)
	}
}

func TestToStepDownCarriesABitrateWhenKnown(t *testing.T) {
	got, err := json.Marshal(toStepDown([]playback.StepRung{
		{Label: "4K", Kind: playback.StepKindEdition, EditionID: "e1", Bitrate: 40_000_000, Current: true},
		{Label: "Original", Kind: playback.StepKindEdition},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"label":"4K","kind":"edition","editionId":"e1","bitrate":40000000,"current":true},{"label":"Original","kind":"edition"}]`
	if string(got) != want {
		t.Errorf("toStepDown = %s\nwant %s", got, want)
	}
}

func TestToStepDownCarriesAHeightWhenKnown(t *testing.T) {
	got, err := json.Marshal(toStepDown([]playback.StepRung{
		{Label: "1080p", Kind: playback.StepKindEdition, EditionID: "e1", Height: 1080, Current: true},
		{Label: "AAC", Kind: playback.StepKindTranscode},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"label":"1080p","kind":"edition","editionId":"e1","height":1080,"current":true},{"label":"AAC","kind":"transcode"}]`
	if string(got) != want {
		t.Errorf("toStepDown = %s\nwant %s", got, want)
	}
}
