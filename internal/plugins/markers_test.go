package plugins_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestAnInstalledMarkerProviderRegistersAndAnswers is the loader-level tracer: a
// manifest declaring marker-provider reaches the registry with its kinds, the
// one call crosses the sandbox carrying the whole request, reaches the source at
// the URL the Settings carry, and the source's answer comes back unchanged — a
// candidate timed for another length included, because the host's judgment is
// not this layer's.
func TestAnInstalledMarkerProviderRegistersAndAnswers(t *testing.T) {
	plugins.Parallel(t)
	var asked pluginapi.MarkersRequest
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &asked)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"markers":[{"kind":"intro","startMs":1000,"endMs":2000,"durationMs":999999}]}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MarkerManifest("example-markers", source.URL))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.MarkerProvider("example-markers")
	if !ok {
		t.Fatal("the Set registered no Marker provider for example-markers")
	}
	d := registration.Descriptor
	if d.ExtensionPoint != pluginapi.ExtensionMarkerProvider || !d.Serves(pluginapi.KindVideo) || d.DefaultURL != source.URL {
		t.Fatalf("descriptor = %+v, want a marker-provider serving video from %s", d, source.URL)
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: d.DefaultURL, URLEntered: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	req := pluginapi.MarkersRequest{
		Kind: "episode", Title: "Pilot", IDs: map[string]string{"imdb": "tt1"},
		ShowTitle: "Show", ShowIDs: map[string]string{"tmdb": "2"}, SeasonNumber: 1, EpisodeNumber: 3, DurationMs: 1000,
	}
	resp, err := provider.Markers(context.Background(), req)
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	if !reflect.DeepEqual(asked, req) {
		t.Fatalf("the source was asked %+v, want %+v", asked, req)
	}
	want := []pluginapi.MarkerCandidate{{Kind: pluginapi.MarkerIntro, StartMs: 1000, EndMs: 2000, DurationMs: 999999}}
	if !reflect.DeepEqual(resp.Markers, want) {
		t.Fatalf("response = %+v, want the source's answer unchanged", resp)
	}
}

// TestMarkerCallsBehindAHungGuestEndWithinTheCallTimeout: a source that never
// answers holds the Plugin's one instance until the deadline kills the call.
// Calls queued behind it must give up by their own call timeout, not wait out
// every call ahead of them in turn — a read waits on them.
func TestMarkerCallsBehindAHungGuestEndWithinTheCallTimeout(t *testing.T) {
	plugins.Parallel(t)
	const timeout = time.Second
	const calls = 5
	release := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer source.Close()
	defer close(release)

	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MarkerManifest("example-markers", source.URL))
	// A short fetch grace, so each call holds the instance for nearly its whole
	// budget, and a threshold no run of timeouts reaches, so a disabled Plugin's
	// quick refusal cannot stand in for a call that gave up in time.
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{CallTimeout: timeout, FetchGrace: 100 * time.Millisecond, FailureThreshold: 100})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.MarkerProvider("example-markers")
	if !ok {
		t.Fatal("the Set registered no Marker provider for example-markers")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: source.URL, URLEntered: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	elapsed := make([]time.Duration, calls)
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			_, errs[i] = provider.Markers(context.Background(), pluginapi.MarkersRequest{Kind: "movie", Title: "Dune", DurationMs: 1000})
			elapsed[i] = time.Since(start)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] == nil {
			t.Errorf("call %d answered no error from a source that never answers", i)
		}
		if elapsed[i] > timeout+time.Second {
			t.Errorf("call %d took %v, want no more than the %v call timeout and a little slack", i, elapsed[i], timeout)
		}
	}
}
