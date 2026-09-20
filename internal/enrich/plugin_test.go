package enrich

import (
	"context"
	"errors"
	"reflect"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The contract edge of the enrichment domain. Three things are worth a unit test
// here, and they are the three that would silently change behavior if they were
// wrong: that every Outcome the contract defines maps to the sentinel the Service
// already matches on, that an UNDECLARED capability answers "unavailable" without
// the Plugin being called at all, and that a record survives the round trip
// through the wire shape with nothing lost.

// fakePlugin is a canned contract-level Metadata provider: it answers with the
// Outcome and payload a test sets, recording what it was asked and how often. It
// is deliberately NOT one of the Built-ins — the adapter must be testable without
// knowing which Plugins exist.
type fakePlugin struct {
	lookupResp  pluginapi.LookupResponse
	lookupErr   error
	searchResp  pluginapi.SearchResponse
	artworkResp pluginapi.ArtworkCandidatesResponse
	seasonsResp pluginapi.SeriesSeasonsResponse
	epsResp     pluginapi.SeasonEpisodesResponse

	lastLookup  pluginapi.LookupRequest
	lastSearch  pluginapi.SearchRequest
	lastArtwork pluginapi.ArtworkCandidatesRequest
	calls       int
}

func (f *fakePlugin) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	f.calls++
	f.lastLookup = req
	return f.lookupResp, f.lookupErr
}

func (f *fakePlugin) Search(_ context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	f.calls++
	f.lastSearch = req
	return f.searchResp, nil
}

func (f *fakePlugin) ArtworkCandidates(_ context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	f.calls++
	f.lastArtwork = req
	return f.artworkResp, nil
}

func (f *fakePlugin) SeriesSeasons(_ context.Context, _ pluginapi.SeriesSeasonsRequest) (pluginapi.SeriesSeasonsResponse, error) {
	f.calls++
	return f.seasonsResp, nil
}

func (f *fakePlugin) SeasonEpisodes(_ context.Context, _ pluginapi.SeasonEpisodesRequest) (pluginapi.SeasonEpisodesResponse, error) {
	f.calls++
	return f.epsResp, nil
}

// fullyCapable is a Descriptor that declares everything, for the tests that are
// about the payload rather than about gating.
func fullyCapable() pluginapi.Descriptor {
	return pluginapi.Descriptor{
		Slug:  "fake",
		Name:  "Fake Source",
		Kinds: []string{KindVideo},
		Capabilities: []pluginapi.Capability{
			pluginapi.CapabilitySearch,
			pluginapi.CapabilityArtworkCandidates,
			pluginapi.CapabilityEpisodeList,
		},
	}
}

// TestOutcomeMapsToTheDomainSentinel: the whole point of an Outcome enum at the
// edge is that the Service, the retry scheduler, the settled-reason writer and
// every existing test keep matching on the sentinels they always did (ADR-0057
// decision 2). The table ranges over pluginapi.AllOutcomes(), so a new Outcome
// cannot be added to the contract without this mapping being made on purpose.
func TestOutcomeMapsToTheDomainSentinel(t *testing.T) {
	want := map[pluginapi.Outcome]error{
		pluginapi.OutcomeMatched: nil,
		// The normal "this source has no record for this Title".
		pluginapi.OutcomeNoMatch: ErrNoMatch,
		// "The source answered and its answer is not this item". No Built-in produces
		// it — acceptance is the host's judgment — but the sentinel is the one the
		// `search-rejected` diagnosis is written from, so it has to survive the edge.
		pluginapi.OutcomeRejected: ErrMatchRejected,
		// "This kind cannot be searched right now" — what an undeclared capability
		// answers too, and what the Edit-item box reports instead of an empty list.
		pluginapi.OutcomeUnavailable: ErrSearchUnavailable,
		// The pasted-ref outcomes, kept distinct because the Admin needs three
		// different sentences: "that is not a link I read", "that is a link to the
		// wrong kind of thing", "that is a link to a thing we do not enrich".
		pluginapi.OutcomeRefInvalid:         ErrExternalRefInvalid,
		pluginapi.OutcomeRefKindMismatch:    ErrExternalRefKindMismatch,
		pluginapi.OutcomeRefUnsupportedKind: ErrExternalRefUnsupportedKind,
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

	// A rejection is still a no-match to every caller that only cares that the item
	// did not settle with a record — that wrapping is what left the retry and
	// Needs-Fixing paths untouched when acceptance moved into the host.
	if !errors.Is(outcomeError(pluginapi.OutcomeRejected), ErrNoMatch) {
		t.Error("a rejected outcome stopped being a no-match; every errors.Is(ErrNoMatch) caller just changed")
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

// TestSentinelMapsToTheOutcomeAndBack: the Plugin side of the same edge. Every
// sentinel a source in this package may return becomes the Outcome that maps back
// to it, and — the case that matters most — a TRANSPORT failure is NOT an outcome:
// it travels as a Go error so the host retries rather than settling the item
// (ADR-0048). Squashing an outage into an outcome would turn every blip into a
// permanent "no match".
func TestSentinelMapsToTheOutcomeAndBack(t *testing.T) {
	for _, sentinel := range []error{
		ErrNoMatch, ErrMatchRejected, ErrSearchUnavailable,
		ErrExternalRefInvalid, ErrExternalRefKindMismatch, ErrExternalRefUnsupportedKind,
	} {
		outcome, ok := errorOutcome(sentinel)
		if !ok {
			t.Fatalf("%v is not recognized as a domain outcome", sentinel)
		}
		if back := outcomeError(outcome); !errors.Is(back, sentinel) {
			t.Errorf("%v → %q → %v, want the same sentinel back", sentinel, outcome, back)
		}
	}
	if outcome, ok := errorOutcome(nil); !ok || outcome != pluginapi.OutcomeMatched {
		t.Errorf("a nil error = %q/%v, want matched", outcome, ok)
	}
	if _, ok := errorOutcome(errors.New("tmdb: status 503")); ok {
		t.Error("a transport failure was reported as a domain outcome; the host would settle the item instead of retrying it")
	}
	// ExternalRefKindMismatchError carries the got/want kinds and still matches its
	// sentinel, so the specific message survives being produced beside the edge.
	if outcome, ok := errorOutcome(&ExternalRefKindMismatchError{Got: "artist", Want: "track"}); !ok || outcome != pluginapi.OutcomeRefKindMismatch {
		t.Errorf("kind-mismatch error = %q/%v, want ref-kind-mismatch", outcome, ok)
	}
}

// TestAnUndeclaredCapabilityIsUnavailableWithoutACall: capabilities are consulted,
// not guessed (ADR-0057 decision 3). A Plugin that declared nothing optional is
// never asked, and what the Admin sees is the same "unavailable" the Edit-item box
// and the artwork picker already render for a kind with no searchable source.
func TestAnUndeclaredCapabilityIsUnavailableWithoutACall(t *testing.T) {
	plugin := &fakePlugin{}
	// A Descriptor with NO capabilities: lookup is mandatory, everything else is not.
	provider := ProviderFromPlugin(pluginapi.Descriptor{Slug: "fake", Kinds: []string{KindVideo}}, plugin)

	if _, err := provider.Search(context.Background(), "movie", "Dune", SearchOptions{}); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("search on a Plugin without the capability = %v, want ErrSearchUnavailable", err)
	}
	if _, err := provider.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie"}, "poster"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("artwork candidates without the capability = %v, want ErrSearchUnavailable", err)
	}
	lister, ok := provider.(EpisodeLister)
	if !ok {
		t.Fatal("an adapted Plugin must always satisfy EpisodeLister, so the chains' assertions still hold")
	}
	if _, err := lister.SeriesSeasons(context.Background(), "1396"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("series seasons without the capability = %v, want ErrSearchUnavailable", err)
	}
	if _, err := lister.SeasonEpisodes(context.Background(), "1396", 2); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("season episodes without the capability = %v, want ErrSearchUnavailable", err)
	}
	if plugin.calls != 0 {
		t.Errorf("the Plugin was called %d times for operations it never declared; an undeclared capability must cost no call", plugin.calls)
	}

	// Lookup is not gated: it is what a Metadata provider IS.
	plugin.lookupResp = pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: pluginapi.MetadataRecord{Matched: true}}
	if _, err := provider.Lookup(context.Background(), TitleRef{Kind: "movie"}); err != nil {
		t.Errorf("lookup: %v", err)
	}
	if plugin.calls != 1 {
		t.Errorf("lookup made %d calls, want exactly 1", plugin.calls)
	}
}

// TestARecordSurvivesTheRoundTrip drives one of this package's own sources out
// through the Plugin side and back in through the host side — the exact path
// BuildProvider composes — and asserts nothing is lost. FromSearch is the field
// that matters most: the host's acceptance test reads it, so a record that loses
// it on the wire would be stored unjudged (ADR-0050, ADR-0057 decision 3).
func TestARecordSurvivesTheRoundTrip(t *testing.T) {
	want := TitleMetadata{
		Matched:        true,
		Name:           "Dune",
		Year:           2021,
		Overview:       "A noble family.",
		Tagline:        "Beyond fear.",
		ContentRating:  "PG-13",
		ReleaseDate:    "2021-10-22",
		RuntimeMinutes: 155,
		Studio:         "Legendary",
		Genres:         []string{"Science Fiction"},
		Cast:           []Credit{{Person: "Timothée Chalamet", Role: "Actor", Character: "Paul", Kind: "cast", PersonRef: "tmdb:1", ImageURL: "https://i/1.jpg"}},
		Artwork:        []ArtworkRef{{Role: "poster", URL: "https://i/p.jpg"}},
		ExternalID:     "438631",
		Source:         "tmdb",
		FromSearch:     true,
	}
	source := &cannedProvider{meta: want}
	provider := ProviderFromPlugin(fullyCapable(), pluginFromProvider(source))

	got, err := provider.Lookup(context.Background(), TitleRef{
		Kind: "movie", Title: "Dune", Year: 2021, TMDBID: "438631",
		SeasonNumber: 1, EpisodeNumber: 2, Artist: "x", Album: "y", Track: "z",
		AlbumHints: []AlbumHint{{Title: "Hotel California", ReleaseGroupMBID: "rg-1"}},
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record lost data across the contract.\n got: %+v\nwant: %+v", got, want)
	}
	if !got.FromSearch {
		t.Error("FromSearch did not cross the contract; the host would store an unjudged search hit as a record")
	}
	// The ref crossed intact too, including the music coordinates and the
	// discography hints a source may identify an artist by (ADR-0053).
	if source.lastRef.TMDBID != "438631" || source.lastRef.EpisodeNumber != 2 ||
		source.lastRef.Track != "z" || len(source.lastRef.AlbumHints) != 1 ||
		source.lastRef.AlbumHints[0].ReleaseGroupMBID != "rg-1" {
		t.Errorf("the ref did not cross intact: %+v", source.lastRef)
	}
}

// TestSearchAndArtworkCarryTheirPagingAndPayload: the narrowing axes and the
// limit/offset the Edit-item picker threads through reach the Plugin, the
// candidates come back whole, and an empty list stays an EMPTY LIST rather than
// becoming an error — "the query found nothing" and "this kind cannot be searched"
// are different sentences and the picker renders both.
func TestSearchAndArtworkCarryTheirPagingAndPayload(t *testing.T) {
	plugin := &fakePlugin{
		searchResp: pluginapi.SearchResponse{
			Outcome: pluginapi.OutcomeMatched,
			Candidates: []pluginapi.SearchCandidate{{
				ExternalID: "438631", Title: "Dune", Year: 2021,
				ThumbnailURL: "https://i/t.jpg", Disambiguation: "part one",
				Kind: "movie", TypeLabel: "Album · Soundtrack",
			}},
		},
		artworkResp: pluginapi.ArtworkCandidatesResponse{
			Outcome:    pluginapi.OutcomeMatched,
			Candidates: []pluginapi.ArtworkCandidate{{URL: "https://i/p.jpg", Width: 2000, Height: 3000, Source: "fake"}},
		},
	}
	provider := ProviderFromPlugin(fullyCapable(), plugin)

	cands, err := provider.Search(context.Background(), "track", "Wasted Time", SearchOptions{
		Artist: "Eagles", Release: "Hotel California", Limit: 25, Offset: 50,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if plugin.lastSearch.Artist != "Eagles" || plugin.lastSearch.Release != "Hotel California" ||
		plugin.lastSearch.Limit != 25 || plugin.lastSearch.Offset != 50 || plugin.lastSearch.Kind != "track" {
		t.Fatalf("the query did not cross intact: %+v", plugin.lastSearch)
	}
	wantCand := Candidate{
		ExternalID: "438631", Title: "Dune", Year: 2021,
		ThumbnailURL: "https://i/t.jpg", Disambiguation: "part one",
		Kind: "movie", TypeLabel: "Album · Soundtrack",
		// Stamped by the host from the Plugin it asked (ADR-0060 decision 5).
		Source: fullyCapable().Slug,
	}
	if len(cands) != 1 || !reflect.DeepEqual(cands[0], wantCand) {
		t.Fatalf("candidates = %+v, want one %+v", cands, wantCand)
	}

	images, err := provider.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie", TMDBID: "438631"}, "poster")
	if err != nil {
		t.Fatalf("artwork candidates: %v", err)
	}
	if plugin.lastArtwork.Role != "poster" || plugin.lastArtwork.Ref.TMDBID != "438631" {
		t.Fatalf("the artwork request did not cross intact: %+v", plugin.lastArtwork)
	}
	wantImage := ArtworkCandidate{URL: "https://i/p.jpg", Width: 2000, Height: 3000, Source: "fake"}
	if len(images) != 1 || images[0] != wantImage {
		t.Fatalf("images = %+v, want one %+v", images, wantImage)
	}

	// Matched with nothing in it is a query that found nothing, not a failure.
	plugin.searchResp = pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}
	plugin.artworkResp = pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}
	if cands, err := provider.Search(context.Background(), "movie", "nothing", SearchOptions{}); err != nil || cands != nil {
		t.Errorf("empty search = %v/%v, want (nil, nil) — an empty result set, not unavailable", cands, err)
	}
	if imgs, err := provider.ArtworkCandidates(context.Background(), TitleRef{Kind: "movie"}, "logo"); err != nil || imgs != nil {
		t.Errorf("empty artwork list = %v/%v, want (nil, nil)", imgs, err)
	}
}

// TestTheEpisodeListCrossesTheContract: the season and episode lists an Admin
// picks from are a declared capability rather than a Go type assertion now, so the
// values they carry have to survive the edge — the still URL especially, since the
// API rewrites it to the same-origin image proxy before a browser sees it.
func TestTheEpisodeListCrossesTheContract(t *testing.T) {
	plugin := &fakePlugin{
		seasonsResp: pluginapi.SeriesSeasonsResponse{
			Outcome: pluginapi.OutcomeMatched,
			Seasons: []pluginapi.SeasonSummary{{Season: 2, EpisodeCount: 13}},
		},
		epsResp: pluginapi.SeasonEpisodesResponse{
			Outcome: pluginapi.OutcomeMatched,
			Episodes: []pluginapi.EpisodeCandidate{{
				Season: 2, Episode: 5, Name: "Breakage",
				Overview: "Hank suffers.", AirDate: "2009-04-05", StillURL: "https://i/s.jpg",
			}},
		},
	}
	lister, ok := ProviderFromPlugin(fullyCapable(), plugin).(EpisodeLister)
	if !ok {
		t.Fatal("an adapted Plugin must satisfy EpisodeLister")
	}

	seasons, err := lister.SeriesSeasons(context.Background(), "1396")
	if err != nil {
		t.Fatalf("series seasons: %v", err)
	}
	if len(seasons) != 1 || seasons[0] != (SeasonSummary{Season: 2, EpisodeCount: 13}) {
		t.Fatalf("seasons = %+v, want one season 2 with 13 episodes", seasons)
	}

	eps, err := lister.SeasonEpisodes(context.Background(), "1396", 2)
	if err != nil {
		t.Fatalf("season episodes: %v", err)
	}
	want := EpisodeCandidate{
		Season: 2, Episode: 5, Name: "Breakage",
		Overview: "Hank suffers.", AirDate: "2009-04-05", StillURL: "https://i/s.jpg",
	}
	if len(eps) != 1 || eps[0] != want {
		t.Fatalf("episodes = %+v, want one %+v", eps, want)
	}
}

// TestASourceWithoutAnEpisodeListIsUnavailableNotAnError: a Built-in that declares
// CapabilityEpisodeList is wrapped by the Plugin side even when the source behind
// it cannot list episodes, so the Plugin side has to answer "unavailable" rather
// than panic on the missing method — the same degradation the type assertion the
// chains used to make produced.
func TestASourceWithoutAnEpisodeListIsUnavailableNotAnError(t *testing.T) {
	lister, ok := pluginFromProvider(&cannedProvider{}).(pluginapi.EpisodeLister)
	if !ok {
		t.Fatal("the Plugin side must always expose the episode-list calls")
	}
	resp, err := lister.SeriesSeasons(context.Background(), pluginapi.SeriesSeasonsRequest{SeriesID: "1"})
	if err != nil {
		t.Fatalf("series seasons: %v", err)
	}
	if resp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable for a source that lists no episodes", resp.Outcome)
	}
	epsResp, err := lister.SeasonEpisodes(context.Background(), pluginapi.SeasonEpisodesRequest{SeriesID: "1", Season: 1})
	if err != nil {
		t.Fatalf("season episodes: %v", err)
	}
	if epsResp.Outcome != pluginapi.OutcomeUnavailable {
		t.Errorf("outcome = %q, want unavailable", epsResp.Outcome)
	}
}

// cannedProvider is a MetadataProvider in this package's OWN vocabulary — what a
// Built-in is — used to drive the Plugin side of the adapter. It implements no
// optional interface, so it is also the "source that cannot list episodes" case.
type cannedProvider struct {
	meta    TitleMetadata
	err     error
	lastRef TitleRef
}

func (c *cannedProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	c.lastRef = ref
	return c.meta, c.err
}

func (c *cannedProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (c *cannedProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}
