package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A Targeted scan of one movie folder picks up a file ADDED to that folder while
// leaving a sibling movie's folder — outside the scope — completely untouched.
func TestTargetedScanAddsWithinScopeOnly(t *testing.T) {
	root := t.TempDir()
	aFolder := filepath.Join(root, "A Movie (2001)")
	bFolder := filepath.Join(root, "B Movie (2002)")
	writeFile(t, filepath.Join(aFolder, "A Movie (2001).mp4"))
	writeFile(t, filepath.Join(bFolder, "B Movie (2002).mp4"))

	ss := newStatefulStore(root)
	svc := NewService(ss, &countingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial full scan: %v", err)
	}

	// Add a second Edition file to A's folder AND a stray file to B's folder.
	writeFile(t, filepath.Join(aFolder, "A Movie (2001) - 1080p.mp4"))
	writeFile(t, filepath.Join(bFolder, "B Movie (2002) - 1080p.mp4"))

	res, err := svc.TargetedScan(context.Background(), "lib1", TargetedScope{
		Folders: []string{aFolder}, Label: "A Movie",
	})
	if err != nil {
		t.Fatalf("targeted scan: %v", err)
	}
	if res.Added != 1 {
		t.Errorf("Added = %d, want 1 (the new file in A's folder)", res.Added)
	}
	// B's stray file must NOT have been catalogued — it's outside the scope.
	if _, ok := ss.files[filepath.Join(bFolder, "B Movie (2002) - 1080p.mp4")]; ok {
		t.Error("targeted scan of A catalogued a file in B's out-of-scope folder")
	}
	// A's new file IS catalogued.
	if _, ok := ss.files[filepath.Join(aFolder, "A Movie (2001) - 1080p.mp4")]; !ok {
		t.Error("targeted scan did not catalogue the new file in A's folder")
	}
}

// The replace case: a Targeted scan marks a File that vanished from the walked
// folder Missing (soft-delete within scope), but never touches a File in a
// sibling folder that also happens to be absent-but-unwalked.
func TestTargetedScanSoftDeletesWithinScope(t *testing.T) {
	root := t.TempDir()
	aFolder := filepath.Join(root, "A Movie (2001)")
	bFolder := filepath.Join(root, "B Movie (2002)")
	aOld := filepath.Join(aFolder, "A Movie (2001).mp4")
	bFile := filepath.Join(bFolder, "B Movie (2002).mp4")
	writeFile(t, aOld)
	writeFile(t, bFile)

	ss := newStatefulStore(root)
	svc := NewService(ss, &countingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial full scan: %v", err)
	}

	// Replace A's file (delete old, drop a new cut). B is left alone on disk.
	if err := os.Remove(aOld); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(aFolder, "A Movie (2001) - Directors Cut.mp4"))

	res, err := svc.TargetedScan(context.Background(), "lib1", TargetedScope{
		Folders: []string{aFolder}, Label: "A Movie",
	})
	if err != nil {
		t.Fatalf("targeted scan: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("Removed = %d, want 1 (A's old file)", res.Removed)
	}
	if ss.files[aOld].Present {
		t.Error("A's replaced file should be marked Missing (present=false)")
	}
	// B's file was never walked, so it must stay present — a Targeted scan of A has
	// no evidence about B.
	if !ss.files[bFile].Present {
		t.Error("targeted scan of A wrongly marked B's out-of-scope file Missing")
	}
}

// When every folder in the scope is unreachable (an unmounted share), the scan
// errors rather than committing a walk that saw nothing — which would otherwise
// soft-delete the entity's whole catalog (ADR-0031 / ADR-0008 guard).
func TestTargetedScanErrorsWhenScopeUnreachable(t *testing.T) {
	root := t.TempDir()
	aFolder := filepath.Join(root, "A Movie (2001)")
	writeFile(t, filepath.Join(aFolder, "A Movie (2001).mp4"))

	ss := newStatefulStore(root)
	svc := NewService(ss, &countingProber{})
	if _, err := svc.Scan(context.Background(), "lib1"); err != nil {
		t.Fatalf("initial full scan: %v", err)
	}

	_, err := svc.TargetedScan(context.Background(), "lib1", TargetedScope{
		Folders: []string{filepath.Join(root, "does-not-exist")}, Label: "Gone",
	})
	if !errors.Is(err, ErrRootsUnavailable) {
		t.Fatalf("err = %v, want ErrRootsUnavailable", err)
	}
}
