package v1

import "context"

// The Web reference provider Extension point (ADR-0063's note on it, behind
// ADR-0057): turn the external ids an item already holds into Web references — a
// label and an https address where a person can read about it elsewhere ("IMDb",
// "Trakt"). One call, request-response, plain data in and plain data out.
//
// It is a PURE computation. Everything it needs is in the request, it needs no
// network and the host gives it none: an Installed Web reference provider that
// reaches for http_fetch while answering is refused, not served.
//
// It also decides nothing. The HOST keeps a reference only when it is https and
// only when it is keyed to an id the host itself holds for that item; anything
// else is dropped before a response is written — never shown, never flagged,
// simply absent. That is why every reference names the id it was built from.

// WebReferencesRequest asks for the references one item's ids point at.
type WebReferencesRequest struct {
	// Kind is the fine entity kind of the item: "movie" | "episode" | "track", and
	// later "show" | "artist" | "album". A Plugin that has nothing to say for a
	// kind answers an empty list.
	Kind string `json:"kind"`
	// IDs is every external id the host holds for THIS item — not its parents' —
	// keyed by External-id namespace (ADR-0060): "tmdb", "imdb", "musicbrainz", or
	// a third-party source's own. These are the only ids a reference may be keyed
	// to.
	IDs map[string]string `json:"ids,omitempty"`
}

// WebReference is one place a person can read about the item.
type WebReference struct {
	// Namespace and ID name the held id this reference was built from. The host
	// drops a reference whose pair is not one it sent, so a Plugin cannot attach
	// a link to an id the item does not have.
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
	// Label is the human name of the destination ("IMDb").
	Label string `json:"label"`
	// URL is the absolute address. Only https survives the host's check.
	URL string `json:"url"`
}

// WebReferencesResponse is what the call answers. An empty list is the normal
// "nothing to link to". It carries no Outcome: there is no source to be
// unavailable and no judgment for a Plugin to report — the host makes the only
// one there is.
type WebReferencesResponse struct {
	References []WebReference `json:"references,omitempty"`
}

// WebReferenceProvider is the Go call surface of the Web reference provider
// Extension point. An error means the Plugin failed, and the host shows the item
// without that Plugin's references.
type WebReferenceProvider interface {
	Links(ctx context.Context, req WebReferencesRequest) (WebReferencesResponse, error)
}

// WebReferenceProviderFactory builds a Web reference provider from the Settings
// the host resolved. This Extension point has no settings of its own beyond being
// switched on: Settings.Enabled is true for anything the host builds, and Values
// carries whatever an Installed plugin's manifest declared for itself.
type WebReferenceProviderFactory func(Settings) (WebReferenceProvider, error)

// WebReferenceProviderRegistration is what a Web reference provider hands the
// host: what it is, and how to build it.
type WebReferenceProviderRegistration struct {
	Descriptor Descriptor
	New        WebReferenceProviderFactory
}

// WebReferencesCall is what the host hands an INSTALLED Web reference provider
// for one call: the request a Built-in would receive and the Settings the host
// resolved, travelling together for the reason SubtitleSearchCall's do. The
// response is un-enveloped — a plain WebReferencesResponse.
type WebReferencesCall struct {
	Request  WebReferencesRequest `json:"request"`
	Settings Settings             `json:"settings"`
}
