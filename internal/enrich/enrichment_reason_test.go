package enrich

import (
	"context"
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0050's reason table, driven through REAL enrichment passes over a real
// store, on the fixture the tracklist tier's own tests use (album_tier_pass_test.go).
//
// The point of every assertion here is that the five values are decided by
// DIFFERENT paths that produce IDENTICAL rows otherwise. A track whose album never
// matched and a track its album's tracklist declined both end the pass 'unmatched',
// with an empty musicbrainz_id, having made one failed search — the reason column
// is the only place they differ, and they want opposite actions from the Admin
// ("fix the album" versus "the album is the wrong release"). If a path stops
// writing its own value nothing else in the system notices.

// wantReason asserts one Track's settled status and reason together, because
// either alone is meaningless: a reason on a matched row is stale, and a status
// with no reason is the sentence the queue printed before this column existed.
func wantReason(t *testing.T, db *store.DB, id, status, reason string) {
	t.Helper()
	got := trackRow(t, db, id)
	if got.EnrichmentStatus != status || got.EnrichmentReason != reason {
		t.Fatalf("%s settled as (%q, %q), want (%q, %q)",
			id, got.EnrichmentStatus, got.EnrichmentReason, status, reason)
	}
}

// The biggest bucket in a real library: 365 of the developer's 730 unmatched
// tracks. The album never matched, so it could name none of its contents — and the
// action that clears all 365 is matching the ALBUM, not picking a recording for
// each track in turn.
//
// The album here has no record and no tag ids, so no tracklist call is even made
// (albumTracklistIDs returns tracklistNoAlbumRecord before the read), and the
// track falls through to a search that finds nothing.
func TestAnAlbumThatCouldNotMatchSaysAlbumUnmatched(t *testing.T) {
	prov := &albumTierProvider{} // no albumRG: the album SEARCH answers ErrNoMatch
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Fatalf("%d tracklist calls, want 0 — an album with no anchor is not asked", n)
	}
	wantReason(t, db, "t1", "unmatched", store.EnrichmentReasonAlbumUnmatched)
}

// The provider answering "this album HAS no tracklist" is the same DIAGNOSIS as
// the album never matching: either way the album could name none of its contents,
// and either way the Admin's move is on the album. It is emphatically not the same
// as the read FAILING — see the transient test below.
func TestAnAlbumWithNoTracklistAlsoSaysAlbumUnmatched(t *testing.T) {
	prov := &albumTierProvider{tracklistErr: ErrNoTracklist}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	wantReason(t, db, "t1", "unmatched", store.EnrichmentReasonAlbumUnmatched)
}

// The album matched, its tracklist was read, and the match rule found no room for
// this track. That is a diagnosis with an obvious action — the album is probably
// pinned to the wrong RELEASE (a remaster, a deluxe edition) — and it is the case
// that is NOT recoverable after the fact: it leaves exactly the row the test above
// leaves.
//
// Two spare positions keep issue 03's leftover rule from pairing the stray with
// the one hole and hiding the decline.
func TestATracklistThatDeclinesATrackSaysNotInTracklist(t *testing.T) {
	prov := &albumTierProvider{
		tracklist: []TrackCandidate{
			entry(1, "She", "rec-1"),
			entry(2, "Recipe for Love", "rec-2"),
			entry(3, "Buried in Blue", "rec-3"),
		},
		recordings: map[string]string{"rec-1": "She"},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks: []seedTrack{
			{id: "t1", title: "She", num: 1},
			{id: "t2", title: "A Song From Some Other Record", num: 2},
		},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	wantReason(t, db, "t2", "unmatched", store.EnrichmentReasonNotInTracklist)
	// Its neighbour matched from the same tracklist, so the reason is about THIS
	// track and not about the album's read having failed.
	if got := trackRow(t, db, "t1"); got.EnrichmentStatus != "matched" {
		t.Fatalf("t1 settled %q, want matched — the tracklist was read fine", got.EnrichmentStatus)
	}
}

// An EXACT id that resolves to nothing. The name was never the problem, so the
// action is on whatever asserts the id — which is why this must not read as
// "the search found nothing".
func TestAnIDThatResolvesToNothingSaysTagIDUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed seedTrack
	}{
		// The population ADR-0049 aimed at: a tagger wrote an id into the file and
		// MusicBrainz has no recording behind it.
		{"tag id", seedTrack{id: "t1", title: "She", num: 1, recordingTag: "rec-gone"}},
		// The record-column twin: an id a pass or an Admin stored that has since gone
		// away. Same broken thing, same action, so the same value.
		{"stored record", seedTrack{id: "t1", title: "She", num: 1,
			record: "rec-gone", status: "unmatched"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &albumTierProvider{recordings: map[string]string{}} // rec-gone is not there
			svc, db := newAlbumFixture(t, prov, seedAlbum{
				entityRecord: "rg-she",
				tracks:       []seedTrack{tc.seed},
			})

			if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
				t.Fatalf("pass: %v", err)
			}
			if n := prov.count("search:"); n != 0 {
				t.Fatalf("%d searches, want 0 — a track carrying an id never reaches the search", n)
			}
			wantReason(t, db, "t1", "unmatched", store.EnrichmentReasonTagIDUnresolved)
		})
	}
}

// The two SEARCH outcomes, told apart only by error value (issue 05):
// ErrMatchRejected wraps ErrNoMatch one-directionally, so testing the plain
// ErrNoMatch first would silently collapse them into one reason. A rejection means
// something close came back and was refused rather than stored blind, which is a
// different thing to tell an Admin than "MusicBrainz has nothing under this title".
//
// Both cases need the album tier to have said NOTHING, or the album-grained reason
// would (correctly) outrank the search's — which is itself worth knowing: with
// ADR-0050 in place, a search reason on a Track means its album was not able to
// speak this pass.
func TestTheTwoSearchOutcomesAreDistinguishable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		searchErr  error
		wantReason string
	}{
		{"empty result", ErrNoMatch, store.EnrichmentReasonSearchNoMatch},
		{"rejected top hit", ErrMatchRejected, store.EnrichmentReasonSearchRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &albumTierProvider{
				tracklistErr: statusError("musicbrainz", "/release", http.StatusServiceUnavailable),
				searchErr:    tc.searchErr,
			}
			svc, db := newAlbumFixture(t, prov, seedAlbum{
				entityRecord: "rg-she",
				tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
			})

			if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
				t.Fatalf("pass: %v", err)
			}
			wantReason(t, db, "t1", "unmatched", tc.wantReason)
		})
	}
}

// THE hard rule (issue 04): a transient tracklist failure must NEVER be written as
// 'album-unmatched'. The album is not unmatched — MusicBrainz was busy — and a
// reason recorded from an outage is exactly the sentence that outlives the problem
// it describes. The tracks fall through to search and are diagnosed by what the
// SEARCH did, or retried if that failed transiently too.
func TestATransientTracklistFailureIsNeverAlbumUnmatched(t *testing.T) {
	shed := statusError("musicbrainz", "/release", http.StatusServiceUnavailable)
	if !IsTransient(shed) {
		t.Fatal("the fixture's error is not transient; the test proves nothing")
	}
	prov := &albumTierProvider{tracklistErr: shed, searchErr: ErrNoMatch}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentReason == store.EnrichmentReasonAlbumUnmatched {
		t.Fatal("a 503 on the tracklist was recorded as 'album-unmatched' — it is an OUTAGE, " +
			"not a diagnosis, and the row would send the Admin to re-match an album that is " +
			"perfectly well matched (ADR-0050, issue 04)")
	}
	if got.EnrichmentReason != store.EnrichmentReasonSearchNoMatch {
		t.Fatalf("reason %q, want %q — the search is what actually settled this track",
			got.EnrichmentReason, store.EnrichmentReasonSearchNoMatch)
	}
}

// A transient LEAF failure records no reason and leaves the prior one standing
// (ADR-0048): the item is in-flight work, not a diagnosis. Clearing it would lose
// the diagnosis for the whole duration of an outage — precisely when the queue is
// longest — and writing one would put a sentence about the network in a column
// about the item.
func TestATransientLeafFailureLeavesThePriorReasonAlone(t *testing.T) {
	prov := &albumTierProvider{tracklistErr: ErrNoTracklist, searchErr: ErrNoMatch}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "She", num: 1}},
	})
	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	wantReason(t, db, "t1", "unmatched", store.EnrichmentReasonAlbumUnmatched)

	// Now MusicBrainz sheds load. The row is retried, not re-diagnosed.
	prov.searchErr = statusError("musicbrainz", "/recording", http.StatusServiceUnavailable)
	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "failed" || got.EnrichmentRetryAt == "" {
		t.Fatalf("status %q retry_at %q, want a scheduled retry", got.EnrichmentStatus, got.EnrichmentRetryAt)
	}
	if got.EnrichmentReason != store.EnrichmentReasonAlbumUnmatched {
		t.Fatalf("reason %q, want it untouched at %q — an outage says nothing about the item",
			got.EnrichmentReason, store.EnrichmentReasonAlbumUnmatched)
	}
}

// THE stale-row case, end to end and through the real pass: a Track that fails with
// a diagnosis and MATCHES on a later pass carries an EMPTY reason. This is the one
// "Done when" the ADR states as a consequence in its own right — a reason that
// outlives its failure is worse than none, because the row confidently explains a
// problem that no longer exists.
//
// The recovery here is the realistic one: the operator matches the ALBUM, which is
// exactly what 'album-unmatched' told them to do, and the album's tracklist then
// resolves the track without anyone touching it.
func TestAMatchOnALaterPassClearsTheReason(t *testing.T) {
	prov := &albumTierProvider{searchErr: ErrNoMatch}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "She", num: 1}},
	})

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	wantReason(t, db, "t1", "unmatched", store.EnrichmentReasonAlbumUnmatched)

	// The Admin fixes the album. Its tracklist now names the recording.
	prov.albumRG = "rg-she"
	prov.tracklist = []TrackCandidate{entry(1, "She", "rec-2")}
	prov.recordings = map[string]string{"rec-2": "She"}

	res, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if res.Matched != 1 {
		t.Fatalf("pass 2 matched %d, want 1 (calls: %v)", res.Matched, prov.history())
	}
	got := trackRow(t, db, "t1")
	if got.EnrichmentStatus != "matched" || got.EnrichmentReason != store.EnrichmentReasonNone {
		t.Fatalf("after the match: status %q, reason %q — want (matched, \"\"). The row still "+
			"carries a sentence sending the Admin to fix an album that is now matched",
			got.EnrichmentStatus, got.EnrichmentReason)
	}
}

// A non-Music leaf gets NO reason: the five values are Music-shaped, and stamping
// 'search-no-match' on an unmatched Movie would put the word "recording" in front
// of an Admin looking at a film. The empty value renders today's generic sentence,
// which is the correct answer until a Movie failure taxonomy is actually asked for.
func TestANonMusicLeafRecordsNoReason(t *testing.T) {
	lw := leafWork{title: store.Title{ID: "m1", Kind: "movie"}}
	if got := lw.unmatchedReason(ErrMatchRejected); got != store.EnrichmentReasonNone {
		t.Fatalf("a Movie was diagnosed %q, want \"\" — the reason set is Music-shaped and "+
			"four of its five values name an album, a tracklist or a recording id", got)
	}
}

// The classification's ORDER, asserted directly, because each step would be
// absorbed by the next if they were swapped:
//
//   - an id present outranks everything: the leaf never reached the search;
//   - the ALBUM tier outranks the search outcome, deliberately — a track under an
//     unmatched album searches and fails like any other, and letting
//     'search-rejected' win would rename the largest actionable bucket in the queue
//     after the least useful thing that happened to it;
//   - rejection is tested before plain no-match, because ErrMatchRejected wraps
//     ErrNoMatch and the other order collapses two reasons into one.
func TestTheReasonClassificationOrder(t *testing.T) {
	track := store.Title{ID: "t1", Kind: "track"}
	for _, tc := range []struct {
		name string
		lw   leafWork
		err  error
		want string
	}{
		{"an id beats the album tier", leafWork{title: track,
			ref: TitleRef{MusicbrainzID: "rec-1"}, tracklist: tracklistNoAlbumRecord},
			ErrNoMatch, store.EnrichmentReasonTagIDUnresolved},
		{"an id beats a rejection", leafWork{title: track, ref: TitleRef{MusicbrainzID: "rec-1"}},
			ErrMatchRejected, store.EnrichmentReasonTagIDUnresolved},
		{"the album tier beats a rejection", leafWork{title: track, tracklist: tracklistNoAlbumRecord},
			ErrMatchRejected, store.EnrichmentReasonAlbumUnmatched},
		{"a decline beats a rejection", leafWork{title: track, tracklist: tracklistRead},
			ErrMatchRejected, store.EnrichmentReasonNotInTracklist},
		{"a rejection beats plain no-match", leafWork{title: track},
			ErrMatchRejected, store.EnrichmentReasonSearchRejected},
		{"an empty search result is not a rejection", leafWork{title: track},
			ErrNoMatch, store.EnrichmentReasonSearchNoMatch},
		{"a provider that answered without an error is not a rejection", leafWork{title: track},
			nil, store.EnrichmentReasonSearchNoMatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lw.unmatchedReason(tc.err); got != tc.want {
				t.Fatalf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// One rule, both callers (ADR-0050): a Cascade fills the same queue the pass does,
// so it must not invent a third vocabulary for it. mapAlbumTracks decides between
// the same two album-grained values from the same fact — whether a tracklist
// existed at all.
func TestTheCascadeWritesThePassesReasons(t *testing.T) {
	for _, tc := range []struct {
		name string
		cand *Candidate
		want string
	}{
		// No candidate: the album could not be resolved, so it named none of its
		// tracks. The same fact routeAlbumTracksToAttention writes when the artist
		// recursion cannot match an album at all.
		{"no candidate", nil, store.EnrichmentReasonAlbumUnmatched},
		// A candidate whose tracklist has no room for the track: the release is wrong.
		{"a tracklist that declines", &Candidate{ExternalID: "rg-she", Tracklist: []TrackCandidate{
			entry(1, "Recipe for Love", "rec-9"),
			entry(2, "Buried in Blue", "rec-8"),
		}}, store.EnrichmentReasonNotInTracklist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := newAlbumFixture(t, &albumTierProvider{}, seedAlbum{
				tracks: []seedTrack{{id: "t1", title: "She", num: 1}},
			})
			if _, err := svc.mapAlbumTracks(context.Background(), "al1", tc.cand); err != nil {
				t.Fatalf("mapAlbumTracks: %v", err)
			}
			wantReason(t, db, "t1", "unmatched", tc.want)
		})
	}
}
