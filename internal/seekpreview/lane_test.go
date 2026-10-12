package seekpreview_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/bglane"
	"github.com/goozakdev/obelo-server/internal/markerdetect"
	"github.com/goozakdev/obelo-server/internal/seekpreview"
	"github.com/goozakdev/obelo-server/internal/store"
)

// eventLog is what the two jobs did, in order, from both sides.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ev...)
}

// runs collapses consecutive events of the same job to one: "m1" for the Marker
// Season under /s1, "p" for a preview.
func (l *eventLog) runs() []string {
	var out []string
	for _, e := range l.snapshot() {
		k := "p"
		if strings.HasPrefix(e, "m:") {
			k = "m" + string(e[len("m:/s")])
		}
		if len(out) == 0 || out[len(out)-1] != k {
			out = append(out, k)
		}
	}
	return out
}

// markerStore serves one Show of two Seasons of two Episodes each.
type markerStore struct{}

func (markerStore) MarkerDetectionEnabled(string) (bool, error) { return true, nil }
func (markerStore) DetectionSeasonsOfLibrary(string) ([]store.DetectionSeason, error) {
	return nil, nil
}
func (markerStore) DetectionSeasonsOfShow(string) ([]store.DetectionSeason, error) {
	season := func(id, dir string) store.DetectionSeason {
		return store.DetectionSeason{ID: id, ShowID: "show", Files: []store.DetectionFile{
			{TitleID: id + "a", Path: dir + "/a.mkv", DurationMs: 600_000},
			{TitleID: id + "b", Path: dir + "/b.mkv", DurationMs: 600_000},
		}}
	}
	return []store.DetectionSeason{season("s1", "/s1"), season("s2", "/s2")}, nil
}
func (markerStore) SaveDetectedMarkers(string, []store.Marker) error { return nil }
func (markerStore) RecordDecode(string, bool) error                  { return nil }

type blockingAnalyzer struct {
	log     *eventLog
	release chan struct{}
}

func (a blockingAnalyzer) Analyze(ctx context.Context, path string, _, _ int64) (markerdetect.Print, error) {
	a.log.add("m:" + path)
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	return markerdetect.Print{}, nil
}

// TestMarkerDetectionAndGenerationShareOneLane: a REAL Detector and a REAL
// Generator over one lane never run together, and a preview that waits while a
// Marker Season is listened to goes next, ahead of the Season after it.
func TestMarkerDetectionAndGenerationShareOneLane(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	f.addFile(t, "libmov", "n", 10_000, "2024-01-02T00:00:00Z")
	log := &eventLog{}
	f.ex.onHit = func(string, int64) { log.add("p") }
	var lane bglane.Lane
	release := make(chan struct{})
	det := markerdetect.New(markerStore{}, blockingAnalyzer{log: log, release: release}, markerdetect.Options{Lane: &lane})
	det.Start()
	t.Cleanup(det.Close)
	if err := det.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first Season being listened to", func() bool { return len(log.snapshot()) > 0 })

	g := f.generator(seekpreview.Options{Lane: &lane})
	start(t, g)
	g.AfterScan("libmov")
	time.Sleep(150 * time.Millisecond)
	if n := len(f.ex.paths()); n != 0 {
		t.Fatalf("%d frames extracted while a Marker Season held the lane", n)
	}
	close(release)
	waitFor(t, "both sets", func() bool { return f.hasSet(t, "m") && f.hasSet(t, "n") })
	waitFor(t, "the second Season", func() bool {
		for _, e := range log.snapshot() {
			if strings.HasPrefix(e, "m:/s2") {
				return true
			}
		}
		return false
	})
	got := strings.Join(log.runs(), ",")
	if got != "m1,p,m2" {
		t.Fatalf("order of jobs = %s, want m1,p,m2 (one at a time, every waiting preview before the next Season)", got)
	}
}

// TestStaleSetIsDeletedWhileAMarkerSeasonHoldsTheLaneAndATranscodeRuns: deleting a
// changed File's set waits for neither Transcodes nor the lane (DECISIONS 7).
func TestStaleSetIsDeletedWhileAMarkerSeasonHoldsTheLaneAndATranscodeRuns(t *testing.T) {
	f := newFixture(t)
	f.addFile(t, "libmov", "m", 10_000, "2024-01-01T00:00:00Z")
	var lane bglane.Lane
	var busy atomic.Bool
	g := f.generator(seekpreview.Options{Lane: &lane, Busy: busy.Load})
	start(t, g)
	g.AfterScan("libmov")
	waitFor(t, "first set", func() bool { return f.hasSet(t, "m") })

	log := &eventLog{}
	release := make(chan struct{})
	defer close(release)
	det := markerdetect.New(markerStore{}, blockingAnalyzer{log: log, release: release}, markerdetect.Options{Lane: &lane})
	det.Start()
	t.Cleanup(det.Close)
	if err := det.DetectShow("show"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a Marker Season holding the lane", func() bool { return len(log.snapshot()) > 0 })
	busy.Store(true)
	mustExec(t, f.db, `UPDATE files SET size_bytes = 4321 WHERE id = 'm'`)
	g.AfterScan("libmov")
	waitFor(t, "the stale set deleted", func() bool {
		_, ok := f.record(t, "m")
		return !ok && !exists(filepath.Join(f.dir, "m"))
	})
}
