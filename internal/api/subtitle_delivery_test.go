package api_test

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/testharness"
)

// Issue subtitles/02 integration test: TEXT subtitle delivery on the direct-play
// tier, out-of-band, through the real API. Using the slice-01 "Sub Movie" fixture
// (embedded eng/fre SubRip Streams + text/image sidecars), a playback negotiation
// with a matroska-capable profile direct-plays the mkv, and the decision must list
// every Subtitle track — text tracks carrying an out-of-band .vtt URL, image tracks
// carrying none. Fetching a text track's URL must return valid WebVTT with the
// expected cue; an image track's URL is a 404; and the endpoint requires auth.
//
// The sidecar path needs no binaries (pure Go conversion); the embedded path uses
// real ffmpeg to extract the Stream, so that sub-assertion skips when ffmpeg is
// absent.

// mkvSubProfile is a Capability profile that direct-plays the Sub Movie mkv
// (matroska/h264/aac): the container + codecs the fixture was muxed with.
func mkvSubProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers": []string{"mp4", "mkv", "matroska"},
			"videoCodecs": []map[string]any{
				{"codec": "h264", "maxLevel": "4.2", "maxResolution": "1080p"},
			},
			"audioCodecs":         []string{"aac", "ac3"},
			"maxAudioChannels":    8,
			"textSubtitleFormats": []string{"webvtt"},
		},
		"constraints": map[string]any{
			"maxBitrate":            int64(100_000_000),
			"maxResolution":         "1080p",
			"preferredSubtitleLang": "en",
		},
	}
}

func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

func TestSubtitleTextDeliveryDirectPlay(t *testing.T) {
	requireSubtitleFixtures(t)
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, subtitlesRoot(t))
	scanLib(t, srv, token, libID, "")

	list := listAllTitles(t, srv, token, libID)
	id := findTitle(t, list, "Sub Movie")

	var dec decisionResp
	status, body := srv.JSON(http.MethodPost, "/api/v1/titles/"+id+"/playback", token, mkvSubProfile(), &dec)
	if status != http.StatusOK {
		t.Fatalf("playback status = %d, want 200; body: %s", status, body)
	}
	if dec.Tier != "directPlay" {
		t.Fatalf("tier = %q, want directPlay (the mkv fixture is h264/aac); body: %s", dec.Tier, body)
	}
	if dec.Subtitles == nil {
		t.Fatalf("decision subtitles is null, want a list; body: %s", body)
	}

	// Bucket the decision tracks and sanity-check the delivery URLs: every TEXT
	// track carries a title-scoped .vtt URL; every IMAGE track carries none.
	var textWithURL, imageTracks int
	var embeddedEN, sidecarEN decisionSubtitleResp
	for _, s := range dec.Subtitles {
		switch s.Kind {
		case "text":
			if s.URL == "" {
				t.Errorf("text track %+v has no delivery URL", s)
			} else {
				textWithURL++
				wantPrefix := "/api/v1/titles/" + id + "/subtitles/"
				if !strings.HasPrefix(s.URL, wantPrefix) || !strings.HasSuffix(s.URL, ".vtt") {
					t.Errorf("text track URL = %q, want %s...%s", s.URL, wantPrefix, ".vtt")
				}
			}
			if s.Source == "embedded" && s.Language == "en" {
				embeddedEN = s
			}
			if s.Source == "sidecar" && s.Language == "en" {
				sidecarEN = s
			}
		case "image":
			imageTracks++
			if s.URL != "" {
				t.Errorf("image track %+v must not carry a text delivery URL", s)
			}
		default:
			t.Errorf("unexpected subtitle kind %q", s.Kind)
		}
	}
	if textWithURL == 0 {
		t.Fatalf("no deliverable text tracks in the decision; got %+v", dec.Subtitles)
	}
	if imageTracks == 0 {
		t.Errorf("expected at least one image track (the .sup / VOBSUB sidecars); got %+v", dec.Subtitles)
	}

	// A SIDECAR text track converts from its on-disk SubRip with NO binaries — fetch
	// it and assert valid WebVTT with the expected cue.
	if sidecarEN.URL == "" {
		t.Fatalf("no sidecar English text track found; got %+v", dec.Subtitles)
	}
	assertValidWebVTT(t, srv, token, sidecarEN.URL, "Sidecar cue")

	// An IMAGE track has no URL, but constructing its endpoint by id must 404 (it is
	// not deliverable as text). Use a real image track id from the decision.
	var imageID string
	for _, s := range dec.Subtitles {
		if s.Kind == "image" {
			imageID = s.ID
			break
		}
	}
	if imageID != "" {
		imgURL := "/api/v1/titles/" + id + "/subtitles/" + imageID + ".vtt"
		st, _ := srv.AuthGET(imgURL, token, nil)
		if st != http.StatusNotFound {
			t.Errorf("image track .vtt status = %d, want 404 (not deliverable as text)", st)
		}
	}

	// The endpoint requires auth: an unauthenticated fetch is rejected (not 200).
	resp := srv.Do(http.MethodGet, sidecarEN.URL, nil)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("unauthenticated subtitle fetch returned 200, want a rejection")
	}

	// An EMBEDDED text track is extracted by real ffmpeg → valid WebVTT with the
	// fixture's "Hello" cue. Skips cleanly when ffmpeg isn't installed.
	if embeddedEN.URL == "" {
		t.Fatalf("no embedded English text track found; got %+v", dec.Subtitles)
	}
	if !ffmpegAvailable() {
		t.Log("ffmpeg not on PATH — skipping the embedded-extraction assertion")
		return
	}
	assertValidWebVTT(t, srv, token, embeddedEN.URL, "Hello")
}

// assertValidWebVTT fetches the given subtitle URL with the bearer token and
// asserts the body is a WebVTT track (WEBVTT header + a dotted-ms cue timing)
// containing wantCue.
func assertValidWebVTT(t *testing.T, srv *testharness.Server, token, url, wantCue string) {
	t.Helper()
	status, body := srv.AuthGET(url, token, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body: %s", url, status, body)
	}
	got := string(body)
	if !strings.HasPrefix(got, "WEBVTT") {
		t.Errorf("GET %s: body is not WebVTT (no WEBVTT header):\n%s", url, got)
	}
	if !strings.Contains(got, "-->") || !strings.Contains(got, ".") {
		t.Errorf("GET %s: body has no cue timing:\n%s", url, got)
	}
	if wantCue != "" && !strings.Contains(got, wantCue) {
		t.Errorf("GET %s: missing expected cue %q:\n%s", url, wantCue, got)
	}
}
