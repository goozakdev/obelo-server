package enrich

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0051, driven through real passes over a real store.
//
// The measured failure this mode exists for: issues 01–06 shipped, the operator
// rescanned a 10,550-track library, and 730 flagged Tracks became 722 — every one
// of them with a BLANK enrichment_reason, which proves no leaf was processed at
// all. A scan's auto-enrich is ModeNew, ModeNew admits 'pending' and due retries,
// and all 722 rows were 'unmatched'. Six slices of matching improvements were
// unreachable because nothing re-asked a settled non-answer.
//
// So the assertions here are mostly about the CALL LOG. "The recheck re-asked
// this row" and "the recheck skipped it" leave the same row in the database when
// the answer has not changed; the only place the two differ is in what the
// provider was asked, which is also where the entire cost of the feature lives.

// --- a settled 'unmatched' leaf is re-asked; a matched one is not -------------

// The headline for leaves. Two Tracks under the same matched Album, one settled
// 'unmatched' and one 'matched': a recheck asks about exactly the first.
func TestRecheckReAsksASettledUnmatchedTrackAndLeavesAMatchedOneAlone(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-1": "Whisper Your Name", "rec-2": "She"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			// The row the operator was looking at: a previous pass asked, the provider had
			// no record, and it settled. ADR-0050 gave the album a tracklist that names it.
			{id: "t1", title: "Whisper Your Name", num: 1, status: "unmatched",
				reason: store.EnrichmentReasonSearchNoMatch},
			// Already resolved. Nothing about it is stale, and re-asking it is the cost
			// that made ModeFull the wrong tool.
			{id: "t2", title: "She", num: 2, status: "matched", record: "rec-2"},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("the pass visited %d leaves, want 1 (calls: %v) — a recheck re-asks the "+
			"settled non-answers and nothing else", res.Total, prov.history())
	}
	if res.Matched != 1 {
		t.Fatalf("matched %d of 1 (calls: %v) — the improvement ADR-0050 shipped is exactly "+
			"what this row was waiting for", res.Matched, prov.history())
	}
	for _, c := range prov.history() {
		if c == "recording:rec-2" || c == "search:She" {
			t.Fatalf("the already-matched Track was asked about again (calls: %v) — that is "+
				"ModeFull's bill, and avoiding it is the whole point of a third mode",
				prov.history())
		}
	}
	if got := trackRow(t, db, "t1"); got.EnrichmentStatus != "matched" || got.MusicbrainzID != "rec-1" {
		t.Fatalf("t1 settled as %q / %q, want matched / rec-1", got.EnrichmentStatus, got.MusicbrainzID)
	}

	// ModeNew over the same library is the control: it is what a scan runs, and it
	// is what did nothing on the operator's machine.
	prov2 := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
	}
	svc2, _ := newAlbumFixture(t, prov2, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1, status: "unmatched"}},
	})
	if res, err := svc2.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("only-new pass: %v", err)
	} else if res.Total != 0 || len(prov2.history()) != 0 {
		t.Fatalf("an only-new pass visited %d leaves and made calls %v — if ModeNew already "+
			"saw this row, ADR-0051 would not exist", res.Total, prov2.history())
	}
}

// --- ADR-0048's line, held ----------------------------------------------------

// A PARKED failure is a settled non-answer and is re-asked. A failure whose retry
// is still in the future is IN-FLIGHT WORK the server already owns, and is not —
// blurring those two would undo the distinction ADR-0048 built the retry column
// for.
func TestRecheckReAsksAParkedFailureButNotOneAwaitingItsRetry(t *testing.T) {
	prov := &albumTierProvider{
		tracklist: []TrackCandidate{entry(1, "Parked", "rec-parked"), entry(2, "In Flight", "rec-inflight")},
		recordings: map[string]string{
			"rec-parked": "Parked", "rec-inflight": "In Flight",
		},
	}
	// The fixture's clock is 2026-09-01T12:00:00Z.
	future := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC).Format(time.RFC3339)
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Parked", num: 1, status: "failed"},
			{id: "t2", title: "In Flight", num: 2, status: "failed", retryAt: future},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("the pass visited %d leaves, want 1 (calls: %v)", res.Total, prov.history())
	}
	if got := trackRow(t, db, "t1"); got.EnrichmentStatus != "matched" {
		t.Fatalf("the parked failure settled as %q, want matched — a permanent refusal is a "+
			"settled non-answer, and nothing else in the system re-asks it",
			got.EnrichmentStatus)
	}
	if got := trackRow(t, db, "t2"); got.EnrichmentStatus != "failed" || got.EnrichmentRetryAt != future {
		t.Fatalf("the in-flight failure is now %q / retry %q, want failed / %s — a scheduled "+
			"retry is the server's own work, and a recheck must not reach into it (ADR-0048)",
			got.EnrichmentStatus, got.EnrichmentRetryAt, future)
	}
	for _, c := range prov.history() {
		if strings.Contains(c, "inflight") || c == "search:In Flight" {
			t.Fatalf("the in-flight Track was asked about (calls: %v)", prov.history())
		}
	}
}

// --- parents ------------------------------------------------------------------

// Half the motivating queue — 365 of 730 Tracks — sits under an Album that is
// itself unmatched, and no amount of re-asking a Track fixes that. So a recheck
// re-asks the ALBUM, and the Album newly matching is what makes its Tracks
// resolvable at all.
func TestRecheckReAsksAnUnmatchedAlbum(t *testing.T) {
	prov := &albumTierProvider{
		albumRG:    "rg-she", // the album SEARCH now succeeds
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityStatus: "unmatched", // settled, with no record of its own
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, status: "unmatched",
				reason: store.EnrichmentReasonAlbumUnmatched},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if n := prov.count("rg?search"); n != 1 {
		t.Fatalf("%d album searches, want 1 (calls: %v) — an unmatched Album is a settled "+
			"non-answer too, and re-asking the Track under it can never clear it",
			n, prov.history())
	}
	if got := trackRow(t, db, "t1"); got.EnrichmentStatus != "matched" {
		t.Fatalf("the Track settled as %q with reason %q, want matched — the Album matching "+
			"is what put its tracklist within reach", got.EnrichmentStatus, got.EnrichmentReason)
	}
	// And the reason is gone, not stale: the failure it described has stopped being
	// true (ADR-0050).
	if got := trackRow(t, db, "t1"); got.EnrichmentReason != store.EnrichmentReasonNone {
		t.Errorf("the matched Track kept the reason %q", got.EnrichmentReason)
	}
}

// The other half of the same trade: an Album that already matched is NOT looked
// up again, and still hands the tracklist tier its stored id as the anchor. This
// is what keeps bucket B (fine albums) cheap while bucket C (unmatched albums)
// becomes fixable.
func TestRecheckDoesNotReAskAMatchedAlbumButStillUsesItsStoredID(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
	}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1, status: "unmatched"}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if n := prov.count("rg"); n != 0 {
		t.Fatalf("the matched Album was looked up %d times (calls: %v) — it short-circuits "+
			"to its stored id in every mode but Full, and a recheck is not a refresh",
			n, prov.history())
	}
	if got, want := prov.history()[0], "tracklist:rg-she||1"; got != want {
		t.Fatalf("first call %q, want %q — the anchor a recheck hands the tracklist tier is "+
			"the Album's STORED record id, which is why not re-asking it costs nothing",
			got, want)
	}
}

// --- the cost floor -----------------------------------------------------------

// A recheck over a Library that is entirely matched makes ZERO provider calls.
// This is the property that lets an operator press the button without thinking
// about it, and the one that separates ModeRecheck from ModeFull.
func TestRecheckOnAFullyMatchedLibraryMakesNoProviderCalls(t *testing.T) {
	prov := &albumTierProvider{}
	svc, _ := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, status: "matched", record: "rec-1"},
			{id: "t2", title: "She", num: 2, status: "matched", record: "rec-2"},
			{id: "t3", title: "Follow the Music", num: 3, status: "matched", record: "rec-3"},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if len(prov.history()) != 0 {
		t.Fatalf("a recheck over a fully matched library made %d calls: %v — the Artist, the "+
			"Album and the tracklist read all have to stay silent for this to hold",
			len(prov.history()), prov.history())
	}
	if res.Total != 0 {
		t.Fatalf("the pass visited %d leaves, want 0", res.Total)
	}
}

// --- an Admin's choice is never re-searched -----------------------------------

// ADR-0045/0046 are untouched by the new mode: recheck changes which items are
// VISITED, never the precedence applied when they are. A Track carrying the
// Admin's chosen record resolves by that id.
func TestRecheckResolvesAChosenRecordByIDAndNeverSearches(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Something Else Entirely", "rec-tracklist")},
		recordings: map[string]string{"chosen-rec": "Chosen Track"},
		searchHits: map[string]string{"Chosen Track": "rec-search"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			// Settled 'unmatched' AND pinned: the provider had nothing behind the id last
			// time. A recheck revisits it — by the id, not by a search.
			{id: "t1", title: "Chosen Track", num: 1, status: "unmatched",
				record: "chosen-rec", origin: "chosen", recordingTag: "tag-ignored"},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if n := prov.count("search:"); n != 0 {
		t.Fatalf("the pass searched %d times (calls: %v) — a human's correction outranks "+
			"everything and is resolved by id in every mode", n, prov.history())
	}
	if n := prov.count("recording:chosen-rec"); n != 1 {
		t.Fatalf("the chosen record was looked up %d times (calls: %v), want 1", n, prov.history())
	}
	if got := trackRow(t, db, "t1"); got.MusicbrainzID != "chosen-rec" {
		t.Fatalf("the chosen record became %q — a recheck must never move it", got.MusicbrainzID)
	}
}

// --- re-ask, never reset ------------------------------------------------------

// A Track that re-asks and fails again stays 'unmatched' and gets its reason
// REWRITTEN. Nothing is lowered to 'pending' and nothing is cleared in advance:
// the stale reason is overwritten by the normal settle path, which is what makes
// the column the honest record of the most recent attempt (ADR-0050/0051).
func TestARecheckedTrackThatFailsAgainKeepsItsStatusAndGetsAFreshReason(t *testing.T) {
	prov := &albumTierProvider{
		// The Album matched and its tracklist was read — it simply has no room for this
		// Track. That is a different diagnosis, and a different action, from the one the
		// row is carrying.
		tracklist:  []TrackCandidate{entry(1, "A Different Song", "rec-other"), entry(2, "And Another", "rec-other2")},
		recordings: map[string]string{},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Nowhere On The Release", num: 1, status: "unmatched",
				reason: store.EnrichmentReasonAlbumUnmatched},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.Unmatched != 1 {
		t.Fatalf("unmatched %d of 1 (calls: %v)", res.Unmatched, prov.history())
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "unmatched" {
		t.Fatalf("status %q, want unmatched — a recheck re-asks, it does not reset: no status "+
			"is ever lowered to 'pending'", got.EnrichmentStatus)
	}
	if got.EnrichmentReason != store.EnrichmentReasonNotInTracklist {
		t.Fatalf("reason %q, want %q — the row was carrying %q from a previous pass, and a "+
			"reason that outlives the failure it described is worse than none",
			got.EnrichmentReason, store.EnrichmentReasonNotInTracklist,
			store.EnrichmentReasonAlbumUnmatched)
	}
}

// --- the twins: the Movie SQL and the TV/Music walk -----------------------------

// shouldProcessLeaf's own comment names store.TitlesForEnrichment its twin: the
// Movie path selects its leaves with a query, the TV and Music paths walk their
// parent trees. A third mode is a third chance for them to drift, and drift here
// is silent — a Movie library and a Music library would quietly disagree about
// what a recheck means.
//
// This drives BOTH paths over the same seeded population and asserts they select
// the same leaves, in ModeNew and in ModeRecheck.
func TestMovieSQLAndTheMusicWalkSelectTheSamePopulation(t *testing.T) {
	// The clock every path measures "due" against.
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	// One row per enrichment state the selection has to distinguish. `anchored` is
	// the record id, which the Music path needs so a leaf resolves by lookup rather
	// than by a search the fake would decline (the SELECTION is what is under test,
	// not the resolution).
	population := []struct{ name, status, retryAt string }{
		{"Pending", "pending", ""},
		{"Matched", "matched", ""},
		{"Unmatched", "unmatched", ""},
		{"Parked", "failed", ""},
		{"RetryDue", "failed", past},
		{"RetryFuture", "failed", future},
		{"Disabled", "disabled", ""},
	}

	for _, mode := range []struct {
		name string
		mode Mode
	}{{"ModeNew", ModeNew}, {"ModeRecheck", ModeRecheck}} {
		t.Run(mode.name, func(t *testing.T) {
			movie := selectedByMoviePath(t, population, mode.mode, now)
			music := selectedByMusicPath(t, population, mode.mode, now)
			if !sameStrings(movie, music) {
				t.Fatalf("the Movie SQL selected %v and the Music walk selected %v — these two "+
					"are twins by shouldProcessLeaf's own comment, and a mode they disagree "+
					"about means a Movie library and a Music library get different answers "+
					"from the same button", movie, music)
			}
			t.Logf("%s selects %v (both paths)", mode.name, movie)
		})
	}
}

// selectedByMoviePath runs a pass over a Movie Library seeded with population and
// returns the titles the pass actually visited, sorted. The visit is observed on
// the provider's call log, which is the only place "selected" is visible.
func selectedByMoviePath(t *testing.T, population []struct{ name, status, retryAt string },
	mode Mode, now time.Time) []string {
	t.Helper()
	db := openRecheckDB(t)
	exec := recheckExec(t, db)
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Movies', 'movie')`)
	for i, l := range population {
		exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
		                          enrichment_status, enrichment_retry_at, added_at)
		      VALUES (?, 'lib', 'movie', ?, ?, ?, ?, ?, ?)`,
			l.name, l.name, l.name+"|key", l.name, l.status, l.retryAt, i)
	}
	return runAndCollectVisits(t, db, mode, now)
}

// selectedByMusicPath is the same population under one already-matched Artist and
// Album, so nothing but shouldProcessLeaf decides which Tracks are visited.
func selectedByMusicPath(t *testing.T, population []struct{ name, status, retryAt string },
	mode Mode, now time.Time) []string {
	t.Helper()
	db := openRecheckDB(t)
	exec := recheckExec(t, db)
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('lib', 'Music', 'music')`)
	exec(`INSERT INTO artists (id, library_id, name, identity_key, sort_name)
	      VALUES ('ar1', 'lib', 'A', 'artist:a', 'a')`)
	exec(`INSERT INTO albums (id, artist_id, title, identity_key, sort_title)
	      VALUES ('al1', 'ar1', 'B', 'artist:a|album:b', 'b')`)
	// Both parents already matched: a recheck short-circuits them, so the only
	// variable left is the leaf rule.
	exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
	      VALUES ('artist', 'ar1', 'artist-1', 'matched')`)
	exec(`INSERT INTO entity_enrichment (entity_type, entity_id, external_id, enrichment_status)
	      VALUES ('album', 'al1', 'rg-1', 'matched')`)
	for i, l := range population {
		exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
		                          album_id, disc_number, track_number,
		                          enrichment_status, enrichment_retry_at, added_at)
		      VALUES (?, 'lib', 'track', ?, ?, ?, 'al1', 1, ?, ?, ?, ?)`,
			l.name, l.name, "artist:a|album:b|t"+l.name, l.name, i+1, l.status, l.retryAt, i)
	}
	return runAndCollectVisits(t, db, mode, now)
}

// visitLogger answers every lookup with ErrNoMatch and records the leaf titles it
// was asked about. It answers AlbumTracklist with ErrNoTracklist so the Music
// path's album tier contributes no ids and no noise — every leaf then reaches the
// provider by name, exactly as a Movie leaf does.
type visitLogger struct{ visited []string }

func (p *visitLogger) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind == "movie" || ref.Kind == "track" {
		p.visited = append(p.visited, ref.Title)
	}
	return TitleMetadata{}, ErrNoMatch
}

func (p *visitLogger) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *visitLogger) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

func (p *visitLogger) AlbumTracklist(context.Context, TracklistRequest) ([]TrackCandidate, error) {
	return nil, ErrNoTracklist
}

func runAndCollectVisits(t *testing.T, db *store.DB, mode Mode, now time.Time) []string {
	t.Helper()
	prov := &visitLogger{}
	svc := NewService(db, prov, noArtwork{}, Enablement{Video: true, Music: true}, t.TempDir(), 0)
	svc.SetClock(func() time.Time { return now })
	if _, err := svc.EnrichLibrary(context.Background(), "lib", mode); err != nil {
		t.Fatalf("pass: %v", err)
	}
	out := append([]string(nil), prov.visited...)
	sort.Strings(out)
	return out
}

func openRecheckDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func recheckExec(t *testing.T, db *store.DB) func(string, ...any) {
	t.Helper()
	return func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
}

func sameStrings(a, b []string) bool {
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

// A transient failure recorded DURING a recheck still schedules its retry rather
// than parking — the mode widens the population, it does not change what happens
// to an item once it is visited.
func TestARecheckedLeafThatFailsTransientlyIsStillRetried(t *testing.T) {
	prov := &retryFakeProvider{answers: []error{
		statusError("tmdb", "/movie/27205", http.StatusServiceUnavailable),
	}}
	svc, db, _ := newRetryFixture(t, prov)
	// The state the operator's 722 rows were in: settled, with a diagnosis, and
	// invisible to every pass a scan can fire.
	if err := db.SetTitleEnrichmentStatus("m1", "unmatched", store.EnrichmentReasonNone); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeRecheck)
	if err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if res.Retrying != 1 || res.Failed != 0 {
		t.Fatalf("recheck reported retrying=%d failed=%d, want 1/0 — a 503 during a recheck is "+
			"still an outage, not an answer about the item", res.Retrying, res.Failed)
	}
	got, err := db.TitleForEnrichmentByID("m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EnrichmentRetryAt == "" {
		t.Fatal("no retry scheduled — a recheck must not park what an outage interrupted")
	}
}
