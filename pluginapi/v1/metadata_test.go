package v1

import "testing"

// The Metadata provider half of the contract's own suite. Every wire type it adds
// is listed here, fully populated beside the exact JSON it must produce, and
// wireCases() folds them into the same round-trip and zero-value loops the
// Subtitle provider types go through — so a field rename in either half is a
// deliberate, visible act while the package can still change freely.

func metadataWireCases() []wireCase {
	return []wireCase{
		{
			name: "MediaRef",
			value: MediaRef{
				Kind:  "episode",
				Title: "Breaking Bad",
				Year:  2008,
				ExternalIDs: map[string]string{
					"tmdb": "1396", "imdb": "tt0903747", "musicbrainz": "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d",
					"thetvdb": "81189", "anidb": "1", "anilist": "4242",
				},
				SeasonNumber:  2,
				EpisodeNumber: 5,
				EpisodeLabel:  "2x05",
				Artist:        "Eagles",
				Album:         "Hotel California",
				Track:         "Wasted Time",
				ReleaseMBID:   "9c9f1380-2516-4fc9-a3e6-f9f61941d090",
				AlbumHints: []AlbumHint{
					{Title: "Hotel California", ReleaseGroupMBID: "f2b67b28-6b1b-4c56-b1cd-1a0b4b1c2b21"},
				},
			},
			golden: `{"kind":"episode","title":"Breaking Bad","year":2008,` +
				`"externalIds":{"anidb":"1","anilist":"4242","imdb":"tt0903747",` +
				`"musicbrainz":"b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d","thetvdb":"81189","tmdb":"1396"},` +
				`"seasonNumber":2,"episodeNumber":5,` +
				`"episodeLabel":"2x05","artist":"Eagles","album":"Hotel California",` +
				`"track":"Wasted Time","releaseMbid":"9c9f1380-2516-4fc9-a3e6-f9f61941d090",` +
				`"albumHints":[{"title":"Hotel California","releaseGroupMbid":"f2b67b28-6b1b-4c56-b1cd-1a0b4b1c2b21"}]}`,
		},
		{
			name:   "AlbumHint",
			value:  AlbumHint{Title: "OK Computer", ReleaseGroupMBID: "b1392450-e666-3926-a536-22c65f834433"},
			golden: `{"title":"OK Computer","releaseGroupMbid":"b1392450-e666-3926-a536-22c65f834433"}`,
		},
		{
			name:   "ArtworkRef",
			value:  ArtworkRef{Role: "poster", URL: "https://images.example.test/p.jpg"},
			golden: `{"role":"poster","url":"https://images.example.test/p.jpg"}`,
		},
		{
			name: "Credit",
			value: Credit{
				Person:    "Bryan Cranston",
				Role:      "Actor",
				Character: "Walter White",
				Kind:      "cast",
				PersonRef: "tmdb:17419",
				ImageURL:  "https://images.example.test/bc.jpg",
			},
			golden: `{"person":"Bryan Cranston","role":"Actor","character":"Walter White",` +
				`"kind":"cast","personRef":"tmdb:17419","imageUrl":"https://images.example.test/bc.jpg"}`,
		},
		{
			name: "MetadataRecord",
			value: MetadataRecord{
				Matched:        true,
				Name:           "Dune",
				Year:           2021,
				Overview:       "A noble family.",
				Tagline:        "Beyond fear, destiny awaits.",
				ContentRating:  "PG-13",
				ReleaseDate:    "2021-10-22",
				RuntimeMinutes: 155,
				Studio:         "Legendary Pictures",
				Genres:         []string{"Science Fiction", "Adventure"},
				Cast:           []Credit{{Person: "Timothée Chalamet", Kind: "cast"}},
				Artwork:        []ArtworkRef{{Role: "poster", URL: "https://images.example.test/p.jpg"}},
				ExternalID:     "438631",
				Source:         "tmdb",
				FromSearch:     true,
			},
			golden: `{"matched":true,"name":"Dune","year":2021,"overview":"A noble family.",` +
				`"tagline":"Beyond fear, destiny awaits.","contentRating":"PG-13",` +
				`"releaseDate":"2021-10-22","runtimeMinutes":155,"studio":"Legendary Pictures",` +
				`"genres":["Science Fiction","Adventure"],` +
				`"cast":[{"person":"Timothée Chalamet","kind":"cast"}],` +
				`"artwork":[{"role":"poster","url":"https://images.example.test/p.jpg"}],` +
				`"externalId":"438631","source":"tmdb","fromSearch":true}`,
		},
		{
			name:   "LookupRequest",
			value:  LookupRequest{Ref: MediaRef{Kind: "movie", Title: "Dune", Year: 2021}},
			golden: `{"ref":{"kind":"movie","title":"Dune","year":2021}}`,
		},
		{
			name: "LookupResponse",
			value: LookupResponse{
				Outcome: OutcomeMatched,
				Record:  MetadataRecord{Matched: true, Name: "Dune", ExternalID: "438631", Source: "tmdb"},
				Detail:  "resolved by id",
			},
			golden: `{"outcome":"matched","record":{"matched":true,"name":"Dune",` +
				`"externalId":"438631","source":"tmdb"},"detail":"resolved by id"}`,
		},
		{
			name: "SearchRequest",
			value: SearchRequest{
				Kind:    "track",
				Query:   "Wasted Time",
				Artist:  "Eagles",
				Release: "Hotel California",
				Page:    Page{Limit: 25, Offset: 25},
			},
			// Page is embedded, so limit/offset are flat on the wire.
			golden: `{"kind":"track","query":"Wasted Time","artist":"Eagles",` +
				`"release":"Hotel California","limit":25,"offset":25}`,
		},
		{
			name: "SearchCandidate",
			value: SearchCandidate{
				ExternalID:     "438631",
				Title:          "Dune",
				Year:           2021,
				ThumbnailURL:   "https://images.example.test/t.jpg",
				Disambiguation: "Part one of a two-part adaptation.",
				Kind:           "movie",
				TypeLabel:      "Album · Soundtrack",
				Tracklist:      []TrackCandidate{{Disc: 1, Position: 3, Title: "Wasted Time"}},
				ReleaseID:      "9c9f1380-2516-4fc9-a3e6-f9f61941d090",
			},
			golden: `{"externalId":"438631","title":"Dune","year":2021,` +
				`"thumbnailUrl":"https://images.example.test/t.jpg",` +
				`"disambiguation":"Part one of a two-part adaptation.","kind":"movie",` +
				`"typeLabel":"Album · Soundtrack",` +
				`"tracklist":[{"disc":1,"position":3,"title":"Wasted Time"}],` +
				`"releaseId":"9c9f1380-2516-4fc9-a3e6-f9f61941d090"}`,
		},
		{
			name: "SearchResponse",
			value: SearchResponse{
				Outcome:    OutcomeMatched,
				Candidates: []SearchCandidate{{ExternalID: "438631", Title: "Dune", Kind: "movie"}},
				Detail:     "1 candidate",
			},
			golden: `{"outcome":"matched","candidates":[{"externalId":"438631","title":"Dune",` +
				`"kind":"movie"}],"detail":"1 candidate"}`,
		},
		{
			name: "ArtworkCandidatesRequest",
			value: ArtworkCandidatesRequest{
				Ref:  MediaRef{Kind: "movie", ExternalIDs: map[string]string{NamespaceTMDB: "438631"}},
				Role: "poster",
				Page: Page{Limit: 30},
			},
			golden: `{"ref":{"kind":"movie","externalIds":{"tmdb":"438631"}},"role":"poster","limit":30}`,
		},
		{
			name:   "ArtworkCandidate",
			value:  ArtworkCandidate{URL: "https://images.example.test/p.jpg", Width: 2000, Height: 3000, Source: "tmdb"},
			golden: `{"url":"https://images.example.test/p.jpg","width":2000,"height":3000,"source":"tmdb"}`,
		},
		{
			name: "ArtworkCandidatesResponse",
			value: ArtworkCandidatesResponse{
				Outcome:    OutcomeMatched,
				Candidates: []ArtworkCandidate{{URL: "https://images.example.test/p.jpg", Source: "tmdb"}},
				Detail:     "1 image",
			},
			golden: `{"outcome":"matched","candidates":[{"url":"https://images.example.test/p.jpg",` +
				`"source":"tmdb"}],"detail":"1 image"}`,
		},
		{
			name:   "SeriesSeasonsRequest",
			value:  SeriesSeasonsRequest{SeriesID: "1396", Page: Page{Limit: 50}},
			golden: `{"seriesId":"1396","limit":50}`,
		},
		{
			name:   "SeasonSummary",
			value:  SeasonSummary{Season: 2, EpisodeCount: 13},
			golden: `{"season":2,"episodeCount":13}`,
		},
		{
			name: "SeriesSeasonsResponse",
			value: SeriesSeasonsResponse{
				Outcome: OutcomeMatched,
				Seasons: []SeasonSummary{{Season: 1, EpisodeCount: 7}},
				Detail:  "1 season",
			},
			golden: `{"outcome":"matched","seasons":[{"season":1,"episodeCount":7}],"detail":"1 season"}`,
		},
		{
			name:   "SeasonEpisodesRequest",
			value:  SeasonEpisodesRequest{SeriesID: "1396", Season: 2, Page: Page{Offset: 10}},
			golden: `{"seriesId":"1396","season":2,"offset":10}`,
		},
		{
			name: "EpisodeCandidate",
			value: EpisodeCandidate{
				Season:   2,
				Episode:  5,
				Name:     "Breakage",
				Overview: "Hank suffers a panic attack.",
				AirDate:  "2009-04-05",
				StillURL: "https://images.example.test/s.jpg",
			},
			golden: `{"season":2,"episode":5,"name":"Breakage",` +
				`"overview":"Hank suffers a panic attack.","airDate":"2009-04-05",` +
				`"stillUrl":"https://images.example.test/s.jpg"}`,
		},
		{
			name: "SeasonEpisodesResponse",
			value: SeasonEpisodesResponse{
				Outcome:  OutcomeMatched,
				Episodes: []EpisodeCandidate{{Season: 2, Episode: 5, Name: "Breakage"}},
				Detail:   "1 episode",
			},
			golden: `{"outcome":"matched","episodes":[{"season":2,"episode":5,"name":"Breakage"}],` +
				`"detail":"1 episode"}`,
		},
		{
			name: "TrackCandidate",
			value: TrackCandidate{
				Disc:       2,
				Position:   4,
				Title:      "(I Could Only) Whisper Your Name",
				ExternalID: "f2b67b28-6b1b-4c56-b1cd-1a0b4b1c2b21",
			},
			golden: `{"disc":2,"position":4,"title":"(I Could Only) Whisper Your Name",` +
				`"externalId":"f2b67b28-6b1b-4c56-b1cd-1a0b4b1c2b21"}`,
		},
		{
			name: "TracklistRequest",
			value: TracklistRequest{
				ReleaseGroupID:  "b1392450-e666-3926-a536-22c65f834433",
				ReleaseID:       "9c9f1380-2516-4fc9-a3e6-f9f61941d090",
				ReleaseIDChosen: true,
				LocalTrackCount: 12,
			},
			golden: `{"releaseGroupId":"b1392450-e666-3926-a536-22c65f834433",` +
				`"releaseId":"9c9f1380-2516-4fc9-a3e6-f9f61941d090","releaseIdChosen":true,` +
				`"localTrackCount":12}`,
		},
		{
			name: "TracklistResponse",
			value: TracklistResponse{
				Outcome: OutcomeMatched,
				Tracks:  []TrackCandidate{{Position: 1, Title: "Airbag"}},
				Detail:  "1 track",
			},
			golden: `{"outcome":"matched","tracks":[{"position":1,"title":"Airbag"}],"detail":"1 track"}`,
		},
		{
			name:   "ReleaseEditionsRequest",
			value:  ReleaseEditionsRequest{ReleaseGroupID: "b1392450-e666-3926-a536-22c65f834433", Page: Page{Limit: 100}},
			golden: `{"releaseGroupId":"b1392450-e666-3926-a536-22c65f834433","limit":100}`,
		},
		{
			name: "ReleaseEdition",
			value: ReleaseEdition{
				ReleaseID:      "9c9f1380-2516-4fc9-a3e6-f9f61941d090",
				Date:           "1997-06-16",
				Country:        "GB",
				Format:         "CD",
				TrackCount:     12,
				Disambiguation: "deluxe edition",
			},
			golden: `{"releaseId":"9c9f1380-2516-4fc9-a3e6-f9f61941d090","date":"1997-06-16",` +
				`"country":"GB","format":"CD","trackCount":12,"disambiguation":"deluxe edition"}`,
		},
		{
			name: "ReleaseEditionsResponse",
			value: ReleaseEditionsResponse{
				Outcome:  OutcomeMatched,
				Editions: []ReleaseEdition{{ReleaseID: "9c9f1380-2516-4fc9-a3e6-f9f61941d090", TrackCount: 12}},
				Detail:   "1 edition",
			},
			golden: `{"outcome":"matched","editions":[{"releaseId":"9c9f1380-2516-4fc9-a3e6-f9f61941d090",` +
				`"trackCount":12}],"detail":"1 edition"}`,
		},
		{
			name:   "ExternalRefRequest",
			value:  ExternalRefRequest{Kind: "album", Pasted: "https://musicbrainz.org/release/9c9f1380-2516-4fc9-a3e6-f9f61941d090"},
			golden: `{"kind":"album","pasted":"https://musicbrainz.org/release/9c9f1380-2516-4fc9-a3e6-f9f61941d090"}`,
		},
		{
			name: "ExternalRefResponse",
			value: ExternalRefResponse{
				Outcome:    OutcomeRefKindMismatch,
				ExternalID: "b1392450-e666-3926-a536-22c65f834433",
				ReleaseID:  "9c9f1380-2516-4fc9-a3e6-f9f61941d090",
				GotKind:    "artist",
				WantKind:   "track",
				Detail:     "that is an artist link",
			},
			golden: `{"outcome":"ref-kind-mismatch","externalId":"b1392450-e666-3926-a536-22c65f834433",` +
				`"releaseId":"9c9f1380-2516-4fc9-a3e6-f9f61941d090","gotKind":"artist",` +
				`"wantKind":"track","detail":"that is an artist link"}`,
		},
	}
}

// TestCapabilityAlbumTracklistCoversBothCalls: one declaration, two calls. The
// tracklist and the edition list are the automatic and the manual half of one
// question, so a Plugin declaring album-tracklist may be asked for either — which
// is what the host's single capability check relies on.
func TestCapabilityAlbumTracklistCoversBothCalls(t *testing.T) {
	d := Descriptor{Capabilities: []Capability{CapabilityAlbumTracklist}}
	if !d.HasCapability(CapabilityAlbumTracklist) {
		t.Fatal("a declared capability was not reported")
	}
	if d.HasCapability(CapabilityExternalRef) || d.HasCapability(CapabilityEpisodeList) {
		t.Error("declaring album-tracklist must not imply any other capability")
	}
}

// TestDescriptorServes: which coarse kinds a Plugin serves is a REGISTRATION fact,
// which is what lets the per-kind chain composition and the Authoritative-provider
// candidate list treat a Built-in and an Installed plugin identically (ADR-0057
// decision 4).
func TestDescriptorServes(t *testing.T) {
	both := Descriptor{Kinds: []string{KindVideo, KindMusic}}
	if !both.Serves(KindVideo) || !both.Serves(KindMusic) {
		t.Error("a two-kind Plugin failed to report a kind it declared")
	}
	if (Descriptor{Kinds: []string{KindMusic}}).Serves(KindVideo) {
		t.Error("a music-only Plugin claimed a video kind")
	}
	if (Descriptor{}).Serves(KindVideo) {
		t.Error("a Plugin that declared no kind serves none")
	}
}

// TestMetadataRegistryIsAValue mirrors the Subtitle provider case: two registries
// never see each other's Plugins, a nil registry reads as "no Plugins", the
// Extension point is stamped by registering, and the copy handed out is a copy.
func TestMetadataRegistryIsAValue(t *testing.T) {
	stub := func(Settings) (MetadataProvider, error) { return nil, nil }
	a := NewRegistry()
	a.RegisterMetadataProvider(MetadataProviderRegistration{
		Descriptor: Descriptor{Slug: "one", Name: "One"}, New: stub,
	})
	b := NewRegistry()

	if got := len(a.MetadataProviders()); got != 1 {
		t.Fatalf("registry a has %d metadata providers, want 1", got)
	}
	if got := len(b.MetadataProviders()); got != 0 {
		t.Fatalf("registry b saw a's registrations (%d) — the registry is not a value", got)
	}
	var nilReg *Registry
	if got := len(nilReg.MetadataProviders()); got != 0 {
		t.Fatalf("nil registry returned %d providers", got)
	}
	if _, ok := nilReg.MetadataProvider("one"); ok {
		t.Fatal("nil registry claimed to know a provider")
	}

	reg, ok := a.MetadataProvider("one")
	if !ok {
		t.Fatal("registered provider not found by slug")
	}
	if reg.Descriptor.ExtensionPoint != ExtensionMetadataProvider {
		t.Fatalf("extension point = %q, want %q", reg.Descriptor.ExtensionPoint, ExtensionMetadataProvider)
	}

	list := a.MetadataProviders()
	list[0].Descriptor.Slug = "mutated"
	if again, _ := a.MetadataProvider("one"); again.Descriptor.Slug != "one" {
		t.Fatal("MetadataProviders() handed out the registry's own backing array")
	}
}

// TestRegisterMetadataProviderSlugRules: a duplicate or missing slug is a
// composition-root programming error and fails loudly at boot — but a nil factory
// is NOT, because a registration may exist only to put a source's static facts on
// the settings screen while the host builds it another way (Cover Art Archive is
// reached through the MusicBrainz Plugin and has no client of its own).
func TestRegisterMetadataProviderSlugRules(t *testing.T) {
	stub := func(Settings) (MetadataProvider, error) { return nil, nil }
	r := NewRegistry()
	r.RegisterMetadataProvider(MetadataProviderRegistration{
		Descriptor: Descriptor{Slug: "one"}, New: stub,
	})

	for _, tc := range []struct {
		name string
		reg  MetadataProviderRegistration
	}{
		{"duplicate slug", MetadataProviderRegistration{Descriptor: Descriptor{Slug: "one"}, New: stub}},
		{"no slug", MetadataProviderRegistration{Descriptor: Descriptor{}, New: stub}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			r.RegisterMetadataProvider(tc.reg)
		})
	}

	r.RegisterMetadataProvider(MetadataProviderRegistration{Descriptor: Descriptor{Slug: "facts-only"}})
	got, ok := r.MetadataProvider("facts-only")
	if !ok || got.New != nil {
		t.Fatal("a factory-less registration must be accepted and stay factory-less")
	}
}
