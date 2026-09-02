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

// SeriesSeasons / SeasonEpisodes forward the optional EpisodeLister capability to
// the VIDEO sub-provider — an episode list is a video notion, and the music side
// has no analogue. A sub-provider that doesn't implement it (a build with a
// non-TMDB authoritative source, or no video provider at all) yields
// ErrSearchUnavailable, so the picker reports "no episode list here" instead of
// hanging or pretending. This is the same graceful degradation the rest of
// enrichment uses (ADR-0001).
func (c CompositeProvider) SeriesSeasons(ctx context.Context, showID string) ([]SeasonSummary, error) {
	lister, ok := c.Video.(EpisodeLister)
	if c.Video == nil || !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.SeriesSeasons(ctx, showID)
}

func (c CompositeProvider) SeasonEpisodes(ctx context.Context, showID string, season int) ([]EpisodeCandidate, error) {
	lister, ok := c.Video.(EpisodeLister)
	if c.Video == nil || !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.SeasonEpisodes(ctx, showID, season)
}

// AlbumTracklist forwards the optional AlbumTracklister capability to the MUSIC
// sub-provider — an album's tracklist is a music notion, and the video side has no
// analogue. A build with no music provider (or one that can't list a tracklist)
// answers ErrNoTracklist, which is the same thing the caller does with an album
// that has none: the tracks fall through to the tiers below (ADR-0050).
func (c CompositeProvider) AlbumTracklist(ctx context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	lister, ok := c.Music.(AlbumTracklister)
	if c.Music == nil || !ok {
		return nil, ErrNoTracklist
	}
	return lister.AlbumTracklist(ctx, req)
}

// ReleaseGroupEditions forwards the optional AlbumEditionLister capability to the
// MUSIC sub-provider, for the same reason AlbumTracklist does. A build with no
// music provider (or one that cannot list editions) answers ErrSearchUnavailable —
// the picker's "not now", which degrades to the pasted-URL escape hatch instead of
// an error page (ADR-0052).
func (c CompositeProvider) ReleaseGroupEditions(ctx context.Context, releaseGroupID string) ([]ReleaseEdition, error) {
	lister, ok := c.Music.(AlbumEditionLister)
	if c.Music == nil || !ok {
		return nil, ErrSearchUnavailable
	}
	return lister.ReleaseGroupEditions(ctx, releaseGroupID)
}
