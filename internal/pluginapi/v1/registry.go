package v1

import "fmt"

// Registry is the set of Plugins one running server has, held as a VALUE the
// composition root builds and hands to the builders and the settings handlers
// (ADR-0057 decision 5). It replaces the package-level catalogs each domain kept:
// there is no init-time registration anywhere, so a test composes a server with
// exactly the Plugins it means and no others, and a Phase 2 loader adds Installed
// plugins to the same value the Built-ins registered into.
//
// It is the one type in this package that is not wire data — it is host-side
// composition, not something that crosses the contract. A nil *Registry behaves
// like an empty one so a narrow test that wires no Plugins reads as "no Plugins"
// rather than panicking.
type Registry struct {
	subtitleProviders []SubtitleProviderRegistration
	eventSinks        []EventSinkRegistration
}

// NewRegistry returns an empty Registry. Nothing is registered until the
// composition root says so, explicitly.
func NewRegistry() *Registry { return &Registry{} }

// RegisterSubtitleProvider adds one Subtitle provider Plugin. Registration order
// is preserved, because it is the order the settings screen lists providers in and
// the order the builder considers them.
//
// It PANICS on a malformed or duplicate registration. That is a composition-root
// programming error — two Plugins claiming one slug would make the persisted
// settings row ambiguous — and it is better found at boot, loudly, than by an
// Admin whose key ended up in the wrong Plugin.
func (r *Registry) RegisterSubtitleProvider(reg SubtitleProviderRegistration) {
	if reg.Descriptor.Slug == "" {
		panic("pluginapi: subtitle provider registered with no slug")
	}
	if reg.New == nil {
		panic(fmt.Sprintf("pluginapi: subtitle provider %q registered with no factory", reg.Descriptor.Slug))
	}
	if _, exists := r.SubtitleProvider(reg.Descriptor.Slug); exists {
		panic(fmt.Sprintf("pluginapi: subtitle provider %q registered twice", reg.Descriptor.Slug))
	}
	reg.Descriptor.ExtensionPoint = ExtensionSubtitleProvider
	r.subtitleProviders = append(r.subtitleProviders, reg)
}

// SubtitleProviders returns the registered Subtitle providers in registration
// order. The slice is a copy, so a caller iterating it cannot reorder what the
// next caller sees.
func (r *Registry) SubtitleProviders() []SubtitleProviderRegistration {
	if r == nil {
		return nil
	}
	out := make([]SubtitleProviderRegistration, len(r.subtitleProviders))
	copy(out, r.subtitleProviders)
	return out
}

// SubtitleProvider returns the registration for a slug, or ok=false for a slug no
// Plugin claimed (which the settings API rejects as an unknown provider).
func (r *Registry) SubtitleProvider(slug string) (SubtitleProviderRegistration, bool) {
	if r == nil {
		return SubtitleProviderRegistration{}, false
	}
	for _, reg := range r.subtitleProviders {
		if reg.Descriptor.Slug == slug {
			return reg, true
		}
	}
	return SubtitleProviderRegistration{}, false
}

// RegisterEventSink adds one Event sink Plugin, under the same rules as a
// Subtitle provider: registration order is preserved, and a malformed or
// duplicate registration PANICS at the composition root rather than leaving an
// Admin's signing secret on an ambiguous settings row.
func (r *Registry) RegisterEventSink(reg EventSinkRegistration) {
	if reg.Descriptor.Slug == "" {
		panic("pluginapi: event sink registered with no slug")
	}
	if reg.New == nil {
		panic(fmt.Sprintf("pluginapi: event sink %q registered with no factory", reg.Descriptor.Slug))
	}
	if _, exists := r.EventSink(reg.Descriptor.Slug); exists {
		panic(fmt.Sprintf("pluginapi: event sink %q registered twice", reg.Descriptor.Slug))
	}
	reg.Descriptor.ExtensionPoint = ExtensionEventSink
	r.eventSinks = append(r.eventSinks, reg)
}

// EventSinks returns the registered Event sinks in registration order, as a copy.
func (r *Registry) EventSinks() []EventSinkRegistration {
	if r == nil {
		return nil
	}
	out := make([]EventSinkRegistration, len(r.eventSinks))
	copy(out, r.eventSinks)
	return out
}

// EventSink returns the registration for a slug, or ok=false for a slug no Plugin
// claimed (which the sink settings API rejects as unknown).
func (r *Registry) EventSink(slug string) (EventSinkRegistration, bool) {
	if r == nil {
		return EventSinkRegistration{}, false
	}
	for _, reg := range r.eventSinks {
		if reg.Descriptor.Slug == slug {
			return reg, true
		}
	}
	return EventSinkRegistration{}, false
}
