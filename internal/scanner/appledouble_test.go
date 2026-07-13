package scanner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// recordingProber succeeds like fakeProber but records every path it is asked to
// probe, so a test can assert the walker never even reaches a file it should have
// skipped before ffprobe.
type recordingProber struct {
	probed []string
}

func (p *recordingProber) Probe(_ context.Context, path string) (MediaInfo, error) {
	p.probed = append(p.probed, path)
	return MediaInfo{
		Container: "mp4",
		Streams: []StreamInfo{
			{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, IsDefault: true},
			{Index: 1, Kind: "audio", Codec: "aac", Channels: 2},
		},
	}, nil
}

// TestScanSkipsAppleDoubleSidecar: a macOS AppleDouble sidecar — the `._`-prefixed
// companion file Finder/smbfs leaves next to real media on a network share — must
// be ignored entirely: never probed, never indexed as an Episode, even though its
// extension is on the media allowlist. Regression for the full-scan crash where a
// `._…S04E28-29-30….avi` sidecar (a multi-episode range) was accepted as media,
// expanded per-episode, failed to probe, and had its identical path inserted twice
// into unmatched_files (UNIQUE(path) violation). Uses a succeeding prober so the
// assertion isolates the walker-level skip, independent of the probe-failure path.
func TestScanSkipsAppleDoubleSidecar(t *testing.T) {
	root := t.TempDir()
	season := filepath.Join(root, "Family Guy", "Season 4")
	real := filepath.Join(season, "Family Guy - S04E10 - Model Misbehavior.avi")
	sidecar := filepath.Join(season, "._Family Guy - S04E28-29-30 - Stewie B. Goode.avi")
	writeFile(t, real)
	writeFile(t, sidecar)

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "tv",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	prober := &recordingProber{}
	svc := NewService(cs, prober)
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	probedReal := false
	for _, p := range prober.probed {
		if strings.HasPrefix(filepath.Base(p), "._") {
			t.Errorf("AppleDouble sidecar was probed (should be skipped by the walker): %q", p)
		}
		if p == real {
			probedReal = true
		}
	}
	if !probedReal {
		t.Errorf("real episode next to the sidecar was not probed; the walk did not run as expected")
	}
	for _, u := range cs.unmatched {
		if strings.HasPrefix(filepath.Base(u.Path), "._") {
			t.Errorf("AppleDouble sidecar leaked into unmatched_files: %q", u.Path)
		}
	}
}

// errProber fails every probe, as ffprobe does on a file that is not a valid media
// container (e.g. a 4 KB AppleDouble resource fork, or a truncated download).
type errProber struct{}

func (errProber) Probe(_ context.Context, _ string) (MediaInfo, error) {
	return MediaInfo{}, errors.New("ffprobe: invalid data found when processing input")
}

// uniqueUnmatchedStore wraps captureStore and enforces the real schema's GLOBAL
// UNIQUE(path) on unmatched_files: a duplicate path within one scan's batch fails
// with the same message the sqlite constraint raises in production, so a test can
// reproduce the reported crash without a live database.
type uniqueUnmatchedStore struct {
	*captureStore
}

func (u *uniqueUnmatchedStore) ReplaceUnmatched(libID string, files []store.UnmatchedFile) error {
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Path] {
			return fmt.Errorf("store: inserting unmatched %q: constraint failed: UNIQUE constraint failed: unmatched_files.path", f.Path)
		}
		seen[f.Path] = true
	}
	return u.captureStore.ReplaceUnmatched(libID, files)
}

// TestScanRangeFileProbeFailureSingleUnmatched: a multi-episode range file
// (S04E28-29-30) that fails to probe must produce exactly ONE unmatched row, not
// one per expanded episode. Before the fix, resolveEpisodeFile expanded the range
// and appended the same path once per episode; the duplicate tripped the global
// UNIQUE(path) constraint on unmatched_files and aborted the whole scan. Uses a
// plain (non-AppleDouble) filename so it exercises the per-file dedup guard
// independently of the AppleDouble filter.
func TestScanRangeFileProbeFailureSingleUnmatched(t *testing.T) {
	// Each is a real (non-AppleDouble) range file that fails to probe. The range
	// width varies (2- and 3-episode) and the names carry the punctuation seen in
	// real libraries (apostrophes, dotted acronyms, "+") to confirm the range
	// token is still found and deduped to a single unmatched row.
	cases := []struct {
		show string
		file string
	}{
		{"Family Guy", "Family Guy - S04E28-29-30 - Stewie B. Goode.avi"},
		{"Marvel's Agents of S.H.I.E.L.D", "Marvel's Agents of S.H.I.E.L.D. - S07E12-13 - The End Is at Hand + What We're Fighting For.mkv"},
	}
	for _, tc := range cases {
		t.Run(tc.show, func(t *testing.T) {
			root := t.TempDir()
			season := filepath.Join(root, tc.show, "Season 7")
			ranged := filepath.Join(season, tc.file)
			writeFile(t, ranged)

			cs := &uniqueUnmatchedStore{captureStore: &captureStore{lib: store.Library{
				ID: "lib1", Kind: "tv",
				Roots: []store.LibraryRoot{{Path: root}},
			}}}
			svc := NewService(cs, errProber{})
			if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
				t.Fatalf("scan hit the unmatched UNIQUE constraint on a range file: %v", err)
			}
			if len(cs.unmatched) != 1 {
				t.Fatalf("unmatched rows = %d, want 1 (range file must dedup to a single row)", len(cs.unmatched))
			}
			if cs.unmatched[0].Path != ranged {
				t.Errorf("unmatched path = %q, want %q", cs.unmatched[0].Path, ranged)
			}
		})
	}
}
