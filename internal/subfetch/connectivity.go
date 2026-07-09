package subfetch

import (
	"context"
	"errors"
)

// TestConnection performs a best-effort, single-shot connectivity/credential probe
// for one subtitle provider using the supplied (current-or-edited) credentials —
// the one place the settings surface makes a real outbound call, and only on an
// explicit Admin action (ADR-0021, mirroring enrich.TestConnection). It constructs
// just that provider and issues one representative Search: a normal result OR
// ErrNoMatch means the host answered and the key was accepted (ok); a
// transport/credential error means it did not (not ok, with the error as detail).
// A key-requiring provider with no key fails fast without any call.
func TestConnection(ctx context.Context, slug, apiKey, baseURL string) (ok bool, detail string) {
	entry, found := RegistryEntryFor(slug)
	if !found {
		return false, "unknown provider"
	}
	if entry.RequiresKey && apiKey == "" {
		return false, "an API key is required to test this provider"
	}
	base := baseURL
	if base == "" {
		base = entry.DefaultBaseURL
	}

	var provider SubtitleProvider
	switch slug {
	case SlugOpenSubtitles:
		provider = NewOpenSubtitlesProvider(apiKey, base)
	default:
		return false, "unknown provider"
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
