package enrich

import (
	"context"
	"errors"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A well-known MusicBrainz artist id (Radiohead), used only as a representative
// probe key for the fanart.tv connectivity test (fanart.tv is keyed by MBID). It
// lives here beside the test it serves; the Descriptor that declares the probe
// reads it (see MetadataPlugins).
const probeArtistMBID = "a74b1b7f-71a5-4011-9441-d0b5e4122711"

// TestConnection performs a best-effort, single-shot connectivity/credential
// probe for one provider using the supplied (current-or-edited) credentials — the
// one place the settings surface makes a real outbound call, and only on an
// explicit Admin action (metadata-providers 02). It constructs just that
// provider (never the whole chain) and issues one representative Lookup: a normal
// result OR ErrNoMatch means the host answered and any key was accepted (ok); a
// transport/credential error means it did not (not ok, with the error as detail).
// The caller supplies a bounded context so a hung host can't stall the request.
// A key-requiring provider with no key fails fast without any call.
//
// WHAT TO LOOK UP IS THE PLUGIN'S OWN DECLARATION (ADR-0059 decision 8), carried
// on Descriptor.Probe. This used to be a switch over eight slugs, each with a
// reference the author of THIS file happened to know that source could answer,
// and an Installed provider fell off the end of it into "unknown provider" —
// which is to say the host's most operator-visible provider feature only worked
// for the providers the host was written around. The judgment stays the host's
// and is unchanged: matched or no-match proves the host was reached and the
// credential accepted; unavailable, a refusal or a fetch error is a failed test
// with the reason as the sentence.
//
// Every source is probed BY BUILDING ITS PLUGIN from the catalog (ADR-0057): the
// probe exercises the registration and the adapter — the same path the real chain
// takes — rather than a private construction no production flow uses.
func TestConnection(ctx context.Context, cat Catalog, slug, apiKey, baseURL, language string) (ok bool, detail string) {
	entry, found := cat.Entry(slug)
	if !found {
		return false, "unknown provider"
	}
	if entry.RequiresKey && apiKey == "" {
		return false, "an API key is required to test this provider"
	}
	base := baseURL
	if base == "" {
		base = entry.DefaultURL
	}

	// buildSlug is whose Plugin is built; settings are what it is built FROM. They
	// differ for exactly one source, below.
	buildSlug := slug
	settings := pluginapi.Settings{
		Enabled:  true,
		Secret:   apiKey,
		URL:      base,
		URL2:     entry.DefaultURL2,
		Language: language,
	}

	// THE ONE REMAINING SPECIAL CASE (.scratch/bundled-plugins: issue 01), and it is
	// the Cover Art Archive's, which issue 06 deletes by folding that host into the
	// MusicBrainz plugin's manifest as its second URL.
	//
	// Cover Art Archive has no Plugin of its own: it is the artwork host of the
	// MusicBrainz Plugin. So it is probed the way it is USED — build MusicBrainz
	// with the SUPPLIED host as its second URL and MusicBrainz's own default as its
	// API — and its Descriptor's probe asks for an ALBUM, so a cover lookup is
	// attempted against the host under test. The mirror image is MusicBrainz's own
	// test, which names the Cover Art default rather than leaving the second host
	// blank, because the Plugin is built with two hosts either way.
	switch slug {
	case SlugCoverArt:
		buildSlug = SlugMusicBrainz
		settings.Secret = ""
		settings.URL, settings.URL2 = registryMusicBrainzBaseURL, base
	case SlugMusicBrainz:
		settings.URL2 = registryCoverArtBaseURL
	}

	if entry.Probe == nil {
		return false, "this provider declares no connection probe"
	}
	provider := cat.buildPlugin(buildSlug, settings)
	if provider == nil {
		return false, "this provider cannot be built from these settings"
	}

	_, err := provider.Lookup(ctx, titleRefFromWire(*entry.Probe))
	switch {
	case err == nil, errors.Is(err, ErrNoMatch):
		return true, "connection succeeded"
	default:
		return false, err.Error()
	}
}
