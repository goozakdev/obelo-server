package v1

import (
	"encoding/json"
	"fmt"
	"testing"
)

// onlineSourceWireCases pins the documents of the Online source provider
// Extension point: a rows answer, a resolve request and its muxed answer, a miss,
// and the two envelopes an Installed plugin is handed.
func onlineSourceWireCases() []wireCase {
	return []wireCase{
		{
			name: "OnlineRowsResponse",
			value: OnlineRowsResponse{Rows: []OnlineRow{{
				ID: "recent", Label: "Recently added",
				Items: []OnlineItem{{
					ID: "v1", Title: "A talk", ThumbnailURL: "https://media.example/v1.jpg", DurationMs: 61000,
					Description: "About things", PublishedAt: "2026-09-01T00:00:00Z",
				}},
			}}},
			golden: `{"rows":[{"id":"recent","label":"Recently added","items":[{"id":"v1","title":"A talk",` +
				`"thumbnailUrl":"https://media.example/v1.jpg","durationMs":61000,"description":"About things",` +
				`"publishedAt":"2026-09-01T00:00:00Z"}]}]}`,
		},
		{
			name: "OnlineRowsResponse paged",
			value: OnlineRowsResponse{Rows: []OnlineRow{{
				ID: "recent", Label: "Recently added", NextCursor: "c2",
				Items: []OnlineItem{{ID: "v1", Title: "A talk", ThumbnailURL: "https://media.example/v1.jpg"}},
			}}},
			golden: `{"rows":[{"id":"recent","label":"Recently added","items":[{"id":"v1","title":"A talk",` +
				`"thumbnailUrl":"https://media.example/v1.jpg","durationMs":0}],"nextCursor":"c2"}]}`,
		},
		{
			name:   "OnlineRowRequest",
			value:  OnlineRowRequest{RowID: "recent", Cursor: "c2"},
			golden: `{"rowId":"recent","cursor":"c2"}`,
		},
		{
			name: "OnlineRowResponse",
			value: OnlineRowResponse{
				Items:      []OnlineItem{{ID: "v2", Title: "Another", ThumbnailURL: "https://media.example/v2.jpg", DurationMs: 5}},
				NextCursor: "c3",
			},
			golden: `{"items":[{"id":"v2","title":"Another","thumbnailUrl":"https://media.example/v2.jpg",` +
				`"durationMs":5}],"nextCursor":"c3"}`,
		},
		{
			name:   "OnlineRowResponse last page",
			value:  OnlineRowResponse{Items: []OnlineItem{}},
			golden: `{"items":[]}`,
		},
		{
			name:  "OnlineRowCall",
			value: OnlineRowCall{Request: OnlineRowRequest{RowID: "recent", Cursor: "c2"}, Settings: Settings{Enabled: true}},
			golden: `{"request":{"rowId":"recent","cursor":"c2"},` +
				`"settings":{"enabled":true}}`,
		},
		{
			name:   "OnlineResolveRequest",
			value:  OnlineResolveRequest{ItemID: "v1", Hints: OnlineHints{MaxHeight: 1080}},
			golden: `{"itemId":"v1","hints":{"maxHeight":1080}}`,
		},
		{
			name: "OnlineResolveResponse muxed",
			value: OnlineResolveResponse{Variants: []OnlineVariant{{
				URL: "https://media.example/v1.mp4", Container: "mp4", Codecs: []string{"h264", "aac"},
				Resolution: "720p", Headers: map[string]string{"Referer": "https://media.example/"},
			}}},
			golden: `{"variants":[{"url":"https://media.example/v1.mp4","container":"mp4",` +
				`"codecs":["h264","aac"],"resolution":"720p","headers":{"Referer":"https://media.example/"}}]}`,
		},
		{
			name: "OnlineResolveResponse split and manifest",
			value: OnlineResolveResponse{Variants: []OnlineVariant{
				{
					Kind: OnlineVariantSplit, VideoURL: "https://media.example/v.mp4", AudioURL: "https://media.example/a.m4a",
					Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: "1080p",
				},
				{Kind: OnlineVariantManifest, URL: "https://media.example/master.m3u8", Container: "hls", Resolution: "720p"},
			}},
			golden: `{"variants":[{"kind":"split","videoUrl":"https://media.example/v.mp4",` +
				`"audioUrl":"https://media.example/a.m4a","container":"mp4","codecs":["h264","aac"],"resolution":"1080p"},` +
				`{"kind":"manifest","url":"https://media.example/master.m3u8","container":"hls","resolution":"720p"}]}`,
		},
		{
			name:   "OnlineResolveResponse miss",
			value:  OnlineResolveResponse{},
			golden: `{}`,
		},
		{
			name:  "OnlineRowsCall",
			value: OnlineRowsCall{Settings: Settings{Enabled: true}},
			golden: `{"request":{},` +
				`"settings":{"enabled":true}}`,
		},
		{
			name:  "OnlineResolveCall",
			value: OnlineResolveCall{Request: OnlineResolveRequest{ItemID: "v1"}, Settings: Settings{Enabled: true}},
			golden: `{"request":{"itemId":"v1","hints":{}},` +
				`"settings":{"enabled":true}}`,
		},
	}
}

// TestRegistryHoldsOnlineSourceProviders: an Online source provider registers into
// the same Registry value as every other seam, reads back in registration order
// under its own slug namespace, and a malformed registration panics at the
// composition root.
func TestRegistryHoldsOnlineSourceProviders(t *testing.T) {
	stub := func(Settings) (OnlineSourceProvider, error) { return nil, nil }
	reg := NewRegistry()
	reg.RegisterOnlineSourceProvider(OnlineSourceProviderRegistration{
		Descriptor: Descriptor{Slug: "peertube", Name: "PeerTube"}, New: stub,
	})
	reg.RegisterOnlineSourceProvider(OnlineSourceProviderRegistration{
		Descriptor: Descriptor{Slug: "archive"}, New: stub,
	})

	got := reg.OnlineSourceProviders()
	if len(got) != 2 || got[0].Descriptor.Slug != "peertube" || got[1].Descriptor.Slug != "archive" {
		t.Fatalf("OnlineSourceProviders() = %+v, want peertube then archive", got)
	}
	if ep := got[0].Descriptor.ExtensionPoint; ep != ExtensionOnlineSourceProvider {
		t.Fatalf("extension point = %q, want %q", ep, ExtensionOnlineSourceProvider)
	}
	got[0].Descriptor.Slug = "mutated"
	if _, ok := reg.OnlineSourceProvider("peertube"); !ok {
		t.Fatal("OnlineSourceProviders() handed out the registry's own backing array")
	}
	if _, ok := reg.MarkerProvider("peertube"); ok {
		t.Fatal("MarkerProvider(peertube) found an online source provider")
	}
	var nilReg *Registry
	if nilReg.OnlineSourceProviders() != nil {
		t.Fatal("a nil registry listed online source providers")
	}

	for _, tc := range []struct {
		name string
		reg  OnlineSourceProviderRegistration
	}{
		{"duplicate slug", OnlineSourceProviderRegistration{Descriptor: Descriptor{Slug: "archive"}, New: stub}},
		{"no slug", OnlineSourceProviderRegistration{New: stub}},
		{"no factory", OnlineSourceProviderRegistration{Descriptor: Descriptor{Slug: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			reg.RegisterOnlineSourceProvider(tc.reg)
		})
	}
}

// TestAnOnlineItemWithANonNumericDurationDecodesAsNegative: one item's bad
// durationMs must not fail the whole answer; it reads as a negative duration the
// host drops, and the sibling decodes untouched.
func TestAnOnlineItemWithANonNumericDurationDecodesAsNegative(t *testing.T) {
	var resp OnlineRowResponse
	doc := `{"items":[{"id":"a","title":"A","durationMs":"soon"},{"id":"b","title":"B","durationMs":-5},` +
		`{"id":"c","title":"C","durationMs":172800000},{"id":"d","title":"D"},{"id":"e","title":"E","durationMs":1e30}]}`
	if err := json.Unmarshal([]byte(doc), &resp); err != nil {
		t.Fatalf("decoding failed over a bad sibling: %v", err)
	}
	var got []int64
	for _, it := range resp.Items {
		got = append(got, it.DurationMs)
	}
	if want := []int64{-1, -1, 172800000, 0, -1}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("durations = %v, want %v", got, want)
	}
}
