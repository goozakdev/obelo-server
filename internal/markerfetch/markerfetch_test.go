package markerfetch_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/markerfetch"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// --- fakes -------------------------------------------------------------------

// memStore is the fetched half of the markers table, in memory.
type memStore struct {
	mu       sync.Mutex
	fetched  map[string][]store.Marker
	question map[string]string
	writes   int
}

func newMemStore() *memStore {
	return &memStore{fetched: map[string][]store.Marker{}, question: map[string]string{}}
}

func (m *memStore) MarkerFetchQuestion(path string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.question[path]
	return q, ok, nil
}

func (m *memStore) SaveFetchedMarkers(path, question string, ms []store.Marker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	m.fetched[path] = ms
	m.question[path] = question
	return nil
}

// fakeProvider answers every question with resp (or err), counting the calls.
type fakeProvider struct {
	slug  string
	resp  pluginapi.MarkersResponse
	err   error
	mu    sync.Mutex
	calls int
	asked []pluginapi.MarkersRequest
}

func (f *fakeProvider) Markers(_ context.Context, req pluginapi.MarkersRequest) (pluginapi.MarkersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, req)
	return f.resp, f.err
}

func register(reg *pluginapi.Registry, kinds []string, providers ...*fakeProvider) {
	for _, p := range providers {
		p := p
		reg.RegisterMarkerProvider(pluginapi.MarkerProviderRegistration{
			Descriptor: pluginapi.Descriptor{Slug: p.slug, Kinds: kinds},
			New:        func(pluginapi.Settings) (pluginapi.MarkerProvider, error) { return p, nil },
		})
	}
}

const path = "/media/Show/Season 01/Show - S01E01.mkv"

// episode is a 22-minute Episode's File.
func episode() markerfetch.Item {
	return markerfetch.Item{
		Path: path, DurationMs: 1_320_000, Kind: "episode", Title: "Pilot",
		IDs: map[string]string{"tvdb": "1"}, ShowTitle: "Show", ShowIDs: map[string]string{"tmdb": "2"},
		SeasonNumber: 1, EpisodeNumber: 1,
	}
}

func candidate(kind string, start, end, lengthMs int64) pluginapi.MarkerCandidate {
	return pluginapi.MarkerCandidate{Kind: kind, StartMs: start, EndMs: end, DurationMs: lengthMs}
}

func fetch(t *testing.T, st *memStore, reg *pluginapi.Registry, it markerfetch.Item) {
	t.Helper()
	if err := markerfetch.New(st, reg).Fetch(context.Background(), it); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// --- the host's judgment ------------------------------------------------------

// TestACandidateTimedForAnotherLengthIsRejected is the host-owned judgment: a
// candidate whose recording was more than the tolerance longer or shorter than
// this File is never stored, however well-formed; one within it is.
func TestACandidateTimedForAnotherLengthIsRejected(t *testing.T) {
	it := episode()
	for _, tc := range []struct {
		name     string
		lengthMs int64
		kept     bool
	}{
		{"exact", it.DurationMs, true},
		{"within the tolerance longer", it.DurationMs + markerfetch.LengthToleranceMs, true},
		{"within the tolerance shorter", it.DurationMs - markerfetch.LengthToleranceMs, true},
		{"just past the tolerance longer", it.DurationMs + markerfetch.LengthToleranceMs + 1, false},
		{"just past the tolerance shorter", it.DurationMs - markerfetch.LengthToleranceMs - 1, false},
		{"ten seconds off", it.DurationMs + 10_000, false},
		{"another cut", 2_580_000, false},
		{"no length stated", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, reg := newMemStore(), pluginapi.NewRegistry()
			register(reg, nil, &fakeProvider{slug: "p", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
				candidate(pluginapi.MarkerIntro, 60_000, 120_000, tc.lengthMs),
			}}})
			fetch(t, st, reg, it)
			var want []store.Marker
			if tc.kept {
				want = []store.Marker{{Kind: "intro", Source: "fetched", StartMs: 60_000, EndMs: 120_000}}
			}
			if got := st.fetched[path]; !reflect.DeepEqual(got, want) {
				t.Fatalf("stored = %+v, want %+v", got, want)
			}
		})
	}
}

// TestAFileOfUnknownLengthKeepsNothing: with no length of its own there is
// nothing to check a candidate against, so none is believed.
func TestAFileOfUnknownLengthKeepsNothing(t *testing.T) {
	it := episode()
	it.DurationMs = 0
	st, reg := newMemStore(), pluginapi.NewRegistry()
	p := &fakeProvider{slug: "p", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
		candidate(pluginapi.MarkerIntro, 60_000, 120_000, 0),
	}}}
	register(reg, nil, p)
	fetch(t, st, reg, it)
	if p.calls != 0 || st.writes != 0 {
		t.Fatalf("calls = %d, writes = %d; want nothing asked or stored for a File of unknown length", p.calls, st.writes)
	}
}

// TestMalformedCandidatesAreDropped: an unknown kind, an empty or inverted span,
// a span outside the File — each dropped; a span running past the end by less
// than the tolerance is kept, ending at the File's end.
func TestMalformedCandidatesAreDropped(t *testing.T) {
	it := episode()
	d := it.DurationMs
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, nil, &fakeProvider{slug: "p", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
		candidate("commercial", 300_000, 360_000, d),
		candidate(pluginapi.MarkerRecap, 50_000, 50_000, d),
		candidate(pluginapi.MarkerRecap, 50_000, 40_000, d),
		candidate(pluginapi.MarkerPreview, -1_000, 10_000, d),
		candidate(pluginapi.MarkerPreview, d, d+1_000, d),
		candidate(pluginapi.MarkerCredits, d-60_000, d+2_000, d+2_000),
	}}})
	fetch(t, st, reg, it)
	want := []store.Marker{{Kind: "credits", Source: "fetched", StartMs: d - 60_000, EndMs: d}}
	if got := st.fetched[path]; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
}

// TestTheFirstProviderToAnswerAKindSuppliesIt: providers are asked in
// registration order, one of each kind is kept, and a later provider fills only
// a kind no earlier one answered acceptably.
func TestTheFirstProviderToAnswerAKindSuppliesIt(t *testing.T) {
	it := episode()
	d := it.DurationMs
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, nil,
		&fakeProvider{slug: "first", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
			candidate(pluginapi.MarkerIntro, 60_000, 120_000, d),
			candidate(pluginapi.MarkerIntro, 61_000, 121_000, d),
			candidate(pluginapi.MarkerCredits, 1_200_000, 1_320_000, d+60_000),
		}}},
		&fakeProvider{slug: "second", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
			candidate(pluginapi.MarkerIntro, 0, 30_000, d),
			candidate(pluginapi.MarkerCredits, 1_250_000, 1_320_000, d),
		}}},
	)
	fetch(t, st, reg, it)
	want := []store.Marker{
		{Kind: "intro", Source: "fetched", StartMs: 60_000, EndMs: 120_000},
		{Kind: "credits", Source: "fetched", StartMs: 1_250_000, EndMs: 1_320_000},
	}
	if got := st.fetched[path]; !reflect.DeepEqual(got, want) {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
}

// --- when the providers are asked ---------------------------------------------

// TestAnAnswerIsRememberedUntilTheQuestionChanges: a hit and a miss are both
// remembered, so a second play asks nobody; a File of another length is a new
// question.
func TestAnAnswerIsRememberedUntilTheQuestionChanges(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	p := &fakeProvider{slug: "p"}
	register(reg, nil, p)
	it := episode()
	fetch(t, st, reg, it)
	fetch(t, st, reg, it)
	if p.calls != 1 {
		t.Fatalf("calls after two plays = %d, want 1: a miss is remembered", p.calls)
	}
	want := pluginapi.MarkersRequest{
		Kind: "episode", Title: "Pilot", IDs: map[string]string{"tvdb": "1"}, ShowTitle: "Show",
		ShowIDs: map[string]string{"tmdb": "2"}, SeasonNumber: 1, EpisodeNumber: 1, DurationMs: it.DurationMs,
	}
	if !reflect.DeepEqual(p.asked[0], want) {
		t.Fatalf("asked %+v, want %+v", p.asked[0], want)
	}
	it.DurationMs += 60_000
	fetch(t, st, reg, it)
	if p.calls != 2 {
		t.Fatalf("calls after the File changed length = %d, want 2", p.calls)
	}
}

// TestAFailedProviderIsNotRememberedAsAMiss: what another provider answered is
// kept, but the question is asked again next time.
func TestAFailedProviderIsNotRememberedAsAMiss(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	it := episode()
	broken := &fakeProvider{slug: "broken", err: errors.New("down")}
	ok := &fakeProvider{slug: "ok", resp: pluginapi.MarkersResponse{Markers: []pluginapi.MarkerCandidate{
		candidate(pluginapi.MarkerIntro, 60_000, 120_000, it.DurationMs),
	}}}
	register(reg, nil, broken, ok)
	fetch(t, st, reg, it)
	if len(st.fetched[path]) != 1 {
		t.Fatalf("stored = %+v, want the working provider's intro", st.fetched[path])
	}
	fetch(t, st, reg, it)
	if broken.calls != 2 || ok.calls != 2 {
		t.Fatalf("calls = %d/%d, want both asked again after a failure", broken.calls, ok.calls)
	}
}

// TestOnlyProvidersServingVideoAreAsked: a provider declaring only music is not
// asked about a video File; one declaring no kinds is.
func TestOnlyProvidersServingVideoAreAsked(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	music := &fakeProvider{slug: "music"}
	every := &fakeProvider{slug: "every"}
	register(reg, []string{pluginapi.KindMusic}, music)
	register(reg, nil, every)
	fetch(t, st, reg, episode())
	if music.calls != 0 || every.calls != 1 {
		t.Fatalf("calls music/every = %d/%d, want 0/1", music.calls, every.calls)
	}
}

// TestNoProviderAsksNothingAndStoresNothing: with no Marker provider there is no
// question, and nothing is remembered that a later install would have to undo.
func TestNoProviderAsksNothingAndStoresNothing(t *testing.T) {
	st := newMemStore()
	fetch(t, st, pluginapi.NewRegistry(), episode())
	if st.writes != 0 {
		t.Fatalf("writes = %d, want 0", st.writes)
	}
}

// TestANewProviderIsANewQuestion: the providers that would be asked are part of
// the question, so a provider installed after a File's answer was remembered is
// asked on the next play — and so is the one that answered before.
func TestANewProviderIsANewQuestion(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	first := &fakeProvider{slug: "first"}
	register(reg, nil, first)
	it := episode()
	fetch(t, st, reg, it)
	later := &fakeProvider{slug: "later"}
	register(reg, nil, later)
	fetch(t, st, reg, it)
	if first.calls != 2 || later.calls != 1 {
		t.Fatalf("calls first/later = %d/%d, want 2/1: a new provider changes the question", first.calls, later.calls)
	}
}
