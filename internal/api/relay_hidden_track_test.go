package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// A relayed play for a User with a Playback ceiling never direct plays a File that
// holds a video track above it. The sharer's `remote` User carries no ceiling, so
// the sharer answers directPlay; the home Server re-asks for the approved track,
// which the sharer remuxes, and the raw File is then unreachable through the relay.

// hiddenTrackLibrary writes a Library holding one mkv: v0 h264 2160p (not default),
// v1 h264 720p (default), one aac track.
func hiddenTrackLibrary(t *testing.T) string {
	t.Helper()
	requireFFmpeg(t)
	root := t.TempDir()
	dir := filepath.Join(root, "Hidden (2020)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "Hidden (2020).mkv")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=3840x2160:rate=2",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=1280x720:rate=2",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
		"-map", "0:v", "-map", "1:v", "-map", "2:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-disposition:v:0", "0", "-disposition:v:1", "default",
		"-c:a", "aac", "-shortest", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the hidden-track fixture: %v: %s", err, b)
	}
	return root
}

func hiddenTrackProfile() map[string]any {
	return map[string]any{
		"deviceProfile": map[string]any{
			"containers":       []string{"mkv", "mp4"},
			"videoCodecs":      []map[string]any{{"codec": "h264", "maxLevel": "5.2", "maxResolution": "2160p"}},
			"audioCodecs":      []string{"aac"},
			"maxAudioChannels": 8,
		},
	}
}

func TestRelayHiddenTrackUnderCeiling(t *testing.T) {
	t.Parallel()
	root := hiddenTrackLibrary(t)
	sharer := testharness.New(t)
	sAdmin := adminToken(t, sharer)
	srcLib := createMovieLibrary(t, sharer, sAdmin, root)
	scanLib(t, sharer, sAdmin, srcLib, "")
	remote := createRemoteUser(t, sharer, sAdmin, "other household")
	grantLibraries(t, sharer, sAdmin, remote, srcLib)

	proxy := httptest.NewServer(sharer.Handler())
	t.Cleanup(proxy.Close)
	home := testharness.New(t)
	hAdmin := adminToken(t, home)
	invite := mintInvite(t, sharer, sAdmin, remote, proxy.URL).Invite
	var l linkResp
	if status, body := postLink(t, home, hAdmin, invite, &l); status != http.StatusCreated {
		t.Fatalf("POST /links = %d; body: %s", status, body)
	}
	mirror := l.Libraries[0].ID
	var ml titlesListResp
	home.AuthGET("/api/v1/libraries/"+mirror+"/titles", hAdmin, &ml)
	title := findTitle(t, ml, "Hidden")

	play := func(tok string) decisionResp {
		var dec decisionResp
		st, raw := home.JSON(http.MethodPost, "/api/v1/titles/"+title+"/playback", tok, hiddenTrackProfile(), &dec)
		if st != http.StatusOK {
			t.Fatalf("relayed playback = %d; body: %s", st, raw)
		}
		return dec
	}

	// Under a 1080p ceiling the relayed play is a remux of v1, and the raw File is hidden.
	kid := home.CreateUser(hAdmin, "kid", "memberpass123", "member")
	grantLibraries(t, home, hAdmin, kid, mirror)
	setPlaybackCeiling(t, home, hAdmin, kid, map[string]any{"maxResolution": "1080p"})
	kTok := home.LoginAs("kid", "memberpass123")
	dec := play(kTok)
	if dec.Tier != "directStream" {
		t.Fatalf("tier under a 1080p ceiling = %q, want directStream", dec.Tier)
	}
	assertRawStream404(t, authStream(t, home, "/api/v1/sessions/"+dec.SessionID+"/stream", kTok, ""), "bearer relay /stream")
	assertRawStream404(t, anonGET(t, home, tokenStream(dec.StreamToken), ""), "token relay /stream")

	// Without a ceiling nothing changes: a client that merely asks for 720p (no User
	// ceiling) keeps the sharer's directPlay of the same File.
	body := hiddenTrackProfile()
	body["constraints"] = map[string]any{"maxResolution": "720p"}
	var plain decisionResp
	if st, raw := home.JSON(http.MethodPost, "/api/v1/titles/"+title+"/playback", hAdmin, body, &plain); st != http.StatusOK || plain.Tier != "directPlay" {
		t.Errorf("no-ceiling relayed play = %d tier %q, want 200 directPlay; body: %.300s", st, plain.Tier, raw)
	}
}
