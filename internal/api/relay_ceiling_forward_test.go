package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// The home Server forwards its User's resolution ceiling as ceilingResolution to a
// sharer advertising features.relayCeiling, and the sharer applies it as its own
// ceiling for that negotiation. A client's own ceilingResolution is ignored. A sharer
// without the flag falls back to the home-side re-ask.

func ceilingFixture(t *testing.T, dir, name string, hidden bool, acodec string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	args := []string{"-y", "-loglevel", "error"}
	if hidden {
		args = append(args,
			"-f", "lavfi", "-i", "testsrc=duration=2:size=3840x2160:rate=2",
			"-f", "lavfi", "-i", "testsrc=duration=2:size=1280x720:rate=2",
			"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
			"-map", "0:v", "-map", "1:v", "-map", "2:a",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-disposition:v:0", "0", "-disposition:v:1", "default")
	} else {
		args = append(args,
			"-f", "lavfi", "-i", "testsrc=duration=2:size=1280x720:rate=2",
			"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
			"-c:v", "libx264", "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:a", acodec, "-shortest", out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot build the fixture %s: %v: %s", name, err, b)
	}
}

func ceilingProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mkv", "mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxLevel": "5.2", "maxResolution": "2160p"}},
			"audioCodecs":      []string{"aac", "flac"},
			"maxAudioChannels": 8,
		},
	}
}

type ceilingRig struct {
	home         *testharness.Server
	hAdmin       string
	kid          string
	sharer       *testharness.Server
	sAdmin       string
	sharerTitles titlesListResp
	titles       titlesListResp
	negotiations *int64
}

// newCeilingRig links a home Server (with a 1080p-capped member) to a sharer holding
// Plain, Flachid (hidden 2160p track, FLAC audio), Aachid (hidden track, AAC) and a
// two-Edition Multi. stripFlag removes features.relayCeiling from the sharer's handshake.
func newCeilingRig(t *testing.T, stripFlag bool) *ceilingRig {
	t.Helper()
	requireFFmpeg(t)
	root := t.TempDir()
	ceilingFixture(t, filepath.Join(root, "Plain (2021)"), "Plain (2021).mkv", false, "aac")
	ceilingFixture(t, filepath.Join(root, "Flachid (2020)"), "Flachid (2020).mkv", true, "flac")
	ceilingFixture(t, filepath.Join(root, "Aachid (2019)"), "Aachid (2019).mkv", true, "aac")
	ceilingFixture(t, filepath.Join(root, "Multi (2018)"), "Multi (2018) {edition-Lean}.mkv", false, "aac")
	ceilingFixture(t, filepath.Join(root, "Multi (2018)"), "Multi (2018) {edition-Hidden}.mkv", true, "aac")

	sharer := testharness.New(t)
	sAdmin := adminToken(t, sharer)
	srcLib := createMovieLibrary(t, sharer, sAdmin, root)
	scanLib(t, sharer, sAdmin, srcLib, "")
	remote := createRemoteUser(t, sharer, sAdmin, "other household")
	grantLibraries(t, sharer, sAdmin, remote, srcLib)

	var negs int64
	h := sharer.Handler()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/playback") {
			atomic.AddInt64(&negs, 1)
		}
		if stripFlag && r.URL.Path == "/api/v1/server" {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var m map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Errorf("handshake: %v", err)
			}
			delete(m["features"].(map[string]any), "relayCeiling")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	home := testharness.New(t)
	hAdmin := adminToken(t, home)
	invite := mintInvite(t, sharer, sAdmin, remote, proxy.URL).Invite
	var l linkResp
	if status, body := postLink(t, home, hAdmin, invite, &l); status != http.StatusCreated {
		t.Fatalf("POST /links = %d; body: %s", status, body)
	}
	mirror := l.Libraries[0].ID
	kid := home.CreateUser(hAdmin, "kid", "memberpass123", "member")
	grantLibraries(t, home, hAdmin, kid, mirror)
	setPlaybackCeiling(t, home, hAdmin, kid, map[string]any{"maxResolution": "1080p"})
	rig := &ceilingRig{home: home, hAdmin: hAdmin, kid: home.LoginAs("kid", "memberpass123"), sharer: sharer, sAdmin: sAdmin, negotiations: &negs}
	home.AuthGET("/api/v1/libraries/"+mirror+"/titles", hAdmin, &rig.titles)
	sharer.AuthGET("/api/v1/libraries/"+srcLib+"/titles", sAdmin, &rig.sharerTitles)
	return rig
}

func (r *ceilingRig) play(t *testing.T, tok, title string, body map[string]any) (int, decisionResp, []byte) {
	t.Helper()
	var dec decisionResp
	st, raw := r.home.JSON(http.MethodPost, "/api/v1/titles/"+title+"/playback", tok, body, &dec)
	return st, dec, raw
}

func TestRelayForwardedCeilingPlaysFlacHiddenTrackAndReplacesNormally(t *testing.T) {
	t.Parallel()
	rig := newCeilingRig(t, false)
	plain, flac := findTitle(t, rig.titles, "Plain"), findTitle(t, rig.titles, "Flachid")

	st, first, raw := rig.play(t, rig.kid, plain, ceilingProfile())
	if st != http.StatusOK {
		t.Fatalf("plain play = %d: %s", st, raw)
	}
	before := atomic.LoadInt64(rig.negotiations)
	body := ceilingProfile()
	body["replacesSessionId"] = first.SessionID
	st, dec, raw := rig.play(t, rig.kid, flac, body)
	if st != http.StatusOK {
		t.Fatalf("FLAC hidden-track replacing play = %d, want 200; body: %.300s", st, raw)
	}
	if dec.Tier == "directPlay" || dec.VideoStream == nil {
		t.Errorf("tier %q: the raw File carries the 2160p track", dec.Tier)
	}
	if n := atomic.LoadInt64(rig.negotiations) - before; n != 1 {
		t.Errorf("sharer negotiations for the play = %d, want 1 (the sharer applied the ceiling, no re-ask)", n)
	}
	assertRawStream404(t, authStream(t, rig.home, "/api/v1/sessions/"+dec.SessionID+"/stream", rig.kid, ""), "relay /stream")
}

func TestRelayForwardedCeilingKeepsTheEditionRungs(t *testing.T) {
	t.Parallel()
	rig := newCeilingRig(t, false)
	st, dec, raw := rig.play(t, rig.kid, findTitle(t, rig.titles, "Multi"), ceilingProfile())
	if st != http.StatusOK {
		t.Fatalf("Multi play = %d: %s", st, raw)
	}
	if dec.Tier == "directPlay" {
		t.Errorf("tier directPlay for a File with a 2160p track under a 1080p ceiling")
	}
	var m struct {
		StepDown []struct {
			Label string `json:"label"`
		} `json:"stepDown"`
	}
	_ = json.Unmarshal(raw, &m)
	labels := []string{}
	for _, r := range m.StepDown {
		labels = append(labels, r.Label)
	}
	if !strings.Contains(strings.Join(labels, "|"), "Lean · 720p") {
		t.Errorf("ladder %v lacks Lean · 720p", labels)
	}
}

// A sharer without features.relayCeiling is never sent the field; the home-side
// re-ask still closes the bypass.
func TestRelayWithoutRelayCeilingFlagFallsBackToTheReask(t *testing.T) {
	t.Parallel()
	rig := newCeilingRig(t, true)
	before := atomic.LoadInt64(rig.negotiations)
	st, dec, raw := rig.play(t, rig.kid, findTitle(t, rig.titles, "Aachid"), ceilingProfile())
	if st != http.StatusOK {
		t.Fatalf("play = %d, want 200 (a sharer without the flag must not be sent the field); body: %.300s", st, raw)
	}
	if dec.Tier != "directStream" {
		t.Errorf("tier %q, want directStream from the re-ask", dec.Tier)
	}
	if n := atomic.LoadInt64(rig.negotiations) - before; n != 2 {
		t.Errorf("sharer negotiations = %d, want 2 (ask, then re-ask)", n)
	}
	assertRawStream404(t, authStream(t, rig.home, "/api/v1/sessions/"+dec.SessionID+"/stream", rig.kid, ""), "fallback relay /stream")
}

// A client of the sharer cannot set its own ceiling through the field.
func TestClientCeilingResolutionFieldIsIgnored(t *testing.T) {
	t.Parallel()
	rig := newCeilingRig(t, false)
	aac := findTitle(t, rig.sharerTitles, "Aachid")
	body := ceilingProfile()
	// The client's own maxResolution makes the 720p track the pick, so the File direct
	// plays; a honoured ceilingResolution would turn it into a remux (a track above it).
	body["constraints"] = map[string]any{"maxResolution": "720p"}
	body["ceilingResolution"] = "480p"
	var dec decisionResp
	st, raw := rig.sharer.JSON(http.MethodPost, "/api/v1/titles/"+aac+"/playback", rig.sAdmin, body, &dec)
	if st != http.StatusOK || dec.Tier != "directPlay" {
		t.Errorf("admin play with ceilingResolution = %d tier %q, want 200 directPlay (field ignored); body: %.300s", st, dec.Tier, raw)
	}
}
