package pluginsdk

// The guest export names, one per contract call, COPIED from the host
// (internal/plugins/guest.go, metadata.go, subtitle.go).
//
// They are copied and not imported, because this module may not import the
// server — that is the point of the module split. A copy is a thing that drifts,
// so the copy is held to the original by a test ON THE HOST SIDE:
// internal/plugins/sdk_export_names_test.go asserts that these constants and the
// loader's unexported ones are the same lists, element for element. Add an export
// there and the test fails here until this file catches up.
//
// An author writing in another language types these strings into their own
// module's export table; that is why they are stated in the authoring guide as
// well as here.
const (
	// ExportAlloc, ExportFree and ExportLastError are the ABI plumbing every
	// module provides whatever seam it fills. They carry the obelo_ prefix; a
	// contract call is named after the call.
	ExportAlloc     = "obelo_alloc"
	ExportFree      = "obelo_free"
	ExportLastError = "last_error"

	// ExportDeliver is the Event sink Extension point's one contract call.
	ExportDeliver = "deliver"

	// The Metadata provider Extension point's eight calls. The `metadata_` part
	// names the seam, so one module can fill two seams without its exports
	// colliding.
	ExportMetadataLookup            = "metadata_lookup"
	ExportMetadataSearch            = "metadata_search"
	ExportMetadataArtworkCandidates = "metadata_artwork_candidates"
	ExportMetadataSeriesSeasons     = "metadata_series_seasons"
	ExportMetadataSeasonEpisodes    = "metadata_season_episodes"
	ExportMetadataAlbumTracklist    = "metadata_album_tracklist"
	ExportMetadataReleaseEditions   = "metadata_release_editions"
	ExportMetadataExternalRef       = "metadata_external_ref"

	// The Subtitle provider Extension point's two calls.
	ExportSubtitleSearch   = "obelo_subtitle_search"
	ExportSubtitleDownload = "obelo_subtitle_download"
)

// HostModule is the namespace the six host functions are imported from. A module
// importing anything but this and wasi_snapshot_preview1 is refused at load,
// before it is instantiated.
const HostModule = "obelo"

// The six host functions, by name. They are here for the same reason the exports
// are: so a test can hold the SDK's list to the loader's, and so the authoring
// guide can quote a list that is checked.
const (
	HostFuncHTTPFetch   = "http_fetch"
	HostFuncLog         = "log"
	HostFuncKVGet       = "kv_get"
	HostFuncKVSet       = "kv_set"
	HostFuncKVDelete    = "kv_delete"
	HostFuncSettingsGet = "settings_get"
)

// MetadataExports is every Metadata provider export in contract order, which is
// what the equality test compares against the host's list.
func MetadataExports() []string {
	return []string{
		ExportMetadataLookup,
		ExportMetadataSearch,
		ExportMetadataArtworkCandidates,
		ExportMetadataSeriesSeasons,
		ExportMetadataSeasonEpisodes,
		ExportMetadataAlbumTracklist,
		ExportMetadataReleaseEditions,
		ExportMetadataExternalRef,
	}
}

// SubtitleExports is both Subtitle provider exports, in contract order.
func SubtitleExports() []string {
	return []string{ExportSubtitleSearch, ExportSubtitleDownload}
}

// ABIExports is the plumbing every module provides, in the order the host looks
// them up.
func ABIExports() []string {
	return []string{ExportAlloc, ExportFree, ExportLastError}
}

// HostFuncs is every host function a guest may import, in the order the host
// registers them.
func HostFuncs() []string {
	return []string{
		HostFuncHTTPFetch,
		HostFuncLog,
		HostFuncKVGet,
		HostFuncKVSet,
		HostFuncKVDelete,
		HostFuncSettingsGet,
	}
}
