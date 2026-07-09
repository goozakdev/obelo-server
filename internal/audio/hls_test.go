package audio

import (
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/subtitle"
)

// TestMasterPlaylistCarriesAudioGroup: a demuxed multi-audio master advertises one
// #EXT-X-MEDIA:TYPE=AUDIO rendition per Stream (language/label/default/URI) and a
// single video variant that references the AUDIO group.
func TestMasterPlaylistCarriesAudioGroup(t *testing.T) {
	master := string(MasterPlaylist("index.m3u8", Variant{}, []Rendition{
		{URI: "audio_a.m3u8", Name: "English Stereo", Language: "en", Default: true},
		{URI: "audio_b.m3u8", Name: "Japanese 5.1", Language: "ja", Default: false},
	}, nil))

	for _, want := range []string{
		"#EXTM3U",
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud"`,
		`NAME="English Stereo"`,
		`LANGUAGE="en"`,
		`URI="audio_a.m3u8"`,
		`NAME="Japanese 5.1"`,
		`#EXT-X-STREAM-INF:BANDWIDTH=`,
		`AUDIO="aud"`,
		"index.m3u8",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master missing %q:\n%s", want, master)
		}
	}

	// Exactly one rendition is DEFAULT=YES (the resolved default audio Stream).
	if got := strings.Count(master, "DEFAULT=YES"); got != 1 {
		t.Errorf("DEFAULT=YES count = %d, want 1:\n%s", got, master)
	}
	// The English rendition is the default, the Japanese one is not.
	if !strings.Contains(master, `NAME="English Stereo",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="audio_a.m3u8"`) {
		t.Errorf("English rendition not marked DEFAULT=YES:\n%s", master)
	}
	if !strings.Contains(master, `NAME="Japanese 5.1",LANGUAGE="ja",DEFAULT=NO,AUTOSELECT=YES,URI="audio_b.m3u8"`) {
		t.Errorf("Japanese rendition not marked DEFAULT=NO:\n%s", master)
	}
}

// TestMasterPlaylistComposesAudioAndSubtitles: one master carries BOTH an AUDIO
// group and a SUBTITLES group, and the video variant references both.
func TestMasterPlaylistComposesAudioAndSubtitles(t *testing.T) {
	master := string(MasterPlaylist("index.m3u8", Variant{},
		[]Rendition{{URI: "audio_a.m3u8", Name: "English Stereo", Language: "en", Default: true}},
		[]subtitle.Rendition{{URI: "subs_x.m3u8", Name: "English", Language: "en"}},
	))
	if !strings.Contains(master, `TYPE=AUDIO`) || !strings.Contains(master, `TYPE=SUBTITLES`) {
		t.Errorf("master missing one of the two groups:\n%s", master)
	}
	if !strings.Contains(master, `AUDIO="aud"`) || !strings.Contains(master, `SUBTITLES="subs"`) {
		t.Errorf("video variant must reference both groups:\n%s", master)
	}
}

// TestMasterPlaylistNoAudioMatchesSubtitleOnly: with no audio renditions the output
// is byte-identical to subtitle.MasterPlaylist — a subtitle-only session is
// unchanged by this slice (the regression pin for the composed builder).
func TestMasterPlaylistNoAudioMatchesSubtitleOnly(t *testing.T) {
	subs := []subtitle.Rendition{
		{URI: "subs_x.m3u8", Name: "English", Language: "en"},
		{URI: "subs_y.m3u8", Name: "Forced", Language: "en", Forced: true},
	}
	unified := string(MasterPlaylist("index.m3u8", Variant{}, nil, subs))
	legacy := string(subtitle.MasterPlaylist("index.m3u8", subs))
	if unified != legacy {
		t.Errorf("unified master (no audio) diverged from subtitle.MasterPlaylist:\n--- unified ---\n%s\n--- legacy ---\n%s", unified, legacy)
	}
}

// TestMasterPlaylistHDRVariantAttributes: the video variant carries the honest
// BANDWIDTH/RESOLUTION/FRAME-RATE/VIDEO-RANGE. For HDR (PQ) content these are
// LOAD-BEARING on Safari: an HDR stream under an implicitly-SDR variant is killed
// with a bare decode error, and a PQ variant without RESOLUTION/FRAME-RATE is
// never even loaded (the nothing-plays Safari bug on a 4K HDR remux).
func TestMasterPlaylistHDRVariantAttributes(t *testing.T) {
	master := string(MasterPlaylist("index.m3u8", Variant{
		Bandwidth:  90_000_000,
		Codecs:     "hvc1.1.6.L153.B0,mp4a.40.2",
		Width:      3840,
		Height:     2160,
		FrameRate:  23.976,
		VideoRange: "PQ",
	}, []Rendition{{URI: "audio_a.m3u8", Name: "English 7.1", Language: "en", Default: true}}, nil))
	for _, want := range []string{
		"BANDWIDTH=90000000",
		`CODECS="hvc1.1.6.L153.B0,mp4a.40.2"`,
		"RESOLUTION=3840x2160",
		"FRAME-RATE=23.976",
		"VIDEO-RANGE=PQ",
	} {
		if !strings.Contains(master, want) {
			t.Errorf("master variant missing %q:\n%s", want, master)
		}
	}
	// SDR / unknown traits omit the attributes entirely (implicit SDR is correct).
	sdr := string(MasterPlaylist("index.m3u8", Variant{Bandwidth: 5_000_000}, nil, nil))
	for _, absent := range []string{"VIDEO-RANGE", "RESOLUTION", "FRAME-RATE"} {
		if strings.Contains(sdr, absent) {
			t.Errorf("SDR variant must omit %s:\n%s", absent, sdr)
		}
	}
}
