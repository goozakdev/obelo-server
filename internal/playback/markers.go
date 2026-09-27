package playback

import (
	"github.com/goozakdev/obelo-server/internal/markers"
	"github.com/goozakdev/obelo-server/internal/store"
)

// CreditsFloor is the least fraction of a File's duration a Credits Marker must
// start at to become its Watched ceiling (ADR-0065 §5). Crossing the ceiling
// clears resume, so a Credits Marker sitting early in a File — mis-detected, or
// a cold-open-heavy cut — would drop a viewer's place almost as soon as they
// started. Below the floor the Marker is ignored and WatchedCeiling applies.
const CreditsFloor = 0.50

// MarkerStore lists a File's stored Markers (ADR-0065). *store.DB satisfies it.
// Like AudioMemoryStore it is an OPTIONAL dependency, type-asserted off the
// passed store in NewService.
type MarkerStore interface {
	MarkersForFile(fileID string) ([]store.Marker, error)
}

// SetFetchedMarkersServed installs whether Fetched Markers are served now: only
// while at least one Marker provider is installed and enabled (markerfetch
// Service.Serving). Their rows are kept either way, so a provider enabled again
// serves them without being asked again. Post-construction, like SetRelay: the
// providers are known only once the Plugins are. Nil serves them.
func (s *Service) SetFetchedMarkersServed(served func() bool) { s.fetchedServed = served }

// fileMarkers lists the Markers of a File as they are served: the store's, less
// the Fetched ones while no Marker provider is enabled.
func (s *Service) fileMarkers(fileID string) ([]store.Marker, error) {
	ms, err := s.markers.MarkersForFile(fileID)
	if err != nil || s.fetchedServed == nil {
		return ms, err
	}
	for _, m := range ms {
		if m.Source == markers.SourceFetched {
			if s.fetchedServed() {
				return ms, nil
			}
			out := ms[:0:0]
			for _, m := range ms {
				if m.Source != markers.SourceFetched {
					out = append(out, m)
				}
			}
			return out, nil
		}
	}
	return ms, nil
}

// watchedCeiling is the fraction of the session's duration at or past which a
// progress report marks the Title watched: the start of the Credits Marker that
// is its Watched point (creditsWatchedPoint), else the flat WatchedCeiling.
func (s *Service) watchedCeiling(sess Session) float64 {
	sessionMs := s.creditsWatchedPoint(sess)
	if sessionMs < 0 {
		return WatchedCeiling
	}
	return float64(sessionMs) / float64(sess.DurationMs)
}

// creditsWatchedPoint is the start, on the session's timeline, of the Credits
// Marker that is the session's Watched point, or -1 when there is none and the
// flat WatchedCeiling applies.
//
// The Credits that end the work are the ones that count: those of the LAST part
// of the timeline the session started with (Session.Parts), which for a session
// whose File IS the whole work is that File. It is the part's earliest Credits
// at or past CreditsFloor of the part's own duration, offset by every earlier
// part. Any earlier part's Credits are not the work's — finishing part 1 must not
// mark the work watched (the defect sessionDurationMs exists to prevent). A part
// the session started with that has since left the Title, or changed length,
// means the timeline it was measured on is gone. That, and any lookup failure,
// falls back to the flat ceiling: a Marker is an improvement to the threshold,
// never a reason a progress report fails.
//
// A relay session reads the sharer's Markers as they were asked for when it
// started (relay.go), already on the session's timeline; only Credits that start
// inside the last part count, and the floor is still this Server's.
func (s *Service) creditsWatchedPoint(sess Session) int64 {
	n := len(sess.Parts)
	if n == 0 || sess.DurationMs <= 0 {
		return -1
	}
	last := sess.Parts[n-1]
	lastStartMs := partStartMs(sess.Parts, n-1)
	if last.DurationMs <= 0 || lastStartMs+last.DurationMs != sess.DurationMs {
		return -1
	}
	var ms []store.Marker // on the last part's own timeline
	if sess.IsRelay() {
		served, ok := sess.relayMarkers.get()
		if !ok {
			return -1
		}
		for _, m := range served {
			if m.StartMs >= lastStartMs && m.StartMs < sess.DurationMs {
				m.StartMs -= lastStartMs
				ms = append(ms, m)
			}
		}
	} else {
		if s.markers == nil || !s.partsUnchanged(sess) {
			return -1
		}
		var err error
		if ms, err = s.fileMarkers(last.FileID); err != nil {
			return -1
		}
	}
	fileMs := int64(-1)
	for _, m := range ms {
		if m.Kind != markers.KindCredits || float64(m.StartMs) < CreditsFloor*float64(last.DurationMs) {
			continue
		}
		if fileMs < 0 || m.StartMs < fileMs {
			fileMs = m.StartMs
		}
	}
	if fileMs < 0 {
		return -1
	}
	return lastStartMs + fileMs
}

// partStartMs is where part index starts on a session's timeline: the sum of
// every earlier part's duration.
func partStartMs(parts []SessionPart, index int) int64 {
	var start int64
	for _, p := range parts[:index] {
		start += p.DurationMs
	}
	return start
}

// partsUnchanged reports whether every part the session started with is still a
// File of its Title with the same duration — and, on a multi-part timeline,
// still present, since a Missing part is no longer played.
func (s *Service) partsUnchanged(sess Session) bool {
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return false
	}
	for _, p := range sess.Parts {
		f, ok := fileByID(detail, p.FileID)
		if !ok || f.DurationMs != p.DurationMs || len(sess.Parts) > 1 && !f.Present {
			return false
		}
	}
	return true
}

// SessionWatchedPointMs is the start of the Credits Marker whose crossing marks
// the session's Title watched (see watchedCeiling), on the session's timeline as
// SessionMarkers serves it, or -1 when there is none, for the player to offer
// "Next episode" only there. An unknown session is -1.
func (s *Service) SessionWatchedPointMs(sessionID string) int64 {
	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		return -1
	}
	return s.creditsWatchedPoint(sess)
}

// SessionMarkers lists the Markers of the session's timeline, for the player's
// Skip button: every part's, each shifted by its part's start on the timeline the
// session started with, so a multi-part session offers Skip in every part. A
// relay session serves the sharer's that lie inside the session's timeline, asked
// for once when it started (empty until that ask has ended, and after one that
// failed). Ownership is the progress
// route's: a reaped, ended or foreign session is ErrSessionNotFound. A Server with
// no Marker store, and a File with none, both answer an empty list.
func (s *Service) SessionMarkers(userID, sessionID string) ([]store.Marker, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return nil, ErrSessionNotFound
	}
	if sess.IsRelay() {
		ms, _ := sess.relayMarkers.get()
		return ms, nil
	}
	if s.markers == nil {
		return nil, nil
	}
	var out []store.Marker
	for i, p := range sess.Parts {
		ms, err := s.fileMarkers(p.FileID)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			out = append(out, ms...)
			continue
		}
		start := partStartMs(sess.Parts, i)
		for _, m := range ms {
			m.StartMs += start
			m.EndMs += start
			out = append(out, m)
		}
	}
	return out, nil
}
