package enrich

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// errShedding is the transient provider failure ADR-0049 measured — the host busy,
// not the album unmatched. It reaches the album tier as tracklistUnavailable, which
// is what leaves a Track for the search tier to diagnose.
var errShedding = errors.New("musicbrainz: the web server is currently busy")

// ADR-0050's tracklist tier on the SINGLE-TITLE path (issue 14): a Track
// re-enriched on its own asks its Album exactly what a library pass asks it,
// through the same function.
//
// The defect this file exists to prevent has already happened once, in the
// friendliest possible disguise. refFor carried a comment promising "same
// precedence as the library pass"; the precedence then gained a fourth tier and
// refFor did not, so the sentence quietly turned into a description of the bug it
// was written to stop. A comment asserting parity between two code paths is worth
// exactly the test that holds them to it — which is why the last test here resolves
// the same Track BOTH WAYS and compares the rows.
//
// The other assertions are mostly about the CALL LOG and the store reads, because
// "the album resolved it" and "it searched and happened to succeed" leave identical
// rows behind. The difference is only ever in what was asked of MusicBrainz, and on
// which endpoint (ADR-0049).

// --- helpers ------------------------------------------------------------------

// reEnrichAlone is the single-Title path with NO new id: re-resolve this one Title,
// exactly as it stands. It is the "try this one again" gesture an Admin makes while
// hand-fixing an album, and MatchTitle is the primitive behind it.
//
// Today's HTTP surface always supplies an id (both PUT endpoints 400 without one),
// which is precisely why the album tier could go missing here for a whole feature
// without anybody noticing: the tier is only ever consulted when the Track has no
// id of its own, and the reachable callers all had one.
func reEnrichAlone(t *testing.T, svc *Service, titleID string) {
	t.Helper()
	if err := svc.MatchTitle(context.Background(), titleID, store.ExternalMatch{}); err != nil {
		t.Fatalf("re-enrich %s: %v", titleID, err)
	}
}

// albumReadCounter counts the ONE store read that opens the single-Title album
// tier. It is the cache-independent guard: a tracklist the Service has already
// cached is served without touching the provider, so counting provider calls alone
// cannot prove that a per-child cascade did not enter the tier. Reaching
// TrackContextForTitle at all means the tier was entered.
type albumReadCounter struct {
	Store
	trackContexts int
}

func (c *albumReadCounter) TrackContextForTitle(titleID string) (store.TrackContext, error) {
	c.trackContexts++
	return c.Store.TrackContextForTitle(titleID)
}

// countAlbumReads wraps the Service's store so a test can assert that the album
// tier was never entered.
func countAlbumReads(svc *Service) *albumReadCounter {
	c := &albumReadCounter{Store: svc.store}
	svc.store = c
	return c
}

// videoRefProvider is albumTierProvider with the video kinds answered and their
// lookup REFS recorded verbatim, so a Movie/Episode re-enrich can be compared
// against refFor byte for byte.
type videoRefProvider struct {
	*albumTierProvider
	videoRefs []TitleRef
}

func (p *videoRefProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	switch ref.Kind {
	case "movie", "episode":
		p.videoRefs = append(p.videoRefs, ref)
		p.note("video:" + ref.Kind)
		return TitleMetadata{Matched: true, Name: ref.Title, ExternalID: ref.TMDBID, Source: "tmdb"}, nil
	}
	return p.albumTierProvider.Lookup(ctx, ref)
}

// --- the tier reaches the single-Title path ------------------------------------

// THE HEADLINE. One Track, re-enriched on its own under a matched Album: it
// resolves from the album's tracklist and never touches the search cluster. Before
// this it searched — for a Track the album could have named outright.
func TestASingleReEnrichResolvesFromTheAlbumTracklist(t *testing.T) {
	prov := &albumTierProvider{
		tracklist: []TrackCandidate{
			entry(1, "Whisper Your Name", "rec-1"),
			entry(2, "She", "rec-2"),
			entry(3, "Follow the Music", "rec-3"),
		},
		recordings: map[string]string{"rec-1": "Whisper Your Name", "rec-2": "She", "rec-3": "Follow the Music"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "She", num: 2},
			{id: "t3", title: "Follow the Music", num: 3},
		},
	})

	reEnrichAlone(t, svc, "t1")

	if n := prov.count("search:"); n != 0 {
		t.Fatalf("%d recording SEARCHES, want 0 — the Album names this recording, and a "+
			"re-enrich must look it up where a pass would (calls: %v)", n, prov.history())
	}
	if n := prov.count("tracklist:"); n != 1 {
		t.Fatalf("%d tracklist reads, want 1 (calls: %v)", n, prov.history())
	}
	// The anchor is the Album's own record and the request carries the WHOLE local
	// track count — the mapping is album-grained even when one Track asked for it.
	if got, want := prov.history()[0], "tracklist:rg-she||3"; got != want {
		t.Fatalf("tracklist request %q, want %q — the album's record is the anchor and its "+
			"whole local list is what the edition is fitted against", got, want)
	}
	got := trackRow(t, db, "t1")
	if got.MusicbrainzID != "rec-1" || got.EnrichmentStatus != "matched" {
		t.Fatalf("t1 = %q/%q, want rec-1/matched (calls: %v)",
			got.MusicbrainzID, got.EnrichmentStatus, prov.history())
	}
	// The RESULT was filtered to this Track, not the input to mapTracks: its two
	// neighbours went into the mapping (they hold their positions on the release) and
	// came out of the re-enrich untouched.
	for _, id := range []string{"t2", "t3"} {
		row := trackRow(t, db, id)
		if row.MusicbrainzID != "" || row.EnrichmentStatus != "pending" {
			t.Errorf("%s = %q/%q after re-enriching t1, want it untouched — a single re-enrich "+
				"maps the whole album and writes ONE Track", id, row.MusicbrainzID, row.EnrichmentStatus)
		}
	}
}

// The chosen edition and its licence reach this path identically (ADR-0052): the
// request says chosen=true, and a Track whose title the release spells differently
// is pinned by its own position — the thing a pass does for it, and the thing an
// operator who pinned an edition is entitled to expect from "try this one again".
func TestASingleReEnrichUnderAChosenEditionMapsByPosition(t *testing.T) {
	prov := viaggioProvider()
	svc, db := newAlbumFixture(t, prov, viaggioSeed())
	pinEdition(t, db, viaggioRelease)

	// t2's local title is "Cilea: L'Arlesiana - Lamento Di Federico"; the release
	// calls it "L'arlesiana: È la solita storia…". Only the position agrees, and only
	// the human's pin makes that evidence.
	reEnrichAlone(t, svc, "t2")

	if n := prov.count("tracklist:" + viaggioGroup + "|" + viaggioRelease + "|chosen=true"); n != 1 {
		t.Fatalf("made %d chosen-edition reads, want 1 — the licence must travel on the "+
			"single-Title path too (calls: %v)", n, prov.history())
	}
	got := trackRow(t, db, "t2")
	if got.MusicbrainzID != "rec-2" {
		t.Fatalf("t2 pinned to %q, want rec-2 — under a pinned edition the numbering is the "+
			"operator's assertion, not our assumption (calls: %v)", got.MusicbrainzID, prov.history())
	}
	if got.EnrichmentStatus != "matched" {
		t.Errorf("t2 status = %q, want matched", got.EnrichmentStatus)
	}
	// A chosen EDITION is not a choice about this track: it stays derived, revisable
	// by the next pass (ADR-0052).
	if got.EnrichmentIDOrigin != store.OriginDerived {
		t.Errorf("t2 origin = %q, want OriginDerived", got.EnrichmentIDOrigin)
	}
}

// --- the diagnosis it writes ---------------------------------------------------

// An Album that can name none of its contents settles its Track as
// `album-unmatched` — the action is on the ALBUM. Before this, the same row read
// `search-no-match` and pointed the Admin at a recording picker.
func TestASingleReEnrichOfAnUnmatchedAlbumSaysAlbumUnmatched(t *testing.T) {
	prov := &albumTierProvider{} // no album record, no tag id: nothing to ask
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1}},
	})

	reEnrichAlone(t, svc, "t1")

	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist reads, want 0 — an Album with neither a record nor a tag id "+
			"knows nothing about its contents and is not asked (calls: %v)", n, prov.history())
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "unmatched" {
		t.Fatalf("status = %q, want unmatched", got.EnrichmentStatus)
	}
	if got.EnrichmentReason != store.EnrichmentReasonAlbumUnmatched {
		t.Fatalf("reason = %q, want %q — the Album is the thing that could not be resolved, "+
			"and a search reason would send the Admin to the wrong screen",
			got.EnrichmentReason, store.EnrichmentReasonAlbumUnmatched)
	}
}

// A matched Album whose tracklist has no room for this Track settles it as
// `not-in-tracklist` — the action is on the Album's RELEASE. Two spare positions
// keep the leftover rule from rescuing it.
func TestASingleReEnrichDeclinedByTheTracklistSaysNotInTracklist(t *testing.T) {
	prov := &albumTierProvider{
		tracklist: []TrackCandidate{
			entry(1, "Something Else", "rec-x"),
			entry(2, "Bonus One", "rec-y"),
			entry(3, "Bonus Two", "rec-z"),
		},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "Nowhere On The Release", num: 1},
			{id: "t2", title: "Also Nowhere", num: 2},
		},
	})

	reEnrichAlone(t, svc, "t1")

	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "unmatched" {
		t.Fatalf("status = %q, want unmatched", got.EnrichmentStatus)
	}
	if got.EnrichmentReason != store.EnrichmentReasonNotInTracklist {
		t.Fatalf("reason = %q, want %q — a tracklist WAS read and this Track was declined by "+
			"the match rule, which is a disagreement about the release",
			got.EnrichmentReason, store.EnrichmentReasonNotInTracklist)
	}
	// And never a search reason on either of these two paths — that is the half of
	// the defect that outlives the failure, because the screen built to say what is
	// wrong would say something that isn't.
	for _, bad := range []string{store.EnrichmentReasonSearchNoMatch, store.EnrichmentReasonSearchRejected} {
		if got.EnrichmentReason == bad {
			t.Fatalf("reason = %q, a SEARCH reason for an album-scoped failure", bad)
		}
	}
}

// --- the common case must not get slower ---------------------------------------

// A Track whose match carries an id resolves BY that id and asks its Album nothing.
// This is the frequent path — an Admin picking a recording out of the picker — and
// the tier must return before it reads anything at all.
func TestASingleReEnrichWithAnIDMakesNoTracklistCall(t *testing.T) {
	prov := &albumTierProvider{
		tracklist:  []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		recordings: map[string]string{"rec-picked": "Whisper Your Name"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1}},
	})
	reads := countAlbumReads(svc)

	if err := svc.MatchTitle(context.Background(), "t1",
		store.ExternalMatch{MusicbrainzID: "rec-picked"}); err != nil {
		t.Fatalf("match: %v", err)
	}

	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist reads, want 0 — the Admin supplied the id, and the record tier "+
			"outranks the album tier anyway (calls: %v)", n, prov.history())
	}
	if reads.trackContexts != 0 {
		t.Fatalf("entered the album tier %d times, want 0 — an id in hand must not cost even "+
			"a store read", reads.trackContexts)
	}
	if n := prov.count("recording:rec-picked"); n != 1 {
		t.Fatalf("looked the picked recording up %d times, want 1 (calls: %v)", n, prov.history())
	}
	if got := trackRow(t, db, "t1"); got.MusicbrainzID != "rec-picked" {
		t.Errorf("t1 = %q, want rec-picked", got.MusicbrainzID)
	}
}

// The Cascade's per-child applyOverride passes an id, so it must not enter the tier
// once per child. A regression here multiplies one album's cascade by its track
// count — the pathology that pays for the early return above.
func TestACascadeEntersTheAlbumTierNoTimes(t *testing.T) {
	prov := viaggioProvider()
	prov.searchAlbums = []Candidate{{
		ExternalID: viaggioGroup, Title: "She", Kind: "album", Tracklist: viaggioTracklist(),
	}}
	svc, db := newAlbumFixture(t, prov, viaggioSeed())
	pinEdition(t, db, viaggioRelease)
	reads := countAlbumReads(svc)

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", viaggioGroup)
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 5 {
		t.Fatalf("cascade updated %d of 5 (calls: %v)", sum.Updated, prov.history())
	}
	if reads.trackContexts != 0 {
		t.Fatalf("the cascade entered the single-Title album tier %d times, want 0 — it pins "+
			"each child BY an id, and re-deriving that id per child would cost one album "+
			"mapping per track", reads.trackContexts)
	}
	// One mapping for the whole album, not one per track: the cascade reads the
	// chosen edition once itself.
	if n := prov.count("tracklist:"); n != 1 {
		t.Fatalf("%d tracklist reads for a 5-track cascade, want 1 (calls: %v)", n, prov.history())
	}
}

// --- the video kinds are untouched ---------------------------------------------

// A Movie and an Episode re-enrich with the ref refFor builds, unchanged: the album
// tier returns before it reads or asks anything, and the reference that goes on the
// wire is byte-identical to the one this path sent before the tier existed.
func TestASingleVideoReEnrichIsUnchangedOnTheWire(t *testing.T) {
	prov := &videoRefProvider{albumTierProvider: &albumTierProvider{}}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "She", num: 1}},
	})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
	}
	exec(`INSERT INTO libraries (id, name, kind) VALUES ('libv', 'Video', 'movie')`)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title, year)
	      VALUES ('m1', 'libv', 'movie', 'Heat', 'movie:heat|1995', 'heat', 1995)`)
	exec(`INSERT INTO titles (id, library_id, kind, title, identity_key, sort_title,
	                          season_number, episode_number, enrichment_season, enrichment_episode)
	      VALUES ('e1', 'libv', 'episode', 'Pilot', 'show:x|s01e01', 'pilot', 1, 1, 2, 5)`)
	reads := countAlbumReads(svc)

	for _, id := range []string{"m1", "e1"} {
		if err := svc.MatchTitle(context.Background(), id,
			store.ExternalMatch{TMDBID: "tmdb-" + id}); err != nil {
			t.Fatalf("match %s: %v", id, err)
		}
		row, err := db.TitleForEnrichmentByID(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		// refFor alone is the whole reference for a non-Music leaf — episode pin
		// included, which is why this compares the built ref and not just the id.
		sent := prov.videoRefs[len(prov.videoRefs)-1]
		if want := refFor(row); !reflect.DeepEqual(sent, want) {
			t.Errorf("%s lookup ref = %+v, want %+v — a Movie/Episode re-enrich must be "+
				"byte-identical on the wire", id, sent, want)
		}
	}
	if reads.trackContexts != 0 {
		t.Fatalf("a video re-enrich entered the album tier %d times, want 0", reads.trackContexts)
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist reads for two video re-enrichments, want 0 (calls: %v)",
			n, prov.history())
	}
}

// --- THE ONE THAT MATTERS: the two paths agree ---------------------------------

// tierProvider is the fake-provider surface the agreement table needs: a provider,
// plus the call log that says WHICH tier answered. Both fakes in this package
// satisfy it, so a case can be built on whichever one its shape needs — the
// edition-aware one for the album tier, the plain one for a transient tracklist
// failure that drops a Track onto the search.
type tierProvider interface {
	MetadataProvider
	history() []string
	count(prefix string) int
}

// tierAgreementCase is one album shape resolved twice — once by a library pass, once
// by a single-Title re-enrich — over two identical fixtures.
type tierAgreementCase struct {
	name  string
	seed  seedAlbum
	pin   string // an Admin's chosen edition, "" for none
	prov  func() tierProvider
	track string // the Track resolved both ways
	// searches is how many recording SEARCHES each path must make FOR THIS TRACK.
	// It is the half of the agreement the row cannot show: an id from the album and
	// an id from a search leave the same columns behind, and the whole cost argument
	// of both tiers is about which endpoint was asked (ADR-0049). Counted per-title
	// rather than in total because the pass necessarily resolves the Track's
	// neighbours too, and the single re-enrich does not.
	searches int
}

// tierRow is the part of a Track's row that says which record it reached and what it
// was told about the failure to reach one. Everything the two paths could plausibly
// disagree about, and nothing that is a timestamp.
type tierRow struct {
	RecordID string
	Status   string
	Reason   string
	Origin   store.RecordOrigin
	Source   string
	Name     string
}

// localTitleOf is the seeded Track's own title, which is what a recording SEARCH is
// logged under ("search:<ref.Track>") — so a case can count the searches made for
// ITS Track and ignore the neighbours a pass necessarily resolves alongside it.
func localTitleOf(t *testing.T, seed seedAlbum, trackID string) string {
	t.Helper()
	for _, tr := range seed.tracks {
		if tr.id == trackID {
			return tr.title
		}
	}
	t.Fatalf("the case names track %q, which its seed does not contain", trackID)
	return ""
}

func rowOf(t *testing.T, db *store.DB, id string) tierRow {
	t.Helper()
	got := trackRow(t, db, id)
	return tierRow{
		RecordID: got.MusicbrainzID, Status: got.EnrichmentStatus, Reason: got.EnrichmentReason,
		Origin: got.EnrichmentIDOrigin, Source: got.EnrichmentSource, Name: got.EnrichedTitle,
	}
}

// THE TEST THE EXTRACTION EXISTS FOR. The same Track, under the same Album, with the
// same provider answers, resolved through a library pass and through a single-Title
// re-enrich, reaches the SAME RECORD — and, when it reaches none, is given the same
// diagnosis.
//
// This is the assertion refFor's comment used to make in prose. It was true when it
// was written, it silently stopped being true when the album tier landed, and no
// test failed. Two callers that can disagree eventually will; the only durable form
// of "these two paths agree" is a test that runs both.
//
// The shapes are everything the precedence can do, ALL FOUR TIERS of it (issues 14
// and 17): resolve from the record on the row, from the file's tag id, from the
// album's tracklist by title, from it by position under a human's edition, from a
// SEARCH when the album declines — and, failing all of that, decline with each of
// the diagnoses that name a different next action. Every one of them lands on a
// different record or a different reason, so a path that skipped any tier fails
// here rather than somewhere quieter.
func TestThePassAndTheSingleReEnrichReachTheSameRecord(t *testing.T) {
	cases := []tierAgreementCase{{
		name: "tier one: the record already on the row",
		prov: func() tierProvider {
			return &editionProvider{
				albumTierProvider: &albumTierProvider{
					recordings: map[string]string{"rec-stored": "Whisper Your Name"},
					// If either path fell past the record tier it would find these and settle
					// on the WRONG id, which is louder than settling on none.
					searchHits: map[string]string{"Whisper Your Name": "rec-leaked"},
				},
				fit: []TrackCandidate{entry(1, "Something Else", "rec-x")},
			}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, record: "rec-stored"},
		}},
		track: "t1",
	}, {
		name: "tier two: the recording id the file asserts",
		prov: func() tierProvider {
			return &editionProvider{
				albumTierProvider: &albumTierProvider{
					recordings: map[string]string{"rec-tag": "Whisper Your Name"},
					searchHits: map[string]string{"Whisper Your Name": "rec-leaked"},
				},
				fit: []TrackCandidate{entry(1, "Something Else", "rec-x")},
			}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1, recordingTag: "rec-tag"},
		}},
		track: "t1",
	}, {
		name: "tier three: resolved from the album's tracklist",
		prov: func() tierProvider {
			return &editionProvider{
				albumTierProvider: &albumTierProvider{
					recordings: map[string]string{"rec-1": "Whisper Your Name", "rec-2": "She"},
				},
				fit: []TrackCandidate{entry(1, "Whisper Your Name", "rec-1"), entry(2, "She", "rec-2")},
			}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
			{id: "t2", title: "She", num: 2},
		}},
		track: "t1",
	}, {
		name:  "tier three: pinned by position under a chosen edition",
		prov:  func() tierProvider { return viaggioProvider() },
		seed:  viaggioSeed(),
		pin:   viaggioRelease,
		track: "t2",
	}, {
		// THE TIER ISSUE 17 ALIGNED. The album read its tracklist and had no room for
		// this Track; the search finds it by name and artist. The single path used to
		// send NOTHING here — refFor left TitleRef.Track blank and trackDetails refuses
		// a blank name — so it filed a failure the pass never saw.
		name: "tier four: resolved by the search the album could not answer",
		prov: func() tierProvider {
			return &editionProvider{
				albumTierProvider: &albumTierProvider{
					searchHits: map[string]string{"Nowhere On The Release": "rec-search"},
				},
				fit: []TrackCandidate{
					entry(1, "Something Else", "rec-x"),
					entry(2, "Bonus One", "rec-y"),
					entry(3, "Bonus Two", "rec-z"),
				},
			}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Nowhere On The Release", num: 1},
			{id: "t2", title: "Also Nowhere", num: 2},
		}},
		track:    "t1",
		searches: 1,
	}, {
		// The search ANSWERED and its top hit was refused (issue 05's acceptance test).
		// The tracklist failed transiently, so the album tier says nothing and the
		// search's own answer is what names the failure: `search-rejected`, on both
		// paths, which is a claim only a path that actually searched can make.
		name: "tier four: the top hit rejected by the acceptance test",
		prov: func() tierProvider {
			return &albumTierProvider{tracklistErr: errShedding, searchErr: ErrMatchRejected}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Whisper Your Name", num: 1},
		}},
		track:    "t1",
		searches: 1,
	}, {
		name: "declined by a tracklist that was read",
		prov: func() tierProvider {
			return &editionProvider{
				albumTierProvider: &albumTierProvider{},
				fit: []TrackCandidate{
					entry(1, "Something Else", "rec-x"),
					entry(2, "Bonus One", "rec-y"),
					entry(3, "Bonus Two", "rec-z"),
				},
			}
		},
		seed: seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
			{id: "t1", title: "Nowhere On The Release", num: 1},
			{id: "t2", title: "Also Nowhere", num: 2},
		}},
		track:    "t1",
		searches: 1,
	}, {
		name: "no album to ask",
		prov: func() tierProvider {
			return &editionProvider{albumTierProvider: &albumTierProvider{}}
		},
		seed:     seedAlbum{tracks: []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1}}},
		track:    "t1",
		searches: 1,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Two identical fixtures, so the two paths cannot influence each other and
			// neither is reading a row the other wrote.
			passProv := c.prov()
			passSvc, passDB := newAlbumFixture(t, passProv, c.seed)
			singleProv := c.prov()
			singleSvc, singleDB := newAlbumFixture(t, singleProv, c.seed)
			if c.pin != "" {
				pinEdition(t, passDB, c.pin)
				pinEdition(t, singleDB, c.pin)
			}

			if _, err := passSvc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
				t.Fatalf("pass: %v", err)
			}
			reEnrichAlone(t, singleSvc, c.track)

			pass, single := rowOf(t, passDB, c.track), rowOf(t, singleDB, c.track)
			if pass != single {
				t.Fatalf("the two paths disagree about %s:\n  pass   = %+v (calls: %v)\n"+
					"  single = %+v (calls: %v)\nThe single-Title path and the pass resolve a "+
					"Track through ONE implementation (ADR-0050); if they differ, one of them "+
					"is not going through it.",
					c.track, pass, passProv.history(), single, singleProv.history())
			}
			// And they must have ASKED the same thing, on the same endpoint. Identical
			// rows are not proof of identical work: an id from the album's tracklist and
			// an id from a text search leave exactly the same columns behind, and WHICH
			// of the two happened is the entire cost argument of ADR-0049 and ADR-0050.
			// This is the assertion that fails if a tier is skipped on one side and the
			// tier below it happens to reach the same record.
			local := localTitleOf(t, c.seed, c.track)
			for _, side := range []struct {
				name string
				prov tierProvider
			}{{"pass", passProv}, {"single", singleProv}} {
				if n := side.prov.count("search:" + local); n != c.searches {
					t.Errorf("the %s made %d searches for %q, want %d — the two paths agree on the "+
						"row but not on what they spent to get it, which is the disagreement "+
						"both tiers exist to close (calls: %v)",
						side.name, n, local, c.searches, side.prov.history())
				}
			}
		})
	}
}

// --- the SEARCH tier: the last one, and the one that was still out of step ------
//
// Issue 17. The album tier above closed the disagreement issue 14 was about and
// left a smaller one directly beneath it, in writing: refFor sets TitleRef.Title
// but not Track/Artist, MusicBrainzProvider.Lookup routes a track with no id to
// trackDetails(ref.Track, ref.Artist), and trackDetails refuses a blank track name
// before it opens a socket. So an unanchored Track re-enriched alone DECLINED WITH
// ZERO REQUESTS and was filed with a search reason for a search nobody made —
// while a pass over the same Library searched for it by name and artist, and often
// matched.
//
// Closing it deliberately adds a request to the endpoint ADR-0049 measured
// shedding load globally, which is why the trade is written down here as well as
// in the code: the alternative is not "one fewer request", it is a wrong answer
// recorded for free.

// searchRefProvider is albumTierProvider with every track SEARCH's reference kept
// verbatim. The call log alone says a search happened; only the reference says
// whether it was the search a PASS would have made, which is the whole claim.
type searchRefProvider struct {
	*albumTierProvider
	mu         sync.Mutex
	searchRefs []TitleRef
}

func (p *searchRefProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind == "track" && strings.TrimSpace(ref.MusicbrainzID) == "" {
		p.mu.Lock()
		p.searchRefs = append(p.searchRefs, ref)
		p.mu.Unlock()
	}
	return p.albumTierProvider.Lookup(ctx, ref)
}

func (p *searchRefProvider) refs() []TitleRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TitleRef(nil), p.searchRefs...)
}

// searchTierProvider is the shape that reaches the search: a matched Album whose
// tracklist has no room for the Track (two spare positions, so the leftover rule
// cannot rescue it), and a source that holds the recording under its own name.
func searchTierProvider() *searchRefProvider {
	return &searchRefProvider{albumTierProvider: &albumTierProvider{
		tracklist: []TrackCandidate{
			entry(1, "Something Else", "rec-x"),
			entry(2, "Bonus One", "rec-y"),
			entry(3, "Bonus Two", "rec-z"),
		},
		searchHits: map[string]string{"Nowhere On The Release": "rec-search"},
	}}
}

func searchTierSeed() seedAlbum {
	return seedAlbum{entityRecord: "rg-she", tracks: []seedTrack{
		{id: "t1", title: "Nowhere On The Release", num: 1},
		{id: "t2", title: "Also Nowhere", num: 2},
	}}
}

// THE HEADLINE OF ISSUE 17, and it is a reference comparison rather than a call
// count on purpose. A Track its Album cannot name, re-enriched alone, sends the
// search a library pass sends for it — the same terms, field for field — and
// therefore reaches the same record. Before this it sent nothing at all and was
// filed unmatched.
func TestASingleReEnrichSendsThePassesSearch(t *testing.T) {
	passProv := searchTierProvider()
	passSvc, passDB := newAlbumFixture(t, passProv, searchTierSeed())
	singleProv := searchTierProvider()
	singleSvc, singleDB := newAlbumFixture(t, singleProv, searchTierSeed())

	if _, err := passSvc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	reEnrichAlone(t, singleSvc, "t1")

	single := singleProv.refs()
	if len(single) != 1 {
		t.Fatalf("the single re-enrich made %d searches, want exactly 1 — a Track no exact "+
			"anchor names must reach the search tier, and must reach it once (calls: %v)",
			len(single), singleProv.history())
	}
	// The pass searched for both of this album's tracks; t1 is the one being compared.
	var want TitleRef
	for _, ref := range passProv.refs() {
		if ref.Track == "Nowhere On The Release" {
			want = ref
		}
	}
	if want.Track == "" {
		t.Fatalf("the PASS made no search for t1 — the fixture no longer exercises the tier "+
			"(calls: %v)", passProv.history())
	}
	if got := single[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the single re-enrich searched with\n  %+v\nthe pass searched with\n  %+v\n"+
			"The Track's name, its Artist and its Album are what the search is narrowed by "+
			"(ADR-0050's query, and issue 15's release clause). A pass has all three because "+
			"it walked Artist → Album → Track; the single path has to read them, and if it "+
			"does not, trackDetails refuses a blank name and the Track declines with no "+
			"request at all.", got, want)
	}
	// And the row that results is the pass's row.
	if got := trackRow(t, singleDB, "t1"); got.MusicbrainzID != "rec-search" || got.EnrichmentStatus != "matched" {
		t.Fatalf("t1 = %q/%q, want rec-search/matched (calls: %v)",
			got.MusicbrainzID, got.EnrichmentStatus, singleProv.history())
	}
	if pass, single := rowOf(t, passDB, "t1"), rowOf(t, singleDB, "t1"); pass != single {
		t.Fatalf("pass = %+v, single = %+v", pass, single)
	}
}

// The trade this issue makes, stated as an assertion: ONE request, and it buys a
// record. The previous behaviour — zero requests and an `unmatched` row carrying a
// search reason for a search that never happened — is what the count of 0 below
// would mean, and it is the thing that must not come back.
func TestTheSearchTierCostsOneRequestAndNotZero(t *testing.T) {
	prov := searchTierProvider()
	svc, db := newAlbumFixture(t, prov, searchTierSeed())

	reEnrichAlone(t, svc, "t1")

	if n := prov.count("search:"); n != 1 {
		t.Fatalf("%d searches, want exactly 1 (calls: %v) — 0 is the defect (a wrong answer "+
			"recorded for free) and 2 is the retry ADR-0049 ruled out, because a second "+
			"request issued during a global shed pushes the wrong way", n, prov.history())
	}
	if got := trackRow(t, db, "t1"); got.EnrichmentReason != store.EnrichmentReasonNone {
		t.Errorf("reason = %q on a matched Track, want it cleared", got.EnrichmentReason)
	}
}

// A rejected top hit is unchanged by any of this (issue 05, ADR-0050): the search
// answered, nothing it offered was this song, and the row says so. This is the
// acceptance test seen from the single-Title path, which now has one to guard.
func TestASingleReEnrichRejectedByTheSearchSaysSearchRejected(t *testing.T) {
	prov := &albumTierProvider{
		// A transient tracklist failure: the album tier learned nothing and must not
		// diagnose the Track, so the search's own answer is what names the failure.
		tracklistErr: errShedding,
		searchErr:    ErrMatchRejected,
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1}},
	})

	reEnrichAlone(t, svc, "t1")

	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "unmatched" {
		t.Fatalf("status = %q, want unmatched — a rejection is a no-match, not a provider "+
			"failure to retry or park (calls: %v)", got.EnrichmentStatus, prov.history())
	}
	if got.EnrichmentReason != store.EnrichmentReasonSearchRejected {
		t.Fatalf("reason = %q, want %q — the search cluster answered and its top hit was "+
			"refused, which is a different next action from every album-scoped reason",
			got.EnrichmentReason, store.EnrichmentReasonSearchRejected)
	}
	if got.MusicbrainzID != "" {
		t.Errorf("recorded %q — the rejected candidate was stored anyway, the silent wrong "+
			"overview ADR-0050's acceptance test exists to prevent", got.MusicbrainzID)
	}
	if n := prov.count("search:"); n != 1 {
		t.Errorf("%d searches, want 1 (calls: %v)", n, prov.history())
	}
}

// --- the three tiers above still short-circuit ---------------------------------

// EVERY TIER ABOVE THE SEARCH STILL ANSWERS FIRST, and answering costs no search.
// This is the guard on the cost of issue 17: the request it adds is for the Tracks
// nothing exact can name, and for no others. A regression here would put the whole
// library back on the endpoint ADR-0049 took it off.
func TestTheTiersAboveTheSearchMakeNoSearch(t *testing.T) {
	cases := []struct {
		name       string
		seed       seedTrack
		recordings map[string]string
		tracklist  []TrackCandidate
		// tracklistCalls is how many AlbumTracklist reads the tier is allowed. The two
		// row-level tiers must not make even one; the album tier makes exactly one.
		tracklistCalls int
		// albumReads is how many times the single-Title album tier may be ENTERED. An
		// id already on the row must not cost so much as a store read (issue 14).
		albumReads int
		wantID     string
	}{{
		name:       "tier one: the enrichment record on the row",
		seed:       seedTrack{id: "t1", title: "Whisper Your Name", num: 1, record: "rec-stored"},
		recordings: map[string]string{"rec-stored": "Whisper Your Name"},
		wantID:     "rec-stored",
	}, {
		name:       "tier two: the recording id the FILE asserts",
		seed:       seedTrack{id: "t1", title: "Whisper Your Name", num: 1, recordingTag: "rec-tag"},
		recordings: map[string]string{"rec-tag": "Whisper Your Name"},
		wantID:     "rec-tag",
	}, {
		name:           "tier three: the id the Album's tracklist supplies",
		seed:           seedTrack{id: "t1", title: "Whisper Your Name", num: 1},
		recordings:     map[string]string{"rec-1": "Whisper Your Name"},
		tracklist:      []TrackCandidate{entry(1, "Whisper Your Name", "rec-1")},
		tracklistCalls: 1,
		albumReads:     1,
		wantID:         "rec-1",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov := &albumTierProvider{
				tracklist:  c.tracklist,
				recordings: c.recordings,
				// If any tier above leaks through to the search, it finds an answer — so a
				// leak shows up as a WRONG RECORD as well as a call, and cannot pass by
				// accidentally producing the right one.
				searchHits: map[string]string{"Whisper Your Name": "rec-leaked"},
			}
			svc, db := newAlbumFixture(t, prov, seedAlbum{
				entityRecord: "rg-she",
				tracks:       []seedTrack{c.seed},
			})
			reads := countAlbumReads(svc)

			reEnrichAlone(t, svc, "t1")

			if n := prov.count("search:"); n != 0 {
				t.Fatalf("%d searches, want 0 — an exact id names this recording, and ADR-0049's "+
					"whole point is that an exact id is not asked for as a text query "+
					"(calls: %v)", n, prov.history())
			}
			if n := prov.count("tracklist:"); n != c.tracklistCalls {
				t.Errorf("%d tracklist reads, want %d (calls: %v)", n, c.tracklistCalls, prov.history())
			}
			if reads.trackContexts != c.albumReads {
				t.Errorf("entered the album tier %d times, want %d", reads.trackContexts, c.albumReads)
			}
			if got := trackRow(t, db, "t1"); got.MusicbrainzID != c.wantID {
				t.Errorf("t1 = %q, want %q (calls: %v)", got.MusicbrainzID, c.wantID, prov.history())
			}
		})
	}
}

// A Library with Music enrichment switched off makes no search either — the leaf is
// recorded 'disabled' with nothing asked of anybody, which is what it was before
// the search terms started travelling.
func TestASingleReEnrichWithMusicDisabledStillAsksNothing(t *testing.T) {
	prov := searchTierProvider()
	svc, db := newAlbumFixture(t, prov, searchTierSeed())
	svc.SetProvider(prov, Enablement{Video: true, Music: false})

	reEnrichAlone(t, svc, "t1")

	if calls := prov.history(); len(calls) != 0 {
		t.Fatalf("made %d calls with Music enrichment off, want 0: %v", len(calls), calls)
	}
	if got := trackRow(t, db, "t1"); got.EnrichmentStatus != "disabled" {
		t.Errorf("status = %q, want disabled", got.EnrichmentStatus)
	}
}
