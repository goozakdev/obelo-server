package plugins_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: d.DefaultURL})
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
