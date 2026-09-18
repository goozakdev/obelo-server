package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/testharness"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Black-box tests for the MANIFEST-DECLARED settings schema (.scratch/plugin-
// system issue 13): an Installed plugin declares its own typed fields, the Admin
// fills them in on the Plugins screen, the server refuses a bad one by name, and
// the guest reads the good ones back through settings_get in the shape it
// declared them.
//
// And the hole Phase 1 flagged twice: a provider that declares it needs no
// credential could not be turned on at all, because "active" was inferred from
// key presence. The last test here is the one issue 04 named and issue 11 worked
// around — a keyless Full provider leading a Library — with issue 11's negative
// kept beside it so the fix cannot have been "stop asking".
//
// Everything is observed through the API an Admin uses. The module is compiled
// from source by the suite, so the schema crosses a real sandbox boundary.

// --- wire shapes ------------------------------------------------------------------

// declaredFieldResp is one field of the schema as the Plugins screen receives it.
type declaredFieldResp struct {
	Key      string          `json:"key"`
	Type     string          `json:"type"`
	Label    string          `json:"label"`
	Help     string          `json:"help"`
	Required bool            `json:"required"`
	Default  json.RawMessage `json:"default"`
	Options  []string        `json:"options"`
	Min      *int            `json:"min"`
	Max      *int            `json:"max"`
}

type declaredSettingsResp struct {
	Values  map[string]any  `json:"values"`
	Secrets map[string]bool `json:"secrets"`
}

type pluginWithSettingsResp struct {
	ID             string                `json:"id"`
	SettingsSchema []declaredFieldResp   `json:"settingsSchema"`
	Settings       *declaredSettingsResp `json:"settings"`
}

type pluginsWithSettingsResp struct {
	Plugins []pluginWithSettingsResp `json:"plugins"`
}

type fieldRefusalResp struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Fields []struct {
				Key     string `json:"key"`
				Message string `json:"message"`
			} `json:"fields"`
		} `json:"details"`
	} `json:"error"`
}

// --- helpers ------------------------------------------------------------------------

func pluginSettingsPath(id string) string {
	return "/api/v1/settings/plugins/" + id + "/settings"
}

// readDeclaredSettings is the Plugins screen's own GET, decoded for the half this
// file is about.
func readDeclaredSettings(t *testing.T, srv *testharness.Server, token, id string) pluginWithSettingsResp {
	t.Helper()
	var resp pluginsWithSettingsResp
	status, body := srv.AuthGET("/api/v1/settings/plugins", token, &resp)
	if status != http.StatusOK {
		t.Fatalf("GET plugins = %d, want 200; body: %s", status, body)
	}
	for _, p := range resp.Plugins {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no plugin %q on the Plugins screen; got %+v", id, resp.Plugins)
	return pluginWithSettingsResp{}
}

// saveDeclaredSettings is the Save button, and it asserts nothing so the refusal
// tests can use it too.
func saveDeclaredSettings(t *testing.T, srv *testharness.Server, token, id string, values map[string]any) (int, []byte) {
	t.Helper()
	return srv.JSON(http.MethodPut, pluginSettingsPath(id), token,
		map[string]any{"values": values}, nil)
}

// installDeclaringGuest places a keyless Full music provider whose manifest
// declares one settings field of every type, in the mode that echoes what it read
// back through settings_get.
//
// Keyless on purpose: this manifest is the one that could not work at all before
// this slice, so the settings half and the active-fact half are proved by the
// same Plugin rather than by two that each avoid the other's problem.
func installDeclaringGuest(t *testing.T, dataDir, id, mode string) {
	t.Helper()
	m := plugintest.KeylessMetadataProviderManifest(id, pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
	})
	m.Settings.DefaultURL = guestBaseURL(mode)
	m.Settings.DefaultURL2 = guestImageHost
	m.Settings.Fields = plugintest.EverySettingsFieldType()
	plugintest.Install(t, dataDir, m)
}

// defaultSettings is what the fixture's manifest declares as defaults, which is
// what the API reports before anything has been saved: a value in force is a value
// in force whether an Admin typed it or an author shipped it.
func defaultSettings() map[string]any {
	return map[string]any{
		"adult":    false,
		"endpoint": "https://mirror.example.test",
	}
}

// validSettings is a document that satisfies every field the fixture declares,
// EXCEPT that it leaves `endpoint` out — so the manifest's declared default is
// what ends up stored, and the guest is what proves it.
func validSettings() map[string]any {
	return map[string]any{
		"account": "ripley",
		"token":   "sekrit",
		"adult":   true,
		"region":  "eu",
		"formats": []string{"srt", "vtt"},
		"retries": 4,
	}
}

// --- the schema, the save, and what the guest reads ---------------------------------

// TestADeclaredSettingsSchemaIsRenderedSavedAndReadBackByTheGuest is the first
// acceptance criterion end to end: a manifest declaring one field of each type
// reaches the Plugins screen as a schema, an Admin's values are saved through the
// API, the secret never comes back, and the GUEST reads exactly those values —
// in the JSON types it declared — from inside the sandbox.
func TestADeclaredSettingsSchemaIsRenderedSavedAndReadBackByTheGuest(t *testing.T) {
	requireMusicFixtures(t)
	dataDir := t.TempDir()
	installDeclaringGuest(t, dataDir, "example-source", "echo-settings")

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)

	// The schema is on the screen, with the facts a form needs to draw a control.
	before := readDeclaredSettings(t, srv, token, "example-source")
	if len(before.SettingsSchema) != len(pluginapi.AllSettingsFieldTypes()) {
		t.Fatalf("schema has %d fields, want one of each declared type; got %+v",
			len(before.SettingsSchema), before.SettingsSchema)
	}
	byKey := map[string]declaredFieldResp{}
	types := map[string]bool{}
	for _, f := range before.SettingsSchema {
		byKey[f.Key] = f
		types[f.Type] = true
	}
	for _, want := range pluginapi.AllSettingsFieldTypes() {
		if !types[string(want)] {
			t.Errorf("no declared field of type %q reached the screen", want)
		}
	}
	if got := byKey["region"]; len(got.Options) != 3 || !got.Required {
		t.Errorf("the enum reached the screen as %+v, want its three options and required", got)
	}
	if got := byKey["retries"]; got.Min == nil || *got.Min != 1 || got.Max == nil || *got.Max != 10 {
		t.Errorf("the integer reached the screen as %+v, want min 1 and max 10", got)
	}
	if got := byKey["endpoint"]; string(got.Default) != `"https://mirror.example.test"` {
		t.Errorf("the url field's default reached the screen as %s, want the manifest's", got.Default)
	}
	// Nothing is saved yet, so what the screen shows is exactly the DEFAULTS the
	// manifest declared — which is what the guest would read, and therefore the
	// only honest thing to put in the boxes.
	if before.Settings == nil || !jsonEqual(before.Settings.Values, defaultSettings()) {
		t.Errorf("before any save the screen shows %+v, want the manifest's declared defaults %+v",
			before.Settings, defaultSettings())
	}
	if before.Settings.Secrets["token"] {
		t.Error("the screen says a secret is on file before anything was saved")
	}

	// Save.
	if status, body := saveDeclaredSettings(t, srv, token, "example-source", validSettings()); status != http.StatusOK {
		t.Fatalf("saving the settings = %d, want 200; body: %s", status, body)
	}

	// What comes back: every value in its own JSON type, the DEFAULT for the field
	// that was left out, and the secret reported as set and never returned.
	after := readDeclaredSettings(t, srv, token, "example-source")
	want := map[string]any{
		"account":  "ripley",
		"adult":    true,
		"endpoint": "https://mirror.example.test",
		"formats":  []any{"srt", "vtt"},
		"region":   "eu",
		"retries":  float64(4),
	}
	if !jsonEqual(after.Settings.Values, want) {
		t.Errorf("saved values = %#v, want %#v", after.Settings.Values, want)
	}
	if _, leaked := after.Settings.Values["token"]; leaked {
		t.Fatalf("the secret came back in a response: %#v", after.Settings.Values)
	}
	if !after.Settings.Secrets["token"] {
		t.Error("the screen does not say a secret is on file, so the form cannot show 'configured'")
	}

	// And the guest reads them. It echoes settings_get's `values` document into the
	// record's overview, so what lands on a Track is what crossed the boundary —
	// secret included, because a secret reaches a guest INSIDE a call and nowhere
	// else.
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "example-source", "enabled": true},
	}}, http.StatusOK)
	putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "example-source"}, http.StatusOK)
	enrichLib(t, srv, token, libID, "full")

	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	var got map[string]any
	overview := getEnrichedDetail(t, srv, token, trackID).Overview
	if err := json.Unmarshal([]byte(overview), &got); err != nil {
		t.Fatalf("the guest's overview is not the settings document it read: %q (%v)", overview, err)
	}
	wantInGuest := map[string]any{
		"account":  "ripley",
		"adult":    true,
		"endpoint": "https://mirror.example.test",
		"formats":  []any{"srt", "vtt"},
		"region":   "eu",
		"retries":  float64(4),
		"token":    "sekrit",
	}
	if !jsonEqual(got, wantInGuest) {
		t.Errorf("the guest read %#v through settings_get, want %#v", got, wantInGuest)
	}
}

// TestADeclaredSettingsSaveIsRefusedFieldByField is the second criterion: a
// required field left empty, an enum value outside the declared set and an
// integer out of range are each refused with a message NAMING THE FIELD — and the
// refusal is structured, so a form can put each sentence under the control that
// caused it rather than one line above the whole panel.
func TestADeclaredSettingsSaveIsRefusedFieldByField(t *testing.T) {
	dataDir := t.TempDir()
	installDeclaringGuest(t, dataDir, "example-source", "echo-settings")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)

	cases := []struct {
		name     string
		mutate   func(map[string]any)
		wantKey  string
		wantText string
	}{
		{
			name:     "a required field left empty",
			mutate:   func(v map[string]any) { v["region"] = "" },
			wantKey:  "region",
			wantText: "Region is required",
		},
		{
			name:     "an enum value outside the declared set",
			mutate:   func(v map[string]any) { v["region"] = "antarctica" },
			wantKey:  "region",
			wantText: "Region must be one of eu, us, apac",
		},
		{
			name:     "an integer out of range",
			mutate:   func(v map[string]any) { v["retries"] = 99 },
			wantKey:  "retries",
			wantText: "Retries must be between 1 and 10",
		},
		{
			name:     "an element of a multi-select outside the declared set",
			mutate:   func(v map[string]any) { v["formats"] = []string{"srt", "pdf"} },
			wantKey:  "formats",
			wantText: `Formats may only contain srt, ass, vtt, and "pdf" is not one of them`,
		},
		{
			name:     "a url that is not one",
			mutate:   func(v map[string]any) { v["endpoint"] = "mirror.example.test" },
			wantKey:  "endpoint",
			wantText: "Mirror must be an absolute http or https URL",
		},
		{
			name:     "a key this plugin does not declare",
			mutate:   func(v map[string]any) { v["colour"] = "teal" },
			wantKey:  "colour",
			wantText: "this plugin declares no setting called colour",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := validSettings()
			tc.mutate(values)
			status, body := saveDeclaredSettings(t, srv, token, "example-source", values)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", status, body)
			}
			var refusal fieldRefusalResp
			if err := json.Unmarshal(body, &refusal); err != nil {
				t.Fatalf("decoding the refusal: %v; body: %s", err, body)
			}
			if refusal.Error.Code != "PLUGIN_INVALID_SETTINGS" {
				t.Errorf("code = %q, want PLUGIN_INVALID_SETTINGS", refusal.Error.Code)
			}
			// The envelope's own message names the field too, so a client that reads
			// only the envelope is still told something it can act on.
			if refusal.Error.Message != tc.wantText {
				t.Errorf("message = %q, want %q", refusal.Error.Message, tc.wantText)
			}
			found := false
			for _, f := range refusal.Error.Details.Fields {
				if f.Key == tc.wantKey && f.Message == tc.wantText {
					found = true
				}
			}
			if !found {
				t.Fatalf("no per-field refusal for %q; got %+v", tc.wantKey, refusal.Error.Details.Fields)
			}
		})
	}

	// And NOTHING was written by any of them: a half-saved form leaves an operator
	// unable to tell which half took. The screen is back to the declared defaults,
	// which is where it started.
	view := readDeclaredSettings(t, srv, token, "example-source")
	if !jsonEqual(view.Settings.Values, defaultSettings()) || view.Settings.Secrets["token"] {
		t.Fatalf("a refused save stored something: %+v", view.Settings)
	}
}

// TestADeclaredSecretSurvivesASaveThatDidNotMentionIt is the corollary of never
// returning a secret: a form that re-submits everything it can SEE must not clear
// the one field it cannot.
func TestADeclaredSecretSurvivesASaveThatDidNotMentionIt(t *testing.T) {
	dataDir := t.TempDir()
	installDeclaringGuest(t, dataDir, "example-source", "echo-settings")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	token := adminToken(t, srv)

	if status, body := saveDeclaredSettings(t, srv, token, "example-source", validSettings()); status != http.StatusOK {
		t.Fatalf("first save = %d, want 200; body: %s", status, body)
	}
	// The second save carries no token at all — exactly what a form whose masked
	// box was left untouched sends. The required-secret rule is satisfied by the
	// one on file.
	second := validSettings()
	delete(second, "token")
	second["account"] = "hicks"
	if status, body := saveDeclaredSettings(t, srv, token, "example-source", second); status != http.StatusOK {
		t.Fatalf("second save = %d, want 200; body: %s", status, body)
	}
	view := readDeclaredSettings(t, srv, token, "example-source")
	if !view.Settings.Secrets["token"] {
		t.Fatal("a save that did not mention the secret cleared it")
	}
	if view.Settings.Values["account"] != "hicks" {
		t.Errorf("account = %v, want the value the second save carried", view.Settings.Values["account"])
	}

	// An explicit null is how it IS cleared, which is the only way a form can
	// unset something it was never shown.
	third := validSettings()
	third["token"] = nil
	status, body := saveDeclaredSettings(t, srv, token, "example-source", third)
	// The field is required, so clearing it is refused BY NAME rather than
	// silently ignored — which is the honest answer to "unset the thing you told
	// me I must fill in".
	if status != http.StatusBadRequest {
		t.Fatalf("clearing a required secret = %d, want 400; body: %s", status, body)
	}
	if view := readDeclaredSettings(t, srv, token, "example-source"); !view.Settings.Secrets["token"] {
		t.Fatal("a refused clear removed the secret anyway")
	}
}

// TestPluginSettingsAreAdminOnly: the same 403 the whole /settings/ subtree gives
// a Member. This is not a special surface with a special rule.
func TestPluginSettingsAreAdminOnly(t *testing.T) {
	dataDir := t.TempDir()
	installDeclaringGuest(t, dataDir, "example-source", "echo-settings")

	srv := testharness.New(t, testharness.WithDataDir(dataDir))
	adminToken(t, srv)
	srv.CreateMember("ripley", "correct-horse-battery")
	member := srv.LoginAs("ripley", "correct-horse-battery")

	status, body := saveDeclaredSettings(t, srv, member, "example-source", validSettings())
	if status != http.StatusForbidden {
		t.Fatalf("a Member saving plugin settings = %d, want 403; body: %s", status, body)
	}
}

// --- the active fact ----------------------------------------------------------------

// TestAKeylessInstalledFullProviderCanLeadALibrary is the hole issue 04 named and
// issue 11 worked around, closed.
//
// Before this slice a manifest declaring `requiresSecret: false` produced a
// provider that was registered, configurable, offered in the Authoritative-
// provider dropdown — and never composed, because every gate asked whether its key
// was non-empty and a keyless source has none to give. The explicit active fact is
// what a keyless provider answers with instead.
//
// The negative from issue 11 is asserted in the SAME test and against the same
// server, because "a keyless provider is now active" must not have been bought by
// letting an unkeyed key-requiring one through.
func TestAKeylessInstalledFullProviderCanLeadALibrary(t *testing.T) {
	requireMusicFixtures(t)
	dataDir := t.TempDir()
	installDeclaringGuest(t, dataDir, "keyless-source", "")
	// Its twin, identical but for the one declaration under test.
	installGuestProvider(t, dataDir, "keyed-source", "", pluginapi.ManifestProvides{
		Kinds: []string{pluginapi.KindMusic},
		Role:  pluginapi.RoleAuthoritative,
		Class: pluginapi.ClassFull,
	})

	srv := testharness.New(t,
		testharness.WithDataDir(dataDir),
		testharness.WithArtworkFetcher(&fakeFetcher{data: []byte("x")}),
	)
	token := adminToken(t, srv)
	libID := createMusicLibrary(t, srv, token, musicRoot(t))
	scanLib(t, srv, token, libID, "")

	// The settings screen agrees about which of them needs a credential.
	if providerBySlug(getProviders(t, srv, token), "keyless-source").RequiresKey {
		t.Error("a manifest declaring requiresSecret false still asks for a key")
	}
	if !providerBySlug(getProviders(t, srv, token), "keyed-source").RequiresKey {
		t.Fatal("the key-requiring twin does not ask for a key; the negative would prove nothing")
	}

	// The negative, unchanged: an unkeyed key-requiring Plugin is not offered.
	policy := getPolicy(t, srv, token, libID)
	if hasAuthoritativeCandidate(policy, "keyed-source") {
		t.Error("an unkeyed key-requiring Installed plugin was offered as a lead")
	}
	if !hasAuthoritativeCandidate(policy, "keyless-source") {
		t.Fatalf("a keyless Full provider is not offered as a lead; candidates: %+v", policy.AuthoritativeCandidates)
	}

	// Switch it on — no key, because it needs none — and point the Library at it.
	putProviders(t, srv, token, map[string]any{"providers": []map[string]any{
		{"slug": "keyless-source", "enabled": true},
	}}, http.StatusOK)
	policy = putPolicy(t, srv, token, libID, map[string]any{"authoritativeProvider": "keyless-source"}, http.StatusOK)
	if policy.EffectiveAuthoritative.Slug != "keyless-source" {
		t.Fatalf("effective lead = %q, want the keyless Installed plugin", policy.EffectiveAuthoritative.Slug)
	}
	// THE ASSERTION THIS ISSUE EXISTS FOR. Before the active fact this was false,
	// and the Library enriched nothing while every screen said it was configured.
	if !policy.Effective.Music {
		t.Fatalf("music is off for a Library led by an enabled keyless provider: %+v", policy.Effective)
	}

	enrichLib(t, srv, token, libID, "full")
	trackID := firstTrackID(t, srv, token, libID)
	if trackID == "" {
		t.Skip("no tracks in music fixture")
	}
	d := getEnrichedDetail(t, srv, token, trackID)
	if d.EnrichmentStatus != "matched" {
		t.Errorf("track status = %q, want matched — the keyless lead was not composed", d.EnrichmentStatus)
	}
	if d.Overview != guestOverview {
		t.Errorf("track overview = %q, want the guest's record", d.Overview)
	}
}

// jsonEqual compares two decoded JSON documents. It goes through the encoder
// rather than reflect.DeepEqual because what is being compared IS the JSON — the
// point of half these assertions is that 4 stayed a number and a multi-select
// stayed an array.
func jsonEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}
