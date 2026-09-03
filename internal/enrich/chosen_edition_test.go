package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// ADR-0052, the storage half: the EDITION an Admin named is kept, and it is what
// decorates the album's tracks.
//
// The motivating case is real. An operator found the exact release of Andrea
// Bocelli's *Viaggio Italiano* on musicbrainz.org, pasted its /release/ URL into Fix
// info, and the system resolved it to its parent release-group — correctly, that is
// what an album IS — and threw the release away. The album then went back to picking
// an edition by track-count fit, which is the thing the operator had just spent a
// trip to someone else's website overruling.

const (
	// The real ids from the motivating library: the release-group that was stored,
	// and the release that was not.
	viaggioGroup   = "054a22c3-742e-34d3-8ebf-ef912e3679e6"
	viaggioRelease = "7c2f5f27-4d4e-4a51-9a9e-1e2d3f4a5b6c"
	// A release of some OTHER release-group entirely.
	strangerRelease = "9f1b6d2e-3a4c-4f5d-8e7a-0b1c2d3e4f5a"
)

// --- 1. the paste path: both ids survive it -----------------------------------

// editionPasteProvider resolves an album Lookup the way MusicBrainz does: a pinned
// RELEASE resolves to its parent release-group (which is what gets stored as the
// record), a pinned release-GROUP resolves to itself.
type editionPasteProvider struct{ parentOf map[string]string }

func (p editionPasteProvider) Lookup(_ context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind != "album" {
		return TitleMetadata{Matched: true, Name: ref.Title, ExternalID: "artist-1", Source: "musicbrainz"}, nil
	}
	if rel := strings.TrimSpace(ref.ReleaseMBID); rel != "" {
		rg, ok := p.parentOf[rel]
		if !ok {
			return TitleMetadata{}, ErrNoMatch
		}
		return TitleMetadata{Matched: true, Name: "Viaggio Italiano", ExternalID: rg, Source: "musicbrainz"}, nil
	}
	if id := strings.TrimSpace(ref.MusicbrainzID); id != "" {
		return TitleMetadata{Matched: true, Name: "Viaggio Italiano", ExternalID: id, Source: "musicbrainz"}, nil
	}
	return TitleMetadata{}, ErrNoMatch
}

func (editionPasteProvider) Search(context.Context, string, string, SearchOptions) ([]Candidate, error) {
	return nil, ErrSearchUnavailable
}

func (editionPasteProvider) ArtworkCandidates(context.Context, TitleRef, string) ([]ArtworkCandidate, error) {
	return nil, nil
}

func albumEnrichment(t *testing.T, db *store.DB) store.EntityEnrichment {
	t.Helper()
	e, err := db.EntityEnrichmentByID(store.EntityAlbum, "al1")
	if err != nil {
		t.Fatalf("read album enrichment: %v", err)
	}
	return e
}

// The headline. A pasted /release/ URL stores the release-GROUP as the record (album
// identity is unchanged, ADR-0038) AND the RELEASE as the chosen edition, with the
// origin that says a human chose it.
func TestPastingAReleaseURLKeepsBothTheGroupAndTheEdition(t *testing.T) {
	prov := editionPasteProvider{parentOf: map[string]string{viaggioRelease: viaggioGroup}}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})

	// What the Admin actually does: paste the URL, see the preview, apply it.
	cand, err := svc.PreviewEntityExternal(context.Background(), store.EntityAlbum, "al1",
		"https://musicbrainz.org/release/"+viaggioRelease)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if cand.ExternalID != viaggioGroup {
		t.Errorf("preview externalId = %q, want the parent release-group %q", cand.ExternalID, viaggioGroup)
	}
	if cand.ReleaseID != viaggioRelease {
		t.Fatalf("preview releaseId = %q, want %q — the preview→apply round trip is exactly "+
			"where the edition used to be dropped", cand.ReleaseID, viaggioRelease)
	}

	if err := svc.ApplyEntityOverride(context.Background(), store.EntityAlbum, "al1",
		EntityPin{ExternalID: cand.ExternalID, ReleaseID: cand.ReleaseID}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	e := albumEnrichment(t, db)
	if e.ExternalID != viaggioGroup {
		t.Errorf("external_id = %q, want the release-group %q", e.ExternalID, viaggioGroup)
	}
	if e.ExternalReleaseID != viaggioRelease {
		t.Fatalf("external_release_id = %q, want %q — the one moment a human names an "+
			"edition, and it must not be upgraded to a release-group and forgotten",
			e.ExternalReleaseID, viaggioRelease)
	}
	if e.ExternalIDOrigin != store.OriginChosen {
		t.Errorf("origin = %q, want %q", e.ExternalIDOrigin, store.OriginChosen)
	}
	if got := e.ChosenReleaseID(); got != viaggioRelease {
		t.Errorf("ChosenReleaseID() = %q, want %q", got, viaggioRelease)
	}

	// Identity is untouched: the album keys on the release-group as it always did.
	var key string
	if err := db.QueryRow(`SELECT identity_key FROM albums WHERE id = 'al1'`).Scan(&key); err != nil {
		t.Fatalf("read identity_key: %v", err)
	}
	if key != "artist:harry connick jr|album:she" {
		t.Errorf("identity_key = %q — pinning an edition must re-key nothing (ADR-0038)", key)
	}
}

// A pasted release-GROUP URL is the Admin naming a LESS specific thing, so it clears
// whatever edition was stored. A stale edition under a new group would silently
// decorate the album from a stranger's tracklist.
func TestPastingAReleaseGroupURLClearsTheChosenEdition(t *testing.T) {
	const otherGroup = "629a5133-b9e6-43c5-8cb6-594a7cbfbfed"
	prov := editionPasteProvider{parentOf: map[string]string{viaggioRelease: viaggioGroup}}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		tracks: []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	if err := svc.ApplyEntityOverride(context.Background(), store.EntityAlbum, "al1",
		EntityPin{ExternalID: viaggioGroup, ReleaseID: viaggioRelease}); err != nil {
		t.Fatalf("pin the edition: %v", err)
	}

	cand, err := svc.PreviewEntityExternal(context.Background(), store.EntityAlbum, "al1",
		"https://musicbrainz.org/release-group/"+otherGroup)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if cand.ReleaseID != "" {
		t.Errorf("a /release-group/ paste previewed releaseId %q, want none", cand.ReleaseID)
	}
	if err := svc.ApplyEntityOverride(context.Background(), store.EntityAlbum, "al1",
		EntityPin{ExternalID: cand.ExternalID, ReleaseID: cand.ReleaseID}); err != nil {
		t.Fatalf("apply the group: %v", err)
	}

	e := albumEnrichment(t, db)
	if e.ExternalID != otherGroup {
		t.Errorf("external_id = %q, want %q", e.ExternalID, otherGroup)
	}
	if e.ExternalReleaseID != "" || e.ChosenReleaseID() != "" {
		t.Fatalf("external_release_id = %q after pinning a release-GROUP, want it cleared",
			e.ExternalReleaseID)
	}
}

// --- 2. the provider seam: a chosen edition is never silently swapped ----------

// A CHOSEN release that is not of this album's release-group stops the read rather
// than falling through to fit-selection inside the provider. Falling through would
// hand back a tracklist the caller could not tell apart from the human's edition,
// and would license position-alone mapping (issue 11) against a release nobody
// asserted — the stranger's-tracklist decoration ADR-0052 forbids.
func TestAChosenStrangerReleaseIsNotSilentlySwappedForAFit(t *testing.T) {
	build := func(t *testing.T) (*MusicBrainzProvider, *tracklistStub) {
		return newTracklistStub(t,
			stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she",
				Discs: [][]stubTrack{disc("std", 3)}},
			stubRelease{ID: strangerRelease, Date: "1997-01-01", RGID: "rg-other",
				Discs: [][]stubTrack{disc("stranger", 3)}},
		)
	}

	t.Run("chosen", func(t *testing.T) {
		p, stub := build(t)
		tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
			ReleaseGroupID: "rg-she", ReleaseID: strangerRelease, ReleaseIDChosen: true,
			LocalTrackCount: 3,
		})
		if !errors.Is(err, ErrNoTracklist) {
			t.Fatalf("(%v, %v), want ErrNoTracklist — a pin that is not of this album must be "+
				"reported, not quietly replaced", gotTitles(tl), err)
		}
		if reqs := stub.requests(); len(reqs) != 1 {
			t.Errorf("made %d requests, want 1 — the provider must not browse for a fit under a "+
				"pin it just rejected: %v", len(reqs), reqs)
		}
	})

	t.Run("from the files", func(t *testing.T) {
		p, _ := build(t)
		tl, err := p.AlbumTracklist(context.Background(), TracklistRequest{
			ReleaseGroupID: "rg-she", ReleaseID: strangerRelease, LocalTrackCount: 3,
		})
		if err != nil {
			t.Fatalf("AlbumTracklist: %v", err)
		}
		// Unchanged from issue 02: a MIS-TAGGED file falls through to fit silently,
		// because nobody asserted anything and there is no licence to withhold.
		wantTitles(t, tl, "1/1 std 1", "1/2 std 2", "1/3 std 3")
	})
}

// --- 3. the Service seam: chosen → tag → fit, and the licence -----------------

func editionService(t *testing.T, releases ...stubRelease) (*Service, providerSnapshot, *tracklistStub) {
	t.Helper()
	p, stub := newTracklistStub(t, releases...)
	svc := NewService(nil, p, nil, Enablement{Music: true}, "", 0)
	svc.tracklists = newListCache[[]TrackCandidate](DefaultAlbumTracklistCacheTTL)
	return svc, providerSnapshot{provider: p, enablement: Enablement{Music: true}}, stub
}

// The precedence, top tier: an Admin's edition outranks the release the FILES name
// AND outranks fit, and the tracklist it produces carries the licence.
func TestTracklistPrefersTheChosenEditionOverTagAndFit(t *testing.T) {
	svc, snap, stub := editionService(t,
		stubRelease{ID: "rel-tag", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("tag", 3)}},
		stubRelease{ID: "rel-chosen", Date: "2011-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("chosen", 3)}},
	)

	res, err := svc.albumTracklistFor(context.Background(), snap, TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-tag", LocalTrackCount: 3,
	}, "rel-chosen")
	if err != nil {
		t.Fatalf("albumTracklistFor: %v", err)
	}
	wantTitles(t, res.Tracks, "1/1 chosen 1", "1/2 chosen 2", "1/3 chosen 3")
	if !res.FromChosenEdition {
		t.Error("FromChosenEdition = false — this IS the human's edition, and issue 11's " +
			"licence hangs on exactly that fact")
	}
	reqs := stub.requests()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1 — the pin answers in the call it needed anyway: %v",
			len(reqs), reqs)
	}
	if !strings.HasPrefix(reqs[0], "/release/rel-chosen?") {
		t.Errorf("asked for %q, want the chosen edition — the tag release is the FALLBACK now", reqs[0])
	}
}

// The contradiction: a chosen edition of some other release-group. It is ignored,
// the album falls back to the release its FILES name, and the licence is withheld —
// what came back is not what the human asserted.
func TestAChosenEditionOfAnotherAlbumFallsBackToTheTagRelease(t *testing.T) {
	svc, snap, _ := editionService(t,
		stubRelease{ID: "rel-tag", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("tag", 3)}},
		stubRelease{ID: strangerRelease, Date: "1997-01-01", RGID: "rg-other",
			Discs: [][]stubTrack{disc("stranger", 3)}},
	)

	res, err := svc.albumTracklistFor(context.Background(), snap, TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-tag", LocalTrackCount: 3,
	}, strangerRelease)
	if err != nil {
		t.Fatalf("albumTracklistFor: %v", err)
	}
	wantTitles(t, res.Tracks, "1/1 tag 1", "1/2 tag 2", "1/3 tag 3")
	if res.FromChosenEdition {
		t.Error("FromChosenEdition = true on a tracklist the pin did not produce — issue 11 " +
			"would then pin every track by position against an edition nobody asserted")
	}
	for _, tr := range res.Tracks {
		if strings.HasPrefix(tr.Title, "stranger") {
			t.Fatalf("decorated from the stranger's tracklist (%q) — never", tr.Title)
		}
	}
}

// The same contradiction with nothing in the tags: the album falls all the way
// through to fit, still without the licence.
func TestAChosenEditionOfAnotherAlbumFallsBackToFit(t *testing.T) {
	svc, snap, _ := editionService(t,
		stubRelease{ID: "rel-std", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("std", 3)}},
		stubRelease{ID: strangerRelease, Date: "1997-01-01", RGID: "rg-other",
			Discs: [][]stubTrack{disc("stranger", 3)}},
	)

	res, err := svc.albumTracklistFor(context.Background(), snap,
		TracklistRequest{ReleaseGroupID: "rg-she", LocalTrackCount: 3}, strangerRelease)
	if err != nil {
		t.Fatalf("albumTracklistFor: %v", err)
	}
	wantTitles(t, res.Tracks, "1/1 std 1", "1/2 std 2", "1/3 std 3")
	if res.FromChosenEdition {
		t.Error("FromChosenEdition = true on a fit tracklist")
	}
}

// An album nobody pinned costs exactly what it cost before this issue: one call, the
// same request, no licence.
func TestAnAlbumWithNoChosenEditionIsUnchanged(t *testing.T) {
	svc, snap, stub := editionService(t,
		stubRelease{ID: "rel-tag", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("tag", 3)}},
	)

	res, err := svc.albumTracklistFor(context.Background(), snap, TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-tag", LocalTrackCount: 3,
	}, "")
	if err != nil {
		t.Fatalf("albumTracklistFor: %v", err)
	}
	wantTitles(t, res.Tracks, "1/1 tag 1", "1/2 tag 2", "1/3 tag 3")
	if res.FromChosenEdition {
		t.Error("FromChosenEdition = true with nothing pinned")
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — an unpinned album must not start paying for the "+
			"precedence it does not use: %v", len(reqs), reqs)
	}
}

// A refusal under a pin comes straight back as itself and issues nothing further:
// ADR-0049's rule is unchanged, and "the pin did not apply" must never be confused
// with "MusicBrainz was busy".
func TestATransientFailureUnderAPinIsNotAFallBack(t *testing.T) {
	svc, snap, stub := editionService(t,
		stubRelease{ID: "rel-tag", Date: "1994-06-21", RGID: "rg-she", Discs: [][]stubTrack{disc("tag", 3)}},
		stubRelease{ID: "rel-chosen", Date: "2011-01-01", RGID: "rg-she", Discs: [][]stubTrack{disc("chosen", 3)}},
	)
	stub.fail("/release/rel-chosen", http.StatusServiceUnavailable)

	_, err := svc.albumTracklistFor(context.Background(), snap, TracklistRequest{
		ReleaseGroupID: "rg-she", ReleaseID: "rel-tag", LocalTrackCount: 3,
	}, "rel-chosen")
	if err == nil {
		t.Fatal("want the refusal back")
	}
	if errors.Is(err, ErrNoTracklist) {
		t.Error("a 503 must stay distinguishable from a settled 'there is nothing here' (ADR-0049)")
	}
	if reqs := stub.requests(); len(reqs) != 1 {
		t.Errorf("made %d requests, want 1 — a shedding host must not be asked twice: %v",
			len(reqs), reqs)
	}
}

// --- 4. the pass: the chosen edition reaches the request ----------------------

// editionProvider is albumTierProvider with a tracklist per RELEASE, so a test can
// tell which edition the pass asked for. Its AlbumTracklist mirrors the real
// provider's contract, parentage check included.
type editionProvider struct {
	*albumTierProvider
	byRelease map[string][]TrackCandidate
	parentOf  map[string]string
	fit       []TrackCandidate
	// searchAlbums is what an album SEARCH answers, which is how the Cascade obtains
	// the corrected release-group and its (roughly-right) preview tracklist.
	searchAlbums []Candidate
	// unknownGroups makes an album's by-id lookup fail, which is how a pass ends up
	// anchoring on the release-group the FILES assert instead of on the album's own
	// record (a transient parent failure, ADR-0048).
	unknownGroups map[string]bool
}

func (p *editionProvider) Lookup(ctx context.Context, ref TitleRef) (TitleMetadata, error) {
	if ref.Kind == "album" && p.unknownGroups[strings.TrimSpace(ref.MusicbrainzID)] {
		p.note("rg:" + ref.MusicbrainzID)
		return TitleMetadata{}, ErrNoMatch
	}
	return p.albumTierProvider.Lookup(ctx, ref)
}

func (p *editionProvider) Search(_ context.Context, kind, _ string, _ SearchOptions) ([]Candidate, error) {
	if kind != "album" || len(p.searchAlbums) == 0 {
		return nil, ErrSearchUnavailable
	}
	p.note("album?search")
	return p.searchAlbums, nil
}

func (p *editionProvider) AlbumTracklist(_ context.Context, req TracklistRequest) ([]TrackCandidate, error) {
	p.note(fmt.Sprintf("tracklist:%s|%s|chosen=%t|%d",
		req.ReleaseGroupID, req.ReleaseID, req.ReleaseIDChosen, req.LocalTrackCount))
	if rel := strings.TrimSpace(req.ReleaseID); rel != "" {
		if strings.EqualFold(p.parentOf[rel], req.ReleaseGroupID) && len(p.byRelease[rel]) > 0 {
			return p.byRelease[rel], nil
		}
		if req.ReleaseIDChosen {
			return nil, ErrNoTracklist
		}
	}
	if len(p.fit) == 0 {
		return nil, ErrNoTracklist
	}
	return p.fit, nil
}

// pinEdition records an Admin's chosen edition on the fixture's album, exactly as
// the apply path would.
func pinEdition(t *testing.T, db *store.DB, releaseID string) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE entity_enrichment SET external_release_id = ?, external_id_origin = 'chosen'
		   WHERE entity_type = 'album' AND entity_id = 'al1'`, releaseID); err != nil {
		t.Fatalf("pin edition: %v", err)
	}
}

// A real pass over a real store: the chosen edition is what the tracklist request
// carries, and its recordings are what the tracks are pinned to — not the tag
// release's, and not a fit's.
func TestThePassResolvesTracksFromTheChosenEdition(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-chosen-1": "Nessun Dorma", "rec-tag-1": "Nessun Dorma"},
		},
		parentOf: map[string]string{"rel-chosen": "rg-she", "rel-tag": "rg-she"},
		byRelease: map[string][]TrackCandidate{
			"rel-chosen": {entry(1, "Nessun Dorma", "rec-chosen-1")},
			"rel-tag":    {entry(1, "Nessun Dorma", "rec-tag-1")},
		},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		releaseTag:   "rel-tag",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	pinEdition(t, db, "rel-chosen")

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}

	want := "tracklist:rg-she|rel-chosen|chosen=true|1"
	var got string
	for _, c := range prov.history() {
		if strings.HasPrefix(c, "tracklist:") {
			got = c
		}
	}
	if got != want {
		t.Fatalf("tracklist request %q, want %q — the Admin's edition is the top tier of the "+
			"anchor precedence, and WHETHER it was chosen has to travel with it (calls: %v)",
			got, want, prov.history())
	}
	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-chosen-1" {
		t.Errorf("track pinned to %q, want rec-chosen-1 — the album decorated from the edition "+
			"the operator went to musicbrainz.org to name", rec)
	}
}

// The pass's own version of the contradiction: a pinned edition of another
// release-group is ignored and the album falls back to the release its files name.
func TestThePassIgnoresAChosenEditionOfAnotherAlbum(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-tag-1": "Nessun Dorma", "rec-stranger-1": "Something Else"},
		},
		parentOf: map[string]string{"rel-tag": "rg-she", strangerRelease: "rg-other"},
		byRelease: map[string][]TrackCandidate{
			"rel-tag":       {entry(1, "Nessun Dorma", "rec-tag-1")},
			strangerRelease: {entry(1, "Something Else", "rec-stranger-1")},
		},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		releaseTag:   "rel-tag",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	pinEdition(t, db, strangerRelease)

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeNew); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-tag-1" {
		t.Fatalf("track pinned to %q, want rec-tag-1 — a chosen release whose parent is not "+
			"this album's is a contradiction, and the album's own group wins (calls: %v)",
			rec, prov.history())
	}
	for _, c := range prov.history() {
		if strings.Contains(c, "rec-stranger") {
			t.Fatalf("looked the stranger's recording up (%q) — never decorate from a stranger", c)
		}
	}
}

// A pinned edition read under a release-group that is NOT the one it was pinned
// under is ignored WITHOUT a call. This is the transient-parent-failure case: the
// album's own record could not be resolved this pass, so the tier anchored on the
// release-group the FILES assert — and the Admin's edition is an edition of the
// other one. Reading it there is the stranger's tracklist by another route, and it
// is cheaper to refuse before the call than to discover it after.
func TestAChosenEditionIsIgnoredUnderADifferentReleaseGroup(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{recordings: map[string]string{"rec-fit-1": "Nessun Dorma"}},
		parentOf:          map[string]string{"rel-chosen": "rg-she"},
		byRelease:         map[string][]TrackCandidate{"rel-chosen": {entry(1, "Nessun Dorma", "rec-chosen-1")}},
		fit:               []TrackCandidate{entry(1, "Nessun Dorma", "rec-fit-1")},
		unknownGroups:     map[string]bool{"rg-she": true},
	}
	// The album's record says rg-she and its files say rg-tag; the record's lookup
	// comes back empty this pass, so the anchor is rg-tag.
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		rgTag:        "rg-tag",
		entityRecord: "rg-she",
		entityStatus: "unmatched",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	pinEdition(t, db, "rel-chosen")

	if _, err := svc.EnrichLibrary(context.Background(), "lib", ModeFull); err != nil {
		t.Fatalf("pass: %v", err)
	}
	for _, c := range prov.history() {
		if strings.HasPrefix(c, "tracklist:") && strings.Contains(c, "chosen=true") {
			t.Fatalf("read the pinned edition under a release-group it was not pinned under (%q)"+
				" (calls: %v)", c, prov.history())
		}
	}
	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-fit-1" {
		t.Errorf("track pinned to %q, want rec-fit-1 — the album falls back to what it can say "+
			"for itself (calls: %v)", rec, prov.history())
	}
}

// --- 5. the cascade agrees with the pass --------------------------------------

// "Also apply to children" on an album whose EDITION the Admin named decorates from
// that edition, not from the candidate's preview tracklist — which is whichever
// release MusicBrainz listed first, and is a wrong answer for every position after a
// deluxe edition's first bonus track. The two paths must not disagree about which
// release an album is (ADR-0052), because issue 11 licenses position-alone mapping
// in both of them from the same fact.
func TestTheCascadeDecoratesFromTheChosenEdition(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-chosen-1": "Nessun Dorma"},
		},
		parentOf:  map[string]string{"rel-chosen": "rg-she"},
		byRelease: map[string][]TrackCandidate{"rel-chosen": {entry(1, "Nessun Dorma", "rec-chosen-1")}},
		searchAlbums: []Candidate{{
			ExternalID: "rg-she", Title: "She", Kind: "album",
			// The preview: the right album, an arbitrary edition of it.
			Tracklist: []TrackCandidate{entry(1, "Nessun Dorma", "rec-preview-1")},
		}},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})
	pinEdition(t, db, "rel-chosen")

	sum, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", "rg-she")
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if sum.Updated != 1 {
		t.Fatalf("cascade updated %d, want 1 (calls: %v)", sum.Updated, prov.history())
	}
	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-chosen-1" {
		t.Errorf("cascade pinned %q, want rec-chosen-1 — the cascade and the pass must resolve "+
			"the same edition (calls: %v)", rec, prov.history())
	}
}

// An album nobody pinned takes none of it: the cascade uses the candidate's own
// preview tracklist and makes no extra provider call, exactly as before ADR-0052.
func TestTheCascadeIsUnchangedWithNoChosenEdition(t *testing.T) {
	prov := &editionProvider{
		albumTierProvider: &albumTierProvider{
			recordings: map[string]string{"rec-preview-1": "Nessun Dorma"},
		},
		searchAlbums: []Candidate{{
			ExternalID: "rg-she", Title: "She", Kind: "album",
			Tracklist: []TrackCandidate{entry(1, "Nessun Dorma", "rec-preview-1")},
		}},
	}
	svc, db := newAlbumFixture(t, prov, seedAlbum{
		entityRecord: "rg-she",
		tracks:       []seedTrack{{id: "t1", title: "Nessun Dorma", num: 1}},
	})

	if _, err := svc.CascadeEntity(context.Background(), store.EntityAlbum, "al1", "rg-she"); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if rec := trackRow(t, db, "t1").MusicbrainzID; rec != "rec-preview-1" {
		t.Errorf("cascade pinned %q, want rec-preview-1 (calls: %v)", rec, prov.history())
	}
	if n := prov.count("tracklist:"); n != 0 {
		t.Errorf("made %d tracklist reads, want 0 — an unpinned album must not start paying for "+
			"a precedence it does not use (calls: %v)", n, prov.history())
	}
}
