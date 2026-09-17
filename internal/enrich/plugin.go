package enrich

import (
	"context"
	"errors"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
)

// The contract edge of the enrichment domain (ADR-0057). A Metadata provider is a
// Plugin now: the server asks it through pluginapi's wire types and it answers
// with an Outcome, and this file is the only place the two vocabularies meet.
//
// It has two halves, because the video Built-ins still live in this package while
// an Installed plugin never will:
//
//   - the PLUGIN side (pluginFromProvider) dresses one of this package's sources
//     as a Plugin: domain values out, wire types back, sentinels turned into
//     Outcomes. It is what a Built-in's registration factory returns.
//   - the HOST side (ProviderFromPlugin) turns any Plugin back into the
//     MetadataProvider the chains and the Service have always called, consulting
//     the Descriptor before an optional call and mapping each Outcome back to the
//     sentinel `errors.Is` callers already match on.
//
// Nothing above the host side knows the contract exists; nothing below the Plugin
// side knows the server's sentinels exist. When Phase 2 puts a sandbox boundary
// under pluginapi, only the host half stays — the Plugin half is what an external
// author writes for themselves, and this is the shape of it.

// errOutcomeNotInPoint is what a Plugin gets for answering with an Outcome this
// build has never heard of. It is a programming error in the Plugin, not a domain
// outcome, so it surfaces as a real error rather than quietly reading as "nothing
// found".
var errOutcomeNotInPoint = errors.New("enrich: outcome is not part of the Metadata provider extension point")

// outcomeError maps a contract Outcome onto the enrichment domain's sentinels —
// the translation ADR-0057 decision 2 puts at the edge so that the Service, the
// retry scheduler, the settled-reason writer and every existing test keep matching
// on exactly the errors they always did. The mapping is TOTAL over
// pluginapi.AllOutcomes(), and the test that proves it ranges over that list, so a
// new Outcome cannot be added without this file deciding what it means here.
//
//   - matched              → nil; the record / candidates are in the response
//   - no-match             → ErrNoMatch, the normal "this source has nothing"
//   - rejected             → ErrMatchRejected. No Built-in produces it — acceptance
//     is the HOST's judgment (ADR-0057 decision 3) — but a Plugin
//     MAY still say its source declined its own top hit, and the
//     item then settles with the `search-rejected` reason exactly
//     as it does today
//   - unavailable          → ErrSearchUnavailable, the "this kind cannot be searched
//     right now" the Edit-item box reports to the Admin instead
//     of an empty result set. It is also what an UNDECLARED
//     capability answers, without the Plugin being called
//   - ref-invalid          → ErrExternalRefInvalid
//   - ref-kind-mismatch    → ErrExternalRefKindMismatch. The got/want kinds that
//     make the message specific are carried by the external-ref
//     response type, which arrives with the music chain and its
//     external-ref capability; until then this is the bare
//     sentinel, which is what errors.Is callers match on anyway
//   - ref-unsupported-kind → ErrExternalRefUnsupportedKind
func outcomeError(o pluginapi.Outcome) error {
	switch o {
	case pluginapi.OutcomeMatched:
		return nil
	case pluginapi.OutcomeNoMatch:
		return ErrNoMatch
	case pluginapi.OutcomeRejected:
		return ErrMatchRejected
	case pluginapi.OutcomeUnavailable:
		return ErrSearchUnavailable
	case pluginapi.OutcomeRefInvalid:
		return ErrExternalRefInvalid
	case pluginapi.OutcomeRefKindMismatch:
		return ErrExternalRefKindMismatch
	case pluginapi.OutcomeRefUnsupportedKind:
		return ErrExternalRefUnsupportedKind
	default:
		return fmt.Errorf("%w: %q", errOutcomeNotInPoint, o)
	}
}

// errorOutcome is outcomeError's inverse, used by the Plugin side: it reports
// which Outcome a domain error IS, or ok=false when the error is not a domain
// outcome at all. That second case is the important one — a transport failure, a
// 503, a context deadline is NOT an outcome, it is a Go error that travels
// alongside the response and the host retries rather than settling the item
// (ADR-0048). Squashing it into an outcome here would turn every outage into a
// permanent "no match".
//
// The order matters: ErrMatchRejected WRAPS ErrNoMatch, so it has to be tested
// first or every rejection would be reported as a plain no-match and the
// `search-rejected` reason would disappear.
func errorOutcome(err error) (pluginapi.Outcome, bool) {
	switch {
	case err == nil:
		return pluginapi.OutcomeMatched, true
	case errors.Is(err, ErrMatchRejected):
		return pluginapi.OutcomeRejected, true
	case errors.Is(err, ErrNoMatch):
		return pluginapi.OutcomeNoMatch, true
	case errors.Is(err, ErrSearchUnavailable):
		return pluginapi.OutcomeUnavailable, true
	case errors.Is(err, ErrExternalRefKindMismatch):
		return pluginapi.OutcomeRefKindMismatch, true
	case errors.Is(err, ErrExternalRefUnsupportedKind):
		return pluginapi.OutcomeRefUnsupportedKind, true
	case errors.Is(err, ErrExternalRefInvalid):
		return pluginapi.OutcomeRefInvalid, true
	default:
		return "", false
	}
}

// --- the Plugin side: a source in this package, dressed as a Plugin -----------

// pluginFromProvider wraps one of this package's sources so it can be registered
// as a Built-in Metadata provider Plugin. It is the factory's return value, and it
// is deliberately total: an optional operation the wrapped source does not
// implement answers OutcomeUnavailable rather than failing, which is the same
// thing the host would answer for an undeclared capability.
func pluginFromProvider(p MetadataProvider) pluginapi.MetadataProvider {
	return builtinPlugin{provider: p}
}

// builtinPlugin is the Plugin half of the adapter: wire types in, wire types out.
type builtinPlugin struct{ provider MetadataProvider }

func (b builtinPlugin) Lookup(ctx context.Context, req pluginapi.LookupRequest) (pluginapi.LookupResponse, error) {
	meta, err := b.provider.Lookup(ctx, titleRefFromWire(req.Ref))
	outcome, ok := errorOutcome(err)
	if !ok {
		return pluginapi.LookupResponse{}, err // a transport failure is not an outcome
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.LookupResponse{Outcome: outcome}, nil
	}
	return pluginapi.LookupResponse{Outcome: outcome, Record: recordFromMetadata(meta)}, nil
}

func (b builtinPlugin) Search(ctx context.Context, req pluginapi.SearchRequest) (pluginapi.SearchResponse, error) {
	cands, err := b.provider.Search(ctx, req.Kind, req.Query, SearchOptions{
		Artist:  req.Artist,
		Release: req.Release,
		Limit:   req.Limit,
		Offset:  req.Offset,
	})
	outcome, ok := errorOutcome(err)
	if !ok {
		return pluginapi.SearchResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.SearchResponse{Outcome: outcome}, nil
	}
	out := make([]pluginapi.SearchCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, pluginapi.SearchCandidate{
			ExternalID:     c.ExternalID,
			Title:          c.Title,
			Year:           c.Year,
			ThumbnailURL:   c.ThumbnailURL,
			Disambiguation: c.Disambiguation,
			Kind:           c.Kind,
			TypeLabel:      c.TypeLabel,
		})
	}
	return pluginapi.SearchResponse{Outcome: outcome, Candidates: out}, nil
}

func (b builtinPlugin) ArtworkCandidates(ctx context.Context, req pluginapi.ArtworkCandidatesRequest) (pluginapi.ArtworkCandidatesResponse, error) {
	cands, err := b.provider.ArtworkCandidates(ctx, titleRefFromWire(req.Ref), req.Role)
	outcome, ok := errorOutcome(err)
	if !ok {
		return pluginapi.ArtworkCandidatesResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.ArtworkCandidatesResponse{Outcome: outcome}, nil
	}
	out := make([]pluginapi.ArtworkCandidate, 0, len(cands))
	for _, c := range cands {
		out = append(out, pluginapi.ArtworkCandidate{URL: c.URL, Width: c.Width, Height: c.Height, Source: c.Source})
	}
	return pluginapi.ArtworkCandidatesResponse{Outcome: outcome, Candidates: out}, nil
}

// SeriesSeasons / SeasonEpisodes are the CapabilityEpisodeList half. A wrapped
// source that does not implement EpisodeLister answers OutcomeUnavailable — the
// same thing the type assertion the chains used to make degraded to.
func (b builtinPlugin) SeriesSeasons(ctx context.Context, req pluginapi.SeriesSeasonsRequest) (pluginapi.SeriesSeasonsResponse, error) {
	lister, ok := b.provider.(EpisodeLister)
	if !ok {
		return pluginapi.SeriesSeasonsResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	seasons, err := lister.SeriesSeasons(ctx, req.SeriesID)
	outcome, known := errorOutcome(err)
	if !known {
		return pluginapi.SeriesSeasonsResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.SeriesSeasonsResponse{Outcome: outcome}, nil
	}
	out := make([]pluginapi.SeasonSummary, 0, len(seasons))
	for _, s := range seasons {
		out = append(out, pluginapi.SeasonSummary{Season: s.Season, EpisodeCount: s.EpisodeCount})
	}
	return pluginapi.SeriesSeasonsResponse{Outcome: outcome, Seasons: out}, nil
}

func (b builtinPlugin) SeasonEpisodes(ctx context.Context, req pluginapi.SeasonEpisodesRequest) (pluginapi.SeasonEpisodesResponse, error) {
	lister, ok := b.provider.(EpisodeLister)
	if !ok {
		return pluginapi.SeasonEpisodesResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	eps, err := lister.SeasonEpisodes(ctx, req.SeriesID, req.Season)
	outcome, known := errorOutcome(err)
	if !known {
		return pluginapi.SeasonEpisodesResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.SeasonEpisodesResponse{Outcome: outcome}, nil
	}
	out := make([]pluginapi.EpisodeCandidate, 0, len(eps))
	for _, e := range eps {
		out = append(out, pluginapi.EpisodeCandidate{
			Season:   e.Season,
			Episode:  e.Episode,
			Name:     e.Name,
			Overview: e.Overview,
			AirDate:  e.AirDate,
			StillURL: e.StillURL,
		})
	}
	return pluginapi.SeasonEpisodesResponse{Outcome: outcome, Episodes: out}, nil
}

// --- the host side: a Plugin, called as a MetadataProvider -------------------

// ProviderFromPlugin adapts a contract-level Metadata provider Plugin to the
// MetadataProvider interface the chains and the Service call. The Descriptor comes
// with it because capabilities are CONSULTED, not guessed: an operation the Plugin
// did not declare costs no call at all and answers ErrSearchUnavailable, which is
// the "unavailable" the Edit-item box and the artwork picker already render.
//
// The returned value always implements EpisodeLister, so the type assertions the
// chains make still succeed; a Plugin without CapabilityEpisodeList answers
// ErrSearchUnavailable through it, which is exactly what a failed assertion used
// to degrade to.
func ProviderFromPlugin(d pluginapi.Descriptor, p pluginapi.MetadataProvider) MetadataProvider {
	if p == nil {
		return nil
	}
	return pluginProvider{desc: d, plugin: p}
}

// pluginProvider is the host half of the adapter: domain values out, wire types
// in, Outcomes back as sentinels.
type pluginProvider struct {
	desc   pluginapi.Descriptor
	plugin pluginapi.MetadataProvider
}

// Lookup is the one call no capability gates: resolving a ref to a record is what
// a Metadata provider IS, and a Plugin that cannot do it has no business
// registering.
func (a pluginProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	resp, err := a.plugin.Lookup(ctx, pluginapi.LookupRequest{Ref: wireRefFromTitleRef(ref)})
	if err != nil {
		return TitleMetadata{}, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return TitleMetadata{}, err
	}
	return metadataFromRecord(resp.Record), nil
}

// Search asks the Plugin for candidates. An undeclared CapabilitySearch is the
// "this kind cannot be searched right now" the Edit-item box reports, produced
// WITHOUT a call; a declared one that finds nothing is an empty list and a nil
// error, which is a different thing the box also already renders.
func (a pluginProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	if !a.desc.HasCapability(pluginapi.CapabilitySearch) {
		return nil, ErrSearchUnavailable
	}
	resp, err := a.plugin.Search(ctx, pluginapi.SearchRequest{
		Kind:    kind,
		Query:   query,
		Artist:  opts.Artist,
		Release: opts.Release,
		Page:    pluginapi.Page{Limit: opts.Limit, Offset: opts.Offset},
	})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	if len(resp.Candidates) == 0 {
		return nil, nil
	}
	out := make([]Candidate, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, Candidate{
			ExternalID:     c.ExternalID,
			Title:          c.Title,
			Year:           c.Year,
			ThumbnailURL:   c.ThumbnailURL,
			Disambiguation: c.Disambiguation,
			Kind:           c.Kind,
			TypeLabel:      c.TypeLabel,
		})
	}
	return out, nil
}

// ArtworkCandidates asks the Plugin for the images it offers for a role. An
// undeclared CapabilityArtworkCandidates is the picker's "not now"
// (ErrSearchUnavailable), which degrades to the upload path rather than an error
// page — the same answer a source that owns no listable set gives today.
func (a pluginProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if !a.desc.HasCapability(pluginapi.CapabilityArtworkCandidates) {
		return nil, ErrSearchUnavailable
	}
	resp, err := a.plugin.ArtworkCandidates(ctx, pluginapi.ArtworkCandidatesRequest{
		Ref:  wireRefFromTitleRef(ref),
		Role: role,
	})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	if len(resp.Candidates) == 0 {
		return nil, nil
	}
	out := make([]ArtworkCandidate, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, ArtworkCandidate{URL: c.URL, Width: c.Width, Height: c.Height, Source: c.Source})
	}
	return out, nil
}

// SeriesSeasons / SeasonEpisodes serve the optional EpisodeLister capability. A
// Plugin that did not declare CapabilityEpisodeList reports ErrSearchUnavailable
// so the picker says "no episode list here" instead of hanging or pretending —
// the graceful posture the rest of enrichment takes (ADR-0001).
func (a pluginProvider) SeriesSeasons(ctx context.Context, showID string) ([]SeasonSummary, error) {
	lister, ok := a.episodeLister()
	if !ok {
		return nil, ErrSearchUnavailable
	}
	resp, err := lister.SeriesSeasons(ctx, pluginapi.SeriesSeasonsRequest{SeriesID: showID})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	out := make([]SeasonSummary, 0, len(resp.Seasons))
	for _, s := range resp.Seasons {
		out = append(out, SeasonSummary{Season: s.Season, EpisodeCount: s.EpisodeCount})
	}
	return out, nil
}

func (a pluginProvider) SeasonEpisodes(ctx context.Context, showID string, season int) ([]EpisodeCandidate, error) {
	lister, ok := a.episodeLister()
	if !ok {
		return nil, ErrSearchUnavailable
	}
	resp, err := lister.SeasonEpisodes(ctx, pluginapi.SeasonEpisodesRequest{SeriesID: showID, Season: season})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	out := make([]EpisodeCandidate, 0, len(resp.Episodes))
	for _, e := range resp.Episodes {
		out = append(out, EpisodeCandidate{
			Season:   e.Season,
			Episode:  e.Episode,
			Name:     e.Name,
			Overview: e.Overview,
			AirDate:  e.AirDate,
			StillURL: e.StillURL,
		})
	}
	return out, nil
}

// episodeLister reports whether this Plugin may be asked for an episode list: it
// has to have DECLARED the capability and to actually implement the call. The
// declaration is checked first, so an undeclared capability costs no call.
func (a pluginProvider) episodeLister() (pluginapi.EpisodeLister, bool) {
	if !a.desc.HasCapability(pluginapi.CapabilityEpisodeList) {
		return nil, false
	}
	lister, ok := a.plugin.(pluginapi.EpisodeLister)
	return lister, ok
}

// --- the two vocabularies' value translations --------------------------------

func wireRefFromTitleRef(ref TitleRef) pluginapi.MediaRef {
	out := pluginapi.MediaRef{
		Kind:          ref.Kind,
		Title:         ref.Title,
		Year:          ref.Year,
		TMDBID:        ref.TMDBID,
		IMDBID:        ref.IMDBID,
		MusicbrainzID: ref.MusicbrainzID,
		TheTVDBID:     ref.TheTVDBID,
		AniDBID:       ref.AniDBID,
		SeasonNumber:  ref.SeasonNumber,
		EpisodeNumber: ref.EpisodeNumber,
		EpisodeLabel:  ref.EpisodeLabel,
		Artist:        ref.Artist,
		Album:         ref.Album,
		Track:         ref.Track,
		ReleaseMBID:   ref.ReleaseMBID,
	}
	for _, h := range ref.AlbumHints {
		out.AlbumHints = append(out.AlbumHints, pluginapi.AlbumHint{
			Title:            h.Title,
			ReleaseGroupMBID: h.ReleaseGroupMBID,
		})
	}
	return out
}

func titleRefFromWire(ref pluginapi.MediaRef) TitleRef {
	out := TitleRef{
		Kind:          ref.Kind,
		Title:         ref.Title,
		Year:          ref.Year,
		TMDBID:        ref.TMDBID,
		IMDBID:        ref.IMDBID,
		MusicbrainzID: ref.MusicbrainzID,
		TheTVDBID:     ref.TheTVDBID,
		AniDBID:       ref.AniDBID,
		SeasonNumber:  ref.SeasonNumber,
		EpisodeNumber: ref.EpisodeNumber,
		EpisodeLabel:  ref.EpisodeLabel,
		Artist:        ref.Artist,
		Album:         ref.Album,
		Track:         ref.Track,
		ReleaseMBID:   ref.ReleaseMBID,
	}
	for _, h := range ref.AlbumHints {
		out.AlbumHints = append(out.AlbumHints, AlbumHint{
			Title:            h.Title,
			ReleaseGroupMBID: h.ReleaseGroupMBID,
		})
	}
	return out
}

func recordFromMetadata(meta TitleMetadata) pluginapi.MetadataRecord {
	rec := pluginapi.MetadataRecord{
		Matched:        meta.Matched,
		Name:           meta.Name,
		Year:           meta.Year,
		Overview:       meta.Overview,
		Tagline:        meta.Tagline,
		ContentRating:  meta.ContentRating,
		ReleaseDate:    meta.ReleaseDate,
		RuntimeMinutes: meta.RuntimeMinutes,
		Studio:         meta.Studio,
		Genres:         meta.Genres,
		ExternalID:     meta.ExternalID,
		Source:         meta.Source,
		FromSearch:     meta.FromSearch,
	}
	for _, c := range meta.Cast {
		rec.Cast = append(rec.Cast, pluginapi.Credit{
			Person:    c.Person,
			Role:      c.Role,
			Character: c.Character,
			Kind:      c.Kind,
			PersonRef: c.PersonRef,
			ImageURL:  c.ImageURL,
		})
	}
	for _, a := range meta.Artwork {
		rec.Artwork = append(rec.Artwork, pluginapi.ArtworkRef{Role: a.Role, URL: a.URL})
	}
	return rec
}

func metadataFromRecord(rec pluginapi.MetadataRecord) TitleMetadata {
	meta := TitleMetadata{
		Matched:        rec.Matched,
		Name:           rec.Name,
		Year:           rec.Year,
		Overview:       rec.Overview,
		Tagline:        rec.Tagline,
		ContentRating:  rec.ContentRating,
		ReleaseDate:    rec.ReleaseDate,
		RuntimeMinutes: rec.RuntimeMinutes,
		Studio:         rec.Studio,
		Genres:         rec.Genres,
		ExternalID:     rec.ExternalID,
		Source:         rec.Source,
		FromSearch:     rec.FromSearch,
	}
	for _, c := range rec.Cast {
		meta.Cast = append(meta.Cast, Credit{
			Person:    c.Person,
			Role:      c.Role,
			Character: c.Character,
			Kind:      c.Kind,
			PersonRef: c.PersonRef,
			ImageURL:  c.ImageURL,
		})
	}
	for _, a := range rec.Artwork {
		meta.Artwork = append(meta.Artwork, ArtworkRef{Role: a.Role, URL: a.URL})
	}
	return meta
}
