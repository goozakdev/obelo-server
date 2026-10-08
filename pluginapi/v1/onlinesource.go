package v1

import "context"

// The Online source provider Extension point (ADR-0068, behind ADR-0057): a Plugin
// that lets a User browse and watch something outside the household — a PeerTube
// instance, the Internet Archive — without any of it becoming part of the
// catalog. Two request-response calls: rows() answers the source's Online rows,
// and resolve() turns one Online item into the URL(s) it plays from.
//
// The Plugin RESOLVES; the host PLAYS. A Plugin only ever supplies URLs and data.
// The host decides whether to fetch, how to play and who may see: it relays a
// muxed variant the client can play untouched, keeps every upstream URL to itself
// (a client is handed a stream token, never the source's address), and applies the
// Playback ceiling exactly as it does to a Title.
//
// Nothing here is persisted. An answer is used for the call that asked for it.

// OnlineVariantMuxed is the one variant shape this version of the contract
// carries: audio and video in a single file at a single URL. Split and manifest
// variants arrive additively, as their own Kind values.
const OnlineVariantMuxed = "muxed"

// OnlineItem is one playable video in an Online row. The only item kind is a
// single video; a channel or a playlist is a row, not an item.
type OnlineItem struct {
	// ID names the item to resolve(). It travels in a URL path, so the host drops
	// an item whose id is empty or outside letters, digits and . _ ~ -.
	ID    string `json:"id"`
	Title string `json:"title"`
	// ThumbnailURL is an https URL the HOST fetches and proxies; a client never
	// sees it.
	ThumbnailURL string `json:"thumbnailUrl"`
	DurationMs   int64  `json:"durationMs"`
	// Description and PublishedAt (RFC 3339) are optional.
	Description string `json:"description,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
}

// OnlineRow is one labelled shelf of an Online source's page.
type OnlineRow struct {
	ID    string       `json:"id"`
	Label string       `json:"label"`
	Items []OnlineItem `json:"items"`
}

// OnlineRowsRequest asks for the source's rows. It carries nothing today; it is a
// struct so a later field (a cursor, a search) is additive.
type OnlineRowsRequest struct{}

// OnlineRowsResponse is the source page: its rows, in the order to show them.
type OnlineRowsResponse struct {
	Rows []OnlineRow `json:"rows"`
}

// OnlineHints are what the host tells a Plugin about the client that will play an
// item, so it can offer a variant that client can use.
type OnlineHints struct {
	// MaxHeight is the tallest picture, in pixels, the host will play for this
	// User; 0 when it states none.
	MaxHeight int `json:"maxHeight,omitempty"`
}

// OnlineResolveRequest asks how to play one item.
type OnlineResolveRequest struct {
	ItemID string      `json:"itemId"`
	Hints  OnlineHints `json:"hints"`
}

// OnlineVariant is one way to play an item.
type OnlineVariant struct {
	// Kind is OnlineVariantMuxed; empty reads as muxed.
	Kind string `json:"kind,omitempty"`
	// URL is an https URL of the media file.
	URL string `json:"url"`
	// Container names the file's container as a client profile does ("mp4",
	// "webm"), and Codecs its codecs ("h264", "aac"), video first.
	Container string   `json:"container"`
	Codecs    []string `json:"codecs"`
	// Resolution is the picture height as a token ("720p"), empty when unknown.
	Resolution string `json:"resolution,omitempty"`
	// Headers are request headers the media host requires (referer, user agent).
	Headers map[string]string `json:"headers,omitempty"`
}

// OnlineResolveResponse is the item's playable variants. None is a source that no
// longer has the item.
type OnlineResolveResponse struct {
	Variants []OnlineVariant `json:"variants,omitempty"`
}

// OnlineSourceProvider is the Go call surface of the Online source provider
// Extension point. An error means the Plugin failed: the host shows the source as
// not responding and plays nothing.
type OnlineSourceProvider interface {
	Rows(ctx context.Context, req OnlineRowsRequest) (OnlineRowsResponse, error)
	Resolve(ctx context.Context, req OnlineResolveRequest) (OnlineResolveResponse, error)
}

// OnlineSourceProviderFactory builds an Online source provider from the Settings
// the host resolved. Settings.Enabled is true for anything the host builds, URL is
// the Descriptor's default, and Values carries whatever the Admin entered for the
// fields the manifest declared. Settings are server-wide: there is no per-User
// variant.
type OnlineSourceProviderFactory func(Settings) (OnlineSourceProvider, error)

// OnlineSourceProviderRegistration is what an Online source provider hands the
// host: what it is, and how to build it.
type OnlineSourceProviderRegistration struct {
	Descriptor Descriptor
	New        OnlineSourceProviderFactory
}

// OnlineRowsCall is what the host hands an INSTALLED Online source provider for a
// rows() call: the request a Built-in would receive and the Settings the host
// resolved, travelling together for the reason SubtitleSearchCall's do. The
// response is un-enveloped — a plain OnlineRowsResponse.
type OnlineRowsCall struct {
	Request  OnlineRowsRequest `json:"request"`
	Settings Settings          `json:"settings"`
}

// OnlineResolveCall is OnlineRowsCall for a resolve() call; its response is a
// plain OnlineResolveResponse.
type OnlineResolveCall struct {
	Request  OnlineResolveRequest `json:"request"`
	Settings Settings             `json:"settings"`
}
