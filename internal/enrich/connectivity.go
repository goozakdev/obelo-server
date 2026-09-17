package enrich

import (
	"context"
	"errors"

	pluginapi "github.com/goozakdev/obelo-server/internal/pluginapi/v1"
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
// A source that goes through the contract is probed BY BUILDING ITS PLUGIN from
// the catalog (ADR-0057): the probe then exercises the registration and the
// adapter — the same path the real chain takes — rather than a private
// construction no production flow uses. The music sources are still constructed
// directly, because that is still how the music chain composes them.
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

	var (
		provider MetadataProvider
		ref      TitleRef
	)
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
		provider = NewMusicBrainzProvider(base, registryCoverArtBaseURL, language)
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	case SlugCoverArt:
		// Cover Art Archive is exercised through the MusicBrainz provider (they are
		// one source under the hood); probe an album so a cover lookup is attempted
		// against the supplied Cover Art host.
		provider = NewMusicBrainzProvider(registryMusicBrainzBaseURL, base, language)
		ref = TitleRef{Kind: "album", Title: "OK Computer", Artist: "Radiohead"}
	case SlugTheAudioDB:
		provider = NewTheAudioDBProvider(apiKey, base, language)
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	default:
		return false, "unknown provider"
	}
	if provider == nil {
		provider = cat.buildPlugin(slug, pluginapi.Settings{
			Enabled:  true,
			Secret:   apiKey,
			URL:      base,
			URL2:     entry.DefaultURL2,
			Language: language,
		})
	}
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
