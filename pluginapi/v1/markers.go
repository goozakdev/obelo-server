package v1

import "context"

// The Marker provider Extension point (ADR-0065, behind ADR-0057): look up the
// Markers someone else measured for a Movie or an Episode — where its Intro,
// Recap, Credits or Preview is — against a remote database. One call,
// request-response.
//
// The host asks lazily: the first time a File is played, and again only once the
// question changes. Hits and misses are both remembered.
//
// It decides nothing. The HOST judges every candidate: one timed for a recording
// more than a few seconds longer or shorter than the File being played is
// dropped outright — a Marker is either right where it is timed or not worth
// having — which is why every candidate states the length of the recording it
// was measured on. What survives is used only where neither the File's own
// chapters or edit-decision list (Local) nor the host's own detection (Detected)
// already says where that kind of Marker is.

// Marker kinds, the closed set a candidate may name.
const (
	MarkerIntro   = "intro"
	MarkerRecap   = "recap"
	MarkerCredits = "credits"
	MarkerPreview = "preview"
)

// MarkersRequest asks for one File's Markers.
type MarkersRequest struct {
	// Kind is the item's kind: "movie" or "episode".
	Kind string `json:"kind"`
	// Title is the item's title as this server files it: the film's, or the
	// Episode's own.
	Title string `json:"title"`
	// Year is the film's year; 0 when unknown or for an Episode.
	Year int `json:"year,omitempty"`
	// IDs is every external id this server holds for the item, keyed by
	// namespace (`imdb`, `tmdb`, …), as a Web reference provider is handed them.
	IDs map[string]string `json:"ids,omitempty"`
	// ShowTitle, ShowIDs, SeasonNumber and EpisodeNumber place an Episode in its
	// Show; all absent for a Movie. SeasonNumber 0 is Specials.
	ShowTitle     string            `json:"showTitle,omitempty"`
	ShowIDs       map[string]string `json:"showIds,omitempty"`
	SeasonNumber  int               `json:"seasonNumber,omitempty"`
	EpisodeNumber int               `json:"episodeNumber,omitempty"`
	// DurationMs is the length of the File being played, in milliseconds.
	DurationMs int64 `json:"durationMs"`
}

// MarkerCandidate is one Marker a provider found: a kind, a half-open
// [StartMs, EndMs) span, and the length of the recording it was timed on.
type MarkerCandidate struct {
	Kind    string `json:"kind"`
	StartMs int64  `json:"startMs"`
	EndMs   int64  `json:"endMs"`
	// DurationMs is the length of the recording the span was measured on. A
	// candidate that states none, or one more than a few seconds off the File's
	// own length, is rejected.
	DurationMs int64 `json:"durationMs"`
}

// MarkersResponse is what the call answers. An empty Markers is the normal
// "nothing found", and carries no Outcome for the reason WebReferencesResponse
// carries none: the only judgment is the host's.
type MarkersResponse struct {
	Markers []MarkerCandidate `json:"markers,omitempty"`
}

// MarkerProvider is the Go call surface of the Marker provider Extension point.
// An error means the Plugin failed: the host plays the File without that
// Plugin's answer and does not remember the failure as a miss.
type MarkerProvider interface {
	Markers(ctx context.Context, req MarkersRequest) (MarkersResponse, error)
}

// MarkerProviderFactory builds a Marker provider from the Settings the host
// resolved. Settings.Enabled is true for anything the host builds, URL is the
// Descriptor's default, and Values carries whatever an Installed plugin's
// manifest declared for itself.
type MarkerProviderFactory func(Settings) (MarkerProvider, error)

// MarkerProviderRegistration is what a Marker provider hands the host: what it
// is, and how to build it.
type MarkerProviderRegistration struct {
	Descriptor Descriptor
	New        MarkerProviderFactory
}

// MarkersCall is what the host hands an INSTALLED Marker provider for one call:
// the request a Built-in would receive and the Settings the host resolved,
// travelling together for the reason SubtitleSearchCall's do. The response is
// un-enveloped — a plain MarkersResponse.
type MarkersCall struct {
	Request  MarkersRequest `json:"request"`
	Settings Settings       `json:"settings"`
}
