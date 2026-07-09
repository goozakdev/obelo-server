package enrich

import (
	"context"
	"errors"
	"log"
)

// MusicChainProvider is the MetadataProvider wired into CompositeProvider.Music
// when an image source is configured. It keeps MusicBrainz authoritative for the
// Music kinds and lets up to two artist sources fill the gaps MusicBrainz
// documents — an artist image (it has none) and a real biography (its Overview is
// a synthesized stub). It runs MusicBrainz first; for an artist it then composes
// the configured sources:
//
//   - Image (fanart.tv) is the PREFERRED artist image. It is strictly MBID-keyed,
//     so it is consulted only when MusicBrainz resolved an MBID, and its poster
//     wins.
//   - ImageBio (TheAudioDB) is the FALLBACK image and the source of a real bio. It
//     matches by MBID when present and otherwise by NAME, so it still covers
//     artists MusicBrainz/fanart.tv couldn't key by id. Its poster only fills a
//     role the preferred source left empty; its non-empty biography is preferred
//     over MusicBrainz's synthesized Overview.
//
// A track carries MusicBrainz's other documented gap, a synopsis: MusicBrainz
// gives a recording only a canonical title, never an Overview. So for a track the
// chain asks ImageBio (TheAudioDB) for a synopsis and fills the Overview ONLY when
// MusicBrainz left it empty; fanart.tv (artist-only) is never consulted. Identity
// and the canonical title stay MusicBrainz's.
//
// Everything else stays fill-only (ADR-0002): genres, the album cover (Cover Art
// Archive), identity/ExternalID, and the canonical title always come from
// MusicBrainz. Lookups are best-effort (ADR-0001): a source with nothing to key
// by is skipped with no error, and a source error/timeout is logged and swallowed
// so the entity keeps its MusicBrainz metadata and the pass continues. The album
// kind passes straight through to MusicBrainz untouched.
type MusicChainProvider struct {
	MusicBrainz MetadataProvider
	Image       MetadataProvider // preferred, MBID-keyed artist image (fanart.tv); may be nil
	ImageBio    MetadataProvider // fallback image + real biography (TheAudioDB); may be nil
}

// NewMusicChainProvider builds a chain over an authoritative MusicBrainz provider
// and up to two optional artist sources: a preferred MBID-keyed image source
// (fanart.tv) and a fallback image-plus-biography source (TheAudioDB). Either may
// be nil; with both nil the chain is a pass-through to MusicBrainz.
func NewMusicChainProvider(musicBrainz, image, imageBio MetadataProvider) *MusicChainProvider {
	return &MusicChainProvider{MusicBrainz: musicBrainz, Image: image, ImageBio: imageBio}
}

// Search delegates to the authoritative MusicBrainz source: the fill-only image/
// bio sources (fanart.tv/TheAudioDB) never own a candidate list — they only
// decorate a record already pinned by id (ADR-0019). So the chain's Search is
// exactly MusicBrainz's.
func (p *MusicChainProvider) Search(ctx context.Context, kind, query string, opts SearchOptions) ([]Candidate, error) {
	return p.MusicBrainz.Search(ctx, kind, query, opts)
}

// ArtworkCandidates lists the images the Edit-item picker offers for a Music role.
// For an ALBUM (and anything else) it is exactly MusicBrainz's — the Cover Art
// Archive covers. For an ARTIST, MusicBrainz has no images, so the chain composes
// the two configured artist-image sources instead: fanart.tv's full artistthumb[]
// (the preferred source, which leads the grid) UNIONed with TheAudioDB's thumb
// (artwork-management/02). It is best-effort exactly like the Lookup chain: a
// source that is nil, has nothing to key by, or fails transiently is skipped (its
// error logged, not returned), so the picker degrades to whatever's available
// (ADR-0001) — an unreachable/unconfigured pair yields an empty grid the UI turns
// into the upload-only state, never a 500. Candidates are de-duplicated by URL;
// the service caps the count.
func (p *MusicChainProvider) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if ref.Kind != "artist" {
		return p.MusicBrainz.ArtworkCandidates(ctx, ref, role)
	}
	var cands []ArtworkCandidate
	seen := make(map[string]bool)
	add := func(src MetadataProvider) {
		if src == nil {
			return
		}
		got, err := src.ArtworkCandidates(ctx, ref, role)
		if err != nil {
			// A no-match / unsearchable source is the normal "no photos here" outcome;
			// a genuine failure is logged and treated as no data so the other source
			// still populates the grid and the pass continues (ADR-0001).
			if !errors.Is(err, ErrNoMatch) && !errors.Is(err, ErrSearchUnavailable) {
				log.Printf("juicebox: enrich artist artwork candidates (mbid %q): %v", ref.MusicbrainzID, err)
			}
			return
		}
		for _, c := range got {
			if c.URL == "" || seen[c.URL] {
				continue
			}
			seen[c.URL] = true
			cands = append(cands, c)
		}
	}
	add(p.Image)    // fanart.tv — preferred, leads the grid with its best-liked thumbs
	add(p.ImageBio) // TheAudioDB — fills the grid with its (single) artist thumb
	return cands, nil
}

// Lookup resolves ref via MusicBrainz, then for an artist fills the image from the
// preferred source and the image/biography from the fallback source, and for a
// track fills the synopsis from the fallback source.
func (p *MusicChainProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	meta, err := p.MusicBrainz.Lookup(ctx, ref)
	if err != nil {
		return meta, err // a MusicBrainz no-match/error is the chain's result
	}
	switch ref.Kind {
	case "artist":
		// fall through to artist composition below
	case "track":
		// MusicBrainz gives a track only a canonical title, never a synopsis, so its
		// Overview is empty. TheAudioDB (the fallback source) supplies one, keyed by the
		// recording MBID when present and otherwise by artist+name. fanart.tv is artist-
		// only and is never consulted. Fill-only: a non-empty MusicBrainz Overview (none
		// today) is never overwritten, and identity/canonical title are untouched.
		if p.ImageBio != nil && meta.Overview == "" {
			tref := TitleRef{Kind: "track", MusicbrainzID: meta.ExternalID, Track: ref.Track, Artist: ref.Artist}
			if syn, ok := p.lookup(ctx, p.ImageBio, tref, meta.ExternalID); ok && syn.Overview != "" {
				meta.Overview = syn.Overview
			}
		}
		return meta, nil
	default:
		return meta, nil // the album kind has no image/bio/synopsis gap to fill
	}

	// Preferred image source (fanart.tv): strictly MBID-keyed, so only when
	// MusicBrainz resolved an MBID. Its poster wins via fill-only merge.
	if p.Image != nil && meta.ExternalID != "" {
		if img, ok := p.lookup(ctx, p.Image, TitleRef{Kind: "artist", MusicbrainzID: meta.ExternalID}, meta.ExternalID); ok {
			meta.Artwork = mergeArtwork(meta.Artwork, img.Artwork)
		}
	}

	// Fallback source (TheAudioDB): keyed by MBID when present and otherwise by
	// name, so un-MBID'd artists still get an image. It fills the poster only if the
	// preferred source left it empty (fill-only), and supplies a real biography
	// preferred over MusicBrainz's synthesized Overview.
	if p.ImageBio != nil {
		ref := TitleRef{Kind: "artist", MusicbrainzID: meta.ExternalID, Title: ref.Title, Artist: ref.Artist}
		if bio, ok := p.lookup(ctx, p.ImageBio, ref, meta.ExternalID); ok {
			meta.Artwork = mergeArtwork(meta.Artwork, bio.Artwork)
			if bio.Overview != "" {
				meta.Overview = bio.Overview
			}
		}
	}
	return meta, nil
}

// lookup runs one auxiliary source, returning its result and whether it should be
// merged. A no-match is the normal "no data for this entity" outcome (ok=false,
// no log); a genuine failure is non-fatal — it is logged and treated as no data so
// the MusicBrainz result is preserved and the pass continues (ADR-0001).
func (p *MusicChainProvider) lookup(ctx context.Context, src MetadataProvider, ref TitleRef, mbid string) (TitleMetadata, bool) {
	meta, err := src.Lookup(ctx, ref)
	if err != nil {
		if !errors.Is(err, ErrNoMatch) {
			log.Printf("juicebox: enrich %s image/bio/synopsis (mbid %q): %v", ref.Kind, mbid, err)
		}
		return TitleMetadata{}, false
	}
	return meta, true
}

// mergeArtwork returns base extended with any of add's refs whose role base does
// not already carry — fill-only, so an existing MusicBrainz image for a role wins.
func mergeArtwork(base, add []ArtworkRef) []ArtworkRef {
	have := make(map[string]bool, len(base))
	for _, a := range base {
		have[a.Role] = true
	}
	for _, a := range add {
		if a.URL == "" || have[a.Role] {
			continue
		}
		base = append(base, a)
		have[a.Role] = true
	}
	return base
}
