package plugins_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/plugins/plugintest"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The manifest-declared settings schema, at the loader's own level
// (.scratch/plugin-system issue 13).
//
// The black-box half — the schema on the Plugins screen, a save refused by field,
// a secret never returned — is in internal/api/plugin_manifest_settings_test.go.
// What lives here is what only this package can be asked: which manifests are
// refused at LOAD for declaring a schema nobody could satisfy, exactly what the
// value column holds, and that the values really cross the sandbox boundary in
// the JSON types they were declared with.

// --- what a manifest may declare ------------------------------------------------

// TestAnUnsatisfiableSettingsSchemaIsRefusedAtLoad: a schema an Admin could never
// save is refused when the Plugin is read, not when the Save button is pressed.
// The person who can fix it is the author, and the person who would otherwise meet
// it is an operator at a button that never works.
func TestAnUnsatisfiableSettingsSchemaIsRefusedAtLoad(t *testing.T) {
	cases := []struct {
		name  string
		field pluginapi.SettingsField
		want  string
	}{
		{
			name:  "no key",
			field: pluginapi.SettingsField{Type: pluginapi.FieldString},
			want:  "has no key",
		},
		{
			name:  "a key that is not one",
			field: pluginapi.SettingsField{Key: "My Region", Type: pluginapi.FieldString},
			want:  "must be lowercase letters",
		},
		{
			name:  "restating a fixed setting",
			field: pluginapi.SettingsField{Key: "secret", Type: pluginapi.FieldSecret},
			want:  "restates a fixed setting",
		},
		{
			name:  "an unknown type",
			field: pluginapi.SettingsField{Key: "palette", Type: "colour-wheel"},
			want:  "unknown type",
		},
		{
			name:  "an enum with no options",
			field: pluginapi.SettingsField{Key: "region", Type: pluginapi.FieldEnum},
			want:  "declares no options",
		},
		{
			name:  "options on something that is not a choice",
			field: pluginapi.SettingsField{Key: "account", Type: pluginapi.FieldString, Options: []string{"a"}},
			want:  "declares options",
		},
		{
			name:  "bounds on something that is not a number",
			field: pluginapi.SettingsField{Key: "account", Type: pluginapi.FieldString, Min: intp(1)},
			want:  "declares a min or a max",
		},
		{
			name:  "a min above its max",
			field: pluginapi.SettingsField{Key: "retries", Type: pluginapi.FieldInteger, Min: intp(9), Max: intp(2)},
			want:  "no value could satisfy it",
		},
		{
			name: "a default its own rules refuse",
			field: pluginapi.SettingsField{
				Key: "region", Type: pluginapi.FieldEnum,
				Options: []string{"eu", "us"}, Default: json.RawMessage(`"antarctica"`),
			},
			want: "not one this server would accept",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			m := plugintest.MetadataProviderManifest("example-source", fullMusicProvides())
			m.Settings.Fields = []pluginapi.SettingsField{tc.field}
			plugintest.Install(t, dataDir, m)

			log := &logSink{}
			set := loadWithKV(t, dataDir, log, nil)
			st, ok := set.Status("example-source")
			if !ok {
				t.Fatal("the plugin vanished instead of being refused with a reason")
			}
			if !st.Disabled || !strings.Contains(st.LastError, tc.want) {
				t.Fatalf("status = %+v, want a refusal containing %q", st, tc.want)
			}
		})
	}
}

// TestADuplicateSettingsKeyIsRefused: two fields under one key would make the
// stored value ambiguous, and the primary key on plugin_settings would silently
// pick a winner.
func TestADuplicateSettingsKeyIsRefused(t *testing.T) {
	dataDir := t.TempDir()
	m := plugintest.MetadataProviderManifest("example-source", fullMusicProvides())
	m.Settings.Fields = []pluginapi.SettingsField{
		{Key: "region", Type: pluginapi.FieldString},
		{Key: "region", Type: pluginapi.FieldInteger},
	}
	plugintest.Install(t, dataDir, m)

	set := loadWithKV(t, dataDir, &logSink{}, nil)
	st, _ := set.Status("example-source")
	if !st.Disabled || !strings.Contains(st.LastError, "declared twice") {
		t.Fatalf("status = %+v, want the duplicate refusal", st)
	}
}

// --- what the column holds --------------------------------------------------------

// TestADeclaredValueIsStoredAsItsOwnJSON pins the storage encoding, because it is
// the one thing three layers agree on and none of them states twice: the column
// holds the field's value as JSON, canonically re-encoded, whatever its type.
func TestADeclaredValueIsStoredAsItsOwnJSON(t *testing.T) {
	fields := plugintest.EverySettingsFieldType()
	rows, errs := plugins.PrepareSettings(fields, submitted(map[string]string{
		"account":  `"ripley"`,
		"token":    `"sekrit"`,
		"endpoint": `"https://mirror.example.test"`,
		"adult":    `true`,
		"region":   `"eu"`,
		"formats":  `["srt","vtt"]`,
		"retries":  `4`,
	}), nil)
	if len(errs) > 0 {
		t.Fatalf("a valid document was refused: %+v", errs)
	}
	got := map[string]string{}
	secret := map[string]bool{}
	for _, r := range rows {
		got[r.Key] = r.Value
		secret[r.Key] = r.Secret
	}
	want := map[string]string{
		"account":  `"ripley"`,
		"token":    `"sekrit"`,
		"endpoint": `"https://mirror.example.test"`,
		"adult":    `true`,
		"region":   `"eu"`,
		"formats":  `["srt","vtt"]`,
		"retries":  `4`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the %s column holds %s, want %s", k, got[k], v)
		}
	}
	// Only the secret field is marked secret, and it is the marking — not a
	// cipher — that keeps it out of every response.
	if !secret["token"] {
		t.Error("the secret field was not stored as a secret")
	}
	if secret["account"] || secret["region"] {
		t.Error("an ordinary field was stored as a secret")
	}
}

// TestAnIntegerIsNotAStringAndNotAFraction: the two ways a number can arrive
// wrong, each refused by name rather than coerced. A truncated 7.5 would be a
// value the operator did not type.
func TestAnIntegerIsNotAStringAndNotAFraction(t *testing.T) {
	fields := []pluginapi.SettingsField{{Key: "retries", Type: pluginapi.FieldInteger, Label: "Retries"}}
	for _, raw := range []string{`"4"`, `4.5`} {
		_, errs := plugins.PrepareSettings(fields, submitted(map[string]string{"retries": raw}), nil)
		if len(errs) != 1 || !strings.Contains(errs[0].Message, "whole number") {
			t.Errorf("%s was accepted or misreported: %+v", raw, errs)
		}
	}
}

// TestAStoredValueTheManifestNoLongerAcceptsIsDropped: an author who narrows an
// enum leaves rows behind that the schema now refuses. They are NOT handed to the
// guest — a value of the wrong shape is worse than no value — and the next save is
// where the operator is told.
func TestAStoredValueTheManifestNoLongerAcceptsIsDropped(t *testing.T) {
	fields := []pluginapi.SettingsField{
		{Key: "region", Type: pluginapi.FieldEnum, Options: []string{"eu"}},
		{Key: "account", Type: pluginapi.FieldString},
	}
	values := plugins.SettingValues(fields, []store.PluginSetting{
		{Key: "region", Value: `"apac"`},
		{Key: "account", Value: `"ripley"`},
		{Key: "gone", Value: `"a field the manifest no longer declares"`},
	})
	if _, present := values["region"]; present {
		t.Errorf("a value the schema now refuses reached the guest: %#v", values)
	}
	if _, present := values["gone"]; present {
		t.Errorf("a row for an undeclared field reached the guest: %#v", values)
	}
	if values["account"] != "ripley" {
		t.Errorf("account = %#v, want the value that is still legal", values["account"])
	}
}

// TestASecretIsNeverInThePublicView is the masking rule, stated where it is
// implemented: the public document carries every other field's value and, for a
// secret, only whether there is one.
func TestASecretIsNeverInThePublicView(t *testing.T) {
	fields := plugintest.EverySettingsFieldType()
	values, secrets := plugins.PublicSettingValues(fields, []store.PluginSetting{
		{Key: "token", Value: `"sekrit"`, Secret: true},
		{Key: "account", Value: `"ripley"`},
	})
	if _, leaked := values["token"]; leaked {
		t.Fatalf("the secret is in the public view: %#v", values)
	}
	if !secrets["token"] {
		t.Error("the public view does not report that a secret is on file")
	}
	if values["account"] != "ripley" {
		t.Errorf("account = %#v, want the stored value", values["account"])
	}
}

// --- across the boundary -----------------------------------------------------------

// TestDeclaredSettingsReachTheGuestInTheTypesItDeclared is the criterion that
// matters from inside the sandbox: the guest asks settings_get and gets back its
// own vocabulary, with an integer still a number and a multi-select still an
// array — and nothing at all when the Plugin declares no fields.
func TestDeclaredSettingsReachTheGuestInTheTypesItDeclared(t *testing.T) {
	dataDir := t.TempDir()
	m := plugintest.MetadataProviderManifest("example-source", fullMusicProvides())
	m.Settings.Fields = plugintest.EverySettingsFieldType()
	plugintest.Install(t, dataDir, m)

	set := loadWithKV(t, dataDir, &logSink{}, nil)
	rows, errs := plugins.PrepareSettings(m.Settings.Fields, submitted(map[string]string{
		"account": `"ripley"`,
		"token":   `"sekrit"`,
		"adult":   `true`,
		"region":  `"eu"`,
		"formats": `["srt","vtt"]`,
		"retries": `4`,
	}), nil)
	if len(errs) > 0 {
		t.Fatalf("a valid document was refused: %+v", errs)
	}
	// The Manager does this after a save; here the test is the Manager.
	for _, p := range set.Plugins() {
		p.SetSettingValues(plugins.SettingValues(m.Settings.Fields, rows))
	}

	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true,
		URL:     "https://source.example.test/v1?obelo-mode=echo-settings",
	})
	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(resp.Record.Overview), &got); err != nil {
		t.Fatalf("the guest echoed %q, which is not the settings document: %v", resp.Record.Overview, err)
	}
	want := map[string]any{
		"account":  "ripley",
		"adult":    true,
		"endpoint": "https://mirror.example.test", // the manifest's declared default
		"formats":  []any{"srt", "vtt"},
		"region":   "eu",
		"retries":  float64(4),
		"token":    "sekrit", // secrets cross INSIDE a call, and only there
	}
	if !sameJSON(got, want) {
		t.Fatalf("the guest read %#v, want %#v", got, want)
	}
}

// TestAGuestThatDeclaresNoFieldsIsHandedNoValues: a Plugin written before the
// schema existed sees exactly what it saw before — no `values` key at all, rather
// than an empty object it would have to tell apart from one.
func TestAGuestThatDeclaresNoFieldsIsHandedNoValues(t *testing.T) {
	dataDir := t.TempDir()
	plugintest.Install(t, dataDir, plugintest.MetadataProviderManifest("example-source", fullMusicProvides()))

	set := loadWithKV(t, dataDir, &logSink{}, nil)
	provider, _ := providerFor(t, set, "example-source", pluginapi.Settings{
		Enabled: true,
		URL:     "https://source.example.test/v1?obelo-mode=echo-settings",
	})
	resp, err := provider.Lookup(context.Background(), pluginapi.LookupRequest{
		Ref: pluginapi.MediaRef{Kind: "artist", Title: "New Order"},
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if resp.Record.Overview != "null" {
		t.Fatalf("the guest read %q, want the absent document a plugin with no schema gets", resp.Record.Overview)
	}
}

// --- helpers --------------------------------------------------------------------------

func intp(v int) *int { return &v }

// submitted turns a table of raw JSON strings into the document the API hands the
// validator, so a test writes the JSON it means rather than Go values that have to
// be re-encoded to find out.
func submitted(raw map[string]string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		out[k] = json.RawMessage(v)
	}
	return out
}

func sameJSON(a, b any) bool {
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
