package enrich

import (
	"context"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
)

// Cascade — apply a parent Enrichment override to its children (item-editing/05,
// ADR-0019). When an Admin corrects a parent (Fix info or Wrong item) with "also
// apply to children" ticked, the correction re-resolves the parent's children
// under the applied record, BEST-EFFORT, writing a DURABLE per-child Enrichment
// override for each mapped child. This is what makes fixing the wrong "Nirvana"
// repair its whole discography, and stops a correctly-matched album from carrying
// the wrong song titles.
//
// Mapping rules (the single hardest correctness surface in this feature):
//   - Album → tracks and Show → episodes map POSITIONALLY (disc+track /
//     season+episode number).
//   - Artist → albums map by TITLE(+year) against the corrected artist's
//     release-groups (obtained by searching the album kind and matching), then
//     RECURSE into each matched album's tracks positionally.
//
// Durability: a mapped leaf child is pinned via the same durable primitives slice
// 01/02 established — a track/episode gets its external-id column written
// (SetTitleExternalMatch, honored by the next collectMusicLeaves/collectTVLeaves
// pass), a mapped album gets entity_enrichment.external_id. So a later enrichment
// pass or rescan resolves the child BY the cascaded id rather than re-auto-matching
// it back to the wrong record.
//
// EVERYTHING THE CASCADE WRITES IS RECORDED AS THE PARENT'S CHOICE
// (store.OriginCascaded), not the child's — a leaf through matchTitle, an Album
// through applyEntityOverride. The child was never looked at; it was mapped under
// the parent's record. Both are durable, and only a Cascade from the SAME parent
// may overwrite them, which is that parent revising its own decision (ADR-0046).
// Writing them as the child's own is what made a second Cascade skip every child
// the first one reached (.../issues/04).
//
// Skip rule (a child's OWN prior correction always wins): a child whose record the
// ADMIN CHOSE ON IT — store.Title.EnrichmentIDOrigin.OwnChoice() for a leaf,
// EntityEnrichment.ExternalIDOrigin.OwnChoice() for an album — OR which carries any
// Locked field is SKIPPED, never clobbered. The skip reads that recorded origin; it
// does NOT infer a choice from "the id column is non-empty", which is what it used
// to do and which excluded auto-matched children from a cascade nobody asked to
// exclude them from (.../issues/03, ADR-0045), and it does not read a record this
// same Cascade wrote as the child's (.../issues/04, ADR-0046).
//
// Best-effort + attention backstop: a child that does not line up — a count/number
// mismatch, a Missing file (a hidden track is simply not enumerated), or no title
// match — is routed to the existing Admin attention list by setting its enrichment
// status to 'unmatched' (so it appears in catalog.TitlesNeedingMatch /
// GET /libraries/{id}/enrichment-attention). The cascade NEVER aborts on partial
// failure; it accumulates a summary instead.

// CascadeSummary is what the Admin sees after a cascade: how many children received
// a durable override (Updated, counted at every grain — a pinned album counts too),
// and how many leaf children were routed to the attention list (Attention).
type CascadeSummary struct {
	Updated   int
	Attention int
}

func (s CascadeSummary) add(o CascadeSummary) CascadeSummary {
	return CascadeSummary{Updated: s.Updated + o.Updated, Attention: s.Attention + o.Attention}
}

// CascadeEntity applies a just-applied parent Enrichment override to the parent's
// children, best-effort, returning a summary. externalID is the authoritative id the
// parent was pinned to (the corrected record). It dispatches by the browse-parent
// kind; a Season (or any childless kind) is a no-op. The caller invokes it AFTER the
// parent override has been applied and its per-Library lock released — each per-child
// re-enrich re-acquires that lock, so calling this while holding it would deadlock.
func (s *Service) CascadeEntity(ctx context.Context, entityType, entityID, externalID string) (CascadeSummary, error) {
	switch entityType {
	case store.EntityAlbum:
		return s.cascadeAlbumTracks(ctx, entityID, externalID)
	case store.EntityArtist:
		return s.cascadeArtistAlbums(ctx, entityID)
	case store.EntityShow:
		return s.cascadeShowEpisodes(ctx, entityID, externalID)
	default:
		return CascadeSummary{}, nil
	}
}

// cascadeShowEpisodes pins the corrected Show id on each of the Show's Episodes and
// re-resolves them positionally (each Episode already carries its own season+episode
// number, which the provider maps under the corrected show). A matched Episode is
// updated; one the corrected show has no record for lands in the attention list. An
// Episode whose record the Admin chose ON IT (enrichment_id_origin = 'chosen') or
// which carries a Locked field is skipped — an Episode this Show's PREVIOUS Cascade
// wrote is not, so re-correcting the Show corrects its Episodes again (ADR-0046).
func (s *Service) cascadeShowEpisodes(ctx context.Context, showID, showExternalID string) (CascadeSummary, error) {
	seasons, err := s.store.SeasonsForShow(showID)
	if err != nil {
		return CascadeSummary{}, err
	}
	var sum CascadeSummary
	for _, se := range seasons {
		eps, err := s.store.EpisodesForSeason(se.ID)
		if err != nil {
			return sum, err
		}
		for _, ep := range eps {
			skip, err := s.childHasOwnOverride(ep)
			if err != nil {
				return sum, err
			}
			if skip {
				continue
			}
			// Pin the corrected show id on the Episode (its own tmdb_id — the durable
			// per-episode anchor collectTVLeaves threads) and re-enrich by its
			// season+episode. Best-effort: a per-child error routes it to attention
			// rather than aborting the whole cascade.
			matched, err := s.reenrichEpisode(ctx, ep, showExternalID)
			if err != nil {
				if serr := s.store.SetTitleEnrichmentStatus(ep.ID, "unmatched"); serr != nil {
					return sum, serr
				}
				sum.Attention++
				continue
			}
			if matched {
				sum.Updated++
			} else {
				sum.Attention++ // the re-enrich left it unmatched/failed (in the attention list)
			}
		}
	}
	return sum, nil
}

// reenrichEpisode pins the corrected show id on an Episode and re-enriches it BY its
// own season+episode number under that show (the positional map). Unlike the leaf
// MatchTitle path — which re-reads the Title without its TV ordering fields — it
// builds the ref from the Episode row the cascade already walked, so the provider
// resolves the right episode. Returns whether the episode settled on a record
// (otherwise the re-enrich left it 'unmatched' → in the attention list). Durable: the
// pinned tmdb_id is honored by the next collectTVLeaves pass.
//
// The pin is recorded as store.OriginCascaded — the SHOW's choice, held by the
// Episode. That keeps it durable against every pass while leaving the Episode
// eligible for its Show's next "apply to children" (ADR-0046).
func (s *Service) reenrichEpisode(ctx context.Context, ep store.Title, showExternalID string) (bool, error) {
	if err := s.store.SetTitleExternalMatch(ep.ID, store.ExternalMatch{TMDBID: showExternalID}, store.OriginCascaded); err != nil {
		return false, err
	}
	// Serialize against a concurrent pass over the same Library (as MatchTitle does).
	lock := s.libLock(ep.LibraryID)
	lock.Lock()
	defer lock.Unlock()

	// Resolve the Episode's Library policy so a switched-off Library records the
	// re-enrich 'disabled' rather than calling out (as the leaf MatchTitle path does).
	snap, err := s.snapshotFor(ctx, ep.LibraryID)
	if err != nil {
		return false, err
	}

	var res Result
	ref := TitleRef{
		Kind: "episode", Title: ep.Title, TMDBID: showExternalID,
		SeasonNumber: ep.SeasonNumber, EpisodeNumber: ep.EpisodeNumber, EpisodeLabel: ep.EpisodeLabel,
	}
	if err := s.processLeaf(ctx, snap, leafWork{title: ep, ref: ref}, &res); err != nil {
		return false, err
	}
	return res.Matched > 0, nil
}

// cascadeAlbumTracks maps the Album's local tracks positionally (disc+track) onto the
// corrected release's tracklist and pins each mapped track a durable recording
// override. Tracks with no positional counterpart (a count/number mismatch) are
// routed to the attention list. The corrected release's tracklist is obtained by
// searching the album kind and matching the candidate whose external id is the one
// the Album was just pinned to (albumExternalID).
func (s *Service) cascadeAlbumTracks(ctx context.Context, albumID, albumExternalID string) (CascadeSummary, error) {
	al, err := s.store.AlbumByID(albumID)
	if err != nil {
		return CascadeSummary{}, err
	}
	cand := s.findAlbumCandidate(ctx, al.Title, 0, albumExternalID)
	return s.mapAlbumTracks(ctx, albumID, cand)
}

// cascadeArtistAlbums maps the Artist's albums by title(+year) onto the corrected
// artist's release-groups, pins each matched album a durable override, and recurses
// into its tracks positionally. An album with no title match has its tracks routed to
// the attention list (the whole album needs a look). An album carrying its own prior
// override/lock is skipped entirely (its tracks too — the child's correction wins).
func (s *Service) cascadeArtistAlbums(ctx context.Context, artistID string) (CascadeSummary, error) {
	albums, err := s.store.AlbumsForArtist(artistID)
	if err != nil {
		return CascadeSummary{}, err
	}
	var sum CascadeSummary
	for _, al := range albums {
		skip, err := s.albumHasOwnOverride(al.ID)
		if err != nil {
			return sum, err
		}
		if skip {
			continue
		}
		cand := s.findAlbumCandidate(ctx, al.Title, al.Year, "")
		if cand == nil {
			// No release-group matched this album by title(+year): route its tracks to
			// the attention list so the Admin can hand-fix them.
			attn, err := s.routeAlbumTracksToAttention(al.ID)
			if err != nil {
				return sum, err
			}
			sum.Attention += attn
			continue
		}
		// A matched album: pin it (durable entity override + re-enrich) and recurse
		// into its tracks positionally.
		// OriginCascaded: the pin is the ARTIST's choice held by the Album. Recording
		// it as the Album's own is what made the next Artist Cascade skip every Album
		// this one pinned — and its Tracks with it, since the recursion never enters a
		// skipped Album (ADR-0046).
		if err := s.applyEntityOverride(ctx, store.EntityAlbum, al.ID, cand.ExternalID, store.OriginCascaded); err != nil {
			attn, aerr := s.routeAlbumTracksToAttention(al.ID)
			if aerr != nil {
				return sum, aerr
			}
			sum.Attention += attn
			continue
		}
		sum.Updated++
		sub, err := s.mapAlbumTracks(ctx, al.ID, cand)
		if err != nil {
			return sum, err
		}
		sum = sum.add(sub)
	}
	return sum, nil
}

// mapAlbumTracks pins each of the Album's local tracks a durable recording override
// from the candidate's positionally-matched tracklist entry; a track with no match is
// routed to attention. A nil candidate (the album could not be resolved) routes every
// non-skipped track to attention. A track with its own prior override/lock is skipped.
func (s *Service) mapAlbumTracks(ctx context.Context, albumID string, cand *Candidate) (CascadeSummary, error) {
	tracks, err := s.store.TracksForAlbum(albumID)
	if err != nil {
		return CascadeSummary{}, err
	}
	byPos := map[[2]int]TrackCandidate{}
	if cand != nil {
		for _, tc := range cand.Tracklist {
			byPos[[2]int{discOrDefault(tc.Disc), tc.Position}] = tc
		}
	}
	var sum CascadeSummary
	for _, tr := range tracks {
		skip, err := s.childHasOwnOverride(tr)
		if err != nil {
			return sum, err
		}
		if skip {
			continue
		}
		tc, ok := byPos[[2]int{discOrDefault(tr.DiscNumber), tr.TrackNumber}]
		if !ok || strings.TrimSpace(tc.ExternalID) == "" {
			// Count/number mismatch (or a tracklist entry that carried no recording id):
			// route to the attention list, don't clobber the track.
			if err := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched"); err != nil {
				return sum, err
			}
			sum.Attention++
			continue
		}
		if err := s.applyOverride(ctx, tr.ID, tc.ExternalID, store.OriginCascaded); err != nil {
			if serr := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched"); serr != nil {
				return sum, serr
			}
			sum.Attention++
			continue
		}
		sum.Updated++
	}
	return sum, nil
}

// routeAlbumTracksToAttention marks every non-skipped track of an album 'unmatched'
// so it surfaces in the attention list — used when the album itself could not be
// mapped (no title match), so the Admin sees each affected track. Returns the count.
func (s *Service) routeAlbumTracksToAttention(albumID string) (int, error) {
	tracks, err := s.store.TracksForAlbum(albumID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, tr := range tracks {
		skip, err := s.childHasOwnOverride(tr)
		if err != nil {
			return n, err
		}
		if skip {
			continue
		}
		if err := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// findAlbumCandidate searches the album kind for a release-group matching a local
// album and returns it, or nil when none lines up. When wantExternalID is set (the
// direct Album cascade, where the album was just pinned to that id) it matches by id;
// otherwise (the artist recursion) it matches by title, confirming the year when both
// sides carry one. A blank query / unavailable provider / no hit is a nil result — the
// cascade then routes the album's tracks to attention (best-effort, never a hard fail).
func (s *Service) findAlbumCandidate(ctx context.Context, title string, year int, wantExternalID string) *Candidate {
	cands, err := s.SearchCandidates(ctx, "album", title, SearchOptions{})
	if err != nil {
		return nil
	}
	for i := range cands {
		c := cands[i]
		if wantExternalID != "" {
			if c.ExternalID == wantExternalID {
				return &c
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(c.Title), strings.TrimSpace(title)) {
			continue
		}
		if year != 0 && c.Year != 0 && c.Year != year {
			continue
		}
		return &c
	}
	return nil
}

// childHasOwnOverride reports whether a leaf child (Episode/Track) should be SKIPPED
// by the cascade because it carries the Admin's OWN prior correction: an Enrichment
// override the Admin chose ON THIS CHILD (EnrichmentIDOrigin.OwnChoice()) or any
// Locked field.
//
// It reads the recorded origin rather than "the record id is non-empty". That older
// guess was wrong in one direction only, and silently: a leaf acquires a record id
// nobody chose all the time — an enrichment pass persists the record it resolved so
// the artwork-candidate lookup has an anchor (store.TitleEnrichment.ExternalIDs,
// fill-only), a split's co-File sibling inherits the survivor's series
// (store.TitleTree.RecordTMDBID), and clearing an Episode pin writes the Show's own
// series back — and every one of those made the child permanently immune to "apply
// to children" with nothing told to the Admin (.../issues/03, ADR-0045).
//
// It asks OwnChoice() rather than Locked() for the same reason one level on: a
// record THIS PARENT'S PREVIOUS CASCADE wrote is durable — nothing re-auto-matches
// it — but it is the parent's choice, not the child's, so it must not exclude the
// child from the parent's next correction. Reading it as the child's own made a
// second "apply to children" report Updated: 0 on exactly the children the first
// one fixed (.../issues/04, ADR-0046). An override the Admin applied DIRECTLY to
// the child still wins, which is the promise the rule exists for.
//
// The origin is namespace-neutral, so a Track's musicbrainz_id is covered by the
// same test as an Episode's record. The id itself is deliberately NOT re-checked
// alongside it (as albumHasOwnOverride checks external_id, where the two columns
// are independent): the origin is only ever set together with an id, and a leaf's
// record spans two namespaces, so an imdb-only override would read as unchosen if
// this insisted on a TMDB id being present — the same guess again in a smaller hat.
//
// On a library that predates migration 0050 EVERY row that had a record was
// backfilled locked, and 0051 reads every such lock as 'chosen' — after the fact
// nothing could tell an Admin's pick from an id a pass echoed back, or from a
// Cascade's own write — so those rows keep the old behaviour (skipped) rather than
// being retroactively demoted into something a Cascade may overwrite. The
// improvement applies to every record written since the upgrade.
func (s *Service) childHasOwnOverride(t store.Title) (bool, error) {
	if t.EnrichmentIDOrigin.OwnChoice() {
		return true, nil
	}
	locks, err := s.store.LockedFields(t.ID)
	if err != nil {
		return false, err
	}
	return len(locks) > 0, nil
}

// albumHasOwnOverride reports whether an Album child (under an Artist cascade) should
// be SKIPPED because it carries a durable Enrichment override the Admin chose ON THE
// ALBUM (ExternalIDOrigin.OwnChoice()) or any Locked field.
//
// The parent recursion has the leaf rule's shape and had its defect: an Artist
// cascade pins each mapped Album through SetEntityExternalMatch, so before ADR-0046
// every Album the first run reached read back as carrying "its own" override and the
// second run skipped it — and skipped its Tracks too, since a skipped Album is never
// recursed into, which makes the blast radius larger here than at the leaf. Asking
// OwnChoice() lets an Artist re-correct its discography while an Album the Admin
// fixed by hand still wins (.../issues/04).
//
// The external_id is still checked alongside the origin, unlike the leaf rule: here
// the two columns really are independent (a row can be written by a pass with an
// origin left empty, or carry an origin with no id), and a parent has exactly one
// Authoritative provider, so there is no second namespace for the check to misread.
func (s *Service) albumHasOwnOverride(albumID string) (bool, error) {
	e, err := s.store.EntityEnrichmentByID(store.EntityAlbum, albumID)
	if err != nil {
		return false, err
	}
	if e.ExternalIDOrigin.OwnChoice() && strings.TrimSpace(e.ExternalID) != "" {
		return true, nil
	}
	locks, err := s.store.EntityLockedFields(store.EntityAlbum, albumID)
	if err != nil {
		return false, err
	}
	return len(locks) > 0, nil
}

// discOrDefault normalizes a disc number so a single-disc album (whose local tracks
// or the provider's tracklist may report disc 0) maps against disc 1.
func discOrDefault(disc int) int {
	if disc <= 0 {
		return 1
	}
	return disc
}
