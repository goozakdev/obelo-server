// Package seekpreview is Seek preview generation (CONTEXT.md, ADR-0070): the
// Server's own work of cutting each video File of a Movie or TV Library into a
// still every ten seconds and packing the stills into JPEG tile sheets.
//
// It is a background job scheduled like Marker detection (ADR-0065 §4): queued
// after a scan, never started while a Transcode runs, giving way at the next safe
// point (between sheets) when one starts, and holding no slot of the transcode cap
// (ADR-0009). It shares one background lane with Marker detection (bglane), ahead
// of it. A File someone starts playing jumps the queue; otherwise the most
// recently added File goes first.
//
// A set lives under <dir>/<file id>/ as 0.jpg … n.jpg, and the store holds what the
// manifest needs plus the File's mtime and size at generation. A File whose mtime
// or size has since changed is stale: its set is deleted at once and it is queued
// again. A set is kept while its File is Missing and when the Library's toggle is
// off; it goes when its File is purged or an Admin deletes the Library's previews.
// A File that fails is recorded with a one-line reason and not tried again until it
// changes or someone asks (Retry, GenerateTitle, GenerateShow).
package seekpreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/bglane"
	"github.com/goozakdev/obelo-server/internal/store"
)

// The v1 grid (ADR-0070 §1, §4); the manifest carries them, so they can change
// without a client change.
const (
	IntervalMs = 10_000
	FrameWidth = 320
	Columns    = 10
	Rows       = 8

	jpegQuality = 75
)

// ErrNoFrame is what an Extractor answers when the video has no frame at the
// asked time: it ended before. After at least one frame, that is the end of the
// video, not a failure.
var ErrNoFrame = errors.New("no frame")

// ErrToneMapUnavailable is what an Extractor answers for an HDR File when its
// ffmpeg cannot tone-map (no zscale). The File fails with exactly this reason
// (DECISIONS 20) rather than getting a washed-out set.
var ErrToneMapUnavailable = errors.New("ffmpeg lacks zscale; cannot tone-map HDR")

// Store is what generation reads and writes. *store.DB satisfies it.
type Store interface {
	SeekPreviewFile(fileID string) (store.SeekPreviewFile, error)
	SeekPreviewFilesOfLibrary(libraryID string) ([]store.SeekPreviewFile, error)
	SeekPreviewFilesAll() ([]store.SeekPreviewFile, error)
	SeekPreviewFilesOfTitle(titleID string) ([]store.SeekPreviewFile, error)
	SeekPreviewFilesOfShow(showID string) ([]store.SeekPreviewFile, error)
	SeekPreviewRecords() ([]store.SeekPreviewRecord, error)
	SeekPreviewFileIDsOfLibrary(libraryID string) ([]string, error)
	SaveSeekPreview(s store.SeekPreviewSet) error
	DeleteSeekPreviewRecord(fileID string) error
}

// Extractor decodes the frame at or before atMs of the File at path, FrameWidth
// wide, display-aspect-correct and tone-mapped to SDR. FFmpeg is the production
// one; tests substitute their own.
type Extractor interface {
	Frame(ctx context.Context, path string, atMs int64) (image.Image, error)
}

// videoDurationer is an optional Extractor capability: how long the video stream
// itself runs, which can be less than the container's duration (a longer audio
// track sets that).
type videoDurationer interface {
	VideoDurationMs(ctx context.Context, path string) (int64, bool)
}

// Options tune a Generator. The zero value is production.
type Options struct {
	// Busy reports whether any Transcode is running. Generation only READS this.
	// Nil means never busy.
	Busy func() bool
	// YieldPoll is how often generation that has given way looks again; default
	// two seconds.
	YieldPoll time.Duration
	// Lane is the background lane shared with Marker detection; nil means this
	// Generator has it to itself.
	Lane *bglane.Lane
}

// entry is one File waiting. front marks one someone is waiting for (it is
// playing, or an Admin asked): those go first, the latest request first.
type entry struct {
	fileID  string
	addedAt string
	front   bool
	force   bool
	seq     uint64
	// started is the delete count when the worker took the entry; a Library deleted
	// since then must not get a set from it.
	started uint64
}

// Generator owns the queue and the one worker that drains it.
type Generator struct {
	store Store
	ex    Extractor
	dir   string
	opts  Options

	mu      sync.Mutex
	libs    []string
	all     bool
	entries []*entry
	seq     uint64
	wake    chan struct{}
	cur     *running
	// deletes counts DeleteLibrary calls and deletedAt records the count of each
	// Library's latest, so work that began earlier cannot save a set after it.
	deletes   uint64
	deletedAt map[string]uint64

	// sweepMu serializes the janitor with the start of a File's generation, so a
	// sweep never takes the directory of a File being made for an orphan.
	sweepMu   sync.Mutex
	sweepWake chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
	jdone  chan struct{}
}

// running is the File being generated.
type running struct {
	fileID, libraryID string
	cancel            context.CancelFunc
	done              chan struct{}
}

// New builds a Generator writing under dir; Start runs it.
func New(st Store, ex Extractor, dir string, opts Options) *Generator {
	if opts.YieldPoll <= 0 {
		opts.YieldPoll = 2 * time.Second
	}
	if opts.Lane == nil {
		opts.Lane = &bglane.Lane{}
	}
	return &Generator{store: st, ex: ex, dir: dir, opts: opts, wake: make(chan struct{}, 1), sweepWake: make(chan struct{}, 1), deletedAt: map[string]uint64{}}
}

// Dir is where a File's sheets live.
func (g *Generator) Dir(fileID string) string { return filepath.Join(g.dir, fileID) }

// Start runs the worker until Close.
func (g *Generator) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	g.done = make(chan struct{})
	g.jdone = make(chan struct{})
	go g.run(ctx)
	go g.janitor(ctx)
}

// Close stops the worker and waits for it, killing any decoder in flight.
func (g *Generator) Close() {
	if g.cancel == nil {
		return
	}
	g.cancel()
	<-g.done
	<-g.jdone
	g.cancel = nil
}

// Backfill queues, at boot, every eligible File that has no set and no failure
// record (DECISIONS 21). It never blocks.
func (g *Generator) Backfill() {
	g.mu.Lock()
	g.all = true
	g.mu.Unlock()
	g.poke()
	g.pokeSweep()
}

// AfterScan queues the Library whose scan just completed: its stale sets go, and
// its Files without a set are queued, most recently added first. It never blocks,
// and a Library already waiting is not queued twice. The Library's toggle is read
// when its turn comes.
func (g *Generator) AfterScan(libraryID string) {
	g.mu.Lock()
	for _, l := range g.libs {
		if l == libraryID {
			g.mu.Unlock()
			return
		}
	}
	g.libs = append(g.libs, libraryID)
	g.mu.Unlock()
	g.poke()
	g.pokeSweep()
}

// Bump queues the File ahead of everything waiting, because someone just started
// playing it. A File that has a set, is not eligible, or whose Library's toggle is
// off is dropped when its turn comes. It never blocks.
func (g *Generator) Bump(fileID string) {
	g.push(fileID, "", true, false)
}

// Retry queues one File ahead of everything waiting and tries it again whatever
// it did last time, failed or not. An unknown or ineligible File is
// store.ErrNotFound.
func (g *Generator) Retry(fileID string) error {
	if _, err := g.store.SeekPreviewFile(fileID); err != nil {
		return err
	}
	g.push(fileID, "", true, true)
	return nil
}

// GenerateTitle forces generation, ahead of everything waiting, for every File of
// a Movie or Episode: an existing set is made again, a failure retried, and the
// Library's toggle does not matter. An unknown Title is store.ErrNotFound.
func (g *Generator) GenerateTitle(titleID string) error {
	files, err := g.store.SeekPreviewFilesOfTitle(titleID)
	if err != nil {
		return err
	}
	g.forceAll(files)
	return nil
}

// GenerateShow is GenerateTitle for every Episode of a Show. An unknown Show is
// store.ErrNotFound.
func (g *Generator) GenerateShow(showID string) error {
	files, err := g.store.SeekPreviewFilesOfShow(showID)
	if err != nil {
		return err
	}
	g.forceAll(files)
	return nil
}

func (g *Generator) forceAll(files []store.SeekPreviewFile) {
	// Oldest pushed first so the most recently added, pushed last, is first out.
	for i := len(files) - 1; i >= 0; i-- {
		g.push(files[i].FileID, files[i].AddedAt, true, true)
	}
}

// DeleteLibrary deletes every set and record of a Library's Files, Missing ones
// included, whatever its toggle. Everything of it still waiting is dropped, and a
// File of it being generated is stopped first; none can save a set afterwards (a
// forced generation asked for after this returns can).
func (g *Generator) DeleteLibrary(libraryID string) error {
	// The Files that can be waiting: the eligible ones (a queued File is one), and
	// those with a record.
	files, err := g.store.SeekPreviewFilesOfLibrary(libraryID)
	if err != nil {
		return err
	}
	ids, err := g.store.SeekPreviewFileIDsOfLibrary(libraryID)
	if err != nil {
		return err
	}
	of := make(map[string]bool, len(ids)+len(files))
	for _, id := range ids {
		of[id] = true
	}
	for _, f := range files {
		of[f.FileID] = true
	}
	g.mu.Lock()
	g.deletes++
	g.deletedAt[libraryID] = g.deletes
	kept := g.entries[:0]
	for _, e := range g.entries {
		if !of[e.fileID] {
			kept = append(kept, e)
		}
	}
	g.entries = kept
	for i, l := range g.libs {
		if l == libraryID {
			g.libs = append(g.libs[:i], g.libs[i+1:]...)
			break
		}
	}
	cur := g.cur
	g.mu.Unlock()
	if cur != nil && cur.libraryID == libraryID {
		cur.cancel()
		<-cur.done
	}
	return RemoveLibrary(g.store, g.dir, libraryID)
}

// deletedSince reports whether the Library was deleted after the count n.
func (g *Generator) deletedSince(libraryID string, n uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deletedAt[libraryID] > n
}

func (g *Generator) push(fileID, addedAt string, front, force bool) {
	g.mu.Lock()
	g.seq++
	for _, e := range g.entries {
		if e.fileID == fileID {
			e.front = e.front || front
			e.force = e.force || force
			e.seq = g.seq
			g.mu.Unlock()
			g.poke()
			return
		}
	}
	g.entries = append(g.entries, &entry{fileID: fileID, addedAt: addedAt, front: front, force: force, seq: g.seq})
	g.mu.Unlock()
	g.poke()
}

func (g *Generator) pokeSweep() {
	select {
	case g.sweepWake <- struct{}{}:
	default:
	}
}

// janitor deletes the sets of purged and changed Files whenever a scan or boot
// asks. It takes neither the lane nor waits for Transcodes: only generation does.
func (g *Generator) janitor(ctx context.Context) {
	defer close(g.jdone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.sweepWake:
		}
		stale, err := g.sweep()
		if err != nil {
			log.Printf("obelo: seek previews: %v", err)
		}
		for _, id := range stale {
			g.push(id, "", false, false)
		}
	}
}

func (g *Generator) poke() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// pick removes and returns the next entry: requested ones first (the latest
// request first), then the most recently added.
func (g *Generator) pick() *entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	best := -1
	for i, e := range g.entries {
		if best < 0 || before(e, g.entries[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	e := g.entries[best]
	e.started = g.deletes
	g.entries = append(g.entries[:best], g.entries[best+1:]...)
	return e
}

func before(a, b *entry) bool {
	if a.front != b.front {
		return a.front
	}
	if a.front {
		return a.seq > b.seq
	}
	if a.addedAt != b.addedAt {
		return a.addedAt > b.addedAt
	}
	return a.fileID < b.fileID
}

func (g *Generator) hasWork() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.libs) > 0 || g.all || len(g.entries) > 0
}

func (g *Generator) waitForWork(ctx context.Context) bool {
	for !g.hasWork() {
		select {
		case <-ctx.Done():
			return false
		case <-g.wake:
		}
	}
	return true
}

// run is the worker: wait for work, then drain it, holding a claim on the lane
// throughout so a waiting Marker Season cannot take it between two Files (every
// queued preview goes before it), but taking and releasing the lane itself for
// each File so shutdown stays prompt.
func (g *Generator) run(ctx context.Context) {
	defer close(g.done)
	for g.waitForWork(ctx) {
		unclaim := g.opts.Lane.Claim(bglane.PrioritySeekPreview)
		for g.hasWork() && ctx.Err() == nil {
			release, err := g.opts.Lane.Acquire(ctx, bglane.PrioritySeekPreview)
			if err != nil {
				break
			}
			g.step(ctx)
			release()
		}
		unclaim()
		if ctx.Err() != nil {
			return
		}
	}
}

// step does one unit of work with the lane held: expand the queued Libraries,
// then generate the next File, if any.
func (g *Generator) step(ctx context.Context) {
	g.expand()
	if g.yield(ctx) != nil {
		return
	}
	e := g.pick()
	if e == nil {
		return
	}
	g.handle(ctx, e)
}

// expand turns each queued Library into its Files without a set.
func (g *Generator) expand() {
	g.mu.Lock()
	libs, all := g.libs, g.all
	g.libs, g.all = nil, false
	g.mu.Unlock()
	if len(libs) == 0 && !all {
		return
	}
	queue := func(files []store.SeekPreviewFile) {
		for _, f := range files {
			if f.Enabled && (f.Set == nil || f.Set.Mtime != f.Mtime || f.Set.SizeBytes != f.SizeBytes) {
				g.push(f.FileID, f.AddedAt, false, false)
			}
		}
	}
	if all {
		files, err := g.store.SeekPreviewFilesAll()
		if err != nil {
			log.Printf("obelo: seek previews backfill: %v", err)
		}
		queue(files)
	}
	for _, lib := range libs {
		files, err := g.store.SeekPreviewFilesOfLibrary(lib)
		if err != nil {
			log.Printf("obelo: seek previews of library %s: %v", lib, err)
			continue
		}
		queue(files)
	}
}

// sweep deletes the set and record of every File that was purged or changed since
// its set was made, and the directories no record owns (a generation cut short).
// It returns the changed Files, to be queued again. A Missing File is neither:
// its row is still there, unchanged.
func (g *Generator) sweep() (stale []string, err error) {
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	g.mu.Lock()
	making := ""
	if g.cur != nil {
		making = g.cur.fileID
	}
	g.mu.Unlock()
	recs, err := g.store.SeekPreviewRecords()
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{making: true} // a File being made is neither stale nor an orphan
	for _, r := range recs {
		switch {
		case r.FileID == making:
		case !r.FileExists:
			err = errors.Join(err, g.remove(r.FileID))
		case r.FileMtime != r.Mtime || r.FileSizeBytes != r.SizeBytes:
			err = errors.Join(err, g.remove(r.FileID))
			stale = append(stale, r.FileID)
		default:
			owned[r.FileID] = true
		}
	}
	if entries, rerr := os.ReadDir(g.dir); rerr == nil {
		for _, de := range entries {
			if !owned[de.Name()] {
				err = errors.Join(err, os.RemoveAll(filepath.Join(g.dir, de.Name())))
			}
		}
	}
	return stale, err
}

// remove deletes a File's set and record.
func (g *Generator) remove(fileID string) error { return removeSet(g.store, g.dir, fileID) }

// libraryRecords is what deleting a Library's previews needs of the store.
type libraryRecords interface {
	SeekPreviewFileIDsOfLibrary(libraryID string) ([]string, error)
	DeleteSeekPreviewRecord(fileID string) error
}

// RemoveLibrary deletes, under dir, the set and record of every File of the
// Library, Missing ones included. It needs no Generator, so it works on a host
// with no usable ffmpeg, and running it again finishes what a failed run left.
func RemoveLibrary(st libraryRecords, dir, libraryID string) error {
	ids, err := st.SeekPreviewFileIDsOfLibrary(libraryID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := removeSet(st, dir, id); err != nil {
			return err
		}
	}
	return nil
}

func removeSet(st interface{ DeleteSeekPreviewRecord(string) error }, dir, fileID string) error {
	if fileID == "" || strings.ContainsAny(fileID, `/\`) || fileID == "." || fileID == ".." {
		return fmt.Errorf("seekpreview: refusing to delete the set of file id %q", fileID)
	}
	if err := os.RemoveAll(filepath.Join(dir, fileID)); err != nil {
		return err
	}
	return st.DeleteSeekPreviewRecord(fileID)
}

// handle decides whether a queued File needs a set, and makes it.
func (g *Generator) handle(ctx context.Context, e *entry) {
	f, err := g.store.SeekPreviewFile(e.fileID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("obelo: seek previews of file %s: %v", e.fileID, err)
		}
		return
	}
	switch {
	case e.force:
		if err := g.remove(f.FileID); err != nil {
			log.Printf("obelo: seek previews of file %s: %v", f.FileID, err)
			return
		}
	case f.Set != nil && (f.Set.Mtime != f.Mtime || f.Set.SizeBytes != f.SizeBytes):
		if err := g.remove(f.FileID); err != nil {
			log.Printf("obelo: seek previews of file %s: %v", f.FileID, err)
			return
		}
	case f.Set != nil:
		return // ready, or failed and unchanged: not tried again on its own
	case !f.Enabled:
		return
	}
	g.generate(ctx, f, e.started)
}

// yield is the safe point: while any Transcode is running, generation waits.
func (g *Generator) yield(ctx context.Context) error {
	if g.opts.Busy == nil {
		return ctx.Err()
	}
	for g.opts.Busy() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.opts.YieldPoll):
		}
	}
	return ctx.Err()
}

// generate makes the set of one File: sheet by sheet, a safe point between each,
// and the record last, so a set with a record is a whole one.
func (g *Generator) generate(parent context.Context, f store.SeekPreviewFile, started uint64) {
	ctx, cancel := context.WithCancel(parent)
	cur := &running{fileID: f.FileID, libraryID: f.LibraryID, cancel: cancel, done: make(chan struct{})}
	g.sweepMu.Lock()
	g.mu.Lock()
	deleted := g.deletedAt[f.LibraryID] > started
	if !deleted {
		g.cur = cur
	}
	g.mu.Unlock()
	g.sweepMu.Unlock()
	defer func() {
		cancel()
		g.mu.Lock()
		if g.cur == cur {
			g.cur = nil
		}
		g.mu.Unlock()
		close(cur.done)
	}()
	if deleted {
		return // the Library's previews were deleted after this File was picked
	}

	set, err := g.build(ctx, f)
	if err == nil && ctx.Err() == nil && g.changedSince(f) && !g.deletedSince(f.LibraryID, started) {
		// The File changed under us: the set was cut from the old bytes. Discard it
		// and make the File again (it has no record now, so it is queued as new).
		_ = os.RemoveAll(g.Dir(f.FileID))
		g.push(f.FileID, f.AddedAt, false, false)
		return
	}
	if ctx.Err() != nil {
		_ = os.RemoveAll(g.Dir(f.FileID)) // cut short: no record, no half set
		return
	}
	if err != nil {
		_ = os.RemoveAll(g.Dir(f.FileID))
		log.Printf("obelo: seek previews of %s: %v", f.Path, err)
		set = store.SeekPreviewSet{
			FileID: f.FileID, Mtime: f.Mtime, SizeBytes: f.SizeBytes,
			Status: store.SeekPreviewFailed, Reason: oneLine(err),
		}
	}
	if err := g.store.SaveSeekPreview(set); err != nil {
		log.Printf("obelo: seek previews of %s: %v", f.Path, err)
	}
}

// changedSince reports whether the catalog now holds a different mtime or size for
// the File than generation started from (the Scanner updates the row in place).
func (g *Generator) changedSince(f store.SeekPreviewFile) bool {
	now, err := g.store.SeekPreviewFile(f.FileID)
	return err == nil && (now.Mtime != f.Mtime || now.SizeBytes != f.SizeBytes)
}

func (g *Generator) build(ctx context.Context, f store.SeekPreviewFile) (store.SeekPreviewSet, error) {
	// The frames asked for cover the longer of the stored and the stream's own
	// duration, so a stored duration that is too short does not under-cover the
	// File; the end of the video is where it ends (ErrNoFrame in the last interval
	// of the stream's duration when known, else the stored one).
	durationMs, endMs := f.DurationMs, f.DurationMs
	if vd, ok := g.ex.(videoDurationer); ok {
		if d, ok := vd.VideoDurationMs(ctx, f.Path); ok {
			endMs = d
			durationMs = max(durationMs, d)
		}
	}
	frames := int((durationMs + IntervalMs - 1) / IntervalMs)
	if frames < 1 {
		frames = 1
	}
	perSheet := Columns * Rows
	dir := g.Dir(f.FileID)
	if err := os.RemoveAll(dir); err != nil {
		return store.SeekPreviewSet{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.SeekPreviewSet{}, err
	}
	var fw, fh int
	sheets := 0
	for s := 0; s*perSheet < frames; s++ {
		if s > 0 {
			if err := g.yield(ctx); err != nil {
				return store.SeekPreviewSet{}, err
			}
		}
		var sheet *image.RGBA
		for c := 0; c < perSheet && s*perSheet+c < frames; c++ {
			i := s*perSheet + c
			img, err := g.ex.Frame(ctx, f.Path, int64(i)*IntervalMs)
			if errors.Is(err, ErrNoFrame) && i > 0 && int64(i)*IntervalMs >= endMs-IntervalMs {
				// The video ends before the container's duration does: this is its end.
				// An empty frame any earlier than the last interval is a failure.
				frames = i
				break
			}
			if errors.Is(err, ErrToneMapUnavailable) {
				return store.SeekPreviewSet{}, ErrToneMapUnavailable
			}
			if err != nil {
				return store.SeekPreviewSet{}, fmt.Errorf("frame %d: %w", i, err)
			}
			b := img.Bounds()
			if fw == 0 {
				fw, fh = b.Dx(), b.Dy()
				if fw == 0 || fh == 0 {
					return store.SeekPreviewSet{}, errors.New("decoded an empty frame")
				}
			}
			if b.Dx() != fw || b.Dy() != fh {
				return store.SeekPreviewSet{}, fmt.Errorf("frame %d is %dx%d, the first was %dx%d", i, b.Dx(), b.Dy(), fw, fh)
			}
			if sheet == nil {
				sheet = image.NewRGBA(image.Rect(0, 0, Columns*fw, Rows*fh))
				draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
			}
			at := image.Pt((c%Columns)*fw, (c/Columns)*fh)
			draw.Draw(sheet, image.Rectangle{Min: at, Max: at.Add(image.Pt(fw, fh))}, img, b.Min, draw.Src)
		}
		if sheet == nil {
			break // the video ended on a sheet boundary
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return store.SeekPreviewSet{}, err
		}
		if err := writeAtomic(filepath.Join(dir, fmt.Sprintf("%d.jpg", s)), buf.Bytes()); err != nil {
			return store.SeekPreviewSet{}, err
		}
		sheets++
	}
	return store.SeekPreviewSet{
		FileID: f.FileID, Mtime: f.Mtime, SizeBytes: f.SizeBytes, Status: store.SeekPreviewReady,
		IntervalMs: IntervalMs, FrameWidth: fw, FrameHeight: fh, Columns: Columns, Rows: Rows,
		FrameCount: frames, SheetCount: sheets,
	}, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// oneLine is the first line of err, shortened, for the failed list.
func oneLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}
