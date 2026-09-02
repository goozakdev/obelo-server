package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// entity_enrichment.external_release_id (migration 0057, ADR-0052) is the exact
// EDITION an ADMIN named for an Album — the release a pasted /release/ URL points
// at — stored beside the release-GROUP their choice resolved to.
//
// The whole reason it is a column of its own rather than a value written into
// albums.musicbrainz_release_id is the separation ADR-0045 exists to enforce and
// ADR-0049 refused this exact temptation to break: that column is SCANNER-owned and
// re-derived from disk on every scan, so an Admin's choice written there is erased
// by the next scan. The tests here assert that separation from both sides.

const (
	chosenEdition = "054a22c3-742e-34d3-8ebf-ef912e3679e6"
	chosenGroup   = "b84ee12a-09ef-421b-82de-0441a926375b"
)

// albumIDOf returns the row id of the seeded "She" album.
func albumIDOf(t *testing.T, db *store.DB) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`SELECT id FROM albums WHERE identity_key = 'harry connick jr|album:she'`,
	).Scan(&id); err != nil {
		t.Fatalf("read album id: %v", err)
	}
	return id
}

func chosenReleaseOf(t *testing.T, db *store.DB, albumID string) store.EntityEnrichment {
	t.Helper()
	e, err := db.EntityEnrichmentByID(store.EntityAlbum, albumID)
	if err != nil {
		t.Fatalf("read entity enrichment: %v", err)
	}
	return e
}

// THE REGRESSION GUARD. A rescan rewrites albums.musicbrainz_release_id from the
// files and must not go anywhere near the Admin's chosen edition. This is ADR-0045's
// bug in its music shape: one value, two owners, one of whom rewrites from disk.
func TestARescanDoesNotDisturbTheChosenEdition(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	if err := db.UpsertArtistTree(albumTree(standardRelease)); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	albumID := albumIDOf(t, db)
	var keyBefore string
	if err := db.QueryRow(`SELECT identity_key FROM albums WHERE id = ?`, albumID).Scan(&keyBefore); err != nil {
		t.Fatalf("read identity_key: %v", err)
	}

	// The operator pastes the /release/ URL of the edition they actually own.
	if err := db.SetEntityExternalMatch(store.EntityAlbum, albumID, store.EntityRecordPin{
		ExternalID: chosenGroup, ReleaseID: chosenEdition, Origin: store.OriginChosen,
	}); err != nil {
		t.Fatalf("pin the chosen edition: %v", err)
	}

	// Now the library is rescanned, and the files turn out to be tagged with a
	// DIFFERENT edition than the one the Admin chose — the very case that makes the
	// two values distinguishable at all.
	if err := db.UpsertArtistTree(albumTree(remasterRelease)); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	if got := releaseIDOf(t, db); got != remasterRelease {
		t.Errorf("albums.musicbrainz_release_id = %q after a retagged rescan, want %q — "+
			"the scanner's column is still scanner-owned and re-derived from disk", got, remasterRelease)
	}
	e := chosenReleaseOf(t, db, albumID)
	if e.ExternalReleaseID != chosenEdition {
		t.Fatalf("a rescan moved entity_enrichment.external_release_id to %q, want %q — this is "+
			"ADR-0045's bug: the Admin's choice erased by the next scan, which is the entire "+
			"reason this column is not albums.musicbrainz_release_id", e.ExternalReleaseID, chosenEdition)
	}
	if e.ChosenReleaseID() != chosenEdition {
		t.Errorf("ChosenReleaseID() = %q after a rescan, want %q", e.ChosenReleaseID(), chosenEdition)
	}
	if e.ExternalID != chosenGroup {
		t.Errorf("a rescan moved external_id to %q, want %q", e.ExternalID, chosenGroup)
	}

	// And identity did not move either (ADR-0038): the edition is a decoration
	// refinement and never enters a key, so no watch state was re-pointed.
	var keyAfter string
	if err := db.QueryRow(`SELECT identity_key FROM albums WHERE id = ?`, albumID).Scan(&keyAfter); err != nil {
		t.Fatalf("read identity_key: %v", err)
	}
	if keyAfter != keyBefore {
		t.Errorf("identity_key moved from %q to %q — a chosen edition must never enter a key",
			keyBefore, keyAfter)
	}
}

// The pass's own write is the other writer that could reach the column, and it must
// not: WriteEntityEnrichment refreshes the decorative fields on every re-enrich, and
// a re-enrich is exactly what applying the pin triggers.
func TestReEnrichingAnAlbumKeepsTheChosenEdition(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	if err := db.UpsertArtistTree(albumTree(standardRelease)); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	albumID := albumIDOf(t, db)
	if err := db.SetEntityExternalMatch(store.EntityAlbum, albumID, store.EntityRecordPin{
		ExternalID: chosenGroup, ReleaseID: chosenEdition, Origin: store.OriginChosen,
	}); err != nil {
		t.Fatalf("pin: %v", err)
	}

	if err := db.WriteEntityEnrichment(store.EntityAlbum, albumID, store.EntityEnrichmentWrite{
		Overview: "A 1994 album.", Source: "musicbrainz", ExternalID: chosenGroup,
	}, nil); err != nil {
		t.Fatalf("re-enrich: %v", err)
	}

	e := chosenReleaseOf(t, db, albumID)
	if e.ExternalReleaseID != chosenEdition {
		t.Errorf("a re-enrich moved external_release_id to %q, want %q — it is untouched by "+
			"the pass exactly as external_id_origin is", e.ExternalReleaseID, chosenEdition)
	}
	if !e.ExternalIDOrigin.Locked() {
		t.Errorf("a re-enrich dropped the origin to %q, want a Locked one", e.ExternalIDOrigin)
	}
	if e.Overview != "A 1994 album." {
		t.Errorf("overview = %q — the re-enrich should still have refreshed it", e.Overview)
	}
}

// An apply that names no edition CLEARS the stored one, in the same statement that
// writes the record. A pasted /release-group/ URL is the Admin naming a LESS specific
// thing, and an edition left behind under a new group would decorate the album from a
// stranger's tracklist.
func TestPinningWithNoEditionClearsTheStoredOne(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)
	if err := db.UpsertArtistTree(albumTree(standardRelease)); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	albumID := albumIDOf(t, db)
	if err := db.SetEntityExternalMatch(store.EntityAlbum, albumID, store.EntityRecordPin{
		ExternalID: chosenGroup, ReleaseID: chosenEdition, Origin: store.OriginChosen,
	}); err != nil {
		t.Fatalf("pin the edition: %v", err)
	}

	const otherGroup = "629a5133-b9e6-43c5-8cb6-594a7cbfbfed"
	if err := db.SetEntityExternalMatch(store.EntityAlbum, albumID, store.EntityRecordPin{
		ExternalID: otherGroup, Origin: store.OriginChosen,
	}); err != nil {
		t.Fatalf("re-pin the group alone: %v", err)
	}

	e := chosenReleaseOf(t, db, albumID)
	if e.ExternalID != otherGroup {
		t.Errorf("external_id = %q, want %q", e.ExternalID, otherGroup)
	}
	if e.ExternalReleaseID != "" || e.ChosenReleaseID() != "" {
		t.Errorf("external_release_id = %q after pinning a release-GROUP, want it cleared — a "+
			"stale edition under a new group decorates the album from a stranger's tracklist",
			e.ExternalReleaseID)
	}
}

// ChosenReleaseID is a named predicate rather than an `!= ""` test at each call
// site, because the question is "did a HUMAN name this edition" and not "is the
// column populated" — the inference ADR-0045/0046 record as having been wrong once.
func TestChosenReleaseIDAnswersWhetherAHumanNamedTheEdition(t *testing.T) {
	cases := []struct {
		name string
		e    store.EntityEnrichment
		want string
	}{
		{"a pinned record with an edition", store.EntityEnrichment{
			ExternalID: chosenGroup, ExternalReleaseID: chosenEdition,
			ExternalIDOrigin: store.OriginChosen}, chosenEdition},
		{"a cascaded record with an edition", store.EntityEnrichment{
			ExternalID: chosenGroup, ExternalReleaseID: chosenEdition,
			ExternalIDOrigin: store.OriginCascaded}, chosenEdition},
		{"an AUTO-resolved record carrying a leftover edition", store.EntityEnrichment{
			ExternalID: chosenGroup, ExternalReleaseID: chosenEdition}, ""},
		{"no record at all", store.EntityEnrichment{
			ExternalReleaseID: chosenEdition, ExternalIDOrigin: store.OriginChosen}, ""},
		{"a pinned record with no edition", store.EntityEnrichment{
			ExternalID: chosenGroup, ExternalIDOrigin: store.OriginChosen}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.ChosenReleaseID(); got != tc.want {
				t.Errorf("ChosenReleaseID() = %q, want %q", got, tc.want)
			}
		})
	}
}
