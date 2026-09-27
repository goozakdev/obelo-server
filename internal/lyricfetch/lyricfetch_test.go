package lyricfetch_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
		Descriptor: pluginapi.Descriptor{Slug: "a", Kinds: []string{pluginapi.KindMusic}},
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

// --- follow-ups --------------------------------------------------------------

// funcProvider answers each call with fn.
type funcProvider struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, call int) (pluginapi.LyricsResponse, error)
}

func (f *funcProvider) Lyrics(ctx context.Context, _ pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(ctx, n)
}

func (f *funcProvider) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func registerFunc(reg *pluginapi.Registry, slug string, p *funcProvider) {
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: slug, Kinds: []string{pluginapi.KindMusic}},
		New:        func(pluginapi.Settings) (pluginapi.LyricProvider, error) { return p, nil },
	})
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// TestAProviderDeclaringNoKindsIsNotAsked: a provider serves the kinds it
// declared, so one that declared none serves no track (Descriptor.Serves).
func TestAProviderDeclaringNoKindsIsNotAsked(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	built := false
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "none"},
		New: func(pluginapi.Settings) (pluginapi.LyricProvider, error) {
			built = true
			return &fakeProvider{slug: "none", resp: plain("x")}, nil
		},
	})
	svc := lyricfetch.New(st, reg)
	if regs, err := svc.Providers(); err != nil || len(regs) != 0 {
		t.Fatalf("Providers = %+v (%v), want none", regs, err)
	}
	if _, ok := open(t, svc, track()); ok || built {
		t.Fatalf("a provider declaring no kinds was built (%v) or answered (%v)", built, ok)
	}
}

// TestAPanickingProviderReleasesTheSlot: a provider that panics is a provider
// that failed. The open it was asked for ends, and the next open of the track
// asks again at once rather than waiting on an asking that will never finish.
func TestAPanickingProviderReleasesTheSlot(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	p := &funcProvider{fn: func(_ context.Context, call int) (pluginapi.LyricsResponse, error) {
		if call == 1 {
			panic("a provider bug")
		}
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc := lyricfetch.New(st, reg)

	func() {
		defer func() { _ = recover() }()
		_, _, _ = svc.Lyrics(context.Background(), track())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, ok, err := svc.Lyrics(ctx, track())
	if err != nil || !ok || a.Lyrics.Kind != lyrics.Synced || p.count() != 2 {
		t.Fatalf("the open after a panic = %+v (found %v, %v), provider asked %d times; want the Synced answer from a second ask",
			a, ok, err, p.count())
	}
}

// hookStore is a syncStore that runs hook after each FetchedLyrics read, outside
// the lock, numbering the reads from 1.
type hookStore struct {
	syncStore
	mu    sync.Mutex
	reads int
	hook  func(read int)
}

func (h *hookStore) readCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads
}

func (h *hookStore) FetchedLyrics(id string) (store.FetchedLyrics, bool, error) {
	f, ok, err := h.syncStore.FetchedLyrics(id)
	h.mu.Lock()
	h.reads++
	n := h.reads
	h.mu.Unlock()
	h.hook(n)
	return f, ok, err
}

// TestTheLeaderReReadsTheCacheAfterClaiming: a second open reads the cache (no
// answer yet), and before it claims the asking, the first open asks, stores its
// answer and finishes. The second open now leads — and finds the answer already
// stored, so the provider is asked once between them.
func TestTheLeaderReReadsTheCacheAfterClaiming(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	p := &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	read, proceed := make(chan struct{}), make(chan struct{})
	hs := &hookStore{syncStore: syncStore{mu: &sync.Mutex{}, m: st}, hook: func(n int) {
		if n == 1 {
			close(read)
			<-proceed
		}
	}}
	svc := lyricfetch.New(hs, reg)

	second := make(chan lyricfetch.Answer)
	go func() {
		a, _, _ := svc.Lyrics(context.Background(), track())
		second <- a
	}()
	<-read
	if first, ok := open(t, svc, track()); !ok || first.Lyrics.Kind != lyrics.Synced {
		t.Fatalf("first open = %+v, want the Synced answer", first)
	}
	close(proceed)
	if a := <-second; a.Lyrics.Kind != lyrics.Synced || p.count() != 1 {
		t.Fatalf("second open = %+v with the provider asked %d times, want the stored answer and 1 ask", a, p.count())
	}
}

// TestAnOpenDuringAnAskWaitsForItInsteadOfAsking: while the provider is being
// asked about a track, another open of it waits for that asking — here until
// its viewer gives up — and neither asks the provider nor takes the asking over
// (which would read the cache a second time, after claiming it). No timing: the
// second open happens inside the first one's call.
func TestAnOpenDuringAnAskWaitsForItInsteadOfAsking(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	hs := &hookStore{syncStore: syncStore{mu: &sync.Mutex{}, m: st}, hook: func(int) {}}
	var svc *lyricfetch.Service
	var during lyricfetch.Answer
	var duringOK bool
	var duringErr error
	duringReads := 0
	p := &funcProvider{fn: func(_ context.Context, call int) (pluginapi.LyricsResponse, error) {
		if call == 1 {
			before := hs.readCount()
			during, duringOK, duringErr = svc.Lyrics(cancelled(), track())
			duringReads = hs.readCount() - before
		}
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc = lyricfetch.New(hs, reg)

	if a, ok := open(t, svc, track()); !ok || a.Lyrics.Kind != lyrics.Synced {
		t.Fatalf("the asking open = %+v, want the Synced answer", a)
	}
	if duringReads != 1 {
		t.Fatalf("the open during the ask read the cache %d times, want 1: it took the asking over", duringReads)
	}
	if duringOK || duringErr != nil {
		t.Fatalf("the open during the ask = %+v (found %v, %v), want nothing yet and no error", during, duringOK, duringErr)
	}
	if p.count() != 1 {
		t.Fatalf("provider asked %d times, want 1: the open during the ask must not ask", p.count())
	}
}

// TestAViewerLeavingDoesNotEndTheAsk: the viewer whose open is asking the
// provider goes away mid-call. The call is the server's, not the viewer's: it is
// not cancelled (a cancelled call would count against the Plugin), and what it
// answers is still remembered for the next open.
func TestAViewerLeavingDoesNotEndTheAsk(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	viewer, leave := context.WithCancel(context.Background())
	var callErr error
	p := &funcProvider{fn: func(ctx context.Context, _ int) (pluginapi.LyricsResponse, error) {
		leave()
		callErr = ctx.Err()
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)

	if _, _, err := svc.Lyrics(viewer, track()); err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	a, ok := open(t, svc, track())
	if callErr != nil {
		t.Fatalf("the provider's call ended with the viewer: %v", callErr)
	}
	if !ok || a.Lyrics.Kind != lyrics.Synced || p.count() != 1 {
		t.Fatalf("next open = %+v (found %v), provider asked %d times; want the remembered answer, 1 ask", a, ok, p.count())
	}
}

// TestTheProviderIsAskedEverythingTheTrackCarries: artist, title, album, the
// track's own length and its recording id, as the host holds them.
func TestTheProviderIsAskedEverythingTheTrackCarries(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	a := &fakeProvider{slug: "a"}
	register(reg, a)
	tr := track()
	tr.RecordingID = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	open(t, lyricfetch.New(st, reg), tr)
	want := pluginapi.LyricsRequest{Artist: "Lyric Band", Title: "Words", Album: "Words", DurationMs: 210000, RecordingID: tr.RecordingID}
	if len(a.asked) != 1 || a.asked[0] != want {
		t.Fatalf("asked %+v, want once with %+v", a.asked, want)
	}
}

// TestAMissIsAskedAgainWhenTheTracksWordsOrLengthChange: each field the question
// carries is part of it, so changing any one of them re-asks a remembered miss —
// and re-opening with none changed does not.
func TestAMissIsAskedAgainWhenTheTracksWordsOrLengthChange(t *testing.T) {
	for name, change := range map[string]func(*lyricfetch.Track){
		"artist":   func(t *lyricfetch.Track) { t.Artist = "Another Band" },
		"title":    func(t *lyricfetch.Track) { t.Title = "Other Words" },
		"album":    func(t *lyricfetch.Track) { t.Album = "Deluxe Edition" },
		"duration": func(t *lyricfetch.Track) { t.DurationMs = 211000 },
	} {
		st, reg := newMemStore(), pluginapi.NewRegistry()
		a := &fakeProvider{slug: "a"}
		register(reg, a)
		svc := lyricfetch.New(st, reg)
		open(t, svc, track())
		open(t, svc, track())
		tr := track()
		change(&tr)
		open(t, svc, tr)
		if a.calls != 2 {
			t.Fatalf("%s changed: asked %d times over three opens, want 2 (once, then once more for the change)", name, a.calls)
		}
	}
}

// TestASecondPressOnARejectedAnswerIsLyricsChanged: two viewers pressed "wrong
// lyrics" on the same answer. The one that lost — whether it lands before the
// providers were asked again or after they missed — names an answer that is no
// longer shown, and is told so (ErrNotShown, LYRICS_CHANGED), not that the track
// has nothing fetched.
func TestASecondPressOnARejectedAnswerIsLyricsChanged(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Wrong song")})
	svc := lyricfetch.New(st, reg)

	shown, _ := open(t, svc, track())
	if _, ok, err := svc.Reject(context.Background(), track(), shown.ID); err != nil || ok {
		t.Fatalf("first press = (%v, %v), want no lyrics left", ok, err)
	}
	if _, _, err := svc.Reject(context.Background(), track(), shown.ID); !errors.Is(err, lyricfetch.ErrNotShown) {
		t.Fatalf("second press after the re-ask missed = %v, want ErrNotShown", err)
	}
	delete(st.fetched, "t1") // the moment between the rejection and the re-ask
	if _, _, err := svc.Reject(context.Background(), track(), shown.ID); !errors.Is(err, lyricfetch.ErrNotShown) {
		t.Fatalf("second press before the re-ask = %v, want ErrNotShown", err)
	}
	if len(st.rejected["t1"]) != 1 {
		t.Fatalf("rejected %d answers, want 1", len(st.rejected["t1"]))
	}
}

// TestRejectWaitsForAnAskUnderWay: a press that arrives while the providers are
// being asked about the track waits for that asking — here until its viewer
// gives up — rather than judging what the track shows before the answer lands.
func TestRejectWaitsForAnAskUnderWay(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	var svc *lyricfetch.Service
	var pressErr error
	p := &funcProvider{fn: func(_ context.Context, call int) (pluginapi.LyricsResponse, error) {
		if call == 1 {
			_, _, pressErr = svc.Reject(cancelled(), track(), "an-answer")
		}
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc = lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)

	shown, _ := open(t, svc, track())
	if !errors.Is(pressErr, context.Canceled) {
		t.Fatalf("the press during the ask = %v, want it to wait (context.Canceled)", pressErr)
	}
	if len(st.rejected["t1"]) != 0 {
		t.Fatalf("rejected %v during the ask, want nothing", st.rejected["t1"])
	}
	if _, _, err := svc.Reject(context.Background(), track(), shown.ID); err != nil {
		t.Fatalf("the press after the ask: %v", err)
	}
}

// syncedAt is a well-timed Synced answer whose lines start at the given times.
func syncedAt(text string, starts ...int64) pluginapi.LyricsResponse {
	r := pluginapi.LyricsResponse{Kind: pluginapi.LyricsSynced, DurationMs: track().DurationMs}
	for i, ms := range starts {
		r.Lines = append(r.Lines, pluginapi.LyricLine{StartMs: ms, Text: fmt.Sprintf("%s %d", text, i+1)})
	}
	return r
}

// TestASyncedAnswerShiftedByAFewMsIsTheRejectedOne: b's answer is a's, every line
// a few milliseconds off — within the 100 ms no reader can see — so rejecting
// a's passes over b's too, for c's.
func TestASyncedAnswerShiftedByAFewMsIsTheRejectedOne(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	b := &fakeProvider{slug: "b", resp: syncedAt("Wrong", 1040, 1900, 3100)}
	register(reg,
		&fakeProvider{slug: "a", resp: syncedAt("Wrong", 1000, 2000, 3000)},
		b,
		&fakeProvider{slug: "c", resp: syncedAt("Right", 1000, 2000, 3000)})
	svc := lyricfetch.New(st, reg)

	open(t, svc, track())
	if got, _ := reject(t, svc, track()); got.Lyrics.Lines[0].Text != "Right 1" || b.calls != 1 {
		t.Fatalf("after wrong lyrics = %+v, want c's (b's is a's, shifted)", got.Lyrics)
	}
}

// TestASyncedAnswerRetimedPastTheToleranceIsNew: b's answer has a's words with
// one line 101 ms off — a retiming, not a copy — so it is a new answer and is
// kept once a's is rejected.
func TestASyncedAnswerRetimedPastTheToleranceIsNew(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg,
		&fakeProvider{slug: "a", resp: syncedAt("Words", 1000, 2000, 3000)},
		&fakeProvider{slug: "b", resp: syncedAt("Words", 1000, 2101, 3000)})
	svc := lyricfetch.New(st, reg)

	open(t, svc, track())
	got, ok := reject(t, svc, track())
	if !ok || st.fetched["t1"].Provider != "b" || got.Lyrics.Lines[1].StartMs != 2101 {
		t.Fatalf("after wrong lyrics = %+v from %q, want b's retimed answer", got.Lyrics, st.fetched["t1"].Provider)
	}
}

// TestARejectionRecordedByIDAloneStillExcludesItsAnswer: a rejection kept as the
// answer's id rather than the answer itself still passes over that answer, and
// a second press naming it is still LYRICS_CHANGED.
func TestARejectionRecordedByIDAloneStillExcludesItsAnswer(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: synced(track().DurationMs, "Wrong song")})
	svc := lyricfetch.New(st, reg)

	shown, _ := open(t, svc, track())
	st.rejected["t1"] = []string{shown.ID}
	delete(st.fetched, "t1")
	if got, ok := open(t, svc, track()); ok {
		t.Fatalf("after a rejection by id = %+v, want none", got)
	}
	if _, _, err := svc.Reject(context.Background(), track(), shown.ID); !errors.Is(err, lyricfetch.ErrNotShown) {
		t.Fatalf("a press naming it = %v, want ErrNotShown", err)
	}
}

// TestARightAnswerSharingItsFirstLinesWithARejectedOneIsNotIt: a rejected Synced
// answer and a later one agree, line for line, as far as the shorter goes. They
// are two answers, not one retimed — the line count differs — so rejecting one
// never passes over the other, whichever of them is the longer.
func TestARightAnswerSharingItsFirstLinesWithARejectedOneIsNotIt(t *testing.T) {
	for name, starts := range map[string][2][]int64{
		"the rejected answer is shorter": {{1000, 2000, 3000}, {1000, 2000, 3000, 4000}},
		"the rejected answer is longer":  {{1000, 2000, 3000, 4000}, {1000, 2000, 3000}},
	} {
		st, reg := newMemStore(), pluginapi.NewRegistry()
		register(reg,
			&fakeProvider{slug: "a", resp: syncedAt("Words", starts[0]...)},
			&fakeProvider{slug: "b", resp: syncedAt("Words", starts[1]...)})
		svc := lyricfetch.New(st, reg)

		open(t, svc, track())
		got, ok := reject(t, svc, track())
		if !ok || st.fetched["t1"].Provider != "b" || len(got.Lyrics.Lines) != len(starts[1]) {
			t.Fatalf("%s: after wrong lyrics = %+v from %q, want b's %d lines",
				name, got.Lyrics, st.fetched["t1"].Provider, len(starts[1]))
		}
	}
}

// titleProvider answers each call with fn, and counts the calls by the title
// asked about.
type titleProvider struct {
	mu    sync.Mutex
	asked map[string]int
	fn    func(ctx context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error)
}

func (p *titleProvider) Lyrics(ctx context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
	p.mu.Lock()
	if p.asked == nil {
		p.asked = map[string]int{}
	}
	p.asked[req.Title]++
	p.mu.Unlock()
	return p.fn(ctx, req)
}

func (p *titleProvider) askedAbout(title string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked[title]
}

// TestAnAskAbandonedBeforeItsCallStartsAsksNobody: a provider answers one call
// at a time, so while it is busy with one track, the asking for another waits
// its turn. Every viewer of that other track leaves before the turn comes: the
// asking ends there, the provider is never called about it (a call nobody waits
// for is no strike against the Plugin, and no load on it either), and nothing is
// remembered — the next open asks.
func TestAnAskAbandonedBeforeItsCallStartsAsksNobody(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	started, gate := make(chan struct{}), make(chan struct{})
	p := &titleProvider{fn: func(_ context.Context, req pluginapi.LyricsRequest) (pluginapi.LyricsResponse, error) {
		if req.Title == track().Title {
			close(started)
			<-gate
		}
		return synced(track().DurationMs, req.Title), nil
	}}
	reg.RegisterLyricProvider(pluginapi.LyricProviderRegistration{
		Descriptor: pluginapi.Descriptor{Slug: "a", Kinds: []string{pluginapi.KindMusic}},
		New:        func(pluginapi.Settings) (pluginapi.LyricProvider, error) { return p, nil },
	})
	svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)

	busy := make(chan struct{})
	go func() {
		defer close(busy)
		_, _, _ = svc.Lyrics(context.Background(), track())
	}()
	<-started
	other := track()
	other.ID, other.Title = "t2", "Other words"
	if a, ok, err := svc.Lyrics(cancelled(), other); ok || err != nil {
		t.Fatalf("the open that left = %+v (found %v, %v), want nothing and no error", a, ok, err)
	}
	// Reject waits out any asking under way for the track, then finds nothing
	// fetched: the abandoned asking ended without asking or remembering.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := svc.Reject(ctx, other, "any"); !errors.Is(err, lyricfetch.ErrNothingToReject) {
		t.Fatalf("after the abandoned asking, a press = %v, want ErrNothingToReject (nothing remembered)", err)
	}
	if n := p.askedAbout(other.Title); n != 0 {
		t.Fatalf("the provider was asked about the abandoned track %d times, want 0", n)
	}
	close(gate)
	<-busy
	if a, ok := open(t, svc, other); !ok || a.Lyrics.Lines[0].Text != other.Title || p.askedAbout(other.Title) != 1 {
		t.Fatalf("the next open = %+v (found %v), asked %d times; want the answer from one ask",
			a, ok, p.askedAbout(other.Title))
	}
}

// TestAnAbandonedAskFinishesACallAlreadyStarted: the viewer leaves — the only
// one waiting — once the provider's call has started. That call is not cut
// short (a cut-short call counts against the Plugin): it runs to its end, and
// its answer is remembered.
func TestAnAbandonedAskFinishesACallAlreadyStarted(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	viewer, leave := context.WithCancel(context.Background())
	gone := make(chan struct{})
	var callErr error
	p := &funcProvider{fn: func(ctx context.Context, _ int) (pluginapi.LyricsResponse, error) {
		leave()
		<-gone
		// Long enough for a cancellation that the viewer's leaving set off to land.
		select {
		case <-ctx.Done():
			callErr = ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)

	if _, ok, err := svc.Lyrics(viewer, track()); ok || err != nil {
		t.Fatalf("the open that left = (%v, %v), want nothing and no error", ok, err)
	}
	close(gone)
	a, ok := open(t, svc, track())
	if callErr != nil {
		t.Fatalf("the call already started ended when its viewer left: %v", callErr)
	}
	if !ok || a.Lyrics.Kind != lyrics.Synced || p.count() != 1 {
		t.Fatalf("next open = %+v (found %v), provider asked %d times; want the remembered answer, 1 ask", a, ok, p.count())
	}
}

// TestEachProviderHasItsOwnTimeLimit: the first provider hangs until its time is
// up. The second is asked with a whole limit of its own, not what the first
// left over, so its answer is the track's.
func TestEachProviderHasItsOwnTimeLimit(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	registerFunc(reg, "hung", &funcProvider{fn: func(ctx context.Context, _ int) (pluginapi.LyricsResponse, error) {
		<-ctx.Done()
		return pluginapi.LyricsResponse{}, ctx.Err()
	}})
	registerFunc(reg, "healthy", &funcProvider{fn: func(ctx context.Context, _ int) (pluginapi.LyricsResponse, error) {
		select {
		case <-time.After(50 * time.Millisecond):
			return synced(track().DurationMs, "Line"), nil
		case <-ctx.Done():
			return pluginapi.LyricsResponse{}, ctx.Err()
		}
	}})
	svc := lyricfetch.New(st, reg)
	lyricfetch.SetAskTimeout(svc, 200*time.Millisecond)

	if a, ok := open(t, svc, track()); !ok || a.Lyrics.Kind != lyrics.Synced || st.fetched["t1"].Provider != "healthy" {
		t.Fatalf("answer = %+v (found %v) from %q, want the healthy provider's", a, ok, st.fetched["t1"].Provider)
	}
}

// lockedBuffer is a log destination safe to write from the asking goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLog sends the standard logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *lockedBuffer {
	buf := &lockedBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// panicStore is a syncStore whose first read of a track's rejections panics.
type panicStore struct {
	syncStore
	panicked bool
}

func (p *panicStore) RejectedLyrics(id string) ([]string, error) {
	if !p.panicked {
		p.panicked = true
		panic("a store bug")
	}
	return p.syncStore.RejectedLyrics(id)
}

// TestAPanicInTheAskingIsRecoveredAndLoggedWithAStack: the asking runs on its
// own goroutine, where nothing else would catch a panic, so a bug outside any
// provider's call — here in the store — is recovered there: the open is an
// error, the panic is logged with the stack that raised it, and the track can be
// asked about again.
func TestAPanicInTheAskingIsRecoveredAndLoggedWithAStack(t *testing.T) {
	logged := captureLog(t)
	st, reg := newMemStore(), pluginapi.NewRegistry()
	p := &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "a", p)
	svc := lyricfetch.New(&panicStore{syncStore: syncStore{mu: &sync.Mutex{}, m: st}}, reg)

	if _, _, err := svc.Lyrics(context.Background(), track()); err == nil {
		t.Fatal("the open whose asking panicked = no error, want one")
	}
	if out := logged.String(); !strings.Contains(out, "a store bug") || !strings.Contains(out, "panicStore).RejectedLyrics") {
		t.Fatalf("log = %q, want the panic and the stack that raised it", out)
	}
	if a, ok := open(t, svc, track()); !ok || a.Lyrics.Kind != lyrics.Synced || p.count() != 1 {
		t.Fatalf("the next open = %+v (found %v), provider asked %d times; want the Synced answer", a, ok, p.count())
	}
}

// TestFetchedLinesPastADayAreKept: a fetched answer's lines are bounded as a
// Local one's are — by the largest start a client reads exactly — not by a day,
// so an audiobook's late lines are kept. Past the bound a line is dropped.
func TestFetchedLinesPastADayAreKept(t *testing.T) {
	const maxSafe = 1<<53 - 1
	resp := pluginapi.LyricsResponse{Kind: pluginapi.LyricsSynced, DurationMs: track().DurationMs, Lines: []pluginapi.LyricLine{
		{StartMs: 1000, Text: "Start"}, {StartMs: 25 * 60 * 60 * 1000, Text: "A day in"},
		{StartMs: maxSafe, Text: "Last"}, {StartMs: maxSafe + 1, Text: "Past the bound"},
	}}
	st, reg := newMemStore(), pluginapi.NewRegistry()
	register(reg, &fakeProvider{slug: "a", resp: resp})
	a, ok := open(t, lyricfetch.New(st, reg), track())
	want := []lyrics.Line{{StartMs: 1000, Text: "Start"}, {StartMs: 25 * 60 * 60 * 1000, Text: "A day in"}, {StartMs: maxSafe, Text: "Last"}}
	if !ok || a.Lyrics.Kind != lyrics.Synced || !reflect.DeepEqual(a.Lyrics.Lines, want) {
		t.Fatalf("answer = %+v (found %v), want lines %+v", a.Lyrics, ok, want)
	}
}

// TestAPanickingProviderFailsLikeAnErrorAndTheNextIsAsked: the first provider
// panics. That is its failure alone — logged, not remembered as a miss — and
// the asking goes on to the second, whose answer is the track's.
func TestAPanickingProviderFailsLikeAnErrorAndTheNextIsAsked(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	logged := captureLog(t)
	registerFunc(reg, "panics", &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		panic("a provider bug")
	}})
	next := &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		return synced(track().DurationMs, "Line"), nil
	}}
	registerFunc(reg, "next", next)
	svc := lyricfetch.New(st, reg)

	a, ok, err := svc.Lyrics(context.Background(), track())
	if err != nil || !ok || a.Lyrics.Kind != lyrics.Synced || next.count() != 1 || st.fetched["t1"].Provider != "next" {
		t.Fatalf("open = %+v (found %v, %v), next asked %d times; want the next provider's Synced answer",
			a, ok, err, next.count())
	}
	if !strings.Contains(logged.String(), "lyrics from panics panicked") {
		t.Fatalf("the panic was not logged as the provider's:\n%s", logged.String())
	}
}

// TestAnAskLeftWhileAProviderAnswersAsksNoMoreProviders: the only viewer leaves
// while the first provider is answering. The second provider's turn is free by
// then, and the asking still goes no further: nobody is waiting for it. Freeing
// of the turn and the leaving can both be ready at once, so this is tried many
// times — an asking that took a free turn over a leaving would call the second
// provider in about half of them.
func TestAnAskLeftWhileAProviderAnswersAsksNoMoreProviders(t *testing.T) {
	for i := 0; i < 64; i++ {
		st, reg := newMemStore(), pluginapi.NewRegistry()
		viewer, leave := context.WithCancel(context.Background())
		gone := make(chan struct{})
		registerFunc(reg, "first", &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
			leave()
			<-gone
			return plain("Words"), nil
		}})
		second := &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
			return synced(track().DurationMs, "Line"), nil
		}}
		registerFunc(reg, "second", second)
		svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)

		if _, ok, err := svc.Lyrics(viewer, track()); ok || err != nil {
			t.Fatalf("the open that left = (%v, %v), want nothing and no error", ok, err)
		}
		close(gone)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _, err := svc.Reject(ctx, track(), "any")
		cancel()
		if !errors.Is(err, lyricfetch.ErrNothingToReject) {
			t.Fatalf("after the abandoned asking, a press = %v, want ErrNothingToReject (nothing remembered)", err)
		}
		if n := second.count(); n != 0 {
			t.Fatalf("try %d: the second provider was asked %d times after every viewer had left, want 0", i, n)
		}
	}
}

// TestAHungProviderDoesNotHoldItsTurn: an in-process provider that never
// returns, whatever its context says. Its call is given up on when its time is
// up — a failure, like any other — so the next provider answers the track, and
// the next track's asking gets the hung provider's turn rather than waiting on
// it forever.
func TestAHungProviderDoesNotHoldItsTurn(t *testing.T) {
	st, reg := newMemStore(), pluginapi.NewRegistry()
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	hung := &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		<-never
		return pluginapi.LyricsResponse{}, errors.New("released at the end of the test")
	}}
	registerFunc(reg, "hung", hung)
	registerFunc(reg, "healthy", &funcProvider{fn: func(context.Context, int) (pluginapi.LyricsResponse, error) {
		return synced(track().DurationMs, "Line"), nil
	}})
	svc := lyricfetch.New(syncStore{mu: &sync.Mutex{}, m: st}, reg)
	lyricfetch.SetAskTimeout(svc, 100*time.Millisecond)

	other := track()
	other.ID = "t2"
	for _, tr := range []lyricfetch.Track{track(), other} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a, ok, err := svc.Lyrics(ctx, tr)
		cancel()
		if err != nil || !ok || a.Lyrics.Kind != lyrics.Synced {
			t.Fatalf("open of %s = %+v (found %v, %v), want the healthy provider's answer", tr.ID, a, ok, err)
		}
	}
	if n := hung.count(); n != 2 {
		t.Fatalf("the hung provider was asked %d times, want once per track", n)
	}
}
