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
// artwork only for a role not already present. It shares fillFromSupplement with
// MusicChainProvider, so the fill-only contract, swallow-and-continue error
// handling, and artwork role-merge are identical across kinds (ADR-0061).
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
		// Fill-only, by the rule the music chain shares: a field is taken only when the
		// lead left it empty (or synthesized), artwork only for a role it didn't carry,
		// and identity is never touched (ADR-0002).
		meta = fillFromSupplement(meta, sup)
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
			log.Printf("obelo: enrich %s video supplement (title %q): %v", ref.Kind, ref.Title, err)
		}
		return TitleMetadata{}, false
	}
	return meta, true
}

// SeriesSeasons / SeasonEpisodes forward the optional EpisodeLister capability to
// the AUTHORITATIVE source only. The fill-only supplements never own a list — they
// decorate a record the authoritative source chose (ADR-0027) — so an episode list
// comes from the same place the episode lookup will. An authoritative source that
// can't list episodes yields ErrSearchUnavailable rather than a partial answer.
func (v *VideoChainProvider) SeriesSeasons(ctx context.Context, showID string) ([]SeasonSummary, error) {
	lister, ok := v.Authoritative.(EpisodeLister)
	if !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.SeriesSeasons(ctx, showID)
}

func (v *VideoChainProvider) SeasonEpisodes(ctx context.Context, showID string, season int) ([]EpisodeCandidate, error) {
	lister, ok := v.Authoritative.(EpisodeLister)
	if !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.SeasonEpisodes(ctx, showID, season)
}

// ParseExternalRef forwards the optional ExternalRefParser capability to the
// AUTHORITATIVE source only, for the reason above: the id an Admin pastes is the id
// this chain PINS, and that is the leader's namespace. None of the video Built-ins
// declares the capability today, so this answers ErrSearchUnavailable and the host
// reads a TMDB paste itself (Service.externalRef) exactly as it always has — the
// forwarding is here so that a video Plugin which DOES declare it is reached
// without the composition changing.
func (v *VideoChainProvider) ParseExternalRef(ctx context.Context, kind, pasted string) (ExternalRef, error) {
	parser, ok := v.Authoritative.(ExternalRefParser)
	if !ok {
		return ExternalRef{}, ErrSearchUnavailable
	}
	return parser.ParseExternalRef(ctx, kind, pasted)
}
