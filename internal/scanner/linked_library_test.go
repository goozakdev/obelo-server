package scanner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// The Scanner never sees a linked Library (ADR-0056 §1). It has no root folders
// and its contents belong to another household's disk; the Server that owns the
// files walks them, and its Export is how the answer reaches here.
//
// The API layer refuses a scan of one with 409 LINKED_LIBRARY before it ever gets
// here, so this is the backstop — for the scheduled sweep, the auto-after-scan
// trigger, and whatever calls this next.
func TestEveryScanEntryPointRefusesALinkedLibrary(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Heat (1995)", "Heat (1995).mkv"))

	cs := &captureStore{lib: store.Library{
		ID: "lib1", Kind: "movie", Source: store.LibrarySourceLinked,
		Roots: []store.LibraryRoot{{Path: root}},
	}}
	prober := &recordingProber{}
	svc := NewService(cs, prober)
	ctx := context.Background()

	if _, err := svc.Scan(ctx, "lib1"); !errors.Is(err, ErrLinkedLibrary) {
		t.Errorf("Scan of a mirror = %v, want ErrLinkedLibrary", err)
	}
	if err := svc.StartScan(ctx, "lib1", ModeIncremental, nil, nil); !errors.Is(err, ErrLinkedLibrary) {
		t.Errorf("StartScan of a mirror = %v, want ErrLinkedLibrary", err)
	}
	scope := TargetedScope{Label: "Heat", Folders: []string{root}}
	if _, err := svc.TargetedScan(ctx, "lib1", scope); !errors.Is(err, ErrLinkedLibrary) {
		t.Errorf("TargetedScan of a mirror = %v, want ErrLinkedLibrary", err)
	}
	if err := svc.StartTargetedScan(ctx, "lib1", scope, nil, nil); !errors.Is(err, ErrLinkedLibrary) {
		t.Errorf("StartTargetedScan of a mirror = %v, want ErrLinkedLibrary", err)
	}

	// The refusal happens before anything is touched: no probe, no scan-status row
	// moved, nothing written. A mirror that got as far as being marked "running"
	// would show a scan spinner nobody could ever clear.
	if len(prober.probed) != 0 {
		t.Errorf("a refused scan still probed %d files", len(prober.probed))
	}

	// The control: the same Library, locally sourced, scans.
	cs.lib.Source = store.LibrarySourceLocal
	if _, err := svc.Scan(ctx, "lib1"); err != nil {
		t.Fatalf("scanning a LOCAL library = %v, want success", err)
	}
	if len(prober.probed) == 0 {
		t.Error("the local control scanned nothing; the test proves nothing")
	}
}
