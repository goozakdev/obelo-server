package enrich

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// "Unavailable" means two different things, and which one it means depends on
// WHICH CALL said it (.scratch/bundled-plugins: issue 04 follow-up).
//
// The Built-in this replaced classified its own failures with enrich.ErrTransient,
// and the pass read that to decide between retrying the item and parking it
// (ADR-0048). A guest cannot wrap a server sentinel, so the same fact now crosses
// the contract as OutcomeUnavailable on a LOOKUP — and if the host reads that as
// the picker's "not now", every slow afternoon settles a library's worth of
// matchable Titles as 'failed'. ADR-0059 decision 6 says the opposite in so many
// words: the item takes the backoff and no failure is counted.

// A lookup that answers unavailable is BOTH: the sentinel every existing caller
// matches on, and transient, which is what recordLeafFailure and
// recordParentFailure read.
func TestAnUnavailableLookupIsTransient(t *testing.T) {
	plugin := &fakePlugin{lookupResp: pluginapi.LookupResponse{
		Outcome: pluginapi.OutcomeUnavailable,
		Detail:  "https://api.example.test/search/movie answered 503",
	}}
	p := ProviderFromPlugin(fullyCapable(), plugin)

	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune", Year: 2021})
	if err == nil {
		t.Fatal("an unavailable lookup answered no error at all")
	}
	if !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("err = %v, want it to satisfy ErrSearchUnavailable — every caller that tells "+
			"this apart from a no-match matches on that sentinel", err)
	}
	if !IsTransient(err) {
		t.Fatalf("err = %v is not transient, so recordLeafFailure parks the Title as 'failed' "+
			"— a source having a bad afternoon would settle a library (ADR-0048, ADR-0059 d6)", err)
	}
	if errors.Is(err, ErrNoMatch) {
		t.Error("an unavailable lookup reads as a no-match, which is a claim about the item")
	}
	// The Plugin's own sentence survives, because it is what an operator reads in
	// the log line the retry writes.
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %q, want the Plugin's Detail in it", err.Error())
	}
}

// A Plugin that says nothing about why still produces a usable error rather than a
// sentence with a dangling colon.
func TestAnUnavailableLookupWithNoDetail(t *testing.T) {
	plugin := &fakePlugin{lookupResp: pluginapi.LookupResponse{Outcome: pluginapi.OutcomeUnavailable}}
	p := ProviderFromPlugin(fullyCapable(), plugin)

	_, err := p.Lookup(context.Background(), TitleRef{Kind: "movie", Title: "Dune"})
	if !IsTransient(err) || !errors.Is(err, ErrSearchUnavailable) {
		t.Fatalf("err = %v, want transient and ErrSearchUnavailable", err)
	}
	if strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("err = %q ends in a dangling separator", err.Error())
	}
}

// EVERY OTHER CALL IS UNCHANGED, and that is the other half of the decision.
// Search, the artwork picker, the episode chooser and a pasted reference are
// surfaces a human is looking at: "unavailable" there is a fact about the source's
// CATALOGUE — no listable set for this kind — with nothing to retry and nobody
// waiting. Marking those transient would put the picker's shrug into the retry
// scheduler.
func TestOnlyTheLookupReadsUnavailableAsTransient(t *testing.T) {
	plugin := &fakePlugin{
		searchResp:  pluginapi.SearchResponse{Outcome: pluginapi.OutcomeUnavailable},
		artworkResp: pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeUnavailable},
		seasonsResp: pluginapi.SeriesSeasonsResponse{Outcome: pluginapi.OutcomeUnavailable},
		epsResp:     pluginapi.SeasonEpisodesResponse{Outcome: pluginapi.OutcomeUnavailable},
	}
	p := ProviderFromPlugin(fullyCapable(), plugin)
	lister, ok := p.(EpisodeLister)
	if !ok {
		t.Fatal("the adapted Plugin does not implement EpisodeLister")
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"search", func() error {
			_, err := p.Search(context.Background(), "movie", "Dune", SearchOptions{})
			return err
		}},
		{"artwork candidates", func() error {
			_, err := p.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie", TMDBID: "1"}, "poster")
			return err
		}},
		{"series seasons", func() error {
			_, err := lister.SeriesSeasons(context.Background(), "1399")
			return err
		}},
		{"season episodes", func() error {
			_, err := lister.SeasonEpisodes(context.Background(), "1399", 1)
			return err
		}},
	} {
		err := tc.call()
		if !errors.Is(err, ErrSearchUnavailable) {
			t.Errorf("%s: err = %v, want ErrSearchUnavailable", tc.name, err)
			continue
		}
		if IsTransient(err) {
			t.Errorf("%s: err is transient — the picker's \"not now\" has been put into the "+
				"retry scheduler, where nothing is waiting for it", tc.name)
		}
	}
}

// And the mapping the rest of the adapter uses is untouched: outcomeError is still
// the total, undecorated map every other caller reads.
func TestOutcomeErrorItselfIsNotTransient(t *testing.T) {
	if IsTransient(outcomeError(pluginapi.OutcomeUnavailable)) {
		t.Fatal("outcomeError marks unavailable transient; the exception belongs to " +
			"pluginProvider.Lookup alone")
	}
}

// NO BUILT-IN ANSWERS UNAVAILABLE TO A LOOKUP, which is why this change cannot
// alter what any compiled-in source does. A Built-in reaches the same adapter
// through builtinPlugin, so an ErrSearchUnavailable returned from ITS Lookup would
// round-trip into the new transient error; none does, and this asks every shipped
// registration directly rather than taking that on trust.
func TestNoBuiltInAnswersUnavailableToALookup(t *testing.T) {
	for _, reg := range MetadataPlugins() {
		if reg.New == nil {
			continue // Cover Art Archive: registered for the screen, never built
		}
		plugin, err := reg.New(pluginapi.Settings{Enabled: true, Secret: "k", URL: "https://unused.test"})
		if err != nil || plugin == nil {
			continue
		}
		for _, kind := range []string{"movie", "show", "season", "episode", "artist", "album", "track"} {
			// A ref with no ids and no title: every source answers this WITHOUT a
			// network call, which is what makes it safe to ask all of them here.
			resp, err := plugin.Lookup(context.Background(), pluginapi.LookupRequest{
				Ref: pluginapi.MediaRef{Kind: kind},
			})
			if err != nil {
				continue // a transport failure is not an outcome
			}
			if resp.Outcome == pluginapi.OutcomeUnavailable {
				t.Errorf("the Built-in %q answers unavailable to a %s lookup; that is now a "+
					"TRANSIENT error and this item would be retried instead of parked",
					reg.Descriptor.Slug, kind)
			}
		}
	}
}
