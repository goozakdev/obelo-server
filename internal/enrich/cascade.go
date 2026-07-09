package enrich

import (
	"context"
	"strings"

	"github.com/marioquake/juicebox/internal/store"
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
// pass), a mapped album gets entity_enrichment.external_id_locked. So a later
// enrichment pass or rescan resolves the child BY the cascaded id rather than
// re-auto-matching it back to the wrong record.
//
// Skip rule (a child's OWN prior correction always wins): a child that already
// carries its own Enrichment override (a non-empty pinned external id — a track's
// musicbrainz_id / an episode's tmdb_id, or an album's locked external_id) OR any
// Locked field is SKIPPED, never clobbered. A normal enrichment pass never writes
// those external-id columns (WriteTitleEnrichment leaves them untouched), so a
// non-empty id reliably means an explicit prior override rather than an auto-match.
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
// Episode with its own prior override (a pinned tmdb_id) or a Locked field is skipped.
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
			skip, err := s.childHasOwnOverride(ep.ID, ep.TMDBID)
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
func (s *Service) reenrichEpisode(ctx context.Context, ep store.Title, showExternalID string) (bool, error) {
	if err := s.store.SetTitleExternalMatch(ep.ID, store.ExternalMatch{TMDBID: showExternalID}); err != nil {
		return false, err
	}
	// Serialize against a concurrent pass over the same Library (as MatchTitle does).
	lock := s.libLock(ep.LibraryID)
	lock.Lock()
	defer lock.Unlock()

	var res Result
	ref := TitleRef{
		Kind: "episode", Title: ep.Title, TMDBID: showExternalID,
		SeasonNumber: ep.SeasonNumber, EpisodeNumber: ep.EpisodeNumber, EpisodeLabel: ep.EpisodeLabel,
	}
	if err := s.processLeaf(ctx, leafWork{title: ep, ref: ref}, &res); err != nil {
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
		if err := s.ApplyEntityOverride(ctx, store.EntityAlbum, al.ID, cand.ExternalID); err != nil {
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
		skip, err := s.childHasOwnOverride(tr.ID, tr.MusicbrainzID)
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
		if err := s.ApplyOverride(ctx, tr.ID, tc.ExternalID); err != nil {
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
		skip, err := s.childHasOwnOverride(tr.ID, tr.MusicbrainzID)
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
// by the cascade because it carries its OWN prior Enrichment override — a non-empty
// pinned external id (a normal pass never writes these columns) — or any Locked field.
func (s *Service) childHasOwnOverride(titleID, pinnedExternalID string) (bool, error) {
	if strings.TrimSpace(pinnedExternalID) != "" {
		return true, nil
	}
	locks, err := s.store.LockedFields(titleID)
	if err != nil {
		return false, err
	}
	return len(locks) > 0, nil
}

// albumHasOwnOverride reports whether an Album child (under an Artist cascade) should
// be SKIPPED because it carries its own durable Enrichment override (a locked
// external_id) or any Locked field.
func (s *Service) albumHasOwnOverride(albumID string) (bool, error) {
	e, err := s.store.EntityEnrichmentByID(store.EntityAlbum, albumID)
	if err != nil {
		return false, err
	}
	if e.ExternalIDLocked && strings.TrimSpace(e.ExternalID) != "" {
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
