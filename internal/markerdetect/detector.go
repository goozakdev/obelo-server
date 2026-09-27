// Package markerdetect is Marker detection (CONTEXT.md, ADR-0065 §4): the
// Server's own work of finding the Intros and Credits of a Show by comparing the
// sound of its episodes with each other, written as Detected Markers.
//
// It is a background job and the lowest-priority work the Server does. It runs
// after a scan of a TV Library completes, or when an Admin asks for one Show,
// one Season at a time. It is not a Transcode and never takes a slot of the
// transcode cap (ADR-0009), but it never starts while a Transcode is running and
// gives way at its next safe point when one starts: listening to a whole Season
// can wait, a viewer pressing play cannot.
//
// The technique: each File's opening and closing stretch is decoded by ffmpeg to
// low-rate mono PCM and fingerprinted (fingerprint.go); every pair of nearby
// episodes is compared at every relative offset for the longest run of
// changing sound they share (match.go). The span near the start that most of a
// File's comparisons agree on is its Intro, near the end its Credits. No cgo and no audio library: ffmpeg decodes, Go does the rest.
package markerdetect

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Store is what detection reads and writes. *store.DB satisfies it.
type Store interface {
	MarkerDetectionEnabled(libraryID string) (bool, error)
	DetectionSeasonsOfLibrary(libraryID string) ([]store.DetectionSeason, error)
	DetectionSeasonsOfShow(showID string) ([]store.DetectionSeason, error)
	SaveDetectedMarkers(path string, ms []store.Marker) error
	RecordDecode(path string, decoded bool) error
}

// maxDecodeFailures is how many runs in a row may fail to decode an unchanged
// File before post-scan runs stop listening to it. A File that cannot be
// decoded would otherwise have its whole Season decoded again after every scan,
// for ever; it is tried again once it changes, or when an Admin asks.
const maxDecodeFailures = 3

// Options tune a Detector. The zero value is production.
type Options struct {
	Params Params
	// Busy reports whether any Transcode is running. Detection only ever READS
	// this — it holds no slot and increments nothing. Nil means never busy.
	Busy func() bool
	// YieldPoll is how often detection that has given way looks again; default
	// two seconds.
	YieldPoll time.Duration
	// Partners is how many following episodes each episode is compared with,
	// those whose audio matches its own first (partnersOf); default 3. Every pair is compared once, so a Season of n costs about
	// n*Partners comparisons instead of n².
	Partners int
}

// job is one queued piece of work: a Library after its scan, or one Show an
// Admin asked for. A Show job is forced: it listens again even to Files heard
// before.
type job struct {
	libraryID string
	showID    string
}

// Detector owns the queue and the one worker that drains it. Only one Season is
// ever being listened to, by construction: there is one worker.
type Detector struct {
	store    Store
	analyzer Analyzer
	opts     Options

	mu    sync.Mutex
	queue []job
	wake  chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a Detector; Start runs it.
func New(st Store, an Analyzer, opts Options) *Detector {
	if opts.Params == (Params{}) {
		opts.Params = DefaultParams
	}
	if opts.YieldPoll <= 0 {
		opts.YieldPoll = 2 * time.Second
	}
	if opts.Partners <= 0 {
		opts.Partners = 3
	}
	return &Detector{store: st, analyzer: an, opts: opts, wake: make(chan struct{}, 1)}
}

// Start runs the worker until Close.
func (d *Detector) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	go d.run(ctx)
}

// Close stops the worker and waits for it, killing any decoder in flight.
func (d *Detector) Close() {
	if d.cancel == nil {
		return
	}
	d.cancel()
	<-d.done
	d.cancel = nil
}

// AfterScan queues the Library whose scan just completed. It never blocks, and a
// Library already waiting is not queued twice. Whether detection is on for the
// Library is read when its turn comes, so a toggle in between is honoured.
func (d *Detector) AfterScan(libraryID string) {
	d.enqueue(job{libraryID: libraryID}, false)
}

// DetectShow queues one Show AHEAD of everything waiting — an Admin asked for it
// now — though never ahead of the Season already being listened to. An unknown
// Show is store.ErrNotFound.
func (d *Detector) DetectShow(showID string) error {
	if _, err := d.store.DetectionSeasonsOfShow(showID); err != nil {
		return err
	}
	d.enqueue(job{showID: showID}, true)
	return nil
}

func (d *Detector) enqueue(j job, front bool) {
	d.mu.Lock()
	for i, q := range d.queue {
		if q == j {
			if !front {
				d.mu.Unlock()
				return
			}
			d.queue = append(d.queue[:i], d.queue[i+1:]...)
			break
		}
	}
	if front {
		d.queue = append([]job{j}, d.queue...)
	} else {
		d.queue = append(d.queue, j)
	}
	d.mu.Unlock()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Detector) next(ctx context.Context) (job, bool) {
	for {
		d.mu.Lock()
		if len(d.queue) > 0 {
			j := d.queue[0]
			d.queue = d.queue[1:]
			d.mu.Unlock()
			return j, true
		}
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return job{}, false
		case <-d.wake:
		}
	}
}

// lowerWorkerPriority is what the lowered goroutine calls first; a test
// observes it.
var lowerWorkerPriority = lowerThreadPriority

// compareEnds is what each comparison of two episodes' ends calls; a test
// observes it.
var compareEnds = longestShared

// run is the worker. It reads the queue and writes the store at normal
// priority: a write takes the store's locks, and a thread at the lowest
// priority must not sit holding them. Only the Go half of detection — the
// listening, which fingerprints, and the comparisons — is handed to a goroutine
// that lowers its own priority first; it ends with run, and its thread with it.
func (d *Detector) run(ctx context.Context) {
	defer close(d.done)
	work := make(chan func())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		lowerWorkerPriority()
		for f := range work {
			f()
		}
	}()
	defer func() {
		close(work)
		<-ended
	}()
	lowered := func(f func()) {
		done := make(chan struct{})
		work <- func() {
			defer close(done)
			f()
		}
		<-done
	}
	for {
		j, ok := d.next(ctx)
		if !ok {
			return
		}
		seasons, err := d.seasonsOf(j)
		if err != nil {
			log.Printf("obelo: marker detection: %v", err)
			continue
		}
		for _, s := range seasons {
			if err := d.detectSeason(ctx, s, j.showID != "", lowered); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("obelo: marker detection of season %s: %v", s.ID, err)
			}
		}
	}
}

// seasonsOf resolves a job to its Seasons. A Library job for a Library whose
// detection is off, or which has none (music, movies), resolves to nothing.
func (d *Detector) seasonsOf(j job) ([]store.DetectionSeason, error) {
	if j.showID != "" {
		return d.store.DetectionSeasonsOfShow(j.showID)
	}
	on, err := d.store.MarkerDetectionEnabled(j.libraryID)
	if errors.Is(err, store.ErrNoMarkerDetection) || errors.Is(err, store.ErrNotFound) || err == nil && !on {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d.store.DetectionSeasonsOfLibrary(j.libraryID)
}

// yield is the safe point: while any Transcode is running, detection waits.
func (d *Detector) yield(ctx context.Context) error {
	if d.opts.Busy == nil {
		return ctx.Err()
	}
	for d.opts.Busy() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.opts.YieldPoll):
		}
	}
	return ctx.Err()
}

// heard is what listening to one File produced: its opening and closing prints.
type heard struct {
	file       store.DetectionFile
	head, tail Print
	intro      []shared
	credits    []shared
	// failed is set when either end could not be decoded. Such a File has not
	// been heard: nothing is saved for it, so the Detected Markers it has are
	// kept and the next run listens to it again — up to maxDecodeFailures runs.
	failed bool
	// partners counts the other episodes it was to be compared with, compared
	// those of them that decoded. A File all of whose partners failed has heard
	// nothing to compare and is not saved either, so it too keeps what it had.
	partners, compared int
}

// detectSeason listens to one Season and saves every File's Detected Markers.
// Unless forced it leaves out Files that failed to decode maxDecodeFailures runs
// in a row, and skips a Season every other File of which was already heard at
// its current mtime. Between every decode and every comparison it passes a safe
// point. Every listen and comparison runs through lowered, at the lowest
// priority; everything else, the store's writes included, runs on the caller.
func (d *Detector) detectSeason(ctx context.Context, s store.DetectionSeason, force bool, lowered func(func())) error {
	var files []*heard
	fresh := force
	titles := map[string]bool{}
	for _, f := range s.Files {
		if f.DurationMs <= 0 || !force && f.DecodeFailures >= maxDecodeFailures {
			continue
		}
		files = append(files, &heard{file: f})
		titles[f.TitleID] = true
		fresh = fresh || !f.Analyzed
	}
	if !fresh || len(titles) < 2 {
		return nil
	}
	p := d.opts.Params
	// A File that cannot be decoded (no audio track, a broken file, a read
	// error) is left unheard and compared with nothing; the first failure is
	// logged once per Season.
	var failed error
	listen := func(h *heard, startMs, lengthMs int64) Print {
		pr, err := d.analyzer.Analyze(ctx, h.file.Path, startMs, lengthMs)
		if err != nil {
			h.failed = true
			if failed == nil && ctx.Err() == nil {
				failed = err
			}
		}
		return pr
	}
	for _, h := range files {
		w := windowMs(h.file.DurationMs, p)
		if err := d.yield(ctx); err != nil {
			return err
		}
		lowered(func() { h.head = listen(h, 0, w) })
		if err := d.yield(ctx); err != nil {
			return err
		}
		lowered(func() { h.tail = listen(h, h.file.DurationMs-w, w) })
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := d.store.RecordDecode(h.file.Path, !h.failed); err != nil {
			return err
		}
	}
	if failed != nil {
		log.Printf("obelo: marker detection of season %s: %v", s.ID, failed)
	}
	for i, a := range files {
		for _, b := range partnersOf(files, i, d.opts.Partners) {
			a.partners++
			b.partners++
			if a.failed || b.failed {
				continue
			}
			if err := d.yield(ctx); err != nil {
				return err
			}
			a.compared++
			b.compared++
			var m shared
			var ok bool
			lowered(func() { m, ok = compareEnds(a.head, b.head, p) })
			if ok {
				a.intro = append(a.intro, m)
				b.intro = append(b.intro, m.swap())
			}
			if err := d.yield(ctx); err != nil {
				return err
			}
			lowered(func() { m, ok = compareEnds(a.tail, b.tail, p) })
			if ok {
				a.credits = append(a.credits, m)
				b.credits = append(b.credits, m.swap())
			}
		}
	}
	for _, h := range files {
		if h.failed || h.partners > 0 && h.compared == 0 {
			continue
		}
		var ms []store.Marker
		if m, ok := pick(h.intro, p.MinSpanMs, p.MaxIntroMs, p.AgreeMs); ok {
			ms = append(ms, toMarker(markers.KindIntro, m, h.file.DurationMs))
		}
		if m, ok := pick(h.credits, p.MinSpanMs, 0, p.AgreeMs); ok {
			ms = append(ms, toMarker(markers.KindCredits, m, h.file.DurationMs))
		}
		if err := d.store.SaveDetectedMarkers(h.file.Path, ms); err != nil {
			return err
		}
	}
	return nil
}

// partnersOf chooses up to n of the episodes after files[i] to compare it with:
// the nearest ones whose audio has as many channels as its own first, then the
// nearest of the rest to make up the number. A Season can mix sources — a 5.1
// release beside a stereo broadcast — and the same theme from two sources need
// not play at quite the same speed, so it matches best within one source. Two
// Files of the same Episode are never partners.
func partnersOf(files []*heard, i, n int) []*heard {
	a := files[i]
	var same, other []*heard
	for _, b := range files[i+1:] {
		if len(same) == n {
			break
		}
		switch {
		case b.file.TitleID == a.file.TitleID:
		case b.file.AudioChannels == a.file.AudioChannels:
			same = append(same, b)
		case len(other) < n:
			other = append(other, b)
		}
	}
	chosen := make([]*heard, 0, n)
	chosen = append(chosen, same...)
	for _, b := range other {
		if len(chosen) == n {
			break
		}
		chosen = append(chosen, b)
	}
	return chosen
}

// windowMs is how much of each end of a File is listened to.
func windowMs(durationMs int64, p Params) int64 {
	w := int64(float64(durationMs) * p.WindowFraction)
	if p.MaxWindowMs > 0 && w > p.MaxWindowMs {
		w = p.MaxWindowMs
	}
	return w
}

func (s shared) swap() shared { return shared{A0: s.B0, A1: s.B1, B0: s.A0, B1: s.A1} }

// pick chooses a File's span from what each episode it was compared with shared
// with it: among the candidates at least minMs long and, when maxMs > 0, at most
// maxMs, the one the most other candidates agree with (start and end within
// agreeMs), the longest among those. A recap only one other episode repeats can
// be longer than the Intro, but the Intro is what every episode shares.
func pick(cands []shared, minMs, maxMs, agreeMs int64) (shared, bool) {
	var best shared
	bestVotes := 0
	for _, c := range cands {
		n := c.lengthMs()
		if n < minMs || maxMs > 0 && n > maxMs {
			continue
		}
		votes := 0
		for _, o := range cands {
			if abs(o.A0-c.A0) <= agreeMs && abs(o.A1-c.A1) <= agreeMs {
				votes++
			}
		}
		if votes > bestVotes || votes == bestVotes && n > best.lengthMs() {
			best, bestVotes = c, votes
		}
	}
	return best, bestVotes > 0
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func toMarker(kind string, s shared, durationMs int64) store.Marker {
	return store.Marker{
		Kind:    kind,
		Source:  markers.SourceDetected,
		StartMs: max(s.A0, 0),
		EndMs:   min(s.A1, durationMs),
	}
}
