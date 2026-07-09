package enrich

import (
	"context"
	"errors"
	"testing"
)

// stubProvider is a fake MetadataProvider that returns a canned result (or error)
// and records how many times it was called and with what ref — enough to assert
// the chain's merge policy and that the image source is (or isn't) consulted.
type stubProvider struct {
	meta  TitleMetadata
	err   error
	calls int
	last  TitleRef

	// artwork/artworkErr are the canned ArtworkCandidates response (the artist-photo
	// picker seam, artwork-management/02); artworkCalls counts how often it was
	// consulted. Zero values mean "returns no images, no error".
	artwork      []ArtworkCandidate
	artworkErr   error
	artworkCalls int
}

func (s *stubProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	s.calls++
	s.last = ref
	return s.meta, s.err
}

// Search satisfies the MetadataProvider interface; the chain-merge tests exercise
// Lookup, not search, so a stub reports no searchable source.
func (s *stubProvider) Search(_ context.Context, _, _ string, _ SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

// ArtworkCandidates satisfies the MetadataProvider interface and returns the
// stub's canned candidate list (used by the artist-photo composition tests).
func (s *stubProvider) ArtworkCandidates(_ context.Context, _ TitleRef, _ string) ([]ArtworkCandidate, error) {
	s.artworkCalls++
	return s.artwork, s.artworkErr
}

// mbArtistResult mimics a MusicBrainz artist lookup: genres + a synthesized
// overview + a resolved MBID, but no artwork (its documented gap).
func mbArtistResult() TitleMetadata {
	return TitleMetadata{
		Matched:    true,
		Overview:   "English rock band from Oxford",
		Genres:     []string{"alternative rock", "art rock"},
		ExternalID: "mb-artist-1",
		Source:     "musicbrainz",
	}
}

func TestMusicChainMergesArtistImage(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	img := &stubProvider{meta: TitleMetadata{
		Matched: true, Source: "fanart.tv",
		Artwork: []ArtworkRef{{Role: "poster", URL: "https://fanart/thumb.jpg"}},
	}}
	chain := NewMusicChainProvider(mb, img, nil)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// The image is added...
	if len(got.Artwork) != 1 || got.Artwork[0].Role != "poster" || got.Artwork[0].URL != "https://fanart/thumb.jpg" {
		t.Errorf("artwork = %+v, want the fanart poster", got.Artwork)
	}
	// ...while MusicBrainz stays authoritative for everything else.
	if got.Source != "musicbrainz" || got.ExternalID != "mb-artist-1" {
		t.Errorf("identity = %q/%q, want musicbrainz/mb-artist-1", got.Source, got.ExternalID)
	}
	if len(got.Genres) != 2 || got.Overview != "English rock band from Oxford" {
		t.Errorf("genres/overview overwritten: %+v", got)
	}
	// The image source was keyed by the MusicBrainz MBID.
	if img.calls != 1 || img.last.MusicbrainzID != "mb-artist-1" || img.last.Kind != "artist" {
		t.Errorf("image source call wrong: calls=%d last=%+v", img.calls, img.last)
	}
}

func TestMusicChainDoesNotOverrideExistingArtwork(t *testing.T) {
	// If MusicBrainz already carried a poster, the image source must not replace it
	// (fill-only). (MusicBrainz has none for artists today; this guards the policy.)
	mbMeta := mbArtistResult()
	mbMeta.Artwork = []ArtworkRef{{Role: "poster", URL: "https://mb/own.jpg"}}
	mb := &stubProvider{meta: mbMeta}
	img := &stubProvider{meta: TitleMetadata{Matched: true, Artwork: []ArtworkRef{{Role: "poster", URL: "https://fanart/thumb.jpg"}}}}
	chain := NewMusicChainProvider(mb, img, nil)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://mb/own.jpg" {
		t.Errorf("artwork = %+v, want MusicBrainz's own poster kept", got.Artwork)
	}
}

func TestMusicChainNoMBIDSkipsImage(t *testing.T) {
	mbMeta := mbArtistResult()
	mbMeta.ExternalID = "" // MusicBrainz didn't resolve an MBID
	mb := &stubProvider{meta: mbMeta}
	img := &stubProvider{meta: TitleMetadata{Matched: true, Artwork: []ArtworkRef{{Role: "poster", URL: "https://fanart/thumb.jpg"}}}}
	chain := NewMusicChainProvider(mb, img, nil)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if img.calls != 0 {
		t.Errorf("image source called %d times, want 0 (no MBID to key by)", img.calls)
	}
	if len(got.Artwork) != 0 {
		t.Errorf("artwork = %+v, want none", got.Artwork)
	}
}

func TestMusicChainImageErrorIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	img := &stubProvider{err: errors.New("fanart.tv timeout")}
	chain := NewMusicChainProvider(mb, img, nil)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if err != nil {
		t.Fatalf("a fanart.tv error must not fail the lookup; got %v", err)
	}
	// The MusicBrainz result is preserved intact, just without an image.
	if got.ExternalID != "mb-artist-1" || len(got.Genres) != 2 || len(got.Artwork) != 0 {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

func TestMusicChainNonArtistPassesThrough(t *testing.T) {
	mb := &stubProvider{meta: TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "rg-1",
		Artwork: []ArtworkRef{{Role: "cover", URL: "https://coverart/front"}}}}
	img := &stubProvider{}
	chain := NewMusicChainProvider(mb, img, nil)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if img.calls != 0 {
		t.Errorf("image source called for an album lookup (%d); want 0", img.calls)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].Role != "cover" {
		t.Errorf("album artwork disturbed: %+v", got.Artwork)
	}
}

func TestMusicChainMusicBrainzNoMatchPropagates(t *testing.T) {
	mb := &stubProvider{err: ErrNoMatch}
	img := &stubProvider{}
	chain := NewMusicChainProvider(mb, img, nil)

	if _, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch propagated from MusicBrainz", err)
	}
	if img.calls != 0 {
		t.Errorf("image source called after MusicBrainz no-match (%d); want 0", img.calls)
	}
}

// --- TheAudioDB (the fallback image + biography source) --------------------

func audiodbResult(thumb, bio string) TitleMetadata {
	m := TitleMetadata{Matched: true, Source: "theaudiodb"}
	if thumb != "" {
		m.Artwork = []ArtworkRef{{Role: "poster", URL: thumb}}
	}
	m.Overview = bio
	return m
}

func TestMusicChainTheAudioDBFillsImageWhenFanartHasNone(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	fanart := &stubProvider{err: ErrNoMatch} // fanart.tv has no image for this artist
	audioDB := &stubProvider{meta: audiodbResult("https://theaudiodb/thumb.jpg", "")}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://theaudiodb/thumb.jpg" {
		t.Errorf("artwork = %+v, want the TheAudioDB poster", got.Artwork)
	}
	// Fanart is still consulted first, keyed by the MBID.
	if fanart.calls != 1 || fanart.last.MusicbrainzID != "mb-artist-1" {
		t.Errorf("fanart call wrong: calls=%d last=%+v", fanart.calls, fanart.last)
	}
}

func TestMusicChainTheAudioDBNameFallbackWithNoMBID(t *testing.T) {
	mbMeta := mbArtistResult()
	mbMeta.ExternalID = "" // MusicBrainz didn't resolve an MBID
	mb := &stubProvider{meta: mbMeta}
	fanart := &stubProvider{meta: audiodbResult("https://fanart/thumb.jpg", "")}
	audioDB := &stubProvider{meta: audiodbResult("https://theaudiodb/thumb.jpg", "")}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// Fanart is MBID-keyed, so with no MBID it is skipped entirely.
	if fanart.calls != 0 {
		t.Errorf("fanart called with no MBID (%d); want 0", fanart.calls)
	}
	// TheAudioDB still matches by name, so the artist gets an image.
	if audioDB.calls != 1 || audioDB.last.Title != "Radiohead" || audioDB.last.MusicbrainzID != "" {
		t.Errorf("theaudiodb call wrong: calls=%d last=%+v", audioDB.calls, audioDB.last)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://theaudiodb/thumb.jpg" {
		t.Errorf("artwork = %+v, want the TheAudioDB poster via name lookup", got.Artwork)
	}
}

func TestMusicChainFanartImagePreferredOverTheAudioDB(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	fanart := &stubProvider{meta: audiodbResult("https://fanart/thumb.jpg", "")}
	audioDB := &stubProvider{meta: audiodbResult("https://theaudiodb/thumb.jpg", "real bio")}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	// The fanart poster wins (fill-only); TheAudioDB's poster does not replace it...
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://fanart/thumb.jpg" {
		t.Errorf("artwork = %+v, want fanart poster preferred", got.Artwork)
	}
	// ...but its biography is still adopted (independent of the image).
	if got.Overview != "real bio" {
		t.Errorf("overview = %q, want the TheAudioDB bio", got.Overview)
	}
}

func TestMusicChainTheAudioDBBioPreferredOverSynthesizedOverview(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	audioDB := &stubProvider{meta: audiodbResult("", "A real, sourced biography.")}
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "A real, sourced biography." {
		t.Errorf("overview = %q, want the real TheAudioDB bio", got.Overview)
	}
	// Identity and genres stay MusicBrainz's.
	if got.Source != "musicbrainz" || got.ExternalID != "mb-artist-1" || len(got.Genres) != 2 {
		t.Errorf("MusicBrainz authority disturbed: %+v", got)
	}
}

func TestMusicChainRetainsMusicBrainzBioWhenTheAudioDBHasNone(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	audioDB := &stubProvider{meta: audiodbResult("https://theaudiodb/thumb.jpg", "")} // image, no bio
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "English rock band from Oxford" {
		t.Errorf("overview = %q, want the MusicBrainz overview retained", got.Overview)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://theaudiodb/thumb.jpg" {
		t.Errorf("artwork = %+v, want the TheAudioDB poster", got.Artwork)
	}
}

func TestMusicChainTheAudioDBErrorIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	fanart := &stubProvider{err: ErrNoMatch}
	audioDB := &stubProvider{err: errors.New("theaudiodb timeout")}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("a TheAudioDB error must not fail the lookup; got %v", err)
	}
	// The MusicBrainz result (overview, genres, identity) is preserved intact.
	if got.ExternalID != "mb-artist-1" || len(got.Genres) != 2 ||
		got.Overview != "English rock band from Oxford" || len(got.Artwork) != 0 {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

// --- track synopses --------------------------------------------------------

// mbTrackResult mimics a MusicBrainz track lookup: a canonical title + recording
// MBID, but no Overview (its documented gap).
func mbTrackResult() TitleMetadata {
	return TitleMetadata{Matched: true, Name: "Creep", ExternalID: "rec-1", Source: "musicbrainz"}
}

func TestMusicChainTrackSynopsisFilledFromTheAudioDB(t *testing.T) {
	mb := &stubProvider{meta: mbTrackResult()}
	fanart := &stubProvider{} // artist-only; must never be consulted for a track
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Source: "theaudiodb", Overview: "A real, sourced synopsis."}}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Overview != "A real, sourced synopsis." {
		t.Errorf("overview = %q, want the TheAudioDB synopsis", got.Overview)
	}
	// The canonical title and identity stay MusicBrainz's.
	if got.Name != "Creep" || got.ExternalID != "rec-1" || got.Source != "musicbrainz" {
		t.Errorf("MusicBrainz canonical/identity disturbed: %+v", got)
	}
	// fanart.tv is artist-only and is not consulted for a track.
	if fanart.calls != 0 {
		t.Errorf("fanart called for a track (%d); want 0", fanart.calls)
	}
	// TheAudioDB is keyed by the recording MBID and carries the track + artist.
	if audioDB.calls != 1 || audioDB.last.Kind != "track" || audioDB.last.MusicbrainzID != "rec-1" ||
		audioDB.last.Track != "Creep" || audioDB.last.Artist != "Radiohead" {
		t.Errorf("theaudiodb track call wrong: calls=%d last=%+v", audioDB.calls, audioDB.last)
	}
}

func TestMusicChainTrackDoesNotOverwriteExistingOverview(t *testing.T) {
	// If MusicBrainz ever carried a track Overview, the synopsis source must not
	// replace it (fill-only). MusicBrainz has none today; this guards the policy.
	mbMeta := mbTrackResult()
	mbMeta.Overview = "A hypothetical MusicBrainz synopsis."
	mb := &stubProvider{meta: mbMeta}
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Source: "theaudiodb", Overview: "TheAudioDB synopsis."}}
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep"})
	if got.Overview != "A hypothetical MusicBrainz synopsis." {
		t.Errorf("overview = %q, want the MusicBrainz overview kept", got.Overview)
	}
	// Fill-only short-circuits: a non-empty Overview means TheAudioDB isn't consulted.
	if audioDB.calls != 0 {
		t.Errorf("theaudiodb consulted despite a non-empty overview (%d); want 0", audioDB.calls)
	}
}

func TestMusicChainTrackTheAudioDBNoMatchIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbTrackResult()}
	audioDB := &stubProvider{err: ErrNoMatch}
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep"})
	if err != nil {
		t.Fatalf("a TheAudioDB no-match must not fail the lookup; got %v", err)
	}
	// The MusicBrainz result is preserved intact, just without a synopsis.
	if got.Name != "Creep" || got.ExternalID != "rec-1" || got.Overview != "" {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

func TestMusicChainTrackTheAudioDBErrorIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbTrackResult()}
	audioDB := &stubProvider{err: errors.New("theaudiodb timeout")}
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep"})
	if err != nil {
		t.Fatalf("a TheAudioDB error must not fail the lookup; got %v", err)
	}
	if got.Name != "Creep" || got.ExternalID != "rec-1" || got.Overview != "" {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

func TestMusicChainTheAudioDBDoesNotOverrideAlbumCover(t *testing.T) {
	// A non-artist kind never consults either image source (album cover untouched).
	mb := &stubProvider{meta: TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "rg-1",
		Artwork: []ArtworkRef{{Role: "cover", URL: "https://coverart/front"}}}}
	audioDB := &stubProvider{}
	chain := NewMusicChainProvider(mb, nil, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer"})
	if audioDB.calls != 0 {
		t.Errorf("TheAudioDB called for an album lookup (%d); want 0", audioDB.calls)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].Role != "cover" {
		t.Errorf("album artwork disturbed: %+v", got.Artwork)
	}
}

// --- Artist Photo candidates (artwork-management/02) ------------------------

// TestMusicChainArtistArtworkCandidatesUnion: the artist picker composes BOTH
// image sources — fanart.tv's full artistthumb[] (leading the grid) UNIONed with
// TheAudioDB's thumb — into one candidate list.
func TestMusicChainArtistArtworkCandidatesUnion(t *testing.T) {
	mb := &stubProvider{} // MusicBrainz has no artist images
	fanart := &stubProvider{artwork: []ArtworkCandidate{
		{URL: "https://fanart/best.jpg", Source: "fanart.tv"},
		{URL: "https://fanart/second.jpg", Source: "fanart.tv"},
	}}
	audioDB := &stubProvider{artwork: []ArtworkCandidate{
		{URL: "https://theaudiodb/thumb.jpg", Source: "theaudiodb"},
	}}
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %d, want 3 (2 fanart + 1 theaudiodb)", len(cands))
	}
	// fanart.tv leads the grid (it is the preferred source), then TheAudioDB.
	want := []string{"https://fanart/best.jpg", "https://fanart/second.jpg", "https://theaudiodb/thumb.jpg"}
	for i, w := range want {
		if cands[i].URL != w {
			t.Errorf("candidate[%d].URL = %q, want %q", i, cands[i].URL, w)
		}
	}
	// MusicBrainz owns no artist images, so it is never consulted for the picker.
	if mb.artworkCalls != 0 {
		t.Errorf("MusicBrainz consulted for artist candidates (%d); want 0", mb.artworkCalls)
	}
}

// TestMusicChainArtistArtworkCandidatesDedup: the same URL from both sources
// appears once (de-duplicated by URL).
func TestMusicChainArtistArtworkCandidatesDedup(t *testing.T) {
	fanart := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://shared/thumb.jpg", Source: "fanart.tv"}}}
	audioDB := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://shared/thumb.jpg", Source: "theaudiodb"}}}
	chain := NewMusicChainProvider(&stubProvider{}, fanart, audioDB)

	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].URL != "https://shared/thumb.jpg" {
		t.Errorf("candidates = %+v, want a single de-duplicated thumb", cands)
	}
}

// TestMusicChainArtistArtworkCandidatesDegrades: a source error is swallowed — the
// other source still populates the grid, and if BOTH fail the result is a graceful
// empty list, never a returned error (ADR-0001, so the API never 500s).
func TestMusicChainArtistArtworkCandidatesDegrades(t *testing.T) {
	// One source errors, the other succeeds → the survivor's images come back.
	fanart := &stubProvider{artworkErr: errors.New("fanart.tv timeout")}
	audioDB := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://theaudiodb/thumb.jpg", Source: "theaudiodb"}}}
	chain := NewMusicChainProvider(&stubProvider{}, fanart, audioDB)
	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates (one source down): %v", err)
	}
	if len(cands) != 1 || cands[0].URL != "https://theaudiodb/thumb.jpg" {
		t.Errorf("candidates = %+v, want just the surviving TheAudioDB thumb", cands)
	}

	// BOTH sources down → an empty list and NO error (the picker degrades to the
	// upload-only state; the handler must not 500).
	bothDown := NewMusicChainProvider(&stubProvider{},
		&stubProvider{artworkErr: errors.New("fanart.tv down")},
		&stubProvider{artworkErr: errors.New("theaudiodb down")})
	cands, err = bothDown.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil {
		t.Fatalf("ArtworkCandidates (both down) returned an error: %v, want graceful empty", err)
	}
	if len(cands) != 0 {
		t.Errorf("candidates = %+v, want empty when both sources fail", cands)
	}
}

// TestMusicChainArtistArtworkCandidatesNoSources: with no image sources configured
// (both nil — offline / no key), the artist picker is empty, not an error.
func TestMusicChainArtistArtworkCandidatesNoSources(t *testing.T) {
	chain := NewMusicChainProvider(&stubProvider{}, nil, nil)
	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("no-sources = (%+v, %v), want (nil, nil)", cands, err)
	}
}

// TestMusicChainAlbumArtworkCandidatesStayMusicBrainz: a non-artist role delegates
// straight to MusicBrainz (Cover Art Archive) and never consults the image sources.
func TestMusicChainAlbumArtworkCandidatesStayMusicBrainz(t *testing.T) {
	mb := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://caa/500.jpg", Source: "coverartarchive"}}}
	fanart := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://fanart/should-not-appear.jpg"}}}
	chain := NewMusicChainProvider(mb, fanart, nil)

	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-1"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 1 || cands[0].Source != "coverartarchive" {
		t.Errorf("album candidates = %+v, want just the MusicBrainz cover", cands)
	}
	if fanart.artworkCalls != 0 {
		t.Errorf("fanart.tv consulted for an album (%d); want 0", fanart.artworkCalls)
	}
}
