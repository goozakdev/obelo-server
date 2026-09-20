package musicbrainz

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk/sdktest"
)

// ADR-0050's tracklist tier at the provider seam: an Album resolving the ordered
// tracks of the release it ACTUALLY is, rather than of whichever release
// MusicBrainz listed first. Ported from internal/enrich/album_tracklist_test.go
// and the provider half of chosen_edition_test.go, canned JSON and all.

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
	for di, d := range r.Discs {
		tracks := make([]any, 0, len(d))
		for ti, tr := range d {
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

// tracklistStub is a canned MusicBrainz: releases addressable by id (the lookup path)
// and by release-group (the browse path), a per-path forced status, and a record of
// every request URL so a test can assert what the call actually COST.
type tracklistStub struct {
	mu     sync.Mutex
	calls  []string
	byID   map[string]stubRelease
	byRG   map[string][]stubRelease
	status map[string]int
}

func newTracklistStub(t *testing.T, releases ...stubRelease) (*Provider, *tracklistStub) {
	t.Helper()
	s := &tracklistStub{byID: map[string]stubRelease{}, byRG: map[string][]stubRelease{}, status: map[string]int{}}
	for _, r := range releases {
		s.byID[r.ID] = r
		s.byRG[r.RGID] = append(s.byRG[r.RGID], r)
	}
	p, _ := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.URL.RequestURI())
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
	}, noPacing())
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
func gotTitles(tl []pluginapi.TrackCandidate) []string {
	out := make([]string, 0, len(tl))
	for _, t := range tl {
		out = append(out, fmt.Sprintf("%d/%d %s", t.Disc, t.Position, t.Title))
	}
	return out
}

func wantTitles(t *testing.T, tl []pluginapi.TrackCandidate, want ...string) {
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{
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
	newStub := func(t *testing.T) *Provider {
		p, _ := newTracklistStub(t,
			stubRelease{ID: "rel-deluxe", Date: "1994-01-01", RGID: "rg-she",
				Discs: [][]stubTrack{disc("deluxe", 15)}},
			stubRelease{ID: "rel-std", Date: "2001-01-01", RGID: "rg-she",
				Discs: [][]stubTrack{disc("std", 12)}},
		)
		return p
	}

	t.Run("a 12-track local album gets the 12-track release", func(t *testing.T) {
		tl, err := tracklist(newStub(t), pluginapi.TracklistRequest{
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
		tl, err := tracklist(newStub(t), pluginapi.TracklistRequest{
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 3})
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
			tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: count})
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
	if _, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 15}); err != nil {
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 3})
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 4})
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

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 12})
	if tl != nil {
		t.Errorf("got a tracklist %v, want none", gotTitles(tl))
	}
	if !errors.Is(err, errNoTracklist) {
		t.Fatalf("err = %v, want the no-tracklist answer", err)
	}
}

// A provider refusal is also "no tracklist", never an empty one — but it is NOT
// no-match, so a caller can still tell a host shedding load apart from a settled
// nothing (ADR-0049) instead of filing a 503 as a fact about the album.
//
// AND IT IS NOT A GO ERROR EITHER (ADR-0059 decision 6): a 503 is an outage, so the
// answer is `unavailable`, the item takes ADR-0048's backoff, and no failure is
// counted against the plugin.
func TestAlbumTracklistProviderErrorIsNotAnEmptyTracklist(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release", http.StatusServiceUnavailable)

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 3})
	if tl != nil {
		t.Errorf("got a tracklist %v, want none", gotTitles(tl))
	}
	if errors.Is(err, errNoTracklist) {
		t.Error("a transient refusal must stay distinguishable from a settled 'there is nothing here'")
	}
	assertUnavailable(t, err, "a refused browse")
}

// A refusal on the NAMED release is handed straight back rather than retried as a
// fit browse: against a host shedding load, a second request issued precisely during
// a failure is the wrong direction to push it (ADR-0049), and the browse would fail
// the same way.
func TestAlbumTracklistNamedReleaseRefusalIsNotRetriedAsAFitBrowse(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release/rel-std", http.StatusServiceUnavailable)

	_, err := tracklist(p, pluginapi.TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-std", LocalTrackCount: 3,
	})
	assertUnavailable(t, err, "a refused named release")
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — a shedding host must not be asked twice: %v", len(reqs), reqs)
	}
}

// An album that resolved to no release-group cannot have a tracklist, and finding
// that out must not cost a request.
func TestAlbumTracklistWithoutAReleaseGroupMakesNoRequest(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})

	tl, err := tracklist(p, pluginapi.TracklistRequest{ReleaseID: "rel-std", LocalTrackCount: 3})
	if tl != nil || !errors.Is(err, errNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, no-tracklist)", gotTitles(tl), err)
	}
	if reqs := stub.requests(); len(reqs) != 0 {
		t.Errorf("made %d requests for an unresolved album, want 0: %v", len(reqs), reqs)
	}
}

// --- 6. a CHOSEN edition (ADR-0052) --------------------------------------------

// A human's chosen release that belongs to another album stops here rather than
// falling through to a fit, because the caller could not tell the fit apart from
// the edition the human asserted — and that distinction is the whole licence
// ADR-0052 grants position-alone mapping.
func TestAChosenStrangerReleaseIsNotSilentlySwappedForAFit(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-stranger", Date: "1999-01-01", RGID: "rg-somebody-else",
			Discs: [][]stubTrack{disc("stranger", 8)}},
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she",
			Discs: [][]stubTrack{disc("std", 3)}},
	)

	tl, err := tracklist(p, pluginapi.TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-stranger", ReleaseIDChosen: true, LocalTrackCount: 3,
	})
	if tl != nil || !errors.Is(err, errNoTracklist) {
		t.Fatalf("got (%v, %v), want (nil, no-tracklist) — a human's edition that does not "+
			"apply must be reported, not replaced", gotTitles(tl), err)
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — there is nothing to fall through to: %v", len(reqs), reqs)
	}
}

// The same release, NOT chosen by a human, falls through silently — which is the
// pair that makes the flag mean something.
func TestATaggedStrangerReleaseFallsThroughToAFit(t *testing.T) {
	p, _ := newTracklistStub(t,
		stubRelease{ID: "rel-stranger", Date: "1999-01-01", RGID: "rg-somebody-else",
			Discs: [][]stubTrack{disc("stranger", 8)}},
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she",
			Discs: [][]stubTrack{disc("std", 3)}},
	)

	tl, err := tracklist(p, pluginapi.TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-stranger", LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
}

// A chosen edition that IS this album's wins over the fit, even when it does not
// fit: the human said which edition.
func TestTracklistPrefersTheChosenEditionOverFit(t *testing.T) {
	p, _ := newTracklistStub(t,
		stubRelease{ID: "rel-deluxe", Date: "2011-01-01", RGID: "rg-she",
			Discs: [][]stubTrack{disc("deluxe", 5)}},
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she",
			Discs: [][]stubTrack{disc("std", 3)}},
	)

	tl, err := tracklist(p, pluginapi.TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-deluxe", ReleaseIDChosen: true, LocalTrackCount: 3,
	})
	if err != nil {
		t.Fatalf("AlbumTracklist: %v", err)
	}
	if len(tl) != 5 || !strings.HasPrefix(tl[0].Title, "deluxe") {
		t.Fatalf("got %v, want the human's 5-track deluxe even though the local album has 3",
			gotTitles(tl))
	}
}

// --- 7. the editions list (ADR-0052) -------------------------------------------

// ReleaseGroupEditions reads the browse fit-selection already pays for: the same
// call, projected onto the five facts an Admin needs to tell two editions apart.
func TestReleaseGroupEditionsReadsTheBrowseFitSelectionAlreadyPaysFor(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she", Country: "GB",
			Format: "CD", Disamb: "original", Discs: [][]stubTrack{disc("std", 3)}},
		stubRelease{ID: "rel-deluxe", Date: "2011-01-01", RGID: "rg-she", Country: "US",
			Format: "CD", Discs: [][]stubTrack{disc("d1", 3), disc("d2", 2)}},
	)

	eds, err := editions(p, "rg-she")
	if err != nil {
		t.Fatalf("ReleaseGroupEditions: %v", err)
	}
	if len(eds) != 2 {
		t.Fatalf("editions = %d, want 2: %+v", len(eds), eds)
	}
	if eds[0].ReleaseID != "rel-std" || eds[0].Country != "GB" || eds[0].Format != "CD" ||
		eds[0].TrackCount != 3 || eds[0].Disambiguation != "original" {
		t.Errorf("edition[0] = %+v", eds[0])
	}
	// A multi-disc set collapses its format to a count and totals its tracks.
	if eds[1].Format != "2×CD" || eds[1].TrackCount != 5 {
		t.Errorf("edition[1] = %+v, want 2×CD with 5 tracks", eds[1])
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want the one browse: %v", len(reqs), reqs)
	}
}

// An unknown release-group and a release-group with no releases are both "there is
// nothing to choose from", which the picker renders — not a failure it reports.
func TestAnUnknownReleaseGroupListsNoEditions(t *testing.T) {
	p, _ := newTracklistStub(t)
	eds, err := editions(p, "rg-nothing")
	if err != nil || len(eds) != 0 {
		t.Errorf("got (%+v, %v), want no editions and no error", eds, err)
	}
	// And a blank release-group costs no request at all.
	p2, stub := newTracklistStub(t)
	if eds, err := editions(p2, "  "); err != nil || len(eds) != 0 {
		t.Errorf("blank release-group = (%+v, %v)", eds, err)
	}
	if reqs := stub.requests(); len(reqs) != 0 {
		t.Errorf("a blank release-group cost %d requests: %v", len(reqs), reqs)
	}
}

// A REFUSED browse is the outage answer, not an empty edition list: the picker must
// not tell an Admin their album has no editions because MusicBrainz was busy.
func TestARefusedEditionBrowseIsReportedAsAFailure(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	stub.fail("/release", http.StatusServiceUnavailable)

	_, err := editions(p, "rg-she")
	assertUnavailable(t, err, "a refused edition browse")
}

// The browse asks for the hundred editions releaseBrowseLimit allows, in one call.
func TestTheEditionBrowseAsksForItsWholeLimit(t *testing.T) {
	p, stub := newTracklistStub(t,
		stubRelease{ID: "rel-std", Date: "1994-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}})
	if _, err := editions(p, "rg-she"); err != nil {
		t.Fatalf("ReleaseGroupEditions: %v", err)
	}
	reqs := stub.requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0], "limit=100") {
		t.Errorf("browse = %v, want one call with limit=100", reqs)
	}
}

// pickEditionByFit's own table, over the wire type both this plugin and the host's
// picker read. It is the rule two implementations share on purpose (see
// pickReleaseByFit), so it is worth stating directly.
func TestPickEditionByFit(t *testing.T) {
	eds := []pluginapi.ReleaseEdition{
		{ReleaseID: "b", Date: "2011-01-01", TrackCount: 12},
		{ReleaseID: "a", Date: "1994-01-01", TrackCount: 15},
		{ReleaseID: "empty", Date: "1990-01-01", TrackCount: 0},
		{ReleaseID: "undated", TrackCount: 12},
	}
	for _, c := range []struct {
		name  string
		count int
		want  string
	}{
		{"an exact fit wins over an earlier date", 12, "b"},
		{"the other exact fit", 15, "a"},
		{"no fit takes the earliest", 7, "a"},
		{"an unknown count takes the earliest", 0, "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			i := pickEditionByFit(eds, c.count)
			if i < 0 || eds[i].ReleaseID != c.want {
				t.Errorf("picked %d (%+v), want %q", i, eds, c.want)
			}
		})
	}
	// An edition with no tracks is never chosen: it cannot be anyone's tracklist.
	if i := pickEditionByFit([]pluginapi.ReleaseEdition{{ReleaseID: "empty"}}, 0); i != -1 {
		t.Errorf("picked an empty edition (%d)", i)
	}
	if i := pickEditionByFit(nil, 3); i != -1 {
		t.Errorf("picked from nothing (%d)", i)
	}
}

// A guard the port needs and the Go provider did not: the browse's release list and
// the edition view it is projected onto must stay INDEX FOR INDEX, because
// pickReleaseByFit maps the chosen index straight back.
func TestEditionsAreProjectedIndexForIndex(t *testing.T) {
	rels := []mbRelease{
		{ID: "one", Media: []mbMedium{{Tracks: []mbTrack{{Title: "a"}}}}},
		{ID: "two"}, // no media at all — it must still occupy its index
		{ID: "three", Media: []mbMedium{{Tracks: []mbTrack{{Title: "b"}, {Title: "c"}}}}},
	}
	eds := mbEditions(rels)
	if len(eds) != len(rels) {
		t.Fatalf("projected %d editions from %d releases", len(eds), len(rels))
	}
	for i := range rels {
		if eds[i].ReleaseID != rels[i].ID {
			t.Fatalf("edition[%d] is %q, want %q", i, eds[i].ReleaseID, rels[i].ID)
		}
	}
}

// requestCount is a small readability helper for the pacer tests below.
func requestCount(h *sdktest.Host) int { return len(h.Requests()) }
