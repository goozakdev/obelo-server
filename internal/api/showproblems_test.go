package api_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Black-box tests for GET /libraries/{id}/show-problems — the read behind the
// Needs-Fixing queue's ONE row per Show (file-matcher/07).
//
// The defect being fixed is not a crash. It is that a Show with five broken files
// produced five rows, each offering to re-pick the SERIES, which was never the part
// that was wrong: one problem, five rows, zero working fixes. One row can only
// replace five if the server can count what the five were — and two of the three
// sources are invisible to the client, because an explicitly unassigned File
// produces neither a Title nor an Unmatched row.
//
// So what these assert is arithmetic, and specifically that the arithmetic CLEARS:
// a row the Admin cannot empty however much work they do is worse than five rows
// that at least name their files.

type showProblemsItem struct {
	ShowID         string   `json:"showId"`
	Title          string   `json:"title"`
	Year           int      `json:"year"`
	Path           string   `json:"path"`
	Unassigned     int      `json:"unassigned"`
	Unidentified   int      `json:"unidentified"`
	UnmatchedPaths []string `json:"unmatchedPaths"`
	Orphaned       int      `json:"orphaned"`
	OrphanedPath   string   `json:"orphanedPath"`
}

type showProblemsResp struct {
	Shows []showProblemsItem `json:"shows"`
}

func getShowProblems(t *testing.T, srv *testharness.Server, token, libID string) showProblemsResp {
	t.Helper()
	var out showProblemsResp
	status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/show-problems", token, &out)
	if status != http.StatusOK {
		t.Fatalf("GET show-problems = %d, want 200; body: %s", status, body)
	}
	return out
}

func showProblemFor(t *testing.T, res showProblemsResp, showID string) (showProblemsItem, bool) {
	t.Helper()
	for _, p := range res.Shows {
		if p.ShowID == showID {
			return p, true
		}
	}
	return showProblemsItem{}, false
}

// applyArrangement PUTs a whole arrangement for one Show, as the matcher screen
// does. `files` is the complete decision set: a File absent from it carries no
// decision and follows its filename.
func applyArrangement(t *testing.T, srv *testharness.Server, token, showID string, files []map[string]any) {
	t.Helper()
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/matcher", token,
		map[string]any{"files": files}, nil)
	if status != http.StatusOK {
		t.Fatalf("PUT matcher = %d, want 200; body: %s", status, body)
	}
}

// copyTree copies a fixture tree into a writable temp dir, so a test that has to
// DELETE a file (the only way to orphan a Placement through the real code path)
// never touches the checked-in fixtures.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatalf("copying fixtures: %v", err)
	}
	return dst
}

// scanCopiedMatcherLibrary scans a writable copy of the matcher fixtures.
func scanCopiedMatcherLibrary(t *testing.T) (*testharness.Server, string, string, string) {
	t.Helper()
	root := copyTree(t, matcherRoot(t))
	srv := testharness.New(t)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, root)
	scanLib(t, srv, token, libID, "")
	return srv, token, libID, root
}

// TestShowProblemsCountsUnmatchedFilesUnderTheShow: the flat Unmatched list is
// paths, and only the Show's folders decide whose they are. The queue folds one
// into the Show that owns it and must be told which, or the same file is both a
// Show row's count and a row of its own — the five-rows-one-problem shape returning
// by the back door.
func TestShowProblemsCountsUnmatchedFilesUnderTheShow(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	p, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID)
	if !ok {
		t.Fatalf("the Show holding an unmatched file is absent from show-problems")
	}
	if p.Unidentified != 1 {
		t.Errorf("unidentified = %d, want 1 (the file nothing could number)", p.Unidentified)
	}
	if len(p.UnmatchedPaths) != 1 || filepath.Base(p.UnmatchedPaths[0]) != "Sorted Show - mystery.mkv" {
		t.Errorf("unmatchedPaths = %v, want the one mystery file — without it the queue lists it twice", p.UnmatchedPaths)
	}
	// "Which file?" is one of the four questions every row answers, and the answer
	// has to be one of the COUNTED files, not merely a file of the Show.
	if filepath.Base(p.Path) != "Sorted Show - mystery.mkv" {
		t.Errorf("representative path = %q, want the unsettled file", p.Path)
	}
	if p.Title != "Sorted Show" {
		t.Errorf("title = %q, want Sorted Show", p.Title)
	}
}

// TestShowProblemsUnassignedKeepsTheShowQueued: unassigned is UNDECIDED, not
// settled (CONTEXT.md). Taking a file off its Slot must keep the Show in the queue
// — the alternative silently forgets a file the Admin meant to come back to, which
// is the whole reason the state exists.
func TestShowProblemsUnassignedKeepsTheShowQueued(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	one, ok := fileNamed(m, "Sorted Show (2018) - S01E01 - One.mkv")
	if !ok {
		t.Fatalf("fixture file missing from the matcher: %+v", m.Files)
	}
	mystery, _ := fileNamed(m, "Sorted Show - mystery.mkv")

	// Settle the unnumbered file, and take a well-filed one off its Slot.
	applyArrangement(t, srv, token, showID, []map[string]any{
		{"path": mystery.Path, "state": "ignored", "placements": []any{}},
		{"path": one.Path, "state": "unassigned", "placements": []any{}},
	})

	p, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID)
	if !ok {
		t.Fatalf("an explicitly unassigned file left the Show out of the queue — undecided is not settled")
	}
	if p.Unassigned != 1 {
		t.Errorf("unassigned = %d, want 1", p.Unassigned)
	}
	if p.Unidentified != 0 {
		t.Errorf("unidentified = %d, want 0 — the unnumbered file was ignored, which settles it", p.Unidentified)
	}
	if p.Path != one.Path {
		t.Errorf("representative path = %q, want the unassigned file %q", p.Path, one.Path)
	}
}

// TestShowProblemsClearedByAssigningOrIgnoring: the row's promise. Assigning or
// ignoring every file empties it — a count the matcher cannot clear would make the
// queue permanently non-zero and therefore meaningless.
func TestShowProblemsClearedByAssigningOrIgnoring(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	if _, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID); !ok {
		t.Fatalf("the Show is not queued to begin with; the test would prove nothing")
	}

	m := getMatcher(t, srv, token, showID, "")
	mystery, ok := fileNamed(m, "Sorted Show - mystery.mkv")
	if !ok {
		t.Fatalf("fixture file missing from the matcher: %+v", m.Files)
	}
	// Assign the one unsettled file onto a free Slot — the matcher's own gesture.
	applyArrangement(t, srv, token, showID, []map[string]any{
		{"path": mystery.Path, "state": "placed",
			"placements": []map[string]any{{"group": 1, "slot": 3}}},
	})

	if p, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID); ok {
		t.Fatalf("assigning every file left the Show queued: %+v", p)
	}

	// And the other settling gesture clears it just as completely.
	applyArrangement(t, srv, token, showID, []map[string]any{
		{"path": mystery.Path, "state": "ignored", "placements": []any{}},
	})
	if p, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID); ok {
		t.Fatalf("ignoring every file left the Show queued: %+v", p)
	}
}

// TestShowProblemsSurfacesOrphanedPlacement: a Placement whose file is gone points
// at nothing. It is broken rather than done, so it is promoted into the queue as a
// problem of its own — exactly as an orphaned folder override already is
// (CONTEXT.md "Orphaned correction") — and never folded in with the undecided
// files, which are a different problem with a different fix.
func TestShowProblemsSurfacesOrphanedPlacement(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID, _ := scanCopiedMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	mystery, ok := fileNamed(m, "Sorted Show - mystery.mkv")
	if !ok {
		t.Fatalf("fixture file missing from the matcher: %+v", m.Files)
	}
	applyArrangement(t, srv, token, showID, []map[string]any{
		{"path": mystery.Path, "state": "placed",
			"placements": []map[string]any{{"group": 1, "slot": 3}}},
	})
	if err := os.Remove(mystery.Path); err != nil {
		t.Fatalf("removing the placed file: %v", err)
	}
	scanLib(t, srv, token, libID, "")

	p, ok := showProblemFor(t, getShowProblems(t, srv, token, libID), showID)
	if !ok {
		t.Fatalf("an orphaned Placement dropped out of the queue entirely")
	}
	if p.Orphaned != 1 || p.OrphanedPath != mystery.Path {
		t.Errorf("orphaned = %d / %q, want 1 / %q", p.Orphaned, p.OrphanedPath, mystery.Path)
	}
	// It is its OWN problem: counting it as an undecided file would offer the wrong
	// fix (place it — there is nothing to place).
	if p.Unassigned != 0 || p.Unidentified != 0 {
		t.Errorf("orphan counted as unsettled work (unassigned %d, unidentified %d)", p.Unassigned, p.Unidentified)
	}
}

// TestShowProblemsMatchesTheMatchersOwnCount: the row and the screen that clears it
// must agree on what is left to do. Two definitions of "unsettled" would show a
// count the matcher cannot empty, and the drift would only appear as an Admin
// working a row that never reaches zero.
func TestShowProblemsMatchesTheMatchersOwnCount(t *testing.T) {
	requireMatcherFixtures(t)
	srv, token, libID := scanMatcherLibrary(t)
	showID := matcherShowID(t, srv, token, libID, "Sorted Show")

	m := getMatcher(t, srv, token, showID, "")
	var unassigned int
	for _, f := range m.Files {
		if f.State == "unassigned" && !f.Orphaned {
			unassigned++
		}
	}
	p, _ := showProblemFor(t, getShowProblems(t, srv, token, libID), showID)
	if got := p.Unassigned + p.Unidentified; got != unassigned {
		t.Errorf("queue counts %d unsettled files, the matcher shows %d", got, unassigned)
	}
}

// TestShowProblemsEmptyForANonTVLibrary: the queue asks every Library the same
// question, so a Movie Library answers "no shows" rather than an error.
func TestShowProblemsEmptyForANonTVLibrary(t *testing.T) {
	requireNamingFixtures(t)
	srv, token, libID := scanNamingLibrary(t)

	res := getShowProblems(t, srv, token, libID)
	if len(res.Shows) != 0 {
		t.Errorf("a Movie library reported show problems: %+v", res.Shows)
	}
}

// TestShowProblemsRequiresAdmin / unknown library: the same posture as every other
// attention surface — Admin-only, and an unknown Library hides its existence.
func TestShowProblemsRequiresAdmin(t *testing.T) {
	requireMatcherFixtures(t)
	srv, _, libID := scanMatcherLibrary(t)

	srv.CreateMember("member", "memberpass123")
	member := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/show-problems", member, nil); status != http.StatusForbidden {
		t.Errorf("member GET show-problems = %d, want 403; body: %s", status, body)
	}
}

func TestShowProblemsUnknownLibraryIsNotFound(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	if status, _ := srv.AuthGET("/api/v1/libraries/nope/show-problems", token, nil); status != http.StatusNotFound {
		t.Errorf("GET show-problems for an unknown library, want 404")
	}
}

// TestReviewShowEpisodesDismissesEveryFlaggedEpisode: "Looks right" on a COLLAPSED
// row stands for the whole set the row counted. Doing it as N browser calls would
// let it half-succeed, leaving a row that says "3 episodes" for reasons the Admin
// cannot see.
func TestReviewShowEpisodesDismissesEveryFlaggedEpisode(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)

	var before needsReviewContextResp
	if status, body := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &before); status != http.StatusOK {
		t.Fatalf("needs-review = %d, want 200; body: %s", status, body)
	}
	var showID string
	var flagged int
	for _, it := range before.Items {
		if it.Kind == "episode" && it.ShowID != "" {
			if showID == "" {
				showID = it.ShowID
			}
			if it.ShowID == showID {
				flagged++
			}
		}
	}
	if showID == "" || flagged == 0 {
		t.Skipf("no flagged Episodes in the fixture tree to dismiss")
	}

	if status, body := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/reviewEpisodes", token, nil, nil); status != http.StatusNoContent {
		t.Fatalf("reviewEpisodes = %d, want 204; body: %s", status, body)
	}

	var after needsReviewContextResp
	if status, _ := srv.AuthGET("/api/v1/libraries/"+libID+"/needs-review", token, &after); status != http.StatusOK {
		t.Fatalf("needs-review after dismiss did not return 200")
	}
	for _, it := range after.Items {
		if it.Kind == "episode" && it.ShowID == showID {
			t.Errorf("episode %q is still flagged after dismissing the Show's row", it.Title)
		}
	}
}

func TestReviewShowEpisodesUnknownShowIsNotFound(t *testing.T) {
	srv := testharness.New(t)
	token := adminToken(t, srv)
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/nope/reviewEpisodes", token, nil, nil); status != http.StatusNotFound {
		t.Errorf("reviewEpisodes for an unknown show, want 404")
	}
}

func TestReviewShowEpisodesRequiresAdmin(t *testing.T) {
	requireTVFixtures(t)
	srv, token, libID := scanTVLibrary(t)
	showID := findShow(t, listShows(t, srv, token, libID), "The Bear")

	srv.CreateMember("member", "memberpass123")
	member := login(t, srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	if status, _ := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/reviewEpisodes", member, nil, nil); status != http.StatusForbidden {
		t.Errorf("member reviewEpisodes, want 403")
	}
}
