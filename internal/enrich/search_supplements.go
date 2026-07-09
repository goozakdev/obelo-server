package enrich

import "context"

// Search stubs for the fill-only supplements. Only the authoritative source per
// kind is ever searched for an Enrichment-override candidate list (ADR-0019): the
// supplements decorate a record already pinned by the authoritative id, so they
// have no candidate list to offer. They satisfy the MetadataProvider interface by
// reporting ErrSearchUnavailable — a defensive value never reached in practice,
// since the Composite routes search to the authoritative source and the chains
// delegate Search to their authoritative inner provider (TMDB / MusicBrainz).

// Search reports that OMDb is not a searchable authoritative source.
func (p *OMDbProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// Search reports that TheTVDB is not a searchable authoritative source.
func (p *TheTVDBProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// Search reports that fanart.tv is not a searchable authoritative source.
func (p *FanartTVProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// Search reports that TheAudioDB is not a searchable authoritative source.
func (p *TheAudioDBProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// ArtworkCandidates stubs for the video-only fill-only supplements: neither OMDb
// nor TheTVDB owns a listable image set for the Edit-item picker (they only fill a
// role the authoritative source left empty, ADR-0019), so each reports
// ErrSearchUnavailable — a defensive value never reached in practice, since the
// Composite routes the picker to the authoritative source and the video chain
// delegates to TMDB. (The music supplements fanart.tv/TheAudioDB DO own a listable
// artist-photo set — artwork-management/02 — so their ArtworkCandidates live with
// the rest of their provider logic, not here.)

// ArtworkCandidates reports that OMDb owns no listable image set.
func (p *OMDbProvider) ArtworkCandidates(_ context.Context, _ TitleRef, _ string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}

// ArtworkCandidates reports that TheTVDB owns no listable image set.
func (p *TheTVDBProvider) ArtworkCandidates(_ context.Context, _ TitleRef, _ string) ([]ArtworkCandidate, error) {
	return nil, ErrSearchUnavailable
}
