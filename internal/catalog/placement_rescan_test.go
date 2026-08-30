package catalog_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/catalog"
	"github.com/goozakdev/obelo-server/internal/scanner"
	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0044's named risk, pinned: APPLY-THEN-RESCAN IS A NO-OP.
//
// Title structure has two writers. Apply (catalog/placement.go) rewrites the rows
// the moment the matcher screen closes; the Scanner (scanner/tv_resolve.go)
// rebuilds the same Show from disk on every scheduled scan. Both read the same
// file-anchored decisions, but through different code — Apply reuses stored File
// rows and moves live Titles, the Scanner re-walks and re-probes — so if they ever
// derive a different arrangement, a scan silently rearranges a hand-sorted Show
// overnight, unobserved, with no error anywhere.
//
// Nothing structural prevents that. Only this file does.
//
// Every test here has the same three beats, and all three carry weight:
//
//  1. seed a real Show by really scanning real files on disk, then Apply;
//  2. assert the arrangement Apply produced against an ABSOLUTE expectation
//     (structure(), an id-free rendering). Idempotence alone would be
//     tautological — scanner.ResolveEpisodes is shared, so breaking it breaks both
//     writers identically and a before/after comparison would still pass;
//  3. assert a full scan, and then a SECOND full scan, leave the catalog
//     byte-identical (snapshot(), which does include ids and watch state).
//
// The second scan is not padding: a writer that converges on the second pass is
// still wrong on the first, and that first pass is the one that runs at 4am.
//
// The Targeted scan (ADR-0030) gets the same treatment, because it loads the
// decisions through its own path (targeted.go) and is exactly what runs right
// after Apply.
//
// It lives in catalog_test rather than beside either writer because it needs
// both: catalog imports scanner, so only this side of that edge can wire the two
// services and a real store together.

const (
	rescanShowFolder = "The Bear (2022)"
	rescanShowKey    = "the bear|2022"
)

// rescanProber is the ffprobe seam. Its answers are a pure function of the file's
// name, which is load-bearing twice over: a re-probe of an unchanged file must
// agree with the row a reused one carries, and every File must be distinguishable
// from every other so a snapshot notices two of them swapping attributes.
type rescanProber struct{}

func (rescanProber) Probe(_ context.Context, path string) (scanner.MediaInfo, error) {
	var sum int
	for _, r := range filepath.Base(path) {
		sum = (sum*31 + int(r)) % 9973
	}
	return scanner.MediaInfo{
		Container:  "mkv",
		DurationMs: int64(600000 + sum*100),
		Bitrate:    int64(4000000 + sum),
		Streams: []scanner.StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
		},
	}, nil
}

// rescanFixture is one real TV Library on disk, scanned into a real store, with
// both writers wired to it: the Scanner that walks the folder and the catalog
// Service that applies an arrangement to it.
type rescanFixture struct {
	t      *testing.T
	db     *store.DB
	root   string
	show   string // the Show folder on disk
	showID string
	scan   *scanner.Service
	cat    *catalog.Service
}

// newRescanFixture writes the named files (paths relative to the Show folder),
// scans them for real, and returns the fixture. Seeding by SCAN rather than by a
// hand-built tree matters: the "before" picture has to be exactly what the
// Scanner would leave, or the invariant is measured against a fiction.
func newRescanFixture(t *testing.T, files ...string) *rescanFixture {
	t.Helper()
	root := t.TempDir()
	show := filepath.Join(root, rescanShowFolder)
	for _, rel := range files {
		writeMediaFile(t, filepath.Join(show, filepath.FromSlash(rel)))
	}

	db := openTemp(t)
	if _, err := db.CreateLibrary("libtv", "TV", "tv",
		[]store.LibraryRootInput{{ID: "root1", Path: root}}); err != nil {
		t.Fatalf("create library: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u1','u1','member')`)
	mustExec(t, db, `INSERT INTO users (id, username, role) VALUES ('u2','u2','member')`)

	f := &rescanFixture{t: t, db: db, root: root, show: show}
	f.scan = scanner.NewService(db, rescanProber{})
	f.cat = catalog.NewService(db, t.TempDir())
	// The real per-Library lock, not a fake: Apply is a catalog writer like a scan
	// and takes the same one (ADR-0031).
	f.cat.SetLibraryLock(f.scan)

	f.fullScan()
	if err := db.QueryRow(`SELECT id FROM shows WHERE library_id = 'libtv'`).Scan(&f.showID); err != nil {
		t.Fatalf("seed scan produced no Show: %v", err)
	}
	return f
}

// writeMediaFile creates a media file with size-carrying content, so the
// incremental scan's (mtime, size) change detection has something real to work
// with and two files are never byte-identical.
func writeMediaFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A junk-filter-safe size (isJunk rejects tiny files as samples).
	body := make([]byte, 128*1024+len(filepath.Base(path)))
	for i := range body {
		body[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// path is the absolute path of a file named relative to the Show folder.
func (f *rescanFixture) path(rel string) string {
	return filepath.Join(f.show, filepath.FromSlash(rel))
}

func (f *rescanFixture) fullScan() {
	f.t.Helper()
	if _, err := f.scan.Scan(context.Background(), "libtv"); err != nil {
		f.t.Fatalf("full scan: %v", err)
	}
}

// targetedScan re-walks just this Show's folder — the narrow scan the API fires
// right after an Apply, which reaches the decisions through targeted.go's own
// load rather than scanRoots'.
func (f *rescanFixture) targetedScan() {
	f.t.Helper()
	if _, err := f.scan.TargetedScan(context.Background(), "libtv", scanner.TargetedScope{
		Folders: []string{f.show}, Label: rescanShowFolder,
	}); err != nil {
		f.t.Fatalf("targeted scan: %v", err)
	}
}

func (f *rescanFixture) apply(decisions ...store.FileDecision) catalog.PlacementResult {
	f.t.Helper()
	res, err := f.cat.ApplyPlacement(catalog.PlacementInput{ShowID: f.showID, Decisions: decisions})
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return res
}

// place builds one 'placed' row for a file named relative to the Show folder.
func (f *rescanFixture) place(rel string, season, episode, ordinal int) store.FileDecision {
	return store.FileDecision{
		Path: f.path(rel), State: store.DecisionPlaced,
		GroupNumber: season, SlotNumber: episode, Ordinal: ordinal,
	}
}

func (f *rescanFixture) settle(rel, state string) store.FileDecision {
	return store.FileDecision{Path: f.path(rel), State: state}
}

func (f *rescanFixture) titleIDAt(season, episode int) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(`SELECT id FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		rescanEpisodeKey(season, episode)).Scan(&id); err != nil {
		f.t.Fatalf("no Title at S%02dE%02d: %v", season, episode, err)
	}
	return id
}

func (f *rescanFixture) titleIDForPath(rel string) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(
		`SELECT t.id FROM titles t JOIN editions e ON e.title_id = t.id
		   JOIN files fi ON fi.edition_id = e.id WHERE fi.path = ?`, f.path(rel)).Scan(&id); err != nil {
		f.t.Fatalf("no Title owns %q: %v", rel, err)
	}
	return id
}

// fileCountUnder is how many File rows an Episode Title owns, present or Missing.
// hidden is a derived cache the recompute rewrites, so a test about a Slot that
// must be GONE asserts the thing hidden is derived from as well.
func (f *rescanFixture) fileCountUnder(identityKey string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM files fi
		   JOIN editions e ON e.id = fi.edition_id
		   JOIN titles t ON t.id = e.title_id
		  WHERE t.library_id = 'libtv' AND t.identity_key = ?`, identityKey).Scan(&n); err != nil {
		f.t.Fatalf("counting files under %q: %v", identityKey, err)
	}
	return n
}

// rowsForPath is how many File rows exist for one on-disk path. A path legitimately
// has several (co-File siblings), so the count is the only way to say "the split is
// gone" or "the soft-deleted row is still there".
func (f *rescanFixture) rowsForPath(rel string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM files WHERE path = ?`, f.path(rel)).Scan(&n); err != nil {
		f.t.Fatalf("counting file rows for %q: %v", rel, err)
	}
	return n
}

func (f *rescanFixture) setWatch(user, titleID string, resumeMs int64, watched bool) {
	f.t.Helper()
	if err := f.db.SaveWatchState(user, titleID, resumeMs, watched, true); err != nil {
		f.t.Fatalf("save watch state: %v", err)
	}
}

func (f *rescanFixture) watch(user, titleID string) store.WatchState {
	f.t.Helper()
	ws, err := f.db.WatchStateFor(user, titleID)
	if err != nil {
		f.t.Fatalf("read watch state: %v", err)
	}
	return ws
}

// durationOf is the joint-timeline arithmetic's input: the duration the seeding
// scan stored for one File.
func (f *rescanFixture) durationOf(rel string) int64 {
	f.t.Helper()
	var ms int64
	if err := f.db.QueryRow(`SELECT duration_ms FROM files WHERE path = ? LIMIT 1`, f.path(rel)).Scan(&ms); err != nil {
		f.t.Fatalf("duration of %q: %v", rel, err)
	}
	return ms
}

// seasonArtwork is the local poster path hung on a Season row, "" when it has
// none — the Scanner's to write, never Apply's.
func (f *rescanFixture) seasonArtwork(season int) string {
	f.t.Helper()
	var path string
	err := f.db.QueryRow(
		`SELECT ea.path FROM entity_artwork ea
		   JOIN seasons s ON s.id = ea.entity_id AND ea.entity_type = 'season'
		   JOIN shows sh ON sh.id = s.show_id
		  WHERE sh.library_id = 'libtv' AND s.season_number = ? AND ea.source = 'local'`,
		season).Scan(&path)
	if err != nil {
		return ""
	}
	return f.rel(path)
}

func rescanEpisodeKey(season, episode int) string {
	return fmt.Sprintf("%s|s%02de%02d", rescanShowKey, season, episode)
}

// --- the two renderings ----------------------------------------------------

// structure renders the Show's arrangement WITHOUT any row id: the seasons that
// exist, the Episodes in them, every field a rearrangement can get wrong, and the
// Files each Episode owns in the order the browse read returns them. It is
// compared against a literal expectation, which is what gives this file teeth
// against the derivation both writers share.
func (f *rescanFixture) structure() string {
	f.t.Helper()
	var b strings.Builder

	// Every Show in the Library, not just the one being rearranged: an Apply that
	// reached beyond its own Show would be replayed against the other one by every
	// future scan.
	shows, err := f.db.Query(
		`SELECT id, identity_key, hidden FROM shows WHERE library_id = 'libtv'
		  ORDER BY identity_key`)
	if err != nil {
		f.t.Fatalf("shows: %v", err)
	}
	type showRow struct {
		id, key string
		hidden  int
	}
	var showRows []showRow
	for shows.Next() {
		var sr showRow
		if err := shows.Scan(&sr.id, &sr.key, &sr.hidden); err != nil {
			f.t.Fatalf("scan show: %v", err)
		}
		showRows = append(showRows, sr)
	}
	_ = shows.Close()

	for _, sr := range showRows {
		fmt.Fprintf(&b, "show %s hidden=%d\n", sr.key, sr.hidden)
		rows, err := f.db.Query(
			`SELECT id, season_number, identity_key, hidden FROM seasons
			  WHERE show_id = ? ORDER BY season_number`, sr.id)
		if err != nil {
			f.t.Fatalf("seasons: %v", err)
		}
		type seasonRow struct {
			id, key string
			number  int
			hidden  int
		}
		var seasons []seasonRow
		for rows.Next() {
			var sn seasonRow
			if err := rows.Scan(&sn.id, &sn.number, &sn.key, &sn.hidden); err != nil {
				f.t.Fatalf("scan season: %v", err)
			}
			seasons = append(seasons, sn)
		}
		_ = rows.Close()
		for _, sn := range seasons {
			fmt.Fprintf(&b, "season %d key=%s hidden=%d\n", sn.number, sn.key, sn.hidden)
			for _, line := range f.episodeLines(sn.id, "  ") {
				b.WriteString(line)
			}
		}
	}

	for _, d := range f.decisionLines() {
		b.WriteString(d)
	}
	for _, u := range f.unmatchedLines() {
		b.WriteString(u)
	}
	return b.String()
}

// episodeLines renders one Season's Episode Titles, keyed by the Season's row id
// so two Shows' season 1 can never be confused for each other.
func (f *rescanFixture) episodeLines(seasonID, indent string) []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT t.id, t.identity_key, t.title, t.sort_title, t.season_number, t.episode_number,
		        t.episode_label, t.needs_review, t.ambiguous, t.hidden
		   FROM titles t WHERE t.season_id = ? ORDER BY t.identity_key`, seasonID)
	if err != nil {
		f.t.Fatalf("episodes: %v", err)
	}
	type epRow struct {
		id, key, title, sortTitle, label string
		seasonNum, epNum                 int
		needsReview, ambiguous, hidden   int
	}
	var eps []epRow
	for rows.Next() {
		var e epRow
		if err := rows.Scan(&e.id, &e.key, &e.title, &e.sortTitle, &e.seasonNum, &e.epNum,
			&e.label, &e.needsReview, &e.ambiguous, &e.hidden); err != nil {
			f.t.Fatalf("scan episode: %v", err)
		}
		eps = append(eps, e)
	}
	_ = rows.Close()

	var out []string
	for _, e := range eps {
		out = append(out, fmt.Sprintf(
			"%s%s S%02dE%02d name=%q sort=%q label=%q review=%d ambiguous=%d hidden=%d\n",
			indent, e.key, e.seasonNum, e.epNum, e.title, e.sortTitle, e.label,
			e.needsReview, e.ambiguous, e.hidden))
		out = append(out, f.editionLines(e.id, indent+"  ")...)
	}
	return out
}

func (f *rescanFixture) editionLines(titleID, indent string) []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT e.id, e.name FROM editions e WHERE e.title_id = ?
		  ORDER BY e.name, (SELECT MIN(path) FROM files WHERE edition_id = e.id)`, titleID)
	if err != nil {
		f.t.Fatalf("editions: %v", err)
	}
	type edRow struct{ id, name string }
	var eds []edRow
	for rows.Next() {
		var e edRow
		if err := rows.Scan(&e.id, &e.name); err != nil {
			f.t.Fatalf("scan edition: %v", err)
		}
		eds = append(eds, e)
	}
	_ = rows.Close()

	var out []string
	for _, e := range eds {
		out = append(out, fmt.Sprintf("%sedition %q\n", indent, e.name))
		// The browse read's own order: part_ordinal then path (catalog.go).
		frows, err := f.db.Query(
			`SELECT path, part_ordinal, present FROM files
			  WHERE edition_id = ? ORDER BY part_ordinal, path`, e.id)
		if err != nil {
			f.t.Fatalf("files: %v", err)
		}
		for frows.Next() {
			var path string
			var part, present int
			if err := frows.Scan(&path, &part, &present); err != nil {
				f.t.Fatalf("scan file: %v", err)
			}
			out = append(out, fmt.Sprintf("%s  file %s part=%d present=%d\n",
				indent, f.rel(path), part, present))
		}
		_ = frows.Close()
	}
	return out
}

func (f *rescanFixture) decisionLines() []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT path, state, COALESCE(group_number, -1), COALESCE(slot_number, -1), ordinal, orphaned
		   FROM file_decisions WHERE library_id = 'libtv'
		  ORDER BY path, group_number, slot_number, ordinal`)
	if err != nil {
		f.t.Fatalf("decisions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path, state string
		var group, slot, ordinal, orphaned int
		if err := rows.Scan(&path, &state, &group, &slot, &ordinal, &orphaned); err != nil {
			f.t.Fatalf("scan decision: %v", err)
		}
		out = append(out, fmt.Sprintf("decision %s %s g=%d s=%d ord=%d orphaned=%d\n",
			f.rel(path), state, group, slot, ordinal, orphaned))
	}
	return out
}

func (f *rescanFixture) unmatchedLines() []string {
	f.t.Helper()
	rows, err := f.db.Query(
		`SELECT path FROM unmatched_files WHERE library_id = 'libtv' ORDER BY path`)
	if err != nil {
		f.t.Fatalf("unmatched: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			f.t.Fatalf("scan unmatched: %v", err)
		}
		out = append(out, "unmatched "+f.rel(path)+"\n")
	}
	return out
}

// snapshot is structure() plus everything a no-op has to preserve but a literal
// expectation cannot name: the row IDS (a Title id that moves takes every User's
// watch history with it), the Show row, enrichment status, the EPISODE PIN, the
// technical File attributes that flow through the probe seam, the elementary
// Streams, and the watch state itself.
//
// The pin is here because it was once missing. The snapshot covered ids, keys,
// numbers, names, enrichment_status, file order, ordinals and watch state, and
// stopped one column short of the record — which a scan blanked on every Episode,
// unobserved (enrichment-override-durability/01). A pin is a PAIR, series plus
// position within it, so all three columns are rendered together: half a pin is
// not a smaller bug than none, it is a worse one, because it still resolves.
//
// The series is rendered as the RESOLVED record (ADR-0045: an Enrichment override,
// else the id local naming asserts), which is what a lookup would actually ask
// for. Rendering the raw identity column would make the snapshot agree with itself
// while every pin in it read as empty.
func (f *rescanFixture) snapshot() string {
	f.t.Helper()
	var b strings.Builder
	b.WriteString(f.structure())

	shows, err := f.db.Query(
		`SELECT id, identity_key, title, COALESCE(year, -1), sort_title, needs_review
		   FROM shows WHERE library_id = 'libtv' ORDER BY identity_key`)
	if err != nil {
		f.t.Fatalf("show ids: %v", err)
	}
	for shows.Next() {
		var id, key, title, sortTitle string
		var year, needsReview int
		if err := shows.Scan(&id, &key, &title, &year, &sortTitle, &needsReview); err != nil {
			f.t.Fatalf("scan show id: %v", err)
		}
		fmt.Fprintf(&b, "show-id %s %s title=%q year=%d sort=%q review=%d\n",
			key, id, title, year, sortTitle, needsReview)
	}
	_ = shows.Close()

	rows, err := f.db.Query(
		`SELECT sh.identity_key, s.season_number, s.id FROM seasons s
		   JOIN shows sh ON sh.id = s.show_id
		  WHERE sh.library_id = 'libtv' ORDER BY sh.identity_key, s.season_number`)
	if err != nil {
		f.t.Fatalf("season ids: %v", err)
	}
	for rows.Next() {
		var showKey, id string
		var n int
		if err := rows.Scan(&showKey, &n, &id); err != nil {
			f.t.Fatalf("scan season id: %v", err)
		}
		fmt.Fprintf(&b, "season-id %s %d %s\n", showKey, n, id)
	}
	_ = rows.Close()

	trows, err := f.db.Query(
		`SELECT id, identity_key, kind, enrichment_status, COALESCE(season_id, ''),
		        COALESCE(NULLIF(enrichment_tmdb_id, ''), tmdb_id),
		        COALESCE(NULLIF(enrichment_imdb_id, ''), imdb_id),
		        COALESCE(enrichment_season, -1), COALESCE(enrichment_episode, -1)
		   FROM titles WHERE library_id = 'libtv' ORDER BY identity_key`)
	if err != nil {
		f.t.Fatalf("title ids: %v", err)
	}
	for trows.Next() {
		var id, key, kind, status, seasonID, tmdbID, imdbID string
		var pinSeason, pinEpisode int
		if err := trows.Scan(&id, &key, &kind, &status, &seasonID,
			&tmdbID, &imdbID, &pinSeason, &pinEpisode); err != nil {
			f.t.Fatalf("scan title id: %v", err)
		}
		fmt.Fprintf(&b, "title-id %s %s kind=%s enrichment=%s season_id=%s record=%s/%d/%d imdb=%s\n",
			key, id, kind, status, seasonID, tmdbID, pinSeason, pinEpisode, imdbID)
	}
	_ = trows.Close()

	frows, err := f.db.Query(
		`SELECT fi.id, fi.path, t.identity_key, fi.container, fi.video_codec, fi.audio_codec,
		        fi.width, fi.height, fi.bitrate, fi.duration_ms, fi.size_bytes, fi.mtime,
		        fi.present, fi.part_ordinal
		   FROM files fi JOIN editions e ON e.id = fi.edition_id
		   JOIN titles t ON t.id = e.title_id
		  WHERE t.library_id = 'libtv' ORDER BY fi.path, t.identity_key`)
	if err != nil {
		f.t.Fatalf("files: %v", err)
	}
	type fileRow struct{ id, line string }
	var fileRows []fileRow
	for frows.Next() {
		var id, path, key, container, vcodec, acodec, mtime string
		var width, height, present, part int
		var bitrate, dur, size int64
		if err := frows.Scan(&id, &path, &key, &container, &vcodec, &acodec, &width, &height,
			&bitrate, &dur, &size, &mtime, &present, &part); err != nil {
			f.t.Fatalf("scan file: %v", err)
		}
		fileRows = append(fileRows, fileRow{id: id, line: fmt.Sprintf(
			"file-row %s %s id=%s container=%s v=%s a=%s %dx%d bitrate=%d dur=%d size=%d mtime=%s present=%d part=%d\n",
			f.rel(path), key, id, container, vcodec, acodec, width, height, bitrate, dur, size, mtime, present, part)})
	}
	_ = frows.Close()
	for _, fr := range fileRows {
		b.WriteString(fr.line)
		srows, err := f.db.Query(
			`SELECT stream_index, kind, codec, language, width, height, channels, is_default
			   FROM streams WHERE file_id = ? ORDER BY stream_index`, fr.id)
		if err != nil {
			f.t.Fatalf("streams: %v", err)
		}
		for srows.Next() {
			var idx, width, height, channels, isDefault int
			var kind, codec, lang string
			if err := srows.Scan(&idx, &kind, &codec, &lang, &width, &height, &channels, &isDefault); err != nil {
				f.t.Fatalf("scan stream: %v", err)
			}
			fmt.Fprintf(&b, "  stream %d %s %s lang=%q %dx%d ch=%d default=%d\n",
				idx, kind, codec, lang, width, height, channels, isDefault)
		}
		_ = srows.Close()
	}

	arows, err := f.db.Query(
		`SELECT ea.entity_type, ea.role, ea.path, ea.source,
		        COALESCE(sh.identity_key, se.identity_key, '')
		   FROM entity_artwork ea
		   LEFT JOIN shows sh ON sh.id = ea.entity_id AND ea.entity_type = 'show'
		   LEFT JOIN seasons se ON se.id = ea.entity_id AND ea.entity_type = 'season'
		  WHERE sh.library_id = 'libtv'
		     OR se.show_id IN (SELECT id FROM shows WHERE library_id = 'libtv')
		  ORDER BY ea.entity_type, ea.role, ea.path`)
	if err != nil {
		f.t.Fatalf("entity artwork: %v", err)
	}
	for arows.Next() {
		var kind, role, path, source, owner string
		if err := arows.Scan(&kind, &role, &path, &source, &owner); err != nil {
			f.t.Fatalf("scan entity artwork: %v", err)
		}
		fmt.Fprintf(&b, "art %s %s role=%s src=%s %s\n", kind, owner, role, source, f.rel(path))
	}
	_ = arows.Close()

	trows2, err := f.db.Query(
		`SELECT t.identity_key, a.role, a.path, a.source FROM artwork a
		   JOIN titles t ON t.id = a.title_id
		  WHERE t.library_id = 'libtv' ORDER BY t.identity_key, a.role, a.path`)
	if err != nil {
		f.t.Fatalf("title artwork: %v", err)
	}
	for trows2.Next() {
		var key, role, path, source string
		if err := trows2.Scan(&key, &role, &path, &source); err != nil {
			f.t.Fatalf("scan title artwork: %v", err)
		}
		fmt.Fprintf(&b, "art title %s role=%s src=%s %s\n", key, role, source, f.rel(path))
	}
	_ = trows2.Close()

	wrows, err := f.db.Query(
		`SELECT w.user_id, t.identity_key, w.resume_position_ms, w.watched
		   FROM watch_state w JOIN titles t ON t.id = w.title_id
		  WHERE t.library_id = 'libtv' ORDER BY w.user_id, t.identity_key`)
	if err != nil {
		f.t.Fatalf("watch state: %v", err)
	}
	defer wrows.Close()
	for wrows.Next() {
		var user, key string
		var resume int64
		var watched int
		if err := wrows.Scan(&user, &key, &resume, &watched); err != nil {
			f.t.Fatalf("scan watch state: %v", err)
		}
		fmt.Fprintf(&b, "watch %s %s resume=%d watched=%d\n", user, key, resume, watched)
	}
	return b.String()
}

// rel renders an absolute path relative to the Show folder, so expectations read
// as the Admin's own filenames rather than as temp-dir noise.
func (f *rescanFixture) rel(path string) string {
	if r, err := filepath.Rel(f.show, path); err == nil {
		return filepath.ToSlash(r)
	}
	return path
}

// --- the invariant ---------------------------------------------------------

// assertRescanIsNoop is the whole point of the file. It snapshots the catalog as
// Apply left it, runs the scan twice, and demands both snapshots be identical to
// the first. Twice, because a writer that converges on the second pass is still
// wrong on the first — and the first is the one that runs unattended.
func (f *rescanFixture) assertRescanIsNoop(rescan func()) {
	f.t.Helper()
	applied := f.snapshot()
	rescan()
	first := f.snapshot()
	if diff := diffLines(applied, first); diff != "" {
		f.t.Fatalf("the first rescan rearranged the Show Apply had just sorted:\n%s", diff)
	}
	rescan()
	second := f.snapshot()
	if diff := diffLines(first, second); diff != "" {
		f.t.Fatalf("the second rescan was not idempotent:\n%s", diff)
	}
}

func (f *rescanFixture) assertStructure(want string) {
	f.t.Helper()
	if diff := diffLines(strings.TrimSpace(want)+"\n", f.structure()); diff != "" {
		f.t.Fatalf("arrangement is not what was asked for:\n%s", diff)
	}
}

// diffLines reports the first differing lines of two renderings, plus every
// line that appears in one and not the other, which is enough to name the field
// that moved without pulling in a diff dependency.
func diffLines(want, got string) string {
	if want == got {
		return ""
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	var b strings.Builder
	counts := map[string]int{}
	for _, l := range wl {
		counts[l]++
	}
	for _, l := range gl {
		counts[l]--
	}
	var keys []string
	for k, v := range counts {
		if v != 0 && strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if counts[k] > 0 {
			fmt.Fprintf(&b, "  - %s\n", k)
		} else {
			fmt.Fprintf(&b, "  + %s\n", k)
		}
	}
	if b.Len() == 0 {
		// Same multiset of lines, different order.
		for i := range wl {
			if i >= len(gl) || wl[i] != gl[i] {
				fmt.Fprintf(&b, "  line %d reordered:\n  - %s\n  + %s\n", i+1, wl[i], gl[minInt(i, len(gl)-1)])
				break
			}
		}
	}
	return b.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- the arrangements ------------------------------------------------------

// rescanCase is one arrangement the matcher can produce: the Files on disk, the
// decision set the Admin pressed Apply on, and the arrangement that must result —
// stated absolutely, because a before/after comparison alone cannot fail when the
// derivation both writers share is the thing that broke.
type rescanCase struct {
	name  string
	files []string
	// decide builds the decision set. It takes the fixture so it can name files
	// relative to the Show folder.
	decide func(f *rescanFixture) []store.FileDecision
	// want is the arrangement Apply must produce, and that both scans must leave
	// exactly as they found it.
	want string
	// wantDisplaced names Files Apply had to write a decision for on the Admin's
	// behalf (a parsed File pushed off a Slot a Placement took).
	wantDisplaced []string
}

const (
	relE01 = "Season 01/The Bear (2022) - S01E01 - System.mkv"
	relE02 = "Season 01/The Bear (2022) - S01E02 - Hands.mkv"
	relE03 = "Season 01/The Bear (2022) - S01E03 - Brigade.mkv"
	relE04 = "Season 01/The Bear (2022) - S01E04 - Dogs.mkv"
	relS00 = "Specials/The Bear (2022) - S00E01 - Behind.mkv"
	relS02 = "Season 02/The Bear (2022) - S02E01 - Beef.mkv"
)

var oneSeason = []string{relE01, relE02, relE03, relE04}
var twoSeasons = []string{relE01, relE02, relS02, "Season 02/The Bear (2022) - S02E02 - Pasta.mkv"}
var withSpecials = []string{relE01, relE02, relS00}

var rescanCases = []rescanCase{
	{
		name: "relabel-within-a-season",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
  the bear|2022|s01e05 S01E05 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv placed g=1 s=5 ord=1 orphaned=0
`,
		files: oneSeason,
		// The file everyone can see is episode 3 is really episode 5. Nothing moves
		// on disk; the Slot moves.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE03, 1, 5, 1)}
		},
	},
	{
		name: "cross-season-move-into-a-folderless-season",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=1 present=1
  the bear|2022|s02e02 S02E02 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv placed g=2 s=1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv placed g=2 s=2 ord=1 orphaned=0
`,
		files: oneSeason,
		// ADR-0044's motivating case: the tail of a Season folder is really the next
		// season. Season 2 gets a row with no folder behind it.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE03, 2, 1, 1), f.place(relE04, 2, 2, 1)}
		},
	},
	{
		name: "cross-season-move-that-empties-a-season",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Beef" sort="beef" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 02/The Bear (2022) - S02E01 - Beef.mkv part=1 present=1
  the bear|2022|s01e04 S01E04 name="Pasta" sort="pasta" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 02/The Bear (2022) - S02E02 - Pasta.mkv part=1 present=1
season 2 key=the bear|2022|s02 hidden=1
decision Season 02/The Bear (2022) - S02E01 - Beef.mkv placed g=1 s=3 ord=1 orphaned=0
decision Season 02/The Bear (2022) - S02E02 - Pasta.mkv placed g=1 s=4 ord=1 orphaned=0
`,
		files: twoSeasons,
		// The other direction: Season 2's whole folder is really more of Season 1,
		// so the Season 2 row is left with nothing and disappears from browse.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{
				f.place(relS02, 1, 3, 1),
				f.place("Season 02/The Bear (2022) - S02E02 - Pasta.mkv", 1, 4, 1),
			}
		},
	},
	{
		name: "move-into-specials",
		want: `
show the bear|2022 hidden=0
season 0 key=the bear|2022|s00 hidden=0
  the bear|2022|s00e01 S00E01 name="Behind" sort="behind" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Specials/The Bear (2022) - S00E01 - Behind.mkv part=0 present=1
  the bear|2022|s00e02 S00E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=0 s=2 ord=1 orphaned=0
`,
		files: withSpecials,
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE02, 0, 2, 1)}
		},
	},
	{
		name: "move-out-of-specials",
		want: `
show the bear|2022 hidden=0
season 0 key=the bear|2022|s00 hidden=1
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Behind" sort="behind" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Specials/The Bear (2022) - S00E01 - Behind.mkv part=1 present=1
decision Specials/The Bear (2022) - S00E01 - Behind.mkv placed g=1 s=3 ord=1 orphaned=0
`,
		files: withSpecials,
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relS00, 1, 3, 1)}
		},
	},
	{
		name: "swap-two-slots",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
  the bear|2022|s01e02 S01E02 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=2 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=1 ord=1 orphaned=0
`,
		files: oneSeason,
		// The swap is the case that needs applyRekeysTx's temporary key: each Title
		// wants the identity_key the other still holds.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE01, 1, 2, 1), f.place(relE02, 1, 1, 1)}
		},
	},
	{
		name: "merge-two-files-onto-one-slot",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=2 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=1 ord=2 orphaned=0
`,
		files: oneSeason,
		// Two Files, one Slot, explicit ordinals: ONE Episode with a two-File joint
		// Edition. The Title the second File vacated is parked as an empty hidden
		// revivable row, which is exactly what a scan leaves behind.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE01, 1, 1, 1), f.place(relE02, 1, 1, 2)}
		},
	},
	{
		name: "merge-in-ordinal-order-not-alphabetical-order",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=2 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=1 ord=2 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=1 ord=1 orphaned=0
`,
		files: oneSeason,
		// Ordinal 1 is the alphabetically LATER file, so a writer that quietly falls
		// back to ORDER BY path puts the halves of the Episode on backwards — and the
		// joint timeline with them.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE02, 1, 1, 1), f.place(relE01, 1, 1, 2)}
		},
	},
	{
		name: "merge-onto-a-slot-neither-file-held",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
  the bear|2022|s01e09 S01E09 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=2 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=9 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=9 ord=2 orphaned=0
`,
		files: oneSeason,
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE01, 1, 9, 1), f.place(relE02, 1, 9, 2)}
		},
	},
	{
		name: "merge-across-season-folders",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
      file Season 02/The Bear (2022) - S02E01 - Beef.mkv part=2 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Beef" sort="beef" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
  the bear|2022|s02e02 S02E02 name="Pasta" sort="pasta" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 02/The Bear (2022) - S02E02 - Pasta.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=1 ord=1 orphaned=0
decision Season 02/The Bear (2022) - S02E01 - Beef.mkv placed g=1 s=1 ord=2 orphaned=0
`,
		files: twoSeasons,
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE01, 1, 1, 1), f.place(relS02, 1, 1, 2)}
		},
	},
	{
		name: "merge-with-non-contiguous-ordinals",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=2 present=1
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=5 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=1 ord=5 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=1 ord=2 orphaned=0
`,
		files: oneSeason,
		// Ordinals are an ORDER, not an index. 2 and 5 must join in that order and
		// persist as those numbers.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE01, 1, 1, 5), f.place(relE02, 1, 1, 2)}
		},
	},
	{
		name: "split-one-file-across-two-slots",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs (S01E04)" sort="dogs (s01e04)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=1 present=1
  the bear|2022|s01e05 S01E05 name="Dogs (S01E05)" sort="dogs (s01e05)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv placed g=1 s=4 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv placed g=1 s=5 ord=1 orphaned=0
`,
		files: oneSeason,
		// The co-File sibling shape: two Titles, one path, distinguishable names.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE04, 1, 4, 1), f.place(relE04, 1, 5, 1)}
		},
	},
	{
		name: "ignored-file",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=0
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv ignored g=-1 s=-1 ord=1 orphaned=0
`,
		files: oneSeason,
		// Settled and silent: no Episode, no Unmatched row, and the File row is
		// soft-deleted so it leaves browse. Nothing on disk is touched.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.settle(relE03, store.DecisionIgnored)}
		},
	},
	{
		name: "unassigned-file",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=0
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv unassigned g=-1 s=-1 ord=1 orphaned=0
`,
		files: oneSeason,
		// Undecided but RECORDED — the state that exists because sparse storage
		// spends "no row" on "follow the parse".
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.settle(relE03, store.DecisionUnassigned)}
		},
	},
	{
		name: "ignore-every-file-in-the-show",
		want: `
show the bear|2022 hidden=1
season 1 key=the bear|2022|s01 hidden=1
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=0
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=0
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=0
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=0
decision Season 01/The Bear (2022) - S01E01 - System.mkv ignored g=-1 s=-1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv ignored g=-1 s=-1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv ignored g=-1 s=-1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv ignored g=-1 s=-1 ord=1 orphaned=0
`,
		files: oneSeason,
		decide: func(f *rescanFixture) []store.FileDecision {
			var out []store.FileDecision
			for _, rel := range oneSeason {
				out = append(out, f.settle(rel, store.DecisionIgnored))
			}
			return out
		},
	},
	{
		name: "displacing-a-parsed-file-writes-its-decision",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=1 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv unassigned g=-1 s=-1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv placed g=1 s=2 ord=1 orphaned=0
`,
		files: oneSeason,
		// The collision Apply must not be able to create: E04's file is placed on the
		// Slot E02's file still PARSES to. Apply records the displacement as an
		// explicit Unassigned, because leaving it unrecorded means the next scan
		// re-places it from the very filename it was displaced from.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE04, 1, 2, 1)}
		},
		wantDisplaced: []string{relE02},
	},
	{
		name: "a-range-file-half-placed",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Double" sort="double" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05-E06 - Double.mkv part=1 present=1
  the bear|2022|s01e06 S01E06 name="Double (S01E06)" sort="double (s01e06)" label="" review=0 ambiguous=0 hidden=1
decision Season 01/The Bear (2022) - S01E05-E06 - Double.mkv placed g=1 s=2 ord=1 orphaned=0
`,
		files: []string{relE01, "Season 01/The Bear (2022) - S01E05-E06 - Double.mkv"},
		// A range file already resolves to two co-File sibling Titles. Placing the
		// file records ONE Slot for it, so both of them cease to serve a Slot: S01E05
		// is the row that MOVES onto the placed Slot (it carries the watch state
		// there), and S01E06 is emptied — the shared File row it held is reclaimed by
		// the tree write's Slot (reclaimEmptiedFilesTx, file-matcher/08), leaving the
		// key as the empty, hidden, revivable row a scan leaves for any emptied
		// Episode. It is FILE-less, not merely hidden: an Edition holding a file that
		// another Slot serves is what kept the removed Episode in browse forever.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place("Season 01/The Bear (2022) - S01E05-E06 - Double.mkv", 1, 2, 1)}
		},
	},
	{
		name: "a-slot-that-lands-back-on-an-emptied-title",
		// S01E03's Title is emptied — its File was merged onto S01E01, so it serves
		// no Slot of its own — and the SAME key is then handed to a Slot of the split
		// file, whose own rows were both claimed by S01E05 and S01E06. So the Title
		// the arrangement emptied is the very row the tree write lands back on, by
		// key. The reclaim of an emptied Title's Files must therefore skip a row the
		// tree wrote, or it deletes the File it was just given.
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=2 present=1
  the bear|2022|s01e03 S01E03 name="Double (S01E03)" sort="double (s01e03)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05-E06 - Double.mkv part=1 present=1
  the bear|2022|s01e05 S01E05 name="Double (S01E05)" sort="double (s01e05)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05-E06 - Double.mkv part=1 present=1
  the bear|2022|s01e06 S01E06 name="Double (S01E06)" sort="double (s01e06)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05-E06 - Double.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv placed g=1 s=1 ord=2 orphaned=0
decision Season 01/The Bear (2022) - S01E05-E06 - Double.mkv placed g=1 s=3 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E05-E06 - Double.mkv placed g=1 s=5 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E05-E06 - Double.mkv placed g=1 s=6 ord=1 orphaned=0
`,
		files: []string{relE01, relE03, "Season 01/The Bear (2022) - S01E05-E06 - Double.mkv"},
		decide: func(f *rescanFixture) []store.FileDecision {
			const dbl = "Season 01/The Bear (2022) - S01E05-E06 - Double.mkv"
			return []store.FileDecision{
				f.place(relE01, 1, 1, 1), f.place(relE03, 1, 1, 2),
				f.place(dbl, 1, 3, 1), f.place(dbl, 1, 5, 1), f.place(dbl, 1, 6, 1),
			}
		},
	},
	{
		name: "loose-episodes-with-no-season-folder",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file The Bear (2022) - S01E01 - System.mkv part=0 present=1
season 3 key=the bear|2022|s03 hidden=0
  the bear|2022|s03e04 S03E04 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
decision The Bear (2022) - S01E02 - Hands.mkv placed g=3 s=4 ord=1 orphaned=0
`,
		files: []string{"The Bear (2022) - S01E01 - System.mkv", "The Bear (2022) - S01E02 - Hands.mkv"},
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place("The Bear (2022) - S01E02 - Hands.mkv", 3, 4, 1)}
		},
	},
	{
		name: "a-season-folder-that-claims-no-slot",
		// A Season a Placement gave nothing to must get NO row, and a Season folder
		// on disk whose only file is Unmatched must not conjure one either — Seasons
		// follow the numbers Episodes CLAIM, never the folders (ADR-0044). Without a
		// Season nobody claims, both writers agree by accident.
		files: []string{relE01, relE02, "Season 05/nameless rip.mkv"},
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{f.place(relE02, 2, 1, 1)}
		},
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=2 s=1 ord=1 orphaned=0
unmatched Season 05/nameless rip.mkv
`,
	},
	{
		name: "a-mixed-show",
		want: `
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
  the bear|2022|s01e02 S01E02 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=1 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition ""
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=1 present=1
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=2 present=1
  the bear|2022|s01e04 S01E04 name="Sheridan (S01E04)" sort="sheridan (s01e04)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05 - Sheridan.mkv part=1 present=1
  the bear|2022|s01e05 S01E05 name="Sheridan (S01E05)" sort="sheridan (s01e05)" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E05 - Sheridan.mkv part=1 present=1
  the bear|2022|s01e07 S01E07 name="Review" sort="review" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E07 - Review.mkv part=0 present=0
  the bear|2022|s01e08 S01E08 name="Braciole" sort="braciole" label="" review=0 ambiguous=0 hidden=1
    edition "1080p"
      file Season 01/The Bear (2022) - S01E08 - Braciole.mkv part=0 present=0
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Ceres" sort="ceres" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E06 - Ceres.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E01 - System.mkv placed g=1 s=2 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=1 s=1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv placed g=1 s=3 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E04 - Dogs.mkv placed g=1 s=3 ord=2 orphaned=0
decision Season 01/The Bear (2022) - S01E05 - Sheridan.mkv placed g=1 s=4 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E05 - Sheridan.mkv placed g=1 s=5 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E06 - Ceres.mkv placed g=2 s=1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E07 - Review.mkv ignored g=-1 s=-1 ord=1 orphaned=0
decision Season 01/The Bear (2022) - S01E08 - Braciole.mkv unassigned g=-1 s=-1 ord=1 orphaned=0
`,
		files: []string{relE01, relE02, relE03, relE04,
			"Season 01/The Bear (2022) - S01E05 - Sheridan.mkv",
			"Season 01/The Bear (2022) - S01E06 - Ceres.mkv",
			"Season 01/The Bear (2022) - S01E07 - Review.mkv",
			"Season 01/The Bear (2022) - S01E08 - Braciole.mkv"},
		// Every shape at once, because the two writers can agree on each in isolation
		// and still disagree about the order they interact in.
		decide: func(f *rescanFixture) []store.FileDecision {
			return []store.FileDecision{
				f.place(relE01, 1, 2, 1), // swap
				f.place(relE02, 1, 1, 1),
				f.place(relE03, 1, 3, 1), // merge
				f.place(relE04, 1, 3, 2),
				f.place("Season 01/The Bear (2022) - S01E05 - Sheridan.mkv", 1, 4, 1), // split
				f.place("Season 01/The Bear (2022) - S01E05 - Sheridan.mkv", 1, 5, 1),
				f.place("Season 01/The Bear (2022) - S01E06 - Ceres.mkv", 2, 1, 1), // cross-season
				f.settle("Season 01/The Bear (2022) - S01E07 - Review.mkv", store.DecisionIgnored),
				f.settle("Season 01/The Bear (2022) - S01E08 - Braciole.mkv", store.DecisionUnassigned),
			}
		},
	},
}

// TestApplyThenRescanIsANoop is ADR-0044's invariant, over every arrangement the
// matcher can produce and both kinds of scan.
//
// Each case is checked three ways, and all three are needed:
//   - the arrangement Apply produced is the one that was asked for (absolute);
//   - the first scan changes nothing (the overnight case);
//   - the second scan changes nothing either (a writer that CONVERGES on pass two
//     was still wrong on pass one, and pass one is the unattended one).
func TestApplyThenRescanIsANoop(t *testing.T) {
	for _, tc := range rescanCases {
		for _, mode := range []struct {
			name   string
			rescan func(*rescanFixture)
		}{
			{"full", func(f *rescanFixture) { f.fullScan() }},
			// The Targeted scan (ADR-0030) loads the decisions through its own path
			// in targeted.go, and is exactly what runs right after an Apply.
			{"targeted", func(f *rescanFixture) { f.targetedScan() }},
		} {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				f := newRescanFixture(t, tc.files...)
				res := f.apply(tc.decide(f)...)
				var displaced []string
				for _, p := range res.Displaced {
					displaced = append(displaced, f.rel(p))
				}
				if strings.Join(displaced, ",") != strings.Join(tc.wantDisplaced, ",") {
					t.Fatalf("displaced = %v, want %v", displaced, tc.wantDisplaced)
				}
				if len(res.Deferred) != 0 {
					t.Fatalf("nothing here should be deferred, got %v", res.Deferred)
				}
				f.assertStructure(tc.want)
				f.assertRescanIsNoop(func() { mode.rescan(f) })
			})
		}
	}
}

// --- watch state -----------------------------------------------------------

// The fold is the one thing a rescan cannot check by comparing itself to itself:
// a scan never folds, so a broken fold produces the same wrong number before and
// after and the no-op assertion sails straight past it. Its values are therefore
// asserted absolutely, and only then handed to the scans.

// TestApplyThenRescanKeepsWatchStateThroughAMove: a move re-keys the row IN PLACE,
// so title_id — and every User's history hanging off it — never moves. This is the
// deliberate opposite of a Wrong-item correction, which resets (ADR-0019).
func TestApplyThenRescanKeepsWatchStateThroughAMove(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	before := f.titleIDForPath(relE01)
	f.setWatch("u1", before, 0, true)
	f.setWatch("u2", before, 250, false)

	f.apply(f.place(relE01, 2, 5, 1))

	after := f.titleIDAt(2, 5)
	if after != before {
		t.Fatalf("title id moved %q → %q; the re-key must be an UPDATE", before, after)
	}
	if ws := f.watch("u1", after); !ws.Watched {
		t.Fatalf("u1 lost their watched flag in the move: %+v", ws)
	}
	if ws := f.watch("u2", after); ws.Watched || ws.ResumePositionMs != 250 {
		t.Fatalf("u2's resume did not follow the move: %+v", ws)
	}
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestApplyThenRescanFoldsAMergeOntoTheJointTimeline: one part finished and the
// next barely started is NOT a watched Episode. It resumes where the viewer
// actually stopped once the two Files are one Edition — the running start of the
// unfinished part plus its own position. Watched-if-any is precisely the bug the
// multi-part duration work exists to prevent (ADR-0028).
func TestApplyThenRescanFoldsAMergeOntoTheJointTimeline(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	firstID, secondID := f.titleIDForPath(relE01), f.titleIDForPath(relE02)
	f.setWatch("u1", firstID, 0, true) // part one finished
	f.setWatch("u1", secondID, 400, false)
	f.setWatch("u2", firstID, 0, true) // both parts finished
	f.setWatch("u2", secondID, 0, true)
	partOne := f.durationOf(relE01)

	f.apply(f.place(relE01, 1, 1, 1), f.place(relE02, 1, 1, 2))
	merged := f.titleIDAt(1, 1)

	if ws := f.watch("u1", merged); ws.Watched || ws.ResumePositionMs != partOne+400 {
		t.Fatalf("u1 folded to %+v, want unwatched resuming at %d (part one + 400)",
			ws, partOne+400)
	}
	if ws := f.watch("u2", merged); !ws.Watched || ws.ResumePositionMs != 0 {
		t.Fatalf("u2 folded to %+v, want watched with the resume cleared", ws)
	}
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestApplyThenRescanCopiesWatchStateAcrossASplit: co-File siblings each inherit
// the original's state, which is what the playback layer already maintains between
// them.
func TestApplyThenRescanCopiesWatchStateAcrossASplit(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	origin := f.titleIDForPath(relE04)
	f.setWatch("u1", origin, 700, false)

	f.apply(f.place(relE04, 1, 4, 1), f.place(relE04, 1, 5, 1))

	for _, slot := range [][2]int{{1, 4}, {1, 5}} {
		id := f.titleIDAt(slot[0], slot[1])
		if ws := f.watch("u1", id); ws.Watched || ws.ResumePositionMs != 700 {
			t.Fatalf("S%02dE%02d has %+v, want the original's resume of 700", slot[0], slot[1], ws)
		}
	}
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// --- the differences that are real, and are therefore pinned ----------------

// TestApplyDefersAnUnprobedFileAndTheScanBuildsIt is the one documented case where
// apply-then-rescan is NOT a no-op, and it is asserted rather than skipped: a
// Placement onto a File the catalog has never probed (an Unmatched file the Admin
// placed for the first time). Apply never runs ffprobe, so it cannot invent the
// Episode's attributes; it stores the decision and reports the path as Deferred.
// The next scan probes the file and builds the Episode — and every scan after that
// changes nothing.
func TestApplyDefersAnUnprobedFileAndTheScanBuildsIt(t *testing.T) {
	const rip = "Season 01/mystery-rip.mkv"
	f := newRescanFixture(t, append(append([]string{}, oneSeason...), rip)...)

	res := f.apply(f.place(rip, 1, 9, 1))
	if len(res.Deferred) != 1 || f.rel(res.Deferred[0]) != rip {
		t.Fatalf("Deferred = %v, want just the never-probed file", res.Deferred)
	}
	var built string
	if err := f.db.QueryRow(
		`SELECT id FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		rescanEpisodeKey(1, 9)).Scan(&built); err == nil {
		t.Fatalf("Apply built Title %q from a file it never probed", built)
	}
	// The decision IS stored, which is the whole point: the Scanner replays it.
	if got := f.decisionLines(); len(got) != 1 || !strings.Contains(got[0], "placed g=1 s=9") {
		t.Fatalf("decisions = %v, want the placement stored", got)
	}

	f.fullScan()
	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e03 S01E03 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
  the bear|2022|s01e09 S01E09 name="S01E09" sort="s01e09" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/mystery-rip.mkv part=1 present=1
decision Season 01/mystery-rip.mkv placed g=1 s=9 ord=1 orphaned=0
`)
	// From here on the invariant holds normally.
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestScanOnlySideEffectsAfterApply pins the three whole-Library things a full scan
// owns and Apply deliberately does not, so "the first scan changed something" is
// never a mystery: the Unmatched list, local Season artwork, and the orphan flag on
// a Placement whose file has gone. A Targeted scan owns none of them either
// (ADR-0031), which is asserted here too.
func TestScanOnlySideEffectsAfterApply(t *testing.T) {
	t.Run("the-unmatched-list", func(t *testing.T) {
		const rip = "Season 01/nameless rip.mkv"
		f := newRescanFixture(t, relE01, rip)
		if got := f.unmatchedLines(); len(got) != 1 {
			t.Fatalf("seed unmatched = %v, want the unparseable file", got)
		}
		// Settling the file removes it from the queue, but the Unmatched LIST is a
		// whole-Library projection only a full scan rewrites.
		f.apply(f.settle(rip, store.DecisionIgnored))
		if got := f.unmatchedLines(); len(got) != 1 {
			t.Fatalf("Apply rewrote the Unmatched list: %v", got)
		}
		f.targetedScan()
		if got := f.unmatchedLines(); len(got) != 1 {
			t.Fatalf("a Targeted scan rewrote the Unmatched list: %v", got)
		}
		f.fullScan()
		if got := f.unmatchedLines(); len(got) != 0 {
			t.Fatalf("the full scan left the settled file Unmatched: %v", got)
		}
		f.assertRescanIsNoop(f.fullScan)
	})

	t.Run("local-season-artwork", func(t *testing.T) {
		f := newRescanFixture(t, "Season 02.jpg", relE01, relE02)
		// Season 2 has a poster in the Show folder but no folder of its own; a
		// Placement conjures the Season row, and the Scanner — which alone owns local
		// artwork — hangs the poster on it at the next scan.
		f.apply(f.place(relE02, 2, 1, 1))
		if got := f.seasonArtwork(2); got != "" {
			t.Fatalf("Apply wrote Season artwork (%q); that is the Scanner's job", got)
		}
		f.fullScan()
		if got := f.seasonArtwork(2); got != "Season 02.jpg" {
			t.Fatalf("season 2 poster = %q, want the local Season 02.jpg", got)
		}
		f.assertRescanIsNoop(f.fullScan)
		f.assertRescanIsNoop(f.targetedScan)
	})

	t.Run("orphaned-placement", func(t *testing.T) {
		f := newRescanFixture(t, oneSeason...)
		f.apply(f.place(relE03, 1, 7, 1))
		if err := os.Remove(f.path(relE03)); err != nil {
			t.Fatal(err)
		}
		// A Targeted scan does not run the orphan pass (a whole-Library operation),
		// so the correction is still flagged clean; the full scan surfaces it.
		f.targetedScan()
		if got := f.decisionLines(); !strings.Contains(got[0], "orphaned=0") {
			t.Fatalf("a Targeted scan surfaced the orphan: %v", got)
		}
		f.fullScan()
		if got := f.decisionLines(); !strings.Contains(got[0], "orphaned=1") {
			t.Fatalf("the full scan did not flag the broken Placement: %v", got)
		}
		// Flagged, never dropped — and stable from here.
		f.assertRescanIsNoop(f.fullScan)
	})
}

// TestApplyRevertsToTheFilenamesAndTheScanAgrees: taking every decision back is
// expressed by the ABSENCE of rows from the new set, which only a whole-set replace
// can say. The Show returns to what its filenames claim, and the scan keeps it
// there.
func TestApplyRevertsToTheFilenamesAndTheScanAgrees(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	seeded := f.structure()

	f.apply(f.place(relE01, 1, 1, 1), f.place(relE02, 1, 1, 2))
	f.fullScan()
	f.apply() // no decisions at all
	f.fullScan()

	if got := f.decisionLines(); len(got) != 0 {
		t.Fatalf("decisions survived the revert: %v", got)
	}
	// The merge left one Title parked as an empty hidden revivable row, exactly as a
	// scan would; everything else is back where the filenames put it.
	// A perfect round trip: the merge's parked Title was revived by its own key, so
	// the Show is character-for-character what the filenames first produced.
	f.assertStructure(seeded)
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestApplyLeavesOtherShowsAlone: a decision is stored per (library, path) with no
// Show on it, so an Apply that reached beyond its own Show would be replayed by
// every future scan against a Show the Admin was never looking at.
func TestApplyLeavesOtherShowsAlone(t *testing.T) {
	f := newRescanFixture(t, relE01, relE02)
	writeMediaFile(t, filepath.Join(f.root, "Severance (2022)", "Season 01",
		"Severance (2022) - S01E01 - Good News.mkv"))
	f.fullScan()

	f.apply(f.place(relE02, 2, 1, 1))
	// structure() covers every Show in the Library, so the other one being untouched
	// is stated, not inferred.
	f.assertStructure(`
show severance|2022 hidden=0
season 1 key=severance|2022|s01 hidden=0
  severance|2022|s01e01 S01E01 name="Good News" sort="good news" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file ../Severance (2022)/Season 01/Severance (2022) - S01E01 - Good News.mkv part=0 present=1
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E02 - Hands.mkv placed g=2 s=1 ord=1 orphaned=0
`)
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestApplyTakesTheOtherSlotOfASharedFileAway: undoing a split — one File across
// two Slots, then back to one — must remove the Slot the Admin took back.
//
// It is the one thing the tree write cannot do for itself. ApplyShowArrangement
// parks the non-surviving Title with its original key, expecting the tree write to
// have taken its Files away, but writeTitleSubtree's cross-Title reclaim is
// deliberately skipped for a path a co-File sibling already wrote in the same
// transaction — the rule that keeps a legitimate split's two File rows from
// stealing each other (store/multiepisode_test.go). For a SHARED path that same
// rule used to leave the parked Title holding a present File, so the hidden
// recompute kept it in browse and no scan ever repaired it: the stale Title is
// absent from the tree the Scanner builds, and the soft-delete pass only marks
// Files gone from DISK — this one is very much there, under another Title.
// reclaimEmptiedFilesTx closes exactly that gap (file-matcher/08).
//
// What is left behind is the same empty, hidden, revivable row every other emptied
// Episode leaves: the Slot stays offerable in the matcher and invisible in browse.
func TestApplyTakesTheOtherSlotOfASharedFileAway(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	f.apply(f.place(relE04, 1, 4, 1), f.place(relE04, 1, 5, 1))
	f.fullScan()

	// Take the second Slot back.
	f.apply(f.place(relE04, 1, 4, 1))
	if got := f.decisionLines(); len(got) != 1 {
		t.Fatalf("decisions = %v, want just the one remaining Slot", got)
	}
	var hidden int
	if err := f.db.QueryRow(
		`SELECT hidden FROM titles WHERE library_id = 'libtv' AND identity_key = ?`,
		rescanEpisodeKey(1, 5)).Scan(&hidden); err != nil {
		t.Fatalf("reading S01E05: %v", err)
	}
	if hidden != 1 {
		t.Fatalf("S01E05 is hidden=%d — the Slot the Admin took back is still in browse", hidden)
	}
	// Hidden is the derived cache, so state the cause too: the removed Slot owns no
	// File at all, and the shared path is left with exactly the one row S01E04
	// serves it from.
	if n := f.fileCountUnder(rescanEpisodeKey(1, 5)); n != 0 {
		t.Fatalf("S01E05 still owns %d File row(s) for a path another Slot serves", n)
	}
	if n := f.rowsForPath(relE04); n != 1 {
		t.Fatalf("the shared path has %d File rows, want just S01E04's", n)
	}
	// And both writers agree about it, which is what keeps the 4am scan from
	// resurrecting the Slot.
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}

// TestApplyKeepsAMissingFilesRowForRePlacing is the boundary that makes the
// reclaim above safe. Taking a File off its Slot soft-deletes its row rather than
// removing it (ADR-0008), and ShowFiles/placementInputs need that row to offer the
// File back — so the reclaim must only ever take a row whose path the NEW tree
// claims. A blanket "clear an emptied Title's Editions" would pass the test above
// and break this one.
func TestApplyKeepsAMissingFilesRowForRePlacing(t *testing.T) {
	f := newRescanFixture(t, oneSeason...)
	origin := f.titleIDForPath(relE03)
	f.setWatch("u1", origin, 900, false)

	// Off its Slot: the Title is emptied, the row stays as a Missing File.
	f.apply(f.settle(relE03, store.DecisionUnassigned))
	if n := f.rowsForPath(relE03); n != 1 {
		t.Fatalf("the unassigned File has %d rows, want its soft-deleted one", n)
	}
	f.assertRescanIsNoop(f.fullScan)

	// And back onto a different Slot, which is only possible because that row
	// survived: Apply never probes, so it has nothing else to build the Episode
	// from.
	f.apply(f.place(relE03, 2, 1, 1))
	f.assertStructure(`
show the bear|2022 hidden=0
season 1 key=the bear|2022|s01 hidden=0
  the bear|2022|s01e01 S01E01 name="System" sort="system" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E01 - System.mkv part=0 present=1
  the bear|2022|s01e02 S01E02 name="Hands" sort="hands" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E02 - Hands.mkv part=0 present=1
  the bear|2022|s01e04 S01E04 name="Dogs" sort="dogs" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E04 - Dogs.mkv part=0 present=1
season 2 key=the bear|2022|s02 hidden=0
  the bear|2022|s02e01 S02E01 name="Brigade" sort="brigade" label="" review=0 ambiguous=0 hidden=0
    edition "1080p"
      file Season 01/The Bear (2022) - S01E03 - Brigade.mkv part=1 present=1
decision Season 01/The Bear (2022) - S01E03 - Brigade.mkv placed g=2 s=1 ord=1 orphaned=0
`)
	// The row was REUSED, not replaced: it is the same Title row, so the watch
	// state rode along with it.
	if id := f.titleIDAt(2, 1); id != origin {
		t.Fatalf("S02E01 is title %q, want the re-placed File's own row %q", id, origin)
	}
	if ws := f.watch("u1", origin); ws.ResumePositionMs != 900 {
		t.Fatalf("the re-placed File lost its resume: %+v", ws)
	}
	f.assertRescanIsNoop(f.fullScan)
	f.assertRescanIsNoop(f.targetedScan)
}
