package enrich

import (
	"context"
	"errors"
	"testing"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// stubProvider is a fake MetadataProvider that returns a canned result (or error)
// and records how many times it was called and with what ref — enough to assert
// the chain's merge policy and what each source was asked.
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

// gatedStub is a stubProvider that SELF-GATES the way the shipped Supplements do
// (ADR-0061): it no-matches a kind it doesn't serve, and — when mbidKeyed — a ref
// carrying no MusicBrainz id. That is fanart.tv (artist only, MBID only) and
// TheAudioDB (artist and track, MBID or name). The chain no longer knows either
// rule; these stand-ins are how the tests keep asserting the composed result.
type gatedStub struct {
	stubProvider
	kinds     map[string]bool
	mbidKeyed bool
}

func (g *gatedStub) serves(ref TitleRef) bool {
	if !g.kinds[ref.Kind] {
		return false
	}
	return !g.mbidKeyed || mergedExternalIDs(ref)[pluginapi.NamespaceMusicBrainz] != ""
}

func (g *gatedStub) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	g.last = ref
	if !g.serves(ref) {
		return TitleMetadata{}, ErrNoMatch
	}
	return g.stubProvider.Lookup(ctx, ref)
}

func (g *gatedStub) ArtworkCandidates(ctx context.Context, ref TitleRef, role string) ([]ArtworkCandidate, error) {
	if !g.serves(ref) {
		return nil, ErrSearchUnavailable
	}
	return g.stubProvider.ArtworkCandidates(ctx, ref, role)
}

// fanartLike is an artist-only, MBID-keyed Supplement with canned data.
func fanartLike(meta TitleMetadata) *gatedStub {
	return &gatedStub{stubProvider: stubProvider{meta: meta}, kinds: map[string]bool{"artist": true}, mbidKeyed: true}
}

// audioDBLike is an artist-and-track Supplement that also matches by name.
func audioDBLike(meta TitleMetadata) *gatedStub {
	return &gatedStub{stubProvider: stubProvider{meta: meta}, kinds: map[string]bool{"artist": true, "track": true}}
}

// mbArtistResult mimics a MusicBrainz artist lookup: genres + a SYNTHESIZED
// overview (declared so) + a resolved MBID, but no artwork (its documented gap).
func mbArtistResult() TitleMetadata {
	return TitleMetadata{
		Matched:             true,
		Overview:            "English rock band from Oxford",
		OverviewSynthesized: true,
		Genres:              []string{"alternative rock", "art rock"},
		ExternalID:          "mb-artist-1",
		Source:              pluginapi.NamespaceMusicBrainz,
	}
}

func posterResult(url string) TitleMetadata {
	return TitleMetadata{Matched: true, Artwork: []ArtworkRef{{Role: "poster", URL: url}}}
}

func TestMusicChainMergesArtistImage(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	img := fanartLike(posterResult("https://fanart/thumb.jpg"))
	chain := NewMusicChainProvider(mb, img)

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
	// The Supplement was keyed by the MBID the LEAD resolved (the caller's ref had
	// none), in the lead's own namespace, and kept the caller's title.
	if img.calls != 1 || img.last.MusicbrainzID != "mb-artist-1" || img.last.Kind != "artist" || img.last.Title != "Radiohead" {
		t.Errorf("image source call wrong: calls=%d last=%+v", img.calls, img.last)
	}
}

func TestMusicChainDoesNotOverrideExistingArtwork(t *testing.T) {
	// If MusicBrainz already carried a poster, the image source must not replace it
	// (fill-only). (MusicBrainz has none for artists today; this guards the policy.)
	mbMeta := mbArtistResult()
	mbMeta.Artwork = []ArtworkRef{{Role: "poster", URL: "https://mb/own.jpg"}}
	mb := &stubProvider{meta: mbMeta}
	img := fanartLike(posterResult("https://fanart/thumb.jpg"))
	chain := NewMusicChainProvider(mb, img)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://mb/own.jpg" {
		t.Errorf("artwork = %+v, want MusicBrainz's own poster kept", got.Artwork)
	}
}

// TestMusicChainNoMBIDLeavesAnMBIDKeyedSourceEmpty: the chain no longer skips an
// MBID-keyed source by name; the source declines a ref with no MBID itself, and a
// no-match contributes nothing.
func TestMusicChainNoMBIDLeavesAnMBIDKeyedSourceEmpty(t *testing.T) {
	mbMeta := mbArtistResult()
	mbMeta.ExternalID = "" // MusicBrainz didn't resolve an MBID
	mb := &stubProvider{meta: mbMeta}
	img := fanartLike(posterResult("https://fanart/thumb.jpg"))
	chain := NewMusicChainProvider(mb, img)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if img.last.MusicbrainzID != "" {
		t.Errorf("the source was handed an MBID %q nobody resolved", img.last.MusicbrainzID)
	}
	if len(got.Artwork) != 0 {
		t.Errorf("artwork = %+v, want none", got.Artwork)
	}
}

func TestMusicChainImageErrorIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	img := &stubProvider{err: errors.New("fanart.tv timeout")}
	chain := NewMusicChainProvider(mb, img)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"})
	if err != nil {
		t.Fatalf("a fanart.tv error must not fail the lookup; got %v", err)
	}
	// The MusicBrainz result is preserved intact, just without an image.
	if got.ExternalID != "mb-artist-1" || len(got.Genres) != 2 || len(got.Artwork) != 0 {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

// TestMusicChainAlbumCoverIsKeptAndTheShippedPairAddNothing: an album now reaches
// the Supplements too (a Cover Art Archive plugin could fill one), but the
// shipped pair serve no album, and MusicBrainz's cover keeps its role.
func TestMusicChainAlbumCoverIsKeptAndTheShippedPairAddNothing(t *testing.T) {
	mb := &stubProvider{meta: TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "rg-1",
		Artwork: []ArtworkRef{{Role: "cover", URL: "https://coverart/front"}}}}
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg"))
	audioDB := audioDBLike(TitleMetadata{Matched: true, Overview: "an artist bio", Artwork: []ArtworkRef{{Role: "cover", URL: "https://theaudiodb/cover"}}})
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://coverart/front" || got.Overview != "" {
		t.Errorf("album disturbed by sources that serve no album: %+v", got)
	}
}

// TestMusicChainAnAlbumSupplementFillsAnEmptyCover is the case ADR-0059 decision
// 4 rejected a Cover Art Archive plugin for: an album Supplement fills the cover
// the lead left empty, and never replaces one it carried.
func TestMusicChainAnAlbumSupplementFillsAnEmptyCover(t *testing.T) {
	mb := &stubProvider{meta: TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "rg-1"}}
	caa := &stubProvider{meta: TitleMetadata{Matched: true, Artwork: []ArtworkRef{{Role: "cover", URL: "https://caa/front"}}}}
	chain := NewMusicChainProvider(mb, caa)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "album", Album: "OK Computer"})
	if len(got.Artwork) != 1 || got.Artwork[0].URL != "https://caa/front" {
		t.Errorf("artwork = %+v, want the album Supplement's cover", got.Artwork)
	}
	if caa.last.MusicbrainzID != "rg-1" {
		t.Errorf("album Supplement keyed by %q, want the lead's release-group id", caa.last.MusicbrainzID)
	}
}

func TestMusicChainMusicBrainzNoMatchPropagates(t *testing.T) {
	mb := &stubProvider{err: ErrNoMatch}
	img := &stubProvider{}
	chain := NewMusicChainProvider(mb, img)

	if _, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist"}); err != ErrNoMatch {
		t.Errorf("err = %v, want ErrNoMatch propagated from MusicBrainz", err)
	}
	if img.calls != 0 {
		t.Errorf("image source called after MusicBrainz no-match (%d); want 0", img.calls)
	}
}

// --- the shipped pair, composed generically --------------------------------

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
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", ""))
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
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg"))
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", ""))
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
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
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg"))
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", "real bio"))
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	// The fanart poster wins (fill-only, first in order); TheAudioDB's does not replace it...
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
	audioDB := audioDBLike(audiodbResult("", "A real, sourced biography."))
	chain := NewMusicChainProvider(mb, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "A real, sourced biography." || got.OverviewSynthesized {
		t.Errorf("overview = %q (synthesized=%v), want the real TheAudioDB bio", got.Overview, got.OverviewSynthesized)
	}
	// Identity and genres stay MusicBrainz's.
	if got.Source != "musicbrainz" || got.ExternalID != "mb-artist-1" || len(got.Genres) != 2 {
		t.Errorf("MusicBrainz authority disturbed: %+v", got)
	}
}

// TestMusicChainKeepsALeadsRealOverview is what the flag buys over the rule it
// replaced ("TheAudioDB's bio always wins"): a lead that holds real prose and says
// so keeps it, exactly as fill-only says.
func TestMusicChainKeepsALeadsRealOverview(t *testing.T) {
	lead := mbArtistResult()
	lead.Overview, lead.OverviewSynthesized = "A lead's own real biography.", false
	mb := &stubProvider{meta: lead}
	audioDB := audioDBLike(audiodbResult("", "A Supplement's biography."))
	chain := NewMusicChainProvider(mb, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "A lead's own real biography." {
		t.Errorf("overview = %q, want the lead's real one kept", got.Overview)
	}
}

// TestMusicChainASynthesizedSupplementFillsButYields: a Supplement's synthesized
// text still fills an EMPTY Overview, and stays replaceable by a later one's real
// text; it never replaces the lead's synthesized one.
func TestMusicChainASynthesizedSupplementFillsButYields(t *testing.T) {
	stub := TitleMetadata{Matched: true, Overview: "A placeholder.", OverviewSynthesized: true}
	real := TitleMetadata{Matched: true, Overview: "Real prose."}

	empty := mbArtistResult()
	empty.Overview, empty.OverviewSynthesized = "", false
	got, _ := NewMusicChainProvider(&stubProvider{meta: empty}, &stubProvider{meta: stub}, &stubProvider{meta: real}).
		Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "Real prose." {
		t.Errorf("overview = %q, want the later real prose over the earlier placeholder", got.Overview)
	}

	got, _ = NewMusicChainProvider(&stubProvider{meta: mbArtistResult()}, &stubProvider{meta: stub}).
		Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if got.Overview != "English rock band from Oxford" {
		t.Errorf("overview = %q, want the lead's placeholder kept over a Supplement's placeholder", got.Overview)
	}
}

func TestMusicChainRetainsMusicBrainzBioWhenTheAudioDBHasNone(t *testing.T) {
	mb := &stubProvider{meta: mbArtistResult()}
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", "")) // image, no bio
	chain := NewMusicChainProvider(mb, audioDB)

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

// --- a third Supplement ------------------------------------------------------

// TestMusicChainComposesAThirdSupplement is the gap this issue closed: a music
// Supplement beyond the shipped two was registered, keyable, and never composed.
// Now it fills whatever the two before it left, in order.
func TestMusicChainComposesAThirdSupplement(t *testing.T) {
	mbMeta := mbArtistResult()
	mbMeta.Genres = nil
	mb := &stubProvider{meta: mbMeta}
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg"))
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", "real bio"))
	third := &stubProvider{meta: TitleMetadata{
		Matched: true, Overview: "third's bio", Genres: []string{"shoegaze"},
		Artwork: []ArtworkRef{{Role: "poster", URL: "https://third/poster"}, {Role: "background", URL: "https://third/bg"}},
	}}
	chain := NewMusicChainProvider(mb, fanart, audioDB, third)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if third.calls != 1 || third.last.MusicbrainzID != "mb-artist-1" {
		t.Fatalf("third Supplement call wrong: calls=%d last=%+v", third.calls, third.last)
	}
	// It fills only what the lead and the two before it left empty.
	if got.Overview != "real bio" {
		t.Errorf("overview = %q, want TheAudioDB's (earlier in order)", got.Overview)
	}
	if len(got.Genres) != 1 || got.Genres[0] != "shoegaze" {
		t.Errorf("genres = %v, want the third's (the lead had none)", got.Genres)
	}
	want := map[string]string{"poster": "https://fanart/thumb.jpg", "background": "https://third/bg"}
	if len(got.Artwork) != len(want) {
		t.Fatalf("artwork = %+v, want %v", got.Artwork, want)
	}
	for _, a := range got.Artwork {
		if want[a.Role] != a.URL {
			t.Errorf("artwork %s = %q, want %q", a.Role, a.URL, want[a.Role])
		}
	}
}

// --- track synopses --------------------------------------------------------

// mbTrackResult mimics a MusicBrainz track lookup: a canonical title + recording
// MBID, but no Overview (its documented gap).
func mbTrackResult() TitleMetadata {
	return TitleMetadata{Matched: true, Name: "Creep", ExternalID: "rec-1", Source: pluginapi.NamespaceMusicBrainz}
}

func TestMusicChainTrackSynopsisFilledFromTheAudioDB(t *testing.T) {
	mb := &stubProvider{meta: mbTrackResult()}
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg")) // artist-only
	audioDB := audioDBLike(TitleMetadata{Matched: true, Source: "theaudiodb", Overview: "A real, sourced synopsis."})
	chain := NewMusicChainProvider(mb, fanart, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Overview != "A real, sourced synopsis." {
		t.Errorf("overview = %q, want the TheAudioDB synopsis", got.Overview)
	}
	// The canonical title and identity stay MusicBrainz's; fanart.tv serves no track.
	if got.Name != "Creep" || got.ExternalID != "rec-1" || got.Source != "musicbrainz" || len(got.Artwork) != 0 {
		t.Errorf("MusicBrainz canonical/identity disturbed: %+v", got)
	}
	// TheAudioDB is keyed by the recording MBID and carries the track + artist.
	if audioDB.calls != 1 || audioDB.last.Kind != "track" || audioDB.last.MusicbrainzID != "rec-1" ||
		audioDB.last.Track != "Creep" || audioDB.last.Artist != "Radiohead" {
		t.Errorf("theaudiodb track call wrong: calls=%d last=%+v", audioDB.calls, audioDB.last)
	}
}

// TestMusicChainSkipsTheSynopsisForACandidateTheHostWillReject closes issue 01's
// third deviation. Moving the acceptance test out of MusicBrainz meant the chain
// stopped seeing a rejection and started decorating candidates the service was
// about to discard — one wasted TheAudioDB request per rejected Track, hundreds on
// a real library.
//
// The fix is a PRECONDITION, not a verdict: the chain asks the host's own rule
// whether this record is one the host will keep, and the service still produces
// ErrMatchRejected and the `search-rejected` reason a moment later.
func TestMusicChainSkipsTheSynopsisForACandidateTheHostWillReject(t *testing.T) {
	// A search hit whose title is a DIFFERENT song — what a relevance query returns
	// when the local track is not in the index.
	hit := mbTrackResult()
	hit.Name, hit.FromSearch = "Creep (Acoustic Version by Someone Else)", true
	mb := &stubProvider{meta: hit}
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Overview: "A synopsis nobody will read."}}
	chain := NewMusicChainProvider(mb, audioDB)

	ref := TitleRef{Kind: "track", Track: "Paranoid Android", Artist: "Radiohead"}
	got, err := chain.Lookup(context.Background(), ref)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if audioDB.calls != 0 {
		t.Errorf("theaudiodb was asked for a synopsis for a candidate the host rejects (%d calls); want 0", audioDB.calls)
	}
	// The record is still handed UP unjudged: the host, not the chain, rejects it.
	if _, err := acceptSearchHit(ref, got, nil); !errors.Is(err, ErrMatchRejected) {
		t.Errorf("the host's verdict = %v, want ErrMatchRejected — the chain must not settle anything itself", err)
	}
}

// TestMusicChainSkipsEverySupplementForAnArtistTheHostWillReject: the precondition
// now gates every music kind, since every kind reaches the Supplements.
func TestMusicChainSkipsEverySupplementForAnArtistTheHostWillReject(t *testing.T) {
	hit := mbArtistResult()
	hit.Name, hit.FromSearch = "Somebody Else Entirely", true
	fanart := fanartLike(posterResult("https://fanart/thumb.jpg"))
	audioDB := audioDBLike(audiodbResult("https://theaudiodb/thumb.jpg", "bio"))
	chain := NewMusicChainProvider(&stubProvider{meta: hit}, fanart, audioDB)

	if _, err := chain.Lookup(context.Background(), TitleRef{Kind: "artist", Title: "Radiohead"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if fanart.calls+audioDB.calls != 0 {
		t.Errorf("Supplements asked about an artist the host rejects (fanart %d, theaudiodb %d); want 0", fanart.calls, audioDB.calls)
	}
}

// TestMusicChainStillFillsTheSynopsisForAnAcceptedSearchHit is the other half, and
// the reason the fix is not simply "skip the fill for any search hit": an accepted
// hit is a perfectly good record, and gating on FromSearch alone would have
// stripped its synopsis — trading a wasted call for a real regression.
func TestMusicChainStillFillsTheSynopsisForAnAcceptedSearchHit(t *testing.T) {
	hit := mbTrackResult()
	// The candidate's own title, spelled as MusicBrainz spells it — the same title
	// under normalizeMatchTitle, which is what the host accepts.
	hit.Name, hit.FromSearch = "Creep (Remastered 2011)", true
	mb := &stubProvider{meta: hit}
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Overview: "A real, sourced synopsis."}}
	chain := NewMusicChainProvider(mb, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", Artist: "Radiohead"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if audioDB.calls != 1 {
		t.Fatalf("theaudiodb calls = %d, want 1 — an accepted search hit keeps its synopsis", audioDB.calls)
	}
	if got.Overview != "A real, sourced synopsis." {
		t.Errorf("overview = %q, want the synopsis", got.Overview)
	}
}

// TestFillNeverFillsTheNameOfASearchHit: Name on a search hit is the
// candidate's own title, the string the host judges it by (ADR-0050). A
// Supplement's title filled in there would be judged in the candidate's place.
func TestFillNeverFillsTheNameOfASearchHit(t *testing.T) {
	sup := TitleMetadata{Matched: true, Name: "Paranoid Android"}
	if got := fillFromSupplement(TitleMetadata{Matched: true, FromSearch: true}, sup); got.Name != "" {
		t.Errorf("name = %q, want a search hit's empty Name left for the host to judge", got.Name)
	}
	if got := fillFromSupplement(TitleMetadata{Matched: true}, sup); got.Name != "Paranoid Android" {
		t.Errorf("name = %q, want a by-id record's empty Name filled", got.Name)
	}
}

// TestMusicChainFillsTheSynopsisForARecordResolvedByID: a record resolved by id is
// never judged (an id IS the identification, ADR-0049), so the precondition must
// not gate it — a canonical title that disagrees with the local one is a spelling.
func TestMusicChainFillsTheSynopsisForARecordResolvedByID(t *testing.T) {
	byID := mbTrackResult()
	byID.Name = "Something Spelled Quite Differently" // and FromSearch stays false
	mb := &stubProvider{meta: byID}
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Overview: "A real, sourced synopsis."}}
	chain := NewMusicChainProvider(mb, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep", MusicbrainzID: "rec-1"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if audioDB.calls != 1 || got.Overview != "A real, sourced synopsis." {
		t.Errorf("a record resolved by id lost its synopsis: calls=%d overview=%q", audioDB.calls, got.Overview)
	}
}

func TestMusicChainTrackDoesNotOverwriteExistingOverview(t *testing.T) {
	// If MusicBrainz ever carried a real track Overview, the synopsis source must not
	// replace it (fill-only). MusicBrainz has none today; this guards the policy.
	mbMeta := mbTrackResult()
	mbMeta.Overview = "A hypothetical MusicBrainz synopsis."
	mb := &stubProvider{meta: mbMeta}
	audioDB := &stubProvider{meta: TitleMetadata{Matched: true, Source: "theaudiodb", Overview: "TheAudioDB synopsis."}}
	chain := NewMusicChainProvider(mb, audioDB)

	got, _ := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep"})
	if got.Overview != "A hypothetical MusicBrainz synopsis." {
		t.Errorf("overview = %q, want the MusicBrainz overview kept", got.Overview)
	}
}

func TestMusicChainTrackTheAudioDBNoMatchIsNonFatal(t *testing.T) {
	mb := &stubProvider{meta: mbTrackResult()}
	audioDB := &stubProvider{err: ErrNoMatch}
	chain := NewMusicChainProvider(mb, audioDB)

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
	chain := NewMusicChainProvider(mb, audioDB)

	got, err := chain.Lookup(context.Background(), TitleRef{Kind: "track", Track: "Creep"})
	if err != nil {
		t.Fatalf("a TheAudioDB error must not fail the lookup; got %v", err)
	}
	if got.Name != "Creep" || got.ExternalID != "rec-1" || got.Overview != "" {
		t.Errorf("MusicBrainz result not preserved: %+v", got)
	}
}

// --- Artist Photo candidates (artwork-management/02) ------------------------

// TestMusicChainArtistArtworkCandidatesUnion: the artist picker composes every
// source — the lead's (MusicBrainz has none for an artist), then fanart.tv's full
// artistthumb[] (leading the grid) UNIONed with TheAudioDB's thumb.
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
	want := []string{"https://fanart/best.jpg", "https://fanart/second.jpg", "https://theaudiodb/thumb.jpg"}
	if len(cands) != len(want) {
		t.Fatalf("candidates = %d, want %d (2 fanart + 1 theaudiodb)", len(cands), len(want))
	}
	for i, w := range want {
		if cands[i].URL != w {
			t.Errorf("candidate[%d].URL = %q, want %q", i, cands[i].URL, w)
		}
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

// TestMusicChainArtistArtworkCandidatesDegrades: a Supplement error is swallowed —
// the other source still populates the grid, and if BOTH fail the result is a
// graceful empty list, never a returned error (ADR-0001, so the API never 500s).
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

	// BOTH Supplements down → an empty list and NO error (the picker degrades to the
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

// TestMusicChainArtistArtworkCandidatesNoSources: with no Supplements configured
// (offline / no key), the artist picker is the lead's — empty, not an error.
func TestMusicChainArtistArtworkCandidatesNoSources(t *testing.T) {
	chain := NewMusicChainProvider(&stubProvider{})
	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "artist", MusicbrainzID: "mb-artist-1"}, "poster")
	if err != nil || len(cands) != 0 {
		t.Errorf("no-sources = (%+v, %v), want (nil, nil)", cands, err)
	}
}

// TestMusicChainAlbumArtworkCandidatesLeadFirst: an album picker is MusicBrainz's
// Cover Art Archive covers first; Supplements that serve no album add nothing, and
// one that does (a Cover Art Archive-style plugin) follows the lead's.
func TestMusicChainAlbumArtworkCandidatesLeadFirst(t *testing.T) {
	mb := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://caa/500.jpg", Source: "coverartarchive"}}}
	fanart := fanartLike(TitleMetadata{})
	fanart.artwork = []ArtworkCandidate{{URL: "https://fanart/should-not-appear.jpg"}}
	albumSup := &stubProvider{artwork: []ArtworkCandidate{{URL: "https://other/cover.jpg", Source: "other"}}}
	chain := NewMusicChainProvider(mb, fanart, albumSup)

	cands, err := chain.ArtworkCandidates(context.Background(),
		TitleRef{Kind: "album", MusicbrainzID: "rg-1"}, "cover")
	if err != nil {
		t.Fatalf("ArtworkCandidates: %v", err)
	}
	if len(cands) != 2 || cands[0].Source != "coverartarchive" || cands[1].Source != "other" {
		t.Errorf("album candidates = %+v, want the MusicBrainz cover then the album Supplement's", cands)
	}
}

// TestMusicChainAlbumArtworkCandidatesKeepTheLeadsError: when nothing produced a
// candidate, the lead's error is the answer, as it was before the Supplements
// were consulted for an album.
func TestMusicChainAlbumArtworkCandidatesKeepTheLeadsError(t *testing.T) {
	mb := &stubProvider{artworkErr: ErrSearchUnavailable}
	chain := NewMusicChainProvider(mb, fanartLike(TitleMetadata{}))
	if _, err := chain.ArtworkCandidates(context.Background(), TitleRef{Kind: "album"}, "cover"); !errors.Is(err, ErrSearchUnavailable) {
		t.Errorf("err = %v, want the lead's ErrSearchUnavailable", err)
	}
}
