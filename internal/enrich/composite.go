package enrich

import "context"

// CompositeProvider routes a lookup to the right source by media kind: video
// kinds (movie/show/season/episode) go to Video (TMDB); music kinds
// (artist/album/track) go to Music (MusicBrainz). It is the single
// MetadataProvider app.New wires, so the rest of the system sees one seam while
// each kind reaches its natural source. A nil sub-provider yields ErrNoMatch for
// its kinds (e.g. a server with only a TMDB key still enriches video).
type CompositeProvider struct {
	Video MetadataProvider
	Music MetadataProvider
}

// Lookup dispatches by ref.Kind. Unknown kinds return ErrNoMatch.
func (c CompositeProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "show", "season", "episode":
		if c.Video == nil {
			return TitleMetadata{}, ErrNoMatch
		}
		return c.Video.Lookup(ctx, ref)
	case "artist", "album", "track":
		if c.Music == nil {
			return TitleMetadata{}, ErrNoMatch
		}
		return c.Music.Lookup(ctx, ref)
	default:
		return TitleMetadata{}, ErrNoMatch
	}
}

// Search dispatches by kind to the authoritative sub-provider (video → TMDB,
// music → MusicBrainz), mirroring Lookup. A nil sub-provider is an unconfigured
// kind, so it returns ErrSearchUnavailable (the Edit-item box reports why); an
// unknown kind is likewise unavailable rather than a silent empty result.
func (c CompositeProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	switch kind {
	case "movie", "show", "season", "episode":
		if c.Video == nil {
			return nil, ErrSearchUnavailable
		}
		return c.Video.Search(ctx, kind, query, opts)
	case "artist", "album", "track":
		if c.Music == nil {
			return nil, ErrSearchUnavailable
		}
		return c.Music.Search(ctx, kind, query, opts)
	default:
		return nil, ErrSearchUnavailable
	}
}

// ArtworkCandidates dispatches by ref.Kind to the authoritative sub-provider
// (video → TMDB, music → MusicBrainz), mirroring Search: a nil sub-provider or an
// unknown kind is unavailable (ErrSearchUnavailable) rather than a silent empty.
func (c CompositeProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	switch ref.Kind {
	case "movie", "show", "season", "episode":
		if c.Video == nil {
			return nil, ErrSearchUnavailable
		}
		return c.Video.ArtworkCandidates(ctx, ref, role)
	case "artist", "album", "track":
		if c.Music == nil {
			return nil, ErrSearchUnavailable
		}
		return c.Music.ArtworkCandidates(ctx, ref, role)
	default:
		return nil, ErrSearchUnavailable
	}
}
