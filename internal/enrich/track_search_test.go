package enrich

import (
	"context"
	"errors"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0050: "A search hit must pass an acceptance test before it becomes a record."
//
// THE PROVIDER HALF OF THIS FILE LEFT WITH MUSICBRAINZ (.scratch/bundled-plugins
// issue 06). The query — escaped, relevance-ranked, artist-narrowed — and the
// recall it buys are asserted natively in plugins/musicbrainz/musicbrainz, against
// the same Lucene-parsing stub that used to stand here, because they are facts
// about what one source sends.
//
// What stays is the half ADR-0057 moved INTO the host and that no source can be
// trusted to report: the acceptance test itself. A relevance query essentially
// always returns something, so its top hit is a CANDIDATE, and only the server
// turns a candidate into a record. The fake below is a source that offers one,
// which is all a source is required to do.

// searchHitSource is a music source that answers a track lookup with one top hit,
// unjudged and marked FromSearch — the shape ADR-0057 obliges a Plugin to return,
// and the only input acceptSearchHit has. It counts its lookups, because ONE
// request per track is the other half of ADR-0049 and a fallback that quietly
// re-asked would be invisible in the rows.
type searchHitSource struct {
	fakeProvider
	title string // the candidate's own title; "" is "the source found nothing"
	id    string
	calls int
}

func (s *searchHitSource) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	s.calls++
	if ref.Kind != "track" || s.title == "" {
		return TitleMetadata{}, ErrNoMatch
	}
	return TitleMetadata{
		Matched: true, Name: s.title, ExternalID: s.id, Source: "musicbrainz", FromSearch: true,
	}, nil
}

// --- the acceptance test, applied by the host ----------------------------------

// The regression the acceptance test exists to prevent, asserted directly: a
// relevance query nearly always returns SOMETHING, and the top hit here is a
// different song. Storing it would be the confident wrong overview ADR-0049 ruled is
// worse than an empty one.
//
// THE PROVIDER NO LONGER REFUSES IT (ADR-0057). It offers the candidate and says
// how it found it; acceptSearchHit does the refusing. Both halves are driven here
// because only across the seam are they one answer — a provider that quietly kept
// judging, and a host that quietly stopped, each pass one half of this test.
func TestTheHostRejectsATopHitThatIsADifferentSong(t *testing.T) {
	p := &searchHitSource{title: "Whispering Your Name", id: "rec-1"}

	ref := TitleRef{Kind: "track", Track: "Whisper Your Name", Artist: "Harry Connick Jr."}
	hit, err := p.Lookup(context.Background(), ref)
	if err != nil || !hit.Matched || !hit.FromSearch || hit.Name != "Whispering Your Name" {
		t.Fatalf("provider = %+v, err = %v — want the top hit handed back UNJUDGED, marked "+
			"FromSearch and carrying its own title, which is what the host judges it by", hit, err)
	}

	meta, err := acceptSearchHit(ref, hit, err)
	if !errors.Is(err, ErrMatchRejected) {
		t.Fatalf("err = %v, want ErrMatchRejected — the top hit was %q, a different song, and "+
			"it must not become this Track's record", err, "Whispering Your Name")
	}
	// Every existing caller tests ErrNoMatch; the new value must keep them working.
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("ErrMatchRejected does not satisfy errors.Is(err, ErrNoMatch) — every caller " +
			"that files a no-match now treats a rejection as a hard failure")
	}
	if meta.Matched || meta.ExternalID != "" || meta.Name != "" {
		t.Errorf("meta = %+v, want the zero value — a rejected hit contributes nothing", meta)
	}
	// The acceptance test guards the TOP hit only. It deliberately does not scan down
	// the list for something acceptable: that is a ranking judgement the picker's
	// human makes.
	if p.calls != 1 {
		t.Fatalf("made %d lookups, want exactly 1 — a rejection must NOT be retried with a "+
			"looser query. ADR-0049 measured the search cluster shedding load globally, and "+
			"a second request issued during those failures pushes the wrong way", p.calls)
	}
}

// Emptiness and rejection are DIFFERENT answers, and issue 06 renders them as
// different next actions ('search-no-match' vs 'search-rejected'). Both still file
// the Track as unmatched, so only the error VALUE separates them.
func TestTrackSearchEmptyResultIsPlainNoMatchNotARejection(t *testing.T) {
	p := &searchHitSource{} // the source found nothing at all

	ref := TitleRef{Kind: "track", Track: "Whisper Your Name", Artist: "Harry Connick Jr."}
	hit, lookupErr := p.Lookup(context.Background(), ref)
	_, err := acceptSearchHit(ref, hit, lookupErr)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
	if errors.Is(err, ErrMatchRejected) {
		t.Errorf("an empty result reported ErrMatchRejected — 'the source found nothing' and " +
			"'the source found something and we refused it' are different diagnoses with " +
			"different remedies")
	}
	if p.calls != 1 {
		t.Fatalf("made %d lookups, want exactly 1 — an empty result must NOT trigger a second, "+
			"looser query (ADR-0049)", p.calls)
	}
}

// An ACCEPTED hit passes straight through, so the test is a filter and not a wall:
// the one bit that separates this from the rejection above is the title the source
// handed over. (The normalizer folds case, so these two ARE the same title.)
func TestAnAcceptedTopHitPassesTheHostsTest(t *testing.T) {
	p := &searchHitSource{title: "Whisper Your Name", id: "rec-1"}

	ref := TitleRef{Kind: "track", Track: "whisper your name", Artist: "Harry Connick Jr."}
	hit, lookupErr := p.Lookup(context.Background(), ref)
	meta, err := acceptSearchHit(ref, hit, lookupErr)
	if err != nil {
		t.Fatalf("acceptSearchHit: %v — the two titles are the same title", err)
	}
	if !meta.Matched || meta.ExternalID != "rec-1" {
		t.Fatalf("meta = %+v, want the accepted candidate", meta)
	}
}

// ErrMatchRejected's wrapping is load-bearing on its own: the whole codebase asks
// errors.Is(err, ErrNoMatch), and this value has to answer yes without BEING it.
func TestErrMatchRejectedWrapsErrNoMatchWithoutBeingIt(t *testing.T) {
	if !errors.Is(ErrMatchRejected, ErrNoMatch) {
		t.Error("ErrMatchRejected must wrap ErrNoMatch, or every existing no-match caller breaks")
	}
	if errors.Is(ErrNoMatch, ErrMatchRejected) {
		t.Error("a plain ErrNoMatch must not report itself as a rejection — the two outcomes " +
			"would stop being distinguishable, which is the point of the new value")
	}
}

// --- through a real pass -------------------------------------------------------

// The row a rejection produces is exactly as honest as the one the exact phrase
// produced: status 'unmatched', record columns untouched. A new error value that
// accidentally parked the Track as a FAILURE (retryable, or needing an Admin) would
// be a regression the provider-level tests above cannot see.
func TestARejectedSearchLeavesTheTrackUnmatchedAndUntouched(t *testing.T) {
	prov := &albumTierProvider{
		// Two spare positions, so the tracklist tier cannot rescue the odd track out
		// by the leftover rule and it really does fall through to the search.
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "Bonus One", "rec-x"),
			entry(3, "Bonus Two", "rec-y"),
		},
		recordings: map[string]string{"rec-1": "Whisper Your Name"},
		searchErr:  ErrMatchRejected, // the search answered; nothing it offered was this song
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "Nowhere On The Release", num: 2},
		},
	})

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if res.Failed != 0 {
		t.Errorf("the pass recorded %d failures — a rejected hit is a no-match, not a provider "+
			"failure, and must not be retried or parked (calls: %v)", res.Failed, prov.history())
	}
	got := trackRow(t, db, "t2")
	if got.EnrichmentStatus != "unmatched" {
		t.Errorf("status = %q, want unmatched", got.EnrichmentStatus)
	}
	if got.MusicbrainzID != "" {
		t.Errorf("recorded %q — the rejected candidate was stored anyway, which is the silent "+
			"wrong overview ADR-0050's acceptance test exists to prevent", got.MusicbrainzID)
	}
	if n := prov.count("search:Nowhere On The Release"); n != 1 {
		t.Errorf("the track searched %d times, want 1 (calls: %v)", n, prov.history())
	}
	// The matched sibling is undisturbed by its neighbour's rejection.
	if sib := trackRow(t, db, "t1"); sib.EnrichmentStatus != "matched" || sib.MusicbrainzID != "rec-1" {
		t.Errorf("sibling row = %s/%s, want matched/rec-1", sib.EnrichmentStatus, sib.MusicbrainzID)
	}
}

// --- the judgement, in the host ------------------------------------------------

// The whole point of the move, end to end: the SOURCE offers a wrong song and says
// nothing about whether it is right, and the SERVER produces the rejection and the
// `search-rejected` diagnosis (ADR-0057). Before the move a fake had to return
// ErrMatchRejected to reach this row — it was reporting a judgement the server had
// delegated to it, which is exactly what a source written by someone who never read
// ADR-0049 would not report.
func TestTheServerNotTheSourceProducesTheSearchRejectedReason(t *testing.T) {
	prov := &albumTierProvider{
		// A TRANSIENT tracklist failure, so the album tier has nothing to say and does
		// not outrank the search's own answer in ADR-0050's reason table — which is the
		// only arrangement under which `search-rejected` is the reason at all.
		tracklistErr: errors.New("musicbrainz busy"),
		searchHits:   map[string]string{"Nowhere On The Release": "rec-wrong"},
		// The source found something and offers it, unjudged. It is a different song.
		searchHitTitles: map[string]string{"Nowhere On The Release": "A Completely Different Song"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t2", title: "Nowhere On The Release", num: 2}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := trackRow(t, db, "t2")
	if got.EnrichmentStatus != "unmatched" || got.EnrichmentReason != store.EnrichmentReasonSearchRejected {
		t.Fatalf("row = %s/%s, want unmatched/%s — the server did not apply the acceptance test "+
			"to a candidate the source declined to judge (calls: %v)",
			got.EnrichmentStatus, got.EnrichmentReason, store.EnrichmentReasonSearchRejected,
			prov.history())
	}
	if got.MusicbrainzID != "" {
		t.Errorf("recorded %q — the refused candidate's id reached the row, so the rejection is "+
			"leaking the record it rejected", got.MusicbrainzID)
	}
}

// The same path accepts, so the move did not simply refuse everything: the one bit
// that separates this row from the one above is the title the source handed over.
func TestAnAcceptedSearchHitStillBecomesTheRecord(t *testing.T) {
	prov := &albumTierProvider{
		tracklistErr: errors.New("musicbrainz busy"),
		searchHits:   map[string]string{"Nowhere On The Release": "rec-right"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t2", title: "Nowhere On The Release", num: 2}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := trackRow(t, db, "t2"); got.EnrichmentStatus != "matched" || got.MusicbrainzID != "rec-right" {
		t.Fatalf("row = %s/%s, want matched/rec-right", got.EnrichmentStatus, got.MusicbrainzID)
	}
}

// Video is NOT filtered by this phase (ADR-0057's consequence, PRD story 6): a movie
// hit whose canonical title is not the parsed one — a re-release, a localized title,
// a subtitle the filename omits — still becomes the record, exactly as it did before
// acceptance moved. The rule is written by kind, so this is the guard against
// someone later "finishing the job" by deleting the kind check.
func TestAcceptanceDoesNotTouchVideo(t *testing.T) {
	ref := TitleRef{Kind: "movie", Title: "Star Wars"}
	hit := TitleMetadata{
		Matched: true, Name: "Star Wars: Episode IV - A New Hope",
		ExternalID: "11", Source: "tmdb", FromSearch: true,
	}
	meta, err := acceptSearchHit(ref, hit, nil)
	if err != nil || meta.ExternalID != "11" {
		t.Fatalf("meta = %+v, err = %v — a video search hit must pass through untouched; "+
			"whether TMDB should face a title test is its own decision, not this one", meta, err)
	}
}
