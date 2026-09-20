package store_test

import (
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The Library Export is a "what changed since" feed (ADR-0056 §4), so every
// catalog write has to leave a mark. Eight exportable tables carry an updated_at
// kept by triggers rather than by callers (0001_init.sql), and these tests are
// the audit the issue asks for: one case per production writer of those tables,
// plus the ancestor propagation (a Stream change is a Title change).
//
// A missed bump is invisible in every other test in this repo — the row simply
// stops reaching the friend's Server — which is exactly why it is asserted here
// against the real writers rather than against the SQL.

// stamp reads one row's updated_at.
func stamp(t *testing.T, db *store.DB, table, id string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(`SELECT updated_at FROM `+table+` WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("reading %s.updated_at for %q: %v", table, id, err)
	}
	return s
}

// settle spaces two writes far enough apart that their millisecond stamps
// differ. strftime('%f') has millisecond resolution, so back-to-back statements
// in one test can legitimately land on the same instant.
func settle() { time.Sleep(3 * time.Millisecond) }

// seedMovie writes a Library with one Movie: title → edition → file → stream,
// through the real scanner writer so the triggers see what a scan does.
func seedMovie(t *testing.T, db *store.DB) (libID, titleID, editionID, fileID, streamID string) {
	t.Helper()
	libID = "lib1"
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib1', 'Movies', 'movie')`)
	tree := store.TitleTree{
		Title: store.Title{
			ID: "t1", LibraryID: libID, Kind: "movie", Title: "Dune", Year: 2021,
			IdentityKey: "dune|2021", SortTitle: "dune",
		},
		Editions: []store.Edition{{
			ID: "e1", Name: "1080p",
			Files: []store.File{{
				ID: "f1", Path: "/media/Dune (2021)/Dune.mkv", Container: "matroska",
				VideoCodec: "h264", Width: 1920, Height: 1080, DurationMs: 1000, SizeBytes: 10,
				Streams: []store.Stream{{ID: "s1", Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true}},
			}},
		}},
	}
	if err := db.UpsertTitleTree(tree); err != nil {
		t.Fatalf("seeding movie: %v", err)
	}
	return libID, "t1", "e1", "f1", "s1"
}

// TestEveryCatalogRowIsStampedOnInsert: the scanner's own writer leaves a stamp
// on every row of the subtree it creates. Without this the very first export of
// a freshly scanned Library would order on empty strings.
func TestEveryCatalogRowIsStampedOnInsert(t *testing.T) {
	db := openTemp(t)
	_, titleID, editionID, fileID, streamID := seedMovie(t, db)

	for _, c := range []struct{ table, id string }{
		{"titles", titleID}, {"editions", editionID}, {"files", fileID}, {"streams", streamID},
	} {
		if got := stamp(t, db, c.table, c.id); got == "" {
			t.Errorf("%s row %q has no updated_at after the scanner wrote it", c.table, c.id)
		}
	}
}

// TestStreamChangeIsATitleChange is the propagation ADR-0056 §4 names: a Stream
// edit must surface as a Title edit, because a mirror that only watches Titles
// would otherwise never learn the audio tracks changed.
func TestStreamChangeIsATitleChange(t *testing.T) {
	db := openTemp(t)
	_, titleID, editionID, fileID, streamID := seedMovie(t, db)

	before := map[string]string{
		"titles":   stamp(t, db, "titles", titleID),
		"editions": stamp(t, db, "editions", editionID),
		"files":    stamp(t, db, "files", fileID),
		"streams":  stamp(t, db, "streams", streamID),
	}
	settle()
	mustExec(t, db, `UPDATE streams SET language = 'fr' WHERE id = ?`, streamID)

	for table, was := range before {
		id := map[string]string{"titles": titleID, "editions": editionID, "files": fileID, "streams": streamID}[table]
		if now := stamp(t, db, table, id); now <= was {
			t.Errorf("a Stream edit left %s.updated_at at %q (was %q): the change never reaches the mirror",
				table, now, was)
		}
	}
}

// TestChildDeletesBumpTheAncestors: a Stream or File that goes away is a change
// to what is left, and the row that could carry a tombstone is gone with it —
// so the ancestors have to move.
func TestChildDeletesBumpTheAncestors(t *testing.T) {
	db := openTemp(t)
	_, titleID, editionID, fileID, streamID := seedMovie(t, db)

	fileWas, titleWas := stamp(t, db, "files", fileID), stamp(t, db, "titles", titleID)
	settle()
	mustExec(t, db, `DELETE FROM streams WHERE id = ?`, streamID)
	if now := stamp(t, db, "files", fileID); now <= fileWas {
		t.Errorf("deleting a Stream left its File's stamp at %q (was %q)", now, fileWas)
	}
	if now := stamp(t, db, "titles", titleID); now <= titleWas {
		t.Errorf("deleting a Stream left its Title's stamp at %q (was %q)", now, titleWas)
	}

	titleWas = stamp(t, db, "titles", titleID)
	settle()
	mustExec(t, db, `DELETE FROM editions WHERE id = ?`, editionID)
	if now := stamp(t, db, "titles", titleID); now <= titleWas {
		t.Errorf("deleting an Edition left its Title's stamp at %q (was %q)", now, titleWas)
	}
	_ = fileID
}

// TestEveryTitleWriterBumpsTheStamp walks the production writers of the titles
// table — one case each — and asserts the row moved. These are the paths issue
// 05's audit enumerates: the scanner, the incremental prune, enrichment, the
// Admin hand-edit, the review dismissal and the identity correction.
func TestEveryTitleWriterBumpsTheStamp(t *testing.T) {
	overview := "a desert planet"
	cases := []struct {
		name  string
		write func(t *testing.T, db *store.DB, libID, titleID string)
	}{
		{"scanner re-upsert (UpsertTitleTree)", func(t *testing.T, db *store.DB, libID, titleID string) {
			tree := store.TitleTree{
				Title: store.Title{
					ID: titleID, LibraryID: libID, Kind: "movie", Title: "Dune: Part One", Year: 2021,
					IdentityKey: "dune|2021", SortTitle: "dune part one",
				},
			}
			if err := db.UpsertTitleTree(tree); err != nil {
				t.Fatalf("re-upsert: %v", err)
			}
		}},
		{"incremental prune (MarkFilesMissing)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if _, err := db.MarkFilesMissing(libID, map[string]bool{}, nil); err != nil {
				t.Fatalf("MarkFilesMissing: %v", err)
			}
		}},
		{"hidden recompute (RecomputeHiddenTitles)", func(t *testing.T, db *store.DB, libID, titleID string) {
			// Make every File Missing first so the recompute actually flips a row.
			mustExec(t, db, `UPDATE files SET present = 0`)
			settle()
			if err := db.RecomputeHiddenTitles(libID); err != nil {
				t.Fatalf("RecomputeHiddenTitles: %v", err)
			}
		}},
		{"enrichment result (WriteTitleEnrichment)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.WriteTitleEnrichment(titleID, store.TitleEnrichment{
				Overview: overview, Source: "tmdb",
			}, nil); err != nil {
				t.Fatalf("WriteTitleEnrichment: %v", err)
			}
		}},
		{"enrichment status (SetTitleEnrichmentStatus)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.SetTitleEnrichmentStatus(titleID, "unmatched", ""); err != nil {
				t.Fatalf("SetTitleEnrichmentStatus: %v", err)
			}
		}},
		{"enrichment record (SetTitleExternalMatch)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.SetTitleExternalMatch(titleID,
				store.ExternalMatch{TMDBID: "438631"}, store.OriginChosen); err != nil {
				t.Fatalf("SetTitleExternalMatch: %v", err)
			}
		}},
		{"admin hand-edit (WriteTitleMetadata)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.WriteTitleMetadata(titleID, store.MetadataEdit{Overview: &overview}); err != nil {
				t.Fatalf("WriteTitleMetadata: %v", err)
			}
		}},
		{"admin genres edit (title_genres)", func(t *testing.T, db *store.DB, libID, titleID string) {
			g := []string{"Science Fiction"}
			if err := db.WriteTitleMetadata(titleID, store.MetadataEdit{Genres: &g}); err != nil {
				t.Fatalf("WriteTitleMetadata genres: %v", err)
			}
		}},
		{"admin cast edit (title_credits)", func(t *testing.T, db *store.DB, libID, titleID string) {
			c := []store.Credit{{Person: "Timothée Chalamet", Kind: "cast", Character: "Paul"}}
			if err := db.WriteTitleMetadata(titleID, store.MetadataEdit{Cast: &c}); err != nil {
				t.Fatalf("WriteTitleMetadata cast: %v", err)
			}
		}},
		{"review dismissal (MarkTitleReviewed)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.MarkTitleReviewed(titleID); err != nil {
				t.Fatalf("MarkTitleReviewed: %v", err)
			}
		}},
		{"identity correction (RekeyTitleIdentity)", func(t *testing.T, db *store.DB, libID, titleID string) {
			if err := db.RekeyTitleIdentity(titleID, "Dune", 2021, "438631", "dune-2021|2021"); err != nil {
				t.Fatalf("RekeyTitleIdentity: %v", err)
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTemp(t)
			libID, titleID, _, _, _ := seedMovie(t, db)
			was := stamp(t, db, "titles", titleID)
			settle()
			c.write(t, db, libID, titleID)
			if now := stamp(t, db, "titles", titleID); now <= was {
				t.Errorf("updated_at = %q, was %q: this writer's change never reaches the export", now, was)
			}
		})
	}
}

// seedShow writes a Show → Season → Episode through the real TV writer.
func seedShow(t *testing.T, db *store.DB) (libID, showID, seasonID string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib2', 'Shows', 'tv')`)
	tree := store.ShowTree{
		Show: store.Show{
			ID: "sh1", LibraryID: "lib2", Title: "Chernobyl", Year: 2019,
			IdentityKey: "chernobyl|2019", SortTitle: "chernobyl",
		},
		Seasons: []store.SeasonTree{{
			SeasonNumber: 1, IdentityKey: "chernobyl|2019|s01",
			Episodes: []store.EpisodeTree{{
				TitleTree: store.TitleTree{
					Title: store.Title{
						ID: "ep1", LibraryID: "lib2", Kind: "episode", Title: "1:23:45",
						IdentityKey: "chernobyl|2019|s01e01", SortTitle: "1 23 45",
					},
					Editions: []store.Edition{{ID: "ee1", Files: []store.File{{
						ID: "ef1", Path: "/media/Chernobyl/S01/e01.mkv", Container: "matroska",
					}}}},
				},
				SeasonNumber: 1, EpisodeNumber: 1,
			}},
		}},
	}
	if err := db.UpsertShowTree(tree); err != nil {
		t.Fatalf("seeding show: %v", err)
	}
	var seasonRow string
	if err := db.QueryRow(`SELECT id FROM seasons WHERE show_id = 'sh1'`).Scan(&seasonRow); err != nil {
		t.Fatalf("reading season id: %v", err)
	}
	return "lib2", "sh1", seasonRow
}

// TestEveryShowWriterBumpsTheStamp: the TV half of the audit.
func TestEveryShowWriterBumpsTheStamp(t *testing.T) {
	network := "HBO"
	cases := []struct {
		name  string
		table string
		which func(showID, seasonID string) string
		write func(t *testing.T, db *store.DB, libID, showID, seasonID string)
	}{
		{"scanner re-upsert bumps the Show", "shows", func(s, _ string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				if err := db.UpsertShowTree(store.ShowTree{
					Show: store.Show{
						ID: showID, LibraryID: libID, Title: "Chernobyl", Year: 2019,
						IdentityKey: "chernobyl|2019", SortTitle: "chernobyl",
					},
				}); err != nil {
					t.Fatalf("re-upsert show: %v", err)
				}
			}},
		{"hidden recompute bumps the Season", "seasons", func(_, s string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				mustExec(t, db, `UPDATE files SET present = 0`)
				settle()
				if err := db.RecomputeHiddenTitles(libID); err != nil {
					t.Fatalf("RecomputeHiddenTitles: %v", err)
				}
				settle()
				if err := db.RecomputeHiddenShows(libID); err != nil {
					t.Fatalf("RecomputeHiddenShows: %v", err)
				}
			}},
		{"review dismissal bumps the Show", "shows", func(s, _ string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				if err := db.MarkShowReviewed(showID); err != nil {
					t.Fatalf("MarkShowReviewed: %v", err)
				}
			}},
		{"identity correction bumps the Show", "shows", func(s, _ string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				if err := db.RekeyShowIdentity(showID, "Chernobyl", 2019, "87108", "chernobyl-2019|2019"); err != nil {
					t.Fatalf("RekeyShowIdentity: %v", err)
				}
			}},
		{"parent enrichment (entity_enrichment) bumps the Show", "shows", func(s, _ string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				if err := db.WriteEntityMetadata("show", showID,
					store.EntityMetadataEdit{Network: &network}); err != nil {
					t.Fatalf("WriteEntityMetadata: %v", err)
				}
			}},
		{"parent genres (entity_genres) bump the Show", "shows", func(s, _ string) string { return s },
			func(t *testing.T, db *store.DB, libID, showID, seasonID string) {
				g := []string{"Drama"}
				if err := db.WriteEntityMetadata("show", showID,
					store.EntityMetadataEdit{Genres: &g}); err != nil {
					t.Fatalf("WriteEntityMetadata genres: %v", err)
				}
			}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTemp(t)
			libID, showID, seasonID := seedShow(t, db)
			id := c.which(showID, seasonID)
			was := stamp(t, db, c.table, id)
			settle()
			c.write(t, db, libID, showID, seasonID)
			if now := stamp(t, db, c.table, id); now <= was {
				t.Errorf("%s.updated_at = %q, was %q: this writer's change never reaches the export",
					c.table, now, was)
			}
		})
	}
}

// TestMusicWritersBumpTheStamp: the music half — an Artist and an Album are
// exported entities of their own, so their writers have to move them too.
func TestMusicWritersBumpTheStamp(t *testing.T) {
	db := openTemp(t)
	mustExec(t, db, `INSERT INTO libraries (id, name, kind) VALUES ('lib3', 'Music', 'music')`)
	tree := store.ArtistTree{
		Artist: store.Artist{
			ID: "ar1", LibraryID: "lib3", Name: "Boards of Canada",
			IdentityKey: "boards of canada", SortName: "boards of canada",
		},
		Albums: []store.AlbumTree{{
			Title: "Music Has the Right to Children", Year: 1998,
			IdentityKey: "boards of canada|music has the right to children",
			SortTitle:   "music has the right to children",
			Tracks: []store.TrackTree{{
				TitleTree: store.TitleTree{
					Title: store.Title{
						ID: "tr1", LibraryID: "lib3", Kind: "track", Title: "Roygbiv",
						IdentityKey: "boards of canada|music has the right to children|1|10",
						SortTitle:   "roygbiv",
					},
					Editions: []store.Edition{{ID: "te1", Files: []store.File{{
						ID: "tf1", Path: "/media/BoC/MHTRTC/10 Roygbiv.flac", Container: "flac",
					}}}},
				},
				DiscNumber: 1, TrackNumber: 10,
			}},
		}},
	}
	if err := db.UpsertArtistTree(tree); err != nil {
		t.Fatalf("seeding artist: %v", err)
	}
	var albumID string
	if err := db.QueryRow(`SELECT id FROM albums WHERE artist_id = 'ar1'`).Scan(&albumID); err != nil {
		t.Fatalf("reading album id: %v", err)
	}
	if stamp(t, db, "artists", "ar1") == "" || stamp(t, db, "albums", albumID) == "" {
		t.Fatal("the music writer left an Artist or Album with no updated_at")
	}

	artistWas, albumWas := stamp(t, db, "artists", "ar1"), stamp(t, db, "albums", albumID)
	settle()
	name := "Boards of Canada (BoC)"
	if err := db.WriteEntityMetadata("album", albumID, store.EntityMetadataEdit{Name: &name}); err != nil {
		t.Fatalf("WriteEntityMetadata album: %v", err)
	}
	if now := stamp(t, db, "albums", albumID); now <= albumWas {
		t.Errorf("an Album rename left its stamp at %q (was %q)", now, albumWas)
	}

	settle()
	mustExec(t, db, `UPDATE files SET present = 0`)
	settle()
	if err := db.RecomputeHiddenTitles("lib3"); err != nil {
		t.Fatalf("RecomputeHiddenTitles: %v", err)
	}
	settle()
	if err := db.RecomputeHiddenArtists("lib3"); err != nil {
		t.Fatalf("RecomputeHiddenArtists: %v", err)
	}
	if now := stamp(t, db, "artists", "ar1"); now <= artistWas {
		t.Errorf("hiding every Track left the Artist's stamp at %q (was %q)", now, artistWas)
	}
}

// TestStampsAreComparableRFC3339: the feed's keyset seek compares these values
// as strings in SQL, so a mixture of formats would silently mis-order ('T' sorts
// after ' '). Every stamp the triggers write is RFC3339-UTC.
func TestStampsAreComparableRFC3339(t *testing.T) {
	db := openTemp(t)
	_, titleID, _, _, _ := seedMovie(t, db)
	got := stamp(t, db, "titles", titleID)
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("updated_at %q is not RFC3339: %v", got, err)
	}
	if got[len(got)-1] != 'Z' {
		t.Errorf("updated_at %q is not UTC-stamped", got)
	}
}
