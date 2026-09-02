package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ADR-0050's tracklist tier, at the provider seam: an Album resolving the ordered
// tracks of the release it ACTUALLY is, rather than of whichever release
// MusicBrainz listed first. Everything here runs against an httptest server serving
// canned MusicBrainz JSON; no live network is touched.

// --- the stub MusicBrainz -----------------------------------------------------

// stubTrack is one track of a canned release. Rec is the recording MBID; "" is the
// real case where MusicBrainz has the track but no recording behind it.
type stubTrack struct {
	Title string
	Rec   string
}

// stubRelease is one canned edition: its id, its date, its parent release-group,
// and its tracks per disc — plus the three descriptive fields the EDITION PICKER
// reads off the same browse (ADR-0052): country, medium format, and MusicBrainz's
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

func (r stubRelease) body() map[string]any {
	media := make([]any, 0, len(r.Discs))
	for di, disc := range r.Discs {
		tracks := make([]any, 0, len(disc))
		for ti, tr := range disc {
			rec := map[string]any{}
			if tr.Rec != "" {
				rec["id"] = tr.Rec
			}
			tracks = append(tracks, map[string]any{
				"position":  ti + 1,
				"number":    fmt.Sprint(ti + 1),
				"title":     tr.Title,
				"recording": rec,
			})
		}
		media = append(media, map[string]any{"position": di + 1, "format": r.Format, "tracks": tracks})
	}
	return map[string]any{
		"id":             r.ID,
		"date":           r.Date,
		"country":        r.Country,
		"disambiguation": r.Disamb,
		"media":          media,
		"release-group":  map[string]any{"id": r.RGID},
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

// tracklistStub is a canned MusicBrainz: releases addressable by id (the lookup path) and
// by release-group (the browse path), a per-path forced status, and a record of
// every request URL so a test can assert what the call actually COST.
type tracklistStub struct {
	mu     sync.Mutex
	calls  []string
	byID   map[string]stubRelease
	byRG   map[string][]stubRelease
	status map[string]int
}

func newTracklistStub(t *testing.T, releases ...stubRelease) (*MusicBrainzProvider, *tracklistStub) {
	t.Helper()
	s := &tracklistStub{byID: map[string]stubRelease{}, byRG: map[string][]stubRelease{}, status: map[string]int{}}
	for _, r := range releases {
		s.byID[r.ID] = r
		s.byRG[r.RGID] = append(s.byRG[r.RGID], r)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.URL.String())
		code := s.status[r.URL.Path]
		s.mu.Unlock()
		if code != 0 {
			// The real global-shed shape (ADR-0049): a 503 the client must NOT sit and
			// retry, so one refused request stays one request in these tests.
			w.Header().Set("x-ratelimit-who", "search-shed")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"The MusicBrainz web server is currently busy. Please try again later."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if id := strings.TrimPrefix(r.URL.Path, "/release/"); id != r.URL.Path {
			rel, ok := s.byID[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(rel.body())
			return
		}
		if r.URL.Path == "/release" {
			var out []any
			for _, rel := range s.byRG[r.URL.Query().Get("release-group")] {
				out = append(out, rel.body())
			}
			if lim := r.URL.Query().Get("limit"); lim == "1" && len(out) > 1 {
				out = out[:1] // the candidate-preview path asks for exactly one
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"releases": out})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0 // no throttling: these tests measure calls, not seconds
	return p, s
}

func (s *tracklistStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *tracklistStub) fail(path string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[path] = code
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

// --- 1. the release the files name -------------------------------------------

// The headline case: the Album's files carry a release MBID, so the tracklist comes
// from THAT edition, in ONE call — the same call that proves the edition is the
// album's. The release-group also holds a decoy deluxe, which must not be consulted.
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
	if tl[0].ExternalID != "rec-std-1" {
		t.Errorf("recording id = %q, want rec-std-1 — the id is the whole point of the tier", tl[0].ExternalID)
	}

	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1: %v", len(reqs), reqs)
	}
	if !strings.HasPrefix(reqs[0], "/release/rel-std?") {
		t.Errorf("asked %q, want the named release by id", reqs[0])
	}
	// One call answers the tracklist AND the parentage, so the parentage check is
	// free. The spelling matters: MusicBrainz reads a multi-valued inc as
	// "recordings+release-groups", which on the wire is a '+' separator — what
	// url.Values produces from a SPACE. A literal '+' in the value would be
	// percent-encoded to %2B and reach the service as one nonsense inc name.
	if !strings.Contains(reqs[0], "inc=recordings+release-groups") {
		t.Errorf("request %q must ask for recordings AND release-groups in one call", reqs[0])
	}
	if strings.Contains(reqs[0], "%2B") {
		t.Errorf("request %q escaped the inc separator; MusicBrainz wants a bare '+'", reqs[0])
	}
}

// --- 2. a stranger's release is ignored ---------------------------------------

// A retagged or mis-tagged file naming a release of some OTHER album must not
// renumber this one: the parent release-group is checked, the stranger discarded,
// and the album falls back to fit-selection.
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
	for _, tc := range tl {
		if strings.HasPrefix(tc.Title, "stranger") {
			t.Fatalf("the stranger's release renumbered the album: %v", gotTitles(tl))
		}
	}
	if reqs := stub.requests(); len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (the rejected lookup, then the fit browse): %v", len(reqs), reqs)
	}
}

// A stale or simply wrong release id 404s. That says nothing about whether the
// ALBUM has a tracklist, so it falls through to fit-selection rather than settling.
func TestAlbumTracklistUnknownReleaseIDFallsBackToFit(t *testing.T) {
	p, _ := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}},
	)

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "00000000-0000-0000-0000-000000000000", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
}

// --- 3. fit-selection ----------------------------------------------------------

// The deluxe-edition case ADR-0050 is about: one release-group, a 12-track standard
// and a 15-track deluxe, and the LOCAL track count decides. The deluxe is listed
// first AND dated earlier, so neither "whichever came back first" nor "the earliest"
// can pass this by accident.
func TestAlbumTracklistChoosesTheReleaseThatFitsTheLocalAlbum(t *testing.T) {
	newStub := func(t *testing.T) *MusicBrainzProvider {
		p, _ := newTracklistStub(t,
			stubRelease{ID: "rel-deluxe", Date: "1994-01-01", RGID: "rg-she",
				Discs: [][]stubTrack{disc("deluxe", 15)}},
			stubRelease{ID: "rel-std", Date: "2001-01-01", RGID: "rg-she",
				Discs: [][]stubTrack{disc("std", 12)}},
		)
		return p
	}

	t.Run("a 12-track local album gets the 12-track release", func(t *testing.T) {
		tl, err := newStub(t).AlbumTracklist(context.Background(), TracklistRequest{
			ReleaseGroupID: "rg-she", LocalTrackCount: 12,
		})
		if err != nil {
			t.Fatalf("AlbumTracklist: %v", err)
		}
		if len(tl) != 12 || !strings.HasPrefix(tl[0].Title, "std") {
			t.Fatalf("got %d tracks starting %q, want the 12-track standard", len(tl), tl[0].Title)
		}
	})

	t.Run("a 15-track local album gets the 15-track release", func(t *testing.T) {
		tl, err := newStub(t).AlbumTracklist(context.Background(), TracklistRequest{
			ReleaseGroupID: "rg-she", LocalTrackCount: 15,
		})
		if err != nil {
			t.Fatalf("AlbumTracklist: %v", err)
		}
		if len(tl) != 15 || !strings.HasPrefix(tl[0].Title, "deluxe") {
			t.Fatalf("got %d tracks starting %q, want the 15-track deluxe", len(tl), tl[0].Title)
		}
	})
}

// Two editions fit equally well: the earliest wins. The later one is listed first,
// so source order cannot be what decided.
func TestAlbumTracklistBreaksFitTiesOnTheEarliestDate(t *testing.T) {
	p, _ := newTracklistStub(t,
		stubRelease{ID: "rel-reissue", Date: "2011-01-01", RGID: "rg-she",
			Discs: [][]stubTrack{disc("reissue", 3)}},
		stubRelease{ID: "rel-original", Date: "1994-06-21", RGID: "rg-she",
			Discs: [][]stubTrack{disc("original", 3)}},
	)

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	if !strings.HasPrefix(tl[0].Title, "original") {
		t.Errorf("picked %q, want the earliest of the two equally-fitting editions", tl[0].Title)
	}
}

// Nothing fits (a local album whose count matches no edition, or one whose count is
// unknown): fall back to the earliest release rather than to an arbitrary one.
func TestAlbumTracklistWithNoFitTakesTheEarliestRelease(t *testing.T) {
	for _, count := range []int{13, 0} {
		t.Run(fmt.Sprintf("local count %d", count), func(t *testing.T) {
			p, _ := newTracklistStub(t,
				stubRelease{ID: "rel-reissue", Date: "2011-01-01", RGID: "rg-she",
					Discs: [][]stubTrack{disc("reissue", 12)}},
				stubRelease{ID: "rel-original", Date: "1994-06-21", RGID: "rg-she",
					Discs: [][]stubTrack{disc("original", 15)}},
			)
			tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
				ReleaseGroupID: "rg-she", LocalTrackCount: count,
			})
			if err != nil {
				t.Fatalf("AlbumTracklist: %v", err)
			}
			if !strings.HasPrefix(tl[0].Title, "original") {
				t.Errorf("picked %q, want the earliest release", tl[0].Title)
			}
		})
	}
}

// Fit-selection is ONE call: the browse carries the tracks, so comparing counts and
// returning the winner never costs a second round-trip at a rate-limited host.
func TestAlbumTracklistFitSelectionCostsOneCall(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-a", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("a", 12)}},
		stubRelease{ID: "rel-b", Date: "2001-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("b", 15)}},
	)
	if _, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 15,
	}); err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1: %v", len(reqs), reqs)
	}
	if !strings.Contains(reqs[0], "inc=recordings") {
		t.Errorf("browse %q must carry the recordings, or the winner needs a second call", reqs[0])
	}
}

// --- 4. what a tracklist entry carries ----------------------------------------

// A tracklist entry the source gave no recording id KEEPS its position. It can never
// be pinned, but it still occupies the slot, which is what the caller's leftover
// rule needs to know.
func TestAlbumTracklistKeepsAPositionWithNoRecordingID(t *testing.T) {
	p, _ := newTracklistStub(t, stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she",
		Discs: [][]stubTrack{{
			{Title: "One", Rec: "rec-1"},
			{Title: "Untraced"},
			{Title: "Three", Rec: "rec-3"},
		}}})

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 One", "1/2 Untraced", "1/3 Three")
	if tl[1].ExternalID != "" {
		t.Errorf("entry 2 external id = %q, want empty", tl[1].ExternalID)
	}
	if tl[2].ExternalID != "rec-3" {
		t.Errorf("entry 3 lost its recording id (%q) — the untraceable entry shifted the rest", tl[2].ExternalID)
	}
}

// A multi-disc release keeps its disc numbers, so (disc, track) stays addressable.
func TestAlbumTracklistNumbersItsDiscs(t *testing.T) {
	p, _ := newTracklistStub(t, stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she",
		Discs: [][]stubTrack{disc("d1", 2), disc("d2", 2)}})

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 4,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 d1 1", "1/2 d1 2", "2/1 d2 1", "2/2 d2 2")
}

// --- 5. "no tracklist" is never an empty tracklist ------------------------------

// A release-group with no releases has no tracklist, and says so — the caller has to
// be able to tell "this album has no tracklist" from "this album's tracklist has no
// room for this track", which are different reasons pointing at different fixes.
func TestAlbumTracklistEmptyReleaseGroupIsNoTracklist(t *testing.T) {
	p, _ := newTracklistStub(t) // nothing at all under rg-she

	tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", LocalTrackCount: 12,
	})
	if tl != nil {
		t.Errorf("got a tracklist %v, want none", gotTitles(tl))
	}
	if !errors.Is(err, ErrNoTracklist) {
		t.Fatalf("err = %v, want ErrNoTracklist", err)
	}
}

// A provider refusal is also "no tracklist", never an empty one — but it is NOT
// ErrNoTracklist, so a caller can still tell a host shedding load apart from a
// settled nothing (ADR-0049) instead of filing a 503 as a fact about the album.
func TestAlbumTracklistProviderErrorIsNotAnEmptyTracklist(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release", http.StatusServiceUnavailable)

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
}

// A refusal on the NAMED release is handed straight back rather than retried as a
// fit browse: against a host shedding load, a second request issued precisely during
// a failure is the wrong direction to push it (ADR-0049), and the browse would fail
// the same way.
func TestAlbumTracklistNamedReleaseRefusalIsNotRetriedAsAFitBrowse(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release/rel-std", http.StatusServiceUnavailable)

	if _, err := p.AlbumTracklist(context.Background(), TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-std", LocalTrackCount: 3,
	}); err == nil {
		t.Fatal("want the refusal back")
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — a shedding host must not be asked twice: %v", len(reqs), reqs)
	}
}

// An album that resolved to no release-group cannot have a tracklist, and finding
// that out must not cost a request.
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
		t.Errorf("made %d requests for an unresolved album, want 0: %v", len(reqs), reqs)
	}
}

// --- 6. the cache --------------------------------------------------------------

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
// CompositeProvider over a MusicChainProvider over MusicBrainz — or the pass gets
// "no tracklist" for every album in production while every provider test passes.
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

// --- 7. the candidate preview is left alone ------------------------------------

// The search-results page must keep paying ONE call per album candidate. The new
// tier is a second question with a second answer; making the preview ask it would
// double every page of album search results at a rate-limited host.
func TestAlbumCandidatePreviewStillCostsOneCallPerCandidate(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-a1", Date: "1994-01-01", RGID: "rg-a", Discs: [][]stubTrack{disc("a", 3)}},
		stubRelease{ID: "rel-a2", Date: "2011-01-01", RGID: "rg-a", Discs: [][]stubTrack{disc("a-deluxe", 5)}},
	)
	// searchReleaseGroups hits /release-group first; the stub 404s that, so drive the
	// preview directly — it is the per-candidate cost that is under test.
	tl, err := p.releaseGroupTracklist(context.Background(), "rg-a")
	if err != nil {
		t.Fatalf("releaseGroupTracklist: %v", err)
	}
	if len(tl) != 3 {
		t.Errorf("preview returned %d tracks, want the first release's 3", len(tl))
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("preview made %d requests, want 1: %v", len(reqs), reqs)
	}
	if !strings.Contains(reqs[0], "limit=1") {
		t.Errorf("preview asked %q, want the cheap limit=1 shape", reqs[0])
	}
	if strings.Contains(reqs[0], "release-groups") {
		t.Errorf("preview asked %q — it must not pay for the parentage it does not check", reqs[0])
	}
}
