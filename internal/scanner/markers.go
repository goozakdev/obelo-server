package scanner

import (
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
// because it can change without the video changing.

// MarkerStore is the persistence for Local Markers. *store.DB satisfies it. It
// is OPTIONAL: a fake Store that does not implement it scans with Markers
// disabled, which keeps every existing scanner test unchanged.
type MarkerStore interface {
	ReplaceLocalMarkers(path string, ms []store.Marker) error
}

// recordLocalMarkers stores the Local Markers of a freshly probed File: its
// `.edl` spans when it names any, else its chapters. A File that names nothing
// clears whatever an earlier probe stored.
func (s *Service) recordLocalMarkers(path string, media MediaInfo) error {
	ms, ok := s.store.(MarkerStore)
	if !ok {
		return nil
	}
	spans := edlMarkers(path, media.DurationMs)
	if len(spans) == 0 {
		spans = markers.FromChapters(media.Chapters, media.DurationMs)
	}
	return ms.ReplaceLocalMarkers(path, toStoreMarkers(spans))
}

// refreshEDLMarkers re-reads the `.edl` of an unchanged (not re-probed) File.
// Only an `.edl` that names something is written; otherwise the stored Markers —
// possibly read from chapters by an earlier probe — are left as they are.
func (s *Service) refreshEDLMarkers(path string, durationMs int64) error {
	ms, ok := s.store.(MarkerStore)
	if !ok {
		return nil
	}
	spans := edlMarkers(path, durationMs)
	if len(spans) == 0 {
		return nil
	}
	return ms.ReplaceLocalMarkers(path, toStoreMarkers(spans))
}

// edlMarkers reads the `.edl` beside path, if there is one.
func edlMarkers(path string, durationMs int64) []markers.Span {
	data, err := os.ReadFile(strings.TrimSuffix(path, filepath.Ext(path)) + ".edl")
	if err != nil {
		return nil
	}
	return markers.ParseEDL(data, durationMs)
}

func toStoreMarkers(spans []markers.Span) []store.Marker {
	out := make([]store.Marker, 0, len(spans))
	for _, sp := range spans {
		out = append(out, store.Marker{Kind: sp.Kind, Source: markers.SourceLocal, StartMs: sp.StartMs, EndMs: sp.EndMs})
	}
	return out
}
