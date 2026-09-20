package enrich

import (
	"context"
	"errors"
	"log"
)

// MusicChainProvider is the MetadataProvider wired into CompositeProvider.Music
// when a music Library composes its Authoritative provider plus at least one
// active music Supplement. It is VideoChainProvider's shape (ADR-0061): the lead
// runs first, then every Supplement in REGISTRATION order fills what the lead left
// empty, through the one rule both chains share (fillFromSupplement). It names no
// source, and it composes however many are active — a third-party music Supplement
// is composed exactly as the two shipped ones are.
//
// Each Supplement self-gates by no-match, which is where the behaviour that used
// to be hardwired by name now lives: fanart.tv answers only an artist, and only
// one keyed by a MusicBrainz id; TheAudioDB answers an artist (by MBID or by name)
// and a track. So for the shipped pair the composition reads as it always did —
//
//   - an artist's poster is fanart.tv's when it has one (it registers first, and
//     fill-only keeps the first image per role), else TheAudioDB's;
//   - an artist's Overview is TheAudioDB's real biography in place of MusicBrainz's
//     synthesized one, because MusicBrainz DECLARES its artist Overview synthesized
//     (MetadataRecord.OverviewSynthesized) and a synthesized Overview fills like an
//     empty one;
//   - a track's synopsis is TheAudioDB's, since MusicBrainz gives a recording none.
//
// Every Supplement is asked with the caller's ref PLUS the id the lead just
// resolved, in the lead's own namespace (ADR-0060), so an id-keyed source keys by
// the answer rather than by the file's tag.
//
// Identity, the canonical title and the candidate list are the lead's (ADR-0002).
// Lookups are best-effort (ADR-0001): a no-match is the normal "nothing here", and
// a Supplement error or timeout is logged and swallowed so the entity keeps the
// lead's metadata and the pass continues.
type MusicChainProvider struct {
	// Authoritative is the lead — MusicBrainz unless a Library repointed its
	// Enrichment policy at another Full music provider (ADR-0027). It owns identity,
	// the canonical title, the candidate list, an album's tracklist and editions,
	// and the id an Admin pastes.
	Authoritative MetadataProvider
	Supplements   []MetadataProvider // fill-only, applied in order (registration order)
}

// NewMusicChainProvider builds a chain over an authoritative music provider and a
// set of optional fill-only Supplements, in the order they fill. With none it is a
// pass-through to the lead.
func NewMusicChainProvider(authoritative MetadataProvider, supplements ...MetadataProvider) *MusicChainProvider {
	return &MusicChainProvider{Authoritative: authoritative, Supplements: supplements}
}

// Search delegates to the authoritative source: the fill-only Supplements never
// own a candidate list — they only decorate a record already pinned by id
// (ADR-0019). So the chain's Search is exactly the lead's.
func (p *MusicChainProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	return p.Authoritative.Search(ctx, kind, query, opts)
}

// AlbumTracklist forwards the optional AlbumTracklister capability to the
// AUTHORITATIVE source only. A fill-only Supplement inventing an ordering for an
// album the lead already numbers would be exactly the fill-only rule broken. An authoritative source that can't answer yields ErrNoTracklist.
func (p *MusicChainProvider) AlbumTracklist(ctx context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	lister, ok := p.Authoritative.(AlbumTracklister)
	if !ok {
		return nil, ErrNoTracklist
	}
	return lister.AlbumTracklist(ctx, req)
}

// ReleaseGroupEditions forwards the optional AlbumEditionLister capability to the
// AUTHORITATIVE source only, for AlbumTracklist's reason: an album's editions are
// the lead's catalogue, and a Supplement listing editions would be the fill-only
// rule broken (ADR-0002). A source that cannot
// answer yields ErrSearchUnavailable, which the picker degrades on.
func (p *MusicChainProvider) ReleaseGroupEditions(ctx context.Context, releaseGroupID string) ([]ReleaseEdition, error) {
	lister, ok := p.Authoritative.(AlbumEditionLister)
	if !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.ReleaseGroupEditions(ctx, releaseGroupID)
}

// ParseExternalRef forwards the optional ExternalRefParser capability to the
// AUTHORITATIVE source only, for AlbumTracklist's reason: the id an Admin pastes
// is the id this chain PINS, and that is the lead's namespace. A fill-only
// Supplement reading a paste would be offering an id nothing stores. A source
// that cannot read one answers ErrSearchUnavailable, which the host takes as
// permission to read it itself.
func (p *MusicChainProvider) ParseExternalRef(ctx context.Context, kind, pasted string) (ExternalRef, error) {
	parser, ok := p.Authoritative.(ExternalRefParser)
	if !ok {
		return ExternalRef{}, ErrSearchUnavailable
	}
	return parser.ParseExternalRef(ctx, kind, pasted)
}

// ArtworkCandidates lists the images the Edit-item picker offers for a Music role:
// the lead's first, then every Supplement's in registration order, de-duplicated
// by URL (the service caps the count). For an album that is MusicBrainz's Cover
// Art Archive covers; for an artist, where MusicBrainz has none, it is fanart.tv's
// artistthumb[] leading the grid and TheAudioDB's thumb after it
// (artwork-management/02) — and a third Supplement's images after those.
//
// Every source is best-effort (ADR-0001): one that has nothing to key by, serves
// another kind, or fails is skipped (a genuine failure is logged), so the picker
// degrades to whatever is available. The LEAD's error is returned only when no
// source produced a candidate, which keeps an album picker's answer exactly the
// lead's when the Supplements have nothing for albums.
func (p *MusicChainProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	var cands []ArtworkCandidate
	seen := make(map[string]bool)
	add := func(got []ArtworkCandidate) {
		for _, c := range got {
			if c.URL == "" || seen[c.URL] {
				continue
			}
			seen[c.URL] = true
			cands = append(cands, c)
		}
	}
	lead, leadErr := p.Authoritative.ArtworkCandidates(ctx, ref, role)
	add(lead)
	for _, s := range p.Supplements {
		if s == nil {
			continue
		}
		got, err := s.ArtworkCandidates(ctx, ref, role)
		if err != nil {
			// A no-match / unsearchable source is the normal "no images here" outcome;
			// a genuine failure is logged and treated as no data (ADR-0001).
			if !errors.Is(err, ErrNoMatch) && !errors.Is(err, ErrSearchUnavailable) {
				log.Printf("obelo: enrich %s artwork candidates (title %q): %v", ref.Kind, ref.Title, err)
			}
			continue
		}
		add(got)
	}
	if len(cands) == 0 && leadErr != nil {
		return nil, leadErr
	}
	return cands, nil
}

// Lookup resolves ref via the lead, then fills what it left empty from each
// Supplement in order. A lead no-match/error is the chain's result.
func (p *MusicChainProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	meta, err := p.Authoritative.Lookup(ctx, ref)
	if err != nil {
		return meta, err // a lead no-match/error is the chain's result
	}
	if len(p.Supplements) == 0 || !worthDecorating(ref, meta) {
		return meta, nil
	}
	// The lead's answer keys the Supplements: its resolved id in its own namespace
	// (an MBID, for MusicBrainz), on top of whatever the caller's ref carried.
	sref := withExternalID(ref, meta.Source, meta.ExternalID)
	for _, s := range p.Supplements {
		if s == nil {
			continue
		}
		if sup, ok := p.lookup(ctx, s, sref); ok {
			meta = fillFromSupplement(meta, sup)
		}
	}
	return meta, nil
}

// worthDecorating reports whether a record is one the host will keep, so the chain
// does not spend a fill-only request on an answer that is about to be discarded.
//
// The chain is HOST code, not a Plugin, which is what makes this legitimate:
// nothing here judges on a source's behalf (ADR-0057 decision 3 is about the
// source not judging itself), and the judgement is the host's one implementation —
// acceptsTitle, the same function acceptSearchHit applies a moment later and the
// same localTitleForAcceptance choosing which field holds the local title. It is
// asked here as a PRECONDITION, not as a verdict: the verdict is still the
// service's, and a record this declines is still returned to the service, which
// rejects it and writes `search-rejected` exactly as before.
//
// Without it a library of hundreds of rejected tracks paid hundreds of TheAudioDB
// requests for synopses that were thrown away with the records they decorated
// (issue 01's third deviation). Gating on FromSearch alone would have been the
// wrong fix — it strips the synopsis from ACCEPTED search hits too, which is a
// real regression — and a record resolved BY ID is never judged at all, so it is
// decorated as it always was. It gates every music kind, since the service judges
// every music kind's search hit by the same test.
func worthDecorating(ref TitleRef, meta TitleMetadata) bool {
	if !meta.Matched || !meta.FromSearch {
		return true
	}
	local, judged := localTitleForAcceptance(ref)
	if !judged {
		return true
	}
	return acceptsTitle(local, meta.Name)
}

// lookup runs one Supplement, returning its result and whether it should be
// merged. A no-match is the normal "no data for this entity" outcome (ok=false,
// no log); a genuine failure is non-fatal — it is logged and treated as no data so
// the lead's result is preserved and the pass continues (ADR-0001).
func (p *MusicChainProvider) lookup(ctx context.Context, src MetadataProvider, ref TitleRef) (TitleMetadata, bool) {
	meta, err := src.Lookup(ctx, ref)
	if err != nil {
		if !errors.Is(err, ErrNoMatch) {
			log.Printf("obelo: enrich %s music supplement (title %q): %v", ref.Kind, ref.Title, err)
		}
		return TitleMetadata{}, false
	}
	return meta, true
}
