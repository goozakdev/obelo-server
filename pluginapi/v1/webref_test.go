package v1

import "testing"

// webReferenceWireCases pins the documents of the Web reference provider
// Extension point: the request, one reference, the response, and the envelope an
// Installed plugin is handed.
func webReferenceWireCases() []wireCase {
	req := WebReferencesRequest{
		Kind: "movie",
		IDs:  map[string]string{"imdb": "tt1160419", "tmdb": "438631"},
	}
	ref := WebReference{
		Namespace: "imdb",
		ID:        "tt1160419",
		Label:     "IMDb",
		URL:       "https://www.imdb.com/title/tt1160419/",
	}
	return []wireCase{
		{
			name:   "WebReferencesRequest",
			value:  req,
			golden: `{"kind":"movie","ids":{"imdb":"tt1160419","tmdb":"438631"}}`,
		},
		{
			name:  "WebReference",
			value: ref,
			golden: `{"namespace":"imdb","id":"tt1160419","label":"IMDb",` +
				`"url":"https://www.imdb.com/title/tt1160419/"}`,
		},
		{
			name:  "WebReferencesResponse",
			value: WebReferencesResponse{References: []WebReference{ref}},
			golden: `{"references":[{"namespace":"imdb","id":"tt1160419","label":"IMDb",` +
				`"url":"https://www.imdb.com/title/tt1160419/"}]}`,
		},
		{
			name:  "WebReferencesCall",
			value: WebReferencesCall{Request: req, Settings: Settings{Enabled: true}},
			golden: `{"request":{"kind":"movie","ids":{"imdb":"tt1160419","tmdb":"438631"}},` +
				`"settings":{"enabled":true}}`,
		},
	}
}

// TestRegistryHoldsWebReferenceProviders: a Web reference provider registers into
// the same Registry value as every other seam, reads back in registration order
// under its own slug namespace, and a malformed registration panics at the
// composition root.
func TestRegistryHoldsWebReferenceProviders(t *testing.T) {
	stub := func(Settings) (WebReferenceProvider, error) { return nil, nil }
	reg := NewRegistry()
	reg.RegisterWebReferenceProvider(WebReferenceProviderRegistration{
		Descriptor: Descriptor{Slug: "imdb-links", Name: "IMDb links"}, New: stub,
	})
	reg.RegisterWebReferenceProvider(WebReferenceProviderRegistration{
		Descriptor: Descriptor{Slug: "trakt-links"}, New: stub,
	})

	got := reg.WebReferenceProviders()
	if len(got) != 2 || got[0].Descriptor.Slug != "imdb-links" || got[1].Descriptor.Slug != "trakt-links" {
		t.Fatalf("WebReferenceProviders() = %+v, want imdb-links then trakt-links", got)
	}
	if ep := got[0].Descriptor.ExtensionPoint; ep != ExtensionWebReferenceProvider {
		t.Fatalf("extension point = %q, want %q", ep, ExtensionWebReferenceProvider)
	}
	got[0].Descriptor.Slug = "mutated"
	if _, ok := reg.WebReferenceProvider("imdb-links"); !ok {
		t.Fatal("WebReferenceProviders() handed out the registry's own backing array")
	}
	if _, ok := reg.EventSink("imdb-links"); ok {
		t.Fatal("EventSink(imdb-links) found a web reference provider")
	}
	var nilReg *Registry
	if nilReg.WebReferenceProviders() != nil {
		t.Fatal("a nil registry listed web reference providers")
	}

	for _, tc := range []struct {
		name string
		reg  WebReferenceProviderRegistration
	}{
		{"duplicate slug", WebReferenceProviderRegistration{Descriptor: Descriptor{Slug: "trakt-links"}, New: stub}},
		{"no slug", WebReferenceProviderRegistration{New: stub}},
		{"no factory", WebReferenceProviderRegistration{Descriptor: Descriptor{Slug: "other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			reg.RegisterWebReferenceProvider(tc.reg)
		})
	}
}
