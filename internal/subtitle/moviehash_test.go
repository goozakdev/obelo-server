package subtitle

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeFixture writes size bytes produced by fill(i) at byte offset i and returns
// the path. It is the known-answer fixture generator: because the OpenSubtitles
// moviehash is filesize + a running uint64 sum over the first and last 64 KiB, a
// fixture built from a simple, hand-computable byte pattern gives an independent
// expected value (no dependency on the production code under test).
func writeFixture(t *testing.T, size int, fill func(i int) byte) string {
	t.Helper()
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = fill(i)
	}
	path := filepath.Join(t.TempDir(), "fixture.bin")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestMovieHashAllZeros(t *testing.T) {
	// An all-zero file contributes nothing to the checksum, so the hash is exactly
	// the filesize — a spec-derivable known answer independent of the implementation.
	const size = 200000 // > 2*64KiB so the two windows don't overlap
	path := writeFixture(t, size, func(int) byte { return 0 })

	got, err := MovieHash(path)
	if err != nil {
		t.Fatalf("MovieHash: %v", err)
	}
	// size = 200000 = 0x30D40.
	if got != "0000000000030d40" {
		t.Fatalf("MovieHash all-zeros = %q, want 0000000000030d40", got)
	}
}

func TestMovieHashKnownPattern(t *testing.T) {
	// A file whose every 8-byte little-endian word equals 1: the checksum is the
	// count of words summed over the first and last 64 KiB windows. With size =
	// 2*64KiB the windows tile the file exactly, so every word is counted once:
	// 2*64KiB/8 = 16384 words, each 1 → sum 16384. hash = size + 16384.
	const chunk = 64 * 1024
	const size = 2 * chunk
	one := make([]byte, 8)
	binary.LittleEndian.PutUint64(one, 1)
	path := writeFixture(t, size, func(i int) byte { return one[i%8] })

	got, err := MovieHash(path)
	if err != nil {
		t.Fatalf("MovieHash: %v", err)
	}
	// size(131072) + 16384 = 147456 = 0x24000.
	if got != "0000000000024000" {
		t.Fatalf("MovieHash pattern = %q, want 0000000000024000", got)
	}
}

func TestMovieHashOverlapWindowsCountTwice(t *testing.T) {
	// When the file is smaller than 2*64KiB the first and last windows overlap; the
	// reference algorithm still reads a full window from each end, so the overlap
	// region is summed twice. Verify against an independent computation.
	const chunk = 64 * 1024
	const size = chunk + 4096 // windows overlap by (2*chunk - size) bytes
	path := writeFixture(t, size, func(i int) byte { return byte(i) })

	want := referenceHash(t, path)
	got, err := MovieHash(path)
	if err != nil {
		t.Fatalf("MovieHash: %v", err)
	}
	if got != want {
		t.Fatalf("MovieHash overlap = %q, want %q", got, want)
	}
}

func TestMovieHashTooSmall(t *testing.T) {
	// A file smaller than one window can't be hashed by the OpenSubtitles scheme;
	// the primitive reports that rather than inventing a value.
	path := writeFixture(t, 1000, func(int) byte { return 0 })
	if _, err := MovieHash(path); err == nil {
		t.Fatalf("MovieHash on tiny file: want error, got nil")
	}
}

// referenceHash is a deliberately naive, obviously-correct second implementation
// of the OpenSubtitles moviehash used only to check the production one on the
// overlap case: filesize + the sum of every uint64 LE word in the first and last
// 64 KiB, read as two independent full windows.
func referenceHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	const chunk = 64 * 1024
	sum := uint64(len(data))
	sumWindow := func(b []byte) {
		for i := 0; i+8 <= len(b); i += 8 {
			sum += binary.LittleEndian.Uint64(b[i : i+8])
		}
	}
	sumWindow(data[:chunk])
	sumWindow(data[len(data)-chunk:])
	return formatHash(sum)
}
