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

// watchedCeiling is the fraction of the session's duration at or past which a
// progress report marks the Title watched: the start of the File's earliest
// Credits Marker at or past CreditsFloor, else the flat WatchedCeiling.
func (s *Service) watchedCeiling(sess Session) float64 {
	startMs := s.creditsWatchedPointMs(sess)
	if startMs < 0 {
		return WatchedCeiling
	}
	return float64(startMs) / float64(sess.DurationMs)
}

// creditsWatchedPointMs is the start of the Credits Marker that is the session
// File's Watched point, or -1 when there is none and the flat WatchedCeiling applies.
//
// Only a session whose File IS the whole work gets the Credits ceiling. On a
// multi-part Edition the session measures the sum of the parts, and one part's
// Credits are not the work's — finishing part 1 must not mark the work watched
// (the defect sessionDurationMs exists to prevent). Any lookup failure falls back
// to the flat ceiling: a Marker is an improvement to the threshold, never a
// reason a progress report fails.
func (s *Service) creditsWatchedPointMs(sess Session) int64 {
	if s.markers == nil || sess.FileID == "" || sess.DurationMs <= 0 {
		return -1
	}
	ms, err := s.markers.MarkersForFile(sess.FileID)
	if err != nil {
		return -1
	}
	var startMs int64 = -1
	for _, m := range ms {
		if m.Kind != markers.KindCredits || float64(m.StartMs) < CreditsFloor*float64(sess.DurationMs) {
			continue
		}
		if startMs < 0 || m.StartMs < startMs {
			startMs = m.StartMs
		}
	}
	if startMs < 0 {
		return -1
	}
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return -1
	}
	if f, ok := fileByID(detail, sess.FileID); !ok || f.DurationMs != sess.DurationMs {
		return -1
	}
	return startMs
}

// SessionWatchedPointMs is the start of the Credits Marker whose crossing marks
// the session's Title watched (see watchedCeiling), or -1 when none does, for the
// player to offer "Next episode" only there. An unknown session is -1.
func (s *Service) SessionWatchedPointMs(sessionID string) int64 {
	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		return -1
	}
	return s.creditsWatchedPointMs(sess)
}

// SessionMarkers lists the Markers of the File a session is playing, for the
// player's Skip button. Ownership is the progress route's: a reaped, ended or
// foreign session is ErrSessionNotFound. A Server with no Marker store, and a
// File with none, both answer an empty list.
func (s *Service) SessionMarkers(userID, sessionID string) ([]store.Marker, error) {
	sess, ok := s.sessions.Get(sessionID)
	if !ok || sess.UserID != userID {
		return nil, ErrSessionNotFound
	}
	if s.markers == nil || sess.FileID == "" {
		return nil, nil
	}
	return s.markers.MarkersForFile(sess.FileID)
}
