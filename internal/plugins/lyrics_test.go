package plugins_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestAnInstalledLyricProviderRegistersAndAnswers is the loader-level tracer: a
// manifest declaring lyric-provider reaches the registry with its kinds, the one
// call crosses the sandbox carrying the whole request, reaches the source at the
// URL the Settings carry, and the source's answer comes back unchanged — the
// host's judgment is not this layer's.
func TestAnInstalledLyricProviderRegistersAndAnswers(t *testing.T) {
	plugins.Parallel(t)
	var asked pluginapi.LyricsRequest
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &asked)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"synced","lines":[{"startMs":1500,"text":"First"}],` +
			`"durationMs":999999,"recordingId":"someone-else"}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.LyricManifest("example-lyrics", source.URL))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.LyricProvider("example-lyrics")
	if !ok {
		t.Fatal("the Set registered no Lyric provider for example-lyrics")
	}
	d := registration.Descriptor
	if d.ExtensionPoint != pluginapi.ExtensionLyricProvider || !d.Serves(pluginapi.KindMusic) || d.DefaultURL != source.URL {
		t.Fatalf("descriptor = %+v, want a lyric-provider serving music from %s", d, source.URL)
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: d.DefaultURL, URLEntered: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	req := pluginapi.LyricsRequest{Artist: "Lyric Band", Title: "Words", Album: "Words", DurationMs: 1000, RecordingID: "rec-1"}
	resp, err := provider.Lyrics(context.Background(), req)
	if err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	if asked != req {
		t.Fatalf("the source was asked %+v, want %+v", asked, req)
	}
	if resp.Kind != pluginapi.LyricsSynced || len(resp.Lines) != 1 || resp.Lines[0].Text != "First" ||
		resp.DurationMs != 999999 || resp.RecordingID != "someone-else" {
		t.Fatalf("response = %+v, want the source's answer unchanged", resp)
	}
}

// TestALyricProviderDeclaringNoKindsIsLoggedOnceAtRegistration: a provider that
// declares no kinds serves no track, so it is never asked, and nor is one that
// declares only kinds without lyrics. That is said once, naming the Plugin, when
// it is registered — and not for one that declares music.
func TestALyricProviderDeclaringNoKindsIsLoggedOnceAtRegistration(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	none := plugintest.LyricManifest("kindless-lyrics", "https://lyrics.example.test")
	none.Provides[0].Kinds = nil
	plugintest.Install(t, dataDir, none)
	videoOnly := plugintest.LyricManifest("video-lyrics", "https://lyrics.example.test")
	videoOnly.Provides[0].Kinds = []string{pluginapi.KindVideo}
	plugintest.Install(t, dataDir, videoOnly)
	plugintest.Install(t, dataDir, plugintest.LyricManifest("music-lyrics", "https://lyrics.example.test"))
	log := &logSink{}
	set := loadWith(t, dataDir, log, plugins.Options{})
	set.Register(pluginapi.NewRegistry())

	if n := strings.Count(log.all(), "kindless-lyrics declares no kinds"); n != 1 {
		t.Fatalf("logged the kindless provider %d times, want once:\n%s", n, log.all())
	}
	if n := strings.Count(log.all(), "video-lyrics declares no kinds"); n != 1 {
		t.Fatalf("logged the video-only provider %d times, want once:\n%s", n, log.all())
	}
	if strings.Contains(log.all(), "music-lyrics declares no kinds") {
		t.Fatalf("logged a provider that declares music:\n%s", log.all())
	}
}
