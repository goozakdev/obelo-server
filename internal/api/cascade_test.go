package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goozakdev/obelo-server/internal/enrich"
	"github.com/goozakdev/obelo-server/internal/testharness"
)

// item-editing issue 05 black-box tests: the Cascade engine — "also apply to
// children" (ADR-0019), the hardest correctness surface in the feature. Driven
// through the HTTP API with the FAKE MetadataProvider (its Search seam answering
// the parent kinds and carrying album tracklists with per-track recording ids),
// zero network. Asserts observable behavior: positional Album→tracks mapping with a
// count mismatch (matching tracks updated, mismatch to attention, no abort);
// Show→episodes positional; Artist→albums by title then RECURSE to tracks;
// durability across a later full pass; the skip rule for a child's own
// override/lock; correct summary counts; cascade on BOTH Fix-info and Wrong-item;
// Fix-label never cascades; and a childless leaf ignores the flag.

// --- wire shapes ------------------------------------------------------------

// cascadeSummaryResp reads the "also apply to children" summary embedded in a
// parent Edit-item apply response.
type cascadeSummaryResp struct {
	Updated   int `json:"updated"`
	Attention int `json:"attention"`
}

// cascadeDetailResp reads a parent apply response including the cascade summary.
type cascadeDetailResp struct {
	Overview           string              `json:"overview"`
	EnrichmentStatus   string              `json:"enrichmentStatus"`
	LockedFields       []string            `json:"lockedFields"`
	EnrichmentOverride *entityOverrideResp `json:"enrichmentOverride"`
	Cascade            *cascadeSummaryResp `json:"cascade"`
}

// --- helpers ----------------------------------------------------------------

// applyAlbumOverrideCascade PUTs an album Fix-info override with "also apply to
// children" ticked, returning the response (with cascade summary).
func applyEntityOverrideCascade(t *testing.T, srv *testharness.Server, token, kindPath, id, externalID string) cascadeDetailResp {
	t.Helper()
	var d cascadeDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/"+kindPath+"/"+id+"/enrichmentOverride",
		token, map[string]any{"externalId": externalID, "cascade": true}, &d)
	if status != http.StatusOK {
		t.Fatalf("PUT %s override (cascade) = %d, want 200; body: %s", kindPath, status, body)
	}
	return d
}

// okComputerAlbum locates Radiohead's "OK Computer" album id + its two track ids
// (Airbag position 1, Paranoid Android position 2). Fatal if the fixture changed.
func okComputerAlbum(t *testing.T, srv *testharness.Server, token, libID string) (albumID, airbagID, paranoidID string) {
	t.Helper()
	artists := listArtists(t, srv, token, libID)
	artistID := findArtist(t, artists, "Radiohead")
	for _, al := range artistAlbums(t, srv, token, artistID).Albums {
		if al.Title != "OK Computer" {
			continue
		}
		albumID = al.ID
		for _, tr := range albumTracks(t, srv, token, al.ID).Tracks {
			switch tr.Title {
			case "Airbag":
				airbagID = tr.ID
			case "Paranoid Android":
				paranoidID = tr.ID
			}
		}
	}
	if albumID == "" || airbagID == "" || paranoidID == "" {
		t.Fatalf("OK Computer fixture not found (album=%q airbag=%q paranoid=%q)", albumID, airbagID, paranoidID)
	}
	return albumID, airbagID, paranoidID
}

// --- Album → tracks: positional map + count mismatch + durability -----------

// TestCascadeAlbumTracksPositional applies an Album Fix-info override with cascade
// on: the local tracks map positionally onto the corrected release's tracklist. Here
// the tracklist carries only position 1 (Airbag) — a track-count mismatch — so Airbag
// is updated durably and Paranoid Android (position 2, no counterpart) lands in the
// attention list, without aborting. The summary reports updated=1, attention=1.
func TestCascadeAlbumTracksPositional(t *testing.T) {
	requireMusicFixtures(t)
	// A tracklist with ONLY position 1: position 2 (Paranoid) has no counterpart.
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
				Tracklist: []enrich.TrackCandidate{
					{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"},
				},
			}}, nil
		},
		fn: musicCascadeLookup(),
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	albumID, airbagID, paranoidID := okComputerAlbum(t, srv, token, libID)

	// (AC) Apply the album override with cascade → positional map runs.
	applied := applyEntityOverrideCascade(t, srv, token, "albums", albumID, "alb-okc")
	if applied.Cascade == nil {
		t.Fatalf("no cascade summary in album apply response")
	}
	if applied.Cascade.Updated != 1 || applied.Cascade.Attention != 1 {
		t.Errorf("cascade summary = %+v, want {Updated:1, Attention:1}", *applied.Cascade)
	}

	// (AC) The matched track (Airbag) got the corrected recording durably.
	airbag := getEnrichedDetail(t, srv, token, airbagID)
	if airbag.Overview != "CASCADE Airbag" {
		t.Errorf("Airbag overview = %q, want the cascaded recording's", airbag.Overview)
	}
	// (AC) The mismatched track (Paranoid) is routed to the attention list, not clobbered.
	if pa, ok := attentionHas(listEnrichmentAttention(t, srv, token, libID), "Paranoid Android"); !ok || pa.ID != paranoidID {
		t.Errorf("Paranoid Android (position 2, no counterpart) not in the attention list")
	}

	// (AC) Durable: a later full pass resolves Airbag BY the pinned recording id and
	// does not revert.
	enrichLib(t, srv, token, libID, "full")
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != "CASCADE Airbag" {
		t.Errorf("Airbag override reverted on full re-enrich: %q", d.Overview)
	}
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "track" && ref.MusicbrainzID == "rec-airbag" {
			sawPinned = true
		}
	}
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("full pass never resolved Airbag by its pinned recording id")
	}
}

// --- Skip rule: a child's own override / lock always wins -------------------

// TestCascadeSkipsChildOwnOverrideAndLock: a track with its OWN prior Enrichment
// override, and a track with a Locked field, are BOTH preserved (skipped) by an
// album cascade rather than clobbered.
func TestCascadeSkipsChildOwnOverrideAndLock(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
				Tracklist: []enrich.TrackCandidate{
					{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"},
					{Disc: 1, Position: 2, Title: "Paranoid Android", ExternalID: "rec-para"},
				},
			}}, nil
		},
		fn: musicCascadeLookup(),
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	albumID, airbagID, paranoidID := okComputerAlbum(t, srv, token, libID)

	// Paranoid gets its OWN prior track override (a durable musicbrainz_id pin).
	if p := applyOverride(t, srv, token, paranoidID, "rec-para-manual"); p.Overview != "MANUAL Paranoid" {
		t.Fatalf("seed track override failed: overview=%q", p.Overview)
	}
	// Airbag gets a hand-edited (Locked) overview.
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/titles/"+airbagID+"/metadata", token,
		map[string]any{"overview": "MY Airbag note"}, nil); st != http.StatusOK {
		t.Fatalf("seed track lock failed")
	}

	// Cascade the album: both children carry their own correction → both skipped.
	applied := applyEntityOverrideCascade(t, srv, token, "albums", albumID, "alb-okc")
	if applied.Cascade == nil || applied.Cascade.Updated != 0 {
		t.Errorf("cascade summary = %+v, want Updated:0 (both children skipped)", applied.Cascade)
	}

	if d := getEnrichedDetail(t, srv, token, paranoidID); d.Overview != "MANUAL Paranoid" {
		t.Errorf("Paranoid's own override clobbered by cascade: %q", d.Overview)
	}
	airbag := getEnrichedDetail(t, srv, token, airbagID)
	if airbag.Overview != "MY Airbag note" {
		t.Errorf("Airbag's Locked overview clobbered by cascade: %q", airbag.Overview)
	}
	if !contains(airbag.LockedFields, "overview") {
		t.Errorf("Airbag overview lock lost: %+v", airbag.LockedFields)
	}
}

// --- Artist → albums by title, RECURSE to tracks ----------------------------

// TestCascadeArtistAlbumsRecurse applies an Artist Fix-info override with cascade:
// the artist's albums map by title(+year) and each matched album RECURSES into its
// tracks positionally. Radiohead's OK Computer (2 tracks) + Lossless Single (1 track)
// are all corrected — updated = 2 albums + 3 tracks = 5, attention = 0.
func TestCascadeArtistAlbumsRecurse(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, query string) ([]enrich.Candidate, error) {
			switch kind {
			case "artist":
				return []enrich.Candidate{{ExternalID: "art-right", Title: "Radiohead", Kind: kind}}, nil
			case "album":
				switch {
				case strings.Contains(query, "OK Computer"):
					return []enrich.Candidate{{ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
						Tracklist: []enrich.TrackCandidate{
							{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"},
							{Disc: 1, Position: 2, Title: "Paranoid Android", ExternalID: "rec-para"},
						}}}, nil
				case strings.Contains(query, "Lossless Single"):
					return []enrich.Candidate{{ExternalID: "alb-loss", Title: "Lossless Single", Kind: kind,
						Tracklist: []enrich.TrackCandidate{
							{Disc: 1, Position: 1, Title: "No Surprises", ExternalID: "rec-nosurprises"},
						}}}, nil
				}
				return nil, nil
			}
			return nil, nil
		},
		fn: musicCascadeLookup(),
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	artists := listArtists(t, srv, token, libID)
	artistID := findArtist(t, artists, "Radiohead")
	_, airbagID, paranoidID := okComputerAlbum(t, srv, token, libID)

	// (AC) Apply the Artist override with cascade → albums-by-title, recurse to tracks.
	applied := applyEntityOverrideCascade(t, srv, token, "artists", artistID, "art-right")
	if applied.Cascade == nil {
		t.Fatalf("no cascade summary in artist apply response")
	}
	if applied.Cascade.Updated != 5 || applied.Cascade.Attention != 0 {
		t.Errorf("artist cascade summary = %+v, want {Updated:5, Attention:0}", *applied.Cascade)
	}

	// (AC) The recursion corrected each album's tracks positionally.
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != "CASCADE Airbag" {
		t.Errorf("Airbag not corrected via artist recursion: %q", d.Overview)
	}
	if d := getEnrichedDetail(t, srv, token, paranoidID); d.Overview != "CASCADE Paranoid" {
		t.Errorf("Paranoid not corrected via artist recursion: %q", d.Overview)
	}

	// (AC) Durable across a full pass: a track still resolves by its pinned recording.
	enrichLib(t, srv, token, libID, "full")
	if d := getEnrichedDetail(t, srv, token, paranoidID); d.Overview != "CASCADE Paranoid" {
		t.Errorf("Paranoid reverted on full re-enrich: %q", d.Overview)
	}
}

// --- Show → episodes positional, via the Wrong-item trigger -----------------

// TestCascadeShowEpisodesViaWrongItem drives the cascade on the SECOND trigger — a
// Show Wrong-item identity correction — with cascade on. Episodes map positionally
// (season+episode) under the corrected show: Season 01 episodes are updated durably;
// the Season 00 Special the corrected show has no record for lands in attention.
func TestCascadeShowEpisodesViaWrongItem(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "555", Title: "The Correct Work", Year: 2018, Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "show":
				m := enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "auto"}
				if ref.TMDBID == "555" {
					m.Overview, m.ExternalID = "CORRECTED show overview.", "555"
				}
				return m, nil
			case "episode":
				// The corrected show (555) has records only for its Season 01 episodes;
				// the Season 00 Special has no counterpart → unmatched → attention.
				if ref.TMDBID == "555" && ref.SeasonNumber == 1 {
					return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "CORRECTED episode."}, nil
				}
				if ref.TMDBID == "555" {
					return enrich.TitleMetadata{}, enrich.ErrNoMatch
				}
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "auto episode."}, nil
			default:
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "auto"}, nil
			}
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	shows := listShows(t, srv, token, libID)
	showID := findShow(t, shows, "The Bear")

	// Locate Season 01 episode ids + the Season 00 special id.
	var s1EpIDs []string
	var specialID string
	for _, s := range showSeasons(t, srv, token, showID).Seasons {
		for _, e := range seasonEpisodes(t, srv, token, s.ID).Episodes {
			if s.SeasonNumber == 1 {
				s1EpIDs = append(s1EpIDs, e.ID)
			} else if s.SeasonNumber == 0 {
				specialID = e.ID
			}
		}
	}
	if len(s1EpIDs) != 2 || specialID == "" {
		t.Fatalf("The Bear fixture unexpected: s1=%d special=%q", len(s1EpIDs), specialID)
	}

	// (AC) Wrong-item apply WITH cascade → the second trigger runs the cascade.
	var applied cascadeDetailResp
	status, body := srv.JSON(http.MethodPut, "/api/v1/shows/"+showID+"/identityCorrection", token,
		map[string]any{"externalId": "555", "title": "The Correct Work", "year": 2018, "cascade": true}, &applied)
	if status != http.StatusOK {
		t.Fatalf("show wrong-item (cascade) = %d, want 200; body: %s", status, body)
	}
	if applied.Cascade == nil || applied.Cascade.Updated != 2 || applied.Cascade.Attention != 1 {
		t.Errorf("show cascade summary = %+v, want {Updated:2, Attention:1}", applied.Cascade)
	}

	// (AC) Season 01 episodes corrected durably; the special is in the attention list.
	for _, id := range s1EpIDs {
		if d := getEnrichedDetail(t, srv, token, id); d.Overview != "CORRECTED episode." {
			t.Errorf("episode %s not corrected by cascade: %q", id, d.Overview)
		}
	}
	if sp, ok := attentionHas(listEnrichmentAttention(t, srv, token, libID), "Special"); !ok || sp.ID != specialID {
		t.Errorf("Season 00 special (no counterpart) not routed to attention")
	}

	// (AC) Durable: a full pass resolves the episodes by their pinned show id.
	enrichLib(t, srv, token, libID, "full")
	if d := getEnrichedDetail(t, srv, token, s1EpIDs[0]); d.Overview != "CORRECTED episode." {
		t.Errorf("episode override reverted on full re-enrich: %q", d.Overview)
	}
	prov.mu.Lock()
	sawPinned := false
	for _, ref := range prov.refs {
		if ref.Kind == "episode" && ref.TMDBID == "555" && ref.SeasonNumber == 1 {
			sawPinned = true
		}
	}
	prov.mu.Unlock()
	if !sawPinned {
		t.Errorf("full pass never resolved an episode by the pinned show id")
	}
}

// --- A record nobody chose is not an override -------------------------------

// TestCascadeReachesATrackWithAnUnchosenRecord is the defect
// .scratch/enrichment-override-durability/issues/03 describes, end to end.
//
// The skip rule used to ask "does this child have a record id?" when the question
// it meant was "did the ADMIN choose this child's record?". A Track answers yes to
// the first after nothing more than an ordinary pass: MusicBrainz answers a
// by-name recording lookup with the recording's own id, and the pass persists it
// so the Edit-item image tab has an anchor to fetch candidates by. Every
// auto-matched Track in the library was therefore excluded from its Album's "apply
// to children" — silently, since a skipped child is reported nowhere.
//
// Here both tracks carry such an id and neither was ever chosen, so both take the
// Album's correction. Against the old rule the summary is {Updated:0} and both
// overviews stay the auto record's.
func TestCascadeReachesATrackWithAnUnchosenRecord(t *testing.T) {
	requireMusicFixtures(t)
	base := musicCascadeLookup()
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
				Tracklist: []enrich.TrackCandidate{
					{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"},
					{Disc: 1, Position: 2, Title: "Paranoid Android", ExternalID: "rec-para"},
				},
			}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			m, err := base(ref)
			// The echo: a by-name recording lookup comes back WITH the recording id
			// it landed on, and WriteTitleEnrichment stores it (fill-only) as the
			// artwork anchor. Nobody chose it, so enrichment_id_origin stays empty.
			if err == nil && ref.Kind == "track" && ref.MusicbrainzID == "" {
				m.ExternalID = "rec-auto"
			}
			return m, err
		},
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	albumID, airbagID, paranoidID := okComputerAlbum(t, srv, token, libID)

	// The precondition, asserted rather than assumed: the echoed id really did
	// land on the tracks, so a later pass looks them up BY it. This is the state
	// the old rule read as "the Admin picked this".
	enrichLib(t, srv, token, libID, "full")
	prov.mu.Lock()
	sawEcho := false
	for _, ref := range prov.refs {
		if ref.Kind == "track" && ref.MusicbrainzID == "rec-auto" {
			sawEcho = true
		}
	}
	prov.mu.Unlock()
	if !sawEcho {
		t.Fatalf("no track carries the auto-resolved recording id; this test needs that state")
	}

	// (AC) Both children take the Album's correction: an id nobody chose is not an
	// override, whatever column it sits in.
	applied := applyEntityOverrideCascade(t, srv, token, "albums", albumID, "alb-okc")
	if applied.Cascade == nil || applied.Cascade.Updated != 2 || applied.Cascade.Attention != 0 {
		t.Errorf("cascade summary = %+v, want {Updated:2, Attention:0} — an auto-resolved "+
			"recording id must not exclude a Track from its Album's correction", applied.Cascade)
	}
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != "CASCADE Airbag" {
		t.Errorf("Airbag overview = %q, want the cascaded recording's", d.Overview)
	}
	if d := getEnrichedDetail(t, srv, token, paranoidID); d.Overview != "CASCADE Paranoid" {
		t.Errorf("Paranoid overview = %q, want the cascaded recording's", d.Overview)
	}
}

// TestCascadeReachesAnEpisodeWithAnUnchosenRecord is the same defect on the TV
// side, and on the FIRST trigger (a Show Fix-info override rather than a
// Wrong-item re-key).
//
// An Episode picks up a record id it never chose several ways — a provider that
// echoes one back on an ordinary lookup (which is exactly why ADR-0045 made the
// enrichment columns fill-only), a split's co-File sibling inheriting the
// survivor's series, and clearing an Episode pin, which writes the Show's own
// series back onto the Slot. None of them is the Admin saying "decorate this
// episode from that record", and none of them may cost the Episode its Show's
// correction.
func TestCascadeReachesAnEpisodeWithAnUnchosenRecord(t *testing.T) {
	requireTVFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{ExternalID: "555", Title: "The Correct Work", Year: 2018, Kind: kind}}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "show":
				m := enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "auto"}
				if ref.TMDBID == "555" {
					m.Overview, m.ExternalID = "CORRECTED show overview.", "555"
				}
				return m, nil
			case "episode":
				if ref.TMDBID == "555" && ref.SeasonNumber == 1 {
					return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "CORRECTED episode."}, nil
				}
				if ref.TMDBID == "555" {
					return enrich.TitleMetadata{}, enrich.ErrNoMatch // no Season 00 record
				}
				if ref.TMDBID == "999" {
					// The record an Admin picked for ONE episode by hand.
					return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "HAND-PICKED episode."}, nil
				}
				// The echo: the ordinary pass answers with a record id of its own,
				// which the store keeps as the Episode's anchor with no lock on it.
				return enrich.TitleMetadata{
					Matched: true, Source: "tmdb", Overview: "auto episode.", ExternalID: "ep-echo",
				}, nil
			default:
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "auto"}, nil
			}
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	showID := findShow(t, listShows(t, srv, token, libID), "The Bear")
	var s1EpIDs []string
	var specialID string
	for _, s := range showSeasons(t, srv, token, showID).Seasons {
		for _, e := range seasonEpisodes(t, srv, token, s.ID).Episodes {
			if s.SeasonNumber == 1 {
				s1EpIDs = append(s1EpIDs, e.ID)
			} else if s.SeasonNumber == 0 {
				specialID = e.ID
			}
		}
	}
	if len(s1EpIDs) != 2 || specialID == "" {
		t.Fatalf("The Bear fixture unexpected: s1=%d special=%q", len(s1EpIDs), specialID)
	}

	// The precondition: every Episode now carries the echoed record id.
	for _, id := range s1EpIDs {
		if d := getEnrichedDetail(t, srv, token, id); d.TMDBID != "ep-echo" {
			t.Fatalf("episode %s tmdbId = %q, want the echoed %q; this test needs that state",
				id, d.TMDBID, "ep-echo")
		}
	}

	// One of the two gets a record the Admin really did pick, so the same cascade
	// has to tell the two apart rather than treating both the same way — which is
	// the whole point, and what makes a green run mean something.
	if d := applyOverride(t, srv, token, s1EpIDs[0], "999"); d.Overview != "HAND-PICKED episode." {
		t.Fatalf("seeding the per-Episode override failed: overview=%q", d.Overview)
	}

	// (AC) Fix info on the Show with "apply to children" reaches the Episode whose
	// id nobody chose, and leaves the hand-picked one alone.
	applied := applyEntityOverrideCascade(t, srv, token, "shows", showID, "555")
	if applied.Cascade == nil || applied.Cascade.Updated != 1 || applied.Cascade.Attention != 1 {
		t.Errorf("show cascade summary = %+v, want {Updated:1, Attention:1} — an echoed "+
			"record id must not exclude an Episode from its Show's correction, and the "+
			"hand-picked Episode must not be clobbered", applied.Cascade)
	}
	if d := getEnrichedDetail(t, srv, token, s1EpIDs[1]); d.Overview != "CORRECTED episode." {
		t.Errorf("the Episode with only an echoed id was not corrected by the cascade: %q", d.Overview)
	}
	if d := getEnrichedDetail(t, srv, token, s1EpIDs[0]); d.Overview != "HAND-PICKED episode." {
		t.Errorf("the hand-picked Episode was clobbered by the cascade: %q", d.Overview)
	}
	if sp, ok := attentionHas(listEnrichmentAttention(t, srv, token, libID), "Special"); !ok || sp.ID != specialID {
		t.Errorf("Season 00 special (no counterpart) not routed to attention")
	}
}

// --- Cascade is opt-in; a leaf ignores it; Fix-label never cascades ---------

// TestCascadeOptInAndLeafIgnored: an album override WITHOUT cascade leaves its tracks
// untouched; a childless leaf (Track) silently ignores the cascade flag (no summary);
// a Fix-label hand-edit on the album never touches its tracks.
func TestCascadeOptInAndLeafIgnored(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
				Tracklist: []enrich.TrackCandidate{
					{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"},
					{Disc: 1, Position: 2, Title: "Paranoid Android", ExternalID: "rec-para"},
				},
			}}, nil
		},
		fn: musicCascadeLookup(),
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	albumID, airbagID, _ := okComputerAlbum(t, srv, token, libID)
	before := getEnrichedDetail(t, srv, token, airbagID).Overview

	// (AC) Unchecked cascade leaves children untouched (reuse the no-cascade apply).
	applyEntityOverride(t, srv, token, "albums", albumID, "alb-okc")
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != before {
		t.Errorf("album apply WITHOUT cascade changed a track overview: %q -> %q", before, d.Overview)
	}

	// (AC) A childless leaf (Track) accepts the flag but never cascades — no summary,
	// still a valid 200 apply.
	var leaf map[string]any
	if st, body := srv.JSON(http.MethodPut, "/api/v1/titles/"+airbagID+"/enrichmentOverride", token,
		map[string]any{"externalId": "rec-airbag", "cascade": true}, &leaf); st != http.StatusOK {
		t.Fatalf("leaf override with cascade flag = %d, want 200; body: %s", st, body)
	}
	if _, ok := leaf["cascade"]; ok {
		t.Errorf("leaf apply returned a cascade summary; a childless leaf must never cascade")
	}

	// (AC) Fix-label: a hand-edit on the album (metadata endpoint has no cascade field)
	// never touches its tracks.
	trackBefore := getEnrichedDetail(t, srv, token, airbagID).Overview
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/albums/"+albumID+"/metadata", token,
		map[string]any{"overview": "hand album note"}, nil); st != http.StatusOK {
		t.Fatalf("album metadata edit failed")
	}
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != trackBefore {
		t.Errorf("Fix-label album edit cascaded to a track: %q -> %q", trackBefore, d.Overview)
	}
}

// --- Access + SSE -----------------------------------------------------------

// TestCascadeAdminOnlyAndSSE: a Member cannot run a cascade apply (403); an Admin
// cascade emits a libraryUpdated SSE nudge so browse reflects the corrections live.
func TestCascadeAdminOnlyAndSSE(t *testing.T) {
	requireMusicFixtures(t)
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{{
				ExternalID: "alb-okc", Title: "OK Computer", Year: 1997, Kind: kind,
				Tracklist: []enrich.TrackCandidate{{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag"}},
			}}, nil
		},
		fn: musicCascadeLookup(),
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	albumID, _, _ := okComputerAlbum(t, srv, token, libID)

	srv.CreateMember("cm", "memberpass123")
	mTok := srv.LoginAs("cm", "memberpass123")
	if st, _ := srv.JSON(http.MethodPut, "/api/v1/albums/"+albumID+"/enrichmentOverride", mTok,
		map[string]any{"externalId": "alb-okc", "cascade": true}, nil); st != http.StatusForbidden {
		t.Errorf("member cascade apply = %d, want 403", st)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := openEventStream(t, ctx, srv, token)
	applyEntityOverrideCascade(t, srv, token, "albums", albumID, "alb-okc")
	waitForLine(t, lines, func(s string) bool {
		return strings.Contains(s, "event: libraryUpdated") || strings.Contains(s, `"libraryId":"`+libID+`"`)
	})
}

// musicCascadeLookup is the shared fake Lookup for the music cascade tests: an
// artist/album resolves corrected BY its pinned id (else an auto record), and a track
// (recording) resolves BY its pinned recording MBID to a distinct overview so a
// cascaded/skipped/durable outcome is observable. A by-name (auto) lookup returns a
// deliberately generic record so a revert would be visible.
func musicCascadeLookup() func(enrich.TitleRef) (enrich.TitleMetadata, error) {
	return func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
		switch ref.Kind {
		case "artist":
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Overview: "auto artist", ExternalID: "art-auto"}
			if ref.MusicbrainzID == "art-right" {
				m.Overview, m.ExternalID = "CORRECTED artist bio.", "art-right"
			}
			return m, nil
		case "album":
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Genres: []string{"Rock"}, ExternalID: "alb-auto"}
			if ref.MusicbrainzID != "" {
				m.ExternalID = ref.MusicbrainzID
				m.Genres = []string{"Alt Rock"}
			}
			return m, nil
		case "track":
			m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Overview: "auto track"}
			switch ref.MusicbrainzID {
			case "rec-airbag":
				m.Overview = "CASCADE Airbag"
			case "rec-para":
				m.Overview = "CASCADE Paranoid"
			case "rec-nosurprises":
				m.Overview = "CASCADE NoSurprises"
			case "rec-para-manual":
				m.Overview = "MANUAL Paranoid"
			}
			return m, nil
		default:
			return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Overview: "x", ExternalID: "auto"}, nil
		}
	}
}

// --- Re-running a Cascade from the same parent ------------------------------

// A Cascade is repeatable, and the two tests below are the ones issue 04 was
// filed for (.scratch/enrichment-override-durability/issues/04, ADR-0046).
//
// The Cascade writes every child through the same store call an Admin's own Fix
// info uses, so the children of a first run came back carrying "the Admin chose
// this record" — and the skip rule, correctly refusing to clobber a child's own
// correction, then refused to touch them. The Admin re-points the parent, it
// works; they correct the parent again later and the children silently keep the
// PREVIOUS correction. No error, no count, just Updated: 0 on exactly the
// children that matter.
//
// The fix is to record WHOSE choice a record is rather than only that it is one:
// a cascaded record is the parent's, held by the child, so it follows the parent.
// Both tests therefore assert the same two things at their own grain — the second
// run re-applies to the children the first one reached, AND a child the Admin
// corrected DIRECTLY still beats the Cascade, which is the protection issue 03
// added and this must not spend.

// TestCascadeRerunsOnTheChildrenItAlreadyReached: a Show re-pointed with "apply to
// children" twice corrects its Episodes both times.
//
// Against the old rule the second summary is {Updated:0} and every Episode is left
// showing the FIRST correction — the failure is silent, which is what makes it
// worth a test rather than a comment.
func TestCascadeRerunsOnTheChildrenItAlreadyReached(t *testing.T) {
	requireTVFixtures(t)
	// Two corrected series, applied one after the other to the same Show. Each
	// answers Season 1 and has no record for the Season 00 Special, so the shape of
	// both runs is identical and only the RECORD differs.
	episodeOverview := map[string]string{"555": "FIRST episode.", "777": "SECOND episode."}
	prov := &fakeProvider{
		searchFn: func(kind, _ string) ([]enrich.Candidate, error) {
			return []enrich.Candidate{
				{ExternalID: "555", Title: "First Correction", Year: 2018, Kind: kind},
				{ExternalID: "777", Title: "Second Correction", Year: 2019, Kind: kind},
			}, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "show":
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: ref.TMDBID}, nil
			case "episode":
				if ref.TMDBID == "999" {
					// The record an Admin picks for ONE Episode by hand, later on.
					return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "HAND-PICKED episode."}, nil
				}
				ov, corrected := episodeOverview[ref.TMDBID]
				if !corrected {
					return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: "auto episode."}, nil
				}
				if ref.SeasonNumber != 1 {
					return enrich.TitleMetadata{}, enrich.ErrNoMatch // no Season 00 record
				}
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", Overview: ov}, nil
			default:
				return enrich.TitleMetadata{Matched: true, Source: "tmdb", ExternalID: "auto"}, nil
			}
		},
	}
	srv := testharness.New(t,
		testharness.WithEnrichmentKey("test-key"),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createTVLibrary(t, srv, token, tvRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	showID := findShow(t, listShows(t, srv, token, libID), "The Bear")
	var s1EpIDs []string
	for _, s := range showSeasons(t, srv, token, showID).Seasons {
		if s.SeasonNumber != 1 {
			continue
		}
		for _, e := range seasonEpisodes(t, srv, token, s.ID).Episodes {
			s1EpIDs = append(s1EpIDs, e.ID)
		}
	}
	if len(s1EpIDs) != 2 {
		t.Fatalf("The Bear fixture unexpected: %d Season 1 episodes", len(s1EpIDs))
	}

	// Run 1: both Season 1 Episodes take the correction; the Special has no
	// counterpart and goes to the attention list.
	first := applyEntityOverrideCascade(t, srv, token, "shows", showID, "555")
	if first.Cascade == nil || first.Cascade.Updated != 2 || first.Cascade.Attention != 1 {
		t.Fatalf("first cascade summary = %+v, want {Updated:2, Attention:1}", first.Cascade)
	}
	for _, id := range s1EpIDs {
		if d := getEnrichedDetail(t, srv, token, id); d.Overview != "FIRST episode." {
			t.Fatalf("episode %s not corrected by the first cascade: %q", id, d.Overview)
		}
	}

	// (AC) Run 2 from the SAME Show reaches the SAME children. The record they are
	// carrying is this Show's own previous decision, so the Show revising it wins.
	second := applyEntityOverrideCascade(t, srv, token, "shows", showID, "777")
	if second.Cascade == nil || second.Cascade.Updated != 2 || second.Cascade.Attention != 1 {
		t.Errorf("second cascade summary = %+v, want {Updated:2, Attention:1} — a record "+
			"the FIRST cascade wrote is the Show's choice, not the Episode's, so re-pointing "+
			"the Show must correct its Episodes again", second.Cascade)
	}
	for _, id := range s1EpIDs {
		if d := getEnrichedDetail(t, srv, token, id); d.Overview != "SECOND episode." {
			t.Errorf("episode %s kept the FIRST correction after a second cascade: %q", id, d.Overview)
		}
	}

	// (AC) The protection issue 03 added is intact: an Episode the Admin corrects
	// DIRECTLY is the Episode's own choice and outranks the Show's next cascade.
	if d := applyOverride(t, srv, token, s1EpIDs[0], "999"); d.Overview != "HAND-PICKED episode." {
		t.Fatalf("seeding the per-Episode override failed: overview=%q", d.Overview)
	}
	third := applyEntityOverrideCascade(t, srv, token, "shows", showID, "777")
	if third.Cascade == nil || third.Cascade.Updated != 1 || third.Cascade.Attention != 1 {
		t.Errorf("third cascade summary = %+v, want {Updated:1, Attention:1} — the "+
			"hand-corrected Episode must be skipped, the cascaded one re-applied", third.Cascade)
	}
	if d := getEnrichedDetail(t, srv, token, s1EpIDs[0]); d.Overview != "HAND-PICKED episode." {
		t.Errorf("the hand-picked Episode was clobbered by a later cascade: %q", d.Overview)
	}
}

// TestCascadeRerunsFromTheSameArtist is the same defect one level up, where it bit
// harder. An Artist cascade pins each mapped ALBUM through the same store call an
// Album's own Fix info uses, so the second run read its own previous pins as the
// Albums' corrections and skipped them — and a skipped Album is never recursed
// into, so its Tracks were skipped too. Fixing only the leaves would have left this
// whole branch broken.
func TestCascadeRerunsFromTheSameArtist(t *testing.T) {
	requireMusicFixtures(t)
	// The provider's answers change between the two runs, so "the second run really
	// re-applied" is observable rather than inferred from a count.
	var round atomic.Int32
	round.Store(1)
	suffix := func() string {
		if round.Load() == 1 {
			return ""
		}
		return "-2"
	}
	prov := &fakeProvider{
		searchFn: func(kind, query string) ([]enrich.Candidate, error) {
			sfx := suffix()
			switch kind {
			case "artist":
				return []enrich.Candidate{{ExternalID: "art-right", Title: "Radiohead", Kind: kind}}, nil
			case "album":
				switch {
				case strings.Contains(query, "OK Computer"):
					return []enrich.Candidate{{ExternalID: "alb-okc" + sfx, Title: "OK Computer", Year: 1997, Kind: kind,
						Tracklist: []enrich.TrackCandidate{
							{Disc: 1, Position: 1, Title: "Airbag", ExternalID: "rec-airbag" + sfx},
							{Disc: 1, Position: 2, Title: "Paranoid Android", ExternalID: "rec-para" + sfx},
						}}}, nil
				case strings.Contains(query, "Lossless Single"):
					return []enrich.Candidate{{ExternalID: "alb-loss" + sfx, Title: "Lossless Single", Kind: kind,
						Tracklist: []enrich.TrackCandidate{
							{Disc: 1, Position: 1, Title: "No Surprises", ExternalID: "rec-nosurprises" + sfx},
						}}}, nil
				}
				return nil, nil
			}
			return nil, nil
		},
		fn: func(ref enrich.TitleRef) (enrich.TitleMetadata, error) {
			switch ref.Kind {
			case "artist":
				return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "art-right"}, nil
			case "album":
				m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "alb-auto"}
				if ref.MusicbrainzID != "" {
					m.ExternalID = ref.MusicbrainzID
				}
				return m, nil
			case "track":
				m := enrich.TitleMetadata{Matched: true, Source: "musicbrainz", Overview: "auto track"}
				switch ref.MusicbrainzID {
				case "rec-airbag":
					m.Overview = "FIRST Airbag"
				case "rec-airbag-2":
					m.Overview = "SECOND Airbag"
				case "rec-para":
					m.Overview = "FIRST Paranoid"
				case "rec-para-2":
					m.Overview = "SECOND Paranoid"
				case "rec-nosurprises":
					m.Overview = "FIRST NoSurprises"
				case "rec-nosurprises-2":
					m.Overview = "SECOND NoSurprises"
				case "rec-loss-manual":
					m.Overview = "MANUAL Lossless"
				}
				return m, nil
			default:
				return enrich.TitleMetadata{Matched: true, Source: "musicbrainz", ExternalID: "auto"}, nil
			}
		},
	}
	srv := testharness.New(t,
		testharness.WithMusicBrainzEnabled(true),
		testharness.WithMetadataProvider(prov),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	enrichLib(t, srv, token, libID, "")

	artistID := findArtist(t, listArtists(t, srv, token, libID), "Radiohead")
	_, airbagID, paranoidID := okComputerAlbum(t, srv, token, libID)
	var losslessID, noSurprisesID string
	for _, al := range artistAlbums(t, srv, token, artistID).Albums {
		if al.Title != "Lossless Single" {
			continue
		}
		losslessID = al.ID
		for _, tr := range albumTracks(t, srv, token, al.ID).Tracks {
			noSurprisesID = tr.ID
		}
	}
	if losslessID == "" || noSurprisesID == "" {
		t.Fatalf("Lossless Single fixture not found (album=%q track=%q)", losslessID, noSurprisesID)
	}

	// Run 1: 2 albums + 3 tracks.
	first := applyEntityOverrideCascade(t, srv, token, "artists", artistID, "art-right")
	if first.Cascade == nil || first.Cascade.Updated != 5 || first.Cascade.Attention != 0 {
		t.Fatalf("first artist cascade summary = %+v, want {Updated:5, Attention:0}", first.Cascade)
	}
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != "FIRST Airbag" {
		t.Fatalf("Airbag not corrected by the first artist cascade: %q", d.Overview)
	}

	// Between the runs the Admin fixes ONE album by hand. That is the Album's own
	// choice and must survive its Artist's next cascade — the promise issue 03 made,
	// which this change may not spend.
	if st, body := srv.JSON(http.MethodPut, "/api/v1/albums/"+losslessID+"/enrichmentOverride",
		token, map[string]any{"externalId": "alb-loss-manual"}, nil); st != http.StatusOK {
		t.Fatalf("seed album override = %d; body: %s", st, body)
	}

	round.Store(2)

	// (AC) Run 2 re-applies to the Album this Artist's own last run pinned, and
	// recurses into its Tracks again: 1 album + 2 tracks. The hand-fixed Album is
	// skipped, and with it the Track it holds.
	second := applyEntityOverrideCascade(t, srv, token, "artists", artistID, "art-right")
	if second.Cascade == nil || second.Cascade.Updated != 3 || second.Cascade.Attention != 0 {
		t.Errorf("second artist cascade summary = %+v, want {Updated:3, Attention:0} — an "+
			"Album the SAME Artist cascade pinned is the Artist's choice and must take the "+
			"next one (with its Tracks); an Album the Admin fixed by hand must not",
			second.Cascade)
	}
	if d := getEnrichedDetail(t, srv, token, airbagID); d.Overview != "SECOND Airbag" {
		t.Errorf("Airbag kept the FIRST correction after a second artist cascade: %q", d.Overview)
	}
	if d := getEnrichedDetail(t, srv, token, paranoidID); d.Overview != "SECOND Paranoid" {
		t.Errorf("Paranoid kept the FIRST correction after a second artist cascade: %q", d.Overview)
	}
	// The skipped Album's Track is the proof the skip was whole: a skipped Album is
	// never recursed into, so this Track must still hold what run 1 gave it.
	if d := getEnrichedDetail(t, srv, token, noSurprisesID); d.Overview != "FIRST NoSurprises" {
		t.Errorf("the hand-fixed Album's Track = %q, want %q — an Album the Admin fixed "+
			"must keep its Artist's cascade out, tracks included", d.Overview, "FIRST NoSurprises")
	}
}
