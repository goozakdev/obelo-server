package enrich

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// ADR-0060 decisions 5, 6 and 7 as a pass sees them (.scratch/bundled-plugins issue
// 15): a record pins its item to its namespace's provider only when it is a
// DECISION, the host stamps every id it writes with the namespace of the provider
// it asked, and a lookup is handed every id the host holds, keyed by namespace.
//
// The sources here are Plugins in all but binary: wire-level stand-ins registered
// under the shipped providers' own Descriptors, built by the per-Library resolver
// exactly as a real server builds them, so the pinned-provider path is the real one
// (Catalog.newProvider) and every ref a source sees is the one that crossed the
// contract.

// nsSource is a stand-in Metadata provider Plugin whose ids live in namespace ns. It
// answers a lookup BY an id in its own namespace when it knows the id, and a lookup
// with no id of its own by the item's name. It records every ref it was handed.
type nsSource struct {
	ns     string
	byID   map[string]bool   // ids in ns this source has a record for
	byName map[string]string // title → the id in ns a name search resolves to

	mu    sync.Mutex
	asked []pluginapi.MediaRef
}

func (s *nsSource) Lookup(_ context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	s.mu.Lock()
	s.asked = append(s.asked, req.Ref)
	s.mu.Unlock()
	id := req.Ref.ID(s.ns)
	if id == "" {
		id = s.byName[req.Ref.Title]
	}
	if id == "" || !s.byID[id] {
		return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
	return pluginapi.LookupResponse{Outcome: pluginapi.OutcomeMatched, Record: pluginapi.MetadataRecord{
		Matched: true, Name: req.Ref.Title, ExternalID: id, Source: s.ns,
		Overview: "from " + s.ns,
	}}, nil
}

func (s *nsSource) Search(context.Context, pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	return pluginapi.SearchResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

func (s *nsSource) ArtworkCandidates(context.Context, pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	return pluginapi.ArtworkCandidatesResponse{Outcome: pluginapi.OutcomeMatched}, nil
}

// refsFor is every ref this source was handed for an item named title.
func (s *nsSource) refsFor(title string) []pluginapi.MediaRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pluginapi.MediaRef
	for _, r := range s.asked {
		if r.Title == title {
			out = append(out, r)
		}
	}
	return out
}

func (s *nsSource) forget() {
	s.mu.Lock()
	s.asked = nil
	s.mu.Unlock()
}

// altMusicSlug is a third-party music source: a Full music provider the core never
// shipped, so a music Library can be repointed at all.
const altMusicSlug = "altmusic"

// nsFixture is a Service over a real migrated DB whose per-Library resolver builds
// the chain from a Catalog of nsSources, led by whatever the test currently says.
type nsFixture struct {
	svc *Service
	db  *store.DB

	mu        sync.Mutex
	videoLead string
	musicLead string
	inactive  map[string]bool // providers muted for the Library (unreachable)
}

func newNSFixture(t *testing.T, sources ...*nsSource) *nsFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	reg := pluginapi.NewRegistry()
	for _, src := range sources {
		src := src
		var d pluginapi.Descriptor
		if src.ns == altMusicSlug {
			d = pluginapi.Descriptor{Slug: altMusicSlug, Name: "Alt Music", Kinds: []string{KindMusic},
				Role: RoleAuthoritative, Class: ClassFull}
		} else {
			d = bundledStandIn(src.ns).Descriptor
		}
		reg.RegisterMetadataProvider(pluginapi.MetadataProviderRegistration{
			Descriptor: d,
			New:        func(pluginapi.Settings) (pluginapi.MetadataProvider, error) { return src, nil },
		})
	}
	cat := NewCatalog(reg)

	f := &nsFixture{db: db, videoLead: SlugTMDB, musicLead: SlugMusicBrainz, inactive: map[string]bool{}}
	f.svc = NewService(db, CompositeProvider{}, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	f.svc.SetClock(func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) })
	f.svc.resolveLibrary = func(context.Context, string) (providerSnapshot, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		cfg := testConfig(withVideoLead(f.videoLead))
		cfg.AuthoritativeMusic = f.musicLead
		for _, src := range sources {
			withActive(src.ns, !f.inactive[src.ns])(&cfg)
		}
		return providerSnapshot{
			provider: CompositeProvider{
				Video: cat.newProvider(cfg, cfg.videoAuthoritativeSlug(), KindVideo),
				Music: cat.newProvider(cfg, cfg.musicAuthoritativeSlug(), KindMusic),
			},
			enablement: Enablement{Video: true, Music: true},
			catalog:    cat,
			config:     cfg,
		}, nil
	}
	return f
}

func (f *nsFixture) repoint(video, music string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if video != "" {
		f.videoLead = video
	}
	if music != "" {
		f.musicLead = music
	}
}

func (f *nsFixture) mute(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inactive[slug] = true
}

func (f *nsFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		t.Fatalf("seed (%s): %v", q, err)
	}
}

func (f *nsFixture) title(t *testing.T, id string) store.Title {
	t.Helper()
	got, err := f.db.TitleForEnrichmentByID(id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return got
}

func (f *nsFixture) pass(t *testing.T, libraryID string, mode Mode) {
	t.Helper()
	if _, err := f.svc.EnrichLibrary(context.Background(), libraryID, mode); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

// seedMovies files two Movies in a movie Library.
func (f *nsFixture) seedMovies(t *testing.T) {
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	f.exec(t, `INSERT INTO titles (id, library_id, kind, title, year, identity_key, sort_title)
	           VALUES ('auto', 'lib', 'movie', 'Inception', 2010, 'inception|2010', 'inception')`)
	f.exec(t, `INSERT INTO titles (id, library_id, kind, title, year, identity_key, sort_title)
	           VALUES ('fixed', 'lib', 'movie', 'The Matrix', 1999, 'the matrix|1999', 'matrix')`)
}

func videoSources() (tmdb, anidb *nsSource) {
	tmdb = &nsSource{ns: SlugTMDB,
		byID:   map[string]bool{"27205": true, "603": true, "604": true, "1399": true},
		byName: map[string]string{"Inception": "27205", "The Matrix": "603"}}
	anidb = &nsSource{ns: SlugAniDB,
		byID:   map[string]bool{"a-1": true, "a-2": true, "a-show": true},
		byName: map[string]string{"Inception": "a-1", "The Matrix": "a-2", "Game of Thrones": "a-show"}}
	return tmdb, anidb
}

// A Library repointed TMDB → AniDB re-resolves an auto-matched Movie via AniDB —
// the record, row and namespace, is REPLACED — but keeps a Fix-info'd Movie on
// TMDB. And the AniDB-led record lands in namespace `anidb` and is looked up BY
// AniDB on the next pass, never at TMDB.
func TestARepointedVideoLibraryReResolvesOnlyWhatNobodyDecided(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.seedMovies(t)
	ctx := context.Background()

	// TMDB leads: both Movies resolve by name, into `tmdb`.
	f.pass(t, "lib", ModeNew)
	if got := f.title(t, "auto"); got.RecordNamespace != "tmdb" || got.RecordID("tmdb") != "27205" {
		t.Fatalf("auto-matched record = %q %v, want tmdb 27205", got.RecordNamespace, got.RecordIDs)
	}
	// The Admin Fix-infos The Matrix to 604, naming the lead's namespace.
	if err := f.svc.ApplyOverride(ctx, "fixed", "604", SlugTMDB); err != nil {
		t.Fatalf("apply override: %v", err)
	}
	if got := f.title(t, "fixed"); got.RecordNamespace != "tmdb" || got.RecordID("tmdb") != "604" ||
		got.EnrichmentIDOrigin != store.OriginChosen {
		t.Fatalf("Fix-info'd record = %q %v (%q), want chosen tmdb 604", got.RecordNamespace, got.RecordIDs,
			got.EnrichmentIDOrigin)
	}

	// Repoint the Library at AniDB and refresh it.
	f.repoint(SlugAniDB, "")
	tmdb.forget()
	anidb.forget()
	f.pass(t, "lib", ModeFull)

	auto := f.title(t, "auto")
	if auto.RecordNamespace != "anidb" || !reflect.DeepEqual(auto.RecordIDs, map[string]string{"anidb": "a-1"}) {
		t.Errorf("auto-matched Movie after the repoint: record %q %v, want the AniDB aid REPLACING the TMDB id "+
			"(ADR-0060 decision 6)", auto.RecordNamespace, auto.RecordIDs)
	}
	if auto.EnrichmentStatus != "matched" || auto.EnrichmentIDOrigin != store.OriginDerived {
		t.Errorf("auto-matched Movie: status %q origin %q, want matched and still nobody's choice",
			auto.EnrichmentStatus, auto.EnrichmentIDOrigin)
	}
	if n := len(tmdb.refsFor("Inception")); n != 0 {
		t.Errorf("TMDB was asked about the auto-matched Movie %d time(s) after the repoint; an id a pass "+
			"resolved is not a pin", n)
	}
	if n := len(anidb.refsFor("Inception")); n != 1 {
		t.Errorf("AniDB was asked about the auto-matched Movie %d time(s), want 1", n)
	}

	fixed := f.title(t, "fixed")
	if fixed.RecordNamespace != "tmdb" || fixed.RecordID("tmdb") != "604" || fixed.EnrichmentStatus != "matched" {
		t.Errorf("Fix-info'd Movie after the repoint: %q %v %q, want still matched on tmdb 604",
			fixed.RecordNamespace, fixed.RecordIDs, fixed.EnrichmentStatus)
	}
	if refs := tmdb.refsFor("The Matrix"); len(refs) != 1 || refs[0].ID("tmdb") != "604" {
		t.Errorf("the Fix-info'd Movie was not resolved BY its pinned record at TMDB: %+v", refs)
	}
	if n := len(anidb.refsFor("The Matrix")); n != 0 {
		t.Errorf("AniDB was asked about the Fix-info'd Movie %d time(s); a chosen record pins its namespace", n)
	}

	// The next pass looks the AniDB record up BY AniDB — in its own namespace — and
	// never at TMDB.
	tmdb.forget()
	anidb.forget()
	f.pass(t, "lib", ModeFull)
	refs := anidb.refsFor("Inception")
	if len(refs) != 1 || refs[0].ID("anidb") != "a-1" {
		t.Errorf("AniDB's next lookup of its own record = %+v, want it BY anidb a-1", refs)
	}
	if refs := tmdb.refsFor("Inception"); len(refs) != 0 {
		t.Errorf("TMDB was asked about an AniDB record: %+v", refs)
	}
	if got := f.title(t, "auto"); got.RecordNamespace != "anidb" || got.RecordID("tmdb") != "" {
		t.Errorf("record after the second pass = %q %v, want anidb alone", got.RecordNamespace, got.RecordIDs)
	}
}

// A folder token is a decision too: a `{tmdb-…}` Movie keeps resolving at TMDB in a
// Library that now leads with AniDB.
func TestAFolderTokenPinsItsNamespace(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	f.exec(t, `INSERT INTO titles (id, library_id, kind, title, year, identity_key, sort_title, tmdb_id)
	           VALUES ('tok', 'lib', 'movie', 'The Matrix', 1999, 'tmdb:603', 'matrix', '603')`)
	f.repoint(SlugAniDB, "")

	f.pass(t, "lib", ModeNew)
	if refs := tmdb.refsFor("The Matrix"); len(refs) != 1 || refs[0].ID("tmdb") != "603" {
		t.Errorf("a {tmdb-603} Movie in an AniDB-led Library: TMDB saw %+v, want one lookup BY 603", refs)
	}
	if n := len(anidb.refsFor("The Matrix")); n != 0 {
		t.Errorf("AniDB was asked about a folder-pinned Movie %d time(s)", n)
	}
}

// An orphaned chosen record goes to the attention list: its provider is muted for
// the Library, so the Title is filed 'unmatched' with no reason and nobody is asked.
func TestAnOrphanedChosenRecordGoesToTheAttentionList(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.seedMovies(t)
	if err := f.svc.ApplyOverride(context.Background(), "fixed", "604", "tmdb"); err != nil {
		t.Fatalf("apply override: %v", err)
	}

	f.repoint(SlugAniDB, "")
	f.mute(SlugTMDB)
	tmdb.forget()
	anidb.forget()
	f.pass(t, "lib", ModeFull)

	got := f.title(t, "fixed")
	if got.EnrichmentStatus != "unmatched" || got.EnrichmentReason != store.EnrichmentReasonNone {
		t.Errorf("orphaned override: status %q reason %q, want unmatched with no reason",
			got.EnrichmentStatus, got.EnrichmentReason)
	}
	if got.RecordNamespace != "tmdb" || got.RecordID("tmdb") != "604" {
		t.Errorf("orphaned override lost its record: %q %v", got.RecordNamespace, got.RecordIDs)
	}
	if n := len(tmdb.refsFor("The Matrix")) + len(anidb.refsFor("The Matrix")); n != 0 {
		t.Errorf("an orphaned override was looked up %d time(s); it must be filed, not resolved elsewhere", n)
	}
	// The auto-matched neighbour is no pin, so the mute does not touch it.
	if got := f.title(t, "auto"); got.EnrichmentStatus != "matched" || got.RecordNamespace != "anidb" {
		t.Errorf("auto-matched neighbour: %q %q, want matched via the new lead", got.EnrichmentStatus, got.RecordNamespace)
	}
}

// A parent pinned to TMDB in a repointed Library resolves via TMDB, in its own
// namespace; the Library's new lead is not asked.
func TestAParentPinnedToTMDBInARepointedLibraryResolvesViaTMDB(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	           VALUES ('sh1', 'tv', 'Game of Thrones', 'game of thrones', 'game of thrones')`)
	ctx := context.Background()

	if err := f.svc.ApplyEntityOverride(ctx, store.EntityShow, "sh1", EntityPin{ExternalID: "1399", Namespace: SlugTMDB}); err != nil {
		t.Fatalf("apply entity override: %v", err)
	}
	e, err := f.db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatal(err)
	}
	if e.ExternalID != "1399" || e.Namespace != "tmdb" {
		t.Fatalf("pinned Show = %q in %q, want 1399 in the lead's namespace tmdb", e.ExternalID, e.Namespace)
	}

	f.repoint(SlugAniDB, "")
	tmdb.forget()
	anidb.forget()
	f.pass(t, "tv", ModeFull)

	if refs := tmdb.refsFor("Game of Thrones"); len(refs) != 1 || refs[0].ID("tmdb") != "1399" {
		t.Errorf("the pinned Show at TMDB: %+v, want one lookup BY tmdb 1399", refs)
	}
	if n := len(anidb.refsFor("Game of Thrones")); n != 0 {
		t.Errorf("the Library's new lead was asked about a Show pinned to TMDB %d time(s)", n)
	}
	e, err = f.db.EntityEnrichmentByID(store.EntityShow, "sh1")
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != "matched" || e.ExternalID != "1399" || e.Namespace != "tmdb" {
		t.Errorf("pinned Show after the pass: %q %q in %q, want matched 1399 in tmdb", e.Status, e.ExternalID, e.Namespace)
	}
}

// A Show's FOLDER token pins it, exactly as a Title's does (ADR-0060 decision 6):
// in an AniDB-led Library a {tmdb-1399} Show is looked up BY 1399 at TMDB and its
// record is stored in `tmdb`, while a token-less neighbour goes to the new lead —
// so it is the token that pins, not the Show's kind. With TMDB unreachable the
// folder-pinned Show is ORPHANED and nobody is asked.
func TestAShowsFolderTokenPinsItsNamespace(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title, tmdb_id)
	           VALUES ('tok', 'tv', 'Game of Thrones', 'tmdb:1399', 'game of thrones', '1399')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	           VALUES ('bare', 'tv', 'Inception', 'inception', 'inception')`)
	f.repoint(SlugAniDB, "")

	f.pass(t, "tv", ModeFull)
	if refs := tmdb.refsFor("Game of Thrones"); len(refs) != 1 || refs[0].ID("tmdb") != "1399" {
		t.Errorf("a {tmdb-1399} Show in an AniDB-led Library: TMDB saw %+v, want one lookup BY 1399", refs)
	}
	if n := len(anidb.refsFor("Game of Thrones")); n != 0 {
		t.Errorf("AniDB was asked about a folder-pinned Show %d time(s)", n)
	}
	e, err := f.db.EntityEnrichmentByID(store.EntityShow, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != "matched" || e.ExternalID != "1399" || e.Namespace != "tmdb" {
		t.Errorf("folder-pinned Show: %q %q in %q, want matched 1399 in tmdb", e.Status, e.ExternalID, e.Namespace)
	}
	if e.ExternalIDOrigin.Locked() {
		t.Errorf("a folder pin wrote origin %q; the folder holds the id, nobody chose it", e.ExternalIDOrigin)
	}
	bare, err := f.db.EntityEnrichmentByID(store.EntityShow, "bare")
	if err != nil {
		t.Fatal(err)
	}
	if bare.ExternalID != "a-1" || bare.Namespace != "anidb" {
		t.Errorf("token-less Show: %q in %q, want a-1 in anidb via the new lead", bare.ExternalID, bare.Namespace)
	}

	f.mute(SlugTMDB)
	tmdb.forget()
	anidb.forget()
	f.pass(t, "tv", ModeFull)
	if n := len(tmdb.refsFor("Game of Thrones")) + len(anidb.refsFor("Game of Thrones")); n != 0 {
		t.Errorf("a folder-pinned Show whose provider is unreachable was looked up %d time(s)", n)
	}
	if e, err = f.db.EntityEnrichmentByID(store.EntityShow, "tok"); err != nil {
		t.Fatal(err)
	}
	if e.Status != "unmatched" {
		t.Errorf("orphaned folder-pinned Show: status %q, want unmatched", e.Status)
	}
}

// refsOfKind is every ref of one entity kind this source was handed.
func (s *nsSource) refsOfKind(kind string) []pluginapi.MediaRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []pluginapi.MediaRef
	for _, r := range s.asked {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// A pinned Show's Seasons and Episodes follow it to its provider: in an AniDB-led
// Library, a Show pinned to TMDB — by its folder token or by an Admin's choice —
// has its Season and Episode asked of TMDB, BY the TMDB series id, and AniDB never
// sees them. A token-less, unpinned Show next door still goes to the lead with its
// children, so it is the Show's pin they follow, not TMDB.
func TestAPinnedShowsSeasonsAndEpisodesFollowIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tmdbID string // the Show's folder token, or "" for an Admin-chosen pin
	}{{"folder token", "1399"}, {"chosen record", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			tmdb, anidb := videoSources()
			f := newNSFixture(t, tmdb, anidb)
			f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
			f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title, tmdb_id)
			           VALUES ('sh1', 'tv', 'Game of Thrones', 'got', 'got', ?)`, tc.tmdbID)
			f.exec(t, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se1', 'sh1', 1, 'got|s01')`)
			f.exec(t, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
			             season_id, season_number, episode_number)
			           VALUES ('ep1', 'tv', 'episode', 'Winter Is Coming', 'got|e1', 'winter', 'se1', 1, 1)`)
			f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
			           VALUES ('sh2', 'tv', 'Inception', 'inception', 'inception')`)
			f.exec(t, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se2', 'sh2', 1, 'inc|s01')`)
			f.exec(t, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
			             season_id, season_number, episode_number)
			           VALUES ('ep2', 'tv', 'episode', 'Dream Within', 'inc|e1', 'dream', 'se2', 1, 1)`)
			if tc.tmdbID == "" {
				if err := f.svc.ApplyEntityOverride(context.Background(), store.EntityShow, "sh1",
					EntityPin{ExternalID: "1399", Namespace: "tmdb"}); err != nil {
					t.Fatalf("apply entity override: %v", err)
				}
			}
			f.repoint(SlugAniDB, "")
			tmdb.forget()
			anidb.forget()

			f.pass(t, "tv", ModeFull)
			if refs := tmdb.refsOfKind("season"); len(refs) != 1 || refs[0].ID("tmdb") != "1399" {
				t.Errorf("the pinned Show's Season at TMDB: %+v, want one lookup under series 1399", refs)
			}
			if refs := tmdb.refsFor("Winter Is Coming"); len(refs) != 1 || refs[0].ID("tmdb") != "1399" {
				t.Errorf("the pinned Show's Episode at TMDB: %+v, want one lookup under series 1399", refs)
			}
			for _, r := range anidb.refsOfKind("season") {
				if r.ID("tmdb") == "1399" {
					t.Errorf("AniDB was handed the pinned Show's Season: %+v", r)
				}
			}
			if n := len(anidb.refsFor("Winter Is Coming")); n != 0 {
				t.Errorf("AniDB was asked about the pinned Show's Episode %d time(s)", n)
			}
			if got := f.title(t, "ep1"); got.EnrichmentStatus != "matched" {
				t.Errorf("the pinned Show's Episode: status %q, want matched via TMDB", got.EnrichmentStatus)
			}
			// The unpinned neighbour and its children stay with the lead.
			if n := len(anidb.refsFor("Dream Within")); n != 1 {
				t.Errorf("the unpinned Show's Episode reached the lead %d time(s), want 1", n)
			}
			if n := len(tmdb.refsFor("Dream Within")); n != 0 {
				t.Errorf("TMDB was asked about an unpinned Show's Episode %d time(s)", n)
			}
		})
	}
}

// A repointed music Library re-resolves auto-matched Tracks via its new lead, and a
// Track whose recording the Admin chose keeps resolving at MusicBrainz — the music
// pin used to be a no-op that reported whatever led.
func TestARepointedMusicLibraryReResolvesAutoMatchedTracks(t *testing.T) {
	mb := &nsSource{ns: SlugMusicBrainz, byID: map[string]bool{"rec-1": true, "rec-2": true}}
	alt := &nsSource{ns: altMusicSlug,
		byID:   map[string]bool{"alt-1": true, "alt-2": true},
		byName: map[string]string{"Airbag": "alt-1", "Paranoid Android": "alt-2"}}
	f := newNSFixture(t, mb, alt)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
	f.exec(t, `INSERT INTO artists (id, library_id, name, identity_key, sort_name)
	           VALUES ('ar1', 'lib', 'Radiohead', 'artist:radiohead', 'radiohead')`)
	f.exec(t, `INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
	           VALUES ('al1', 'ar1', 'OK Computer', 'artist:radiohead|album:ok computer', 'ok computer')`)
	seedTrack := func(id, title string, num int, record string, origin store.RecordOrigin) {
		f.exec(t, `INSERT INTO titles
		             (id, library_id, kind, title, identity_key, sort_title, album_id, disc_number, track_number,
		              enrichment_id_origin, enrichment_id_namespace, enrichment_status)
		           VALUES (?, 'lib', 'track', ?, ?, ?, 'al1', 1, ?, ?, 'musicbrainz', 'matched')`,
			id, title, "radiohead|ok computer|"+id, title, num, string(origin))
		f.exec(t, `INSERT INTO title_external_ids (title_id, namespace, external_id) VALUES (?, 'musicbrainz', ?)`,
			id, record)
	}
	seedTrack("t-auto", "Airbag", 1, "rec-1", store.OriginDerived)
	seedTrack("t-chosen", "Paranoid Android", 2, "rec-2", store.OriginChosen)

	f.repoint("", altMusicSlug)
	f.pass(t, "lib", ModeFull)

	auto := f.title(t, "t-auto")
	if auto.RecordNamespace != altMusicSlug || !reflect.DeepEqual(auto.RecordIDs, map[string]string{altMusicSlug: "alt-1"}) {
		t.Errorf("auto-matched Track after the repoint: %q %v, want the new lead's id replacing the MBID",
			auto.RecordNamespace, auto.RecordIDs)
	}
	if n := len(mb.refsFor("Airbag")); n != 0 {
		t.Errorf("MusicBrainz was asked about an auto-matched Track %d time(s) after the repoint", n)
	}
	chosen := f.title(t, "t-chosen")
	if chosen.RecordNamespace != "musicbrainz" || chosen.RecordID("musicbrainz") != "rec-2" ||
		chosen.EnrichmentStatus != "matched" {
		t.Errorf("chosen Track after the repoint: %q %v %q, want still matched on musicbrainz rec-2",
			chosen.RecordNamespace, chosen.RecordIDs, chosen.EnrichmentStatus)
	}
	if refs := mb.refsFor("Paranoid Android"); len(refs) != 1 || refs[0].ID("musicbrainz") != "rec-2" {
		t.Errorf("the chosen Track at MusicBrainz: %+v, want one lookup BY rec-2", refs)
	}
	if n := len(alt.refsFor("Paranoid Android")); n != 0 {
		t.Errorf("the new lead was asked about a chosen Track %d time(s)", n)
	}
}

// An Episode pin inherits its Show's namespace, and a Show Cascade stamps its
// Episodes with it too (ADR-0060 decision 5).
func TestAnEpisodePinAndACascadeInheritTheShowsNamespace(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	           VALUES ('sh1', 'tv', 'Game of Thrones', 'game of thrones', 'game of thrones')`)
	f.exec(t, `INSERT INTO seasons (id, show_id, season_number, identity_key) VALUES ('se1', 'sh1', 1, 'got|s01')`)
	for _, ep := range []string{"e1", "e2"} {
		f.exec(t, `INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
		             season_id, season_number, episode_number)
		           VALUES (?, 'tv', 'episode', ?, ?, ?, 'se1', 1, ?)`, ep, ep, "got|"+ep, ep, len(ep))
	}
	f.repoint(SlugAniDB, "")
	ctx := context.Background()
	if err := f.svc.ApplyEntityOverride(ctx, store.EntityShow, "sh1", EntityPin{ExternalID: "a-show", Namespace: SlugAniDB}); err != nil {
		t.Fatalf("apply entity override: %v", err)
	}

	if err := f.svc.ApplyEpisodeOverride(ctx, "e1", "a-show", 1, 3); err != nil {
		t.Fatalf("episode pin: %v", err)
	}
	if got := f.title(t, "e1"); got.RecordNamespace != "anidb" || got.RecordID("anidb") != "a-show" {
		t.Errorf("Episode pin recorded %q %v, want the Show's namespace anidb", got.RecordNamespace, got.RecordIDs)
	}

	if _, err := f.svc.CascadeEntity(ctx, store.EntityShow, "sh1", "a-show"); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if got := f.title(t, "e2"); got.RecordNamespace != "anidb" || got.RecordID("anidb") != "a-show" ||
		got.EnrichmentIDOrigin != store.OriginCascaded {
		t.Errorf("cascaded Episode recorded %q %v (%q), want cascaded a-show in anidb",
			got.RecordNamespace, got.RecordIDs, got.EnrichmentIDOrigin)
	}
}

// An override always names its namespace: a direct Service call naming none is
// ErrUnknownNamespace, not a silent fall to the lead.
func TestApplyOverrideWithNoNamespaceIsRefused(t *testing.T) {
	tmdb, anidb := videoSources()
	f := newNSFixture(t, tmdb, anidb)
	f.seedMovies(t)
	ctx := context.Background()

	if err := f.svc.ApplyOverride(ctx, "fixed", "604", ""); !errors.Is(err, ErrUnknownNamespace) {
		t.Errorf("ApplyOverride with no namespace = %v, want ErrUnknownNamespace", err)
	}

	f.exec(t, `INSERT INTO libraries (id, name, kind) VALUES ('tv', 'TV', 'tv')`)
	f.exec(t, `INSERT INTO shows (id, library_id, title, identity_key, sort_title)
	           VALUES ('sh1', 'tv', 'Game of Thrones', 'game of thrones', 'game of thrones')`)
	if err := f.svc.ApplyEntityOverride(ctx, store.EntityShow, "sh1", EntityPin{ExternalID: "1399"}); !errors.Is(err, ErrUnknownNamespace) {
		t.Errorf("ApplyEntityOverride with no namespace = %v, want ErrUnknownNamespace", err)
	}
}

// refWithPinnedEntityID no longer defaults a blank namespace to a kind's lead: every
// current caller's namespace already names the record (storedParentRecord and
// showAssertedRecord never hand back a non-empty id with a blank namespace — an
// EntityEnrichment's own contract is "Namespace is \"\" exactly when ExternalID is
// empty", store/enrich_entities.go). A blank namespace here is just withExternalID's
// existing no-op guard, proven by a ref that carries NO id for it rather than one
// silently filed under tmdb/musicbrainz.
func TestRefWithPinnedEntityIDDoesNotDefaultABlankNamespace(t *testing.T) {
	ref := refWithPinnedEntityID(TitleRef{Kind: "show"}, "", "1399")
	if ref.TMDBID != "" || ref.MusicbrainzID != "" || len(ref.ExternalIDs) != 0 {
		t.Errorf("ref with a blank namespace = %+v, want no id recorded at all", ref)
	}
}
