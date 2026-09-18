package enrich

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ADR-0050's tracklist tier, AT THE HOST'S SEAM.
//
// The provider half of this file left with MusicBrainz (.scratch/bundled-plugins
// issue 06): the request shapes, the parent check, fit-selection and the
// candidate preview are asserted natively in plugins/musicbrainz/musicbrainz,
// where the provider now lives. What stays here is what the SERVICE owns — the
// enablement gate, the capability type-assert, the cache, the composition, and
// the tiering as the pass sees it — and the stub below is what it is asserted
// against.
//
// THE STUB IS NOW A GO FAKE, NOT AN HTTP ONE, and that is the honest shape. A
// canned MusicBrainz served over httptest was how these tests reached a compiled-in
// source; the source is a WebAssembly module behind the contract now, and standing
// up a sandbox to answer a cache question would be a slower test of the same thing.
// The fake implements the three seams the service asks for and counts the calls it
// would have made, which is exactly what every assertion below reads.

// --- the canned album source ---------------------------------------------------

// stubTrack is one track of a canned release. Rec is the recording MBID; "" is the
// real case where MusicBrainz has the track but no recording behind it.
type stubTrack struct {
	Title string
	Rec   string
}

// stubRelease is one canned edition: its id, its date, its parent release-group,
// and its tracks per disc — plus the three descriptive fields the EDITION PICKER
// reads off the same browse (ADR-0052): country, medium format, and the source's
// own disambiguation comment.
type stubRelease struct {
	ID      string
	Date    string
	RGID    string
	Country string
	Format  string
	Disamb  string
	Discs   [][]stubTrack
}

// tracks flattens a canned release into the ordered TrackCandidates a source
// returns for it — discs numbered from 1, an entry with no recording id keeping
// its position with an empty ExternalID.
func (r stubRelease) tracks() []TrackCandidate {
	var out []TrackCandidate
	for di, d := range r.Discs {
		for ti, tr := range d {
			out = append(out, TrackCandidate{
				Disc: di + 1, Position: ti + 1, Title: tr.Title, ExternalID: tr.Rec,
			})
		}
	}
	return out
}

func (r stubRelease) trackCount() int {
	n := 0
	for _, d := range r.Discs {
		n += len(d)
	}
	return n
}

// edition projects a canned release onto the ReleaseEdition view the picker reads.
func (r stubRelease) edition() ReleaseEdition {
	format := r.Format
	if format != "" && len(r.Discs) > 1 {
		format = fmt.Sprintf("%d×%s", len(r.Discs), format)
	}
	return ReleaseEdition{
		ReleaseID: r.ID, Date: r.Date, Country: r.Country, Format: format,
		TrackCount: r.trackCount(), Disambiguation: r.Disamb,
	}
}

// disc builds one disc of n tracks titled "<prefix> N", each with recording id
// "rec-<prefix>-N", so a test can tell two editions apart by title alone.
func disc(prefix string, n int) []stubTrack {
	out := make([]stubTrack, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, stubTrack{
			Title: fmt.Sprintf("%s %d", prefix, i),
			Rec:   fmt.Sprintf("rec-%s-%d", prefix, i),
		})
	}
	return out
}

// tracklistStub is a canned music source: releases addressable by id and by
// release-group, a per-CALL forced failure, and a record of every call so a test
// can assert what the answer actually COST.
//
// It applies ADR-0050's tiering the way the MusicBrainz plugin does — the named
// release is used only when it belongs to the album, a human's chosen release that
// does not apply stops rather than falling through, and fit-selection goes through
// pickEditionByFit, which is the host's own copy of the rule and the same one the
// edition picker marks its in-use row with.
type tracklistStub struct {
	mu    sync.Mutex
	calls []string
	byID  map[string]stubRelease
	byRG  map[string][]stubRelease
	fails map[string]error
}

func newTracklistStub(t *testing.T, releases ...stubRelease) (*tracklistStub, *tracklistStub) {
	t.Helper()
	s := &tracklistStub{
		byID: map[string]stubRelease{}, byRG: map[string][]stubRelease{}, fails: map[string]error{},
	}
	for _, r := range releases {
		s.byID[r.ID] = r
		s.byRG[r.RGID] = append(s.byRG[r.RGID], r)
	}
	// Two returns so the call sites read as they did when the provider and the
	// recorder were different objects.
	return s, s
}

func (s *tracklistStub) note(call string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
	return s.fails[call]
}

func (s *tracklistStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// fail makes the named call answer err instead. The calls are named the way the
// wire named them, so a test reads the same as it did: "/release/<id>" is the
// named-release lookup and "/release" the release-group browse.
func (s *tracklistStub) fail(call string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[call] = err
}

func (s *tracklistStub) Lookup(context.Context, TitleRef) (TitleMetadata, error) {
	return TitleMetadata{}, ErrNoMatch
}

func (s *tracklistStub) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (s *tracklistStub) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, nil
}

func (s *tracklistStub) AlbumTracklist(_ context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	rgID := strings.TrimSpace(req.ReleaseGroupID)
	if rgID == "" {
		return nil, ErrNoTracklist
	}
	if relID := strings.TrimSpace(req.ReleaseID); relID != "" {
		if err := s.note("/release/" + relID); err != nil {
			return nil, err
		}
		rel, ok := s.lookupRelease(relID)
		switch {
		case ok && strings.EqualFold(rel.RGID, rgID):
			if tl := rel.tracks(); len(tl) > 0 {
				return tl, nil
			}
		}
		// A stranger's release, an unknown id, or one with no tracks: none of them
		// says anything about whether the ALBUM has a tracklist.
		if req.ReleaseIDChosen {
			return nil, ErrNoTracklist
		}
	}
	if err := s.note("/release"); err != nil {
		return nil, err
	}
	eds, rels := s.editionsOf(rgID)
	i := pickEditionByFit(eds, req.LocalTrackCount)
	if i < 0 {
		return nil, ErrNoTracklist
	}
	tl := rels[i].tracks()
	if len(tl) == 0 {
		return nil, ErrNoTracklist
	}
	return tl, nil
}

func (s *tracklistStub) ReleaseGroupEditions(_ context.Context, releaseGroupID string) ([]ReleaseEdition, error) {
	rgID := strings.TrimSpace(releaseGroupID)
	if rgID == "" {
		return nil, nil
	}
	if err := s.note("/release"); err != nil {
		return nil, err
	}
	eds, _ := s.editionsOf(rgID)
	return eds, nil
}

func (s *tracklistStub) lookupRelease(id string) (stubRelease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	return r, ok
}

// editionsOf returns the release-group's editions and the releases they were
// projected from, INDEX FOR INDEX.
func (s *tracklistStub) editionsOf(rgID string) ([]ReleaseEdition, []stubRelease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rels := s.byRG[rgID]
	eds := make([]ReleaseEdition, 0, len(rels))
	for _, r := range rels {
		eds = append(eds, r.edition())
	}
	return eds, rels
}

// gotTitles renders a tracklist as "disc/position title" lines, which is what every
// assertion here is actually about: WHICH edition came back, in order.
func gotTitles(tl []TrackCandidate) []string {
	out := make([]string, 0, len(tl))
	for _, t := range tl {
		out = append(out, fmt.Sprintf("%d/%d %s", t.Disc, t.Position, t.Title))
	}
	return out
}

func wantTitles(t *testing.T, tl []TrackCandidate, want ...string) {
	t.Helper()
	got := gotTitles(tl)
	if len(got) != len(want) {
		t.Fatalf("tracklist has %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tracklist[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// errShed is what the canned source answers when a test wants an outage rather
// than an answer: transient, so the pass retries it (ADR-0048), which is what a
// 503 from a real source produces.
var errShed = transient(errors.New("enrich: musicbrainz: status 503 (the host is shedding load)"))

// --- the cache ------------------------------------------------------------------

// countingTracklister answers a fixed tracklist and counts how often it was asked —
// the cache's whole subject.
type countingTracklister struct {
	fakeProvider
	mu    sync.Mutex
	calls int
	tl    []TrackCandidate
	err   error
}

func (c *countingTracklister) AlbumTracklist(context.Context, TracklistRequest) ([]TrackCandidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.tl, c.err
}

func (c *countingTracklister) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fakeProvider satisfies MetadataProvider so a tracklister can be a Service's
// provider; nothing here calls these.
type fakeProvider struct{}

func (fakeProvider) Lookup(context.Context, TitleRef) (TitleMetadata, error) {
	return TitleMetadata{}, ErrNoMatch
}
func (fakeProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}
func (fakeProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, nil
}

func tracklistService(t *testing.T, ttl time.Duration) (*Service, *countingTracklister, providerSnapshot) {
	t.Helper()
	prov := &countingTracklister{tl: []TrackCandidate{{Disc: 1, Position: 1, Title: "One", ExternalID: "rec-1"}}}
	svc := NewService(nil, prov, nil, Enablement{Music: true}, "", 0)
	svc.tracklists = newListCache[[]TrackCandidate](ttl)
	return svc, prov, providerSnapshot{provider: prov, enablement: Enablement{Music: true}}
}

// Two reads for the same album inside the TTL cost one provider call — the point of
// the cache, at a host that counts requests.
func TestAlbumTracklistCachedWithinTTL(t *testing.T) {
	svc, prov, snap := tracklistService(t, DefaultAlbumTracklistCacheTTL)
	req := TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12}

	for i := 0; i < 2; i++ {
		tl, err := svc.albumTracklist(context.Background(), snap, req)
		if err != nil || len(tl) != 1 {
			t.Fatalf("read %d: (%v, %v)", i, tl, err)
		}
	}
	if got := prov.count(); got != 1 {
		t.Errorf("made %d provider calls, want 1", got)
	}
}

// A zero TTL disables the cache entirely, with no change in behavior — the property
// listCache promises, asserted rather than assumed.
func TestAlbumTracklistZeroTTLDisablesTheCache(t *testing.T) {
	svc, prov, snap := tracklistService(t, 0)
	req := TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12}

	for i := 0; i < 2; i++ {
		if _, err := svc.albumTracklist(context.Background(), snap, req); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := prov.count(); got != 2 {
		t.Errorf("made %d provider calls, want 2", got)
	}
}

// Two albums sharing a release-group but not a track count — a standard edition and
// its deluxe, ripped separately — must not be served each other's tracklist.
func TestAlbumTracklistCacheIsKeyedByTheWholeRequest(t *testing.T) {
	svc, prov, snap := tracklistService(t, DefaultAlbumTracklistCacheTTL)

	for _, count := range []int{12, 15} {
		if _, err := svc.albumTracklist(context.Background(), snap,
			TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: count}); err != nil {
			t.Fatalf("count %d: %v", count, err)
		}
	}
	if got := prov.count(); got != 2 {
		t.Errorf("made %d provider calls, want 2 — the two counts share one entry", got)
	}
}

// A provider swap empties the cache: the entries name the OLD provider's recording
// ids, and pinning a track to an id the new provider never chose is the silent wrong
// answer a cache must not introduce.
func TestAlbumTracklistCacheClearedOnProviderSwap(t *testing.T) {
	svc, prov, snap := tracklistService(t, DefaultAlbumTracklistCacheTTL)
	req := TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12}

	if _, err := svc.albumTracklist(context.Background(), snap, req); err != nil {
		t.Fatalf("first read: %v", err)
	}
	svc.SetProvider(prov, Enablement{Music: true})
	if _, err := svc.albumTracklist(context.Background(), snap, req); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if got := prov.count(); got != 2 {
		t.Errorf("made %d provider calls, want 2 — the swap must not leave stale entries", got)
	}
}

// Music enrichment switched off makes no outbound call at all (ADR-0001), and the
// album simply has no tracklist.
func TestAlbumTracklistDisabledMakesNoCall(t *testing.T) {
	svc, prov, _ := tracklistService(t, DefaultAlbumTracklistCacheTTL)
	snap := providerSnapshot{provider: prov, enablement: Enablement{Music: false}}

	tl, err := svc.albumTracklist(context.Background(), snap,
		TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12})
	if tl != nil || !errors.Is(err, ErrNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, ErrNoTracklist)", tl, err)
	}
	if got := prov.count(); got != 0 {
		t.Errorf("made %d provider calls with music enrichment off, want 0", got)
	}
}

// A provider that cannot list a tracklist is "no tracklist", not a crash and not an
// empty list — the same graceful degradation EpisodeLister takes.
func TestAlbumTracklistUnsupportedProviderIsNoTracklist(t *testing.T) {
	svc := NewService(nil, fakeProvider{}, nil, Enablement{Music: true}, "", 0)
	snap := providerSnapshot{provider: fakeProvider{}, enablement: Enablement{Music: true}}

	tl, err := svc.albumTracklist(context.Background(), snap,
		TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12})
	if tl != nil || !errors.Is(err, ErrNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, ErrNoTracklist)", tl, err)
	}
}

// The capability has to survive the composition the server actually wires — a
// CompositeProvider over a MusicChainProvider over the music lead — or the pass
// gets "no tracklist" for every album in production while every provider test
// passes.
func TestAlbumTracklistForwardsThroughTheComposedProvider(t *testing.T) {
	mb, _ := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	prov := CompositeProvider{Music: NewMusicChainProvider(mb, nil, nil)}

	lister, ok := any(prov).(AlbumTracklister)
	if !ok {
		t.Fatal("the composed provider does not implement AlbumTracklister")
	}
	tl, err := lister.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist through the composition: %v", err)
	}
	wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
}

// A composition with no music provider degrades to "no tracklist" rather than
// panicking on a nil sub-provider.
func TestAlbumTracklistWithNoMusicProviderIsNoTracklist(t *testing.T) {
	tl, err := CompositeProvider{}.AlbumTracklist(context.Background(),
		TracklistRequest{ReleaseGroupID: "rg-she"})
	if tl != nil || !errors.Is(err, ErrNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, ErrNoTracklist)", tl, err)
	}
}

// --- the tiering, as the caller sees it ----------------------------------------
//
// These four are the host's stake in ADR-0050: the REQUEST it builds decides which
// edition answers, so the distinctions it draws — a file's release versus a
// human's, a count it knows versus one it does not — have to reach the source and
// come back meaning what they meant. The request shapes themselves are asserted in
// the plugin; what is asserted here is that the service asks the right question.

func TestAlbumTracklistUsesTheReleaseTheFilesName(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}},
		stubRelease{ID: "rel-deluxe", Date: "2011-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("deluxe", 5)}},
	)

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-std", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Fatalf("made %d calls, want 1: %v", len(reqs), reqs)
	}
}

func TestAlbumTracklistIgnoresAReleaseOfAnotherReleaseGroup(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-stranger", Date: "1999-01-01", RGID: "rg-somebody-else",
			Discs: [][]stubTrack{disc("stranger", 8)}},
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she",
			Discs: [][]stubTrack{disc("std", 3)}},
	)

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-stranger", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
	if reqs := stub.requests(); len(reqs) != 2 {
		t.Fatalf("made %d calls, want 2 (the rejected lookup, then the fit browse): %v", len(reqs), reqs)
	}
}

// A refused browse is NOT "this album has no tracklist": a caller has to be able to
// tell a host shedding load apart from a settled nothing (ADR-0049) instead of
// filing a 503 as a fact about the album.
func TestAlbumTracklistProviderErrorIsNotAnEmptyTracklist(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release", errShed)

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 3,
	})
	if tl != nil {
		t.Errorf("got a tracklist %v, want none", gotTitles(tl))
	}
	if err == nil {
		t.Fatal("a refused browse must not read as an album with no tracks")
	}
	if errors.Is(err, ErrNoTracklist) {
		t.Error("a transient refusal must stay distinguishable from a settled 'there is nothing here'")
	}
	if !IsTransient(err) {
		t.Error("a shed browse must be retryable, or the album parks on an outage")
	}
}

// An album that resolved to no release-group cannot have a tracklist, and finding
// that out must not cost a call.
func TestAlbumTracklistWithoutAReleaseGroupMakesNoRequest(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseID: "rel-std", LocalTrackCount: 3,
	})
	if tl != nil || !errors.Is(err, ErrNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, ErrNoTracklist)", gotTitles(tl), err)
	}
	if reqs := stub.requests(); len(reqs) != 0 {
		t.Errorf("made %d calls for an unresolved album, want 0: %v", len(reqs), reqs)
	}
}
