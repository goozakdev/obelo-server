package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0053: "An Album corroborates its Artist."
//
// The bug this file exists for was measured, not imagined. The Artist row for "The
// Eagles" in a real library held a4852e21-7f09-470b-b5ae-9740d939d183, which
// MusicBrainz calls "The Eagles — 1960s UK instrumental group", British, formed
// 1958. The American band is named "Eagles". artistDetails searched
// artist:"The Eagles" as an exact phrase, took Artists[0], and stored it with no
// acceptance test of any kind — so the row read `matched`, nothing ever flagged it,
// and the damage surfaced as thirteen unmatched tracks on Hell Freezes Over three
// levels down.
//
// EVERY REPAIR BUILT OUT OF THE NAME FAILS, and the fixtures below are chosen so
// that a future repair of that shape fails HERE rather than in someone's library:
// the stub holds an artist literally named "The Eagles", spelled exactly as the
// local row spells it. Stripping the article finds "Eagles" and leaves "The Eagles"
// still matching exactly and still winning; a name acceptance test accepts
// identical names; an exact match scores 100. The two bands are told apart by their
// discographies, and the library is holding one of those albums.

// --- the fixture --------------------------------------------------------------

const (
	// britishEaglesMBID is the REAL id the motivating library stored, and the whole
	// point of this file is that it must stop being the answer.
	britishEaglesMBID = "a4852e21-7f09-470b-b5ae-9740d939d183"
	// americanEaglesMBID stands for the band that actually recorded the album. A
	// fixture UUID: what matters is that it is a DIFFERENT artist, reached only
	// through the album.
	americanEaglesMBID = "11111111-2222-3333-4444-555555555555"
	// hellFreezesOverRGID is the release-group the files assert for the album.
	hellFreezesOverRGID = "66666666-7777-8888-9999-aaaaaaaaaaaa"
)

// stubArtist is one MusicBrainz artist as both the name search and /artist/<id>
// return it.
type stubArtist struct {
	id, name, kind, disambiguation, area string
	tags                                 []string
}

// stubReleaseGroup is one MusicBrainz release-group, credited to an artist by id —
// which is the only fact corroboration reads.
type stubReleaseGroup struct {
	id, title, artistID, artistName string
}

// mbArtistStub is a MusicBrainz that behaves like the real one on the four
// endpoints an Artist resolution can touch, and records the exact wire query of
// every request it is sent.
//
//	GET /artist?query=artist:"X"   the NAME search — an exact phrase, so it matches
//	                               a name spelled exactly X and nothing else
//	GET /artist/<id>               the artist lookup
//	GET /release-group?query=…     the album SEARCH — relevance terms, so a title
//	                               containing them comes back, in fixture order
//	GET /release-group/<id>        the album lookup, carrying its artist-credit
type mbArtistStub struct {
	mu   sync.Mutex
	wire []string // "<path>?<query>", in order

	artists []stubArtist
	groups  []stubReleaseGroup // fixture order IS relevance order
	status  map[string]int     // path → a status to answer with instead (a 503 shed)
}

func (s *mbArtistStub) note(path, query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if query == "" {
		s.wire = append(s.wire, path)
		return
	}
	s.wire = append(s.wire, path+"?"+query)
}

// calls returns every request the provider made, in order, as "<path>?<query>".
func (s *mbArtistStub) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.wire...)
}

// count returns how many recorded calls start with prefix.
func (s *mbArtistStub) count(prefix string) int {
	n := 0
	for _, c := range s.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// nameSearches counts calls to the ARTIST NAME SEARCH — the one endpoint ADR-0053
// is refusing to trust, and the one corroboration must REPLACE rather than join.
func (s *mbArtistStub) nameSearches() int { return s.count("/artist?") }

// searches counts every call to a SEARCH endpoint, as opposed to a lookup. This is
// ADR-0049's currency: the search cluster is the dependency that sheds load
// globally while the lookup endpoints answer normally.
func (s *mbArtistStub) searches() int { return s.count("/artist?") + s.count("/release-group?") }

func (s *mbArtistStub) artistByID(id string) (stubArtist, bool) {
	for _, a := range s.artists {
		if a.id == id {
			return a, true
		}
	}
	return stubArtist{}, false
}

func (s *mbArtistStub) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.note(path, r.URL.RawQuery)
	if code, ok := s.status[path]; ok {
		http.Error(w, `{"error":"The MusicBrainz web server is currently busy. Please try again later."}`, code)
		return
	}
	switch {
	case path == "/artist":
		s.serveArtistSearch(w, r.URL.Query().Get("query"))
	case strings.HasPrefix(path, "/artist/"):
		s.serveArtistLookup(w, strings.TrimPrefix(path, "/artist/"))
	case path == "/release-group":
		s.serveReleaseGroupSearch(w, r.URL.Query().Get("query"))
	case strings.HasPrefix(path, "/release-group/"):
		s.serveReleaseGroupLookup(w, strings.TrimPrefix(path, "/release-group/"))
	default:
		http.NotFound(w, r)
	}
}

// serveArtistSearch answers the exact-phrase name search the way MusicBrainz does:
// artist:"The Eagles" matches an artist NAMED "The Eagles", which is precisely how
// the wrong band won.
func (s *mbArtistStub) serveArtistSearch(w http.ResponseWriter, query string) {
	phrase := ""
	if rest, ok := strings.CutPrefix(query, `artist:"`); ok {
		phrase = strings.TrimSuffix(rest, `"`)
	}
	type area struct {
		Name string `json:"name"`
	}
	type row struct {
		ID             string  `json:"id"`
		Name           string  `json:"name"`
		Type           string  `json:"type"`
		Disambiguation string  `json:"disambiguation"`
		Area           area    `json:"area"`
		Tags           []mbTag `json:"tags"`
	}
	rows := []row{}
	for _, a := range s.artists {
		if a.name != phrase {
			continue
		}
		rows = append(rows, row{ID: a.id, Name: a.name, Type: a.kind,
			Disambiguation: a.disambiguation, Area: area{Name: a.area}, Tags: stubTags(a.tags)})
	}
	writeJSON(w, map[string][]row{"artists": rows})
}

func (s *mbArtistStub) serveArtistLookup(w http.ResponseWriter, id string) {
	a, ok := s.artistByID(id)
	if !ok {
		http.Error(w, `{"error":"Not Found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"id": a.id, "name": a.name, "type": a.kind, "disambiguation": a.disambiguation,
		"area": map[string]string{"name": a.area}, "tags": stubTags(a.tags),
	})
}

// serveReleaseGroupSearch scores rather than filters, the way a relevance query
// does: every release-group whose title contains the terms comes back, in fixture
// order, so the fixture decides which one is the TOP hit.
func (s *mbArtistStub) serveReleaseGroupSearch(w http.ResponseWriter, query string) {
	terms := strings.ToLower(unescapeLucene(query))
	type row struct {
		ID           string     `json:"id"`
		Title        string     `json:"title"`
		ArtistCredit []mbCredit `json:"artist-credit"`
	}
	rows := []row{}
	for _, g := range s.groups {
		if terms == "" || !strings.Contains(strings.ToLower(g.title), terms) {
			continue
		}
		rows = append(rows, row{ID: g.id, Title: g.title, ArtistCredit: stubCredit(g)})
	}
	writeJSON(w, map[string][]row{"release-groups": rows})
}

func (s *mbArtistStub) serveReleaseGroupLookup(w http.ResponseWriter, id string) {
	for _, g := range s.groups {
		if g.id != id {
			continue
		}
		writeJSON(w, map[string]any{"id": g.id, "title": g.title, "artist-credit": stubCredit(g)})
		return
	}
	http.Error(w, `{"error":"Not Found"}`, http.StatusNotFound)
}

func stubCredit(g stubReleaseGroup) []mbCredit {
	c := mbCredit{Name: g.artistName}
	c.Artist.ID = g.artistID
	return []mbCredit{c}
}

func stubTags(names []string) []mbTag {
	out := make([]mbTag, 0, len(names))
	for _, n := range names {
		out = append(out, mbTag{Name: n})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// unescapeLucene undoes escapeLucene, so the stub matches on the TEXT the provider
// meant rather than on its escaping.
func unescapeLucene(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// theEaglesStub is the motivating library, on a wire: both bands exist, only one of
// them recorded the album, and the local Artist's name is spelled exactly like the
// wrong one's.
func theEaglesStub(t *testing.T) (*MusicBrainzProvider, *mbArtistStub) {
	t.Helper()
	stub := &mbArtistStub{
		artists: []stubArtist{
			{id: britishEaglesMBID, name: "The Eagles", kind: "Group",
				disambiguation: "1960s UK instrumental group", area: "United Kingdom"},
			{id: americanEaglesMBID, name: "Eagles", kind: "Group",
				area: "United States", tags: []string{"rock", "country rock"}},
		},
		groups: []stubReleaseGroup{
			{id: hellFreezesOverRGID, title: "Hell Freezes Over",
				artistID: americanEaglesMBID, artistName: "Eagles"},
		},
	}
	return newStubbedProvider(t, stub), stub
}

func newStubbedProvider(t *testing.T, stub *mbArtistStub) *MusicBrainzProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	p := NewMusicBrainzProvider(srv.URL, srv.URL, "en")
	p.MinInterval = 0 // don't throttle the test
	return p
}

// artistRef builds the ref collectMusicLeaves hands the provider for an Artist that
// carries no id of its own.
func artistRef(name string, hints ...AlbumHint) TitleRef {
	return TitleRef{Kind: "artist", Title: name, Artist: name, AlbumHints: hints}
}

// --- the headline: the album decides, not the name ----------------------------

// "An Artist named 'The Eagles' whose library holds Hell Freezes Over resolves to
// the American band, not to a4852e21-…."
//
// The decoy is the assertion. An artist literally named "The Eagles" is sitting in
// the stub, matching the local name exactly; any repair that reaches for the name —
// article stripping, a name acceptance test, a score threshold — resolves to it and
// fails this test.
func TestTheEaglesResolveThroughTheirAlbumNotTheirName(t *testing.T) {
	p, stub := theEaglesStub(t)

	meta, err := p.Lookup(context.Background(),
		artistRef("The Eagles", AlbumHint{Title: "Hell Freezes Over"}))
	if err != nil {
		t.Fatalf("lookup: %v (calls: %v)", err, stub.calls())
	}
	if meta.ExternalID == britishEaglesMBID {
		t.Fatalf("resolved to %s — the 1960s UK instrumental group. That is the exact wrong "+
			"answer ADR-0053 was written from: the local name matches it exactly, so every "+
			"discriminator built out of the NAME accepts it. The album is what tells the two "+
			"bands apart (calls: %v)", meta.ExternalID, stub.calls())
	}
	if !meta.Matched || meta.ExternalID != americanEaglesMBID {
		t.Fatalf("meta = %+v, want the band credited on Hell Freezes Over (%s) (calls: %v)",
			meta, americanEaglesMBID, stub.calls())
	}
	// The metadata is the corroborated artist's, fetched by id — not the decoy's.
	if meta.Name != "Eagles" {
		t.Errorf("Name = %q, want %q — the artist is fetched BY ID for its metadata, so the "+
			"row stops carrying another band's bio and genres", meta.Name, "Eagles")
	}
	if n := stub.nameSearches(); n != 0 {
		t.Errorf("%d artist NAME searches, want 0 — corroboration REPLACES the name search, "+
			"it does not join it (calls: %v)", n, stub.calls())
	}
}

// "An artist whose albums carry tag release-group ids resolves with ZERO search
// calls." The id the files assert is resolved with one lookup, which is ADR-0049's
// lookup-beats-search preference applied one level up — and the search cluster,
// the dependency that sheds load globally, is never touched.
func TestATaggedAlbumCorroboratesItsArtistWithNoSearchAtAll(t *testing.T) {
	p, stub := theEaglesStub(t)

	meta, err := p.Lookup(context.Background(), artistRef("The Eagles",
		AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: hellFreezesOverRGID}))
	if err != nil {
		t.Fatalf("lookup: %v (calls: %v)", err, stub.calls())
	}
	if meta.ExternalID != americanEaglesMBID {
		t.Fatalf("resolved to %q, want %q (calls: %v)", meta.ExternalID, americanEaglesMBID, stub.calls())
	}
	if n := stub.searches(); n != 0 {
		t.Fatalf("%d SEARCH calls, want 0 — the files named the release-group, so the whole "+
			"resolution is lookups (calls: %v)", n, stub.calls())
	}
	want := []string{
		"/release-group/" + hellFreezesOverRGID + "?fmt=json&inc=artist-credits",
		"/artist/" + americanEaglesMBID + "?fmt=json&inc=tags",
	}
	if got := stub.calls(); !equalStrings(got, want) {
		t.Errorf("wire = %v, want %v", got, want)
	}
}

// "The corroborating search is unnarrowed — asserted on the wire, because narrowing
// it is the natural 'improvement' that would silently undo this issue."
//
// AND-ing artist:"The Eagles" into this query can only ever return an album by the
// artist the name already picked, which is the band that never recorded it. The
// query would then find nothing, corroboration would decline, and the name search
// would quietly go back to being the answer — with every test above still passing
// on its own fixtures. So the query itself is the assertion.
func TestTheCorroboratingAlbumSearchIsUnnarrowedOnTheWire(t *testing.T) {
	p, stub := theEaglesStub(t)

	if _, err := p.Lookup(context.Background(),
		artistRef("The Eagles", AlbumHint{Title: "Hell Freezes Over"})); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	var query string
	for _, c := range stub.calls() {
		if strings.HasPrefix(c, "/release-group?") {
			query = c
		}
	}
	if query == "" {
		t.Fatalf("no release-group search on the wire (calls: %v)", stub.calls())
	}
	if strings.Contains(query, "artist%3A") || strings.Contains(query, "artist:") {
		t.Fatalf("the corroborating search is narrowed by the artist: %q\n\n"+
			"That reintroduces the name ADR-0053 exists not to trust. On the motivating "+
			"library `Hell Freezes Over AND artist:\"The Eagles\"` returns NOTHING, because "+
			"the only artist by that name is a 1958 British instrumental group — so the "+
			"narrowed query declines, the artist falls back to its name, and the whole "+
			"mechanism is undone without a single other test failing.", query)
	}
	if want := "/release-group?fmt=json&query=Hell+Freezes+Over"; query != want {
		t.Errorf("query = %q, want %q — the album title, escaped, and nothing else", query, want)
	}
}

// "A corroborating album whose title fails the acceptance test does NOT supply an
// artist; the lookup falls through to the name search."
//
// The top hit here is a different album that merely mentions the one we hold — the
// shape a relevance query returns constantly. normalizeMatchTitle (issue 03's
// function, the same acceptance test issue 05 applies to a track) refuses it, and
// the artist is no worse off than it was before corroboration existed.
func TestACorroboratingAlbumThatFailsTheTitleTestFallsThroughToTheName(t *testing.T) {
	p, stub := theEaglesStub(t)
	stub.groups = []stubReleaseGroup{{
		id:       "cccccccc-dddd-eeee-ffff-000000000000",
		title:    "Hell Freezes Over Tour Souvenir",
		artistID: "99999999-8888-7777-6666-555555555555", artistName: "Somebody Else",
	}}

	meta, err := p.Lookup(context.Background(),
		artistRef("The Eagles", AlbumHint{Title: "Hell Freezes Over"}))
	if err != nil {
		t.Fatalf("lookup: %v (calls: %v)", err, stub.calls())
	}
	if meta.ExternalID == "99999999-8888-7777-6666-555555555555" {
		t.Fatalf("took the artist credited on %q — a different album that merely mentions "+
			"the one we hold. A corroborated Artist is only as good as its corroborating "+
			"Album, and the title test is what bounds that (ADR-0053)", "Hell Freezes Over Tour Souvenir")
	}
	if meta.ExternalID != britishEaglesMBID {
		t.Fatalf("meta = %+v, want the NAME search's answer (%s) — a declined corroboration "+
			"falls through to the last resort rather than guessing (calls: %v)",
			meta, britishEaglesMBID, stub.calls())
	}
	if n := stub.nameSearches(); n != 1 {
		t.Errorf("%d name searches, want exactly 1 — the fall-through is the OLD path, "+
			"unchanged (calls: %v)", n, stub.calls())
	}
}

// "An Artist with no identifiable albums (a soundtrack filed under the film's name,
// 'Unknown Artist') behaves exactly as it does today." Same one request, same
// exact-phrase query, same answer.
func TestAnArtistWithNoIdentifiableAlbumsAsksExactlyWhatItAlwaysDid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hints []AlbumHint
	}{
		{"no albums at all", nil},
		{"albums with neither an id nor a title", []AlbumHint{{}, {Title: "   "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, stub := theEaglesStub(t)

			meta, err := p.Lookup(context.Background(), artistRef("The Eagles", tc.hints...))
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if meta.ExternalID != britishEaglesMBID {
				t.Errorf("meta = %+v, want today's name-search answer unchanged", meta)
			}
			want := []string{`/artist?fmt=json&query=artist%3A%22The+Eagles%22`}
			if got := stub.calls(); !equalStrings(got, want) {
				t.Errorf("wire = %v, want %v — an artist corroboration has nothing to say "+
					"about must cost exactly what it used to", got, want)
			}
		})
	}
}

// A blank name and no albums still never reaches the network.
func TestAnArtistWithNoNameAndNoAlbumsAsksNothing(t *testing.T) {
	p, stub := theEaglesStub(t)

	if _, err := p.Lookup(context.Background(), artistRef("  ")); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	if n := len(stub.calls()); n != 0 {
		t.Fatalf("sent %d requests for a nameless Artist with no albums, want 0 (%v)", n, stub.calls())
	}
}

// "The tag artist MBID still wins over corroboration" (ADR-0049, unchanged and
// still first). A tagged library never pays for corroboration at all.
func TestTheTagArtistMBIDStillWinsOverCorroboration(t *testing.T) {
	p, stub := theEaglesStub(t)
	ref := artistRef("The Eagles", AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: hellFreezesOverRGID})
	ref.MusicbrainzID = americanEaglesMBID

	meta, err := p.Lookup(context.Background(), ref)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if meta.ExternalID != americanEaglesMBID {
		t.Fatalf("meta = %+v, want the id the FILES assert", meta)
	}
	want := []string{"/artist/" + americanEaglesMBID + "?fmt=json&inc=tags"}
	if got := stub.calls(); !equalStrings(got, want) {
		t.Errorf("wire = %v, want %v — an id the files assert resolves by lookup and asks "+
			"nothing else (ADR-0049)", got, want)
	}
}

// "Cost is neutral or better: an artist resolved by corroboration makes no MORE
// provider calls than the name search it replaces."
//
// The unit is the IDENTIFYING call — the one request that decides WHICH artist this
// is. Today that is one artist name search. Corroboration replaces it one-for-one
// with a single release-group request, and never adds one on top; resolving the
// answer to its metadata then goes through artistByID, the same by-id fetch a
// pinned artist has always paid. On the search cluster ADR-0049 measured shedding
// load, the tagged path is strictly cheaper: one search becomes none.
func TestCorroborationReplacesTheNameSearchRatherThanJoiningIt(t *testing.T) {
	cases := []struct {
		name          string
		hint          AlbumHint
		wantIdentify  int // release-group requests: the corroborating call
		wantSearches  int // calls on the search cluster
		wantNameQuery int // artist NAME searches — must be 0 when corroboration answers
	}{
		{"the files name the release-group", AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: hellFreezesOverRGID}, 1, 0, 0},
		{"only the album title is known", AlbumHint{Title: "Hell Freezes Over"}, 1, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, stub := theEaglesStub(t)

			if _, err := p.Lookup(context.Background(), artistRef("The Eagles", tc.hint)); err != nil {
				t.Fatalf("lookup: %v", err)
			}
			identify := stub.count("/release-group")
			if identify != tc.wantIdentify {
				t.Errorf("%d release-group requests, want %d — corroboration makes exactly ONE "+
					"identifying call; walking the hints one after another would cost an artist "+
					"three requests to learn what one already said (calls: %v)",
					identify, tc.wantIdentify, stub.calls())
			}
			if got := stub.searches(); got != tc.wantSearches {
				t.Errorf("%d search-cluster calls, want %d (calls: %v)", got, tc.wantSearches, stub.calls())
			}
			if got := stub.nameSearches(); got != tc.wantNameQuery {
				t.Errorf("%d artist name searches, want %d — the name search is REPLACED, "+
					"never joined (calls: %v)", got, tc.wantNameQuery, stub.calls())
			}
		})
	}
}

// Three hints are carried, but only ONE is spent: the best evidence available wins
// and the rest are never asked about. A hint that carries a release-group id
// outranks one that carries only a title, wherever it sits in the list.
func TestOnlyOneHintIsEverSpentAndTheTaggedOneWins(t *testing.T) {
	p, stub := theEaglesStub(t)

	meta, err := p.Lookup(context.Background(), artistRef("The Eagles",
		AlbumHint{Title: "Their Greatest Hits"},
		AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: hellFreezesOverRGID},
		AlbumHint{Title: "Hotel California"},
	))
	if err != nil {
		t.Fatalf("lookup: %v (calls: %v)", err, stub.calls())
	}
	if meta.ExternalID != americanEaglesMBID {
		t.Fatalf("meta = %+v, want the band the tagged album credits", meta)
	}
	if n := stub.searches(); n != 0 {
		t.Fatalf("%d searches, want 0 — a hint carrying an id is preferred over the two "+
			"carrying only a title, so nothing is searched for (calls: %v)", n, stub.calls())
	}
	if n := stub.count("/release-group"); n != 1 {
		t.Fatalf("%d release-group requests, want 1 (calls: %v)", n, stub.calls())
	}
}

// A release-group id the files assert can be stale or merged, and MusicBrainz
// answers 404. That is "this album could not corroborate", not "this artist does
// not exist" — so it falls through to the name search rather than settling the
// Artist as unmatched.
func TestAStaleTaggedReleaseGroupFallsThroughToTheNameSearch(t *testing.T) {
	p, stub := theEaglesStub(t)

	meta, err := p.Lookup(context.Background(), artistRef("The Eagles",
		AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: "deadbeef-0000-0000-0000-000000000000"}))
	if err != nil {
		t.Fatalf("lookup: %v (calls: %v)", err, stub.calls())
	}
	if meta.ExternalID != britishEaglesMBID {
		t.Fatalf("meta = %+v, want the name search's answer — a 404 on a corroborating album "+
			"must not settle the Artist (calls: %v)", meta, stub.calls())
	}
	if n := stub.nameSearches(); n != 1 {
		t.Errorf("%d name searches, want 1 (calls: %v)", n, stub.calls())
	}
}

// A hinted release-group id that is not a UUID is never sent (ADR-0049: ids are
// validated before use). An unvalidated id would 404 and spend the one
// corroborating call on a typo.
func TestAMalformedTaggedReleaseGroupIsNeverSent(t *testing.T) {
	p, stub := theEaglesStub(t)

	if _, err := p.Lookup(context.Background(), artistRef("The Eagles",
		AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: "not-a-uuid"})); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// It falls back to the TITLE of the same hint, which is still corroboration.
	if n := stub.count("/release-group/"); n != 0 {
		t.Fatalf("sent %d release-group LOOKUPS for a malformed id, want 0 (calls: %v)",
			n, stub.calls())
	}
	if n := stub.count("/release-group?"); n != 1 {
		t.Errorf("%d release-group searches, want 1 — a hint with an unusable id still has a "+
			"usable title (calls: %v)", n, stub.calls())
	}
}

// A corroborating call that fails TRANSIENTLY is an outage, not an answer. It must
// not fall through to the name search: doing so would resurrect the wrong-Eagles
// match precisely while MusicBrainz is shedding load, and write it `matched` where
// nothing ever flags it. The pass records a failure and ADR-0048's backoff re-asks.
func TestATransientCorroborationFailureDoesNotFallBackToTheName(t *testing.T) {
	p, stub := theEaglesStub(t)
	stub.status = map[string]int{"/release-group/" + hellFreezesOverRGID: http.StatusServiceUnavailable}

	_, err := p.Lookup(context.Background(), artistRef("The Eagles",
		AlbumHint{Title: "Hell Freezes Over", ReleaseGroupMBID: hellFreezesOverRGID}))
	if err == nil || errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want a transient provider failure — 'the discography says "+
			"nothing' and 'MusicBrainz was busy' are different answers, and only the first "+
			"may reach the name search (calls: %v)", err, stub.calls())
	}
	if n := stub.nameSearches(); n != 0 {
		t.Errorf("%d name searches during an outage, want 0 — answering by name here is "+
			"exactly how the wrong band gets stored `matched` (calls: %v)", n, stub.calls())
	}
}

// --- the hints the pass builds ------------------------------------------------

// musicAlbumHints is deterministic and capped: two passes ask MusicBrainz the same
// question about the same album, so the second is answerable from a cache instead
// of from the search cluster.
func TestMusicAlbumHintsPrefersTaggedAlbumsCapsAtThreeAndIsStable(t *testing.T) {
	albums := []store.Album{
		{Title: "On the Border"},
		{Title: "Hotel California", MusicbrainzID: "rg-hotel"},
		{Title: "The Long Run"},
		{Title: "Hell Freezes Over", MusicbrainzID: "rg-hell"},
		{Title: "Desperado", MusicbrainzID: "rg-desperado"},
		{Title: "Eagles Live", MusicbrainzID: "rg-live"},
		{Title: "   "},
	}
	got := musicAlbumHints(albums)
	want := []AlbumHint{
		{Title: "Hotel California", ReleaseGroupMBID: "rg-hotel"},
		{Title: "Hell Freezes Over", ReleaseGroupMBID: "rg-hell"},
		{Title: "Desperado", ReleaseGroupMBID: "rg-desperado"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("hints = %v, want %v — albums the FILES identify come first, the store's "+
			"order is preserved inside each group, and three is the cap", got, want)
	}
	if fmt.Sprint(musicAlbumHints(albums)) != fmt.Sprint(got) {
		t.Error("two calls produced different hints — the question asked of MusicBrainz must " +
			"not move between passes, or the cache never helps")
	}
}

// With no tagged album the titles carry the hints, still in store order, still
// capped; an album with neither an id nor a title corroborates nothing and is
// dropped.
func TestMusicAlbumHintsFallsBackToTitlesAndDropsTheUnusable(t *testing.T) {
	got := musicAlbumHints([]store.Album{
		{Title: ""},
		{Title: "Hell Freezes Over"},
		{Title: "Hotel California"},
		{Title: "The Long Run"},
		{Title: "Desperado"},
	})
	want := []AlbumHint{{Title: "Hell Freezes Over"}, {Title: "Hotel California"}, {Title: "The Long Run"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("hints = %v, want %v", got, want)
	}
	if got := musicAlbumHints(nil); got != nil {
		t.Errorf("hints = %v for an Artist with no albums, want nil", got)
	}
}

// --- through a real pass ------------------------------------------------------

// newEaglesLibrary builds a Service over a real migrated DB holding one music
// Library → the Artist "The Eagles" → the Album "Hell Freezes Over" → one Track,
// against the two-Eagles MusicBrainz stub.
func newEaglesLibrary(t *testing.T, rgTag string, seedArtist func(exec func(string, ...any))) (*Service, *store.DB, *mbArtistStub) {
	t.Helper()
	p, stub := theEaglesStub(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
	// The article is spelled exactly as the operator's tags spell it — and exactly as
	// the wrong band is named at MusicBrainz. ADR-0037 made the article irrelevant to
	// Obelo's own identity KEY; this row is about the provider query, which never saw
	// that rule and is not going to need it.
	exec(`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
	      VALUES ('ar1', 'lib', 'The Eagles', 'artist:eagles', 'eagles')`)
	exec(`INSERT INTO albums (id, artist_id, title, identity_key, sort_title, musicbrainz_id)
	      VALUES ('al1', 'ar1', 'Hell Freezes Over', 'artist:eagles|album:hell freezes over',
	              'hell freezes over', ?)`, rgTag)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, album_id,
	                          disc_number, track_number)
	      VALUES ('t1', 'lib', 'track', 'Get Over It',
	              'artist:eagles|album:hell freezes over|d01t01:get over it', 'get over it', 'al1', 1, 1)`)
	if seedArtist != nil {
		seedArtist(exec)
	}
	svc := NewService(db, p, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) })
	return svc, db, stub
}

// The whole thing, through a real pass over a real database: the Albums are now
// read BEFORE the Artist is enriched, so the Artist's ref carries the album that
// identifies it. This is the reordering half of the change — without it the
// provider has the mechanism and nothing to run it on.
func TestAPassCorroboratesTheArtistThroughItsAlbum(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, nil)

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got, err := db.EntityEnrichmentByID(store.EntityArtist, "ar1")
	if err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if got.ExternalID == britishEaglesMBID {
		t.Fatalf("the pass stored %s — the 1960s UK instrumental group, the exact row that "+
			"read `matched` while thirteen tracks on this album went unmatched three levels "+
			"down (calls: %v)", got.ExternalID, stub.calls())
	}
	if got.ExternalID != americanEaglesMBID {
		t.Fatalf("artist external_id = %q, want %q (calls: %v)", got.ExternalID, americanEaglesMBID, stub.calls())
	}
	// "A corroborated Artist is written OriginDerived." Nobody chose it: a later pass
	// may revise it, and an Admin's Fix-info still outranks it (ADR-0045/0046).
	if got.ExternalIDOrigin != store.OriginDerived {
		t.Errorf("origin = %q, want OriginDerived (%q) — corroboration is the pass's own "+
			"derivation, not anyone's choice, and marking it otherwise would make it "+
			"immune to correction", got.ExternalIDOrigin, store.OriginDerived)
	}
	if got.ExternalIDOrigin.Locked() {
		t.Error("a corroborated Artist reads as a durable override — an Admin's Fix-info " +
			"would no longer be able to outrank it")
	}
	if n := stub.nameSearches(); n != 0 {
		t.Errorf("%d artist name searches in the pass, want 0 (calls: %v)", n, stub.calls())
	}
}

// "An Admin's Fix-info still wins over everything" (ADR-0045/0046, ADR-0019): a
// pinned Artist resolves BY the pinned id every pass and never corroborates.
func TestAnAdminsFixInfoStillWinsOverCorroboration(t *testing.T) {
	svc, db, stub := newEaglesLibrary(t, hellFreezesOverRGID, func(exec func(string, ...any)) {
		exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, external_id_origin,
		                                     enrichment_status)
		      VALUES ('artist', 'ar1', ?, 'chosen', 'pending')`, britishEaglesMBID)
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got, err := db.EntityEnrichmentByID(store.EntityArtist, "ar1")
	if err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if got.ExternalID != britishEaglesMBID {
		t.Fatalf("artist external_id = %q, want the Admin's choice %q — corroboration is the "+
			"pass's evidence, and it does not get to overrule a human (calls: %v)",
			got.ExternalID, britishEaglesMBID, stub.calls())
	}
	// inc=artist-credits is corroboration's fingerprint on the wire, and nothing
	// else in the pass asks for it — the Album's own lookup asks for inc=tags.
	for _, c := range stub.calls() {
		if strings.Contains(c, "inc=artist-credits") {
			t.Errorf("the pass corroborated a pinned Artist (calls: %v) — a Fix-info id "+
				"resolves by lookup and asks nothing else", stub.calls())
			break
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
