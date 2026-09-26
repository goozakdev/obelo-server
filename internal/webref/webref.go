// Package webref is the host half of the Web reference provider Extension point:
// it asks every registered Web reference provider what an item's external ids
// point at, and keeps only what this server is prepared to show.
//
// The judgment is the HOST's, and it is two rules (ADR-0063's note on this
// seam): a reference survives only when its address is https, and only when it
// is keyed to an id this server itself holds for the item. Anything else is
// dropped here, before a response is written — never shown, never flagged,
// simply absent. A Plugin is never asked whether its own answer is acceptable.
package webref

import (
	"context"
	"log"
	"net/url"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Reference is one Web reference as a client sees it: a label and an address.
type Reference struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// maxReferences is how many Web references one item keeps. An item's page links
// out to a handful of sites; past this a provider is not linking, it is flooding.
const maxReferences = 50

// Collect asks every Web reference provider in reg that serves kind for the
// references held points at, and returns the ones that pass, in registration
// order, each address once — at most maxReferences of them, the first in that
// order.
//
// held is every external id the host holds for the item, keyed by namespace. A
// provider that cannot be built or fails its call contributes nothing and costs
// the item nothing else: the page still renders (ADR-0001).
func Collect(ctx context.Context, reg *pluginapi.Registry, kind string, held map[string]string) []Reference {
	if len(held) == 0 {
		return nil
	}
	var out []Reference
	seen := map[string]bool{}
	for _, r := range reg.WebReferenceProviders() {
		if !serves(r.Descriptor, kind) {
			continue
		}
		p, err := r.New(pluginapi.Settings{Enabled: true})
		if err != nil {
			log.Printf("obelo: web references from %s skipped: %v", r.Descriptor.Slug, err)
			continue
		}
		resp, err := p.Links(ctx, pluginapi.WebReferencesRequest{Kind: kind, IDs: copyIDs(held)})
		if err != nil {
			log.Printf("obelo: web references from %s: %v", r.Descriptor.Slug, err)
			continue
		}
		for _, ref := range resp.References {
			if !keep(ref, held) || seen[ref.URL] {
				continue
			}
			seen[ref.URL] = true
			out = append(out, Reference{Label: ref.Label, URL: ref.URL})
			if len(out) == maxReferences {
				return out
			}
		}
	}
	return out
}

// keep is the host's whole judgment on one reference: keyed to an id held for
// the item, labelled, and an absolute https address with no credentials in it.
func keep(ref pluginapi.WebReference, held map[string]string) bool {
	if ref.Label == "" || ref.ID == "" || held[ref.Namespace] != ref.ID {
		return false
	}
	u, err := url.Parse(ref.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return false
	}
	return true
}

// serves reports whether a provider declared the coarse kind an item's fine kind
// belongs to. A provider that declares no kinds serves none, as
// Descriptor.Serves says for every seam.
func serves(d pluginapi.Descriptor, kind string) bool {
	return d.Serves(coarseKind(kind))
}

// coarseKind maps a fine entity kind onto the media-kind group a Descriptor
// declares.
func coarseKind(kind string) string {
	switch kind {
	case "artist", "album", "track":
		return pluginapi.KindMusic
	}
	return pluginapi.KindVideo
}

// copyIDs hands each provider its own map, so one Plugin's adapter can never
// change what the next one is asked — or what the host checks against.
func copyIDs(held map[string]string) map[string]string {
	out := make(map[string]string, len(held))
	for k, v := range held {
		out[k] = v
	}
	return out
}
