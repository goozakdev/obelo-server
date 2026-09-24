package markers

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"Intro":           KindIntro,
		"Opening Credits": KindIntro,
		"OP":              KindIntro,
		"Previously on…":  KindRecap,
		"Recap":           KindRecap,
		"End Credits":     KindCredits,
		"Credits":         KindCredits,
		"ED":              KindCredits,
		"Preview":         KindPreview,
		"Next Episode":    KindPreview,
		"Chapter 3":       "",
		"Ed's Big Day":    "",
		"Commercial":      "",
		"":                "",
	}
	for label, want := range cases {
		if got := Classify(label); got != want {
			t.Errorf("Classify(%q) = %q, want %q", label, got, want)
		}
	}
}

// TestFromChaptersKeepsOnlyNamedSpans: chapters are read, but only the ones whose
// title names a kind become Markers — a numbered chapter is not a Marker.
func TestFromChaptersKeepsOnlyNamedSpans(t *testing.T) {
	got := FromChapters([]Chapter{
		{StartMs: 0, EndMs: 60_000, Title: "Chapter 1"},
		{StartMs: 60_000, EndMs: 150_000, Title: "Intro"},
		{StartMs: 150_000, EndMs: 1_200_000, Title: "Chapter 2"},
		{StartMs: 1_200_000, EndMs: 1_400_000, Title: "End Credits"},
	}, 1_300_000)
	want := []Span{
		{Kind: KindIntro, StartMs: 60_000, EndMs: 150_000},
		{Kind: KindCredits, StartMs: 1_200_000, EndMs: 1_300_000}, // trimmed to the File
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromChapters = %+v, want %+v", got, want)
	}
}

func TestParseEDL(t *testing.T) {
	edl := []byte("" +
		"0.0\t84.5\t0\tIntro\n" +
		"300 360 3\n" + // a commercial break with no label: nothing we can name
		"1290.25 1380 3 # Credits\n" +
		"not a line\n" +
		"10 5 0 Recap\n" + // inverted
		"2000 2100 0 Preview\n" + // past the end
		"90 120 recap\n") // no action number
	got := ParseEDL(edl, 1_350_000)
	want := []Span{
		{Kind: KindIntro, StartMs: 0, EndMs: 84_500},
		{Kind: KindRecap, StartMs: 90_000, EndMs: 120_000},
		{Kind: KindCredits, StartMs: 1_290_250, EndMs: 1_350_000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseEDL = %+v, want %+v", got, want)
	}
}
