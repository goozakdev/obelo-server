package lyricfetch_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/lyricfetch"
	"github.com/goozakdev/obelo-server/internal/lyrics"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// --- fakes -------------------------------------------------------------------

// memStore is the lyrics table, the rejected answers and the Admin's order, in
// memory.
type memStore struct {
	local    map[string]lyrics.Lyrics
	fetched  map[string]store.FetchedLyrics
	rejected map[string][]string
	order    []string
	writes   int
}

func newMemStore() *memStore {
	return &memStore{
		local: map[string]lyrics.Lyrics{}, fetched: map[string]store.FetchedLyrics{}, rejected: map[string][]string{},
	}
}

func (m *memStore) LocalLyrics(id string) (lyrics.Lyrics, bool, error) {
	l, ok := m.local[id]
	return l, ok, nil
}

func (m *memStore) FetchedLyrics(id string) (store.FetchedLyrics, bool, error) {
	f, ok := m.fetched[id]
	return f, ok, nil
}

func (m *memStore) WriteFetchedLyrics(id string, f store.FetchedLyrics) error {
	m.writes++
	m.fetched[id] = f
	return nil
}

func (m *memStore) LyricProviderOrder() ([]string, error) { return m.order, nil }

func (m *memStore) RejectedLyrics(id string) ([]string, error) { return m.rejected[id], nil }

func (m *memStore) RejectFetchedLyrics(id, answer string) error {
	m.rejected[id] = append(m.rejected[id], answer)
	delete(m.fetched, id)
	return nil
}

// fakeProvider answers every question with resp (or err), counting the calls
// and appending its slug to a shared log so a test can read the asking order.
type fakeProvider struct {
	slug  string
	resp  pluginapi.LyricsResponse
	err   error
	mu    sync.Mutex
	calls int
	asked []pluginapi.LyricsRequest
	log   *[]string
}

func (f *fakeProvider) Lyrics(_ context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append(f.asked, req)
	if f.log != nil {
		*f.log = append(*f.log, f.slug)
	}
	return f.resp, f.err
}

func register(reg *pluginapi.Registry, providers ...*fakeProvider) {
	for _, p := range providers {
		p := p
		reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
			Descriptor: pluginapi.Descriptor{Slug: p.slug, Kinds: []string{pluginapi.KindMusic}},
			New:        func(pluginapi.Settings) (pluginapi.LyricProvider, error) { return p, nil },
		})
	}
}

// track is a three-and-a-half-minute Track with no recording id.
func track() lyricfetch.Track {
	return lyricfetch.Track{ID: "t1", Artist: "Lyric Band", Title: "Words", Album: "Words", DurationMs: 210000}
}

func synced(durationMs int64, lines ...string) pluginapi.LyricsResponse {
	r := pluginapi.LyricsResponse{Kind: pluginapi.LyricsSynced, DurationMs: durationMs}
	for i, text := range lines {
		r.Lines = append(r.Lines, pluginapi.LyricLine{StartMs: int64(i+1) * 1000, Text: text})
	}
	return r
}

func plain(text string) pluginapi.LyricsResponse {
	return pluginapi.LyricsResponse{Kind: pluginapi.LyricsPlain, Text: text}
}

func open(t *testing.T, svc *lyricfetch.Service, tr lyricfetch.Track) (lyricfetch.Answer, bool) {
	t.Helper()
	a, ok, err := svc.Lyrics(context.Background(), tr)
	if err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	return a, ok
}

func syncedLines(lines ...string) []lyrics.Line {
	var out []lyrics.Line
	for i, text := range lines {
		out = append(out, lyrics.Line{StartMs: int64(i+1) * 1000, Text: text})
	}
	return out
}

// --- the judgment ------------------------------------------------------------

// TestASyncedAnswerTimedTenSecondsOffIsKeptOnlyAsPlain: the provider's lines
// were timed against a recording ten seconds longer than this track. Following
// them would put every line in the wrong place, but the words are still the
// words, so the answer is stored and served as Plain — never as Synced, and never
// thrown away.
func TestASyncedAnswerTimedTenSecondsOffIsKeptOnlyAsPlain(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: synced(track().DurationMs+10000, "First line", "Second line")})
	svc := lyricfetch.New(st, reg)

	a, ok := open(t, svc, track())
	want := lyrics.Lyrics{Kind: lyrics.Plain, Text: "First line\nSecond line"}
	if !ok || a.Source != lyricfetch.SourceFetched || !reflect.DeepEqual(a.Lyrics, want) {
		t.Fatalf("answer = %+v (found %v), want fetched %+v", a, ok, want)
	}
	if got := st.fetched["t1"].Lyrics; got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
}

// TestASyncedAnswerStatingNoDurationIsKeptOnlyAsPlain: an answer that does not
// say what it was timed against cannot be checked, so it is not trusted as
// Synced either.
func TestASyncedAnswerStatingNoDurationIsKeptOnlyAsPlain(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: synced(0, "Only line")})
	a, ok := open(t, lyricfetch.New(st, reg), track())
	if !ok || a.Lyrics.Kind != lyrics.Plain || a.Lyrics.Text != "Only line" {
		t.Fatalf("answer = %+v (found %v), want Plain \"Only line\"", a, ok)
	}
}

// TestASyncedAnswerWithinThreeSecondsIsKeptSynced: a recording a couple of
// seconds shorter than the file (a trimmed silence) is the same recording.
func TestASyncedAnswerWithinThreeSecondsIsKeptSynced(t *testing.T) {
	for _, off := range []int64{-3000, 2500, 3000} {
		st, reg := newMemStore(), pluginapi.NewRegistry()
		register(reg, &fakeProvider{slug: "a", resp: synced(track().DurationMs+off, "First line", "Second line")})
		a, ok := open(t, lyricfetch.New(st, reg), track())
		want := lyrics.Lyrics{Kind: lyrics.Synced, Lines: syncedLines("First line", "Second line")}
		if !ok || !reflect.DeepEqual(a.Lyrics, want) {
			t.Fatalf("off by %d ms: answer = %+v, want %+v", off, a.Lyrics, want)
		}
	}
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: synced(track().DurationMs+3001, "Line")})
	if a, _ := open(t, lyricfetch.New(st, reg), track()); a.Lyrics.Kind != lyrics.Plain {
		t.Fatalf("off by 3001 ms: kind = %q, want plain", a.Lyrics.Kind)
	}
}

// TestAnAnswerForAnotherRecordingIsDroppedWhole: the host sent the recording id
// it holds and the provider answered for a different one. That answer is not
// kept even as Plain — it is not this song — so with nothing else asked, the
// track has no lyrics, and the next provider's answer is the one stored.
func TestAnAnswerForAnotherRecordingIsDroppedWhole(t *testing.T) {
	tr := track()
	tr.RecordingID = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	wrong := synced(tr.DurationMs, "Somebody else's song")
	wrong.RecordingID = "00000000-0000-0000-0000-000000000000"

	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", resp: wrong}
	register(reg, a)
	if got, ok := open(t, lyricfetch.New(st, reg), tr); ok {
		t.Fatalf("answer = %+v, want none: the only answer named another recording", got)
	}
	if a.asked[0].RecordingID != tr.RecordingID {
		t.Fatalf("the provider was asked for recording %q, want %q", a.asked[0].RecordingID, tr.RecordingID)
	}
	if f := st.fetched["t1"]; f.Lyrics != nil {
		t.Fatalf("stored = %+v, want no stored answer", *f.Lyrics)
	}

	st, reg = newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: wrong}, &fakeProvider{slug: "b", resp: plain("The right words")})
	got, ok := open(t, lyricfetch.New(st, reg), tr)
	if !ok || got.Lyrics.Text != "The right words" || st.fetched["t1"].Provider != "b" {
		t.Fatalf("answer = %+v stored from %q, want b's Plain answer", got, st.fetched["t1"].Provider)
	}
}

// TestAnAnswerNamingTheSameRecordingOrNoneIsKept: the check rejects a
// DIFFERENT recording, not a source that cannot name one.
func TestAnAnswerNamingTheSameRecordingOrNoneIsKept(t *testing.T) {
	tr := track()
	tr.RecordingID = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	for _, rec := range []string{"", "B1A9C0E9-D987-4042-AE91-78D6A3267D69"} {
		resp := synced(tr.DurationMs, "Line")
		resp.RecordingID = rec
		st, reg := newMemStore(), pluginapi.NewRegistry()
		register(reg, &fakeProvider{slug: "a", resp: resp})
		if a, ok := open(t, lyricfetch.New(st, reg), tr); !ok || a.Lyrics.Kind != lyrics.Synced {
			t.Fatalf("recording %q: answer = %+v (found %v), want Synced", rec, a, ok)
		}
	}
}

// TestTheFirstAcceptableSyncedAnswerInAdminOrderWins: the Admin put A before B
// (the reverse of registration). A's Synced answer is mistimed and B's is good,
// so B's Synced answer is the track's — not A's demoted Plain one.
func TestTheFirstAcceptableSyncedAnswerInAdminOrderWins(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	st.order = []string{"a", "b"}
	var asked []string
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs+10000, "A's words"), log: &asked}
	b := &fakeProvider{slug: "b", resp: synced(track().DurationMs, "B's words"), log: &asked}
	register(reg, b, a)

	got, ok := open(t, lyricfetch.New(st, reg), track())
	want := lyrics.Lyrics{Kind: lyrics.Synced, Lines: syncedLines("B's words")}
	if !ok || !reflect.DeepEqual(got.Lyrics, want) || st.fetched["t1"].Provider != "b" {
		t.Fatalf("answer = %+v from %q, want B's Synced %+v", got.Lyrics, st.fetched["t1"].Provider, want)
	}
	if !reflect.DeepEqual(asked, []string{"a", "b"}) {
		t.Fatalf("asked in order %v, want the Admin's [a b]", asked)
	}
}

// TestAnAcceptableSyncedAnswerStopsTheAsking: once one is accepted, the
// providers after it are not asked.
func TestAnAcceptableSyncedAnswerStopsTheAsking(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs, "A's words")}
	b := &fakeProvider{slug: "b", resp: synced(track().DurationMs, "B's words")}
	register(reg, a, b)
	if got, _ := open(t, lyricfetch.New(st, reg), track()); got.Lyrics.Lines[0].Text != "A's words" || b.calls != 0 {
		t.Fatalf("answer = %+v, b asked %d times; want A's and b never asked", got.Lyrics, b.calls)
	}
}

// TestTheFirstPlainAnswerIsTheFallback: with no Synced answer accepted from
// anyone, the first Plain answer seen in Admin order is kept.
func TestTheFirstPlainAnswerIsTheFallback(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg,
		&fakeProvider{slug: "a", resp: plain("A's words")},
		&fakeProvider{slug: "b", resp: synced(track().DurationMs+9000, "B's words")},
		&fakeProvider{slug: "c", resp: plain("C's words")})
	got, ok := open(t, lyricfetch.New(st, reg), track())
	if !ok || got.Lyrics.Kind != lyrics.Plain || got.Lyrics.Text != "A's words" || st.fetched["t1"].Provider != "a" {
		t.Fatalf("answer = %+v from %q, want A's Plain", got.Lyrics, st.fetched["t1"].Provider)
	}
}

// --- Local precedence ----------------------------------------------------------

// TestLocalSyncedLyricsAreNeverAskedAbout: a track whose own Local lyrics are
// Synced has everything a provider could give it.
func TestLocalSyncedLyricsAreNeverAskedAbout(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	st.local["t1"] = lyrics.Lyrics{Kind: lyrics.Synced, Lines: syncedLines("Local line")}
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Fetched line")}
	register(reg, a)
	got, ok := open(t, lyricfetch.New(st, reg), track())
	if !ok || got.Source != lyricfetch.SourceLocal || a.calls != 0 || st.writes != 0 {
		t.Fatalf("answer = %+v, provider asked %d times, %d writes; want Local and nothing asked", got, a.calls, st.writes)
	}
}

// TestLocalPlainLyricsStillAskEveryProvider: a Plain-only Local lyric is not an
// answer to "can this track be followed along", so each enabled provider is
// asked, and a fetched Synced answer wins over the Local Plain one.
func TestLocalPlainLyricsStillAskEveryProvider(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	st.local["t1"] = lyrics.Lyrics{Kind: lyrics.Plain, Text: "Local words"}
	a := &fakeProvider{slug: "a"}
	b := &fakeProvider{slug: "b", resp: synced(track().DurationMs, "Fetched line")}
	register(reg, a, b)
	got, ok := open(t, lyricfetch.New(st, reg), track())
	if a.calls != 1 || b.calls != 1 {
		t.Fatalf("asked a %d and b %d times, want each once", a.calls, b.calls)
	}
	if !ok || got.Source != lyricfetch.SourceFetched || got.Lyrics.Kind != lyrics.Synced {
		t.Fatalf("answer = %+v, want the fetched Synced one", got)
	}
}

// TestALocalPlainLyricOutranksAFetchedPlainOne: the first Plain answer seen is
// the Local one, so a provider's Plain answer does not replace it.
func TestALocalPlainLyricOutranksAFetchedPlainOne(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	st.local["t1"] = lyrics.Lyrics{Kind: lyrics.Plain, Text: "Local words"}
	register(reg, &fakeProvider{slug: "a", resp: plain("Fetched words")})
	svc := lyricfetch.New(st, reg)
	for i := 0; i < 2; i++ {
		got, _ := open(t, svc, track())
		if got.Source != lyricfetch.SourceLocal || got.Lyrics.Text != "Local words" {
			t.Fatalf("open %d: answer = %+v, want the Local Plain words", i+1, got)
		}
	}
}

// --- the cache -----------------------------------------------------------------

// TestASecondOpenReadsTheCache: the first open asks; the second reads what it
// found and asks nobody.
func TestASecondOpenReadsTheCache(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Line")}
	register(reg, a)
	svc := lyricfetch.New(st, reg)
	first, _ := open(t, svc, track())
	second, ok := open(t, svc, track())
	if a.calls != 1 {
		t.Fatalf("provider asked %d times over two opens, want 1", a.calls)
	}
	if !ok || !reflect.DeepEqual(first, second) {
		t.Fatalf("second open = %+v, want the first's %+v", second, first)
	}
}

// TestAMissIsRememberedUntilTheQuestionChanges: every provider missed, and that
// is remembered — re-opening with the same providers asks nobody. Enabling a new
// provider, or the track gaining a recording id, changes the question, and only
// then is it asked again (ADR-0051).
func TestAMissIsRememberedUntilTheQuestionChanges(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a"}
	register(reg, a)
	svc := lyricfetch.New(st, reg)

	if _, ok := open(t, svc, track()); ok {
		t.Fatal("found lyrics; want none")
	}
	if _, ok := open(t, svc, track()); ok || a.calls != 1 {
		t.Fatalf("after a second open the provider was asked %d times, want 1", a.calls)
	}

	b := &fakeProvider{slug: "b"}
	register(reg, b)
	open(t, svc, track())
	if a.calls != 2 || b.calls != 1 {
		t.Fatalf("after enabling b: asked a %d, b %d times; want 2 and 1", a.calls, b.calls)
	}
	open(t, svc, track())
	if a.calls != 2 || b.calls != 1 {
		t.Fatalf("re-opening with a and b: asked a %d, b %d times; want no more", a.calls, b.calls)
	}

	tr := track()
	tr.RecordingID = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	open(t, svc, tr)
	if a.calls != 3 || b.calls != 2 {
		t.Fatalf("after a recording id appeared: asked a %d, b %d times; want 3 and 2", a.calls, b.calls)
	}
}

// TestAFailedCallIsNotRememberedAsAMiss: a provider that errored did not answer,
// so nothing is settled and the next open asks again.
func TestAFailedCallIsNotRememberedAsAMiss(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", err: errors.New("source unreachable")}
	register(reg, a)
	svc := lyricfetch.New(st, reg)
	open(t, svc, track())
	open(t, svc, track())
	if a.calls != 2 || st.writes != 0 {
		t.Fatalf("asked %d times with %d writes, want 2 asks and nothing remembered", a.calls, st.writes)
	}
}

// TestAProviderForVideoOnlyIsNotAsked: a provider that declared only video
// serves no track.
func TestAProviderForVideoOnlyIsNotAsked(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	called := false
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "video", Kinds: []string{pluginapi.KindVideo}},
		New: func(pluginapi.Settings) (pluginapi.LyricProvider, error) {
			called = true
			return &fakeProvider{slug: "video", resp: plain("x")}, nil
		},
	})
	if _, ok := open(t, lyricfetch.New(st, reg), track()); ok || called {
		t.Fatalf("a video-only provider was built (%v) or answered (%v)", called, ok)
	}
}

// gatedProvider answers only once gate is closed, counting the calls.
type gatedProvider struct {
	gate  chan struct{}
	mu    sync.Mutex
	calls int
}

func (g *gatedProvider) Lyrics(ctx context.Context, _ pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	<-g.gate
	return synced(track().DurationMs, "Line"), nil
}

// TestTwoViewersOpeningAtOnceAskOnce: the second open waits for the first one's
// asking and reads what it remembered, rather than asking again.
func TestTwoViewersOpeningAtOnceAskOnce(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	g := &gatedProvider{gate: make(chan struct{})}
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "a"},
		New:        func(pluginapi.Settings) (pluginapi.LyricProvider, error) { return g, nil },
	})
	svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)
	var wg sync.WaitGroup
	answers := make([]lyricfetch.Answer, 2)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			answers[i], _, _ = svc.Lyrics(context.Background(), track())
		}(i)
	}
	for {
		g.mu.Lock()
		n := g.calls
		g.mu.Unlock()
		if n > 0 {
			break
		}
	}
	close(g.gate)
	wg.Wait()
	if g.calls != 1 {
		t.Fatalf("provider asked %d times by two concurrent opens, want 1", g.calls)
	}
	for i, a := range answers {
		if a.Lyrics.Kind != lyrics.Synced {
			t.Fatalf("viewer %d got %+v, want the Synced answer", i, a)
		}
	}
}

// syncStore serializes a memStore for the concurrent test.
type syncStore struct {
	mu *sync.Mutex
	m  *memStore
}

func (s syncStore) lock() func() {
	s.mu.Lock()
	return s.mu.Unlock
}

func (s syncStore) LocalLyrics(id string) (lyrics.Lyrics, bool, error) {
	defer s.lock()()
	return s.m.LocalLyrics(id)
}

func (s syncStore) FetchedLyrics(id string) (store.FetchedLyrics, bool, error) {
	defer s.lock()()
	return s.m.FetchedLyrics(id)
}

func (s syncStore) WriteFetchedLyrics(id string, f store.FetchedLyrics) error {
	defer s.lock()()
	return s.m.WriteFetchedLyrics(id, f)
}

func (s syncStore) LyricProviderOrder() ([]string, error) {
	defer s.lock()()
	return s.m.LyricProviderOrder()
}

func (s syncStore) RejectedLyrics(id string) ([]string, error) {
	defer s.lock()()
	return s.m.RejectedLyrics(id)
}

func (s syncStore) RejectFetchedLyrics(id, answer string) error {
	defer s.lock()()
	return s.m.RejectFetchedLyrics(id, answer)
}

// --- wrong lyrics --------------------------------------------------------------

// reject presses "wrong lyrics" on what tr shows.
func reject(t *testing.T, svc *lyricfetch.Service, tr lyricfetch.Track) (lyricfetch.Answer, bool) {
	t.Helper()
	shown, _ := open(t, svc, tr)
	a, ok, err := svc.Reject(context.Background(), tr, shown.ID)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	return a, ok
}

// TestWrongLyricsAdvancesToTheNextCandidate: a's Synced answer is stored and
// marked wrong. Every provider is asked again — a first, still answering the same
// — and a's answer is passed over for b's, which is what is stored and shown.
func TestWrongLyricsAdvancesToTheNextCandidate(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Wrong song")}
	b := &fakeProvider{slug: "b", resp: synced(track().DurationMs, "Right song")}
	register(reg, a, b)
	svc := lyricfetch.New(st, reg)

	if got, _ := open(t, svc, track()); got.Lyrics.Lines[0].Text != "Wrong song" {
		t.Fatalf("first answer = %+v, want a's", got)
	}
	got, ok := reject(t, svc, track())
	want := lyrics.Lyrics{Kind: lyrics.Synced, Lines: syncedLines("Right song")}
	if !ok || got.Source != lyricfetch.SourceFetched || !reflect.DeepEqual(got.Lyrics, want) {
		t.Fatalf("after wrong lyrics = %+v (found %v), want b's %+v", got, ok, want)
	}
	if f := st.fetched["t1"]; f.Provider != "b" || f.Lyrics == nil || !reflect.DeepEqual(*f.Lyrics, want) {
		t.Fatalf("stored = %+v, want b's answer", f)
	}
	if a.calls != 2 || b.calls != 1 {
		t.Fatalf("asked a %d, b %d times; want a re-asked (2) and b once", a.calls, b.calls)
	}
	if again, _ := open(t, svc, track()); !reflect.DeepEqual(again, got) || a.calls != 2 {
		t.Fatalf("next open = %+v with a asked %d times, want b's from the cache", again, a.calls)
	}
}

// TestARejectedAnswerIsNeverStoredAgain: the only provider keeps giving the
// answer marked wrong. The track ends with no lyrics — not the rejected answer,
// and not a stale hit — and the remembered outcome is a miss, so the next open
// still shows none.
func TestARejectedAnswerIsNeverStoredAgain(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Wrong song")}
	register(reg, a)
	svc := lyricfetch.New(st, reg)

	open(t, svc, track())
	if got, ok := reject(t, svc, track()); ok {
		t.Fatalf("after wrong lyrics = %+v, want none", got)
	}
	if f, have := st.fetched["t1"]; !have || f.Lyrics != nil {
		t.Fatalf("stored = %+v (present %v), want a remembered miss", f, have)
	}
	if got, ok := open(t, svc, track()); ok {
		t.Fatalf("next open = %+v, want none", got)
	}
	if a.calls != 2 {
		t.Fatalf("a asked %d times, want 2: once first, once re-asked", a.calls)
	}
}

// TestEveryRejectedAnswerStaysExcluded: a's answer is rejected, then b's. With
// only those two on offer, nothing is left — a's first rejection is not
// forgotten when b's is recorded.
func TestEveryRejectedAnswerStaysExcluded(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg,
		&fakeProvider{slug: "a", resp: synced(track().DurationMs, "First wrong")},
		&fakeProvider{slug: "b", resp: plain("Second wrong")})
	svc := lyricfetch.New(st, reg)

	open(t, svc, track())
	if got, _ := reject(t, svc, track()); got.Lyrics.Text != "Second wrong" {
		t.Fatalf("after the first rejection = %+v, want b's Plain answer", got)
	}
	if got, ok := reject(t, svc, track()); ok {
		t.Fatalf("after the second rejection = %+v, want none", got)
	}
}

// TestARejectedPlainAnswerIsExcludedEvenAsAFallback: a's mistimed Synced answer
// was kept as Plain and rejected. The same words from the same answer are not
// offered again as the fallback; b's Plain answer is.
func TestARejectedPlainAnswerIsExcludedEvenAsAFallback(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg,
		&fakeProvider{slug: "a", resp: synced(track().DurationMs+10000, "Mistimed")},
		&fakeProvider{slug: "b", resp: plain("Other words")})
	svc := lyricfetch.New(st, reg)

	if got, _ := open(t, svc, track()); got.Lyrics.Text != "Mistimed" {
		t.Fatalf("first answer = %+v, want a's as Plain", got)
	}
	if got, _ := reject(t, svc, track()); got.Lyrics.Text != "Other words" {
		t.Fatalf("after wrong lyrics = %+v, want b's", got)
	}
}

// TestWrongLyricsNeedsAFetchedAnswerOnShow: with nothing fetched, or with the
// track showing its own Local lyrics, there is no provider answer to reject.
func TestWrongLyricsNeedsAFetchedAnswerOnShow(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: plain("Fetched words")})
	svc := lyricfetch.New(st, reg)
	if _, _, err := svc.Reject(context.Background(), track(), "some-answer"); !errors.Is(err, lyricfetch.ErrNothingToReject) {
		t.Fatalf("Reject before any open = %v, want ErrNothingToReject", err)
	}

	st.local["t1"] = lyrics.Lyrics{Kind: lyrics.Plain, Text: "Local words"}
	open(t, svc, track())
	if _, _, err := svc.Reject(context.Background(), track(), "some-answer"); !errors.Is(err, lyricfetch.ErrNothingToReject) {
		t.Fatalf("Reject with Local lyrics on show = %v, want ErrNothingToReject", err)
	}
	if len(st.rejected["t1"]) != 0 || st.fetched["t1"].Lyrics == nil {
		t.Fatalf("rejected %v, stored %+v; want nothing rejected and the fetched answer kept",
			st.rejected["t1"], st.fetched["t1"])
	}
}

// TestARejectionNamingAnAnswerNoLongerShownRejectsNothing: two viewers see a's
// answer. The first marks it wrong and b's is shown. The second, still naming
// a's, presses too: b's answer is not the one named, so nothing is rejected and
// b's stays.
func TestARejectionNamingAnAnswerNoLongerShownRejectsNothing(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	c := &fakeProvider{slug: "c", resp: synced(track().DurationMs, "Third song")}
	register(reg,
		&fakeProvider{slug: "a", resp: synced(track().DurationMs, "Wrong song")},
		&fakeProvider{slug: "b", resp: synced(track().DurationMs, "Right song")}, c)
	svc := lyricfetch.New(st, reg)

	stale, _ := open(t, svc, track())
	if stale.ID == "" {
		t.Fatalf("a's answer = %+v, want it named", stale)
	}
	after, _ := reject(t, svc, track())
	if _, _, err := svc.Reject(context.Background(), track(), stale.ID); !errors.Is(err, lyricfetch.ErrNotShown) {
		t.Fatalf("Reject naming a's answer again = %v, want ErrNotShown", err)
	}
	if len(st.rejected["t1"]) != 1 || c.calls != 0 {
		t.Fatalf("rejected %d answers and asked c %d times; want only a's rejected, c never asked",
			len(st.rejected["t1"]), c.calls)
	}
	if got, _ := open(t, svc, track()); !reflect.DeepEqual(got, after) {
		t.Fatalf("shown = %+v, want b's %+v", got, after)
	}
}

// TestWhitespaceAndCaseVariantsOfARejectedAnswerStayExcluded: "A" is rejected.
// "A " and "a" are the same answer to a reader, so neither is kept in its place.
func TestWhitespaceAndCaseVariantsOfARejectedAnswerStayExcluded(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg,
		&fakeProvider{slug: "a", resp: synced(track().DurationMs, "A")},
		&fakeProvider{slug: "b", resp: synced(track().DurationMs, "A ")},
		&fakeProvider{slug: "c", resp: synced(track().DurationMs, "a")})
	svc := lyricfetch.New(st, reg)

	if got, _ := open(t, svc, track()); got.Lyrics.Lines[0].Text != "A" {
		t.Fatalf("first answer = %+v, want a's", got)
	}
	if got, ok := reject(t, svc, track()); ok {
		t.Fatalf("after rejecting \"A\" = %+v, want none: \"A \" and \"a\" are the same answer", got)
	}
}
