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

// TestClassifyEndingOnlyAsTheEndingItself: "Ending" names the Credits only as the
// chapter that IS the ending (the anime "Ending", "Ending Theme"). A story chapter
// that merely has an ending in its name — "Alternate Ending" past halfway — would
// otherwise become the Watched ceiling and clear resume before the story is over.
// "End Titles" is the film name for the Credits.
func TestClassifyEndingOnlyAsTheEndingItself(t *testing.T) {
	cases := map[string]string{
		"Ending":           KindCredits,
		"Ending Theme":     KindCredits,
		"Alternate Ending": "",
		"The Happy Ending": "",
		"End Titles":       KindCredits,
		"End Title":        KindCredits,
		"Main Titles":      "",
		"The End":          "",
	}
	for label, want := range cases {
		if got := Classify(label); got != want {
			t.Errorf("Classify(%q) = %q, want %q", label, got, want)
		}
	}
}

// TestClassifyReadsTheNameAfterAChapterNumber: a leading chapter number —
// "05 - ", "Chapter 5 - ", "Part C: " — is not part of the chapter's name, so
// "05 - Ending" is the Credits as "Ending" is. "End Title(s)" is the Credits only
// as the whole name after it: "Dead End Title" is a story chapter.
func TestClassifyReadsTheNameAfterAChapterNumber(t *testing.T) {
	cases := map[string]string{
		"05 - Ending":           KindCredits,
		"Chapter 5 - Ending":    KindCredits,
		"Part C: Ending":        KindCredits,
		"01 Opening":            KindIntro,
		"Dead End Title":        "",
		"Alternate Ending":      "",
		"End Titles":            KindCredits,
		"12 - End Titles":       KindCredits,
		"Chapter 12":            "",
		"05 - Alternate Ending": "",
	}
	for label, want := range cases {
		if got := Classify(label); got != want {
			t.Errorf("Classify(%q) = %q, want %q", label, got, want)
		}
	}
}
