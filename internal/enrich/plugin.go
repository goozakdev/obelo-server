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
// IT HAS ONE HALF LEFT, which is the point of .scratch/bundled-plugins. It used to
// have two, because the shipped sources lived a few files away in this package:
//
//   - the PLUGIN side (pluginFromProvider) dressed one of them as a Plugin — domain
//     values out, wire types back, sentinels turned into Outcomes — and was what a
//     Built-in's registration factory returned;
//   - the HOST side (ProviderFromPlugin) turns any Plugin back into the
//     MetadataProvider the chains and the Service have always called, consulting
//     the Descriptor before an optional call and mapping each Outcome back to the
//     sentinel `errors.Is` callers already match on.
//
// That file comment used to end "when Phase 2 puts a sandbox boundary under
// pluginapi, only the host half stays — the Plugin half is what an external author
// writes for themselves". Phase 2 landed, the seven shipped sources are
// WebAssembly guests (ADR-0059), and the Plugin half is now the SDK's dispatcher
// (pluginsdk/metadata) on the other side of the sandbox. What is left here is the
// host half and the shared vocabulary it needs. The Plugin half survives only as a
// test double — see plugin_side_test.go — which is what still drives the
// round-trip assertions this edge is worth having.
//
// Nothing above the host side knows the contract exists.

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
			// The HOST stamps the namespace from the Plugin it asked (ADR-0060
			// decision 5): a source's namespace is its plugin id.
			Source: a.desc.Slug,
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

// wireRefFromTitleRef sends every id the TitleRef holds in BOTH carriers
// (ADR-0060 decision 7): ExternalIDs is the TitleRef's map merged with its five
// named fields, and each named MediaRef field is then set FROM that merged map. So a
// caller that sets only the named fields (every caller until the store keeps ids by
// namespace) still sends a populated map to a new guest, and a caller that sets
// only the map still fills the named mirrors a v1 guest reads — TheTVDBID and
// AniDBID included, which no caller used to fill.
func wireRefFromTitleRef(ref TitleRef) pluginapi.MediaRef {
	ids := mergedExternalIDs(ref)
	out := pluginapi.MediaRef{
		Kind:          ref.Kind,
		Title:         ref.Title,
		Year:          ref.Year,
		ExternalIDs:   ids,
		TMDBID:        ids[pluginapi.NamespaceTMDB],
		IMDBID:        ids[pluginapi.NamespaceIMDB],
		MusicbrainzID: ids[pluginapi.NamespaceMusicBrainz],
		TheTVDBID:     ids[pluginapi.NamespaceTheTVDB],
		AniDBID:       ids[pluginapi.NamespaceAniDB],
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

// mergedExternalIDs is the TitleRef's ExternalIDs with its five named fields folded
// in, or nil when it holds no id at all. The map is the carrier, so where a
// namespace is set in both and they differ the MAP'S ENTRY WINS; the host never
// builds such a ref on purpose, but a rule is still needed for when it does. A
// blank value is no id and is dropped, whichever side it came from. The caller's
// map is never written to.
func mergedExternalIDs(ref TitleRef) map[string]string {
	named := pluginapi.MediaRef{
		TMDBID: ref.TMDBID, IMDBID: ref.IMDBID, MusicbrainzID: ref.MusicbrainzID,
		TheTVDBID: ref.TheTVDBID, AniDBID: ref.AniDBID,
	}
	var out map[string]string
	put := func(ns, id string) {
		if out == nil {
			out = make(map[string]string)
		}
		out[ns] = id
	}
	for ns, id := range ref.ExternalIDs {
		if id = strings.TrimSpace(id); id != "" && ns != "" {
			put(ns, id)
		}
	}
	for _, ns := range pluginapi.NamedNamespaces() {
		if _, ok := out[ns]; ok {
			continue
		}
		if id := strings.TrimSpace(named.NamedID(ns)); id != "" {
			put(ns, id)
		}
	}
	return out
}

// titleRefFromWire is the reverse, for a reference a Plugin DECLARED (its connection
// probe): both carriers are read the way a guest reads them, through MediaRef.ID, so
// a Descriptor written against either shape yields the same TitleRef.
func titleRefFromWire(ref pluginapi.MediaRef) TitleRef {
	var ids map[string]string
	for ns := range ref.ExternalIDs {
		if id := ref.ID(ns); id != "" && ns != "" {
			if ids == nil {
				ids = make(map[string]string)
			}
			ids[ns] = id
		}
	}
	out := TitleRef{
		Kind:          ref.Kind,
		Title:         ref.Title,
		Year:          ref.Year,
		ExternalIDs:   ids,
		TMDBID:        ref.ID(pluginapi.NamespaceTMDB),
		IMDBID:        ref.ID(pluginapi.NamespaceIMDB),
		MusicbrainzID: ref.ID(pluginapi.NamespaceMusicBrainz),
		TheTVDBID:     ref.ID(pluginapi.NamespaceTheTVDB),
		AniDBID:       ref.ID(pluginapi.NamespaceAniDB),
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

func metadataFromRecord(rec pluginapi.MetadataRecord) TitleMetadata {
	meta := TitleMetadata{
		Matched:             rec.Matched,
		Name:                rec.Name,
		Year:                rec.Year,
		Overview:            rec.Overview,
		OverviewSynthesized: rec.OverviewSynthesized,
		Tagline:             rec.Tagline,
		ContentRating:       rec.ContentRating,
		ReleaseDate:         rec.ReleaseDate,
		RuntimeMinutes:      rec.RuntimeMinutes,
		Studio:              rec.Studio,
		Genres:              rec.Genres,
		ExternalID:          rec.ExternalID,
		Source:              rec.Source,
		FromSearch:          rec.FromSearch,
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

// tracklistError is the AlbumTracklist call's OWN Outcome→error mapping, and it is
// deliberately not outcomeError. Within this one call "no-match" MEANS "this album
// has no tracklist": the call guarantees a matched answer is never empty
// (ErrNoTracklist exists precisely so an empty list cannot stand in for it —
// ADR-0050), so the two values round-trip losslessly and the contract needs no
// eighth Outcome that only one Extension point could ever mean anything by.
//
// An Outcome this call has no meaning for is still mapped by the domain's general
// table, so a Plugin answering "unavailable" is reported as such rather than
// silently read as an album with nothing on it. Collapsing those two would turn an
// outage into a diagnosis, which is the mistake ADR-0049 spent an outage learning.
func tracklistError(o pluginapi.Outcome) error {
	if o == pluginapi.OutcomeNoMatch {
		return ErrNoTracklist
	}
	return outcomeError(o)
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
	// Stamped with the answering Plugin's id: it is the namespace of the id it read
	// (ADR-0060 decision 5).
	return ExternalRef{ExternalID: resp.ExternalID, ReleaseID: resp.ReleaseID, Namespace: a.desc.Slug}, nil
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

// domainTracklist translates a tracklist from the contract's vocabulary into this
// package's. An entry with no recording id keeps its position: it still CLAIMS
// that position for the host's match rule (ADR-0050).
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
