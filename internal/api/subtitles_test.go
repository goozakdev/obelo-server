package api_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue subtitles/01 integration test: the read path end to end. A fixture Movie
// carries embedded subtitle Streams (eng default + fre forced) plus a spread of
// Sidecar subtitles (labeled/forced/unlabeled text, a .sup image, and a VOBSUB
// .idx/.sub pair). After a scan, GET /titles/{id} must expose them all as one
// `subtitles` list with the right source, kind, normalized language, and forced
// flags — through the real ffprobe/scanner/store/API stack, never internals.
//
// Fixtures live under testdata/subtitles/ (its own root, so other trees are
// untouched); the mkv is generated with ffmpeg lazily and the test skips if
// ffmpeg is absent.

const subtitlesRootRel = "subtitles"

const subMovieDir = "Sub Movie (2020)"

var subtitleFixturesAvailable bool

func init() {
	subtitleFixturesAvailable = ensureSubtitleFixtures()
}

func requireSubtitleFixtures(t *testing.T) {
	t.Helper()
	if !subtitleFixturesAvailable {
		t.Skip("subtitle fixtures unavailable (ffmpeg not on PATH)")
	}
}

// ensureSubtitleFixtures generates the Sub Movie (2020) fixture if missing: an
// mkv muxing two embedded subtitle streams, plus the sidecar files (dummy
// content — the scanner keys off filenames, not bytes).
func ensureSubtitleFixtures() bool {
	dir := filepath.Join("testdata", subtitlesRootRel, subMovieDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}

	// Sidecar files: text (labeled / forced / unlabeled), an image .sup, and a
	// VOBSUB .idx/.sub pair (one logical track). The TEXT sidecars carry real
	// SubRip so the slice-02 delivery test can fetch a valid WebVTT track from them
	// (the scanner keys off filenames for the counts slice-01 asserts, but delivery
	// reads the bytes). The image sidecars' content is irrelevant (never served as
	// text), so they stay dummy.
	textSRT := "1\n00:00:00,000 --> 00:00:01,000\nSidecar cue\n"
	sidecars := map[string]string{
		"Sub Movie (2020).en.srt":        textSRT,
		"Sub Movie (2020).es.forced.srt": textSRT,
		"Sub Movie (2020).srt":           textSRT, // no language → Unknown
		"Sub Movie (2020).de.sup":        "dummy subtitle fixture\n",
		"Sub Movie (2020).it.idx":        "dummy subtitle fixture\n",
		"Sub Movie (2020).it.sub":        "dummy subtitle fixture\n",
		// A sidecar for the trailer Extra, not the movie — must NOT surface as a
		// Title subtitle (so the sidecar count stays 5, not 6).
		"Sub Movie (2020)-trailer.en.srt": textSRT,
	}
	for name, content := range sidecars {
		p := filepath.Join(dir, name)
		if fileExists(p) {
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return false
		}
	}

	out := filepath.Join(dir, "Sub Movie (2020).mkv")
	if fileExists(out) {
		return true
	}
	return generateEmbeddedSubClip(out)
}

// generateEmbeddedSubClip muxes a 1s clip with a video, an audio, and two
// embedded SubRip subtitle streams: stream 0 = English (default), stream 1 =
// French (forced). Returns false if ffmpeg is unavailable or fails.
func generateEmbeddedSubClip(out string) bool {
	tmp, err := os.MkdirTemp("", "subfix")
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)

	srt := "1\n00:00:00,000 --> 00:00:01,000\nHello\n"
	engSRT := filepath.Join(tmp, "eng.srt")
	freSRT := filepath.Join(tmp, "fre.srt")
	if os.WriteFile(engSRT, []byte(srt), 0o644) != nil || os.WriteFile(freSRT, []byte(srt), 0o644) != nil {
		return false
	}

	cmd := exec.Command("ffmpeg",
		"-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=24",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-i", engSRT,
		"-i", freSRT,
		"-map", "0:v", "-map", "1:a", "-map", "2", "-map", "3",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-c:s", "srt",
		"-metadata:s:s:0", "language=eng", "-disposition:s:0", "default",
		"-metadata:s:s:1", "language=fre", "-disposition:s:1", "forced",
		"-shortest", out)
	return cmd.Run() == nil
}

func subtitlesRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", subtitlesRootRel))
	if err != nil {
		t.Fatalf("resolving subtitles root: %v", err)
	}
	return abs
}

// --- wire shapes (only the subtitle surface) --------------------------------

type subtitleTrackResp struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Kind     string `json:"kind"`
	Language string `json:"language"`
	Forced   bool   `json:"forced"`
	Label    string `json:"label"`
}

type subtitleDetailResp struct {
	ID        string              `json:"id"`
	Title     string              `json:"title"`
	Editions  []editionResp       `json:"editions"`
	Subtitles []subtitleTrackResp `json:"subtitles"`
}

func scanSubtitleMovie(t *testing.T) subtitleDetailResp {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Sub Movie")

	var d subtitleDetailResp
	status, body := srv.AuthGET("/api/v1/titles/"+id, token, &d)
	if status != http.StatusOK {
		t.Fatalf("get title status = %d, want 200; body: %s", status, body)
	}
	return d
}

func TestSubtitleTracksListed(t *testing.T) {
	requireSubtitleFixtures(t)
	d := scanSubtitleMovie(t)

	// Group by (source, kind, language) for assertions; count forced separately.
	var embedded, sidecar, image int
	byLang := map[string]subtitleTrackResp{}
	var unknownCount, vobsubIt int
	for _, s := range d.Subtitles {
		switch s.Source {
		case "embedded":
			embedded++
		case "sidecar":
			sidecar++
		default:
			t.Errorf("unexpected subtitle source %q", s.Source)
		}
		if s.Kind == "image" {
			image++
		}
		if s.Language == "" {
			unknownCount++
			if s.Label != "Unknown" {
				t.Errorf("unlabeled sub: label = %q, want Unknown", s.Label)
			}
		}
		if s.Kind == "image" && s.Language == "it" {
			vobsubIt++
		}
		byLang[s.Source+"/"+s.Language] = s
	}

	// Embedded: two streams, languages normalized eng→en / fre→fr, fre forced.
	if embedded != 2 {
		t.Errorf("embedded track count = %d, want 2 (eng + fre)", embedded)
	}
	if en, ok := byLang["embedded/en"]; !ok {
		t.Error("missing embedded English track (eng should normalize to en)")
	} else if en.Kind != "text" || en.Forced {
		t.Errorf("embedded en: kind=%q forced=%v, want text/false", en.Kind, en.Forced)
	}
	if fr, ok := byLang["embedded/fr"]; !ok {
		t.Error("missing embedded French track (fre should normalize to fr)")
	} else if !fr.Forced || fr.Label != "French (Forced)" {
		t.Errorf("embedded fr: forced=%v label=%q, want true/'French (Forced)'", fr.Forced, fr.Label)
	}

	// Sidecars: en, es(forced), unlabeled, de(.sup image), it(VOBSUB image). The
	// trailer sidecar is excluded, so the count is 5 not 6.
	if sidecar != 5 {
		t.Errorf("sidecar track count = %d, want 5 (trailer sidecar excluded); tracks: %+v", sidecar, d.Subtitles)
	}
	if es, ok := byLang["sidecar/es"]; !ok || !es.Forced {
		t.Errorf("sidecar es forced track missing/not forced: %+v (ok=%v)", es, ok)
	}
	if unknownCount != 1 {
		t.Errorf("unknown-language track count = %d, want exactly 1 (the unlabeled .srt)", unknownCount)
	}
	// The VOBSUB .idx/.sub pair must collapse to exactly ONE image track.
	if vobsubIt != 1 {
		t.Errorf("VOBSUB Italian image tracks = %d, want exactly 1 (idx/sub pair is one track)", vobsubIt)
	}
	// Two image sidecars total: the .sup (de) and the VOBSUB pair (it).
	if image != 2 {
		t.Errorf("image track count = %d, want 2 (.sup + VOBSUB pair)", image)
	}
	if de, ok := byLang["sidecar/de"]; !ok || de.Kind != "image" {
		t.Errorf("sidecar de .sup should be an image track: %+v (ok=%v)", de, ok)
	}
}

// TestSubtitleRescanNoDuplicates: an incremental rescan keeps the local subtitle
// rows current without duplicating them (a fetched row, arriving in slice 05,
// would likewise survive by construction).
func TestSubtitleRescanNoDuplicates(t *testing.T) {
	requireSubtitleFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))

	scanLib(t, srv, token, libID, "")
	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Sub Movie")

	var first subtitleDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &first)

	// Rescan (incremental): nothing on disk changed.
	scanLib(t, srv, token, libID, "incremental")
	var second subtitleDetailResp
	srv.AuthGET("/api/v1/titles/"+id, token, &second)

	if len(second.Subtitles) != len(first.Subtitles) {
		t.Fatalf("subtitle count changed across rescan: %d → %d (duplication?)",
			len(first.Subtitles), len(second.Subtitles))
	}
	if len(first.Subtitles) == 0 {
		t.Fatal("expected subtitle tracks on the fixture, got none")
	}
}
