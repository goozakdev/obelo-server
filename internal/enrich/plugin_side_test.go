package enrich

import (
	"context"
	"errors"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// THE PLUGIN SIDE OF THE CONTRACT EDGE, WHICH IS NO LONGER PRODUCTION CODE
// (.scratch/bundled-plugins: issue 08).
//
// Until the seven shipped Metadata providers became Bundled plugins (ADR-0059),
// every one of them was a Go source living in this package, and this adapter was
// what a Built-in's registration factory returned: domain values out, wire types
// back, sentinels turned into Outcomes. plugin.go held it beside the host half.
//
// There is no Built-in Metadata provider left to dress, so it moved here. It is
// kept, rather than deleted, because it is what still drives the ROUND-TRIP
// assertions in plugin_test.go and plugin_music_test.go: a source in this
// package's own vocabulary, pushed out through the contract and pulled back in
// through ProviderFromPlugin, with nothing allowed to go missing on the way. Those
// are the tests that would notice a field quietly dropped from the wire — the
// mistake that does not fail a compile and shows up as an empty track preview or a
// record stored unjudged.
//
// The real Plugin side now lives on the far side of the sandbox, in the SDK's
// dispatcher (pluginsdk/metadata) and in what each plugins/<id>/ module returns.
// This is the same shape, in-process, and its behaviour is deliberately identical:
// a transport failure is a Go error and not an Outcome, an unimplemented optional
// call answers unavailable (no-match, for a tracklist), and a kind mismatch lifts
// its got/want kinds onto the response where they survive a boundary. If the two
// ever disagree, the assertions here are describing a contract nothing implements
// — which is the day to delete this file rather than to teach it a new trick.

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

// pluginFromProvider wraps a source written in this package's own vocabulary so it
// can be called as a Metadata provider Plugin. It used to be what a Built-in's
// registration factory returned; it is now how a test produces a guest-shaped
// answer without a guest. It is deliberately total: an optional operation the
// wrapped source does not implement answers OutcomeUnavailable rather than
// failing, which is the same thing the host answers for an undeclared capability
// and the same thing the SDK answers for an unimplemented one.
func pluginFromProvider(p MetadataProvider) pluginapi.MetadataProvider {
	return builtinPlugin{provider: p}
}

// builtinPlugin is the Plugin half of the adapter: wire types in, wire types out.
// The name is the history — these were the Built-ins' clothes.
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
