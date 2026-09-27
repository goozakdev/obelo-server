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
	_, _, sessionMs := s.creditsWatchedPoint(sess)
	if sessionMs < 0 {
		return WatchedCeiling
	}
	return float64(sessionMs) / float64(sess.DurationMs)
}

// creditsWatchedPoint finds the Credits Marker that is the session's Watched
// point: the File it belongs to, its start on that File's own timeline, and that
// start on the session's timeline. sessionMs is -1 when there is none and the
// flat WatchedCeiling applies.
//
// The Credits that end the work are the ones that count. For a session whose File
// IS the whole work, that is the File's earliest Credits at or past CreditsFloor of
// its duration. On a multi-part Edition the session measures the sum of the parts,
// and only the LAST part's Credits end the work: the floor is measured on that
// part's own duration and its start is offset by every earlier part. Any earlier
// part's Credits are not the work's — finishing part 1 must not mark the work
// watched (the defect sessionDurationMs exists to prevent). Any lookup failure
// falls back to the flat ceiling: a Marker is an improvement to the threshold,
// never a reason a progress report fails.
func (s *Service) creditsWatchedPoint(sess Session) (fileID string, fileMs, sessionMs int64) {
	if s.markers == nil || sess.FileID == "" || sess.DurationMs <= 0 {
		return "", -1, -1
	}
	detail, err := s.store.TitleByID(sess.TitleID)
	if err != nil {
		return "", -1, -1
	}
	last, offsetMs, ok := lastPartOf(detail, sess)
	if !ok || last.DurationMs <= 0 || offsetMs+last.DurationMs != sess.DurationMs {
		return "", -1, -1
	}
	ms, err := s.fileMarkers(last.ID)
	if err != nil {
		return "", -1, -1
	}
	fileMs = -1
	for _, m := range ms {
		if m.Kind != markers.KindCredits || float64(m.StartMs) < CreditsFloor*float64(last.DurationMs) {
			continue
		}
		if fileMs < 0 || m.StartMs < fileMs {
			fileMs = m.StartMs
		}
	}
	if fileMs < 0 {
		return "", -1, -1
	}
	return last.ID, fileMs, offsetMs + fileMs
}

// lastPartOf is the File that ends the session's timeline and the session offset
// it starts at: the last part of the session's multi-part Edition, else the
// session's own File at 0.
func lastPartOf(detail store.TitleDetail, sess Session) (store.File, int64, bool) {
	for _, ed := range detail.Editions {
		parts := ed.Parts()
		if len(parts) < 2 || parts[0].ID != sess.FileID {
			continue
		}
		return parts[len(parts)-1], ed.PartStartMs(len(parts) - 1), true
	}
	f, ok := fileByID(detail, sess.FileID)
	return f, 0, ok
}

// SessionWatchedPointMs is the start of the Credits Marker whose crossing marks
// the session's Title watched (see watchedCeiling), on the session File's own
// timeline, or -1 when none of that File's Markers does, for the player to offer
// "Next episode" only there. An unknown session is -1.
func (s *Service) SessionWatchedPointMs(sessionID string) int64 {
	sess, ok := s.sessions.Get(sessionID)
	if !ok {
		return -1
	}
	fileID, fileMs, _ := s.creditsWatchedPoint(sess)
	if fileID != sess.FileID {
		return -1
	}
	return fileMs
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
	return s.fileMarkers(sess.FileID)
}
