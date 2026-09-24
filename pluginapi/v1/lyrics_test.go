package v1

import "testing"

// lyricWireCases pins the documents of the Lyric provider Extension point: the
// request, a Synced and a Plain response, a miss, and the envelope an Installed
// plugin is handed.
func lyricWireCases() []wireCase {
	req := LyricsRequest{
		Artist:      "Lyric Band",
		Title:       "Words",
		Album:       "Words",
		DurationMs:  215000,
		RecordingID: "b1a9c0e9-d987-4042-ae91-78d6a3267d69",
	}
	return []wireCase{
		{
			name:  "LyricsRequest",
			value: req,
			golden: `{"artist":"Lyric Band","title":"Words","album":"Words","durationMs":215000,` +
				`"recordingId":"b1a9c0e9-d987-4042-ae91-78d6a3267d69"}`,
		},
		{
			name: "LyricsResponse synced",
			value: LyricsResponse{
				Kind:        LyricsSynced,
				Lines:       []LyricLine{{StartMs: 1500, Text: "First line"}, {StartMs: 3250, Text: ""}},
				DurationMs:  215000,
				RecordingID: "b1a9c0e9-d987-4042-ae91-78d6a3267d69",
			},
			golden: `{"kind":"synced","lines":[{"startMs":1500,"text":"First line"},{"startMs":3250,"text":""}],` +
				`"durationMs":215000,"recordingId":"b1a9c0e9-d987-4042-ae91-78d6a3267d69"}`,
		},
		{
			name:   "LyricsResponse plain",
			value:  LyricsResponse{Kind: LyricsPlain, Text: "First line\nSecond line"},
			golden: `{"kind":"plain","text":"First line\nSecond line"}`,
		},
		{
			name:   "LyricsResponse miss",
			value:  LyricsResponse{},
			golden: `{}`,
		},
		{
			name:  "LyricsCall",
			value: LyricsCall{Request: LyricsRequest{Artist: "A", Title: "B"}, Settings: Settings{Enabled: true}},
			golden: `{"request":{"artist":"A","title":"B"},` +
				`"settings":{"enabled":true}}`,
		},
	}
}

// TestRegistryHoldsLyricProviders: a Lyric provider registers into the same
// Registry value as every other seam, reads back in registration order under its
// own slug namespace, and a malformed registration panics at the composition
// root.
func TestRegistryHoldsLyricProviders(t *testing.T) {
	stub := func(Settings) (LyricProvider, error) { return nil, nil }
	reg := NewRegistry()
	reg.RegisterLyricProvider(LyricProviderRegistration{
		Descriptor: Descriptor{Slug: "lrclib", Name: "LRCLIB"}, New: stub,
	})
	reg.RegisterLyricProvider(LyricProviderRegistration{
		Descriptor: Descriptor{Slug: "other-lyrics"}, New: stub,
	})

	got := reg.LyricProviders()
	if len(got) != 2 || got[0].Descriptor.Slug != "lrclib" || got[1].Descriptor.Slug != "other-lyrics" {
		t.Fatalf("LyricProviders() = %+v, want lrclib then other-lyrics", got)
	}
	if ep := got[0].Descriptor.ExtensionPoint; ep != ExtensionLyricProvider {
		t.Fatalf("extension point = %q, want %q", ep, ExtensionLyricProvider)
	}
	got[0].Descriptor.Slug = "mutated"
	if _, ok := reg.LyricProvider("lrclib"); !ok {
		t.Fatal("LyricProviders() handed out the registry's own backing array")
	}
	if _, ok := reg.WebReferenceProvider("lrclib"); ok {
		t.Fatal("WebReferenceProvider(lrclib) found a lyric provider")
	}
	var nilReg *Registry
	if nilReg.LyricProviders() != nil {
		t.Fatal("a nil registry listed lyric providers")
	}

	for _, tc := range []struct {
		name string
		reg  LyricProviderRegistration
	}{
		{"duplicate slug", LyricProviderRegistration{Descriptor: Descriptor{Slug: "other-lyrics"}, New: stub}},
		{"no slug", LyricProviderRegistration{New: stub}},
		{"no factory", LyricProviderRegistration{Descriptor: Descriptor{Slug: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			reg.RegisterLyricProvider(tc.reg)
		})
	}
}
