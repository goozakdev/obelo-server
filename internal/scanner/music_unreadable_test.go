package scanner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// musicProbeStore captures what a music scan wrote AND the `seen` set it handed
// the soft-delete pass, which is the only place the second half of this bug is
// observable.
type musicProbeStore struct {
	*captureStore
	artistTrees []store.ArtistTree
	seen        map[string]bool
}

func (m *musicProbeStore) UpsertArtistTree(t store.ArtistTree) error {
	m.artistTrees = append(m.artistTrees, t)
	return nil
}

func (m *musicProbeStore) MarkFilesMissing(_ string, seen map[string]bool, _ []string) (int, error) {
	m.seen = seen
	return 0, nil
}

// refusingProber probes every audio file happily except one, which it refuses the
// way the real FFprobe does — with a *ProbeError carrying ffprobe's own verdict.
type refusingProber struct {
	refuse string // basename ffprobe refuses
}

func (p refusingProber) Probe(_ context.Context, path string) (MediaInfo, error) {
	if filepath.Base(path) == p.refuse {
		return MediaInfo{}, &ProbeError{
			Path:   path,
			Detail: "No such file or directory",
			Err:    errors.New("exit status 1"),
		}
	}
	return MediaInfo{
		Container:  "flac",
		DurationMs: 200_000,
		Streams:    []StreamInfo{{Index: 0, Kind: "audio", Codec: "flac", Channels: 2, IsDefault: true}},
		Tags: map[string]string{
			"artist": "Eminem", "album_artist": "Eminem", "album": "Relapse",
			"title": "Beautiful", "track": "17", "date": "2009",
		},
	}, nil
}

// TestMusicScanFilesRefusedProbeAsUnreadable: a Track whose bytes ffprobe refuses
// is Unreadable, not Unmatched (ADR-0047). The music path is where the
// distinction bites hardest — a Track takes its identity from TAGS, which live
// inside the very bytes that could not be read, so "Not recognized as a title"
// invites the Admin to name a file that no naming will ever rescue, above a
// fix-match button that is inert by construction.
//
// The live case this was found on: a filename stored on a Samba server in NFD
// (`De\xcc\x81ja\xcc\x80 vu.flac`). It comes back from readdir, so the scanner
// probes it; every lookup macOS smbfs sends is recomposed to NFC, so ffprobe
// gets ENOENT. Obelo cannot fix that mount, but it must not label a file it
// could not open as one it could not name.
func TestMusicScanFilesRefusedProbeAsUnreadable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Eminem", "Relapse")
	good := filepath.Join(dir, "17 Eminem - Beautiful.flac")
	bad := filepath.Join(dir, "16 Eminem - Déjà vu.flac")
	writeFile(t, good)
	writeFile(t, bad)

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "music",
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	ms := &musicProbeStore{captureStore: cs}
	svc := NewService(ms, refusingProber{refuse: filepath.Base(bad)})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(cs.unmatched) != 1 {
		t.Fatalf("unmatched rows = %d, want 1: %+v", len(cs.unmatched), cs.unmatched)
	}
	row := cs.unmatched[0]
	if row.Path != bad {
		t.Fatalf("unmatched path = %q, want the refused file %q", row.Path, bad)
	}
	if row.Kind != store.UnmatchedUnreadable {
		t.Errorf("kind = %q, want %q — a refused file must not be offered an identity correction",
			row.Kind, store.UnmatchedUnreadable)
	}
	// ffprobe's own verdict is the whole diagnosis, and the reason must carry it
	// rather than the generic prose the unidentified half uses.
	if !strings.Contains(row.Reason, "No such file or directory") {
		t.Errorf("reason = %q, want ffprobe's verdict", row.Reason)
	}
	if strings.Contains(row.Reason, "could not probe audio file") {
		t.Errorf("reason = %q still uses the unidentified wording", row.Reason)
	}

	// The readable sibling is unaffected: one Artist, with the good Track on it.
	if len(ms.artistTrees) != 1 {
		t.Fatalf("artist trees = %d, want 1", len(ms.artistTrees))
	}

	// The refused file must NOT count as seen, or its stale Track row stays
	// present=1 and holds the Album and Artist above it open forever (ADR-0047).
	if ms.seen == nil {
		t.Fatal("soft-delete pass never ran; cannot assert the seen set")
	}
	if ms.seen[bad] {
		t.Errorf("refused file %q was marked seen; it must be left to the soft-delete pass", bad)
	}
	if !ms.seen[good] {
		t.Errorf("readable file %q must be marked seen", good)
	}
}
