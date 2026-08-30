package api_test

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for the file matcher's HTTP surface (file-matcher/05,
// ADR-0044): GET /shows/{id}/matcher, PUT /shows/{id}/matcher, and
// GET /shows/{id}/seriesSeasons.
//
// What these have to prove is mostly about ABSENCE, which is why they are worth
// having: that the file list omits nothing (a File it does not list is a File the
// Admin cannot place, and they cannot tell an omission from an absence), that a
// missing provider costs the records and nothing else, and that the two refusals
// an Admin can hit say enough to act on.
//
// The fixtures live under testdata/matcher/ rather than testdata/tv/ on purpose:
// the TV tests assert their tree's Show count, so a Show added there to exercise
// a collision would break them somewhere else entirely.

const matcherRootRel = "matcher"

var matcherClips = []tvClip{
	// A well-formed Show carrying a provider id, so the provider half of the
	// response has a series to list from.
	{filepath.Join("Sorted Show (2018) {tmdb-1438}", "Season 01", "Sorted Show (2018) - S01E01 - One.mkv"), "160x120"},
	{filepath.Join("Sorted Show (2018) {tmdb-1438}", "Season 01", "Sorted Show (2018) - S01E02 - Two.mkv"), "160x120"},
	// Recognized media the Scanner cannot number: no SxxExx, no date, no digits at
	// all. It becomes an Unmatched row and must still appear in the matcher.
	{filepath.Join("Sorted Show (2018) {tmdb-1438}", "Season 01", "Sorted Show - mystery.mkv"), "160x120"},
	// Two files whose FILENAMES already claim S01E06 — the collision the matcher
	// did not create and cannot settle for the Admin. The range file claims E05 as
	// well, so E06 holds two Files that are not parts of one another and the Episode
	// is flagged ambiguous (naming-convention.md's Collision rule).
	//
	// This pair used to be the one place the matcher misrepresented the disk: the
	// scan built one Episode tree per file, both keyed to S01E06, and the loser's
	// File row was deleted by the second write without ever reaching
	// unmatched_files (tv-episode-editions/01). Both are catalogued now, which is
	// what TestMatcherShowsBothFilesOfAParseCollision pins.
	{filepath.Join("Clash Show (2019)", "Season 01", "Clash Show (2019) - S01E05-E06 - Alpha.mkv"), "160x120"},
	{filepath.Join("Clash Show (2019)", "Season 01", "Clash Show (2019) - S01E06 - Zulu.mkv"), "160x120"},
}

var matcherFixturesAvailable bool

func init() { matcherFixturesAvailable = ensureMatcherFixtures() }

func ensureMatcherFixtures() bool {
	root := filepath.Join("testdata", matcherRootRel)
	for _, c := range matcherClips {
		out := filepath.Join(root, c.relPath)
		if fileExists(out) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return false
		}
		cmd := exec.Command("ffmpeg",
			"-y", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=1:size="+c.size+":rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-shortest", out)
		if cmd.Run() != nil {
			return false
		}
	}
	return true
}

func requireMatcherFixtures(t *testing.T) {
	t.Helper()
	if !matcherFixturesAvailable {
		t.Skip("matcher fixtures unavailable (ffmpeg not on PATH)")
	}
}

func matcherRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", matcherRootRel))
	if err != nil {
		t.Fatalf("resolving matcher root: %v", err)
	}
	return abs
}

// --- wire shapes ------------------------------------------------------------

type slotPositionResp struct {
	Group int `json:"group"`
	Slot  int `json:"slot"`
}

type slotRecordRefResp struct {
	ExternalID string `json:"externalId"`
	Group      int    `json:"group"`
	Slot       int    `json:"slot"`
	// The borrowed record's own words, which ride alongside its address so the
	// screen can show what it borrowed without the Slot's own record being replaced.
	Name     string `json:"name"`
	Overview string `json:"overview"`
	AirDate  string `json:"airDate"`
	StillURL string `json:"stillUrl"`
}

type matcherSlotResp struct {
	Group    int                `json:"group"`
	Slot     int                `json:"slot"`
	TitleID  string             `json:"titleId"`
	Name     string             `json:"name"`
	Overview string             `json:"overview"`
	AirDate  string             `json:"airDate"`
	StillURL string             `json:"stillUrl"`
	Record   *slotRecordRefResp `json:"record"`
}

type matcherGroupResp struct {
	Number           int               `json:"number"`
	Source           string            `json:"source"`
	SlotCount        int               `json:"slotCount"`
	SlotsLoaded      bool              `json:"slotsLoaded"`
	SlotsUnavailable string            `json:"slotsUnavailable"`
	FileCount        int               `json:"fileCount"`
	PlacedCount      int               `json:"placedCount"`
	UnassignedCount  int               `json:"unassignedCount"`
	IgnoredCount     int               `json:"ignoredCount"`
	Slots            []matcherSlotResp `json:"slots"`
}

type matcherPlacementResp struct {
	Group   int `json:"group"`
	Slot    int `json:"slot"`
	Ordinal int `json:"ordinal"`
}

type matcherFileResp struct {
	Path       string                 `json:"path"`
	State      string                 `json:"state"`
	TitleID    string                 `json:"titleId"`
	Parsed     []slotPositionResp     `json:"parsed"`
	Placements []matcherPlacementResp `json:"placements"`
	Decided    bool                   `json:"decided"`
	Orphaned   bool                   `json:"orphaned"`
	Reason     string                 `json:"reason"`
}

type matcherAppliedResp struct {
	Rearranged int      `json:"rearranged"`
	Displaced  []string `json:"displaced"`
	Deferred   []string `json:"deferred"`
}

type matcherResp struct {
	ContainerID      string              `json:"containerId"`
	ContainerType    string              `json:"containerType"`
	LibraryID        string              `json:"libraryId"`
	Title            string              `json:"title"`
	Year             int                 `json:"year"`
	SeriesExternalID string              `json:"seriesExternalId"`
	SlotsUnavailable string              `json:"slotsUnavailable"`
	Groups           []matcherGroupResp  `json:"groups"`
	Files            []matcherFileResp   `json:"files"`
	Applied          *matcherAppliedResp `json:"applied"`
}

type seriesSlotsResp struct {
	ExternalID string `json:"externalId"`
	Groups     []struct {
		Number    int `json:"number"`
		SlotCount int `json:"slotCount"`
	} `json:"groups"`
	Slots []matcherSlotResp `json:"slots"`
}

// --- helpers ----------------------------------------------------------------

// fakeEpisodeProvider is a fakeProvider that ALSO implements the optional
// EpisodeLister capability, and counts its calls — which is the whole point:
// per-group loading is a claim about how many provider round-trips a screen
// costs, and only a counter can check it.
type fakeEpisodeProvider struct {
	fakeProvider
	mu       sync.Mutex
	seasons  int
	episodes []int
	// groups is how many seasons the fake series has.
	groups int
}

func (f *fakeEpisodeProvider) SeriesSeasons(_ context.Context, _ string) ([]enrich.SeasonSummary, error) {
	f.mu.Lock()
	f.seasons++
	f.mu.Unlock()
	out := make([]enrich.SeasonSummary, 0, f.groups)
	for n := 1; n <= f.groups; n++ {
		out = append(out, enrich.SeasonSummary{Season: n, EpisodeCount: 3})
	}
	return out, nil
}

func (f *fakeEpisodeProvider) SeasonEpisodes(_ context.Context, _ string, season int) ([]enrich.EpisodeCandidate, error) {
	f.mu.Lock()
	f.episodes = append(f.episodes, season)
	f.mu.Unlock()
	return []enrich.EpisodeCandidate{
		{Season: season, Episode: 1, Name: "Provider One", Overview: "first", AirDate: "2018-01-01"},
		{Season: season, Episode: 2, Name: "Provider Two"},
		{Season: season, Episode: 3, Name: "Provider Three"},
	}, nil
}

func (f *fakeEpisodeProvider) counts() (seasons int, episodes []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seasons, append([]int(nil), f.episodes...)
}

// scanMatcherLibrary boots a server with the given options, scans the matcher
// fixture tree as a TV Library, and returns server/admin token/library id.
func scanMatcherLibrary(t *testing.T, opts ...testharness.Option) (*testharness.Server, string, string) {
	t.Helper()
	srv := testharness.New(t, opts...)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, matcherRoot(t))
	scanLib(t, srv, token, libID, "")
	return srv, token, libID
}

func matcherShowID(t *testing.T, srv *testharness.Server, token, libID, title string) string {
	t.Helper()
	return findShow(t, listShows(t, srv, token, libID), title)
}

func getMatcher(t *testing.T, srv *testharness.Server, token, showID, query string) matcherResp {
	t.Helper()
	var out matcherResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/matcher"+query, token, &out)
	if status != http.StatusOK {
		t.Fatalf("GET matcher%s = %d, want 200; body: %s", query, status, body)
	}
	return out
}

func fileNamed(m matcherResp, base string) (matcherFileResp, bool) {
	for _, f := range m.Files {
		if filepath.Base(f.Path) == base {
			return f, true
		}
	}
	return matcherFileResp{}, false
}

func groupNumbered(m matcherResp, n int) (matcherGroupResp, bool) {
	for _, g := range m.Groups {
		if g.Number == n {
			return g, true
		}
	}
	return matcherGroupResp{}, false
}

// --- The local half is complete -------------------------------------------

// TestMatcherListsEveryFileUnderTheShow: the file list must name every recognized
// media file under the Show, drawn from all three places the ingestion path can
// leave one. Here that is two Episode Files and one Unmatched row; the Unmatched
// one is the whole reason this screen exists (PRD user story 7) and is exactly the
// one a per-Title endpoint can never reach.
func TestMatcherListsEveryFileUnderTheShow(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	if m.ContainerID != showID || m.ContainerType != "show" {
		t.Errorf("container = %q/%q, want %q/show", m.ContainerID, m.ContainerType, showID)
	}
	for _, want := range []string{
		"Sorted Show (2018) - S01E01 - One.mkv",
		"Sorted Show (2018) - S01E02 - Two.mkv",
		"Sorted Show - mystery.mkv",
	} {
		if _, ok := fileNamed(m, want); !ok {
			t.Errorf("file %q missing from the matcher; a file it does not list is a file the Admin cannot place. have: %+v", want, m.Files)
		}
	}
}

// TestMatcherUnmatchedFileIsUnassigned: a file the Scanner could not number is a
// Placement problem, not an identity one — it reports `unassigned` with the reason
// it could not be numbered, and it is NOT marked decided (nobody said anything
// about it yet; the parse simply failed).
func TestMatcherUnmatchedFileIsUnassigned(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	f, ok := fileNamed(m, "Sorted Show - mystery.mkv")
	if !ok {
		t.Fatalf("the unmatched file is absent from the matcher: %+v", m.Files)
	}
	if f.State != "unassigned" {
		t.Errorf("unmatched file state = %q, want unassigned", f.State)
	}
	if len(f.Placements) != 0 {
		t.Errorf("unmatched file has placements %+v, want none", f.Placements)
	}
	if f.Decided {
		t.Errorf("unmatched file reads as decided; nobody has said anything about it")
	}
	if f.Reason == "" {
		t.Errorf("unmatched file carries no reason — the screen cannot say why it is here")
	}
}

// TestMatcherPlacedFileCarriesItsSlotAndParse: a normally-filed Episode reports
// where it sits AND what its filename says, because the screen compares the two
// and their disagreement is the correction being made (PRD user story 2).
func TestMatcherPlacedFileCarriesItsSlotAndParse(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	f, ok := fileNamed(m, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("S01E01 missing: %+v", m.Files)
	}
	if f.State != "placed" {
		t.Errorf("state = %q, want placed", f.State)
	}
	if f.TitleID == "" {
		t.Errorf("a placed file carries no titleId")
	}
	if len(f.Placements) != 1 || f.Placements[0].Group != 1 || f.Placements[0].Slot != 1 {
		t.Errorf("placements = %+v, want [{group 1 slot 1}]", f.Placements)
	}
	if len(f.Parsed) != 1 || f.Parsed[0].Group != 1 || f.Parsed[0].Slot != 1 {
		t.Errorf("parsed = %+v, want [{group 1 slot 1}]", f.Parsed)
	}
	if f.Decided {
		t.Errorf("a file following its own filename must not read as decided")
	}

	g, ok := groupNumbered(m, 1)
	if !ok {
		t.Fatalf("group 1 missing: %+v", m.Groups)
	}
	if g.PlacedCount != 2 || g.UnassignedCount != 1 {
		t.Errorf("group 1 counts = placed %d / unassigned %d, want 2 / 1", g.PlacedCount, g.UnassignedCount)
	}
	if len(g.Slots) < 2 {
		t.Errorf("group 1 has %d slots, want at least the two the local files claim", len(g.Slots))
	}
}

// --- The degraded path is a state, not an error -----------------------------

// TestMatcherDegradesWhenTheProviderCannotListEpisodes: a Library whose
// Authoritative provider does not implement EpisodeLister (only TMDB does) gets
// bare numbered Slots and a REASON — never a 5xx. Pure renumbering still works
// offline, which is most of what this screen is for (ADR-0044).
func TestMatcherDegradesWhenTheProviderCannotListEpisodes(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeProvider{fn: func(enrich.TitleRef) (enrich.TitleMetadata, error) {
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}}
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	if m.SlotsUnavailable != "provider-cannot-list" {
		t.Errorf("slotsUnavailable = %q, want provider-cannot-list", m.SlotsUnavailable)
	}
	// The local half is untouched: the Slots the files claim are still there, and
	// they are bare.
	g, ok := groupNumbered(m, 1)
	if !ok || len(g.Slots) == 0 {
		t.Fatalf("degraded response lost its local slots: %+v", m.Groups)
	}
	for _, s := range g.Slots {
		if s.Name != "" {
			t.Errorf("slot S%02dE%02d carries a record %q on the degraded path", s.Group, s.Slot, s.Name)
		}
	}
	if len(m.Files) == 0 {
		t.Errorf("degraded response lost the file list, which needs no provider at all")
	}
}

// TestMatcherDegradesWithEnrichmentOff: the other degraded reason. It is a
// separate constant because the fixes differ — one is a switch, the other is a
// different provider — and the screen has to be able to say which.
func TestMatcherDegradesWithEnrichmentOff(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	if m.SlotsUnavailable != "enrichment-disabled" {
		t.Errorf("slotsUnavailable = %q, want enrichment-disabled", m.SlotsUnavailable)
	}
}

// TestMatcherDegradesWhenTheShowNeverMatched: a Show with no provider record has
// no series to list. That is not a provider failure and must not read as one —
// the fix is to match the Show, not to reconfigure enrichment.
func TestMatcherDegradesWhenTheShowNeverMatched(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeEpisodeProvider{groups: 2}
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return enrich.TitleMetadata{}, enrich.ErrNoMatch }
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	// Clash Show's folder carries no {tmdb-…} tag, so it never matched.
	showID := matcherShowID(t, srv, token, libID, "Clash Show")

	m := getMatcher(t, srv, token, showID, "")
	if m.SlotsUnavailable != "no-series-match" {
		t.Errorf("slotsUnavailable = %q, want no-series-match", m.SlotsUnavailable)
	}
	if seasons, _ := prov.counts(); seasons != 0 {
		t.Errorf("the provider was asked %d times about a Show with no series id", seasons)
	}
}

// TestMatcherListsSlotsForAShowMatchedByProviderSearch is the ordinary Show, and
// the majority case: no {tmdb-…} in the folder name, matched by a provider TITLE
// SEARCH. That match is recorded in entity_enrichment.external_id and leaves
// shows.tmdb_id empty — so a matcher that reads the column alone reports
// "no-series-match" for a Show that is matched, enriched and decorated everywhere
// else in the app (file-matcher/10).
func TestMatcherListsSlotsForAShowMatchedByProviderSearch(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeEpisodeProvider{groups: 2}
	prov.fn = func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		// Only the Show parent resolves, and only by search: the ref carries a title
		// and no id, which is exactly the shape that writes external_id and nothing
		// else.
		if ref.Kind == "show" && ref.TMDBID == "" && strings.HasPrefix(ref.Title, "Clash") {
			return enrich.TitleMetadata{Matched: true, ExternalID: "4242", Overview: "searched"}, nil
		}
		return enrich.TitleMetadata{}, enrich.ErrNoMatch
	}
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	// The pass the whole case turns on: a title search, whose result lands in
	// entity_enrichment and nowhere near shows.tmdb_id.
	enrichLib(t, srv, token, libID, "full")
	showID := matcherShowID(t, srv, token, libID, "Clash Show")

	m := getMatcher(t, srv, token, showID, "")
	if m.SlotsUnavailable != "" {
		t.Fatalf("slotsUnavailable = %q, want the Slots listed from the searched match", m.SlotsUnavailable)
	}
	// The folder carries no id at all, so this can only have come from the
	// enrichment match.
	if m.SeriesExternalID != "4242" {
		t.Errorf("seriesExternalId = %q, want the searched series %q", m.SeriesExternalID, "4242")
	}
	g, ok := groupNumbered(m, 2)
	if !ok || g.Source != "provider" {
		t.Errorf("season 2 = %+v (present=%v), want a provider-sourced group", g, ok)
	}
}

// --- Slots load per group ---------------------------------------------------

// TestMatcherLoadsSlotsPerGroup: opening a ten-group Show costs ONE provider call,
// not ten; expanding a group costs one more; expanding it again costs none.
//
// This is the whole reason the endpoint takes a group parameter. SeasonEpisodes is
// one round-trip per season, so an eager response would spend ten of them against a
// rate-limited API to fill nine sections nobody has opened.
func TestMatcherLoadsSlotsPerGroup(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeEpisodeProvider{groups: 10}
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return enrich.TitleMetadata{}, enrich.ErrNoMatch }
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	seasons, episodes := prov.counts()
	if seasons != 1 || len(episodes) != 0 {
		t.Fatalf("opening the matcher cost %d group calls and %d slot calls, want 1 and 0", seasons, len(episodes))
	}
	if m.SlotsUnavailable != "" {
		t.Fatalf("slotsUnavailable = %q with a listing provider wired", m.SlotsUnavailable)
	}
	// Every group the provider knows about is offered, so a season the folders do
	// not have is still available to drag into (PRD user story 4) — collapsed, with
	// its count, and not yet loaded.
	if len(m.Groups) < 10 {
		t.Errorf("groups = %d, want at least the provider's 10", len(m.Groups))
	}
	g, ok := groupNumbered(m, 4)
	if !ok {
		t.Fatalf("provider group 4 absent from the first response: %+v", m.Groups)
	}
	if g.Source != "provider" || g.SlotCount != 3 || g.SlotsLoaded {
		t.Errorf("group 4 = %+v, want source provider, slotCount 3, slotsLoaded false", g)
	}

	// Expanding one group fetches exactly that group.
	m = getMatcher(t, srv, token, showID, "?group=1")
	_, episodes = prov.counts()
	if len(episodes) != 1 || episodes[0] != 1 {
		t.Fatalf("expanding group 1 made slot calls %v, want exactly [1]", episodes)
	}
	g, ok = groupNumbered(m, 1)
	if !ok || !g.SlotsLoaded {
		t.Fatalf("group 1 did not load: %+v", g)
	}
	var named int
	for _, s := range g.Slots {
		if s.Name != "" {
			named++
		}
	}
	if named == 0 {
		t.Errorf("group 1 loaded no records: %+v", g.Slots)
	}

	// A second expand of the same group is served from cache.
	getMatcher(t, srv, token, showID, "?group=1")
	if _, again := prov.counts(); len(again) != 1 {
		t.Errorf("re-expanding group 1 made slot calls %v, want the cached answer", again)
	}
}

// TestMatcherSlotReportsAForeignRecord: a Slot has two independent halves — its
// POSITION, always the library's own numbering, and its RECORD. When an Admin has
// pinned a Slot to another series' episode, the matcher must SAY so, or the screen
// shows a title that does not come from the Show's own series with no way to tell.
// This is the Batman → New Batman Adventures case the pin exists for (ADR-0044).
func TestMatcherSlotReportsAForeignRecord(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeEpisodeProvider{groups: 2}
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return richMeta(), nil }
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	before := getMatcher(t, srv, token, showID, "")
	g, _ := groupNumbered(before, 1)
	var episodeID string
	for _, s := range g.Slots {
		if s.Slot == 1 && s.TitleID != "" {
			episodeID = s.TitleID
		}
		if s.Record != nil {
			t.Fatalf("slot S%02dE%02d reports a foreign record before anything was pinned", s.Group, s.Slot)
		}
	}
	if episodeID == "" {
		t.Fatalf("no Title at S01E01 to pin: %+v", g.Slots)
	}

	// Pin S01E01's record to season 2, episode 3 of a DIFFERENT series.
	if status, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+episodeID+"/enrichmentOverride", token,
		map[string]any{"externalId": "77777", "season": 2, "episode": 3}, nil); status != http.StatusOK {
		t.Fatalf("enrichmentOverride = %d, want 200; body: %s", status, body)
	}

	after := getMatcher(t, srv, token, showID, "")
	g, _ = groupNumbered(after, 1)
	for _, s := range g.Slots {
		if s.Slot != 1 {
			continue
		}
		if s.Record == nil {
			t.Fatalf("S01E01 does not report its pinned record: %+v", s)
		}
		if s.Record.ExternalID != "77777" || s.Record.Group != 2 || s.Record.Slot != 3 {
			t.Errorf("record = %+v, want the foreign series 77777 at group 2 slot 3", s.Record)
		}
		// The POSITION is untouched: the pin decides what decorates the Slot, never
		// where the file sits (ADR-0014 keeps the watch state with the position).
		if s.Group != 1 || s.Slot != 1 {
			t.Errorf("the pin moved the Slot to S%02dE%02d", s.Group, s.Slot)
		}
		return
	}
	t.Fatalf("S01E01 vanished after pinning: %+v", g.Slots)
}

// TestSeriesSeasonsListsAForeignSeries: filling a group's Slots from ANOTHER
// series' records — the Batman → New Batman Adventures case the Episode pin exists
// for. The Slot's position stays local; only its record changes.
func TestSeriesSeasonsListsAForeignSeries(t *testing.T) {
	requireMatcherFixtures(t)
	prov := &fakeEpisodeProvider{groups: 2}
	prov.fn = func(enrich.TitleRef) (enrich.TitleMetadata, error) { return enrich.TitleMetadata{}, enrich.ErrNoMatch }
	srv, token, libID := scanMatcherLibrary(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov))
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	var out seriesSlotsResp
	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seriesSeasons?externalId=99999", token, &out)
	if status != http.StatusOK {
		t.Fatalf("seriesSeasons = %d, want 200; body: %s", status, body)
	}
	if out.ExternalID != "99999" || len(out.Groups) != 2 {
		t.Errorf("seriesSeasons = %+v, want the foreign series' 2 groups", out)
	}
	if len(out.Slots) != 0 {
		t.Errorf("seriesSeasons returned slots with no group asked for: %+v", out.Slots)
	}

	status, body = srv.AuthGET("/api/v1/shows/"+showID+"/seriesSeasons?externalId=99999&group=2", token, &out)
	if status != http.StatusOK {
		t.Fatalf("seriesSeasons group = %d, want 200; body: %s", status, body)
	}
	if len(out.Slots) != 3 || out.Slots[0].Name == "" {
		t.Errorf("seriesSeasons group 2 slots = %+v, want the foreign series' records", out.Slots)
	}

	// Missing externalId has nothing to answer, and saying so beats an empty list
	// that reads as "that series has no episodes".
	if status, _ := srv.AuthGET("/api/v1/shows/"+showID+"/seriesSeasons", token, nil); status != http.StatusBadRequest {
		t.Errorf("seriesSeasons with no externalId = %d, want 400", status)
	}
}

// TestSeriesSeasonsReportsAProviderThatCannotList: unlike the matcher, this route
// has nothing but provider data to return, so a provider that cannot list is an
// error here rather than a degraded state.
func TestSeriesSeasonsReportsAProviderThatCannotList(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seriesSeasons?externalId=1438", token, nil)
	if status != http.StatusServiceUnavailable {
		t.Errorf("seriesSeasons with no listing provider = %d, want 503; body: %s", status, body)
	}
}

// --- Apply ------------------------------------------------------------------

// TestApplyMatcherMovesAFileAndReturnsTheArrangement: the Apply commits and hands
// back the RE-READ working set, so the client never has to guess what the server
// made of its payload.
func TestApplyMatcherMovesAFileAndReturnsTheArrangement(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	before := getMatcher(t, srv, token, showID, "")
	two, ok := fileNamed(before, "Sorted Show (2018) - S01E02 - Two.mkv")
	if !ok {
		t.Fatalf("S01E02 missing: %+v", before.Files)
	}
	one, ok := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("S01E01 missing: %+v", before.Files)
	}

	// Move the second file into a season the folders do not have. Season 4 has no
	// folder on disk and never will: seasons follow assignments, not folders.
	var after matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": one.Path, "state": "placed", "placements": []map[string]any{{"group": 1, "slot": 1}}},
			{"path": two.Path, "state": "placed", "placements": []map[string]any{{"group": 4, "slot": 1}}},
		},
	}, &after)
	if status != http.StatusOK {
		t.Fatalf("apply = %d, want 200; body: %s", status, body)
	}
	if after.Applied == nil {
		t.Fatalf("apply returned no `applied` block; the client cannot tell what happened: %s", body)
	}
	if after.Applied.Rearranged == 0 {
		t.Errorf("applied.rearranged = 0 after moving a file to another group")
	}

	moved, ok := fileNamed(after, "Sorted Show (2018) - S01E02 - Two.mkv")
	if !ok {
		t.Fatalf("the moved file vanished from the re-read arrangement: %+v", after.Files)
	}
	if len(moved.Placements) != 1 || moved.Placements[0].Group != 4 || moved.Placements[0].Slot != 1 {
		t.Errorf("moved file placements = %+v, want [{group 4 slot 1}]", moved.Placements)
	}
	if !moved.Decided {
		t.Errorf("a hand-placed file must read as decided, or Revert has nothing to revert")
	}
	// Its FILENAME still says S01E02, which is what the screen highlights.
	if len(moved.Parsed) != 1 || moved.Parsed[0].Slot != 2 {
		t.Errorf("moved file parsed = %+v, want the filename's [{group 1 slot 2}]", moved.Parsed)
	}

	// The Show really moved: season 4 now exists in browse and holds one episode.
	seasons := showSeasons(t, srv, token, showID)
	var found bool
	for _, s := range seasons.Seasons {
		if s.SeasonNumber == 4 {
			found = true
		}
	}
	if !found {
		t.Errorf("season 4 is not in the Show after the apply: %+v", seasons.Seasons)
	}
}

// TestApplyMatcherIgnoreAndUnassign: the two settled/undecided states round-trip,
// and neither leaves the file listed as an Episode.
func TestApplyMatcherIgnoreAndUnassign(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	before := getMatcher(t, srv, token, showID, "")
	one, _ := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")
	mystery, _ := fileNamed(before, "Sorted Show - mystery.mkv")

	var after matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": one.Path, "state": "unassigned"},
			{"path": mystery.Path, "state": "ignored"},
		},
	}, &after)
	if status != http.StatusOK {
		t.Fatalf("apply = %d, want 200; body: %s", status, body)
	}
	// An unassigned File is still LISTED — it is undecided, not gone. That is the
	// difference between it and a deleted file, and it is what keeps the Show in
	// the Needs-Fixing queue.
	got, ok := fileNamed(after, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("an unassigned file disappeared from the matcher: %+v", after.Files)
	}
	if got.State != "unassigned" || !got.Decided {
		t.Errorf("unassigned file = %+v, want state unassigned and decided true", got)
	}
	ign, ok := fileNamed(after, "Sorted Show - mystery.mkv")
	if !ok {
		t.Fatalf("an ignored file disappeared from the matcher — it is recoverable, so it stays listed: %+v", after.Files)
	}
	if ign.State != "ignored" || !ign.Decided {
		t.Errorf("ignored file = %+v, want state ignored and decided true", ign)
	}
}

// TestApplyMatcherSurfacesDeferred: a Placement onto a File the catalog has never
// probed is stored, but the Episode cannot be built without ffprobe — which Apply
// deliberately does not run. The response must SAY so, or the screen looks like it
// silently dropped the Admin's most deliberate correction.
func TestApplyMatcherSurfacesDeferred(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	before := getMatcher(t, srv, token, showID, "")
	mystery, ok := fileNamed(before, "Sorted Show - mystery.mkv")
	if !ok {
		t.Fatalf("the unmatched file is absent: %+v", before.Files)
	}

	var after matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": mystery.Path, "state": "placed", "placements": []map[string]any{{"group": 1, "slot": 9}}},
		},
	}, &after)
	if status != http.StatusOK {
		t.Fatalf("apply = %d, want 200; body: %s", status, body)
	}
	if after.Applied == nil || len(after.Applied.Deferred) == 0 {
		t.Fatalf("applied.deferred is empty; a placement on a never-probed file must be reported, not silently held: %s", body)
	}
	if filepath.Base(after.Applied.Deferred[0]) != "Sorted Show - mystery.mkv" {
		t.Errorf("deferred = %v, want the unmatched file", after.Applied.Deferred)
	}
}

// TestApplyMatcherRejectsAForeignPath: a decision is stored per (library, path)
// with no Show on it, so accepting a path from another Show would let an Apply
// here quietly rearrange one the Admin was not even looking at.
func TestApplyMatcherRejectsAForeignPath(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	sortedID := matcherShowID(t, srv, token, libID, "Sorted Show")
	clashID := matcherShowID(t, srv, token, libID, "Clash Show")

	clash := getMatcher(t, srv, token, clashID, "")
	if len(clash.Files) == 0 {
		t.Fatalf("Clash Show has no files to borrow: %+v", clash)
	}

	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+sortedID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": clash.Files[0].Path, "state": "ignored"},
		},
	}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("apply with a foreign path = %d, want 422; body: %s", status, body)
	}
	if !strings.Contains(string(body), "OUTSIDE_SHOW") {
		t.Errorf("error code missing from body: %s", body)
	}
}

// TestMatcherShowsBothFilesOfAParseCollision: the file matcher promises to name
// every recognized media file under the Show, and this Show is the one case where
// it used to lie.
//
// Two files claim S01E06: a range file (which also claims E05) and a standalone.
// The parsed TV branch used to build one Episode tree PER FILE, so both carried
// the identity_key of S01E06, writeTitleSubtree dropped the loser's Editions and
// its File cascaded away, and the row reached neither `files` nor
// `unmatched_files`. The file was on disk and absent from every table the API can
// read, on the Show most in need of the matcher (tv-episode-editions/01,
// file-matcher/05).
//
// It is not absent any more: the resolver groups a Slot's Files before building
// it, so both survive the scan and both are listed. The Episode is flagged
// ambiguous instead — the convention's answer for two files it cannot tell apart
// — which is a flag on a row, not a deletion of one.
func TestMatcherShowsBothFilesOfAParseCollision(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Clash Show")

	m := getMatcher(t, srv, token, showID, "")
	for _, want := range []string{
		"Clash Show (2019) - S01E05-E06 - Alpha.mkv",
		"Clash Show (2019) - S01E06 - Zulu.mkv",
	} {
		f, ok := fileNamed(m, want)
		if !ok {
			t.Fatalf("file %q missing from the matcher; it is on disk. have: %+v", want, m.Files)
		}
		if f.State == "unassigned" && f.Reason != "" {
			t.Errorf("%q reports %q; it parsed fine, it merely collides", want, f.Reason)
		}
	}
	// Both claim S01E06, which is the conflict the Admin has to be shown.
	claimsSix := 0
	for _, f := range m.Files {
		for _, p := range f.Parsed {
			if p.Group == 1 && p.Slot == 6 {
				claimsSix++
			}
		}
	}
	if claimsSix != 2 {
		t.Errorf("%d files report claiming S01E06, want 2 — the collision is invisible otherwise", claimsSix)
	}
}

// TestApplyMatcherParseCollisionIsNotARefusal: Apply used to answer 409
// SLOT_COLLISION for a Show whose FILENAMES already claimed one Slot, because the
// arrangement it was being asked to bless was destructive — the Scanner would
// collapse the two Episodes onto one Title and delete one File's row. It is not
// destructive any more, so there is nothing left to refuse: the Slot resolves to
// one Episode holding both Files, flagged ambiguous.
//
// The refusal itself is kept for a set that still cannot be settled; nothing an
// Admin can produce from filenames alone reaches it now.
func TestApplyMatcherParseCollisionIsNotARefusal(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Clash Show")

	var out matcherResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token,
		map[string]any{"files": []map[string]any{}}, &out)
	if status != http.StatusOK {
		t.Fatalf("apply on a show with a parse collision = %d, want 200; body: %s", status, body)
	}
	// Both files are still there afterwards. That is the whole point: a refusal at
	// least left them alone, and a silent collapse did not.
	for _, want := range []string{
		"Clash Show (2019) - S01E05-E06 - Alpha.mkv",
		"Clash Show (2019) - S01E06 - Zulu.mkv",
	} {
		if _, ok := fileNamed(out, want); !ok {
			t.Errorf("file %q vanished across Apply. have: %+v", want, out.Files)
		}
	}
}

// TestApplyMatcherDuringAScanConflictsAndWritesNothing: Apply is the SECOND writer
// of catalog rows, so it takes the same per-Library lock a scan does (ADR-0031). A
// rearrangement half-written under a running scan is exactly the row race the rule
// exists to prevent, so it refuses idempotently — and the arrangement afterwards
// must be untouched.
func TestApplyMatcherDuringAScanConflictsAndWritesNothing(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	before := getMatcher(t, srv, token, showID, "")
	one, _ := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")

	release := srv.HoldLibraryScanLock(libID)
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token, map[string]any{
		"files": []map[string]any{
			{"path": one.Path, "state": "ignored"},
		},
	}, nil)
	release()
	if status != http.StatusConflict {
		t.Fatalf("apply during a scan = %d, want 409; body: %s", status, body)
	}
	if !strings.Contains(string(body), "SCAN_RUNNING") {
		t.Errorf("error code missing from body: %s", body)
	}

	after := getMatcher(t, srv, token, showID, "")
	got, ok := fileNamed(after, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("the file vanished after a refused apply: %+v", after.Files)
	}
	if got.State != "placed" || got.Decided {
		t.Errorf("a refused apply wrote something: file = %+v, want the untouched placed/undecided state", got)
	}
}

// TestApplyMatcherRejectsAPlacedFileWithNoSlot: "placed" with nothing to place it
// on cannot mean anything, and silently storing it would leave a decision the next
// scan reads as an empty Placement.
func TestApplyMatcherRejectsMalformedStates(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")
	before := getMatcher(t, srv, token, showID, "")
	one, _ := fileNamed(before, "Sorted Show (2018) - S01E01 - One.mkv")

	for _, tc := range []struct {
		name string
		file map[string]any
	}{
		{"placed with no slot", map[string]any{"path": one.Path, "state": "placed"}},
		{"unknown state", map[string]any{"path": one.Path, "state": "maybe"}},
		{"no path", map[string]any{"path": "", "state": "ignored"}},
		{"settled with a slot", map[string]any{
			"path": one.Path, "state": "ignored",
			"placements": []map[string]any{{"group": 1, "slot": 1}},
		}},
	} {
		status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token,
			map[string]any{"files": []map[string]any{tc.file}}, nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400; body: %s", tc.name, status, body)
		}
	}
}

// --- Access -----------------------------------------------------------------

// TestMatcherRoutesRequireAdmin: all three reach an external provider and rewrite
// the catalog on the Admin's behalf. A Member gets 403 on every one.
func TestMatcherRoutesRequireAdmin(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	srv.CreateMember("member", "memberpass123")
	member := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token

	if status, body := srv.AuthGET("/api/v1/shows/"+showID+"/matcher", member, nil); status != http.StatusForbidden {
		t.Errorf("member GET matcher = %d, want 403; body: %s", status, body)
	}
	if status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", member,
		map[string]any{"files": []map[string]any{}}, nil); status != http.StatusForbidden {
		t.Errorf("member PUT matcher = %d, want 403; body: %s", status, body)
	}
	if status, body := srv.AuthGET("/api/v1/shows/"+showID+"/seriesSeasons?externalId=1438", member, nil); status != http.StatusForbidden {
		t.Errorf("member GET seriesSeasons = %d, want 403; body: %s", status, body)
	}
}

// TestMatcherUnknownShowIsNotFound: an unknown id is 404 on every verb — hide
// existence, never 403 (api-contract.md).
func TestMatcherUnknownShowIsNotFound(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, _ := scanMatcherLibrary(t)

	if status, _ := srv.AuthGET("/api/v1/shows/nope/matcher", token, nil); status != http.StatusNotFound {
		t.Errorf("GET unknown show matcher = %d, want 404", status)
	}
	if status, _ := srv.JSON(http.MethodPut, "/api/v1/shows/nope/matcher", token,
		map[string]any{"files": []map[string]any{}}, nil); status != http.StatusNotFound {
		t.Errorf("PUT unknown show matcher = %d, want 404", status)
	}
	if status, _ := srv.AuthGET("/api/v1/shows/nope/seriesSeasons?externalId=1", token, nil); status != http.StatusNotFound {
		t.Errorf("GET unknown show seriesSeasons = %d, want 404", status)
	}
}
