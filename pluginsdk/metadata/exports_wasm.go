//go:build wasm

package metadata

import (
	"context"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The eight exports of the Metadata provider Extension point. Every one has the
// same five lines — decode, dispatch, encode — which is exactly why they are
// here and not in seven plugins.
//
// # The context
//
// context.Background(), and that is not a shrug. The host's deadline is enforced
// by UNWINDING the guest (wazero's WithCloseOnContextDone), not by cancelling
// something the guest holds, so there is no deadline in here to pass on and a
// plugin cannot outlive its budget by ignoring a ctx. The parameter is on the
// contract's interfaces because a Built-in is called in-process, where the
// deadline is real; a plugin gets a context it can pass to an SDK helper and
// nothing more.
//
// # Errors
//
// A Go error from a served method means the transport or the plugin failed, which
// the host retries rather than parking the item (ADR-0048). It travels back as
// the 0-and-last_error the ABI reserves for exactly that, so the host counts a
// failure — which is what an error IS. A source that simply has nothing is
// OutcomeNoMatch and is not an error.

//go:wasmexport metadata_lookup
func metadataLookup(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var req pluginapi.LookupRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a LookupRequest")
	}
	resp, err := p.Lookup(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("lookup: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_search
func metadataSearch(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var req pluginapi.SearchRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a SearchRequest")
	}
	resp, err := p.Search(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("search: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_artwork_candidates
func metadataArtworkCandidates(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var req pluginapi.ArtworkCandidatesRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not an ArtworkCandidatesRequest")
	}
	resp, err := p.ArtworkCandidates(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("artwork candidates: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_series_seasons
func metadataSeriesSeasons(ptr, n uint32) uint64 {
	lister, ok := served.(EpisodeLister)
	if !ok {
		return pluginsdk.Reply(pluginapi.SeriesSeasonsResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  unimplemented("episode-list"),
		})
	}
	var req pluginapi.SeriesSeasonsRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a SeriesSeasonsRequest")
	}
	resp, err := lister.SeriesSeasons(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("series seasons: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_season_episodes
func metadataSeasonEpisodes(ptr, n uint32) uint64 {
	lister, ok := served.(EpisodeLister)
	if !ok {
		return pluginsdk.Reply(pluginapi.SeasonEpisodesResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  unimplemented("episode-list"),
		})
	}
	var req pluginapi.SeasonEpisodesRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a SeasonEpisodesRequest")
	}
	resp, err := lister.SeasonEpisodes(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("season episodes: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

// metadata_album_tracklist answers OutcomeUnavailable — not the OutcomeNoMatch
// the host substitutes for a MISSING export — when the served value does not
// implement [AlbumTracklister].
//
// The two are different claims and only one of them is true here. No-match is
// settled: "this album can name none of its contents", which sends an Admin to
// the Album. A provider that does not implement the call has diagnosed nothing,
// so it says so, and each Track falls through to the tiers ADR-0050 puts under
// it. The host's substitution is for a module whose exports it cannot see inside;
// this module can.
//
//go:wasmexport metadata_album_tracklist
func metadataAlbumTracklist(ptr, n uint32) uint64 {
	lister, ok := served.(AlbumTracklister)
	if !ok {
		return pluginsdk.Reply(pluginapi.TracklistResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  unimplemented("album-tracklist"),
		})
	}
	var req pluginapi.TracklistRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a TracklistRequest")
	}
	resp, err := lister.AlbumTracklist(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("album tracklist: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_release_editions
func metadataReleaseEditions(ptr, n uint32) uint64 {
	lister, ok := served.(AlbumTracklister)
	if !ok {
		return pluginsdk.Reply(pluginapi.ReleaseEditionsResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  unimplemented("album-tracklist"),
		})
	}
	var req pluginapi.ReleaseEditionsRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a ReleaseEditionsRequest")
	}
	resp, err := lister.ReleaseGroupEditions(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("release editions: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_external_ref
func metadataExternalRef(ptr, n uint32) uint64 {
	parser, ok := served.(ExternalRefParser)
	if !ok {
		return pluginsdk.Reply(pluginapi.ExternalRefResponse{
			Outcome: pluginapi.OutcomeUnavailable,
			Detail:  unimplemented("external-ref"),
		})
	}
	var req pluginapi.ExternalRefRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not an ExternalRefRequest")
	}
	resp, err := parser.ParseExternalRef(context.Background(), req)
	if err != nil {
		return pluginsdk.Fail("external ref: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

// noProvider is what every call answers when init() never reached Serve. It is a
// failure and not an "unavailable", because the module is broken rather than
// limited, and the sentence says which line is missing.
func noProvider() uint64 {
	return pluginsdk.Fail("this module serves no Metadata provider: call metadata.Serve from init() — " +
		"a -buildmode=c-shared module never runs main")
}

func unimplemented(capability string) string {
	return "this plugin's manifest declares the " + capability +
		" capability but its provider does not implement it"
}
