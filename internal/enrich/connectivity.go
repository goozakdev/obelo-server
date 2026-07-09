package enrich

import (
	"context"
	"errors"
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
func TestConnection(ctx context.Context, slug, apiKey, baseURL, language string) (ok bool, detail string) {
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

	var (
		provider MetadataProvider
		ref      TitleRef
	)
	switch slug {
	case SlugTMDB:
		// The image host is irrelevant to a connectivity probe (no artwork bytes are
		// fetched), so the registry default suffices here.
		provider = NewTMDBProvider(apiKey, language, base, entry.DefaultImageBaseURL)
		ref = TitleRef{Kind: "movie", Title: "Inception", Year: 2010}
	case SlugOMDb:
		provider = NewOMDbProvider(apiKey, base)
		ref = TitleRef{Kind: "movie", Title: "Inception", Year: 2010}
	case SlugTheTVDB:
		provider = NewTheTVDBProvider(apiKey, base)
		ref = TitleRef{Kind: "show", Title: "Breaking Bad"}
	case SlugMusicBrainz:
		provider = NewMusicBrainzProvider(base, registryCoverArtBaseURL, language)
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	case SlugCoverArt:
		// Cover Art Archive is exercised through the MusicBrainz provider (they are
		// one source under the hood); probe an album so a cover lookup is attempted
		// against the supplied Cover Art host.
		provider = NewMusicBrainzProvider(registryMusicBrainzBaseURL, base, language)
		ref = TitleRef{Kind: "album", Title: "OK Computer", Artist: "Radiohead"}
	case SlugFanartTV:
		provider = NewFanartTVProvider(apiKey, base)
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead", MusicbrainzID: probeArtistMBID}
	case SlugTheAudioDB:
		provider = NewTheAudioDBProvider(apiKey, base, language)
		ref = TitleRef{Kind: "artist", Title: "Radiohead", Artist: "Radiohead"}
	default:
		return false, "unknown provider"
	}

	_, err := provider.Lookup(ctx, ref)
	switch {
	case err == nil, errors.Is(err, ErrNoMatch):
		return true, "connection succeeded"
	default:
		return false, err.Error()
	}
}
