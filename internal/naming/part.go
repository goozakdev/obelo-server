// Package naming holds the multi-part suffix rule from the on-disk naming
// convention (docs/naming-convention.md): `- part1` / `cd1` / `pt1` / `disc1` /
// `disk1`.
//
// Almost all of the grammar lives in the scanner, which is where it belongs: the
// scanner is the only writer of identity (ADR-0002), and every other layer reads
// the structures it produced rather than re-parsing paths. This one rule lives
// here instead, standalone, so a test fixture that builds a multi-part set can
// call the scanner's own parser rather than hand-writing a second regex that can
// drift from it.
package naming

import (
	"path/filepath"
	"regexp"
	"strings"
)

// partRe matches a multi-part suffix: part1/part 1, cd1, pt1, disc1, disk1
// (naming-convention.md aliases). Group 1 is the part number.
var partRe = regexp.MustCompile(`(?i)[-_. ](?:part|pt|cd|disc|disk)[ _]?(\d+)\b`)

// PartNumber returns the multi-part number a filename declares (1-based), or 0
// when the name says nothing about parts.
//
// This is the convention's ONLY way to say "these files are one work played
// back-to-back" from disk: `Dune (2021) - part1.mkv` / `- part2.mkv`, and the
// `cd`/`pt`/`disc`/`disk` aliases. Two files in one Edition that this returns 0
// for are the collision rule's case — the same Edition claimed twice, flagged
// ambiguous rather than silently joined.
//
// `name` may be a bare filename or a full path; the extension is ignored.
func PartNumber(name string) int {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	m := partRe.FindStringSubmatch(base)
	if m == nil {
		return 0
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n
}
