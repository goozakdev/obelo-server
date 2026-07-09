package subtitle

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// hashChunk is the window size the OpenSubtitles moviehash reads from each end of
// the file (64 KiB), fixed by the algorithm.
const hashChunk = 64 * 1024

// MovieHash computes the OpenSubtitles "moviehash" for a media file: the 64-bit
// sum of the filesize plus every little-endian uint64 word in the first 64 KiB and
// the last 64 KiB of the file (overflow wraps), rendered as a zero-padded 16-digit
// lowercase hex string. It is the one release-exact matching primitive slice 05
// adds — computed LAZILY on a fetch, never stored as identity (ADR-0002 identity
// stays path+mtime+size; this hashes media BYTES only to match a subtitle release).
//
// A file smaller than one 64 KiB window can't be hashed by this scheme (there is
// not a full window to read) and returns an error; every real video file is far
// larger. When the file is between one and two windows the two windows overlap and
// the overlapping bytes are summed twice — the reference algorithm's behavior,
// reproduced here so a small fixture still matches other implementations.
func MovieHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("subtitle: opening for moviehash: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("subtitle: stat for moviehash: %w", err)
	}
	size := info.Size()
	if size < hashChunk {
		return "", fmt.Errorf("subtitle: file too small to moviehash (%d < %d bytes)", size, hashChunk)
	}

	sum := uint64(size)

	head := make([]byte, hashChunk)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", fmt.Errorf("subtitle: reading moviehash head: %w", err)
	}
	sum += sumWords(head)

	tail := make([]byte, hashChunk)
	if _, err := f.ReadAt(tail, size-hashChunk); err != nil && err != io.EOF {
		return "", fmt.Errorf("subtitle: reading moviehash tail: %w", err)
	}
	sum += sumWords(tail)

	return formatHash(sum), nil
}

// sumWords adds every whole little-endian uint64 word in b to a wrapping 64-bit
// accumulator. A trailing partial word (b not a multiple of 8) is ignored, exactly
// as the reference algorithm does over its fixed-size windows.
func sumWords(b []byte) uint64 {
	var s uint64
	for i := 0; i+8 <= len(b); i += 8 {
		s += binary.LittleEndian.Uint64(b[i : i+8])
	}
	return s
}

// formatHash renders the 64-bit moviehash as the OpenSubtitles wire form: a
// zero-padded, 16-digit lowercase hex string.
func formatHash(sum uint64) string {
	return fmt.Sprintf("%016x", sum)
}
