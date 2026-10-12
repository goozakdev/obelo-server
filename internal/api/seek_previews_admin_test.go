package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// Seek preview administration (ADR-0070): the toggle with its coverage and failed
// list, retry, delete, and "generate now". Generation runs a stub extractor that
// records which File each frame was asked for, so what an admin action queued is
// observed without a real ffmpeg.

type adminStubExtractor struct {
	mu    sync.Mutex
	paths []string
	err   error
	// hold, when set, parks the first frame asked for until it is closed or the
	// generation is cancelled.
	hold chan struct{}
	held bool
}

func (x *adminStubExtractor) Frame(ctx context.Context, path string, _ int64) (image.Image, error) {
	x.mu.Lock()
	park := x.hold != nil && !x.held
	x.held = x.held || park
	x.paths = append(x.paths, path)
	x.mu.Unlock()
	if park {
		select {
		case <-x.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return nil, x.err
	}
	return image.NewRGBA(image.Rect(0, 0, 320, 180)), nil
}

func (x *adminStubExtractor) seen() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.paths...)
}

type seekPreviewStatusResp struct {
	Enabled   bool `json:"enabled"`
	Available bool `json:"available"`
	Eligible  int  `json:"eligible"`
	Ready     int  `json:"ready"`
	Failed    []struct {
		FileID  string `json:"fileId"`
		TitleID string `json:"titleId"`
		Label   string `json:"label"`
		Path    string `json:"path"`
		Reason  string `json:"reason"`
	} `json:"failed"`
}

// seekAdmin is a server over the three fixture movies, scanned with the Library's
// toggle off so nothing has been generated.
type seekAdmin struct {
	srv   *testharness.Server
	ex    *adminStubExtractor
	token string
	libID string
	list  titlesListResp
}

func newSeekAdmin(t *testing.T, opts ...testharness.Option) *seekAdmin {
	t.Helper()
	requireFixtures(t)
	ex := &adminStubExtractor{}
	srv := testharness.New(t, append([]testharness.Option{
		testharness.WithFFmpegAvailability(true), testharness.WithSeekPreviewExtractor(ex)}, opts...)...)
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	srv.SetSeekPreviewsEnabled(libID, false)
	scanLib(t, srv, token, libID, "")
	return &seekAdmin{srv: srv, ex: ex, token: token, libID: libID, list: listAllTitles(t, srv, token, libID)}
}

func (a *seekAdmin) path(suffix string) string { return "/api/v1/libraries/" + a.libID + suffix }

func (a *seekAdmin) status(t *testing.T) seekPreviewStatusResp {
	t.Helper()
	var st seekPreviewStatusResp
	if code, body := a.srv.AuthGET(a.path("/seek-previews"), a.token, &st); code != http.StatusOK {
		t.Fatalf("GET seek-previews = %d; body: %s", code, body)
	}
	return st
}

func (a *seekAdmin) fileOf(t *testing.T, title string) string {
	t.Helper()
	return fileIDOfTitle(t, a.srv, a.token, findTitle(t, a.list, title))
}

func (a *seekAdmin) expect(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	code, raw := a.srv.JSON(method, path, a.token, body, nil)
	if code != want {
		t.Fatalf("%s %s = %d, want %d; body: %s", method, path, code, want, raw)
	}
	return raw
}

func TestSeekPreviewStatusShapeAndCoverage(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	dune := a.fileOf(t, "Dune")
	blade := a.fileOf(t, "Blade Runner")
	a.srv.SeedSeekPreviews(dune, store.SeekPreviewSet{Status: store.SeekPreviewReady, IntervalMs: 10000, FrameWidth: 320, FrameHeight: 180, Columns: 10, Rows: 8, FrameCount: 3}, [][]byte{{1}})
	a.srv.SeedSeekPreviews(blade, store.SeekPreviewSet{Mtime: "an older mtime", SizeBytes: 1, Status: store.SeekPreviewReady, SheetCount: 1}, [][]byte{{1}})

	st := a.status(t)
	if st.Enabled || !st.Available || st.Eligible != 3 || st.Ready != 1 {
		t.Fatalf("status = %+v, want disabled, available, 3 eligible, 1 ready (the stale set is not counted)", st)
	}
	raw := a.expect(t, http.MethodGet, a.path("/seek-previews"), nil, http.StatusOK)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if string(keys["failed"]) != "[]" {
		t.Fatalf("failed = %s, want [] when nothing failed", keys["failed"])
	}
}

func TestSeekPreviewFailedListContentsAndOrder(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	for title, reason := range map[string]string{"Sample Movie": "no frame", "Blade Runner": "ffmpeg lacks zscale; cannot tone-map HDR"} {
		a.srv.SeedSeekPreviews(a.fileOf(t, title), store.SeekPreviewSet{Status: store.SeekPreviewFailed, Reason: reason}, nil)
	}
	st := a.status(t)
	if len(st.Failed) != 2 {
		t.Fatalf("failed = %+v, want 2", st.Failed)
	}
	f0, f1 := st.Failed[0], st.Failed[1]
	if f0.Label != "Blade Runner (1982)" || f1.Label != "Sample Movie (2000)" {
		t.Fatalf("labels = %q, %q, want Blade Runner (1982) then Sample Movie (2000)", f0.Label, f1.Label)
	}
	if f0.FileID != a.fileOf(t, "Blade Runner") || f0.TitleID != findTitle(t, a.list, "Blade Runner") ||
		f0.Reason != "ffmpeg lacks zscale; cannot tone-map HDR" || filepath.Base(f0.Path) != "Blade Runner (1982).mkv" {
		t.Fatalf("failed[0] = %+v", f0)
	}
}

func TestSeekPreviewTogglePutOnQueuesGenerationAndOffKeepsSets(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	dune := a.fileOf(t, "Dune")
	a.srv.SeedSeekPreviews(dune, store.SeekPreviewSet{Status: store.SeekPreviewReady, SheetCount: 1}, [][]byte{{1}})

	a.expect(t, http.MethodPut, a.path("/seek-previews"), map[string]any{}, http.StatusBadRequest)
	if n := len(a.ex.seen()); n != 0 {
		t.Fatalf("%d frames decoded though nothing was accepted", n)
	}
	var st seekPreviewStatusResp
	if code, body := a.srv.JSON(http.MethodPut, a.path("/seek-previews"), a.token, map[string]any{"enabled": true}, &st); code != http.StatusOK || !st.Enabled {
		t.Fatalf("PUT on = %d %+v; body: %s", code, st, body)
	}
	pollFor(t, "the Library's other Files to be generated", func() bool { return a.status(t).Ready == 3 })
	for _, p := range a.ex.seen() {
		if filepath.Base(p) == "Dune (2021).mp4" {
			t.Fatalf("Dune already had a set but %s was decoded", p)
		}
	}

	if code, body := a.srv.JSON(http.MethodPut, a.path("/seek-previews"), a.token, map[string]any{"enabled": false}, &st); code != http.StatusOK || st.Enabled || st.Ready != 3 {
		t.Fatalf("PUT off = %d %+v, want 200 disabled with the 3 sets kept; body: %s", code, st, body)
	}
}

func TestSeekPreviewDeleteTurnsOffAndRemovesSetsAndRecords(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	a.srv.SetSeekPreviewsEnabled(a.libID, true)
	dune, blade := a.fileOf(t, "Dune"), a.fileOf(t, "Blade Runner")
	a.srv.SeedSeekPreviews(dune, store.SeekPreviewSet{Status: store.SeekPreviewReady, SheetCount: 1}, [][]byte{{1}})
	a.srv.SeedSeekPreviews(blade, store.SeekPreviewSet{Status: store.SeekPreviewFailed, Reason: "boom"}, nil)
	dir := filepath.Join(a.srv.DataDir, "seekpreviews")
	if _, err := os.Stat(filepath.Join(dir, dune, "0.jpg")); err != nil {
		t.Fatal(err)
	}

	var st seekPreviewStatusResp
	if code, body := a.srv.JSON(http.MethodDelete, a.path("/seek-previews"), a.token, nil, &st); code != http.StatusOK {
		t.Fatalf("DELETE = %d; body: %s", code, body)
	}
	if st.Enabled || st.Ready != 0 || len(st.Failed) != 0 || st.Eligible != 3 {
		t.Fatalf("after DELETE = %+v, want disabled, 0 ready, no failures", st)
	}
	if _, err := os.Stat(filepath.Join(dir, dune)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Dune's sheets remain after DELETE: %v", err)
	}
	if got := a.status(t); got.Enabled || got.Ready != 0 || len(got.Failed) != 0 {
		t.Fatalf("GET after DELETE = %+v", got)
	}
	if n := len(a.ex.seen()); n != 0 {
		t.Fatalf("%d frames decoded after the toggle went off", n)
	}
}

// setDirs lists the file ids that have a set directory on disk.
func (a *seekAdmin) setDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(a.srv.DataDir, "seekpreviews"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// deleteWhileHeld queues work for all three movies, with the first frame held,
// deletes the Library's previews and lets the held File go; nothing may come back.
func deleteWhileHeld(t *testing.T, queue func(a *seekAdmin)) *seekAdmin {
	t.Helper()
	hold := make(chan struct{})
	a := newSeekAdmin(t)
	a.ex.hold = hold
	queue(a)
	pollFor(t, "the first File to be held in the decoder", func() bool { return len(a.ex.seen()) > 0 })
	var st seekPreviewStatusResp
	if code, body := a.srv.JSON(http.MethodDelete, a.path("/seek-previews"), a.token, nil, &st); code != http.StatusOK {
		t.Fatalf("DELETE = %d; body: %s", code, body)
	}
	close(hold)
	time.Sleep(500 * time.Millisecond) // anything queued would have been made by now
	if got := a.status(t); got.Enabled || got.Ready != 0 || len(got.Failed) != 0 {
		t.Fatalf("after DELETE = %+v, want disabled, 0 ready, no failures", got)
	}
	if dirs := a.setDirs(t); len(dirs) != 0 {
		t.Fatalf("set directories after DELETE: %v", dirs)
	}
	return a
}

func TestSeekPreviewDeleteIsFinalAfterGenerateNow(t *testing.T) {
	t.Parallel()
	deleteWhileHeld(t, func(a *seekAdmin) {
		for _, title := range []string{"Dune", "Blade Runner", "Sample Movie"} {
			a.expect(t, http.MethodPost, "/api/v1/titles/"+findTitle(t, a.list, title)+"/generate-seek-previews", nil, http.StatusAccepted)
		}
	})
}

func TestSeekPreviewDeleteIsFinalAfterRetry(t *testing.T) {
	t.Parallel()
	deleteWhileHeld(t, func(a *seekAdmin) {
		for _, title := range []string{"Dune", "Blade Runner", "Sample Movie"} {
			a.srv.SeedSeekPreviews(a.fileOf(t, title), store.SeekPreviewSet{Status: store.SeekPreviewFailed, Reason: "boom"}, nil)
		}
		a.expect(t, http.MethodPost, a.path("/seek-previews/retry"), map[string]any{}, http.StatusAccepted)
	})
}

func TestSeekPreviewGenerateNowAfterDeleteStillWorks(t *testing.T) {
	t.Parallel()
	a := deleteWhileHeld(t, func(a *seekAdmin) {
		for _, title := range []string{"Dune", "Blade Runner", "Sample Movie"} {
			a.expect(t, http.MethodPost, "/api/v1/titles/"+findTitle(t, a.list, title)+"/generate-seek-previews", nil, http.StatusAccepted)
		}
	})
	a.expect(t, http.MethodPost, "/api/v1/titles/"+findTitle(t, a.list, "Dune")+"/generate-seek-previews", nil, http.StatusAccepted)
	pollFor(t, "Dune's set after the delete", func() bool { return a.status(t).Ready == 1 })
}

func TestSeekPreviewDeleteTwice(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	a.srv.SeedSeekPreviews(a.fileOf(t, "Dune"), store.SeekPreviewSet{Status: store.SeekPreviewReady, SheetCount: 1}, [][]byte{{1}})
	a.expect(t, http.MethodDelete, a.path("/seek-previews"), nil, http.StatusOK)
	a.expect(t, http.MethodDelete, a.path("/seek-previews"), nil, http.StatusOK)
	if st := a.status(t); st.Enabled || st.Ready != 0 {
		t.Fatalf("after two DELETEs = %+v", st)
	}
}

func TestSeekPreviewRetryOneAndAll(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	for _, title := range []string{"Dune", "Blade Runner"} {
		a.srv.SeedSeekPreviews(a.fileOf(t, title), store.SeekPreviewSet{Status: store.SeekPreviewFailed, Reason: "boom"}, nil)
	}
	a.expect(t, http.MethodPost, a.path("/seek-previews/retry"), map[string]any{"fileId": a.fileOf(t, "Dune")}, http.StatusAccepted)
	pollFor(t, "Dune to be retried", func() bool { return len(a.ex.seen()) > 0 })
	pollFor(t, "Dune to be ready", func() bool { return a.status(t).Ready == 1 })
	for _, p := range a.ex.seen() {
		if filepath.Base(p) != "Dune (2021).mp4" {
			t.Fatalf("retry of Dune decoded %s", p)
		}
	}

	raw := a.expect(t, http.MethodPost, a.path("/seek-previews/retry"), map[string]any{}, http.StatusAccepted)
	if string(raw) != "{}\n" && string(raw) != "{}" {
		t.Fatalf("retry body = %q, want {}", raw)
	}
	pollFor(t, "every failed File to be retried", func() bool { return a.status(t).Ready == 2 })
	if st := a.status(t); len(st.Failed) != 0 {
		t.Fatalf("failed after retrying all = %+v", st.Failed)
	}
	a.expect(t, http.MethodPost, a.path("/seek-previews/retry"), nil, http.StatusAccepted) // no body at all is {}
}

func TestSeekPreviewRetryRefusesAFileOfAnotherLibraryOrUnknown(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	other := createLibraryNamed(t, a.srv, a.token, "Other", t.TempDir())
	for _, id := range []string{a.fileOf(t, "Dune"), "no-such-file"} {
		code, raw := a.srv.JSON(http.MethodPost, "/api/v1/libraries/"+other+"/seek-previews/retry", a.token, map[string]any{"fileId": id}, nil)
		if code != http.StatusNotFound || !json.Valid(raw) {
			t.Fatalf("retry of %q via another Library = %d; body: %s", id, code, raw)
		}
		var env errorEnvelope
		_ = json.Unmarshal(raw, &env)
		if env.Error.Message != "file not found" {
			t.Fatalf("message = %q, want %q", env.Error.Message, "file not found")
		}
	}
	if n := len(a.ex.seen()); n != 0 {
		t.Fatalf("%d frames decoded for a refused retry", n)
	}
}

func TestSeekPreviewGenerateNowForATitleAndAShow(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	a.expect(t, http.MethodPost, "/api/v1/titles/"+findTitle(t, a.list, "Blade Runner")+"/generate-seek-previews", nil, http.StatusAccepted)
	pollFor(t, "Blade Runner's set", func() bool { return a.status(t).Ready == 1 })
	if got := a.ex.seen(); len(got) == 0 || filepath.Base(got[0]) != "Blade Runner (1982).mkv" {
		t.Fatalf("generate-now decoded %v", got)
	}

	a.expect(t, http.MethodPost, "/api/v1/titles/nope/generate-seek-previews", nil, http.StatusNotFound)
	// A Missing File is not eligible: a Title with only that is a 404.
	a.srv.Exec(`UPDATE files SET present = 0 WHERE id = ?`, a.fileOf(t, "Dune"))
	a.expect(t, http.MethodPost, "/api/v1/titles/"+findTitle(t, a.list, "Dune")+"/generate-seek-previews", nil, http.StatusNotFound)
}

func TestSeekPreviewGenerateNowForAShow(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	ex := &adminStubExtractor{}
	srv := testharness.New(t, testharness.WithFFmpegAvailability(true), testharness.WithSeekPreviewExtractor(ex))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	srv.SetSeekPreviewsEnabled(libID, false)
	scanLib(t, srv, token, libID, "")
	showID, eps := bearSeason1Episodes(t, srv, token, libID)
	if code, body := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/generate-seek-previews", token, nil, nil); code != http.StatusAccepted {
		t.Fatalf("generate = %d; body: %s", code, body)
	}
	pollFor(t, "the Show's Files to be decoded", func() bool {
		var st seekPreviewStatusResp
		srv.AuthGET("/api/v1/libraries/"+libID+"/seek-previews", token, &st)
		return st.Ready >= len(eps.Episodes)
	})
	if code, _ := srv.JSON(http.MethodPost, "/api/v1/shows/nope/generate-seek-previews", token, nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown Show = %d, want 404", code)
	}
}

func TestSeekPreviewAdminRoutesRefuseAMember(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	a.srv.CreateMember("member", "memberpass123")
	member := login(t, a.srv, "member", "memberpass123", "Phone", "ios", "member-client").Token
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, a.path("/seek-previews")},
		{http.MethodPut, a.path("/seek-previews")},
		{http.MethodDelete, a.path("/seek-previews")},
		{http.MethodPost, a.path("/seek-previews/retry")},
		{http.MethodPost, "/api/v1/titles/" + findTitle(t, a.list, "Dune") + "/generate-seek-previews"},
		{http.MethodPost, "/api/v1/shows/anything/generate-seek-previews"},
	} {
		if code, body := a.srv.JSON(r.method, r.path, member, map[string]any{"enabled": true}, nil); code != http.StatusForbidden {
			t.Errorf("Member %s %s = %d, want 403; body: %s", r.method, r.path, code, body)
		}
	}
}

func TestSeekPreviewLibraryRoutesWithoutAToggleAreNotFound(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	music := createMusicLibrary(t, a.srv, a.token, t.TempDir())
	for _, id := range []string{music, "no-such-library"} {
		for _, r := range []struct {
			method, suffix string
			body           any
		}{
			{http.MethodGet, "/seek-previews", nil},
			{http.MethodPut, "/seek-previews", map[string]any{"enabled": true}},
			{http.MethodDelete, "/seek-previews", nil},
			{http.MethodPost, "/seek-previews/retry", map[string]any{}},
		} {
			code, raw := a.srv.JSON(r.method, "/api/v1/libraries/"+id+r.suffix, a.token, r.body, nil)
			var env errorEnvelope
			_ = json.Unmarshal(raw, &env)
			if code != http.StatusNotFound || env.Error.Message != "this library has no seek previews" {
				t.Errorf("%s %s%s = %d %q, want 404 \"this library has no seek previews\"", r.method, id, r.suffix, code, env.Error.Message)
			}
		}
	}
}

func TestSeekPreviewRoutesRefuseALinkedMirror(t *testing.T) {
	t.Parallel()
	a := newSeekAdmin(t)
	titleID := findTitle(t, a.list, "Dune")
	a.srv.Exec(`UPDATE libraries SET source = 'linked', remote_library_id = 'remote' WHERE id = ?`, a.libID)
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, a.path("/seek-previews")},
		{http.MethodPut, a.path("/seek-previews")},
		{http.MethodDelete, a.path("/seek-previews")},
		{http.MethodPost, a.path("/seek-previews/retry")},
		{http.MethodPost, "/api/v1/titles/" + titleID + "/generate-seek-previews"},
	} {
		code, raw := a.srv.JSON(r.method, r.path, a.token, map[string]any{"enabled": true}, nil)
		var env errorEnvelope
		_ = json.Unmarshal(raw, &env)
		if code != http.StatusConflict || env.Error.Code != "LINKED_LIBRARY" {
			t.Errorf("%s %s = %d/%s, want 409 LINKED_LIBRARY; body: %s", r.method, r.path, code, env.Error.Code, raw)
		}
	}
}

func TestSeekPreviewShowRouteRefusesAMirroredShow(t *testing.T) {
	t.Parallel()
	requireTVFixtures(t)
	srv := testharness.New(t, testharness.WithFFmpegAvailability(true), testharness.WithSeekPreviewExtractor(&adminStubExtractor{}))
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	srv.SetSeekPreviewsEnabled(libID, false)
	scanLib(t, srv, token, libID, "")
	showID, _ := bearSeason1Episodes(t, srv, token, libID)
	srv.Exec(`UPDATE libraries SET source = 'linked', remote_library_id = 'remote' WHERE id = ?`, libID)
	code, raw := srv.JSON(http.MethodPost, "/api/v1/shows/"+showID+"/generate-seek-previews", token, nil, nil)
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env)
	if code != http.StatusConflict || env.Error.Code != "LINKED_LIBRARY" {
		t.Fatalf("mirrored Show = %d/%s, want 409 LINKED_LIBRARY; body: %s", code, env.Error.Code, raw)
	}
}

func TestSeekPreviewAdminWithoutFFmpeg(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithFFmpegAvailability(false))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	titleID := findTitle(t, listAllTitles(t, srv, token, libID), "Dune")

	var st seekPreviewStatusResp
	if code, body := srv.AuthGET("/api/v1/libraries/"+libID+"/seek-previews", token, &st); code != http.StatusOK || st.Available || !st.Enabled || st.Eligible != 3 {
		t.Fatalf("GET without ffmpeg = %d %+v, want 200 available:false; body: %s", code, st, body)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/libraries/" + libID + "/seek-previews/retry"},
		{http.MethodPost, "/api/v1/titles/" + titleID + "/generate-seek-previews"},
		{http.MethodPost, "/api/v1/shows/anything/generate-seek-previews"},
	} {
		if code, body := srv.JSON(r.method, r.path, token, nil, nil); code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without ffmpeg = %d, want 503; body: %s", r.method, r.path, code, body)
		}
	}
}

// Deleting needs no ffmpeg: the sets and records are plain files and rows.
func TestSeekPreviewDeleteWithoutFFmpeg(t *testing.T) {
	t.Parallel()
	requireFixtures(t)
	srv := testharness.New(t, testharness.WithFFmpegAvailability(false))
	token := adminToken(t, srv)
	libID := createMovieLibrary(t, srv, token, fixtureRoot(t))
	scanLib(t, srv, token, libID, "")
	dune := fileIDOfTitle(t, srv, token, findTitle(t, listAllTitles(t, srv, token, libID), "Dune"))
	srv.SeedSeekPreviews(dune, store.SeekPreviewSet{Status: store.SeekPreviewReady, SheetCount: 1}, [][]byte{{1}})

	for i := 0; i < 2; i++ {
		var st seekPreviewStatusResp
		if code, body := srv.JSON(http.MethodDelete, "/api/v1/libraries/"+libID+"/seek-previews", token, nil, &st); code != http.StatusOK || st.Enabled || st.Available || st.Ready != 0 {
			t.Fatalf("DELETE #%d without ffmpeg = %d %+v, want 200 disabled, 0 ready; body: %s", i+1, code, st, body)
		}
	}
	if _, err := os.Stat(filepath.Join(srv.DataDir, "seekpreviews", dune)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Dune's sheets remain: %v", err)
	}
}
