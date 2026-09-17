package subfetch

import (
	"context"
	"errors"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// TestConnection performs a best-effort, single-shot connectivity/credential probe
// for one subtitle provider using the supplied (current-or-edited) credentials —
// the one place the settings surface makes a real outbound call, and only on an
// explicit Admin action (ADR-0021, mirroring enrich.TestConnection). It builds just
// that Plugin from the registry and issues one representative search: a normal
// result OR ErrNoMatch means the host answered and the key was accepted (ok); a
// transport/credential error means it did not (not ok, with the error as detail).
// A key-requiring provider with no key fails fast without any call.
//
// The Plugin is built through the same BuildProvider path a save would take, so the
// probe exercises the registration and the contract adapter rather than a private
// construction the real flow never uses.
func TestConnection(ctx context.Context, reg *pluginapi.Registry, slug, apiKey, baseURL string) (ok bool, detail string) {
	registration, found := reg.SubtitleProvider(slug)
	if !found {
		return false, "unknown provider"
	}
	if registration.Descriptor.RequiresKey && apiKey == "" {
		return false, "an API key is required to test this provider"
	}

	provider := BuildProvider(reg, []store.SubtitleProviderRow{{
		Slug: slug, Enabled: true, APIKey: apiKey, BaseURL: baseURL,
	}})
	if _, disabled := provider.(disabledProvider); disabled {
		return false, "this provider could not be built from these settings"
	}

	// A representative search: a well-known film in English. A host that answers
	// (even with zero candidates → ErrNoMatch) and accepts the key is "ok".
	_, err := provider.Search(ctx, SubtitleRef{Title: "Inception", Year: 2010}, "en")
	switch {
	case err == nil, errors.Is(err, ErrNoMatch):
		return true, "connection succeeded"
	default:
		return false, err.Error()
	}
}
