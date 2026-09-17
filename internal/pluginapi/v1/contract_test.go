package v1

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The contract's own suite, and the only unit-level suite the plugin system adds
// (PRD, "Secondary seam"). Its purpose is not coverage: it is to make an
// accidental field rename a DELIBERATE, visible act while the package can still
// change freely, and to make adding an Outcome impossible to do silently.
//
// Everything that crosses the contract must round-trip through JSON (ADR-0057
// decision 2), so each wire type is marshalled fully populated, compared to a
// checked-in golden document, and read back.

// wireCase is one wire type with every field set to a distinguishable value and
// the exact JSON it must produce.
type wireCase struct {
	name   string
	value  any
	golden string
}

func wireCases() []wireCase {
	return []wireCase{
		{
			name: "Settings",
			value: Settings{
				Enabled: true,
				Secret:  "sk-123",
				URL:     "https://api.example.test/v1",
				URL2:    "https://images.example.test",
				Events:  []string{"scan.completed", "playback.started"},
			},
			golden: `{"enabled":true,"secret":"sk-123","url":"https://api.example.test/v1",` +
				`"url2":"https://images.example.test","events":["scan.completed","playback.started"]}`,
		},
		{
			name: "Descriptor",
			value: Descriptor{
				Slug:           "example",
				Name:           "Example Source",
				ExtensionPoint: ExtensionMetadataProvider,
				Kinds:          []string{KindVideo, KindMusic},
				Role:           RoleAuthoritative,
				Class:          ClassFull,
				RequiresKey:    true,
				Capabilities:   []Capability{CapabilitySearch, CapabilityArtworkCandidates, CapabilityAlbumTracklist, CapabilityExternalRef},
				DefaultURL:     "https://api.example.test/v1",
				DefaultURL2:    "https://images.example.test",
				Description:    "An example source.",
				DocsURL:        "https://example.test/docs",
			},
			golden: `{"slug":"example","name":"Example Source","extensionPoint":"metadata-provider",` +
				`"kinds":["video","music"],"role":"authoritative","class":"full","requiresKey":true,` +
				`"capabilities":["search","artwork-candidates","album-tracklist","external-ref"],` +
				`"defaultUrl":"https://api.example.test/v1","defaultUrl2":"https://images.example.test",` +
				`"description":"An example source.","docsUrl":"https://example.test/docs"}`,
		},
		{
			name:   "Page",
			value:  Page{Limit: 20, Offset: 40},
			golden: `{"limit":20,"offset":40}`,
		},
		{
			name: "SubtitleRef",
			value: SubtitleRef{
				Title:     "Dune",
				Year:      2021,
				IMDBID:    "tt1160419",
				MovieHash: "8e245d9679d31e12",
				FileSize:  1234567890,
			},
			golden: `{"title":"Dune","year":2021,"imdbId":"tt1160419",` +
				`"movieHash":"8e245d9679d31e12","fileSize":1234567890}`,
		},
		{
			name: "SubtitleSearchRequest",
			value: SubtitleSearchRequest{
				Ref:      SubtitleRef{Title: "Dune", Year: 2021},
				Language: "de",
				Page:     Page{Limit: 10, Offset: 0},
			},
			// Page is embedded, so limit/offset are flat on the wire.
			golden: `{"ref":{"title":"Dune","year":2021},"language":"de","limit":10}`,
		},
		{
			name: "SubtitleCandidate",
			value: SubtitleCandidate{
				ID:              "42",
				Language:        "de",
				Format:          "srt",
				Release:         "Dune.2021.1080p.BluRay",
				HearingImpaired: true,
				Forced:          true,
				MatchedBy:       "moviehash",
				Downloads:       9001,
			},
			golden: `{"id":"42","language":"de","format":"srt","release":"Dune.2021.1080p.BluRay",` +
				`"hearingImpaired":true,"forced":true,"matchedBy":"moviehash","downloads":9001}`,
		},
		{
			name: "SubtitleSearchResponse",
			value: SubtitleSearchResponse{
				Outcome:    OutcomeMatched,
				Candidates: []SubtitleCandidate{{ID: "42", Language: "de", Format: "srt"}},
				Detail:     "1 candidate",
			},
			golden: `{"outcome":"matched","candidates":[{"id":"42","language":"de","format":"srt"}],` +
				`"detail":"1 candidate"}`,
		},
		{
			name: "SubtitleDownloadRequest",
			value: SubtitleDownloadRequest{
				Candidate: SubtitleCandidate{ID: "42", Format: "srt"},
				MaxBytes:  8 << 20,
			},
			golden: `{"candidate":{"id":"42","format":"srt"},"maxBytes":8388608}`,
		},
		{
			name: "SubtitleDownloadResponse",
			value: SubtitleDownloadResponse{
				Outcome:     OutcomeMatched,
				Data:        []byte("WEBVTT\n"),
				Format:      "vtt",
				ContentType: "text/vtt",
				Detail:      "7 bytes",
			},
			// Data is base64 in JSON — bytes come back whole, never streamed.
			golden: `{"outcome":"matched","data":"V0VCVlRUCg==","format":"vtt",` +
				`"contentType":"text/vtt","detail":"7 bytes"}`,
		},
	}
}

// TestWireTypesRoundTripThroughJSON: every wire type marshals to exactly the
// checked-in document and reads back equal. A failure here is either a bug or a
// rename someone now has to make on purpose.
func TestWireTypesRoundTripThroughJSON(t *testing.T) {
	for _, tc := range wireCases() {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(encoded) != tc.golden {
				t.Fatalf("wire shape changed.\n got: %s\nwant: %s", encoded, tc.golden)
			}

			back := reflect.New(reflect.TypeOf(tc.value))
			if err := json.Unmarshal(encoded, back.Interface()); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := back.Elem().Interface(); !reflect.DeepEqual(got, tc.value) {
				t.Fatalf("round trip lost data.\n got: %#v\nwant: %#v", got, tc.value)
			}
		})
	}
}

// TestZeroValuesRoundTrip: a zero value is the common case on the wire (an
// un-enriched Title's ref, a no-match response) and must survive too — omitempty
// must never turn an absent field into a decode error.
func TestZeroValuesRoundTrip(t *testing.T) {
	for _, tc := range wireCases() {
		t.Run(tc.name, func(t *testing.T) {
			zero := reflect.New(reflect.TypeOf(tc.value)).Elem().Interface()
			encoded, err := json.Marshal(zero)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			back := reflect.New(reflect.TypeOf(tc.value))
			if err := json.Unmarshal(encoded, back.Interface()); err != nil {
				t.Fatalf("unmarshal %s: %v", encoded, err)
			}
			if got := back.Elem().Interface(); !reflect.DeepEqual(got, zero) {
				t.Fatalf("zero round trip lost data.\n got: %#v\nwant: %#v", got, zero)
			}
		})
	}
}

// TestAllOutcomesIsComplete: AllOutcomes is what every adapter's mapping test
// ranges over, so it has to actually list every value. A new constant that is not
// in the list would let a service silently forget to decide what it means.
func TestAllOutcomesIsComplete(t *testing.T) {
	declared := []Outcome{
		OutcomeMatched, OutcomeNoMatch, OutcomeRejected, OutcomeUnavailable,
		OutcomeRefInvalid, OutcomeRefKindMismatch, OutcomeRefUnsupportedKind,
	}
	if !reflect.DeepEqual(AllOutcomes(), declared) {
		t.Fatalf("AllOutcomes() = %v, want %v", AllOutcomes(), declared)
	}
	seen := map[Outcome]bool{}
	for _, o := range AllOutcomes() {
		if o == "" {
			t.Fatal("the empty string is not an outcome — it is an unset field")
		}
		if seen[o] {
			t.Fatalf("outcome %q listed twice", o)
		}
		seen[o] = true
	}
}

// TestDescriptorHasCapability: the host asks before it calls, so an undeclared
// operation costs no call at all.
func TestDescriptorHasCapability(t *testing.T) {
	d := Descriptor{Capabilities: []Capability{CapabilitySearch}}
	if !d.HasCapability(CapabilitySearch) {
		t.Error("declared capability reported as absent")
	}
	if d.HasCapability(CapabilityAlbumTracklist) {
		t.Error("undeclared capability reported as present")
	}
	if (Descriptor{}).HasCapability(CapabilitySearch) {
		t.Error("a Plugin that declared nothing must declare nothing")
	}
}

// TestRegistryIsAValue: registration is explicit and the registry is a value, so
// two registries never see each other's Plugins (ADR-0057 decision 5). The nil
// registry a narrow test leaves unset reads as "no Plugins", not a panic.
func TestRegistryIsAValue(t *testing.T) {
	stub := func(Settings) (SubtitleProvider, error) { return nil, nil }
	a := NewRegistry()
	a.RegisterSubtitleProvider(SubtitleProviderRegistration{
		Descriptor: Descriptor{Slug: "one", Name: "One"}, New: stub,
	})
	b := NewRegistry()

	if got := len(a.SubtitleProviders()); got != 1 {
		t.Fatalf("registry a has %d subtitle providers, want 1", got)
	}
	if got := len(b.SubtitleProviders()); got != 0 {
		t.Fatalf("registry b saw a's registrations (%d) — the registry is not a value", got)
	}
	var nilReg *Registry
	if got := len(nilReg.SubtitleProviders()); got != 0 {
		t.Fatalf("nil registry returned %d providers", got)
	}
	if _, ok := nilReg.SubtitleProvider("one"); ok {
		t.Fatal("nil registry claimed to know a provider")
	}

	// The Extension point is stamped by registering, not by the caller remembering.
	reg, ok := a.SubtitleProvider("one")
	if !ok {
		t.Fatal("registered provider not found by slug")
	}
	if reg.Descriptor.ExtensionPoint != ExtensionSubtitleProvider {
		t.Fatalf("extension point = %q, want %q", reg.Descriptor.ExtensionPoint, ExtensionSubtitleProvider)
	}

	// A copy handed out cannot reorder what the next caller sees.
	list := a.SubtitleProviders()
	list[0].Descriptor.Slug = "mutated"
	if again, _ := a.SubtitleProvider("one"); again.Descriptor.Slug != "one" {
		t.Fatal("SubtitleProviders() handed out the registry's own backing array")
	}
}

// TestRegisterRefusesADuplicateSlug: a second Plugin claiming a slug would make
// the persisted settings row ambiguous, so the composition root fails loudly at
// boot rather than an Admin's key reaching the wrong Plugin.
func TestRegisterRefusesADuplicateSlug(t *testing.T) {
	stub := func(Settings) (SubtitleProvider, error) { return nil, nil }
	r := NewRegistry()
	r.RegisterSubtitleProvider(SubtitleProviderRegistration{
		Descriptor: Descriptor{Slug: "one"}, New: stub,
	})

	for _, tc := range []struct {
		name string
		reg  SubtitleProviderRegistration
	}{
		{"duplicate slug", SubtitleProviderRegistration{Descriptor: Descriptor{Slug: "one"}, New: stub}},
		{"no slug", SubtitleProviderRegistration{Descriptor: Descriptor{}, New: stub}},
		{"no factory", SubtitleProviderRegistration{Descriptor: Descriptor{Slug: "two"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("registration was accepted; want a panic at the composition root")
				}
			}()
			r.RegisterSubtitleProvider(tc.reg)
		})
	}
}
