package enrich

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
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
//     capability answers, without the Plugin being called.
//     ONE CALLER READS IT DIFFERENTLY: pluginProvider.Lookup
//     marks it transient as well, because on a lookup it means
//     "we could not ask" rather than "there is nothing to show"
//     (see lookupUnavailable)
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
			// An album candidate carries its tracklist preview and, when the Admin named
			// one, the edition it came from. Both are album-only and both were left off
			// the wire until music crossed it; dropping them here would have quietly
			// emptied the picker's track preview and cleared a pasted /release/ URL's
			// chosen edition (ADR-0052).
			Tracklist: wireTracklist(c.Tracklist),
			ReleaseID: c.ReleaseID,
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
//
// OutcomeUnavailable ON A LOOKUP IS TRANSIENT, and that is the one place this
// adapter reads an Outcome as more than outcomeError does. See lookupUnavailable.
func (a pluginProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	resp, err := a.plugin.Lookup(ctx, pluginapi.LookupRequest{Ref: wireRefFromTitleRef(ref)})
	if err != nil {
		return TitleMetadata{}, err
	}
	if resp.Outcome == pluginapi.OutcomeUnavailable {
		return TitleMetadata{}, lookupUnavailable(resp.Detail)
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return TitleMetadata{}, err
	}
	return metadataFromRecord(resp.Record), nil
}

// lookupUnavailable is what a Plugin answering OutcomeUnavailable to a LOOKUP
// means, and it is deliberately not what the same Outcome means anywhere else.
//
// # Why one call reads it differently
//
// Everywhere else — search, the artwork picker, the episode chooser, a pasted
// reference — "unavailable" is a fact about the SOURCE'S CATALOGUE that a human
// is looking at right now: this source owns no listable set for this kind, so the
// picker says "not now" and offers the upload path instead. There is nothing to
// retry and nobody waiting.
//
// A LOOKUP is the enrichment pass, and a pass has exactly one question to answer
// about a failed lookup (ADR-0048): did we manage to ask? A Plugin says
// "unavailable" to a lookup when the host refused its fetch, when the fetch ran
// out of the call's budget, or when the source answered 503 — none of which is a
// statement about the item, and all of which ADR-0059 decision 6 says must take
// the backoff rather than park the Title. Without this, a guest that answered
// honestly would settle a perfectly matchable movie as 'failed' on one bad
// afternoon, which is the exact failure ADR-0048 exists to prevent and which the
// Built-in it replaced never had.
//
// The error satisfies BOTH matchers on purpose: errors.Is(err, ErrSearchUnavailable)
// keeps every existing caller that tells this apart from a no-match working, and
// IsTransient(err) is what recordLeafFailure and recordParentFailure read.
func lookupUnavailable(detail string) error {
	if strings.TrimSpace(detail) == "" {
		return transient(ErrSearchUnavailable)
	}
	return transient(fmt.Errorf("%w: %s", ErrSearchUnavailable, detail))
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
			Tracklist:      domainTracklist(c.Tracklist),
			ReleaseID:      c.ReleaseID,
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

// --- the music-only calls: album tracklist, album editions, external refs ----

// tracklistCallOutcome is the AlbumTracklist call's OWN error→Outcome mapping, and it
// is deliberately not errorOutcome. Within this one call "no-match" MEANS "this
// album has no tracklist": the call guarantees a matched answer is never empty
// (ErrNoTracklist exists precisely so an empty list cannot stand in for it —
// ADR-0050), so the two values round-trip losslessly and the contract needs no
// eighth Outcome that only one Extension point could ever mean anything by.
//
// Everything else — including a bare ErrNoMatch, which is what a 404 on the
// release browse produces — is NOT an outcome here and travels as a Go error, so
// the host still retries a failed fetch (ADR-0048) instead of settling the album
// as "has no tracklist". Collapsing those two would turn an outage into a
// diagnosis, which is the mistake ADR-0049 spent an outage learning.
func tracklistCallOutcome(err error) (pluginapi.Outcome, bool) {
	switch {
	case err == nil:
		return pluginapi.OutcomeMatched, true
	case errors.Is(err, ErrNoTracklist):
		return pluginapi.OutcomeNoMatch, true
	default:
		return "", false
	}
}

// tracklistCallError is tracklistCallOutcome's inverse on the host side: the same
// call-scoped reading of no-match, so what the Plugin said is what the caller
// gets. An Outcome this call has no meaning for is still mapped by the domain's
// general table, so a Plugin answering "unavailable" is reported as such rather
// than silently read as an album with nothing on it.
func tracklistError(o pluginapi.Outcome) error {
	if o == pluginapi.OutcomeNoMatch {
		return ErrNoTracklist
	}
	return outcomeError(o)
}

// AlbumTracklist (Plugin side) exposes a wrapped source's optional
// AlbumTracklister. A source that does not implement it answers OutcomeNoMatch —
// "this album has no tracklist" — which is exactly what the chains degraded to
// when the type assertion they used to make failed.
func (b builtinPlugin) AlbumTracklist(ctx context.Context, req pluginapi.TracklistRequest) (pluginapi.TracklistResponse, error) {
	lister, ok := b.provider.(AlbumTracklister)
	if !ok {
		return pluginapi.TracklistResponse{Outcome: pluginapi.OutcomeNoMatch}, nil
	}
	tracks, err := lister.AlbumTracklist(ctx, TracklistRequest{
		ReleaseGroupID:  req.ReleaseGroupID,
		ReleaseID:       req.ReleaseID,
		ReleaseIDChosen: req.ReleaseIDChosen,
		LocalTrackCount: req.LocalTrackCount,
	})
	outcome, known := tracklistCallOutcome(err)
	if !known {
		return pluginapi.TracklistResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.TracklistResponse{Outcome: outcome}, nil
	}
	return pluginapi.TracklistResponse{Outcome: outcome, Tracks: wireTracklist(tracks)}, nil
}

// ReleaseGroupEditions (Plugin side) exposes a wrapped source's optional
// AlbumEditionLister. A source that does not implement it answers
// OutcomeUnavailable — the picker's "not now", which is what the failed type
// assertion produced — and NOT the no-match a missing tracklist answers: an album
// with no editions to choose from is a real, matched answer.
func (b builtinPlugin) ReleaseGroupEditions(ctx context.Context, req pluginapi.ReleaseEditionsRequest) (pluginapi.ReleaseEditionsResponse, error) {
	lister, ok := b.provider.(AlbumEditionLister)
	if !ok {
		return pluginapi.ReleaseEditionsResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	eds, err := lister.ReleaseGroupEditions(ctx, req.ReleaseGroupID)
	outcome, known := errorOutcome(err)
	if !known {
		return pluginapi.ReleaseEditionsResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		return pluginapi.ReleaseEditionsResponse{Outcome: outcome}, nil
	}
	out := make([]pluginapi.ReleaseEdition, 0, len(eds))
	for _, e := range eds {
		out = append(out, pluginapi.ReleaseEdition{
			ReleaseID:      e.ReleaseID,
			Date:           e.Date,
			Country:        e.Country,
			Format:         e.Format,
			TrackCount:     e.TrackCount,
			Disambiguation: e.Disambiguation,
		})
	}
	return pluginapi.ReleaseEditionsResponse{Outcome: outcome, Editions: out}, nil
}

// ParseExternalRef (Plugin side) exposes a wrapped source's optional
// ExternalRefParser, turning its three refusal sentinels into the three distinct
// Outcomes and lifting a kind mismatch's got/want kinds out of the typed error
// into the response — where they survive a boundary, which an error's Go type
// does not.
func (b builtinPlugin) ParseExternalRef(ctx context.Context, req pluginapi.ExternalRefRequest) (pluginapi.ExternalRefResponse, error) {
	parser, ok := b.provider.(ExternalRefParser)
	if !ok {
		return pluginapi.ExternalRefResponse{Outcome: pluginapi.OutcomeUnavailable}, nil
	}
	ref, err := parser.ParseExternalRef(ctx, req.Kind, req.Pasted)
	outcome, known := errorOutcome(err)
	if !known {
		return pluginapi.ExternalRefResponse{}, err
	}
	if outcome != pluginapi.OutcomeMatched {
		resp := pluginapi.ExternalRefResponse{Outcome: outcome}
		var mismatch *ExternalRefKindMismatchError
		if errors.As(err, &mismatch) {
			resp.GotKind, resp.WantKind = mismatch.Got, mismatch.Want
		}
		return resp, nil
	}
	return pluginapi.ExternalRefResponse{
		Outcome:    outcome,
		ExternalID: ref.ExternalID,
		ReleaseID:  ref.ReleaseID,
	}, nil
}

// AlbumTracklist (host side) asks the Plugin what its album holds. An undeclared
// CapabilityAlbumTracklist answers ErrNoTracklist without a call — the same
// "nothing to say about this album's contents" the pass already knows how to
// record, so the Tracks below it fall through to the tiers ADR-0050 puts under it.
func (a pluginProvider) AlbumTracklist(ctx context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	lister, ok := a.albumTracklister()
	if !ok {
		return nil, ErrNoTracklist
	}
	resp, err := lister.AlbumTracklist(ctx, pluginapi.TracklistRequest{
		ReleaseGroupID:  req.ReleaseGroupID,
		ReleaseID:       req.ReleaseID,
		ReleaseIDChosen: req.ReleaseIDChosen,
		LocalTrackCount: req.LocalTrackCount,
	})
	if err != nil {
		return nil, err
	}
	if err := tracklistError(resp.Outcome); err != nil {
		return nil, err
	}
	if len(resp.Tracks) == 0 {
		// The call forbids this, so a Plugin that does it anyway is normalized here
		// rather than at the call sites, which would each have to check both.
		return nil, ErrNoTracklist
	}
	return domainTracklist(resp.Tracks), nil
}

// ReleaseGroupEditions (host side) asks the Plugin for the album's editions. An
// undeclared capability answers ErrSearchUnavailable without a call, which is the
// picker's "not now" and degrades to the pasted-URL escape hatch (ADR-0052).
func (a pluginProvider) ReleaseGroupEditions(ctx context.Context, releaseGroupID string) ([]ReleaseEdition, error) {
	lister, ok := a.albumTracklister()
	if !ok {
		return nil, ErrSearchUnavailable
	}
	resp, err := lister.ReleaseGroupEditions(ctx, pluginapi.ReleaseEditionsRequest{ReleaseGroupID: releaseGroupID})
	if err != nil {
		return nil, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		return nil, err
	}
	out := make([]ReleaseEdition, 0, len(resp.Editions))
	for _, e := range resp.Editions {
		out = append(out, ReleaseEdition{
			ReleaseID:      e.ReleaseID,
			Date:           e.Date,
			Country:        e.Country,
			Format:         e.Format,
			TrackCount:     e.TrackCount,
			Disambiguation: e.Disambiguation,
		})
	}
	return out, nil
}

// ParseExternalRef (host side) asks the Plugin to read a pasted id or URL, and
// rebuilds the specific kind-mismatch error from the got/want kinds the response
// carries — so the Admin still gets the sentence naming both kinds, which is the
// whole reason those two fields cross the wire.
//
// An undeclared CapabilityExternalRef answers ErrSearchUnavailable without a call.
// That is not the paste box's final word: the host reads the id namespaces it
// already keeps columns for itself (see Service.externalRef), and only a Plugin
// that DECLARES the capability speaks for its own source's id shapes.
func (a pluginProvider) ParseExternalRef(ctx context.Context, kind, pasted string) (ExternalRef, error) {
	if !a.desc.HasCapability(pluginapi.CapabilityExternalRef) {
		return ExternalRef{}, ErrSearchUnavailable
	}
	parser, ok := a.plugin.(pluginapi.ExternalRefParser)
	if !ok {
		return ExternalRef{}, ErrSearchUnavailable
	}
	resp, err := parser.ParseExternalRef(ctx, pluginapi.ExternalRefRequest{Kind: kind, Pasted: pasted})
	if err != nil {
		return ExternalRef{}, err
	}
	if err := outcomeError(resp.Outcome); err != nil {
		if resp.Outcome == pluginapi.OutcomeRefKindMismatch && resp.GotKind != "" {
			return ExternalRef{}, &ExternalRefKindMismatchError{Got: resp.GotKind, Want: resp.WantKind}
		}
		return ExternalRef{}, err
	}
	return ExternalRef{ExternalID: resp.ExternalID, ReleaseID: resp.ReleaseID}, nil
}

// albumTracklister reports whether this Plugin may be asked what an album holds:
// it has to have DECLARED CapabilityAlbumTracklist and to implement the calls.
// The declaration is checked first, so an undeclared capability costs no call.
func (a pluginProvider) albumTracklister() (pluginapi.AlbumTracklister, bool) {
	if !a.desc.HasCapability(pluginapi.CapabilityAlbumTracklist) {
		return nil, false
	}
	lister, ok := a.plugin.(pluginapi.AlbumTracklister)
	return lister, ok
}

// wireTracklist / domainTracklist translate a tracklist between the two
// vocabularies. An entry with no recording id keeps its position on both sides:
// it still CLAIMS that position for the host's match rule (ADR-0050).
func wireTracklist(tracks []TrackCandidate) []pluginapi.TrackCandidate {
	if len(tracks) == 0 {
		return nil
	}
	out := make([]pluginapi.TrackCandidate, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, pluginapi.TrackCandidate{
			Disc:       t.Disc,
			Position:   t.Position,
			Title:      t.Title,
			ExternalID: t.ExternalID,
		})
	}
	return out
}

func domainTracklist(tracks []pluginapi.TrackCandidate) []TrackCandidate {
	if len(tracks) == 0 {
		return nil
	}
	out := make([]TrackCandidate, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, TrackCandidate{
			Disc:       t.Disc,
			Position:   t.Position,
			Title:      t.Title,
			ExternalID: t.ExternalID,
		})
	}
	return out
}
