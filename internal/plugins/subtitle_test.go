package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The loader's half of the Subtitle provider Extension point
// (.scratch/plugin-system issue 12), driven by the same real module every other
// test in this package uses: compiled from source, instantiated in a wazero
// sandbox, called across the hand-rolled ABI.
//
// The end-to-end half — a viewer's "search online" answered by an Installed
// provider, cached, recorded as fetched and played — is a black-box test in
// internal/api/installed_subtitle_provider_test.go. What lives here is what only
// the loader can be asked: that the seam registers, that the settings reach the
// guest with the call and not before it, and that the byte cap is the HOST's.

// --- a source for the Plugin to wrap -----------------------------------------

// subtitleSource is the external service an Installed Subtitle provider talks to:
// a search that answers candidates and a download that answers bytes. It records
// what it was asked, because half of what this seam has to prove is that the
// language and the host-computed content hash reached the far side of the
// sandbox.
type subtitleSource struct {
	srv  *httptest.Server
	body []byte

	mu       sync.Mutex
	searches []sourceSearch
	keys     []string
	hits     int
}

// sourceSearch is the search request as the SOURCE saw it — deliberately decoded
// into its own shape rather than into pluginapi's, so this is what crossed the
// wire and not what we hoped did.
type sourceSearch struct {
	Ref struct {
		Title     string `json:"title"`
		Year      int    `json:"year"`
		IMDBID    string `json:"imdbId"`
		MovieHash string `json:"movieHash"`
		FileSize  int64  `json:"fileSize"`
	} `json:"ref"`
	Language string `json:"language"`
}

const sourceSRT = "1\n00:00:01,000 --> 00:00:03,000\nGuten Tag\n"

func newSubtitleSource(t *testing.T) *subtitleSource {
	t.Helper()
	s := &subtitleSource{body: []byte(sourceSRT)}
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		var got sourceSearch
		_ = json.NewDecoder(r.Body).Decode(&got)
		s.mu.Lock()
		s.searches = append(s.searches, got)
		s.keys = append(s.keys, r.Header.Get("Api-Key"))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"subtitles":[` +
			`{"id":"9001","language":"` + got.Language + `","format":"srt",` +
			`"release":"Dune.2021.1080p.BluRay","downloads":42}]}`))
	})
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		_, _ = w.Write(s.body)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *subtitleSource) lastSearch(t *testing.T) sourceSearch {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.searches) == 0 {
		t.Fatal("the source was never asked for candidates")
	}
	return s.searches[len(s.searches)-1]
}

func (s *subtitleSource) lastKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return ""
	}
	return s.keys[len(s.keys)-1]
}

// --- helpers -----------------------------------------------------------------

// loadWith is load with options of the caller's choosing, for the tests that are
// about a number the loader holds.
func loadWith(t *testing.T, dataDir string, log *logSink, opts plugins.Options) *plugins.Set {
	t.Helper()
	opts.Logf = log.logf
	if opts.CallTimeout == 0 {
		opts.CallTimeout = 2 * time.Second
	}
	set, err := plugins.Load(context.Background(), filepath.Join(dataDir, plugins.DirName), opts)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = set.Close(context.Background()) })
	return set
}

// providerFor registers the Set into a fresh Registry and builds the one Subtitle
// provider for id, exactly as subfetch.BuildProvider does from a settings row.
func subtitleProviderFor(t *testing.T, set *plugins.Set, id string, s pluginapi.Settings) pluginapi.SubtitleProvider {
	t.Helper()
	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SubtitleProvider(id)
	if !ok {
		t.Fatalf("the Set registered no Subtitle provider for %q", id)
	}
	provider, err := registration.New(s)
	if err != nil {
		t.Fatalf("building the provider: %v", err)
	}
	return provider
}

func germanSearch(hash string) pluginapi.SubtitleSearchRequest {
	return pluginapi.SubtitleSearchRequest{
		Ref: pluginapi.SubtitleRef{
			Title: "Dune", Year: 2021, IMDBID: "tt1160419",
			MovieHash: hash, FileSize: 12909756,
		},
		Language: "de",
	}
}

// --- the seam ----------------------------------------------------------------

// TestAnInstalledSubtitleProviderRegistersAndAnswersBothCalls is the loader-level
// tracer: a manifest declaring subtitle-provider reaches the registry as the same
// kind of value OpenSubtitles does, and both contract calls cross the sandbox.
func TestAnInstalledSubtitleProviderRegistersAndAnswersBothCalls(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("example-subs"))
	source := newSubtitleSource(t)
	log := &logSink{}
	set := load(t, dataDir, log)

	reg := pluginapi.NewRegistry()
	set.Register(reg)

	registration, ok := reg.SubtitleProvider("example-subs")
	if !ok {
		t.Fatalf("the Plugin is not registered as a Subtitle provider; log:\n%s", log.all())
	}
	d := registration.Descriptor
	switch {
	case d.Slug != "example-subs":
		t.Errorf("descriptor slug = %q, want the directory id", d.Slug)
	case d.Name != "Test Subtitles (example-subs)":
		t.Errorf("descriptor name = %q, want the manifest's", d.Name)
	case d.ExtensionPoint != pluginapi.ExtensionSubtitleProvider:
		t.Errorf("descriptor extension point = %q, want subtitle-provider", d.ExtensionPoint)
	case !d.RequiresKey:
		t.Error("the manifest requires a secret, but the descriptor does not say so")
	case d.DefaultURL != "https://subs.example.test/v1":
		t.Errorf("descriptor default URL = %q, want the manifest's", d.DefaultURL)
	case d.DocsURL == "" || d.Description == "":
		t.Errorf("the settings screen's copy did not survive the manifest: %+v", d)
	case !d.HasCapability(pluginapi.CapabilitySearch):
		t.Errorf("the declared search capability did not survive: %+v", d.Capabilities)
	}

	// The SAME module also exports deliver, and is not an Event sink here: the
	// manifest's provides list decides which seams a module fills, not its exports.
	if _, isSink := reg.EventSink("example-subs"); isSink {
		t.Error("a Plugin providing only subtitle-provider was registered as an Event sink too")
	}

	provider := subtitleProviderFor(t, set, "example-subs", pluginapi.Settings{
		Enabled: true, Secret: "sk-test", URL: source.srv.URL,
	})

	resp, err := provider.SearchSubtitles(context.Background(), germanSearch("8e245d9679d31e12"))
	if err != nil {
		t.Fatalf("SearchSubtitles: %v; log:\n%s", err, log.all())
	}
	if resp.Outcome != pluginapi.OutcomeMatched || len(resp.Candidates) != 1 {
		t.Fatalf("search answered %+v, want one matched candidate", resp)
	}
	c := resp.Candidates[0]
	if c.ID != "9001" || c.Language != "de" || c.Format != "srt" || c.Downloads != 42 {
		t.Errorf("candidate = %+v, want the source's 9001/de/srt", c)
	}
	// The guest reported WHICH signal produced the candidate, which is the one
	// judgment about a subtitle only the Plugin can make.
	if c.MatchedBy != "moviehash" {
		t.Errorf("matchedBy = %q, want moviehash — the host computed a hash and it travelled", c.MatchedBy)
	}

	// What the far side of the sandbox actually received.
	got := source.lastSearch(t)
	if got.Language != "de" {
		t.Errorf("the source was asked for language %q, want de", got.Language)
	}
	if got.Ref.MovieHash != "8e245d9679d31e12" || got.Ref.FileSize != 12909756 {
		t.Errorf("the content hash did not reach the source: %+v", got.Ref)
	}
	// The secret reached the guest WITH the call, and nowhere else: there is no
	// host function that would hand it one.
	if source.lastKey() != "sk-test" {
		t.Errorf("the source saw api key %q, want the secret the Admin saved", source.lastKey())
	}

	dl, err := provider.DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: c, MaxBytes: 8 << 20,
	})
	if err != nil {
		t.Fatalf("DownloadSubtitle: %v; log:\n%s", err, log.all())
	}
	if dl.Outcome != pluginapi.OutcomeMatched || string(dl.Data) != sourceSRT {
		t.Fatalf("download answered %+v, want the source's bytes whole", dl)
	}
	if dl.Format != "srt" {
		t.Errorf("download format = %q, want srt", dl.Format)
	}
}

// TestAGuestSearchingWithNoHashSaysSoIsQuery: the match signal is reported from
// what the request carried, so an un-enriched Title with an unreadable file is
// still searched — by query — rather than not at all (ADR-0021's match order).
func TestAGuestSearchingWithNoHashSaysSoIsQuery(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("example-subs"))
	source := newSubtitleSource(t)
	set := load(t, dataDir, &logSink{})

	provider := subtitleProviderFor(t, set, "example-subs", pluginapi.Settings{
		Enabled: true, Secret: "sk-test", URL: source.srv.URL,
	})
	req := pluginapi.SubtitleSearchRequest{
		Ref:      pluginapi.SubtitleRef{Title: "Some Film", Year: 1999},
		Language: "en",
	}
	resp, err := provider.SearchSubtitles(context.Background(), req)
	if err != nil {
		t.Fatalf("SearchSubtitles: %v", err)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].MatchedBy != "query" {
		t.Fatalf("search answered %+v, want one candidate matched by query", resp.Candidates)
	}
}

// --- the byte cap ------------------------------------------------------------

// TestAnOversizeDownloadIsRefusedWholeAndRecorded is the acceptance criterion the
// contract makes the host responsible for: the cap is the CALLER's, so a guest
// that answers with more than it gets its answer discarded — not trimmed to fit —
// and the refusal is recorded where an Admin reads it.
func TestAnOversizeDownloadIsRefusedWholeAndRecorded(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("greedy-subs"))
	source := newSubtitleSource(t)
	log := &logSink{}
	// A small cap, so the guest's oversize answer is small too. The mechanism is
	// the production one; only the number is smaller.
	set := loadWith(t, dataDir, log, plugins.Options{MaxFetchBytes: 4096})

	// The secret carries the mode marker: the one string a test can set that
	// reaches a provider guest.
	provider := subtitleProviderFor(t, set, "greedy-subs", pluginapi.Settings{
		Enabled: true, Secret: "sk-test;obelo-mode=oversize", URL: source.srv.URL,
	})

	resp, err := provider.DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
		Candidate: pluginapi.SubtitleCandidate{ID: "9001", Format: "srt"},
		MaxBytes:  8 << 20, // the subtitle domain's cap, wider than the loader's
	})
	if err == nil {
		t.Fatalf("an oversize download was accepted, answering %d bytes", len(resp.Data))
	}
	// Discarded WHOLE. A truncated subtitle is one the host would cache and a
	// viewer would play as though it were complete.
	if len(resp.Data) != 0 {
		t.Fatalf("the refused response still carried %d bytes; it must be discarded, not trimmed", len(resp.Data))
	}
	if !strings.Contains(err.Error(), "4096") || !strings.Contains(err.Error(), "greedy-subs") {
		t.Errorf("the error does not name the plugin and the cap: %v", err)
	}
	// Recorded where the settings screen reads it.
	status, ok := set.Status("greedy-subs")
	if !ok || status.LastError == "" {
		t.Fatalf("the refusal was not recorded on the Plugin: %+v (known=%v)", status, ok)
	}
	if status.Disabled {
		t.Errorf("one refusal disabled the Plugin; it takes a run of them: %+v", status)
	}
	if source.hits != 0 {
		t.Errorf("the guest reached the source %d times for bytes it invented", source.hits)
	}
}

// TestAPluginThatOnlyEverAnswersOversizeIsDisabled: a refusal is counted as the
// failure it is, so a Plugin that does it every time stops being called — the
// same accounting a trapping or hanging one gets.
func TestAPluginThatOnlyEverAnswersOversizeIsDisabled(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("greedy-subs"))
	source := newSubtitleSource(t)
	set := loadWith(t, dataDir, &logSink{}, plugins.Options{MaxFetchBytes: 4096})

	provider := subtitleProviderFor(t, set, "greedy-subs", pluginapi.Settings{
		Enabled: true, Secret: "obelo-mode=oversize", URL: source.srv.URL,
	})
	req := pluginapi.SubtitleDownloadRequest{Candidate: pluginapi.SubtitleCandidate{ID: "9001"}}
	for i := 0; i < plugins.DefaultFailureThreshold; i++ {
		if _, err := provider.DownloadSubtitle(context.Background(), req); err == nil {
			t.Fatalf("download %d was accepted", i+1)
		}
	}
	status, _ := set.Status("greedy-subs")
	if !status.Disabled {
		t.Fatalf("after %d refused downloads the Plugin is still live: %+v", plugins.DefaultFailureThreshold, status)
	}
	// And a disabled Plugin is not called at all — the search says so by sentinel
	// rather than by an empty answer that looks like "nothing found".
	_, err := provider.SearchSubtitles(context.Background(), germanSearch(""))
	if !errors.Is(err, plugins.ErrDisabled) {
		t.Fatalf("a disabled Plugin's search answered %v, want ErrDisabled", err)
	}
}

// --- refusals ----------------------------------------------------------------

// TestARefusedSubtitlePluginIsStillListedAndItsFactoryRefuses: a Plugin an
// operator placed appears on the subtitle-providers screen even when this server
// will not run it, and enabling it says why rather than searching silently.
func TestARefusedSubtitlePluginIsStillListedAndItsFactoryRefuses(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.InstallModule(t, dataDir, plugintest.SubtitleManifest("broken-subs"),
		[]byte("this is not a WebAssembly module"))
	log := &logSink{}
	set := load(t, dataDir, log)

	reg := pluginapi.NewRegistry()
	set.Register(reg)
	registration, ok := reg.SubtitleProvider("broken-subs")
	if !ok {
		t.Fatalf("a Plugin that would not compile vanished from the screen entirely; log:\n%s", log.all())
	}
	provider, err := registration.New(pluginapi.Settings{Enabled: true, Secret: "k"})
	if err == nil {
		t.Fatalf("the factory of a refused Plugin built %T", provider)
	}
	if !strings.Contains(err.Error(), "broken-subs") {
		t.Errorf("the refusal does not name the Plugin: %v", err)
	}
	if status, _ := set.Status("broken-subs"); !status.Disabled || status.LastError == "" {
		t.Errorf("the screen shows %+v, want disabled with a reason", status)
	}
}

// TestASubtitlePluginCannotShadowAnAlreadyRegisteredProvider: the slug is the
// settings key, so a Plugin claiming one a Built-in already holds would move an
// Admin's API key onto code the maintainer did not write.
func TestASubtitlePluginCannotShadowAnAlreadyRegisteredProvider(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.SubtitleManifest("opensubtitles"))
	log := &logSink{}
	set := load(t, dataDir, log)

	reg := pluginapi.NewRegistry()
	// The Built-in goes first, from the composition root, exactly as app.New has it.
	reg.RegisterSubtitleProvider(pluginapi.SubtitleProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "opensubtitles", Name: "OpenSubtitles"},
		New: func(pluginapi.Settings) (pluginapi.SubtitleProvider, error) {
			return nil, errors.New("the Built-in, which must survive this")
		},
	})
	set.Register(reg)

	registration, ok := reg.SubtitleProvider("opensubtitles")
	if !ok {
		t.Fatal("the Built-in registration disappeared")
	}
	if registration.Descriptor.Name != "OpenSubtitles" {
		t.Fatalf("the Plugin shadowed the Built-in: %+v", registration.Descriptor)
	}
	if status, _ := set.Status("opensubtitles"); !status.Disabled {
		t.Errorf("the shadowing Plugin is not recorded as refused: %+v", status)
	}
	if !log.contains(t, "already claimed") {
		t.Errorf("no log line says why the Plugin did nothing:\n%s", log.all())
	}
}

// --- what a subtitle manifest may raise (.scratch/bundled-plugins issue 09) ---

// TestASubtitleManifestRaisesItsBudgetAndItsByteCap: callBudgetMillis and
// maxFetchBytes on a subtitle-provider entry are honoured, as they are on a
// metadata-provider entry. The bundled OpenSubtitles asks for both, because the
// Built-in it replaced took an 8 MiB subtitle and waited out the request's own
// 30 s, where the seam's defaults are 1 MiB and 10 s.
func TestASubtitleManifestRaisesItsBudgetAndItsByteCap(t *testing.T) {
	t.Run("the budget", func(t *testing.T) {
		dataDir := t.TempDir()
		m := plugintest.SubtitleManifest("patient-subs")
		m.Provides[0].CallBudgetMillis = 700
		plugintest.Install(t, dataDir, m)
		// A default budget far past the test's patience: if the manifest's 700 ms
		// were not read, the spinning guest would run for a minute.
		set := loadWith(t, dataDir, &logSink{}, plugins.Options{CallTimeout: time.Minute})
		provider := subtitleProviderFor(t, set, "patient-subs", pluginapi.Settings{
			Enabled: true, Secret: "obelo-mode=spin", URL: "http://unused.example.test",
		})

		started := time.Now()
		if _, err := provider.SearchSubtitles(context.Background(), germanSearch("")); err == nil {
			t.Fatal("the spinning guest answered")
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("the guest ran for %s; the manifest asked for 700 ms", elapsed)
		}
	})

	t.Run("the default budget is the seam's own when the manifest says nothing", func(t *testing.T) {
		dataDir := t.TempDir()
		plugintest.Install(t, dataDir, plugintest.SubtitleManifest("plain-subs"))
		set := loadWith(t, dataDir, &logSink{}, plugins.Options{CallTimeout: 500 * time.Millisecond})
		provider := subtitleProviderFor(t, set, "plain-subs", pluginapi.Settings{
			Enabled: true, Secret: "obelo-mode=spin", URL: "http://unused.example.test",
		})

		started := time.Now()
		if _, err := provider.SearchSubtitles(context.Background(), germanSearch("")); err == nil {
			t.Fatal("the spinning guest answered")
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Errorf("the guest ran for %s; the seam's default is 500 ms here", elapsed)
		}
	})

	t.Run("the byte cap", func(t *testing.T) {
		dataDir := t.TempDir()
		m := plugintest.SubtitleManifest("big-subs")
		m.Provides[0].MaxFetchBytes = 8192
		plugintest.Install(t, dataDir, m)
		set := loadWith(t, dataDir, &logSink{}, plugins.Options{MaxFetchBytes: 4096})
		provider := subtitleProviderFor(t, set, "big-subs", pluginapi.Settings{
			Enabled: true, Secret: "obelo-mode=oversize", URL: "http://unused.example.test",
		})

		// The oversize guest answers one byte past the cap it was TOLD, so the
		// number in the refusal is the number the host resolved.
		_, err := provider.DownloadSubtitle(context.Background(), pluginapi.SubtitleDownloadRequest{
			Candidate: pluginapi.SubtitleCandidate{ID: "9001"}, MaxBytes: 8 << 20,
		})
		if err == nil || !strings.Contains(err.Error(), "more than the 8192") {
			t.Fatalf("err = %v, want a refusal at the manifest's 8192, not the default 4096", err)
		}
	})
}
