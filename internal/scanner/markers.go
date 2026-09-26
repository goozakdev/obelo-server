package scanner

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/store"
)

// Local Markers (ADR-0065): the Scanner reads a video File's own chapters, or
// the edit-decision sidecar beside it (`<name>.edl`), and stores every span it
// can name as an Intro, Recap, Credits or Preview. No Plugin is involved and
// nothing leaves the disk.
//
// When both exist the `.edl` wins, because somebody wrote it on purpose to say
// where to skip; the chapters are the fallback. An `.edl` that names nothing (a
// classic commercial-break list) is no answer at all, so the chapters still
// count then.
//
// Markers are stored by path, so they are written as soon as the File is read,
// independent of whether its Title upserts. An unchanged File is not re-probed,
// so its chapter Markers stay as stored; its `.edl` is still re-read every walk
// because it can change without the video changing. When the `.edl` its Markers
// came from is gone, or names nothing any more, the File is probed once more for
// its chapters. What is kept about a path no files row holds any more — a File
// renamed or deleted, or a missing part of a multi-file Title — is removed at the
// end of every completed scan of a Library, incremental or full.

// MarkerStore is the persistence for Local Markers. *store.DB satisfies it. It
// is OPTIONAL: a fake Store that does not implement it scans with Markers
// disabled, which keeps every existing scanner test unchanged.
type MarkerStore interface {
	ReplaceLocalMarkers(path string, ms []store.Marker) error
	ReplaceEDLMarkers(path string, ms []store.Marker) error
	LocalMarkersFromEDL(path string) (bool, error)
	PruneOrphanedMarkers(gone func(path string) bool) (int, error)
}

// recordLocalMarkers stores the Local Markers of a freshly probed File: its
// `.edl` spans when it names any, else its chapters. A File that names nothing
// clears whatever an earlier probe stored.
func (s *Service) recordLocalMarkers(sc *scanCtx, path string, media MediaInfo) error {
	ms, ok := s.store.(MarkerStore)
	if !ok {
		return nil
	}
	if spans := sc.edlMarkers(path, media.DurationMs); len(spans) > 0 {
		return ms.ReplaceEDLMarkers(path, toStoreMarkers(spans))
	}
	return ms.ReplaceLocalMarkers(path, toStoreMarkers(markers.FromChapters(media.Chapters, media.DurationMs)))
}

// refreshEDLMarkers re-reads the `.edl` of an unchanged (not re-probed) File.
// Only an `.edl` that names something is written; otherwise the stored Markers —
// possibly read from chapters by an earlier probe — are left as they are, unless
// they were read from an `.edl` that is gone or names nothing now: then it
// reports reprobe, and the File's chapters are read again.
func (s *Service) refreshEDLMarkers(sc *scanCtx, path string, durationMs int64) (reprobe bool, err error) {
	ms, ok := s.store.(MarkerStore)
	if !ok {
		return false, nil
	}
	spans := sc.edlMarkers(path, durationMs)
	if len(spans) == 0 {
		return ms.LocalMarkersFromEDL(path)
	}
	return false, ms.ReplaceEDLMarkers(path, toStoreMarkers(spans))
}

// pruneOrphanedMarkers removes what is kept about paths that are gone.
func (s *Service) pruneOrphanedMarkers() error {
	ms, ok := s.store.(MarkerStore)
	if !ok {
		return nil
	}
	_, err := ms.PruneOrphanedMarkers(func(path string) bool {
		_, err := os.Lstat(path)
		return errors.Is(err, fs.ErrNotExist)
	})
	return err
}

// listDir lists a folder for its `.edl` names; a test counts the listings.
var listDir = os.ReadDir

// edlMarkers reads the `.edl` beside path, if there is one.
func (sc *scanCtx) edlMarkers(path string, durationMs int64) []markers.Span {
	name := edlName(filepath.Base(path), sc.namesIn(filepath.Dir(path)))
	if name == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
	if err != nil {
		return nil
	}
	return markers.ParseEDL(data, durationMs)
}

// namesIn lists dir, once a scan: every File of a folder looks in it for its
// `.edl`, and a flat folder of N Files listed once per File would cost N
// listings of N names. An unreadable folder has no names.
func (sc *scanCtx) namesIn(dir string) []string {
	if names, ok := sc.dirNames[dir]; ok {
		return names
	}
	entries, _ := listDir(dir)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if sc.dirNames == nil {
		sc.dirNames = map[string][]string{}
	}
	sc.dirNames[dir] = names
	return names
}

// edlName picks the `.edl` of the video named base from a directory's names:
// `<name>.edl`, or failing that the same name in any case (`X.EDL`), which an
// exact lookup misses on a case-sensitive disk. "" when there is none.
func edlName(base string, names []string) string {
	want := strings.TrimSuffix(base, filepath.Ext(base)) + ".edl"
	found := ""
	for _, n := range names {
		if n == want {
			return n
		}
		if found == "" && strings.EqualFold(n, want) {
			found = n
		}
	}
	return found
}

func toStoreMarkers(spans []markers.Span) []store.Marker {
	out := make([]store.Marker, 0, len(spans))
	for _, sp := range spans {
		out = append(out, store.Marker{Kind: sp.Kind, Source: markers.SourceLocal, StartMs: sp.StartMs, EndMs: sp.EndMs})
	}
	return out
}
