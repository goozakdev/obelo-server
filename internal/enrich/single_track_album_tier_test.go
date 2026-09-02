package enrich

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

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

// tierAgreementCase is one album shape resolved twice — once by a library pass, once
// by a single-Title re-enrich — over two identical fixtures.
type tierAgreementCase struct {
	name  string
	seed  seedAlbum
	pin   string // an Admin's chosen edition, "" for none
	prov  func() *editionProvider
	track string // the Track resolved both ways
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
// The four shapes are the four things the tier can do: pin by title, pin by position
// under a human's edition, decline with a tracklist in hand, and have no album to
// ask. Every one of them is a different record or a different reason, so a path that
// skipped the tier would fail here on all four.
func TestThePassAndTheSingleReEnrichReachTheSameRecord(t *testing.T) {
	cases := []tierAgreementCase{{
		name: "resolved from the album's tracklist",
		prov: func() *editionProvider {
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
		name:  "pinned by position under a chosen edition",
		prov:  viaggioProvider,
		seed:  viaggioSeed(),
		pin:   viaggioRelease,
		track: "t2",
	}, {
		name: "declined by a tracklist that was read",
		prov: func() *editionProvider {
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
		track: "t1",
	}, {
		name: "no album to ask",
		prov: func() *editionProvider {
			return &editionProvider{albumTierProvider: &albumTierProvider{}}
		},
		seed:  seedAlbum{tracks: []seedTrack{{id: "t1", title: "Whisper Your Name", num: 1}}},
		track: "t1",
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
			// And where a record WAS reached, the album tier is what reached it — not two
			// searches that happened to land on the same answer. (A Track the tier
			// declines still falls through to the search on the pass side, unchanged;
			// that is the fourth tier doing its job, and the rows above already agree.)
			if pass.RecordID == "" {
				return
			}
			for _, calls := range [][]string{passProv.history(), singleProv.history()} {
				for _, call := range calls {
					if strings.HasPrefix(call, "search:") {
						t.Errorf("reached the search cluster (%q) for a Track the album named — "+
							"the album tier is what these two paths are supposed to share "+
							"(calls: %v)", call, calls)
					}
				}
			}
		})
	}
}
