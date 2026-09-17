package v1

import (
	"fmt"
	"sync"
	"sync/atomic"
)

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
// It is also SWAPPABLE, and that is the one thing about it worth reading twice.
// The three slices live behind an atomic pointer rather than in the struct, so a
// reader — the enrichment Catalog, the subtitle builder, the event-sink Manager,
// the settings handlers — holds the same *Registry for the life of the process and
// still sees a whole new set of Plugins the instant one is installed (Swap, below).
// A reader never observes a half-built value, and none of them had to learn what an
// install is.
type Registry struct {
	// mu serializes WRITERS. The registrations themselves are copy-on-write: a
	// Register builds the next state under this lock and publishes it in one
	// store, so a concurrent reader sees either the state before or the state
	// after and never a slice being appended to.
	mu sync.Mutex
	// state is nil for a zero-value Registry, which reads as empty — a narrow test
	// that wires no Plugins must not have to know this type has an initializer.
	state atomic.Pointer[registryState]
}

// registryState is everything a Registry holds, as one immutable value. It is
// replaced, never mutated, which is what makes the atomic read safe.
type registryState struct {
	metadataProviders []MetadataProviderRegistration
	subtitleProviders []SubtitleProviderRegistration
	eventSinks        []EventSinkRegistration
}

// NewRegistry returns an empty Registry. Nothing is registered until the
// composition root says so, explicitly.
func NewRegistry() *Registry {
	r := &Registry{}
	r.state.Store(&registryState{})
	return r
}

// load is every reader's entry point. A nil Registry and a zero-value one both
// read as empty.
func (r *Registry) load() *registryState {
	if r == nil {
		return &registryState{}
	}
	if s := r.state.Load(); s != nil {
		return s
	}
	return &registryState{}
}

// mutate applies f to a COPY of the current state and publishes the result. Every
// Register goes through it, which is why no reader ever sees a partially appended
// slice.
func (r *Registry) mutate(f func(*registryState)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.load()
	next := &registryState{
		metadataProviders: append([]MetadataProviderRegistration(nil), cur.metadataProviders...),
		subtitleProviders: append([]SubtitleProviderRegistration(nil), cur.subtitleProviders...),
		eventSinks:        append([]EventSinkRegistration(nil), cur.eventSinks...),
	}
	f(next)
	r.state.Store(next)
}

// Swap replaces everything this Registry holds with everything next holds, in ONE
// store, and is how a Plugin is installed or uninstalled on a running server
// (.scratch/plugin-system issue 10).
//
// The composition root builds a FRESH Registry from scratch — the Built-ins
// registered again, then the Installed plugins re-read from disk — and hands it
// here. Nothing is mutated in place and nothing is registered into a live value,
// so the failure mode this design exists to remove cannot happen: there is no
// moment at which a reader sees the Built-ins but not the Plugins, or a Plugin
// whose module is still compiling.
//
// Readers keep their pointer. That is the whole trick, and it is why installing a
// Plugin did not have to change the signature of the enrichment Catalog, the
// subtitle builder, the event-sink Manager or the settings Deps: each of them
// already asks this value a question on every read, and this makes the answer
// current.
//
// Swapping does NOT rebuild anything downstream. A composed provider chain or a
// live sink is built from a snapshot of this Registry and keeps running until its
// own Manager is told to Reload — which the installer does, in order, right after
// calling this.
func (r *Registry) Swap(next *Registry) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Store(next.load())
}

// RegisterMetadataProvider adds one Metadata provider Plugin. Registration order
// is preserved and it MATTERS here in a way it does not for subtitles: it is the
// order the settings screen lists sources in, the order the fill-only Supplements
// are composed behind the Authoritative provider (ADR-0027 keeps one global order —
// there is no per-Library reordering), and the first authoritative-role Full
// provider of a kind is that kind's global default lead.
//
// It PANICS on a missing or duplicate slug, for the same reason its Subtitle
// provider sibling does: registration is a composition-root act, and two Plugins
// claiming one slug would make a persisted settings row ambiguous.
//
// A nil New is ALLOWED and means what MetadataProviderRegistration documents — the
// static facts are registered while the host still builds the source itself.
func (r *Registry) RegisterMetadataProvider(reg MetadataProviderRegistration) {
	if reg.Descriptor.Slug == "" {
		panic("pluginapi: metadata provider registered with no slug")
	}
	if _, exists := r.MetadataProvider(reg.Descriptor.Slug); exists {
		panic(fmt.Sprintf("pluginapi: metadata provider %q registered twice", reg.Descriptor.Slug))
	}
	reg.Descriptor.ExtensionPoint = ExtensionMetadataProvider
	r.mutate(func(s *registryState) {
		s.metadataProviders = append(s.metadataProviders, reg)
	})
}

// MetadataProviders returns the registered Metadata providers in registration
// order. The slice is a copy, so a caller iterating it cannot reorder what the
// next caller sees.
func (r *Registry) MetadataProviders() []MetadataProviderRegistration {
	if r == nil {
		return nil
	}
	cur := r.load()
	out := make([]MetadataProviderRegistration, len(cur.metadataProviders))
	copy(out, cur.metadataProviders)
	return out
}

// MetadataProvider returns the registration for a slug, or ok=false for a slug no
// Plugin claimed (which the settings API rejects as an unknown provider).
func (r *Registry) MetadataProvider(slug string) (MetadataProviderRegistration, bool) {
	for _, reg := range r.load().metadataProviders {
		if reg.Descriptor.Slug == slug {
			return reg, true
		}
	}
	return MetadataProviderRegistration{}, false
}

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
	r.mutate(func(s *registryState) {
		s.subtitleProviders = append(s.subtitleProviders, reg)
	})
}

// SubtitleProviders returns the registered Subtitle providers in registration
// order. The slice is a copy, so a caller iterating it cannot reorder what the
// next caller sees.
func (r *Registry) SubtitleProviders() []SubtitleProviderRegistration {
	if r == nil {
		return nil
	}
	cur := r.load()
	out := make([]SubtitleProviderRegistration, len(cur.subtitleProviders))
	copy(out, cur.subtitleProviders)
	return out
}

// SubtitleProvider returns the registration for a slug, or ok=false for a slug no
// Plugin claimed (which the settings API rejects as an unknown provider).
func (r *Registry) SubtitleProvider(slug string) (SubtitleProviderRegistration, bool) {
	for _, reg := range r.load().subtitleProviders {
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
	r.mutate(func(s *registryState) {
		s.eventSinks = append(s.eventSinks, reg)
	})
}

// EventSinks returns the registered Event sinks in registration order, as a copy.
func (r *Registry) EventSinks() []EventSinkRegistration {
	if r == nil {
		return nil
	}
	cur := r.load()
	out := make([]EventSinkRegistration, len(cur.eventSinks))
	copy(out, cur.eventSinks)
	return out
}

// EventSink returns the registration for a slug, or ok=false for a slug no Plugin
// claimed (which the sink settings API rejects as unknown).
func (r *Registry) EventSink(slug string) (EventSinkRegistration, bool) {
	for _, reg := range r.load().eventSinks {
		if reg.Descriptor.Slug == slug {
			return reg, true
		}
	}
	return EventSinkRegistration{}, false
}
