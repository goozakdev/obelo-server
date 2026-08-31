package catalog

import (
	"errors"

	"github.com/goozakdev/obelo-server/internal/store"
)

// showproblems.go answers the question the Needs-Fixing queue asks once per Show:
// how much UNSETTLED work is there under this Show, and which file should the row
// point at?
//
// It exists because the queue collapsed its episode-level rows into one row per
// Show (file-matcher/07). Five broken files used to be five rows, each offering to
// re-pick the SERIES — a fix that was inert by construction, because the series was
// never the part that was wrong. One row can only replace five if it can count what
// the five were, and two thirds of that count is invisible to the client:
//
//   - which Unmatched rows fall under THIS Show's folders (the Library list is flat
//     paths; only the Show's folders decide whose they are), and
//   - which Files the Admin explicitly left unassigned, which by design produce
//     neither a Title nor an Unmatched row (scanner/arrangement.go) and so appear in
//     no list the queue already fetches.
//
// THE COUNT IS THE MATCHER'S OWN. Every number here is read off
// `showArrangement` — the same function `ShowMatcher` renders from. That is not a
// convenience: the row's promise is that assigning or ignoring every file clears
// it, and the screen that does the assigning is the matcher. A count derived from
// its own query would drift from what the matcher shows, and the first symptom
// would be a row the Admin cannot clear however much work they do.
//
// What it does NOT count is the flagged Episodes — the needs-review and
// enrichment-attention Titles. Those the client already holds, in the two lists the
// queue has always fetched, and each already names its Show. Counting them again
// here would be a second definition of the same thing.

// ShowProblems is one Show's unsettled Files, as the matcher counts them.
//
// A Show with none of these is not in the result at all: the queue lists work, and
// a Show whose every File is placed or ignored has none.
type ShowProblems struct {
	ShowID string
	Title  string
	Year   int
	// Path is a representative unsettled File — the "which file?" every queue row
	// has to answer. It is one of the counted Files, never just any File of the
	// Show, so it points at the problem rather than near it.
	Path string
	// Unassigned counts Files the Admin deliberately took off a Slot. Undecided is
	// not settled (CONTEXT.md "Unassigned"), so these keep the Show queued until
	// they are placed or ignored.
	Unassigned int
	// Unidentified counts Files nothing could number and nobody has decided about —
	// the Unmatched rows under this Show's folders, plus any File the parse left
	// with no Slot.
	Unidentified int
	// UnmatchedPaths are the Library Unmatched rows this Show absorbed. The queue
	// drops them from its own flat list: a file counted in a Show row and listed
	// again on its own is the five-rows-one-problem shape returning by the back
	// door.
	UnmatchedPaths []string
	// Orphaned counts Placements whose anchor file is gone, and OrphanedPath names
	// one. An orphaned correction is a problem in its own right, never folded in
	// with the rest (CONTEXT.md "Orphaned correction"), so the queue gives it its
	// own row.
	Orphaned     int
	OrphanedPath string
	// UnreadablePaths are this Show's files ffprobe refused (CONTEXT.md
	// "Unreadable"). They are ATTRIBUTED but never COUNTED, and that split is the
	// whole point of the field: an unreadable file is not work the matcher can
	// settle, so it stays a flat row of its own rather than becoming a number in
	// this Show's row — and the row still needs to know which Show it belongs to,
	// or it can only tell the Admin what is broken and not where to go about it
	// (ADR-0047).
	UnreadablePaths []string
}

// Unsettled is how many Files keep this Show queued. Orphaned Placements are
// excluded: they are a broken correction rather than an undecided File, and are
// counted by their own row.
func (p ShowProblems) Unsettled() int { return p.Unassigned + p.Unidentified }

// Empty reports whether this Show has nothing for the queue to show.
func (p ShowProblems) Empty() bool { return p.Unsettled() == 0 && p.Orphaned == 0 }

// ShowProblems returns the unsettled-File counts for every Show of a TV Library.
// A Library with no Shows (a Movie or Music Library) returns an empty list rather
// than an error — the queue asks every Library the same question.
//
// The Library-wide reads (the decisions map, the Unmatched list) are made ONCE and
// shared across every Show, which is the only reason this is affordable on a
// Library with a hundred Shows.
func (s *Service) ShowProblems(libraryID string) ([]ShowProblems, error) {
	exists, err := s.store.LibraryExists(libraryID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	shows, err := s.store.ShowsByLibrary(libraryID)
	if err != nil {
		return nil, err
	}
	if len(shows) == 0 {
		return []ShowProblems{}, nil
	}
	lib, err := s.libraryFileState(libraryID)
	if err != nil {
		return nil, err
	}

	out := make([]ShowProblems, 0, len(shows))
	for _, sh := range shows {
		local, err := s.showArrangement(sh, lib)
		if err != nil {
			return nil, err
		}
		p := ShowProblems{ShowID: sh.ID, Title: sh.Title, Year: sh.Year}
		for _, f := range local.files {
			// An orphaned Placement is not an undecided File — its anchor is gone, so
			// there is nothing to place. It gets its own row and its own count.
			if f.Orphaned {
				p.Orphaned++
				if p.OrphanedPath == "" {
					p.OrphanedPath = f.Path
				}
				continue
			}
			if f.State != store.DecisionUnassigned {
				continue
			}
			// Decided tells the two unassigned States apart: the Admin took this File
			// off its Slot (a recorded decision), or nothing could ever number it. The
			// row's sentence says different things about the two, and they belong under
			// different filter chips.
			if f.Decided {
				p.Unassigned++
			} else {
				p.Unidentified++
			}
			if p.Path == "" {
				p.Path = f.Path
			}
		}
		// The Unmatched rows this Show absorbed, attributed by the SAME folder set
		// the matcher scopes its file list with, so a path counted here is a path the
		// matcher will offer.
		//
		// An UNREADABLE file is deliberately not absorbed. Absorbing a path is a
		// promise that the matcher can settle it, and the matcher cannot: the file's
		// name numbers it, so the screen shows it correctly placed on its Slot while
		// ffprobe still refuses the bytes and no Title is ever built. Folded in, it
		// would vanish from the queue's flat list into a Show row that says nothing
		// about it — the one file in the library that needs a human, hidden behind a
		// screen that reports it as already fine. It keeps its own row instead
		// (CONTEXT.md "Unreadable").
		for _, u := range lib.unmatched {
			if !local.folders[showFolderOf(u.Path)] {
				continue
			}
			if u.Unreadable() {
				p.UnreadablePaths = append(p.UnreadablePaths, u.Path)
				continue
			}
			p.UnmatchedPaths = append(p.UnmatchedPaths, u.Path)
		}
		// A Show whose ONLY trouble is an unreadable file is still reported, and still
		// reports nothing to do: every count is zero, so no client builds a Show row
		// from it, and the attribution the flat row needs is there all the same.
		if p.Empty() && len(p.UnreadablePaths) == 0 {
			continue
		}
		if p.Path == "" {
			p.Path = p.OrphanedPath
		}
		out = append(out, p)
	}
	return out, nil
}

// MarkShowEpisodesReviewed dismisses the needs-review flag on every flagged
// Episode of a Show — the "Looks right" behind a collapsed Show row, which stands
// for the whole set the row counted rather than for one Episode. ErrNotFound for
// an unknown Show; a Show with nothing flagged reports 0 and is not an error.
func (s *Service) MarkShowEpisodesReviewed(showID string) (int, error) {
	if _, err := s.store.ShowByID(showID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return s.store.MarkShowEpisodesReviewed(showID)
}
