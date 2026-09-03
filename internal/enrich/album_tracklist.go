package enrich

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// album_tracklist.go is the Service's side of ADR-0050's tracklist tier: one cached
// read that answers "the ordered tracks of THIS album", so a matched Album can name
// the recording behind each of its own Tracks instead of every Track paying a text
// search at the endpoint ADR-0049 watched shed load.
//
// The provider half (which release, and how it is chosen) lives in musicbrainz.go
// behind the AlbumTracklister seam. What lives here is the part the provider must
// not own: the enablement gate, the capability type-assert, and the cache.

// DefaultAlbumTracklistCacheTTL is how long one album's resolved tracklist is reused
// before the next read re-queries. A release's tracklist is about as stable as a
// series' episode list — it changes when someone edits MusicBrainz, which is not
// within a sitting — so it shares that TTL, long enough that the pass and a Cascade
// over the same album inside one operator gesture cost one call between them, short
// enough that a MusicBrainz correction shows up without a restart.
const DefaultAlbumTracklistCacheTTL = DefaultEpisodeListCacheTTL

// DefaultAlbumEditionCacheTTL is how long a release-group's EDITION LIST is reused
// (ADR-0052). Same TTL and the same reasoning as the tracklist's — a release-group
// gains an edition when someone edits MusicBrainz, which is not within a sitting —
// with one extra beneficiary: an Admin opening, closing and reopening the edition
// section while they decide pays for it once.
const DefaultAlbumEditionCacheTTL = DefaultAlbumTracklistCacheTTL

// albumTracklist returns the album's ordered tracks, from the cache when it can and
// from the provider otherwise. It is the ONLY way the pass should reach a tracklist:
// the enablement gate and the capability check are here, not at the call sites.
//
// The error contract is the AlbumTracklister's, narrowed by one guarantee: the list
// is never empty with a nil error. ErrNoTracklist means the album HAS no tracklist —
// unresolved, music enrichment off, a provider that can't answer, a release-group
// with no releases — and every other error is a real failure returned as itself, so
// a caller can still tell a 503 apart from a settled nothing (ADR-0049).
//
// That distinction is the point of the sentinel: "this album has no tracklist" and
// "this album's tracklist has no room for this track" send an Admin to two different
// places, and a provider returning (nil, nil) for the first would render them as the
// same shrug (ADR-0050).
//
// snap is passed rather than read from the Service because a pass resolves its
// Library's effective provider once at the start and threads it through; reading the
// global snapshot here would silently ignore a per-Library Enrichment policy.
func (s *Service) albumTracklist(ctx context.Context, snap providerSnapshot, req TracklistRequest) ([]TrackCandidate, error) {
	if strings.TrimSpace(req.ReleaseGroupID) == "" {
		return nil, ErrNoTracklist
	}
	if !snap.enablement.enabledFor("album") {
		return nil, ErrNoTracklist
	}
	lister, ok := snap.provider.(AlbumTracklister)
	if !ok {
		return nil, ErrNoTracklist
	}
	key := albumTracklistKey(req)
	if tl, hit := s.tracklists.get(key); hit {
		return tl, nil
	}
	tl, err := lister.AlbumTracklist(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(tl) == 0 {
		// A provider outside this package may still hand back the empty tracklist the
		// seam forbids. Normalize it here so no caller has to check both.
		return nil, ErrNoTracklist
	}
	s.tracklists.put(key, tl)
	return tl, nil
}

// albumTracklistResult is one album's resolved tracklist plus the single fact about
// it that the tracklist itself cannot carry: whether these entries came from the
// edition a HUMAN named (ADR-0052). Every release's tracklist looks alike, so once
// the entries are in hand there is no way back to which release produced them.
//
// FromChosenEdition is the LICENCE. ADR-0052 grants position-alone mapping on
// exactly one fact — that a human asserted this edition — and emphatically not on
// the counts matching or the titles mostly agreeing, which are the automatic
// matcher's heuristics in a new hat. It is true only when the Admin's pinned release
// actually supplied these entries: a pin that is a stranger to the album's
// release-group, or that MusicBrainz has since moved away, resolves through the
// ordinary tag/fit path with the licence withheld.
type albumTracklistResult struct {
	Tracks            []TrackCandidate
	FromChosenEdition bool
}

// albumTracklistFor resolves ADR-0052's precedence for one album — the edition an
// ADMIN chose, then the release the FILES name, then best fit by track count — over
// the single-request read above, and reports which tier answered.
//
// req is the ordinary tag/fit request (release-group, the tag release, the local
// count); chosenReleaseID is store.EntityEnrichment.ChosenReleaseID, "" when nobody
// pinned an edition. With no pin this is exactly albumTracklist and one call, which
// is what "an album with no chosen release behaves as it does today" means
// mechanically.
//
// With a pin it is still one call in the case that matters — the pin applies and
// answers. It costs a second only in the contradiction: an edition whose parent
// release-group is not this album's. That is not a retry of a refusal (ADR-0049's
// rule, honored: a real failure returns as itself and issues nothing further); it is
// the fall-back the precedence always described, taken deliberately so the licence
// can be withheld from the tracklist that fall-back produces.
func (s *Service) albumTracklistFor(ctx context.Context, snap providerSnapshot,
	req TracklistRequest, chosenReleaseID string) (albumTracklistResult, error) {

	if chosen := strings.TrimSpace(chosenReleaseID); chosen != "" {
		pinned := req
		pinned.ReleaseID, pinned.ReleaseIDChosen = chosen, true
		tl, err := s.albumTracklist(ctx, snap, pinned)
		switch {
		case err == nil:
			return albumTracklistResult{Tracks: tl, FromChosenEdition: true}, nil
		case !errors.Is(err, ErrNoTracklist):
			return albumTracklistResult{}, err
		}
		// ErrNoTracklist under a pin means the edition did not apply: a stranger to
		// this release-group, an id MusicBrainz no longer knows, or a release with no
		// tracks. Fall back to what the album can say for itself — and drop the licence
		// with the pin, because what comes back is not what the human asserted.
	}
	req.ReleaseIDChosen = false
	tl, err := s.albumTracklist(ctx, snap, req)
	if err != nil {
		return albumTracklistResult{}, err
	}
	return albumTracklistResult{Tracks: tl}, nil
}

// albumTracklistKey keys the cache by the whole REQUEST, not by the album: the
// answer is a function of the release-group, the release named, whether that release
// was a human's pin, and the local track count — and two albums sharing a
// release-group but not a track count (a standard edition and its deluxe, ripped
// separately) must not be served each other's tracklist. The NUL separators keep two
// fields from concatenating into one key some other pair could also produce.
//
// ReleaseIDChosen belongs in the key even though it never changes a successful
// answer, because it changes what a FAILING one falls back to: the same release id
// asked un-pinned can cache a fit tracklist, and a later pinned ask that hit that
// entry would inherit the licence for entries the human never asserted.
func albumTracklistKey(req TracklistRequest) string {
	chosen := "0"
	if req.ReleaseIDChosen {
		chosen = "1"
	}
	return strings.ToLower(strings.TrimSpace(req.ReleaseGroupID)) + "\x00" +
		strings.ToLower(strings.TrimSpace(req.ReleaseID)) + "\x00" + chosen + "\x00" +
		strconv.Itoa(req.LocalTrackCount)
}
