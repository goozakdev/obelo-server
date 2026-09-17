package enrich

import (
	"context"
	"errors"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// A well-known MusicBrainz artist id (Radiohead), used only as a representative
// probe key for the fanart.tv connectivity test (fanart.tv is keyed by MBID).
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
// Every source is probed BY BUILDING ITS PLUGIN from the catalog (ADR-0057): the
// probe exercises the registration and the adapter — the same path the real chain
// takes — rather than a private construction no production flow uses. Cover Art
// Archive is the one that reads oddly and is the most honest of the set: it has no
// Plugin of its own, so it is probed exactly as it is USED, through the MusicBrainz
// Plugin with the supplied host as that Plugin's second URL.
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
	// differ for exactly one source, and that difference is what Cover Art Archive IS.
	buildSlug := slug
	settings := pluginapi.Settings{
		Enabled:  true,
		Secret:   apiKey,
		URL:      base,
		URL2:     entry.DefaultURL2,
		Language: language,
	}
	var ref TitleRef
	switch slug {
	case SlugTMDB:
		// The image host is irrelevant to a connectivity probe (no artwork bytes are
		// fetched), so the Descriptor default suffices here.
		ref = TitleRef{Kind: "movie", Title: "Inception", Year: 2010}
	case SlugOMDb:
		ref = TitleRef{Kind: "movie", Title: "Inception", Year: 2010}
	case SlugTheTVDB:
		ref = TitleRef{Kind: "show", Title: "Breaking Bad"}
	case SlugAniDB:
		// AniDB resolves BY anime id (no name search), so probe a well-known aid
		// (aid=1) — a normal record OR an unknown-aid no-match both prove the host
		// answered and the client name was accepted. The apiKey is the client name.
		ref = TitleRef{Kind: "show", Title: "Cowboy Bebop", AniDBID: "1"}
	case SlugFanartTV:
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead", MusicbrainzID: probeArtistMBID}
	case SlugMusicBrainz:
		// The artwork host is irrelevant to an artist probe, but the Plugin is built
		// with two hosts, so name the Cover Art default rather than leave it blank.
		settings.URL2 = registryCoverArtBaseURL
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	case SlugCoverArt:
		// Cover Art Archive has no Plugin of its own: it is the artwork host of the
		// MusicBrainz Plugin. So probe it the way it is used — build MusicBrainz with
		// the SUPPLIED host as its second URL and its own default as its API — and ask
		// for an album, so a cover lookup is attempted against the host under test.
		buildSlug = SlugMusicBrainz
		settings.Secret = ""
		settings.URL, settings.URL2 = registryMusicBrainzBaseURL, base
		ref = TitleRef{Kind: "album", Title: "OK Computer", Artist: "Radiohead"}
	case SlugTheAudioDB:
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	default:
		return false, "unknown provider"
	}
	provider := cat.buildPlugin(buildSlug, settings)
	if provider == nil {
		return false, "this provider cannot be built from these settings"
	}

	_, err := provider.Lookup(ctx, ref)
	switch {
	case err == nil, errors.Is(err, ErrNoMatch):
		return true, "connection succeeded"
	default:
		return false, err.Error()
	}
}
