package catalog

import (
	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/lyrics"
)

// TitleLyrics returns the Local lyrics of a Title the Scope may see, and false
// when it has none — a Track the Scanner found no words for, or any Title that is
// not a Track. A Title outside the Scope is ErrNotFound, exactly as GetTitle.
func (s *Service) TitleLyrics(scope access.Scope, id string) (lyrics.Lyrics, bool, error) {
	if _, err := s.GetTitle(scope, id); err != nil {
		return lyrics.Lyrics{}, false, err
	}
	return s.store.LocalLyrics(id)
}
