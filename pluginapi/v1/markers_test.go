package v1

import "testing"

// markerWireCases pins the documents of the Marker provider Extension point: an
// Episode's request and a Movie's, an answer, a miss, and the envelope an
// Installed plugin is handed.
func markerWireCases() []wireCase {
	return []wireCase{
		{
			name: "MarkersRequest episode",
			value: MarkersRequest{
				Kind:          "episode",
				Title:         "Pilot",
				IDs:           map[string]string{"imdb": "tt0959621"},
				ShowTitle:     "Breaking Bad",
				ShowIDs:       map[string]string{"tmdb": "1396"},
				SeasonNumber:  1,
				EpisodeNumber: 1,
				DurationMs:    3487000,
			},
			golden: `{"kind":"episode","title":"Pilot","ids":{"imdb":"tt0959621"},"showTitle":"Breaking Bad",` +
				`"showIds":{"tmdb":"1396"},"seasonNumber":1,"episodeNumber":1,"durationMs":3487000}`,
		},
		{
			name:   "MarkersRequest movie",
			value:  MarkersRequest{Kind: "movie", Title: "Dune", Year: 2021, DurationMs: 9360000},
			golden: `{"kind":"movie","title":"Dune","year":2021,"durationMs":9360000}`,
		},
		{
			name: "MarkersResponse",
			value: MarkersResponse{Markers: []MarkerCandidate{
				{Kind: MarkerIntro, StartMs: 60000, EndMs: 120000, DurationMs: 3487500},
				{Kind: MarkerCredits, StartMs: 3400000, EndMs: 3487000, DurationMs: 3487500},
			}},
			golden: `{"markers":[{"kind":"intro","startMs":60000,"endMs":120000,"durationMs":3487500},` +
				`{"kind":"credits","startMs":3400000,"endMs":3487000,"durationMs":3487500}]}`,
		},
		{
			name:   "MarkersResponse miss",
			value:  MarkersResponse{},
			golden: `{}`,
		},
		{
			name:  "MarkersCall",
			value: MarkersCall{Request: MarkersRequest{Kind: "movie", Title: "B", DurationMs: 1}, Settings: Settings{Enabled: true}},
			golden: `{"request":{"kind":"movie","title":"B","durationMs":1},` +
				`"settings":{"enabled":true}}`,
		},
	}
}

// TestRegistryHoldsMarkerProviders: a Marker provider registers into the same
// Registry value as every other seam, reads back in registration order under its
// own slug namespace, and a malformed registration panics at the composition
// root.
func TestRegistryHoldsMarkerProviders(t *testing.T) {
	stub := func(Settings) (MarkerProvider, error) { return nil, nil }
	reg := NewRegistry()
	reg.RegisterMarkerProvider(MarkerProviderRegistration{
		Descriptor: Descriptor{Slug: "introdb", Name: "IntroDB"}, New: stub,
	})
	reg.RegisterMarkerProvider(MarkerProviderRegistration{
		Descriptor: Descriptor{Slug: "other-markers"}, New: stub,
	})

	got := reg.MarkerProviders()
	if len(got) != 2 || got[0].Descriptor.Slug != "introdb" || got[1].Descriptor.Slug != "other-markers" {
		t.Fatalf("MarkerProviders() = %+v, want introdb then other-markers", got)
	}
	if ep := got[0].Descriptor.ExtensionPoint; ep != ExtensionMarkerProvider {
		t.Fatalf("extension point = %q, want %q", ep, ExtensionMarkerProvider)
	}
	got[0].Descriptor.Slug = "mutated"
	if _, ok := reg.MarkerProvider("introdb"); !ok {
		t.Fatal("MarkerProviders() handed out the registry's own backing array")
	}
	if _, ok := reg.LyricProvider("introdb"); ok {
		t.Fatal("LyricProvider(introdb) found a marker provider")
	}
	var nilReg *Registry
	if nilReg.MarkerProviders() != nil {
		t.Fatal("a nil registry listed marker providers")
	}

	for _, tc := range []struct {
		name string
		reg  MarkerProviderRegistration
	}{
		{"duplicate slug", MarkerProviderRegistration{Descriptor: Descriptor{Slug: "other-markers"}, New: stub}},
		{"no slug", MarkerProviderRegistration{New: stub}},
		{"no factory", MarkerProviderRegistration{Descriptor: Descriptor{Slug: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			reg.RegisterMarkerProvider(tc.reg)
		})
	}
}
