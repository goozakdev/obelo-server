package markerdetect_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/markerdetect"
	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Scheduling (ADR-0065 §4, ADR-0009): detection is background work that yields
// to Transcodes, never holds a transcode slot, and does one Season at a time.
// These tests drive the Detector with a fake Analyzer that blocks on every call,
// so each safe point is observable.

// fakeStore serves fixed Seasons and records what was saved. A File saved is
// heard: the next listing reports it Analyzed, as the real store does at an
// unchanged mtime. It counts consecutive decode failures the same way; clearing
// a File's count stands in for its mtime changing.
type fakeStore struct {
	mu        sync.Mutex
	seasons   map[string][]store.DetectionSeason // by show id
	libraries map[string][]store.DetectionSeason // by library id
	enabled   map[string]bool                    // by library id
	saved     map[string][]store.Marker
	heard     map[string]bool
	failures  map[string]int
}

func (f *fakeStore) withHeard(ss []store.DetectionSeason) []store.DetectionSeason {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.DetectionSeason, len(ss))
	for i, s := range ss {
		s.Files = append([]store.DetectionFile(nil), s.Files...)
		for j := range s.Files {
			s.Files[j].Analyzed = s.Files[j].Analyzed || f.heard[s.Files[j].Path]
			s.Files[j].DecodeFailures = f.failures[s.Files[j].Path]
		}
		out[i] = s
	}
	return out
}

func (f *fakeStore) savedCopy(path string) ([]store.Marker, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ms, ok := f.saved[path]
	return ms, ok
}

func (f *fakeStore) MarkerDetectionEnabled(libraryID string) (bool, error) {
	on, ok := f.enabled[libraryID]
	if !ok {
		return false, store.ErrNoMarkerDetection
	}
	return on, nil
}

func (f *fakeStore) DetectionSeasonsOfLibrary(libraryID string) ([]store.DetectionSeason, error) {
	return f.withHeard(f.libraries[libraryID]), nil
}

func (f *fakeStore) DetectionSeasonsOfShow(showID string) ([]store.DetectionSeason, error) {
	s, ok := f.seasons[showID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return f.withHeard(s), nil
}

func (f *fakeStore) SaveDetectedMarkers(path string, ms []store.Marker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved == nil {
		f.saved = map[string][]store.Marker{}
	}
	if f.heard == nil {
		f.heard = map[string]bool{}
	}
	f.saved[path] = ms
	f.heard[path] = true
	return nil
}

func (f *fakeStore) RecordDecode(path string, decoded bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures == nil {
		f.failures = map[string]int{}
	}
	if decoded {
		delete(f.failures, path)
	} else {
		f.failures[path]++
	}
	return nil
}

func (f *fakeStore) failuresOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures[path]
}

func (f *fakeStore) forget(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.saved, path)
	delete(f.failures, path)
}

// gatedAnalyzer announces each call on calls and returns only when released.
type gatedAnalyzer struct {
	calls    chan string
	release  chan struct{}
	inflight atomic.Int32
	maxSeen  atomic.Int32
}

func newGatedAnalyzer() *gatedAnalyzer {
	return &gatedAnalyzer{calls: make(chan string, 100), release: make(chan struct{})}
}

func (g *gatedAnalyzer) Analyze(ctx context.Context, path string, _, _ int64) (markerdetect.Print, error) {
	n := g.inflight.Add(1)
	defer g.inflight.Add(-1)
	for {
		m := g.maxSeen.Load()
		if n <= m || g.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	g.calls <- path
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return markerdetect.Print{}, ctx.Err()
}

// expectCall waits for the next Analyze call.
func (g *gatedAnalyzer) expectCall(t *testing.T) string {
	t.Helper()
	select {
	case p := <-g.calls:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no Analyze call arrived")
		return ""
	}
}

// expectNoCall asserts nothing is listened to for a while.
func (g *gatedAnalyzer) expectNoCall(t *testing.T, why string) {
	t.Helper()
	select {
	case p := <-g.calls:
		t.Fatalf("%s: detection listened to %s", why, p)
	case <-time.After(300 * time.Millisecond):
	}
}

func season(id string, paths ...string) store.DetectionSeason {
	s := store.DetectionSeason{ID: id, ShowID: "show"}
	for i, p := range paths {
		s.Files = append(s.Files, store.DetectionFile{TitleID: id + string(rune('a'+i)), Path: p, DurationMs: 60_000})
	}
	return s
}

func startDetector(t *testing.T, st markerdetect.Store, an markerdetect.Analyzer, busy func() bool) *markerdetect.Detector {
	t.Helper()
	d := markerdetect.New(st, an, markerdetect.Options{Busy: busy, YieldPoll: 5 * time.Millisecond})
	d.Start()
	t.Cleanup(d.Close)
	return d
}

// TestDetectionYieldsWhenATranscodeStartsMidRun: once a Transcode starts while
// a Season is being listened to, detection stops at its next safe point and
// resumes only when the Transcode is gone. A scheduler that treated detection as
// ordinary background work would carry straight on to the next decode.
func TestDetectionYieldsWhenATranscodeStartsMidRun(t *testing.T) {
	var busy atomic.Bool
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{"show": {season("s1", "/e1", "/e2", "/e3")}}}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, busy.Load)
	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}

	if got := an.expectCall(t); got != "/e1" {
		t.Fatalf("first listen = %s, want /e1", got)
	}
	busy.Store(true) // a Transcode starts mid-run
	an.release <- struct{}{}
	an.expectNoCall(t, "a Transcode is running")

	busy.Store(false)
	an.expectCall(t)
}

// TestDetectionDoesNotStartWhileATranscodeRuns: a Season waiting its turn does
// not begin while any Transcode is running.
func TestDetectionDoesNotStartWhileATranscodeRuns(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{"show": {season("s1", "/e1", "/e2")}}}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, busy.Load)
	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	an.expectNoCall(t, "a Transcode was already running")

	busy.Store(false)
	if got := an.expectCall(t); got != "/e1" {
		t.Fatalf("first listen = %s, want /e1", got)
	}
}

// TestDetectionNeverTakesATranscodeSlot: with the cap at one, a transcode can
// still start while detection is mid-run — detection holds no slot and never
// touches the counter — and detection then gives way to it.
func TestDetectionNeverTakesATranscodeSlot(t *testing.T) {
	m := playback.NewManager()
	m.SetTranscodeCap(1)
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{"show": {season("s1", "/e1", "/e2")}}}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, func() bool { return m.ActiveTranscodes() > 0 })
	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	an.expectCall(t)
	if got := m.TranscodeLoad(); got.Active != 0 {
		t.Fatalf("transcode load mid-detection = %+v, want nothing active", got)
	}
	sess, err := m.CreateGoverned(playback.CreateInput{UserID: "u"}, playback.Decision{Tier: playback.TierTranscode})
	if err != nil {
		t.Fatalf("a transcode under cap 1 was refused while detection ran: %v", err)
	}
	an.release <- struct{}{}
	an.expectNoCall(t, "the transcode detection let in is running")
	m.End(sess.ID)
	an.expectCall(t)
	if got := m.TranscodeLoad(); got.Active != 0 {
		t.Fatalf("transcode load after = %+v, want nothing active", got)
	}
}

// TestDetectionRunsOneSeasonAtATime: every File of the first Season is heard
// before any File of the second, and never two at once.
func TestDetectionRunsOneSeasonAtATime(t *testing.T) {
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{"show": {
		season("s1", "/s1e1", "/s1e2"),
		season("s2", "/s2e1", "/s2e2"),
	}}}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, nil)
	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	var order []string
	for range 8 { // two Files per Season, two listens (opening, closing) per File
		order = append(order, an.expectCall(t))
		an.release <- struct{}{}
	}
	want := []string{"/s1e1", "/s1e1", "/s1e2", "/s1e2", "/s2e1", "/s2e1", "/s2e2", "/s2e2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("listen order = %v, want %v", order, want)
		}
	}
	if n := an.maxSeen.Load(); n != 1 {
		t.Errorf("up to %d listens ran at once, want 1", n)
	}
}

// TestDetectShowJumpsTheQueue: an Admin's "detect markers now" runs before
// Libraries still waiting from their scans, though after the Season already
// being listened to.
func TestDetectShowJumpsTheQueue(t *testing.T) {
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{
			"lib":  {season("s1", "/a1", "/a2")},
			"lib2": {season("s2", "/c1", "/c2")},
		},
		seasons: map[string][]store.DetectionSeason{"other": {season("o1", "/b1", "/b2")}},
		enabled: map[string]bool{"lib": true, "lib2": true},
	}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	if got := an.expectCall(t); got != "/a1" {
		t.Fatalf("first listen = %s, want /a1", got)
	}
	d.AfterScan("lib2")
	if err := d.DetectShow("other"); err != nil {
		t.Fatal(err)
	}
	an.release <- struct{}{}
	for _, want := range []string{"/a1", "/a2", "/a2", "/b1"} {
		if got := an.expectCall(t); got != want {
			t.Fatalf("listen = %s, want %s (the running Season, then the Admin's Show)", got, want)
		}
		an.release <- struct{}{}
	}
}

// TestDetectionSkipsALibraryWithDetectionOff: after a scan of a Library whose
// toggle is off — or which has none — nothing is listened to.
func TestDetectionSkipsALibraryWithDetectionOff(t *testing.T) {
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{
			"off":   {season("s1", "/e1", "/e2")},
			"music": {season("s2", "/m1", "/m2")},
		},
		enabled: map[string]bool{"off": false},
	}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, nil)
	d.AfterScan("off")
	d.AfterScan("music") // no toggle at all
	an.expectNoCall(t, "detection is off or absent")
}

// TestDetectShowUnknown: "detect markers now" on a Show that does not exist is
// refused rather than queued.
func TestDetectShowUnknown(t *testing.T) {
	st := &fakeStore{seasons: map[string][]store.DetectionSeason{}}
	d := startDetector(t, st, newGatedAnalyzer(), nil)
	if err := d.DetectShow("nope"); err != store.ErrNotFound {
		t.Fatalf("DetectShow(unknown) = %v, want ErrNotFound", err)
	}
}

// failingAnalyzer fails every listen for a path fail says to, and hears nothing
// (without error) for the rest, announcing every call.
type failingAnalyzer struct {
	calls chan string
	fail  func(path string) error
}

func (f *failingAnalyzer) Analyze(ctx context.Context, path string, startMs, lengthMs int64) (markerdetect.Print, error) {
	f.calls <- path
	if err := f.fail(path); err != nil {
		return markerdetect.Print{}, err
	}
	return markerdetect.Print{}, nil
}

// waitSaved waits until every path has been saved.
func waitSaved(t *testing.T, st *fakeStore, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range paths {
		for {
			if _, ok := st.savedCopy(p); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s was never saved", p)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestDetectionWithoutADecoderMarksNothing: with no ffmpeg to decode, nothing is
// heard — no File is recorded as listened to, and Detected Markers it already
// had are kept — so the Season is listened to again once there is one.
func TestDetectionWithoutADecoderMarksNothing(t *testing.T) {
	kept := []store.Marker{{Kind: "intro", Source: "detected", StartMs: 1000, EndMs: 21000}}
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{
			"lib":   {season("s1", "/e1", "/e2")},
			"after": {season("s2", "/ok/1", "/ok/2")},
		},
		enabled: map[string]bool{"lib": true, "after": true},
		saved:   map[string][]store.Marker{"/e1": kept},
	}
	missing := markerdetect.FFmpeg{Binary: "/nonexistent/ffmpeg"}
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(path string) error {
		if strings.HasPrefix(path, "/ok/") {
			return nil
		}
		_, err := missing.Analyze(context.Background(), path, 0, 1000)
		return err
	}}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	d.AfterScan("after") // one worker, in order: once this is saved, lib is done
	waitSaved(t, st, "/ok/1", "/ok/2")

	if ms, _ := st.savedCopy("/e1"); len(ms) != 1 || ms[0] != kept[0] {
		t.Errorf("/e1 detected markers = %+v, want the ones it had kept", ms)
	}
	if _, heard := st.savedCopy("/e2"); heard {
		t.Error("/e2 was recorded as heard though nothing could decode it")
	}
	for _, s := range st.withHeard(st.libraries["lib"]) {
		for _, f := range s.Files {
			if f.Analyzed {
				t.Errorf("%s is marked heard though nothing could decode it", f.Path)
			}
		}
	}
}

// TestDetectionRetriesAFileThatFailedToDecode: one File of a Season failing to
// decode once leaves it unheard, and the next post-scan run listens to it again
// and records it; the Files that decoded are recorded the first time.
func TestDetectionRetriesAFileThatFailedToDecode(t *testing.T) {
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {season("s1", "/e1", "/e2", "/e3")}},
		enabled:   map[string]bool{"lib": true},
	}
	var failures atomic.Int32
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(path string) error {
		if path == "/e2" && failures.Add(1) == 1 {
			return errors.New("transient read error")
		}
		return nil
	}}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	waitSaved(t, st, "/e1", "/e3")
	if _, heard := st.savedCopy("/e2"); heard {
		t.Fatal("/e2 was recorded as heard though it failed to decode")
	}
	for len(an.calls) > 0 {
		<-an.calls
	}

	d.AfterScan("lib")
	waitSaved(t, st, "/e2")
	var sawE2 bool
	for len(an.calls) > 0 {
		if <-an.calls == "/e2" {
			sawE2 = true
		}
	}
	if !sawE2 {
		t.Error("the next run recorded /e2 without listening to it again")
	}
}

// TestDetectionSavesNothingForAFileWhosePartnersAllFailed: a File that decoded
// but was compared with no episode that did has heard nothing to compare — it
// is not saved, so the Detected Markers it had are kept and it is not heard.
func TestDetectionSavesNothingForAFileWhosePartnersAllFailed(t *testing.T) {
	kept := []store.Marker{{Kind: "intro", Source: "detected", StartMs: 1000, EndMs: 21000}}
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{
			"lib":   {season("s1", "/e1", "/e2", "/e3")},
			"after": {season("s2", "/ok/1", "/ok/2")},
		},
		enabled: map[string]bool{"lib": true, "after": true},
		saved:   map[string][]store.Marker{"/e1": kept},
	}
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(path string) error {
		if path == "/e2" || path == "/e3" {
			return errors.New("no audio track")
		}
		return nil
	}}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	d.AfterScan("after") // one worker, in order: once this is saved, lib is done
	waitSaved(t, st, "/ok/1", "/ok/2")

	if ms, _ := st.savedCopy("/e1"); len(ms) != 1 || ms[0] != kept[0] {
		t.Errorf("/e1 detected markers = %+v, want the ones it had kept", ms)
	}
	for _, s := range st.withHeard(st.libraries["lib"]) {
		for _, f := range s.Files {
			if f.Analyzed {
				t.Errorf("%s is marked heard though it was compared with nothing", f.Path)
			}
		}
	}
}

// TestDetectionGivesUpOnAFileThatKeepsFailing: a File that failed to decode
// three runs in a row, unchanged, is not listened to by post-scan runs any more
// — its Season is not re-decoded after every scan for its sake — until it
// changes; "detect markers now" still tries it.
func TestDetectionGivesUpOnAFileThatKeepsFailing(t *testing.T) {
	s1 := season("s1", "/e1", "/e2", "/e3")
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {s1}},
		seasons: map[string][]store.DetectionSeason{
			"show": {s1},
			"sync": {season("x", "/sync/1", "/sync/2")},
		},
		enabled: map[string]bool{"lib": true},
	}
	an := &failingAnalyzer{calls: make(chan string, 1000), fail: func(path string) error {
		if path == "/e2" {
			return errors.New("broken file")
		}
		return nil
	}}
	d := startDetector(t, st, an, nil)
	// settle waits for everything queued so far: the one worker runs in order.
	settle := func() {
		t.Helper()
		st.forget("/sync/1")
		st.forget("/sync/2")
		if err := d.DetectShow("sync"); err != nil {
			t.Fatal(err)
		}
		waitSaved(t, st, "/sync/1", "/sync/2")
	}
	listened := func(path string) bool {
		saw := false
		for len(an.calls) > 0 {
			if <-an.calls == path {
				saw = true
			}
		}
		return saw
	}
	for run := 1; run <= 3; run++ {
		d.AfterScan("lib")
		settle()
		if !listened("/e2") {
			t.Fatalf("post-scan run %d did not listen to /e2", run)
		}
		if n := st.failuresOf("/e2"); n != run {
			t.Fatalf("/e2 failures after run %d = %d, want %d", run, n, run)
		}
	}

	d.AfterScan("lib")
	settle()
	if listened("/e2") {
		t.Fatal("a post-scan run listened again to a File that failed three times unchanged")
	}

	if err := d.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	settle()
	if !listened("/e2") {
		t.Fatal(`"detect markers now" did not try the File again`)
	}

	st.forget("/e2") // its mtime changed
	d.AfterScan("lib")
	settle()
	if !listened("/e2") {
		t.Fatal("a post-scan run did not listen to the File once it changed")
	}
}

// TestADecodeClearsAFileFailures: a File that failed to decode, then decodes,
// starts again from no failures — a File that fails now and then is never left
// out by failures that did not happen in a row.
func TestADecodeClearsAFileFailures(t *testing.T) {
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {season("s1", "/e1", "/e2")}},
		enabled:   map[string]bool{"lib": true},
		failures:  map[string]int{"/e1": 2},
	}
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(string) error { return nil }}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	waitSaved(t, st, "/e1", "/e2")
	if n := st.failuresOf("/e1"); n != 0 {
		t.Errorf("/e1 failures after it decoded = %d, want 0", n)
	}
}

// TestACancelledRunIsNotADecodeFailure: shutting down while a File's closing
// stretch is being decoded kills the decoder, and the listen fails — but the
// File did not fail to decode, so no failure is counted against it.
func TestACancelledRunIsNotADecodeFailure(t *testing.T) {
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {season("s1", "/e1", "/e2")}},
		enabled:   map[string]bool{"lib": true},
	}
	an := newGatedAnalyzer()
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	if p := an.expectCall(t); p != "/e1" {
		t.Fatalf("first listen = %s, want /e1", p)
	}
	an.release <- struct{}{} // the opening stretch decodes
	if p := an.expectCall(t); p != "/e1" {
		t.Fatalf("second listen = %s, want /e1's closing stretch", p)
	}
	d.Close() // mid-decode of the closing stretch
	if n := st.failuresOf("/e1"); n != 0 {
		t.Errorf("/e1 failures after a cancelled run = %d, want 0", n)
	}
}

// TestTheWorkerLowersItsOwnPriorityFirst: the Go half of detection — the
// fingerprinting and every comparison — runs on the worker, so the worker lowers
// its own priority before it listens to anything, as it does each decoder's.
func TestTheWorkerLowersItsOwnPriorityFirst(t *testing.T) {
	var lowered atomic.Bool
	t.Cleanup(markerdetect.SetLowerWorkerPriority(func() { lowered.Store(true) }))
	st := &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {season("s1", "/e1", "/e2")}},
		enabled:   map[string]bool{"lib": true},
	}
	var loweredFirst atomic.Bool
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(string) error {
		loweredFirst.Store(lowered.Load())
		return nil
	}}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	waitSaved(t, st, "/e1", "/e2")
	if !loweredFirst.Load() {
		t.Error("the worker listened before lowering its priority")
	}
}

// goroutineID is the calling goroutine's number, from its stack header.
func goroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	return strings.Fields(string(buf))[1]
}

// writeWatchingStore is a fakeStore that notes which goroutine made each write.
type writeWatchingStore struct {
	*fakeStore
	mu      sync.Mutex
	writers []string
}

func (w *writeWatchingStore) note() {
	w.mu.Lock()
	w.writers = append(w.writers, goroutineID())
	w.mu.Unlock()
}

func (w *writeWatchingStore) SaveDetectedMarkers(path string, ms []store.Marker) error {
	w.note()
	return w.fakeStore.SaveDetectedMarkers(path, ms)
}

func (w *writeWatchingStore) RecordDecode(path string, decoded bool) error {
	w.note()
	return w.fakeStore.RecordDecode(path, decoded)
}

// TestStoreWritesAreNotMadeAtTheLowestPriority: the listening and the comparing,
// and only they, run on the thread detection lowered to the lowest priority. The
// store's writes take its locks, and a thread every other thread outranks must
// not sit holding them while a viewer's request waits.
func TestStoreWritesAreNotMadeAtTheLowestPriority(t *testing.T) {
	var niced atomic.Value
	t.Cleanup(markerdetect.SetLowerWorkerPriority(func() { niced.Store(goroutineID()) }))
	st := &writeWatchingStore{fakeStore: &fakeStore{
		libraries: map[string][]store.DetectionSeason{"lib": {season("s1", "/e1", "/e2")}},
		enabled:   map[string]bool{"lib": true},
	}}
	var listeners, comparers sync.Map
	t.Cleanup(markerdetect.SetCompareEnds(func() { comparers.Store(goroutineID(), true) }))
	an := &failingAnalyzer{calls: make(chan string, 100), fail: func(string) error {
		listeners.Store(goroutineID(), true)
		return nil
	}}
	d := startDetector(t, st, an, nil)
	d.AfterScan("lib")
	waitSaved(t, st.fakeStore, "/e1", "/e2")
	lowered, _ := niced.Load().(string)
	if lowered == "" {
		t.Fatal("nothing lowered its priority")
	}
	listeners.Range(func(g, _ any) bool {
		if g.(string) != lowered {
			t.Errorf("listened on goroutine %s, want the lowered one %s", g, lowered)
		}
		return true
	})
	compared := 0
	comparers.Range(func(g, _ any) bool {
		compared++
		if g.(string) != lowered {
			t.Errorf("compared on goroutine %s, want the lowered one %s", g, lowered)
		}
		return true
	})
	if compared == 0 {
		t.Error("no comparison was seen")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.writers) == 0 {
		t.Fatal("no store writes were seen")
	}
	for _, g := range st.writers {
		if g == lowered {
			t.Errorf("a store write was made on the lowered goroutine %s", g)
			break
		}
	}
}
