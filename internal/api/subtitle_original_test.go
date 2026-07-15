package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// ADR-0033 integration tests: original-format text-subtitle delivery through the
// real API. A client whose Capability profile declares a track's original format
// in textSubtitleFormats gets the ORIGINAL bytes (styling intact — the libmpv
// path); a client that declares only webvtt (the browser) gets the .vtt
// conversion exactly as before; and asking the endpoint for a format the track
// isn't in is a 404, not a transcode.
//
// The fixture is its own root (testdata/subtitles-original/) so the count-pinned
// Sub Movie assertions in subtitles_test.go stay untouched: one mkv with an
// embedded SubRip stream, plus an .srt and a real .ass sidecar. Generated lazily
// with ffmpeg; tests skip when ffmpeg is absent.

const subtitleOriginalRootRel = "subtitles-original"

const originalMovieDir = "Styled Movie (2021)"

// originalASS is a minimal but real ASS script — a Script Info head, a style, and
// one dialogue cue with an inline override tag. The override ({\i1}) is exactly
// what the WebVTT conversion strips and original delivery must preserve.
const originalASS = `[Script Info]
Title: Styled fixture
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour
Style: Default,Arial,20,&H00FFFFFF

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,{\i1}Styled cue{\i0}
`

const originalSRT = "1\n00:00:00,000 --> 00:00:01,000\nPlain cue\n"

var subtitleOriginalFixturesAvailable bool

func init() {
	subtitleOriginalFixturesAvailable = ensureSubtitleOriginalFixtures()
}

func requireSubtitleOriginalFixtures(t *testing.T) {
	t.Helper()
	if !subtitleOriginalFixturesAvailable {
		t.Skip("subtitle-original fixtures unavailable (ffmpeg not on PATH)")
	}
}

func ensureSubtitleOriginalFixtures() bool {
	dir := filepath.Join("testdata", subtitleOriginalRootRel, originalMovieDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	sidecars := map[string]string{
		"Styled Movie (2021).en.srt": originalSRT,
		"Styled Movie (2021).de.ass": originalASS,
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
	out := filepath.Join(dir, "Styled Movie (2021).mkv")
	if fileExists(out) {
		return true
	}
	// Reuses the Sub Movie generator: video + audio + two embedded SubRip streams
	// (eng default carrying the "Hello" cue).
	return generateEmbeddedSubClip(out)
}

func subtitleOriginalRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", subtitleOriginalRootRel))
	if err != nil {
		t.Fatalf("resolving subtitles-original root: %v", err)
	}
	return abs
}

// originalCapableProfile direct-plays the mkv and declares srt+ass+vtt support —
// the libmpv-shaped profile that should be offered originals.
func originalCapableProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers": []string{"mkv"},
			"videoCodecs": []map[string]any{
				{"codec": "h264", "maxResolution": "1080p"},
			},
			"audioCodecs":         []string{"aac"},
			"textSubtitleFormats": []string{"ass", "srt", "vtt"},
		},
	}
}

func negotiateStyled(t *testing.T, srv *testharness.Server, token, id string, payload map[string]any) decisionResp {
	t.Helper()
	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, payload, &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	return dec
}

func scanStyledMovie(t *testing.T) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitleOriginalRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, findTitle(t, listAllTitles(t, srv, token, libID), "Styled Movie")
}

func subtitleBySourceLang(t *testing.T, dec decisionResp, source, lang string) decisionSubtitleResp {
	t.Helper()
	for _, s := range dec.Subtitles {
		if s.Source == source && s.Language == lang {
			return s
		}
	}
	t.Fatalf("no %s %s subtitle in decision; got %+v", source, lang, dec.Subtitles)
	return decisionSubtitleResp{}
}

// TestSubtitleOriginalDeliveryToCapableClient: a profile declaring ass+srt gets
// original-format URLs — the .ass sidecar raw with its override tags intact, the
// .srt sidecar raw with SRT timings, and the embedded SubRip codec-copied to .srt.
func TestSubtitleOriginalDeliveryToCapableClient(t *testing.T) {
	requireSubtitleOriginalFixtures(t)
	srv, token, id := scanStyledMovie(t)

	dec := negotiateStyled(t, srv, token, id, originalCapableProfile())
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %q, want directPlay", dec.Tier)
	}

	// The .ass sidecar: original URL, format tag, and RAW bytes with styling.
	ass := subtitleBySourceLang(t, dec, "sidecar", "de")
	if !strings.HasSuffix(ass.URL, ".ass") || ass.Format != "ass" {
		t.Fatalf("ass sidecar delivery = {url:%q format:%q}, want .ass/ass", ass.URL, ass.Format)
	}
	status, body := srv.AuthGET(ass.URL, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body: %s", ass.URL, status, body)
	}
	if got := string(body); got != originalASS {
		t.Errorf("ass sidecar bytes are not the original:\n%s", got)
	}

	// The .srt sidecar: original URL and raw SRT (comma timings, cue counter).
	srt := subtitleBySourceLang(t, dec, "sidecar", "en")
	if !strings.HasSuffix(srt.URL, ".srt") || srt.Format != "srt" {
		t.Fatalf("srt sidecar delivery = {url:%q format:%q}, want .srt/srt", srt.URL, srt.Format)
	}
	status, body = srv.AuthGET(srt.URL, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", srt.URL, status)
	}
	if got := string(body); got != originalSRT {
		t.Errorf("srt sidecar bytes are not the original:\n%s", got)
	}

	// The embedded SubRip stream: codec-copied out by ffmpeg as .srt — SRT comma
	// timings prove no WebVTT conversion happened.
	emb := subtitleBySourceLang(t, dec, "embedded", "en")
	if !strings.HasSuffix(emb.URL, ".srt") || emb.Format != "srt" {
		t.Fatalf("embedded delivery = {url:%q format:%q}, want .srt/srt", emb.URL, emb.Format)
	}
	status, body = srv.AuthGET(emb.URL, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body: %s", emb.URL, status, body)
	}
	// Raw SRT uses comma-millisecond timings (WebVTT uses dots) and no WEBVTT
	// header; the container may nudge timestamps, so match the comma, not exact ms.
	got := string(body)
	if strings.HasPrefix(got, "WEBVTT") || !strings.Contains(got, "Hello") ||
		!strings.Contains(got, ",") || !strings.Contains(got, "-->") {
		t.Errorf("embedded original is not raw SRT:\n%s", got)
	}
}

// TestSubtitleVTTFallbackForBrowserProfile: a profile declaring only webvtt keeps
// the pre-ADR-0033 behavior byte-for-byte — every text URL is .vtt (the .ass
// sidecar converted, styling stripped) and format says "vtt".
func TestSubtitleVTTFallbackForBrowserProfile(t *testing.T) {
	requireSubtitleOriginalFixtures(t)
	srv, token, id := scanStyledMovie(t)

	payload := originalCapableProfile()
	payload["deviceProfile"].(map[string]any)["textSubtitleFormats"] = []string{"webvtt"}
	dec := negotiateStyled(t, srv, token, id, payload)

	for _, s := range dec.Subtitles {
		if s.Kind != "text" || s.URL == "" {
			continue
		}
		if !strings.HasSuffix(s.URL, ".vtt") || s.Format != "vtt" {
			t.Errorf("browser-profile track %+v: url/format = %q/%q, want .vtt/vtt", s, s.URL, s.Format)
		}
	}

	// The converted .ass still serves as valid, styling-stripped WebVTT.
	ass := subtitleBySourceLang(t, dec, "sidecar", "de")
	status, body := srv.AuthGET(ass.URL, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", ass.URL, status)
	}
	got := string(body)
	if !strings.HasPrefix(got, "WEBVTT") || !strings.Contains(got, "Styled cue") {
		t.Errorf("converted ass is not WebVTT with the cue:\n%s", got)
	}
	if strings.Contains(got, `{\i1}`) {
		t.Errorf("converted ass leaked override tags:\n%s", got)
	}
}

// TestSubtitleOriginalFormatMismatch404s: requesting a format the track is not in
// (.ass of an SRT sidecar, .srt of an ASS sidecar) is a 404 — original delivery
// never transcodes between subtitle formats.
func TestSubtitleOriginalFormatMismatch404s(t *testing.T) {
	requireSubtitleOriginalFixtures(t)
	srv, token, id := scanStyledMovie(t)

	dec := negotiateStyled(t, srv, token, id, originalCapableProfile())
	srtID := subtitleBySourceLang(t, dec, "sidecar", "en").ID
	assID := subtitleBySourceLang(t, dec, "sidecar", "de").ID

	for _, url := range []string{
		"/api/v1/titles/" + id + "/subtitles/" + srtID + ".ass",
		"/api/v1/titles/" + id + "/subtitles/" + assID + ".srt",
	} {
		if status, _ := srv.AuthGET(url, token, nil); status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 (format mismatch is not a transcode)", url, status)
		}
	}
}
