package enrich

import (
	"context"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
)

// album_editions.go is the Service's side of ADR-0052's last decision: an edition
// can be chosen WITHOUT LEAVING OBELO.
//
// The operator's own account of the workflow this replaces: *"requires going to the
// web page and getting a URL because i was unable to select a specific edition. It
// shows the best guess, but I cant choose a specific edition out of that
// release-group."* Everything below exists to answer exactly that — here are the
// editions, here is the one in use and why, here is how many tracks yours has.
//
// The provider half (the browse) lives in musicbrainz.go behind the
// AlbumEditionLister seam and is the SAME browse fit-selection pays for. What lives
// here is what the provider must not own: the enablement gate, the capability
// type-assert, the cache, and the album-shaped question — which release-group, which
// edition is in use, and how many tracks the LOCAL album holds.

// EditionSource names WHY an edition is the one in use — the three tiers of
// ADR-0052's precedence, in the words the picker says them in.
const (
	// EditionSourceChosen — a human named this edition, and it applies. This is the
	// tier that licenses position-alone mapping (ADR-0052, issue 11).
	EditionSourceChosen = "chosen"
	// EditionSourceTagged — nobody chose; the FILES name this release
	// (musicbrainz_albumid), and it belongs to this release-group.
	EditionSourceTagged = "tagged"
	// EditionSourceFit — nobody chose and no usable tag: the system picked by
	// track-count fit. THIS is "the best guess" the operator could see and could not
	// overrule, and naming it as a guess in the picker is half the point of showing
	// the list at all.
	EditionSourceFit = "fit"
)

// AlbumEditions is everything the edition picker needs about ONE album: which
// release-group it is matched to, that group's editions, which edition currently
// decorates its tracks and on whose authority, and how many Tracks the LOCAL album
// holds.
//
// LocalTrackCount is not a convenience. The decision the Admin is making is "which
// of these is my copy", the answer is nearly always "the one with my number of
// tracks", and a picker that lists sixteen counts without stating the local one asks
// them to do arithmetic against a number that is not on the screen.
//
// An album with no matched release-group yields the zero value with no error and
// costs no provider call: there is nothing to list, which the picker renders as
// nothing rather than as a failure.
type AlbumEditions struct {
	ReleaseGroupID string
	// ChosenReleaseID is the edition STORED against this album (ADR-0052), "" when
	// nobody pinned one. It is deliberately reported beside InUseReleaseID rather
	// than folded into it: a pin that no longer belongs to this release-group is
	// stored and NOT in use, and collapsing the two would hide that.
	ChosenReleaseID string
	// InUseReleaseID is the edition that actually decorates this album's tracks
	// today, resolved through the same precedence the tracklist read uses, and
	// InUseSource says which tier answered. "" when the list is empty or nothing in
	// it can be chosen.
	InUseReleaseID  string
	InUseSource     string
	LocalTrackCount int
	Editions        []ReleaseEdition
}

// AlbumEditions lists the editions of the album's matched release-group, marking
// the one in use (ADR-0052).
//
// ErrSearchUnavailable when music enrichment is off or the provider cannot list
// editions — the picker's "not now", which the UI degrades to the pasted-URL escape
// hatch rather than an error page. Any other provider error is returned as itself so
// a 503 load shed stays distinguishable from a fact about the record (ADR-0049).
//
// Applying one of these is NOT done here. An edition is applied through the album's
// existing Enrichment override carrying the release id (ApplyEntityOverride +
// CascadeEntity), because an edition is a refinement of the album's record and not a
// second kind of pin — one apply path, one cascade, one summary.
func (s *Service) AlbumEditions(ctx context.Context, albumID string) (AlbumEditions, error) {
	lister, err := s.albumEditionLister()
	if err != nil {
		return AlbumEditions{}, err
	}
	al, err := s.store.AlbumByID(albumID)
	if err != nil {
		return AlbumEditions{}, err // ErrNotFound flows through
	}
	// The release-group the album is matched to, resolved exactly as the tracklist
	// tier resolves it: the Admin's/pass's record first, the FILES' release-group
	// (musicbrainz_albumid's parent) as the fallback a fully-tagged library gets for
	// free. Anything else would list the editions of an album this one is not.
	e, err := s.store.EntityEnrichmentByID(store.EntityAlbum, albumID)
	if err != nil {
		return AlbumEditions{}, err
	}
	anchor := strings.TrimSpace(e.ExternalID)
	if anchor == "" {
		anchor = strings.TrimSpace(al.MusicbrainzID)
	}
	if anchor == "" {
		// Unmatched: no release-group, no editions, no call. The album's own match is
		// the thing to fix, and the picker above this section is where that happens.
		return AlbumEditions{}, nil
	}
	tracks, err := s.store.TracksForAlbum(albumID)
	if err != nil {
		return AlbumEditions{}, err
	}
	out := AlbumEditions{
		ReleaseGroupID: anchor,
		// chosenAlbumEdition, not e.ChosenReleaseID(), for its release-group guard: an
		// edition stored under a DIFFERENT release-group is a stranger to the album as
		// it is matched now, and the tracklist read ignores it. The picker must agree,
		// or it would mark a row the album is not actually decorated from.
		ChosenReleaseID: s.chosenAlbumEdition(store.EntityAlbum, albumID, anchor),
		LocalTrackCount: len(tracks),
	}
	eds, err := s.editions(ctx, lister, anchor)
	if err != nil {
		return AlbumEditions{}, err
	}
	out.Editions = eds
	out.InUseReleaseID, out.InUseSource = editionInUse(eds, out.ChosenReleaseID,
		al.MusicbrainzReleaseID, out.LocalTrackCount)
	return out, nil
}

// editions reads one release-group's editions, from the cache when it can and from
// the provider otherwise. The cache is keyed by the release-group alone — unlike the
// tracklist's, this answer does not depend on the local album at all, so two albums
// of the same release-group share one entry.
//
// The cache is the reason opening the section twice costs ONE request. That matters
// more here than anywhere else in the picker: this list is what an Admin toggles
// while they think, at the rate-limited host ADR-0049 watched shed load.
func (s *Service) editions(ctx context.Context, lister AlbumEditionLister, rgID string) ([]ReleaseEdition, error) {
	key := strings.ToLower(strings.TrimSpace(rgID))
	if eds, hit := s.editionLists.get(key); hit {
		return eds, nil
	}
	eds, err := lister.ReleaseGroupEditions(ctx, rgID)
	if err != nil {
		return nil, err
	}
	s.editionLists.put(key, eds)
	return eds, nil
}

// albumEditionLister resolves the configured provider to the optional
// AlbumEditionLister capability, gated on MUSIC enrichment being on at all (an
// album's editions are a music notion). A provider that doesn't implement it is
// ErrSearchUnavailable — the same "not now" every other provider-backed list gives,
// and the signal the UI degrades to the paste box on.
func (s *Service) albumEditionLister() (AlbumEditionLister, error) {
	snap := s.snapshot()
	if !snap.enablement.enabledFor("album") {
		return nil, ErrSearchUnavailable
	}
	lister, ok := snap.provider.(AlbumEditionLister)
	if !ok {
		return nil, ErrSearchUnavailable
	}
	return lister, nil
}

// editionInUse resolves ADR-0052's precedence over a listed set of editions — the
// edition a HUMAN chose, then the one the FILES name, then best fit by track count —
// and reports which tier answered.
//
// It is the picker's twin of what albumTracklistFor + the provider do with one call
// each, decided here from the listing instead, and it agrees with them by
// construction on the tier that is hardest to guess: the fit is pickEditionByFit,
// the same function fit-selection itself calls.
//
// Both pins are honoured only when the edition is IN THIS LIST, which is the
// membership check the provider pays a lookup for (does this release belong to the
// album's release-group?). A stale pin, or one MusicBrainz has since moved, falls
// through here exactly as it falls through there — and reporting it as in use when
// it is not would tell the operator their choice had taken effect when it had not.
func editionInUse(eds []ReleaseEdition, chosenID, taggedID string, localCount int) (string, string) {
	if id := findEdition(eds, chosenID); id != "" {
		return id, EditionSourceChosen
	}
	if id := findEdition(eds, taggedID); id != "" {
		return id, EditionSourceTagged
	}
	if i := pickEditionByFit(eds, localCount); i >= 0 {
		return eds[i].ReleaseID, EditionSourceFit
	}
	return "", ""
}

// findEdition returns the listed edition's id when the list holds it, "" otherwise
// (including for an empty id, which is the ordinary "nobody named one").
func findEdition(eds []ReleaseEdition, releaseID string) string {
	id := strings.TrimSpace(releaseID)
	if id == "" {
		return ""
	}
	for _, e := range eds {
		if strings.EqualFold(strings.TrimSpace(e.ReleaseID), id) {
			return e.ReleaseID
		}
	}
	return ""
}
