//go:build wasm

package metadata

import (
	"context"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// The eight exports of the Metadata provider Extension point. Every one is the
// same decode, dispatch, encode — [dispatch] — which is exactly why they are
// here and not in seven plugins.
//
// # The context
//
// callCtx() reads Settings.CallRemainingMillis — the host-resolved time
// remaining this call is under, riding the wire exactly as RateLimitMillis does
// — and builds either
// context.Background() (no budget stated) or a context.WithTimeout a margin
// short of it ([pluginsdk.CallContext]). The host's OWN deadline is still
// enforced the hard way, by unwinding the guest (wazero's WithCloseOnContextDone)
// and capping any real sleep at what is left of it (ADR-0059 decision 6,
// internal/plugins/guest.go): a plugin that ignores this ctx entirely still
// cannot outlive its budget. What this ctx buys a plugin that DOES honour it —
// [pluginsdk.Pacer.Wait], [pluginsdk.GetJSON] — is the chance to notice first and
// answer OutcomeUnavailable instead of being killed mid-call, which the host
// counts as an answer rather than a failure (ADR-0059 decision 6).
//
// # Errors
//
// A Go error from a served method means the transport or the plugin failed, which
// the host retries rather than parking the item (ADR-0048). It travels back as
// the 0-and-last_error the ABI reserves for exactly that, so the host counts a
// failure — which is what an error IS. A source that simply has nothing is
// OutcomeNoMatch and is not an error.

// callCtx builds the ctx every export hands to served provider code: see the
// package comment above for what it does and why. It reads Settings itself,
// rather than trusting the provider to have read them first, because a plugin
// that never calls Host.Settings() must still get a bounded ctx exactly like one
// that does.
func callCtx() (context.Context, context.CancelFunc) {
	return pluginsdk.CallContext(pluginsdk.Sandbox().Settings().CallRemainingMillis)
}

// dispatch is the tail every export shares: decode the request, build the call
// ctx, call the served method, and answer with its response or with the failure
// the ABI reserves for a Go error. request completes "the request is not ..."
// and label prefixes the error, so each export keeps its own sentence.
func dispatch[Req, Resp any](ptr, n uint32, request, label string, call func(context.Context, Req) (Resp, error)) uint64 {
	var req Req
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not " + request)
	}
	ctx, cancel := callCtx()
	defer cancel()
	resp, err := call(ctx, req)
	if err != nil {
		return pluginsdk.Fail(label + ": " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport metadata_lookup
func metadataLookup(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	return dispatch(ptr, n, "a LookupRequest", "lookup", p.Lookup)
}

//go:wasmexport metadata_search
func metadataSearch(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	return dispatch(ptr, n, "a SearchRequest", "search", p.Search)
}

//go:wasmexport metadata_artwork_candidates
func metadataArtworkCandidates(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	return dispatch(ptr, n, "an ArtworkCandidatesRequest", "artwork candidates", p.ArtworkCandidates)
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
	return dispatch(ptr, n, "a SeriesSeasonsRequest", "series seasons", lister.SeriesSeasons)
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
	return dispatch(ptr, n, "a SeasonEpisodesRequest", "season episodes", lister.SeasonEpisodes)
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
	return dispatch(ptr, n, "a TracklistRequest", "album tracklist", lister.AlbumTracklist)
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
	return dispatch(ptr, n, "a ReleaseEditionsRequest", "release editions", lister.ReleaseGroupEditions)
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
	return dispatch(ptr, n, "an ExternalRefRequest", "external ref", parser.ParseExternalRef)
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
