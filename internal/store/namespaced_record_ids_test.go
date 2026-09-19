package store_test

import (
	"reflect"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A Title's record ids are rows keyed by namespace, and the store's record API
// speaks namespaces (ADR-0060 decision 2, .scratch/bundled-plugins issue 14). The
// named-field spelling (TMDBID / IMDBID / MusicbrainzID) keeps meaning namespaces
// tmdb / imdb / musicbrainz; these tests pin the parts only a namespace can say.

func seedNamespacedMovie(t *testing.T, folderTMDB string) *store.DB {
	t.Helper()
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)
	tree := movieTree("m1", "dune|2021", "Dune", 2021, "/media/Movies/Dune (2021)/Dune (2021).mkv")
	tree.TMDBID = folderTMDB
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	return db
}

func readRecord(t *testing.T, db *store.DB) store.Title {
	t.Helper()
	got, err := db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatalf("reading m1: %v", err)
	}
	return got
}

// An Admin's pick in a namespace no named field spells is stored under it, is the
// record, and is NOT a TMDB id.
func TestAnExternalMatchInAnyNamespaceIsTheRecord(t *testing.T) {
	db := seedNamespacedMovie(t, "")
	if err := db.SetTitleExternalMatch("m1",
		store.ExternalMatch{Namespace: "anidb", ID: "4563"}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	got := readRecord(t, db)
	if !reflect.DeepEqual(got.RecordIDs, map[string]string{"anidb": "4563"}) || got.RecordNamespace != "anidb" {
		t.Errorf("record = %v in %q, want anidb 4563 as the record", got.RecordIDs, got.RecordNamespace)
	}
	if got.TMDBID != "" {
		t.Errorf("TMDBID = %q; an AniDB aid must never read as a TMDB id", got.TMDBID)
	}
	if got.EnrichmentIDOrigin != store.OriginChosen {
		t.Errorf("origin = %q, want chosen", got.EnrichmentIDOrigin)
	}
}

// ADR-0045's fill-only rule, per namespace: a pass fills only a namespace the Title
// holds nothing in, and a fill beside an existing record is a cross-reference —
// the record namespace does not move.
func TestAPassFillsOnlyEmptyNamespacesAndKeepsTheRecord(t *testing.T) {
	db := seedNamespacedMovie(t, "")
	if err := db.SetTitleExternalMatch("m1",
		store.ExternalMatch{Namespace: "anidb", ID: "4563"}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Source:      "anidb",
		ExternalIDs: store.ExternalMatch{Namespace: "anidb", ID: "9999", IMDBID: "tt1160419"},
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got := readRecord(t, db)
	want := map[string]string{"anidb": "4563", "imdb": "tt1160419"}
	if !reflect.DeepEqual(got.RecordIDs, want) {
		t.Errorf("record rows = %v, want %v (the Admin's aid kept, the IMDb id filled)", got.RecordIDs, want)
	}
	if got.RecordNamespace != "anidb" {
		t.Errorf("record namespace = %q, want anidb — a fill beside a record is not a new record",
			got.RecordNamespace)
	}
	if got.IMDBID != "tt1160419" {
		t.Errorf("IMDBID = %q, want the filled cross-reference", got.IMDBID)
	}
}

// A Title with no record gets one from its first fill, named in RECORD order.
func TestAFirstFillNamesTheRecord(t *testing.T) {
	db := seedNamespacedMovie(t, "")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Source: "tmdb", ExternalIDs: store.ExternalMatch{TMDBID: "438631", IMDBID: "tt1160419"},
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got := readRecord(t, db)
	if got.RecordNamespace != "tmdb" || got.TMDBID != "438631" || got.IMDBID != "tt1160419" {
		t.Errorf("record %q, tmdb %q, imdb %q; want tmdb as the record with both ids",
			got.RecordNamespace, got.TMDBID, got.IMDBID)
	}
}

// The folder's identity id outranks a pass's echo in its own namespace, and only
// there.
func TestAFoldersIDBlocksTheFillInItsNamespaceOnly(t *testing.T) {
	db := seedNamespacedMovie(t, "438631")
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Source: "tmdb", ExternalIDs: store.ExternalMatch{TMDBID: "999", IMDBID: "tt1160419"},
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	got := readRecord(t, db)
	if got.RecordID("tmdb") != "" || got.TMDBID != "438631" {
		t.Errorf("tmdb row %q / TMDBID %q; want no row and the folder's id", got.RecordID("tmdb"), got.TMDBID)
	}
	if got.RecordID("imdb") != "tt1160419" || got.RecordNamespace != "imdb" {
		t.Errorf("imdb row %q in namespace %q; want the IMDb id filled as the record",
			got.RecordID("imdb"), got.RecordNamespace)
	}
}

// Wrong item is a clean slate: every record row goes, and the namespace with them.
func TestRekeyDeletesEveryRecordRow(t *testing.T) {
	db := seedNamespacedMovie(t, "")
	if err := db.SetTitleExternalMatch("m1",
		store.ExternalMatch{Namespace: "anidb", ID: "4563", IMDBID: "tt1"}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	if err := db.RekeyTitleIdentity("m1", "Arrival", 2016, "329865", "tmdb:329865"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	got := readRecord(t, db)
	if len(got.RecordIDs) != 0 || got.RecordNamespace != "" {
		t.Errorf("after rekey: rows %v, namespace %q; want none", got.RecordIDs, got.RecordNamespace)
	}
	if got.TMDBID != "329865" {
		t.Errorf("TMDBID = %q, want the new identity id", got.TMDBID)
	}
}

// A parent's id carries its namespace: the caller's, else the Authoritative source
// that resolved it, else the kind's default lead.
func TestAParentsRecordCarriesItsNamespace(t *testing.T) {
	db := openTemp(t)
	read := func(kind, id string) store.EntityEnrichment {
		t.Helper()
		e, err := db.EntityEnrichmentByID(kind, id)
		if err != nil {
			t.Fatalf("reading %s %s: %v", kind, id, err)
		}
		return e
	}

	if err := db.WriteEntityEnrichment(store.EntityShow, "sh1",
		store.EntityEnrichmentWrite{Source: "anidb", ExternalID: "4563"}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := read(store.EntityShow, "sh1").Namespace; got != "anidb" {
		t.Errorf("an AniDB-resolved Show's namespace = %q, want anidb", got)
	}
	if err := db.WriteEntityEnrichment(store.EntityShow, "sh2",
		store.EntityEnrichmentWrite{Source: "omdb", ExternalID: "1399"}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := read(store.EntityShow, "sh2").Namespace; got != "tmdb" {
		t.Errorf("a Show with no Authoritative source reads %q, want the default tmdb", got)
	}
	if err := db.WriteEntityEnrichment(store.EntityAlbum, "al1",
		store.EntityEnrichmentWrite{Source: "musicbrainz", ExternalID: "rg-1", Namespace: "discogs"}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := read(store.EntityAlbum, "al1").Namespace; got != "discogs" {
		t.Errorf("an explicit namespace reads %q, want discogs", got)
	}

	if err := db.SetEntityExternalMatch(store.EntityArtist, "ar1",
		store.EntityRecordPin{ExternalID: "art-1", Origin: store.OriginChosen}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got := read(store.EntityArtist, "ar1").Namespace; got != "musicbrainz" {
		t.Errorf("a pinned Artist with no namespace reads %q, want musicbrainz", got)
	}
	if err := db.SetEntityExternalMatch(store.EntityShow, "sh1",
		store.EntityRecordPin{ExternalID: "", Origin: store.OriginChosen}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got := read(store.EntityShow, "sh1").Namespace; got != "" {
		t.Errorf("a parent with no id reads namespace %q, want none", got)
	}
}
