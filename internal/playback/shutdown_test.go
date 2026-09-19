package playback

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goozakdev/obelo-server/internal/store"
	"github.com/goozakdev/obelo-server/internal/transcode"
)

// lingeringRunner launches jobs that behave like a real SIGKILLed ffmpeg: Kill only
// signals, and the process is gone a little later.
type lingeringRunner struct {
	mu   sync.Mutex
	jobs []*lingeringJob
}

func (r *lingeringRunner) Start(_ context.Context, args []string) (transcode.Job, error) {
	j := &lingeringJob{dir: filepath.Dir(args[len(args)-1]), killed: make(chan struct{})}
	r.mu.Lock()
	r.jobs = append(r.jobs, j)
	r.mu.Unlock()
	return j, nil
}

// lingeringJob exits lingerAfterKill after Kill, and records whether its scratch
// dir still existed at that moment — i.e. whether whoever killed it waited for it
// to be gone before deleting the directory it writes into.
type lingeringJob struct {
	dir    string
	once   sync.Once
	killed chan struct{}

	mu            sync.Mutex
	exited        bool
	dirAtExitSeen bool
}

const lingerAfterKill = 50 * time.Millisecond

func (j *lingeringJob) Wait() error {
	<-j.killed
	time.Sleep(lingerAfterKill)
	_, err := os.Stat(j.dir)
	j.mu.Lock()
	j.exited, j.dirAtExitSeen = true, err == nil
	j.mu.Unlock()
	return nil
}

func (j *lingeringJob) Kill() error {
	j.once.Do(func() { close(j.killed) })
	return nil
}

func (j *lingeringJob) state() (exited, dirAtExit bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.exited, j.dirAtExitSeen
}

// EndAll ends every session, idle or not, and each teardown waits for its killed
// ffmpeg to EXIT before removing the scratch dir. Removing it first is the race
// behind the api suite's "TempDir RemoveAll cleanup: directory not empty": a job
// still writing a segment into a directory that is being deleted.
func TestEndAllWaitsForEachKilledJobBeforeRemovingItsScratch(t *testing.T) {
	root := t.TempDir()
	runner := &lingeringRunner{}
	m := NewRemuxManager(runner, root)

	var dirs []string
	for _, id := range []string{"t1", "t2"} {
		dec := Decision{
			Tier:    TierDirectStream,
			Edition: store.Edition{ID: "e-" + id},
			File:    store.File{ID: "f-" + id, Path: "/movies/" + id + ".mkv"},
		}
		s := m.Create(CreateInput{
			UserID: "u1", TitleID: id,
			BuildHLSArgs: func(outputDir string, seek transcode.SeekOffset) []string {
				return transcode.RemuxArgs(transcode.RemuxJob{SourcePath: dec.File.Path, OutputDir: outputDir, Seek: seek})
			},
		}, dec)
		rt, ok := m.remuxRuntimeFor(s.ID)
		if !ok {
			t.Fatalf("no runtime for %s", id)
		}
		if err := rt.EnsureStarted(); err != nil {
			t.Fatalf("EnsureStarted %s: %v", id, err)
		}
		dirs = append(dirs, s.ScratchDir)
	}

	if n := m.EndAll(); n != 2 {
		t.Fatalf("EndAll ended %d sessions, want 2", n)
	}
	for i, j := range runner.jobs {
		exited, dirAtExit := j.state()
		if !exited {
			t.Errorf("job %d had not exited when EndAll returned", i)
		}
		if !dirAtExit {
			t.Errorf("job %d's scratch dir was removed while it was still running", i)
		}
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("scratch %q still present after EndAll (err=%v)", d, err)
		}
	}
	if n := m.EndAll(); n != 0 {
		t.Errorf("a second EndAll ended %d sessions, want 0", n)
	}
}
