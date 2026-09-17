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

// intPtr is for the one wire field that is a pointer, because absent and zero are
// two different instructions there (see Settings.RateLimitMillis).
func intPtr(n int) *int { return &n }

func wireCases() []wireCase {
	return append([]wireCase{
		{
			name: "Settings",
			value: Settings{
				Enabled:         true,
				Secret:          "sk-123",
				URL:             "https://api.example.test/v1",
				URL2:            "https://images.example.test",
				Events:          []string{"scan.completed", "playback.started"},
				Language:        "en-US",
				RateLimitMillis: intPtr(1000),
			},
			golden: `{"enabled":true,"secret":"sk-123","url":"https://api.example.test/v1",` +
				`"url2":"https://images.example.test","events":["scan.completed","playback.started"],` +
				`"language":"en-US","rateLimitMillis":1000}`,
		},
		{
			// A rate limit of ZERO is the operator's "do not throttle at all" and has to
			// survive the wire as a stated 0, not vanish into the absent "use your own
			// default" (ADR-0049 — a mirror with no rate policy is a real setting).
			name:   "Settings with an explicit zero rate limit",
			value:  Settings{Enabled: true, RateLimitMillis: intPtr(0)},
			golden: `{"enabled":true,"rateLimitMillis":0}`,
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
				Capabilities: []Capability{
					CapabilitySearch, CapabilityArtworkCandidates, CapabilityAlbumTracklist,
					CapabilityExternalRef, CapabilityEpisodeList,
				},
				DefaultURL:  "https://api.example.test/v1",
				DefaultURL2: "https://images.example.test",
				Description: "An example source.",
				DocsURL:     "https://example.test/docs",
			},
			golden: `{"slug":"example","name":"Example Source","extensionPoint":"metadata-provider",` +
				`"kinds":["video","music"],"role":"authoritative","class":"full","requiresKey":true,` +
				`"capabilities":["search","artwork-candidates","album-tracklist","external-ref","episode-list"],` +
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
		{
			name:   "EventEntity",
			value:  EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
			golden: `{"id":"lib-1","name":"Movies","kind":"movie"}`,
		},
		{
			name:   "EventActor",
			value:  EventActor{LinkID: "link-1", Name: "Brandon's server"},
			golden: `{"linkId":"link-1","name":"Brandon's server"}`,
		},
		{
			name: "EventScan",
			value: EventScan{
				TitlesFound: 12, FilesFound: 14, Added: 2, Removed: 1, Scope: "The Wire",
			},
			golden: `{"titlesFound":12,"filesFound":14,"added":2,"removed":1,"scope":"The Wire"}`,
		},
		{
			name: "EventEnrich",
			value: EventEnrich{
				Total: 40, Done: 40, Matched: 33, Unmatched: 4, Failed: 1,
				Disabled: 1, Retrying: 1,
			},
			golden: `{"total":40,"done":40,"matched":33,"unmatched":4,"failed":1,` +
				`"disabled":1,"retrying":1}`,
		},
		{
			// Every block a LEGAL document may carry at once. Not a document any
			// event actually produces — no event names both a Library and a Title,
			// and none of them name a Device alongside a scan — but it is the case
			// that pins the FIELD ORDER of the whole type, which is what a receiving
			// script's golden fixtures are compared against.
			//
			// It carries the SCAN block and not the enrich one, and its twin below
			// carries the enrich block, because the two are MUTUALLY EXCLUSIVE and
			// the JSON schema now says so (issue 07). A single case holding both
			// pinned the order at the price of being a document the contract
			// forbids — which nobody noticed until the rule stopped being prose.
			name: "SinkEvent",
			value: SinkEvent{
				ID:      "6f9619ff-8b86-d011-b42d-00c04fc964ff",
				Type:    EventPlaybackStarted,
				At:      "2026-09-16T12:00:00Z",
				Library: EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
				Title:   EventEntity{ID: "title-1", Name: "Dune", Kind: "movie"},
				Actor:   EventActor{UserID: "user-1", Name: "brandon"},
				Device:  EventEntity{ID: "dev-1", Name: "Laptop", Kind: "macos"},
				Scan:    &EventScan{TitlesFound: 12, FilesFound: 14},
			},
			golden: `{"id":"6f9619ff-8b86-d011-b42d-00c04fc964ff","type":"playback.started",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-1","name":"Movies","kind":"movie"},` +
				`"title":{"id":"title-1","name":"Dune","kind":"movie"},` +
				`"actor":{"userId":"user-1","name":"brandon"},` +
				`"device":{"id":"dev-1","name":"Laptop","kind":"macos"},` +
				`"scan":{"titlesFound":12,"filesFound":14}}`,
		},
		{
			// The maximal case's twin: the same every-block document with the ENRICH
			// block in the scan block's place, so the enrich block's position in the
			// field order is pinned by a document the contract permits.
			name: "SinkEvent/maximalWithEnrich",
			value: SinkEvent{
				ID:      "7f9619ff-8b86-d011-b42d-00c04fc964ff",
				Type:    EventPlaybackStarted,
				At:      "2026-09-16T12:00:00Z",
				Library: EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
				Title:   EventEntity{ID: "title-1", Name: "Dune", Kind: "movie"},
				Actor:   EventActor{UserID: "user-1", Name: "brandon"},
				Device:  EventEntity{ID: "dev-1", Name: "Laptop", Kind: "macos"},
				Enrich:  &EventEnrich{Total: 2, Done: 2, Matched: 2},
			},
			golden: `{"id":"7f9619ff-8b86-d011-b42d-00c04fc964ff","type":"playback.started",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-1","name":"Movies","kind":"movie"},` +
				`"title":{"id":"title-1","name":"Dune","kind":"movie"},` +
				`"actor":{"userId":"user-1","name":"brandon"},` +
				`"device":{"id":"dev-1","name":"Laptop","kind":"macos"},` +
				`"enrich":{"total":2,"done":2,"matched":2,"unmatched":0}}`,
		},
		{
			// The shape an operator's webhook actually receives today: only the
			// blocks the event has something to say in. A scan that found nothing
			// still carries its scan block (EventScan is a pointer for exactly this
			// reason) — "zero files" is an answer, not an absence.
			name: "SinkEvent/scanCompleted",
			value: SinkEvent{
				ID:      "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
				Type:    EventScanCompleted,
				At:      "2026-09-16T12:00:00Z",
				Library: EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
				Scan:    &EventScan{},
			},
			golden: `{"id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"scan.completed",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-1","name":"Movies","kind":"movie"},` +
				`"scan":{"titlesFound":0,"filesFound":0}}`,
		},
		{
			// An Enrichment pass with nothing left to do, for the scan case's
			// reason: "40 Titles, all already settled" is an answer, and an
			// omit-when-empty block would report it as no pass having happened.
			name: "SinkEvent/enrichCompleted",
			value: SinkEvent{
				ID:      "2b4e28ba-2fa1-11d2-883f-0016d3cca427",
				Type:    EventEnrichCompleted,
				At:      "2026-09-16T12:00:00Z",
				Library: EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
				Enrich:  &EventEnrich{Total: 40, Done: 40, Matched: 40},
			},
			golden: `{"id":"2b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"enrich.completed",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-1","name":"Movies","kind":"movie"},` +
				`"enrich":{"total":40,"done":40,"matched":40,"unmatched":0}}`,
		},
		{
			// A LOCAL play: the person is this server's own User, so the actor names
			// them. No library block — a playback event is about a Title.
			name: "SinkEvent/playbackStarted",
			value: SinkEvent{
				ID:     "3b4e28ba-2fa1-11d2-883f-0016d3cca427",
				Type:   EventPlaybackStarted,
				At:     "2026-09-16T12:00:00Z",
				Title:  EventEntity{ID: "title-1", Name: "Dune", Kind: "movie"},
				Actor:  EventActor{UserID: "user-1", Name: "brandon"},
				Device: EventEntity{ID: "dev-1", Name: "Laptop", Kind: "macos"},
			},
			golden: `{"id":"3b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"playback.started",` +
				`"at":"2026-09-16T12:00:00Z","title":{"id":"title-1","name":"Dune","kind":"movie"},` +
				`"actor":{"userId":"user-1","name":"brandon"},` +
				`"device":{"id":"dev-1","name":"Laptop","kind":"macos"}}`,
		},
		{
			// A RELAYED play, stopping: the actor is the LINK, there is no userId
			// anywhere in the document, and the only name is the label THIS
			// household's Admin typed for the linked Server (ADR-0054 section 3).
			// The Device is absent because a relayed session is bound to the linked
			// Server's own Device record and naming it would say something about
			// the far household's hardware.
			name: "SinkEvent/playbackStoppedOverALink",
			value: SinkEvent{
				ID:    "4b4e28ba-2fa1-11d2-883f-0016d3cca427",
				Type:  EventPlaybackStopped,
				At:    "2026-09-16T12:34:56Z",
				Title: EventEntity{ID: "title-1", Name: "Dune", Kind: "movie"},
				Actor: EventActor{LinkID: "user-9", Name: "Brandon's server"},
			},
			golden: `{"id":"4b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"playback.stopped",` +
				`"at":"2026-09-16T12:34:56Z","title":{"id":"title-1","name":"Dune","kind":"movie"},` +
				`"actor":{"linkId":"user-9","name":"Brandon's server"}}`,
		},
		{
			// The refetch nudge: a Library and nothing else. There is deliberately
			// no diff — the catalog is the truth about what changed.
			name: "SinkEvent/libraryChanged",
			value: SinkEvent{
				ID:      "5b4e28ba-2fa1-11d2-883f-0016d3cca427",
				Type:    EventLibraryChanged,
				At:      "2026-09-16T12:00:00Z",
				Library: EventEntity{ID: "lib-2", Name: "Shows", Kind: "show"},
			},
			golden: `{"id":"5b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"library.changed",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-2","name":"Shows","kind":"show"}}`,
		},
	}, append(metadataWireCases(), installedWireCases()...)...)
}

// installedWireCases pins the documents an INSTALLED plugin deals in (ADR-0058):
// the manifest an author writes by hand, the request and response of the one
// guest call this slice implements, and the two shapes of the fetch host function.
//
// These matter more than most goldens here, because they are the documents an
// author who is not writing Go reads the schema for. A field order or a key name
// that drifts is a Plugin in another language that stops loading.
func installedWireCases() []wireCase {
	return []wireCase{
		{
			// The whole manifest, every field set, so the key ORDER is pinned.
			name: "Manifest",
			value: Manifest{
				ID:         "example-sink",
				Name:       "Example Sink",
				Version:    "0.2.0",
				APIVersion: APIVersion,
				Provides: []ManifestProvides{{
					Kind:           ExtensionEventSink,
					RequiresSecret: true,
				}},
				Network:     ManifestNetwork{Hosts: []string{"automation.example.test"}},
				Settings:    ManifestSettings{RequiresSecret: true},
				Description: "Posts a signed document when something finishes.",
				DocsURL:     "https://example.test/obelo-plugin",
				Module:      "plugin.wasm",
			},
			golden: `{"id":"example-sink","name":"Example Sink","version":"0.2.0","apiVersion":1,` +
				`"provides":[{"kind":"event-sink","requiresSecret":true}],` +
				`"network":{"hosts":["automation.example.test"]},"settings":{"requiresSecret":true},` +
				`"description":"Posts a signed document when something finishes.",` +
				`"docsUrl":"https://example.test/obelo-plugin","module":"plugin.wasm"}`,
		},
		{
			// The smallest manifest that is still a Plugin: a Metadata provider
			// declaring where it belongs in the chain and what it can do. Nothing
			// optional is present, which is what an author's first file looks like.
			name: "Manifest/minimal",
			value: Manifest{
				ID:         "anilist",
				Name:       "AniList",
				APIVersion: APIVersion,
				Provides: []ManifestProvides{{
					Kind:         ExtensionMetadataProvider,
					Kinds:        []string{KindVideo},
					Role:         RoleSupplement,
					Class:        ClassFull,
					Capabilities: []Capability{CapabilitySearch},
				}},
			},
			golden: `{"id":"anilist","name":"AniList","apiVersion":1,` +
				`"provides":[{"kind":"metadata-provider","kinds":["video"],"role":"supplement",` +
				`"class":"full","capabilities":["search"]}]}`,
		},
		{
			name: "ManifestProvides",
			value: ManifestProvides{
				Kind:           ExtensionMetadataProvider,
				Kinds:          []string{KindVideo, KindMusic},
				Role:           RoleAuthoritative,
				Class:          ClassFull,
				Capabilities:   []Capability{CapabilitySearch, CapabilityExternalRef},
				RequiresSecret: true,
			},
			golden: `{"kind":"metadata-provider","kinds":["video","music"],"role":"authoritative",` +
				`"class":"full","capabilities":["search","external-ref"],"requiresSecret":true}`,
		},
		{
			name:   "ManifestNetwork",
			value:  ManifestNetwork{Hosts: []string{"api.example.test", "images.example.test"}},
			golden: `{"hosts":["api.example.test","images.example.test"]}`,
		},
		{
			name: "ManifestSettings",
			value: ManifestSettings{
				RequiresSecret: true,
				DefaultURL:     "https://api.example.test/v1",
				DefaultURL2:    "https://images.example.test",
			},
			golden: `{"requiresSecret":true,"defaultUrl":"https://api.example.test/v1",` +
				`"defaultUrl2":"https://images.example.test"}`,
		},
		{
			// The settings ride WITH the call: a secret reaches a guest for the
			// duration of one delivery and is never installed into it.
			name: "SinkDeliverRequest",
			value: SinkDeliverRequest{
				Event: SinkEvent{
					ID:      "1b4e28ba-2fa1-11d2-883f-0016d3cca427",
					Type:    EventScanCompleted,
					At:      "2026-09-16T12:00:00Z",
					Library: EventEntity{ID: "lib-1", Name: "Movies", Kind: "movie"},
					Scan:    &EventScan{TitlesFound: 3, FilesFound: 4},
				},
				Settings: Settings{
					Enabled: true,
					Secret:  "topsecret",
					URL:     "https://automation.example.test/obelo",
					Events:  []string{EventScanCompleted},
				},
			},
			golden: `{"event":{"id":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","type":"scan.completed",` +
				`"at":"2026-09-16T12:00:00Z","library":{"id":"lib-1","name":"Movies","kind":"movie"},` +
				`"scan":{"titlesFound":3,"filesFound":4}},` +
				`"settings":{"enabled":true,"secret":"topsecret",` +
				`"url":"https://automation.example.test/obelo","events":["scan.completed"]}}`,
		},
		{
			name:   "SinkDeliverResponse",
			value:  SinkDeliverResponse{Delivered: true},
			golden: `{"delivered":true}`,
		},
		{
			// A failure the author described. delivered is written either way — the
			// key is always present, so a guest that forgets it says "not delivered",
			// which is the safe reading.
			name:   "SinkDeliverResponse/failed",
			value:  SinkDeliverResponse{Error: "automation.example.test answered 503"},
			golden: `{"delivered":false,"error":"automation.example.test answered 503"}`,
		},
		{
			name: "FetchRequest",
			value: FetchRequest{
				Method:  "POST",
				URL:     "https://automation.example.test/obelo",
				Headers: []FetchHeader{{Name: "Content-Type", Value: "application/json"}},
				Body:    []byte(`{"ok":true}`),
			},
			golden: `{"method":"POST","url":"https://automation.example.test/obelo",` +
				`"headers":[{"name":"Content-Type","value":"application/json"}],` +
				`"body":"eyJvayI6dHJ1ZX0="}`,
		},
		{
			name:   "FetchHeader",
			value:  FetchHeader{Name: "Accept", Value: "application/json"},
			golden: `{"name":"Accept","value":"application/json"}`,
		},
		{
			name: "FetchResponse",
			value: FetchResponse{
				Status:  204,
				Headers: []FetchHeader{{Name: "Content-Length", Value: "0"}},
			},
			golden: `{"status":204,"headers":[{"name":"Content-Length","value":"0"}]}`,
		},
		{
			// The refusal an author will meet first, and the one the audit line is
			// about. No status, no body: the request was never sent.
			name:   "FetchResponse/refused",
			value:  FetchResponse{Refused: "host not in allowlist"},
			golden: `{"refused":"host not in allowlist"}`,
		},
	}
}

// TestAllEventTypesIsComplete: the curated set is closed (ADR-0057 decision 6), so
// AllEventTypes has to list exactly it. A type added to the constants but not the
// list would be one nothing could subscribe to; one in the list with no constant
// would be one nothing can produce.
func TestAllEventTypesIsComplete(t *testing.T) {
	declared := []string{
		EventScanCompleted, EventEnrichCompleted,
		EventPlaybackStarted, EventPlaybackStopped, EventLibraryChanged,
	}
	if !reflect.DeepEqual(AllEventTypes(), declared) {
		t.Fatalf("AllEventTypes() = %v, want %v", AllEventTypes(), declared)
	}
	seen := map[string]bool{}
	for _, ev := range AllEventTypes() {
		if ev == "" {
			t.Fatal("the empty string is not an event type — it is an unset field")
		}
		if seen[ev] {
			t.Fatalf("event type %q listed twice", ev)
		}
		seen[ev] = true
	}
}

// TestRegistryHoldsEventSinks: a sink registers into the same Registry VALUE the
// providers do, and reads back in registration order under its own slug namespace.
func TestRegistryHoldsEventSinks(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterEventSink(EventSinkRegistration{
		Descriptor: Descriptor{Slug: "webhook", Name: "Webhook"},
		New:        func(Settings) (EventSink, error) { return nil, nil },
	})

	sinks := reg.EventSinks()
	if len(sinks) != 1 || sinks[0].Descriptor.Slug != "webhook" {
		t.Fatalf("EventSinks() = %+v, want the one webhook registration", sinks)
	}
	// Registering fills in the Extension point, so a Descriptor cannot claim to be
	// something other than what it registered as.
	if got := sinks[0].Descriptor.ExtensionPoint; got != ExtensionEventSink {
		t.Fatalf("extension point = %q, want %q", got, ExtensionEventSink)
	}
	if _, ok := reg.EventSink("webhook"); !ok {
		t.Fatal("EventSink(webhook) not found")
	}
	// Slug namespaces do not collide across Extension points: a Subtitle provider
	// lookup must not find a sink.
	if _, ok := reg.SubtitleProvider("webhook"); ok {
		t.Fatal("SubtitleProvider(webhook) found an event sink")
	}
	// A nil Registry reads as "no Plugins" rather than panicking, which is what
	// keeps a narrow api.Deps test from needing a composition root.
	var nilReg *Registry
	if got := nilReg.EventSinks(); got != nil {
		t.Fatalf("nil registry EventSinks() = %v, want nil", got)
	}
	if _, ok := nilReg.EventSink("webhook"); ok {
		t.Fatal("nil registry claimed to have a sink")
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
