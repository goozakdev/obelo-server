package store_test

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// AN ENRICHMENT OVERRIDE SURVIVES A RESCAN (enrichment-override-durability/01,
// then /02).
//
// This is the rule stated on its own, at the one place that can break it. An
// Enrichment override is an Admin's answer to "which external record decorates
// this item" — Fix info on a Movie or a Track, an Episode pin's series on an
// Episode (ADR-0019, CONTEXT.md) — and surviving the next scheduled scan is the
// entire point of it: an override that lasts until 4am is not an override.
//
// It used to be stored on titles.tmdb_id / imdb_id, the same columns the scanner
// writes from the tree it just built, because local naming can carry an identity
// id of its own ({tmdb-…}, a folder-anchored Match override). The scanner wrote
// them UNCONDITIONALLY, and the TV resolve path builds Episode trees carrying no
// external id at all — so every scan asserted "no id" over every Admin's answer.
//
// Losing the id would be bad. What actually happened is worse: an Episode pin is
// a PAIR — which series and which episode within it (enrichment_season/
// enrichment_episode) — and a scan stripped the first while keeping the second.
// The lookup then silently means "the Show's OWN series, at the borrowed
// numbering": a real record, confidently wrong, with no error anywhere (ADR-0044).
//
// Issue 01 held the line with a guard — a scan may FILL an external id it actually
// has and must leave alone one it has nothing to say about. ADR-0045 replaced the
// guard with the separation it was standing in for: the record lives in
// enrichment_tmdb_id / enrichment_imdb_id / musicbrainz_id, which no tree write
// touches on an existing row, and the scanner owns tmdb_id / imdb_id outright.
//
// So these tests read the RECORD — COALESCE(enrichment id, folder id), which is
// what enrich.refFor resolves and store.recordExternalIDs spells — rather than one
// raw column. Every assertion they made still has to hold: the promise was never
// "the value stays in that column", it was "the Admin's answer is what the next
// lookup uses". Reading the record is that promise, and the identity column is
// asserted alongside it wherever the two now legitimately differ.

const (
	// A provider record an Admin chose. It is deliberately NOT the id any folder
	// name here carries — that is what makes it an override rather than a parse.
	pickedSeries = "77777"
	pickedMovie  = "438631"
	pickedIMDB   = "tt1160419"
	pickedMBID   = "b1a9c0e9-d987-4042-ae91-78d6a3267d69"
	// correctedMovie is a SECOND record, picked on an item whose folder already
	// names one. It exists only for issue 02's case, where the Admin and the folder
	// name genuinely disagree.
	correctedMovie = "693134"
)

// TestAMoviesFixInfoSurvivesARescan is the case the issue asked to confirm or
// rule out, and it holds: a Movie corrected with Fix info carries an Enrichment
// override and NO folder-keyed Match override (Fix info deliberately leaves
// identity alone, ADR-0019), so the folder still parses to "dune|2021" with no
// external id and nothing puts the id back. Long-standing, silent, and well
// beyond TV.
func TestAMoviesFixInfoSurvivesARescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	tree := movieTree("m1", "dune|2021", "Dune", 2021,
		"/media/Movies/Dune (2021)/Dune (2021).mkv")
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Fix info: the Admin picked the right record. Identity is untouched.
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{
		TMDBID: pickedMovie, IMDBID: pickedIMDB,
	}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}

	// The scheduled scan re-resolves the same folder, which says nothing about ids.
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, pickedIMDB)

	// And again — a rule that only holds for one pass is not a rule.
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("second rescan: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, pickedIMDB)
}

// TestAnEpisodePinsSeriesSurvivesARescan: the motivating case, at the store. The
// five Batman files are pinned to a foreign series' S01E01-05; a scan must not
// strip the series and leave the numbering, because that resolves to five real
// records of the WRONG show rather than to nothing.
func TestAnEpisodePinsSeriesSurvivesARescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libtv','TV','tv')`)

	const path = "/media/TV/Batman (1992)/Season 04/Batman (1992) - S04E01.mkv"
	tree := store.ShowTree{
		Show: store.Show{
			ID: "show1", LibraryID: "libtv", Title: "Batman", Year: 1992,
			IdentityKey: "batman|1992", SortTitle: "batman",
		},
		Seasons: []store.SeasonTree{{
			SeasonNumber: 4, IdentityKey: "batman|1992|s04",
			Episodes: []store.EpisodeTree{
				episodeTree("t1", "batman|1992|s04e01", 4, 1, "f1", path),
			},
		}},
	}
	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// The pin: this Slot is decorated from another series' S01E01.
	if err := db.SetTitleExternalMatch("t1", store.ExternalMatch{
		TMDBID: pickedSeries, EpisodeSeason: 1, EpisodeNumber: 1,
	}, store.OriginChosen); err != nil {
		t.Fatalf("pin: %v", err)
	}

	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	assertEpisodePin(t, db, "t1", pickedSeries, 1, 1)

	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("second rescan: %v", err)
	}
	assertEpisodePin(t, db, "t1", pickedSeries, 1, 1)
}

// TestATracksFixInfoSurvivesARescan: music's own external id, musicbrainz_id, was
// never in the scanner's UPDATE at all and so was never at risk — but a Track goes
// through the very same writeTitleRow as a Movie and an Episode, so the claim is
// worth an assertion rather than an argument. The video columns on a Track are
// asserted alongside it: they are the ones that WERE being blanked.
func TestATracksFixInfoSurvivesARescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('libmus','Music','music')`)

	const path = "/media/Music/Nirvana/Nevermind/01 - Smells Like Teen Spirit.flac"
	tree := store.ArtistTree{
		Artist: store.Artist{
			ID: "a1", LibraryID: "libmus", Name: "Nirvana",
			IdentityKey: "nirvana", SortName: "nirvana",
		},
		Albums: []store.AlbumTree{{
			Title: "Nevermind", Year: 1991, IdentityKey: "nirvana|nevermind",
			SortTitle: "nevermind",
			Tracks: []store.TrackTree{{
				TitleTree: store.TitleTree{
					Title: store.Title{
						ID: "tr1", LibraryID: "libmus", Kind: "track",
						Title: "Smells Like Teen Spirit", SortTitle: "smells like teen spirit",
						IdentityKey: "nirvana|nevermind|1|1",
					},
					Editions: []store.Edition{{
						ID: "ed1",
						Files: []store.File{{
							ID: "f1", EditionID: "ed1", Path: path, Present: true,
						}},
					}},
				},
				DiscNumber: 1, TrackNumber: 1,
			}},
		}},
	}
	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// Fix info on a Track: the RIGHT Nirvana, the one MusicBrainz's search buried.
	if err := db.SetTitleExternalMatch("tr1", store.ExternalMatch{
		MusicbrainzID: pickedMBID, TMDBID: pickedMovie,
	}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}

	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	var mbid string
	if err := db.QueryRow(`SELECT musicbrainz_id FROM titles WHERE id = 'tr1'`).Scan(&mbid); err != nil {
		t.Fatalf("read musicbrainz_id: %v", err)
	}
	if mbid != pickedMBID {
		t.Errorf("a rescan left the Track's musicbrainz_id = %q, want the Admin's %q", mbid, pickedMBID)
	}
	assertExternalIDs(t, db, "tr1", pickedMovie, "")
}

// TestAnEmbeddedIDIsStillAdoptedOnARescan is the other half of the rule, and the
// reason it is "fill, never blank" rather than "only fill an empty column". A
// folder that names its own record ({tmdb-N}) is making an IDENTITY claim
// (ADR-0002), and it is the same claim identity_key already carries — so the
// scanner may keep restating it. Weakening the scan to "fill only when empty"
// would have made this test the price.
func TestAnEmbeddedIDIsStillAdoptedOnARescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	// The folder is `Dune (2021) {tmdb-438631}`, so the scanner's identity IS the
	// id: key "tmdb:438631", tmdb_id 438631.
	tree := movieTree("m1", "tmdb:"+pickedMovie, "Dune", 2021,
		"/media/Movies/Dune (2021) {tmdb-438631}/Dune (2021).mkv")
	tree.TMDBID = pickedMovie
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, "")

	// Wipe the column behind the scanner's back — a row that predates the id, an
	// interrupted migration, anything. The next scan still has the id and says so.
	// (The scanner now writes it unconditionally rather than through issue 01's
	// fill-only guard, so this is a stronger statement than it used to be.)
	mustExec(t, db, `UPDATE titles SET tmdb_id = '' WHERE id = 'm1'`)
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, "")
}

// TestFixInfoOnAnEmbeddedIDFolderSurvivesARescan is issue 02's own case, and the
// one issue 01's fix could not reach: a folder that NAMES its own record, plus an
// Admin who says that record is the wrong one.
//
// `Dune (2021) {tmdb-438631}` gives the scanner a real id to write, so the fill-
// only guard never engaged — every scan restated the folder's id over the Admin's
// answer, silently, overnight, with Fix info having reported success. Reachable on
// any Movie or Show that has been through Wrong item, too, since that leaves an
// embedded-style id behind in exactly the same shape.
//
// ADR-0045 answers it: an Enrichment override outranks the id a folder name
// asserts. The folder keeps every bit of authority it had over IDENTITY — the key
// is still "tmdb:438631" and the scanner still restates the column — and loses
// only the job of deciding which record supplies the poster.
func TestFixInfoOnAnEmbeddedIDFolderSurvivesARescan(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	tree := movieTree("m1", "tmdb:"+pickedMovie, "Dune", 2021,
		"/media/Movies/Dune (2021) {tmdb-438631}/Dune (2021).mkv")
	tree.TMDBID = pickedMovie
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, "")

	// Fix info: the folder filed the right FILE and named the wrong RECORD.
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{TMDBID: correctedMovie}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	assertExternalIDs(t, db, "m1", correctedMovie, "")
	assertRecordIsTheAdmins(t, db, "m1", true)

	// The scheduled scan re-derives "tmdb:438631" from the folder and says so.
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("second rescan: %v", err)
	}
	assertExternalIDs(t, db, "m1", correctedMovie, "")
	// ...onto the identity column, which is still the folder's to write. Identity
	// never moved: same key, same row, same watch state (ADR-0002/0014).
	assertIdentityIDs(t, db, "m1", pickedMovie, "")
	assertIdentityKey(t, db, "m1", "tmdb:"+pickedMovie)
}

// TestAnEnrichmentPassDoesNotClaimTheAdminsLock: the record columns have two
// writers — the Admin and an enrichment pass persisting the id it resolved — and
// only the first of them is a CHOICE. enrichment_id_origin is what says which,
// and a pass must never set it.
//
// enrich.childHasOwnOverride reads the flag now (issue 03), so this assertion is
// what stands between an auto-matched leaf and being excluded from its parent's
// Cascade forever. It was asserted here before anything read it, because the value
// is only worth anything if it is right from the first row that has one; a flag
// written wrong is worse than no flag.
func TestAnEnrichmentPassDoesNotClaimTheAdminsLock(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	tree := movieTree("m1", "dune|2021", "Dune", 2021,
		"/media/Movies/Dune (2021)/Dune (2021).mkv")
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	// An ordinary pass: matched by title+year, and it persists the id it landed on
	// so the artwork-candidate lookup has an anchor.
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Overview: "A duke's son leads desert warriors.", Source: "tmdb",
		ExternalIDs: store.ExternalMatch{TMDBID: pickedMovie, IMDBID: pickedIMDB},
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, pickedIMDB)
	assertRecordIsTheAdmins(t, db, "m1", false)
	// And it said nothing about the folder's name, which asserted no id at all.
	assertIdentityIDs(t, db, "m1", "", "")

	// A later pass reporting a different record does not move a pinned one.
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{TMDBID: correctedMovie}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Overview: "…", Source: "tmdb",
		ExternalIDs: store.ExternalMatch{TMDBID: pickedMovie},
	}, nil); err != nil {
		t.Fatalf("re-enrich: %v", err)
	}
	assertExternalIDs(t, db, "m1", correctedMovie, pickedIMDB)
	assertRecordIsTheAdmins(t, db, "m1", true)
}

// TestWrongItemClearsThePriorRecord: Wrong item says the file is a genuinely
// different work, and a different work is a clean slate — watch state and Locked
// fields already go. A Fix-info override made for the PREVIOUS work must go with
// them, or the split would have made it durable enough to outlive the very
// correction that says it no longer applies (ADR-0019/ADR-0045).
func TestWrongItemClearsThePriorRecord(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	tree := movieTree("m1", "dune|2021", "Dune", 2021,
		"/media/Movies/Dune (2021)/Dune (2021).mkv")
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{
		TMDBID: pickedMovie, IMDBID: pickedIMDB,
	}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}

	if err := db.RekeyTitleIdentity("m1", "Dune: Part Two", 2024,
		correctedMovie, "tmdb:"+correctedMovie); err != nil {
		t.Fatalf("wrong item: %v", err)
	}
	assertExternalIDs(t, db, "m1", correctedMovie, "")
	assertIdentityIDs(t, db, "m1", correctedMovie, "")
	assertRecordIsTheAdmins(t, db, "m1", false)
}

// TestAnEnrichmentPassCannotShadowTheFoldersID: the precedence is three deep, and
// only the top two levels are anyone's DECISION — the Admin's override, then the
// id the folder name asserts, then whatever a lookup happened to echo back.
//
// A by-id lookup normally echoes the id it was asked for, so this only bites when
// a provider answers with something else (a merged record, a redirect, a stub).
// Without the guard that would be enough to silently re-point a Title nobody
// corrected, using the very column that exists to make corrections durable.
func TestAnEnrichmentPassCannotShadowTheFoldersID(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1','Movies','movie')`)

	tree := movieTree("m1", "tmdb:"+pickedMovie, "Dune", 2021,
		"/media/Movies/Dune (2021) {tmdb-438631}/Dune (2021).mkv")
	tree.TMDBID = pickedMovie
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	if err := db.WriteTitleEnrichment("m1", store.TitleEnrichment{
		Overview: "…", Source: "tmdb",
		ExternalIDs: store.ExternalMatch{TMDBID: correctedMovie},
	}, nil); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	assertExternalIDs(t, db, "m1", pickedMovie, "")
	assertRecordIsTheAdmins(t, db, "m1", false)

	// The Admin, however, outranks the folder — that is issue 02's whole answer.
	if err := db.SetTitleExternalMatch("m1", store.ExternalMatch{TMDBID: correctedMovie}, store.OriginChosen); err != nil {
		t.Fatalf("fix info: %v", err)
	}
	assertExternalIDs(t, db, "m1", correctedMovie, "")
}

// --- helpers ---------------------------------------------------------------

// movieTree is one Movie as a scan of its folder would leave it: one unnamed
// Edition holding one present File, and no external id unless the caller adds one.
func movieTree(titleID, identityKey, title string, year int, path string) store.TitleTree {
	return store.TitleTree{
		Title: store.Title{
			ID: titleID, LibraryID: "lib1", Kind: "movie",
			Title: title, Year: year, IdentityKey: identityKey, SortTitle: title,
		},
		Editions: []store.Edition{{
			ID: titleID + "-ed",
			Files: []store.File{{
				ID: titleID + "-f", EditionID: titleID + "-ed", Path: path, Present: true,
			}},
		}},
	}
}

// assertExternalIDs asserts the RECORD a Title resolves against — the Admin's
// Enrichment override when there is one, else the id the folder name asserts
// (ADR-0045). It is the same expression every read path selects, so what it
// asserts is exactly what the next provider lookup will ask for.
func assertExternalIDs(t *testing.T, db *store.DB, titleID, wantTMDB, wantIMDB string) {
	t.Helper()
	var tmdbID, imdbID string
	if err := db.QueryRow(
		`SELECT COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id),
		        COALESCE(NULLIF(enrichment_imdb_id, ''), imdb_id)
		   FROM titles WHERE id = ?`, titleID,
	).Scan(&tmdbID, &imdbID); err != nil {
		t.Fatalf("read external ids of %q: %v", titleID, err)
	}
	if tmdbID != wantTMDB {
		t.Errorf("%s resolves against tmdb record %q, want the Admin's %q (an override outranks the folder's id)",
			titleID, tmdbID, wantTMDB)
	}
	if imdbID != wantIMDB {
		t.Errorf("%s resolves against imdb record %q, want %q", titleID, imdbID, wantIMDB)
	}
}

// assertIdentityIDs asserts the OTHER half of the split: what local naming says,
// in the columns the scanner owns. Used where the two legitimately differ, which
// is the whole subject of issue 02.
func assertIdentityIDs(t *testing.T, db *store.DB, titleID, wantTMDB, wantIMDB string) {
	t.Helper()
	var tmdbID, imdbID string
	if err := db.QueryRow(
		`SELECT tmdb_id, imdb_id FROM titles WHERE id = ?`, titleID,
	).Scan(&tmdbID, &imdbID); err != nil {
		t.Fatalf("read identity ids of %q: %v", titleID, err)
	}
	if tmdbID != wantTMDB || imdbID != wantIMDB {
		t.Errorf("%s identity ids = (%q, %q), want (%q, %q)",
			titleID, tmdbID, imdbID, wantTMDB, wantIMDB)
	}
}

// assertRecordIsTheAdmins asserts that enrichment_id_origin marks the record as
// the Admin's OWN choice on this Title — as opposed to an id an enrichment pass
// resolved on its own (an empty origin), or one a parent's Cascade applied
// (OriginCascaded, ADR-0046). It is the signal that did not exist before ADR-0045,
// and the one issue 03 needs; every caller here is a direct Fix info / Wrong item
// / pin on the Title, so 'chosen' is the exact expectation, not merely "locked".
func assertRecordIsTheAdmins(t *testing.T, db *store.DB, titleID string, want bool) {
	t.Helper()
	var origin string
	if err := db.QueryRow(
		`SELECT enrichment_id_origin FROM titles WHERE id = ?`, titleID,
	).Scan(&origin); err != nil {
		t.Fatalf("read enrichment_id_origin of %q: %v", titleID, err)
	}
	if (store.RecordOrigin(origin) == store.OriginChosen) != want {
		t.Errorf("%s enrichment_id_origin = %q, want OwnChoice()=%v — the origin is what "+
			"tells an Admin's own pick apart from an id a pass resolved and from a record "+
			"a parent's Cascade applied", titleID, origin, want)
	}
}

// assertEpisodePin insists on the pin as a PAIR. Reporting the series and the
// numbering separately would let the dangerous half-pin — numbering kept, series
// gone, resolving confidently against the Show's own series — read as one small
// failure instead of the wrong-record one it is.
func assertEpisodePin(t *testing.T, db *store.DB, titleID, wantSeries string, wantSeason, wantEpisode int) {
	t.Helper()
	var series string
	var season, episode int
	if err := db.QueryRow(
		`SELECT COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id),
		        COALESCE(enrichment_season, -1), COALESCE(enrichment_episode, -1)
		   FROM titles WHERE id = ?`, titleID,
	).Scan(&series, &season, &episode); err != nil {
		t.Fatalf("read episode pin of %q: %v", titleID, err)
	}
	if series == wantSeries && season == wantSeason && episode == wantEpisode {
		return
	}
	if series == "" && season == wantSeason && episode == wantEpisode {
		t.Fatalf("%s kept the pinned numbering S%02dE%02d but lost its series: the lookup now means "+
			"the Show's OWN series at the borrowed numbering — a real record, and the wrong one",
			titleID, season, episode)
	}
	t.Fatalf("%s pin = %s S%02dE%02d, want %s S%02dE%02d",
		titleID, series, season, episode, wantSeries, wantSeason, wantEpisode)
}

func assertIdentityKey(t *testing.T, db *store.DB, titleID, want string) {
	t.Helper()
	var key string
	if err := db.QueryRow(`SELECT identity_key FROM titles WHERE id = ?`, titleID).Scan(&key); err != nil {
		t.Fatalf("read identity_key of %q: %v", titleID, err)
	}
	if key != want {
		t.Errorf("%s identity_key = %q, want %q — an Enrichment override must never move identity",
			titleID, key, want)
	}
}
