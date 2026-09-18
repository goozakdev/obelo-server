package enrich

import "context"

// Search stubs for the fill-only supplements. Only the authoritative source per
// kind is ever searched for an Enrichment-override candidate list (ADR-0019): the
// supplements decorate a record already pinned by the authoritative id, so they
// have no candidate list to offer. They satisfy the MetadataProvider interface by
// reporting ErrSearchUnavailable — a defensive value never reached in practice,
// since the Composite routes search to the authoritative source and the chains
// delegate Search to their authoritative inner provider (TMDB / MusicBrainz).

// Search reports that fanart.tv is not a searchable authoritative source.
func (p *FanartTVProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// Search reports that TheAudioDB is not a searchable authoritative source.
func (p *TheAudioDBProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// The video-only fill-only supplements used to need the same pair of stubs here —
// neither OMDb nor TheTVDB owns a candidate list or a listable image set
// (ADR-0019). They are Bundled plugins now (.scratch/bundled-plugins: issue 05),
// and a plugin says the same thing by DECLARING neither capability in its
// manifest: the host consults the declaration before it calls, so the call an
// undeclared capability would have answered is never made at all. (The music
// supplements fanart.tv/TheAudioDB DO own a listable artist-photo set —
// artwork-management/02 — so their ArtworkCandidates live with the rest of their
// provider logic, not here.)
