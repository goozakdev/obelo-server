package plugins_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/sdkguesttest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The SDK-built guest as each of the four Extension points it gained a
// dispatcher for — Web reference, Lyric, Marker and password Sign-in provider —
// across the real ABI. What each provider answers is pluginsdk's own example
// (pluginsdk/internal/testprovider/seams.go); what is proved here is that the
// dispatcher's export is the one the host calls and that the answer crosses.

// TestASDKBuiltGuestAnswersAsAWebReferenceProvider: the guest links the IMDb id
// it is handed, under the offline policy the seam's call runs under.
func TestASDKBuiltGuestAnswersAsAWebReferenceProvider(t *testing.T) {
	plugins.Parallel(t)
	dataDir := t.TempDir()
	sdkguesttest.Install(t, dataDir, sdkguesttest.WebReferenceManifest("sdk-refs"))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.WebReferenceProvider("sdk-refs")
	if !ok {
		t.Fatal("the Set registered no Web reference provider for sdk-refs")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	resp, err := provider.Links(context.Background(), pluginapi.WebReferencesRequest{
		Kind: "movie", IDs: map[string]string{"imdb": "tt0111161", "tmdb": "278"},
	})
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	want := []pluginapi.WebReference{{Namespace: "imdb", ID: "tt0111161", Label: "IMDb", URL: "https://www.imdb.com/title/tt0111161/"}}
	if !reflect.DeepEqual(resp.References, want) {
		t.Fatalf("references = %+v, want %+v", resp.References, want)
	}
}

// TestASDKBuiltGuestAnswersAsALyricProvider: the guest asks the source at the
// URL the Settings carry, by artist, title, album and duration, and answers a
// Synced source answer as Synced; a source that has nothing (404) is an empty
// answer, not a failure.
func TestASDKBuiltGuestAnswersAsALyricProvider(t *testing.T) {
	plugins.Parallel(t)
	var asked map[string]string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lyrics" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		asked = map[string]string{"artist": q.Get("artist"), "title": q.Get("title"), "album": q.Get("album"),
			"duration_ms": q.Get("duration_ms")}
		if q.Get("title") == "Unknown" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"synced":[{"startMs":1500,"text":"First"}],"plain":"First","durationMs":201000}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	sdkguesttest.Install(t, dataDir, sdkguesttest.LyricManifest("sdk-lyrics", source.URL))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.LyricProvider("sdk-lyrics")
	if !ok {
		t.Fatal("the Set registered no Lyric provider for sdk-lyrics")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: registration.Descriptor.DefaultURL, URLEntered: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	resp, err := provider.Lyrics(context.Background(), pluginapi.LyricsRequest{
		Artist: "Lyric Band", Title: "Words", Album: "Words", DurationMs: 201000,
	})
	if err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	wantAsked := map[string]string{"artist": "Lyric Band", "title": "Words", "album": "Words", "duration_ms": "201000"}
	if !reflect.DeepEqual(asked, wantAsked) {
		t.Fatalf("the source was asked %v, want %v", asked, wantAsked)
	}
	want := pluginapi.LyricsResponse{Kind: pluginapi.LyricsSynced, Lines: []pluginapi.LyricLine{{StartMs: 1500, Text: "First"}}, DurationMs: 201000}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}

	resp, err = provider.Lyrics(context.Background(), pluginapi.LyricsRequest{Artist: "Lyric Band", Title: "Unknown"})
	if err != nil || !reflect.DeepEqual(resp, pluginapi.LyricsResponse{}) {
		t.Fatalf("a miss = %+v, %v; want an empty answer and no error", resp, err)
	}
}

// TestASDKBuiltGuestAnswersAsAMarkerProvider: the guest asks the source at the
// URL the Settings carry by the item's IMDb id, and answers every span it
// returns with the length of the recording they were measured on.
func TestASDKBuiltGuestAnswersAsAMarkerProvider(t *testing.T) {
	plugins.Parallel(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markers" || r.URL.Query().Get("imdb") != "tt0959621" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"durationMs":2900000,"markers":[{"kind":"intro","startMs":10000,"endMs":40000},` +
			`{"kind":"credits","startMs":2800000,"endMs":2900000}]}`))
	}))
	defer source.Close()

	dataDir := t.TempDir()
	sdkguesttest.Install(t, dataDir, sdkguesttest.MarkerManifest("sdk-markers", source.URL))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{})
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.MarkerProvider("sdk-markers")
	if !ok {
		t.Fatal("the Set registered no Marker provider for sdk-markers")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, URL: registration.Descriptor.DefaultURL, URLEntered: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	resp, err := provider.Markers(context.Background(), pluginapi.MarkersRequest{
		Kind: "episode", Title: "Pilot", IDs: map[string]string{"imdb": "tt0959621"}, DurationMs: 2900000,
	})
	if err != nil {
		t.Fatalf("Markers: %v", err)
	}
	want := []pluginapi.MarkerCandidate{
		{Kind: pluginapi.MarkerIntro, StartMs: 10000, EndMs: 40000, DurationMs: 2900000},
		{Kind: pluginapi.MarkerCredits, StartMs: 2800000, EndMs: 2900000, DurationMs: 2900000},
	}
	if !reflect.DeepEqual(resp.Markers, want) {
		t.Fatalf("markers = %+v, want %+v", resp.Markers, want)
	}
}

// directoryTransport is an HTTP directory, without a network: POST /login with
// a JSON username and password answers the person or 401, and GET
// /users/<subject> answers the person, or 404 for one it no longer knows.
type directoryTransport struct{ handler http.Handler }

func (rt directoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// TestASDKBuiltGuestAnswersAsAPasswordSignInProvider: the guest checks a
// password against its directory and answers the person it names, rejects a
// wrong one as a rejection rather than a failure, and answers lookup(subject)
// as active with the groups now, or gone.
func TestASDKBuiltGuestAnswersAsAPasswordSignInProvider(t *testing.T) {
	plugins.Parallel(t)
	const directory = "http://203.0.113.7"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var creds struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&creds)
		if creds.Username != "ada" || creds.Password != "correct horse" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"u-1","name":"ada","groups":["crew"]}`))
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "u-1" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":"u-1","name":"ada","groups":["crew","admins"]}`))
	})

	dataDir := t.TempDir()
	sdkguesttest.Install(t, dataDir, sdkguesttest.SignInManifest("sdk-directory", directory))
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{HTTPClient: &http.Client{Transport: directoryTransport{mux}}})
	for _, p := range set.Plugins() {
		p.SetSettingValues(map[string]any{"directory": directory})
	}
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SignInProvider("sdk-directory")
	if !ok {
		t.Fatal("the Set registered no Sign-in provider for sdk-directory")
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true})
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}

	resp, err := provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "correct horse"})
	if err != nil {
		t.Fatalf("CheckPassword: %v", err)
	}
	want := pluginapi.SignInPasswordResponse{Accepted: true, Identity: &pluginapi.SignInIdentity{Subject: "u-1", Username: "ada", Groups: []string{"crew"}}}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("a right password = %+v (%+v), want %+v", resp, resp.Identity, want.Identity)
	}
	resp, err = provider.CheckPassword(context.Background(), pluginapi.SignInPasswordRequest{Username: "ada", Password: "wrong"})
	if err != nil || resp.Accepted {
		t.Fatalf("a wrong password = %+v, %v; want rejected with no error", resp, err)
	}

	lookup, ok := provider.(pluginapi.SignInLookupProvider)
	if !ok {
		t.Fatalf("the provider %T does not answer lookup(subject)", provider)
	}
	active, err := lookup.Lookup(context.Background(), pluginapi.SignInLookupRequest{Subject: "u-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	wantActive := pluginapi.SignInLookupResponse{Status: pluginapi.SignInActive,
		Identity: &pluginapi.SignInIdentity{Subject: "u-1", Username: "ada", Groups: []string{"crew", "admins"}}}
	if !reflect.DeepEqual(active, wantActive) {
		t.Fatalf("lookup u-1 = %+v (%+v), want %+v", active, active.Identity, wantActive.Identity)
	}
	gone, err := lookup.Lookup(context.Background(), pluginapi.SignInLookupRequest{Subject: "u-2"})
	if err != nil || gone.Status != pluginapi.SignInGone {
		t.Fatalf("lookup u-2 = %+v, %v; want gone", gone, err)
	}
}
