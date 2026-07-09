package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/marioquake/juicebox/internal/enrich"
	"github.com/marioquake/juicebox/internal/testharness"
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
