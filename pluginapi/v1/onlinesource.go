package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
)

// The Online source provider Extension point (ADR-0068, behind ADR-0057): a Plugin
// that lets a User browse and watch something outside the household — a PeerTube
// instance, the Internet Archive — without any of it becoming part of the
// catalog. Four request-response calls: rows() answers the source's Online rows,
// row() answers the next page of one row by an opaque cursor, search() answers the
// items matching a query typed on the source's page, and resolve() turns one
// Online item into the URL(s) it plays from.
//
// The Plugin RESOLVES; the host PLAYS. A Plugin only ever supplies URLs and data.
// The host decides whether to fetch, how to play and who may see: it relays a
// muxed variant the client can play untouched, keeps every upstream URL to itself
// (a client is handed a stream token, never the source's address), and applies the
// Playback ceiling exactly as it does to a Title.
//
// Nothing here is persisted. An answer is used for the call that asked for it.

// The shapes a variant takes (OnlineVariant.Kind). A muxed variant is audio and
// video in a single file at a single URL; the host relays it when the client can
// play it as is. A split variant carries the picture and the sound at two URLs, and
// a manifest variant is an HLS or DASH manifest; neither can be relayed as a
// single file, so the host has ffmpeg read them.
const (
	OnlineVariantMuxed    = "muxed"
	OnlineVariantSplit    = "split"
	OnlineVariantManifest = "manifest"
)

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

// UnmarshalJSON reads an item whose durationMs is not a JSON number as a NEGATIVE
// duration instead of failing the whole answer, so the host can drop that one item
// as malformed and keep its valid siblings.
func (it *OnlineItem) UnmarshalJSON(b []byte) error {
	type plain OnlineItem
	aux := struct {
		*plain
		DurationMs json.RawMessage `json:"durationMs"`
	}{plain: (*plain)(it)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	it.DurationMs = 0
	if raw := bytes.TrimSpace(aux.DurationMs); len(raw) > 0 && string(raw) != "null" {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil || f < 0 || f >= math.MaxInt64 {
			it.DurationMs = -1
		} else {
			it.DurationMs = int64(f)
		}
	}
	return nil
}

// OnlineRow is one labelled shelf of an Online source's page.
type OnlineRow struct {
	ID    string       `json:"id"`
	Label string       `json:"label"`
	Items []OnlineItem `json:"items"`
	// NextCursor is the opaque token that asks row() for the page after these
	// items; empty when the row has no more.
	NextCursor string `json:"nextCursor,omitempty"`
}

// OnlineRowsRequest asks for the source's rows. It carries nothing today; it is a
// struct so a later field (a cursor, a search) is additive.
type OnlineRowsRequest struct{}

// OnlineRowsResponse is the source page: its rows, in the order to show them.
type OnlineRowsResponse struct {
	Rows []OnlineRow `json:"rows"`
}

// OnlineRowRequest asks for the page of one row after Cursor, the token the
// previous answer (rows() or row()) named. The cursor is the Plugin's own: the
// host never reads it, only hands it back.
type OnlineRowRequest struct {
	RowID  string `json:"rowId"`
	Cursor string `json:"cursor"`
}

// OnlineRowResponse is one more page of a row: its items, and the cursor for the
// page after them, empty on the last.
type OnlineRowResponse struct {
	Items      []OnlineItem `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
}

// OnlineSearchRequest asks for the items matching Query, as typed on the source's
// page. The host trims it and never sends an empty one.
type OnlineSearchRequest struct {
	Query string `json:"query"`
}

// OnlineSearchResponse is the matching items, best first. There is no cursor: a
// search is one page, and the host caps it as it caps a row page.
type OnlineSearchResponse struct {
	Items []OnlineItem `json:"items"`
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
	// Kind is OnlineVariantMuxed, OnlineVariantSplit or OnlineVariantManifest;
	// empty reads as muxed.
	Kind string `json:"kind,omitempty"`
	// URL is an https URL of the media file (muxed) or of the manifest (manifest).
	URL string `json:"url,omitempty"`
	// VideoURL and AudioURL are the https URLs of a split variant's picture and
	// sound.
	VideoURL string `json:"videoUrl,omitempty"`
	AudioURL string `json:"audioUrl,omitempty"`
	// Container names the file's container as a client profile does ("mp4",
	// "webm"; "hls" or "dash" for a manifest), and Codecs its codecs ("h264",
	// "aac"), video first.
	Container string   `json:"container"`
	Codecs    []string `json:"codecs,omitempty"`
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
	Row(ctx context.Context, req OnlineRowRequest) (OnlineRowResponse, error)
	Search(ctx context.Context, req OnlineSearchRequest) (OnlineSearchResponse, error)
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

// OnlineRowCall is OnlineRowsCall for a row() call; its response is a plain
// OnlineRowResponse.
type OnlineRowCall struct {
	Request  OnlineRowRequest `json:"request"`
	Settings Settings         `json:"settings"`
}

// OnlineSearchCall is OnlineRowsCall for a search() call; its response is a plain
// OnlineSearchResponse.
type OnlineSearchCall struct {
	Request  OnlineSearchRequest `json:"request"`
	Settings Settings            `json:"settings"`
}

// OnlineResolveCall is OnlineRowsCall for a resolve() call; its response is a
// plain OnlineResolveResponse.
type OnlineResolveCall struct {
	Request  OnlineResolveRequest `json:"request"`
	Settings Settings             `json:"settings"`
}
