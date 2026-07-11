package enrich

import (
	"context"
	"errors"
	"log"
)

// VideoChainProvider is the MetadataProvider wired into CompositeProvider.Video
// when a video Library composes an Authoritative provider plus at least one
// fill-only supplement. The Authoritative provider (TMDB by default, or whatever
// Full provider a Library's Enrichment policy points at — ADR-0027) leads the video
// kinds (movie/show/season/episode); the fill-only supplements add only what it
// left empty — a text field only when the authoritative result carried none, and
// artwork only for a role not already present (reusing mergeArtwork). It mirrors
// MusicChainProvider so the fill-only contract, swallow-and-continue error handling,
// and artwork role-merge are identical across kinds.
//
// It runs the authoritative source first, then composes each supplement in order
// over the result. Each supplement self-gates by kind (a no-match for a kind it
// doesn't serve): OMDb serves the Movie kind, TheTVDB the TV kinds
// (show/season/episode). Every video kind is offered to the supplements; a
// supplement that doesn't serve the kind no-matches and contributes nothing.
// Identity and — for a Movie/Show — the canonical title always stay the
// authoritative source's (ADR-0002). An episode Name a supplement contributes is
// applied as a DISPLAY-ONLY override (only when the authoritative left it empty),
// exactly the rule the music chain uses for a sparse track — never identity.
//
// Lookups are best-effort (ADR-0001): a supplement no-match is the normal "no data
// for this entity" outcome, and a supplement error/timeout is logged and swallowed
// so the entity keeps its authoritative metadata and the pass continues.
type VideoChainProvider struct {
	Authoritative MetadataProvider   // the Full provider that leads (TMDB by default)
	Supplements   []MetadataProvider // fill-only supplements, applied in order (e.g. OMDb)
}

// NewVideoChainProvider builds a chain over an authoritative Full video provider and
// a set of optional fill-only supplements. With no supplements the chain is a
// pass-through to the authoritative source.
func NewVideoChainProvider(authoritative MetadataProvider, supplements ...MetadataProvider) *VideoChainProvider {
	return &VideoChainProvider{Authoritative: authoritative, Supplements: supplements}
}

// Search delegates to the authoritative source: the fill-only supplements
// (OMDb/TheTVDB/fanart.tv) never own a candidate list — they only decorate a
// record already pinned by id (ADR-0019). So the chain's Search is exactly the
// authoritative source's.
func (p *VideoChainProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	return p.Authoritative.Search(ctx, kind, query, opts)
}

// ArtworkCandidates delegates to the authoritative source: the fill-only
// supplements never own an image list to choose from (they only fill a role the
// authoritative left empty), so the chain's picker is exactly the authoritative
// source's (ADR-0019).
func (p *VideoChainProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	return p.Authoritative.ArtworkCandidates(ctx, ref, role)
}

// Lookup resolves ref via the authoritative source, then fills empty fields from
// each configured supplement (fill-only). An authoritative no-match/error is the
// chain's result.
func (p *VideoChainProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	meta, err := p.Authoritative.Lookup(ctx, ref)
	if err != nil {
		return meta, err // an authoritative no-match/error is the chain's result
	}
	// Only the video kinds carry supplements; the music kinds never reach this chain.
	switch ref.Kind {
	case "movie", "show", "season", "episode":
	default:
		return meta, nil
	}
	for _, s := range p.Supplements {
		if s == nil {
			continue
		}
		sup, ok := p.lookup(ctx, s, ref)
		if !ok {
			continue
		}
		// Fill-only: a text field is taken only when TMDB left it empty; artwork is
		// merged only for a role TMDB didn't already carry; identity is never touched.
		// A supplied Name is a display-only override taken only when TMDB left it
		// empty (an episode/show canonical title) — never identity (ADR-0002).
		if meta.Name == "" {
			meta.Name = sup.Name
		}
		if meta.Overview == "" {
			meta.Overview = sup.Overview
		}
		if meta.ContentRating == "" {
			meta.ContentRating = sup.ContentRating
		}
		if len(meta.Genres) == 0 {
			meta.Genres = sup.Genres
		}
		meta.Artwork = mergeArtwork(meta.Artwork, sup.Artwork)
	}
	return meta, nil
}

// lookup runs one supplement, returning its result and whether it should be
// merged. A no-match is the normal "no data for this entity" outcome (ok=false, no
// log); a genuine failure is non-fatal — it is logged and treated as no data so the
// TMDB result is preserved and the pass continues (ADR-0001).
func (p *VideoChainProvider) lookup(ctx context.Context, src MetadataProvider, ref TitleRef) (TitleMetadata, bool) {
	meta, err := src.Lookup(ctx, ref)
	if err != nil {
		if !errors.Is(err, ErrNoMatch) {
			log.Printf("juicebox: enrich %s video supplement (title %q): %v", ref.Kind, ref.Title, err)
		}
		return TitleMetadata{}, false
	}
	return meta, true
}
