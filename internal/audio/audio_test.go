package audio

import "testing"

func TestChannelLayout(t *testing.T) {
	cases := map[int]string{
		0: "",
		1: "Mono",
		2: "Stereo",
		3: "2.1",
		4: "4.0",
		5: "5.0",
		6: "5.1",
		7: "6.1",
		8: "7.1",
	}
	for ch, want := range cases {
		if got := ChannelLayout(ch); got != want {
			t.Errorf("ChannelLayout(%d) = %q, want %q", ch, got, want)
		}
	}
	// An unusual count still renders something rather than an empty label.
	if got := ChannelLayout(10); got == "" {
		t.Errorf("ChannelLayout(10) = %q, want a non-empty fallback", got)
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		name       string
		lang       string
		channels   int
		title      string
		commentary bool
		want       string
	}{
		{"english stereo", "en", 2, "", false, "English Stereo"},
		{"japanese 5.1", "ja", 6, "", false, "Japanese 5.1"},
		{"title tag wins over layout", "en", 2, "Director's Commentary", true, "English Director's Commentary"},
		{"untagged language is Unknown", "", 1, "", false, "Unknown Mono"},
		{"commentary flag without a title tag", "en", 2, "", true, "English Stereo Commentary"},
		{"no channels, no title", "fr", 0, "", false, "French"},
	}
	for _, c := range cases {
		if got := Label(c.lang, c.channels, c.title, c.commentary); got != c.want {
			t.Errorf("%s: Label(%q,%d,%q,%v) = %q, want %q",
				c.name, c.lang, c.channels, c.title, c.commentary, got, c.want)
		}
	}
}

// NormalizeLang is a thin reuse of the subtitle ISO-639 machinery; a spot check
// that the reuse is wired (jpn/ja/Japanese all fold to "ja", junk to "").
func TestNormalizeLangReuse(t *testing.T) {
	for _, raw := range []string{"jpn", "ja", "Japanese", "jpn-JP"} {
		if got := NormalizeLang(raw); got != "ja" {
			t.Errorf("NormalizeLang(%q) = %q, want ja", raw, got)
		}
	}
	if got := NormalizeLang("und"); got != "" {
		t.Errorf("NormalizeLang(und) = %q, want empty", got)
	}
}
