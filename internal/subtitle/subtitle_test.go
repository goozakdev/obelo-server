package subtitle

import "testing"

func TestNormalizeLang(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// The load-bearing case: three spellings of English fold to one code.
		{"eng", "en"},
		{"en", "en"},
		{"English", "en"},
		{"ENG", "en"},
		{"  en  ", "en"},
		// BCP-47 region/script subtags are dropped.
		{"en-US", "en"},
		{"pt_BR", "pt"},
		{"zh-Hans", "zh"},
		// Bibliographic vs terminological three-letter variants.
		{"fre", "fr"},
		{"fra", "fr"},
		{"ger", "de"},
		{"deu", "de"},
		// Names.
		{"French", "fr"},
		{"Deutsch", "de"},
		{"español", "es"},
		// Undetermined / empty / unrecognized → Unknown ("").
		{"und", ""},
		{"", ""},
		{"   ", ""},
		{"xx", ""},
		{"zzz", ""},
		{"Klingon", ""},
	}
	for _, c := range cases {
		if got := NormalizeLang(c.in); got != c.want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDisplayLang(t *testing.T) {
	cases := []struct{ in, want string }{
		{"en", "English"},
		{"fr", "French"},
		{"", "Unknown"},
		{"xx", "Unknown"},
	}
	for _, c := range cases {
		if got := DisplayLang(c.in); got != c.want {
			t.Errorf("DisplayLang(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKindForCodec(t *testing.T) {
	text := []string{"subrip", "srt", "ass", "ssa", "mov_text", "webvtt", "vtt", ""}
	image := []string{"hdmv_pgs_subtitle", "pgssub", "dvd_subtitle", "vobsub", "sup", "DVDSUB"}
	for _, c := range text {
		if got := KindForCodec(c); got != "text" {
			t.Errorf("KindForCodec(%q) = %q, want text", c, got)
		}
	}
	for _, c := range image {
		if got := KindForCodec(c); got != "image" {
			t.Errorf("KindForCodec(%q) = %q, want image", c, got)
		}
	}
}

func TestParseSidecarName(t *testing.T) {
	cases := []struct {
		in     string
		lang   string
		forced bool
	}{
		{"Movie.en.srt", "en", false},
		{"Movie.en.forced.srt", "en", true},
		{"Movie.forced.en.srt", "en", true},
		{"Movie.en.sdh.srt", "en", false},
		// "hi" must parse as Hindi, not the hearing-impaired flag.
		{"Movie.hi.srt", "hi", false},
		{"Movie.hi.forced.srt", "hi", true},
		{"Movie.eng.srt", "en", false},
		{"Movie.English.srt", "en", false},
		// Unlabeled → Unknown, not forced.
		{"Movie.srt", "", false},
		// Forced with no language.
		{"Movie.forced.srt", "", true},
		// A dotted title stem must not be mistaken for a language.
		{"Some.Movie.srt", "", false},
		{"Some.Movie.fr.srt", "fr", false},
		// VOBSUB pair members parse the same way.
		{"Movie.en.idx", "en", false},
		{"Movie.en.sub", "en", false},
	}
	for _, c := range cases {
		gotLang, gotForced := ParseSidecarName(c.in)
		if gotLang != c.lang || gotForced != c.forced {
			t.Errorf("ParseSidecarName(%q) = (%q, %v), want (%q, %v)",
				c.in, gotLang, gotForced, c.lang, c.forced)
		}
	}
}

func TestParseSidecarStem(t *testing.T) {
	cases := []struct {
		in   string
		stem string
	}{
		{"Movie.en.srt", "Movie"},
		{"Movie.en.forced.srt", "Movie"},
		{"Movie.srt", "Movie"},
		{"Some.Movie.fr.srt", "Some.Movie"},
		{"Movie-trailer.en.srt", "Movie-trailer"},
		{"Movie-trailer.srt", "Movie-trailer"},
	}
	for _, c := range cases {
		if stem, _, _ := ParseSidecar(c.in); stem != c.stem {
			t.Errorf("ParseSidecar(%q) stem = %q, want %q", c.in, stem, c.stem)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		lang   string
		forced bool
		want   string
	}{
		{"en", false, "English"},
		{"en", true, "English (Forced)"},
		{"", false, "Unknown"},
		{"", true, "Unknown (Forced)"},
	}
	for _, c := range cases {
		if got := Label(c.lang, c.forced); got != c.want {
			t.Errorf("Label(%q, %v) = %q, want %q", c.lang, c.forced, got, c.want)
		}
	}
}
