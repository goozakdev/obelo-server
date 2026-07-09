package scanner

import "testing"

// buildSidecarSubtitles folds a VOBSUB .idx/.sub pair into one image track,
// classifies text vs image, and parses each filename for language/forced. This
// exercises that logic without any media binary (the API integration test
// covers the same end to end with real ffprobe).
func TestBuildSidecarSubtitles(t *testing.T) {
	names := []string{
		"Movie.en.srt",
		"Movie.es.forced.srt",
		"Movie.srt", // unlabeled → Unknown
		"Movie.de.sup",
		"Movie.it.idx",
		"Movie.it.sub",         // absorbed into the .idx track (VOBSUB pair = one track)
		"Movie.fr.sub",         // standalone .sub = MicroDVD text, not a bitmap
		"Movie-trailer.en.srt", // belongs to the trailer Extra → skipped
		"sample.srt",           // junk stem → skipped
	}
	got := buildSidecarSubtitles("/movies/Movie", names)

	// 9 files → 6 tracks: the idx/sub pair collapses to one; the trailer and
	// sample sidecars are dropped.
	if len(got) != 6 {
		t.Fatalf("track count = %d, want 6; got %+v", len(got), got)
	}

	var image, text, unknown, forced, vobsub, microdvd int
	for _, s := range got {
		if s.Codec == "microdvd" {
			microdvd++
		}
		if s.Source != "sidecar" {
			t.Errorf("source = %q, want sidecar", s.Source)
		}
		switch s.Kind {
		case "image":
			image++
		case "text":
			text++
		default:
			t.Errorf("unexpected kind %q", s.Kind)
		}
		if s.Language == "" {
			unknown++
		}
		if s.Forced {
			forced++
		}
		if s.Codec == "vobsub" {
			vobsub++
		}
	}
	if text != 4 { // en, es(forced), unlabeled, fr(microdvd)
		t.Errorf("text tracks = %d, want 4", text)
	}
	if image != 2 { // de(.sup) + it(vobsub)
		t.Errorf("image tracks = %d, want 2", image)
	}
	if unknown != 1 {
		t.Errorf("unknown-language tracks = %d, want 1", unknown)
	}
	if forced != 1 { // es
		t.Errorf("forced tracks = %d, want 1", forced)
	}
	if vobsub != 1 { // the single collapsed idx/sub track
		t.Errorf("vobsub tracks = %d, want 1", vobsub)
	}
	if microdvd != 1 { // the standalone .sub → text
		t.Errorf("microdvd (standalone .sub, text) tracks = %d, want 1", microdvd)
	}
}
