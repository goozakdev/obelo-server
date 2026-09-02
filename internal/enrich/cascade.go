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
//   - Show → episodes map POSITIONALLY (season+episode number).
//   - Album → tracks map by the shared ADR-0050 rule (mapTracks, track_match.go):
//     position AND title, then title anywhere, then the one leftover pair, then
//     decline — never position alone.
//   - Artist → albums map by TITLE(+year) against the corrected artist's
//     release-groups (obtained by searching the album kind and matching), then
//     RECURSE into each matched album's tracks by that same rule.
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
				// An Episode, so none of ADR-0050's five reasons applies (they are
				// Music-shaped); the empty reason also clears any prior one.
				if serr := s.store.SetTitleEnrichmentStatus(ep.ID, "unmatched", store.EnrichmentReasonNone); serr != nil {
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
		// EntityPin with no ReleaseID: an Artist Cascade names a release-GROUP per
		// album and never an edition, so it also clears one — correctly, since the
		// album it is re-pointing may not be the album whose edition was chosen. The
		// skip above means it never reaches an album whose record the Admin chose ON
		// IT, which is the only way an edition can be stored (ADR-0046/0052).
		if err := s.applyEntityOverride(ctx, store.EntityAlbum, al.ID,
			EntityPin{ExternalID: cand.ExternalID}, store.OriginCascaded); err != nil {
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
// from the tracklist entry the ADR-0050 match rule pairs it with; a track with no
// match is routed to attention. A nil candidate (the album could not be resolved)
// routes every non-skipped track to attention. A track with its own prior
// override/lock is skipped.
//
// The mapping is mapTracks — position-and-title, then title, then the leftover
// pair, then position alone ONLY under a chosen edition (ADR-0052), then decline.
// It replaced the unconditional position-only map this function used to carry,
// which pinned the wrong recording to every track after a drift in a hand-numbered
// album's numbering. A cascade therefore pins FEWER tracks than it once did on a
// mis-numbered album nobody pinned an edition on, and queues the rest; the ones it
// stops pinning are the ones it was getting wrong.
//
// Every track goes into mapTracks, including the ones this cascade will skip: a
// skipped track still holds its position on the release, so leaving it out would
// free its entry for a neighbour and let the leftover rule fire on a hole that is
// not one. The skip is applied after, on the result.
func (s *Service) mapAlbumTracks(ctx context.Context, albumID string, cand *Candidate) (CascadeSummary, error) {
	tracks, err := s.store.TracksForAlbum(albumID)
	if err != nil {
		return CascadeSummary{}, err
	}
	var tracklist []TrackCandidate
	if cand != nil {
		tracklist = cand.Tracklist
	}
	// ADR-0052: when an Admin has named the EDITION, the cascade decorates from that
	// edition's tracklist rather than from the candidate's preview — which is the
	// first release MusicBrainz happens to list (releaseGroupTracklist, limit=1) and
	// is a wrong answer for every position after a deluxe edition's first bonus
	// track. chosenEdition is the licence issue 11 reads here, the twin of the pass's
	// res.FromChosenEdition; the two paths must agree on the rule AND on the licence.
	//
	// An album with no chosen edition takes none of this and behaves exactly as
	// before: the cascade already had a tracklist in hand and does not start paying a
	// call per album to re-fetch what it was given.
	fromChosenEdition := false
	if cand != nil {
		if res, ok := s.chosenEditionTracklist(ctx, albumID, cand.ExternalID, len(tracks)); ok {
			// res.FromChosenEdition is the flag rule 4 is gated on, the twin of the pass's
			// (ADR-0052). ok says only that a pin was READ; the licence says the pin
			// ANSWERED. A read that fell back to the tag release or to fit comes back ok
			// with the licence withheld, and the cascade maps it exactly as it maps an
			// album nobody pinned.
			tracklist, fromChosenEdition = res.Tracks, res.FromChosenEdition
		}
	}
	// One rule, both callers, licence included: the cascade and the pass must not
	// differ on which tracks an album resolves (ADR-0050, ADR-0052).
	matched := mapTracks(tracks, tracklist, fromChosenEdition)
	// The same two reasons the PASS writes, decided the same way and from the same
	// fact (ADR-0050): with no candidate the Album named none of its contents, which
	// is the Admin's problem with the Album; with one, a tracklist was read and this
	// Track was declined by the match rule, so the Album is probably the wrong
	// release. One rule, both callers — the cascade must not invent a third
	// vocabulary for the queue it fills.
	declineReason := store.EnrichmentReasonNotInTracklist
	if cand == nil {
		declineReason = store.EnrichmentReasonAlbumUnmatched
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
		tc, ok := matched[tr.ID]
		if !ok || strings.TrimSpace(tc.ExternalID) == "" {
			// The rule declined (no position+title, no unique title, no clean leftover
			// pair), or the entry it paired with carried no recording id: route to the
			// attention list, don't clobber the track.
			if err := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched", declineReason); err != nil {
				return sum, err
			}
			sum.Attention++
			continue
		}
		if err := s.applyOverride(ctx, tr.ID, tc.ExternalID, store.OriginCascaded); err != nil {
			// The mapping SUCCEEDED and pinning it failed — a provider or store error,
			// not a statement about this Track. None of the five fits, so the empty
			// reason gives the generic sentence rather than a confident wrong one.
			if serr := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched", store.EnrichmentReasonNone); serr != nil {
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
//
// Every row it writes is EnrichmentReasonAlbumUnmatched, which is precisely true
// here and is the whole point of the value: the Album is the thing that could not
// be resolved, so a per-track recording search is the wrong offer to put in front
// of the Admin (ADR-0050).
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
		if err := s.store.SetTitleEnrichmentStatus(tr.ID, "unmatched",
			store.EnrichmentReasonAlbumUnmatched); err != nil {
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

// chosenEditionTracklist resolves the tracklist of the EDITION an Admin named for
// this Album, for the cascade path (ADR-0052). ok is false — and the caller keeps the
// candidate's own preview tracklist — whenever there is nothing better to offer: no
// edition was chosen, the edition was chosen under a different release-group than the
// one being applied, or the read failed. A cascade is best-effort and must never fail
// on a decoration read.
//
// The release-group it reads under is releaseGroupID, the record the cascade is
// applying, NOT whatever the album's row happens to hold: an edition chosen under
// some other release-group is a stranger to this correction and is ignored, exactly
// as the pass's chosenAlbumEdition ignores it.
//
// This is the second of the two places ADR-0052's precedence is resolved. Both go
// through albumTracklistFor, so the cascade and the pass cannot drift on which
// edition decorates an album, nor on whether a human chose it.
func (s *Service) chosenEditionTracklist(ctx context.Context, albumID, releaseGroupID string,
	localCount int) (albumTracklistResult, bool) {

	rgID := strings.TrimSpace(releaseGroupID)
	chosen := s.chosenAlbumEdition(store.EntityAlbum, albumID, rgID)
	if rgID == "" || chosen == "" {
		return albumTracklistResult{}, false
	}
	al, err := s.store.AlbumByID(albumID)
	if err != nil {
		return albumTracklistResult{}, false
	}
	res, err := s.albumTracklistFor(ctx, s.snapshot(), TracklistRequest{
		ReleaseGroupID:  rgID,
		ReleaseID:       al.MusicbrainzReleaseID,
		LocalTrackCount: localCount,
	}, chosen)
	if err != nil {
		return albumTracklistResult{}, false
	}
	return res, true
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
