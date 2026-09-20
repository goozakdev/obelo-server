package enrich

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0053: "An Album corroborates its Artist." — THE HOST'S HALF.
//
// The bug this file exists for was measured, not imagined. The Artist row for "The
// Eagles" in a real library held a4852e21-…, which MusicBrainz calls "The Eagles —
// 1960s UK instrumental group", British, formed 1958. The American band is named
// "Eagles". The source searched artist:"The Eagles" as an exact phrase, took
// Artists[0], and stored it with no acceptance test — so the row read `matched`,
// nothing ever flagged it, and the damage surfaced as thirteen unmatched tracks on
// Hell Freezes Over three levels down.
//
// The fix has two halves in two places, and only the second is still here
// (.scratch/bundled-plugins issue 06):
//
//   - the SOURCE asks the discography before it asks the name. Every assertion
//     about that — the decoy artist, the unnarrowed corroborating query on the
//     wire, the one-call cost, the title test that bounds it, the 404 and the
//     outage — moved to plugins/musicbrainz/musicbrainz with the provider.
//   - the PASS reads a Library's Albums BEFORE it enriches the Artist, so the ref
//     it hands the source CARRIES the album that identifies it. Without that the
//     source has the mechanism and nothing to run it on, and that is what the two
//     pass tests at the bottom hold, over a real migrated database.
//
// The fake below is a source that corroborates the way the MusicBrainz plugin
// does. It is here because these tests are about the REF the pass builds and the
// ROW the pass writes, and both are observable only through a source that reads
// the one and produces the other.

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

// eaglesSource is the motivating library as a source: both bands exist, only one of
// them recorded the album, and the local Artist's name is spelled exactly like the
// wrong one's.
//
// It applies ADR-0053's precedence the way the MusicBrainz plugin does — a pinned
// id first, then the album that carries a release-group id, then the album that
// carries only a title, then the name — and RECORDS each step in the vocabulary the
// wire used, so a pass test can still say "no name search happened" and "nothing
// asked for inc=artist-credits".
type eaglesSource struct {
	fakeProvider
	mu   sync.Mutex
	wire []string
	// status forces a failure on the call whose recorded name starts with the key —
	// the shape a 503 takes at this seam. The value is the HTTP status the source
	// would have answered, and any of them produces a TRANSIENT error, because that
	// is the only kind a retryable status can produce (ADR-0048). It is a map of
	// statuses rather than of errors so the tests that set it read as they did when
	// the fake was an httptest server.
	status map[string]int
}

// note records one call and reports whether the test forced it to fail.
func (s *eaglesSource) note(call string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire = append(s.wire, call)
	for prefix, code := range s.status {
		if strings.HasPrefix(call, prefix) {
			return statusError("musicbrainz", call, code)
		}
	}
	return nil
}

// calls returns every request the source made, in order.
func (s *eaglesSource) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.wire...)
}

// count returns how many recorded calls start with prefix.
func (s *eaglesSource) count(prefix string) int {
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
func (s *eaglesSource) nameSearches() int { return s.count("/artist?") }

// searches counts every call to a SEARCH endpoint, as opposed to a lookup. This is
// ADR-0049's currency: the search cluster is the dependency that sheds load
// globally while the lookup endpoints answer normally.
func (s *eaglesSource) searches() int { return s.count("/artist?") + s.count("/release-group?") }

func (s *eaglesSource) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind != "artist" {
		return TitleMetadata{}, ErrNoMatch
	}
	if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
		return s.artistByID(id)
	}
	// The discography, one hint only: the first that carries a release-group id,
	// else the first that carries a title.
	for _, h := range ref.AlbumHints {
		if isUUID(strings.TrimSpace(h.ReleaseGroupMBID)) {
			if err := s.note("/release-group/" + h.ReleaseGroupMBID + "?inc=artist-credits"); err != nil {
				// An OUTAGE, not an answer: it must NOT fall through to the name search,
				// which is exactly how the wrong band gets stored `matched` while the
				// source is shedding load.
				return TitleMetadata{}, err
			}
			if h.ReleaseGroupMBID == hellFreezesOverRGID {
				return s.artistByID(americanEaglesMBID)
			}
			return s.artistByName(ref.Title) // a stale id corroborates nothing
		}
	}
	for _, h := range ref.AlbumHints {
		if strings.TrimSpace(h.Title) != "" {
			if err := s.note("/release-group?query=" + h.Title); err != nil {
				return TitleMetadata{}, err
			}
			if acceptsTitle(h.Title, "Hell Freezes Over") {
				return s.artistByID(americanEaglesMBID)
			}
			return s.artistByName(ref.Title)
		}
	}
	return s.artistByName(ref.Title)
}

func (s *eaglesSource) artistByID(id string) (TitleMetadata, error) {
	if err := s.note("/artist/" + id + "?inc=tags"); err != nil {
		return TitleMetadata{}, err
	}
	switch id {
	case americanEaglesMBID:
		return TitleMetadata{Matched: true, Name: "Eagles", ExternalID: id, Source: "musicbrainz",
			Genres: []string{"rock", "country rock"}}, nil
	case britishEaglesMBID:
		return TitleMetadata{Matched: true, Name: "The Eagles", ExternalID: id, Source: "musicbrainz"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

// artistByName is the last tier, and it is confidently wrong on this library: the
// only artist NAMED "The Eagles" is the 1960s British instrumental group.
func (s *eaglesSource) artistByName(name string) (TitleMetadata, error) {
	if err := s.note(`/artist?query=artist:"` + name + `"`); err != nil {
		return TitleMetadata{}, err
	}
	if strings.TrimSpace(name) == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	if name == "The Eagles" {
		return TitleMetadata{Matched: true, Name: "The Eagles", ExternalID: britishEaglesMBID,
			Source: "musicbrainz"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

// --- the hints the pass builds ------------------------------------------------

// musicAlbumHints is deterministic and capped: two passes ask the source the same
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
		t.Error("two calls produced different hints — the question asked of the source must " +
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
// against the two-Eagles source.
func newEaglesLibrary(t *testing.T, rgTag string, seedArtist func(exec func(string, ...any))) (*Service, *store.DB, *eaglesSource) {
	t.Helper()
	p := &eaglesSource{}
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
	return svc, db, p
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
