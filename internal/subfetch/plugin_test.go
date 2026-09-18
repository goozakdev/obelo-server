package subfetch

import (
	"context"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The contract edge. Two things are worth a unit test here, and they are the two
// things that would silently change behaviour if they were wrong: that every
// Outcome the contract defines maps to the sentinel the Service already matches
// on, and that the adapter carries a candidate's fields and the host's size cap
// across intact.

// fakePlugin is a canned contract-level Subtitle provider: it answers with the
// Outcome and payload a test sets, recording what it was asked. It is deliberately
// NOT the OpenSubtitles Built-in — the subtitle domain must be testable without
// knowing which Plugins exist.
type fakePlugin struct {
	searchResp   pluginapi.SubtitleSearchResponse
	searchErr    error
	downloadResp pluginapi.SubtitleDownloadResponse
	downloadErr  error

	lastSearch   pluginapi.SubtitleSearchRequest
	lastDownload pluginapi.SubtitleDownloadRequest
}

func (f *fakePlugin) SearchSubtitles(_ context.Context, req pluginapi.SubtitleSearchRequest) (pluginapi.SubtitleSearchResponse, error) {
	f.lastSearch = req
	return f.searchResp, f.searchErr
}

func (f *fakePlugin) DownloadSubtitle(_ context.Context, req pluginapi.SubtitleDownloadRequest) (pluginapi.SubtitleDownloadResponse, error) {
	f.lastDownload = req
	return f.downloadResp, f.downloadErr
}

// testRegistry registers the fake Plugin under a slug, mirroring what the builtins
// package does for OpenSubtitles from the composition root.
func testRegistry(t *testing.T, slug string, plugin pluginapi.SubtitleProvider, requiresKey bool) *pluginapi.Registry {
	t.Helper()
	reg := pluginapi.NewRegistry()
	reg.RegisterSubtitleProvider(pluginapi.SubtitleProviderRegistration{
		Descriptor: pluginapi.Descriptor{
			Slug:        slug,
			Name:        "Fake Source",
			RequiresKey: requiresKey,
			DefaultURL:  "https://fake.test/api",
		},
		New: func(pluginapi.Settings) (pluginapi.SubtitleProvider, error) { return plugin, nil },
	})
	return reg
}

// TestOutcomeMapsToTheDomainSentinel: the whole point of an Outcome enum at the
// edge is that the Service, the handlers and every existing test keep matching on
// the sentinels they always did (ADR-0057 decision 2). The table ranges over
// pluginapi.AllOutcomes(), so a new Outcome cannot be added to the contract
// without this mapping being made on purpose.
func TestOutcomeMapsToTheDomainSentinel(t *testing.T) {
	want := map[pluginapi.Outcome]error{
		pluginapi.OutcomeMatched: nil,
		// The normal "nothing for this release in this language".
		pluginapi.OutcomeNoMatch: ErrNoMatch,
		// Acceptance is the host's judgment and the subtitle domain applies none, so
		// a declined top hit is indistinguishable from having nothing.
		pluginapi.OutcomeRejected: ErrNoMatch,
		// "No source is answering" — what the nil-object provider reports, which
		// degrades to an empty candidate list rather than an error (ADR-0001).
		pluginapi.OutcomeUnavailable: ErrProviderDisabled,
		// The ref-* outcomes belong to the Metadata provider extension point. A
		// Subtitle provider returning one is a Plugin bug, never "nothing found".
		pluginapi.OutcomeRefInvalid:         errOutcomeNotInPoint,
		pluginapi.OutcomeRefKindMismatch:    errOutcomeNotInPoint,
		pluginapi.OutcomeRefUnsupportedKind: errOutcomeNotInPoint,
	}

	for _, outcome := range pluginapi.AllOutcomes() {
		expected, mapped := want[outcome]
		if !mapped {
			t.Fatalf("outcome %q has no decided mapping — the contract grew and the adapter did not", outcome)
		}
		got := outcomeError(outcome)
		switch {
		case expected == nil && got != nil:
			t.Errorf("%q mapped to %v, want no error", outcome, got)
		case expected != nil && !errors.Is(got, expected):
			t.Errorf("%q mapped to %v, want %v", outcome, got, expected)
		}
	}

	// A value this build has never heard of is a bug, not a quiet no-match — an
	// older server must never read a newer Plugin's answer as "nothing found".
	if err := outcomeError(pluginapi.Outcome("invented-later")); !errors.Is(err, errOutcomeNotInPoint) {
		t.Errorf("unknown outcome mapped to %v, want it to be refused", err)
	}
	if err := outcomeError(""); err == nil {
		t.Error("an unset outcome mapped to success")
	}
}

// TestAdapterCarriesTheSearchAcrossTheContract: the ref, the language and every
// candidate field survive the round trip, and an empty candidate list reads as a
// no-match however the Plugin labelled it.
func TestAdapterCarriesTheSearchAcrossTheContract(t *testing.T) {
	plugin := &fakePlugin{searchResp: pluginapi.SubtitleSearchResponse{
		Outcome: pluginapi.OutcomeMatched,
		Candidates: []pluginapi.SubtitleCandidate{{
			ID: "42", Language: "de", Format: "srt", Release: "Dune.2021.1080p",
			HearingImpaired: true, Forced: true, MatchedBy: "moviehash", Downloads: 7,
		}},
	}}
	provider := ProviderFromPlugin(plugin)

	ref := SubtitleRef{Title: "Dune", Year: 2021, IMDBID: "tt1160419", MovieHash: "8e24", FileSize: 99}
	cands, err := provider.Search(context.Background(), ref, "de")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if plugin.lastSearch.Language != "de" || plugin.lastSearch.Ref.MovieHash != "8e24" ||
		plugin.lastSearch.Ref.IMDBID != "tt1160419" || plugin.lastSearch.Ref.FileSize != 99 {
		t.Fatalf("the match ref did not cross intact: %+v", plugin.lastSearch)
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	got := cands[0]
	want := Candidate{
		ID: "42", Language: "de", Format: "srt", Release: "Dune.2021.1080p",
		HearingImpaired: true, Forced: true, MatchedBy: "moviehash", Downloads: 7,
	}
	if got != want {
		t.Fatalf("candidate = %+v, want %+v", got, want)
	}

	// Matched with nothing in it means the same thing a no-match does.
	plugin.searchResp = pluginapi.SubtitleSearchResponse{Outcome: pluginapi.OutcomeMatched}
	if _, err := provider.Search(context.Background(), ref, "de"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("empty match = %v, want ErrNoMatch", err)
	}

	// A transport failure is passed through untouched — it is transient, and the
	// host retries it rather than settling the item (ADR-0048).
	boom := errors.New("dial tcp: refused")
	plugin.searchErr = boom
	if _, err := provider.Search(context.Background(), ref, "de"); !errors.Is(err, boom) {
		t.Fatalf("transport failure = %v, want it passed through", err)
	}
}

// TestAdapterCapsTheDownload: the contract says the CALLER caps byte payloads, so
// the host states the cap and re-checks the answer rather than trusting the
// Plugin's good manners.
func TestAdapterCapsTheDownload(t *testing.T) {
	plugin := &fakePlugin{downloadResp: pluginapi.SubtitleDownloadResponse{
		Outcome: pluginapi.OutcomeMatched, Data: []byte("WEBVTT\n"), Format: "vtt",
	}}
	provider := ProviderFromPlugin(plugin)

	data, format, err := provider.Download(context.Background(), Candidate{ID: "42", Format: "srt"})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(data) != "WEBVTT\n" || format != "vtt" {
		t.Fatalf("got %q/%q, want the response's bytes and its format", data, format)
	}
	if plugin.lastDownload.MaxBytes != maxSubtitleBytes {
		t.Fatalf("MaxBytes = %d, want the host's cap %d", plugin.lastDownload.MaxBytes, maxSubtitleBytes)
	}
	if plugin.lastDownload.Candidate.ID != "42" {
		t.Fatalf("the candidate did not cross intact: %+v", plugin.lastDownload.Candidate)
	}

	// A Plugin that ignores the cap is refused, not written to the subtitle cache.
	plugin.downloadResp.Data = make([]byte, maxSubtitleBytes+1)
	if _, _, err := provider.Download(context.Background(), Candidate{ID: "42"}); err == nil {
		t.Fatal("an over-cap payload was accepted")
	}

	// A candidate that vanished between the search and the pick is a no-match.
	plugin.downloadResp = pluginapi.SubtitleDownloadResponse{Outcome: pluginapi.OutcomeNoMatch}
	if _, _, err := provider.Download(context.Background(), Candidate{ID: "42"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("vanished candidate = %v, want ErrNoMatch", err)
	}
}

// TestBuildProviderUsesTheRegistryValue: the builder has no opinion about which
// Plugins exist — it reads the registry it was handed (ADR-0057 decision 5), so a
// settings row whose slug no Plugin claims simply never builds anything.
func TestBuildProviderUsesTheRegistryValue(t *testing.T) {
	plugin := &fakePlugin{}
	reg := testRegistry(t, "fake", plugin, true)
	rows := []store.SubtitleProviderRow{{Slug: "fake", Enabled: true, APIKey: "k"}}

	if _, ok := BuildProvider(reg, rows).(pluginProvider); !ok {
		t.Fatal("an enabled, keyed Plugin should build through the contract adapter")
	}
	// A row for a Plugin this build does not have makes no calls at all.
	other := []store.SubtitleProviderRow{{Slug: "not-registered", Enabled: true, APIKey: "k"}}
	if _, ok := BuildProvider(reg, other).(disabledProvider); !ok {
		t.Fatal("an unregistered slug should yield the disabled provider")
	}
	// An empty registry is a server with no Subtitle provider Plugins.
	if _, ok := BuildProvider(pluginapi.NewRegistry(), rows).(disabledProvider); !ok {
		t.Fatal("an empty registry should yield the disabled provider")
	}
}

// TestBuildProviderResolvesSettings: the fixed Settings shape is what the factory
// receives — the key as the secret, the operator's override or the registration's
// default as the URL.
func TestBuildProviderResolvesSettings(t *testing.T) {
	var got pluginapi.Settings
	reg := pluginapi.NewRegistry()
	reg.RegisterSubtitleProvider(pluginapi.SubtitleProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "fake", RequiresKey: true, DefaultURL: "https://fake.test/api"},
		New: func(s pluginapi.Settings) (pluginapi.SubtitleProvider, error) {
			got = s
			return &fakePlugin{}, nil
		},
	})

	BuildProvider(reg, []store.SubtitleProviderRow{{Slug: "fake", Enabled: true, APIKey: "k"}})
	if !got.Enabled || got.Secret != "k" || got.URL != "https://fake.test/api" {
		t.Fatalf("settings = %+v, want the key and the registration's default URL", got)
	}

	BuildProvider(reg, []store.SubtitleProviderRow{{Slug: "fake", Enabled: true, APIKey: "k", BaseURL: "https://mirror.test"}})
	if got.URL != "https://mirror.test" {
		t.Fatalf("settings URL = %q, want the operator's override", got.URL)
	}
}
