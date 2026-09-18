package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The host side of the manifest-declared settings schema (.scratch/plugin-system
// issue 13): reading a manifest's field list, judging what an Admin typed against
// it, and turning the result into the rows `plugin_settings` holds and the
// document a guest reads through settings_get.
//
// # Where the judgment lives, and why it is here
//
// The manifest DECLARES; the host DECIDES. Every constraint — required, enum
// membership, integer range, the shape of a URL — is checked on this side, from
// the file on disk, at save time, before a byte is written. A guest is never asked
// whether a value is acceptable and has nothing in the contract to say so with,
// which is the same rule ManifestNetwork follows: a declaration is a claim about
// what the Plugin wants, and the host is what makes it true.
//
// Validating at SAVE rather than at call time is the difference between an
// operator reading "region must be one of eu, us, apac" next to the box they typed
// it in, and a scan quietly producing nothing three hours later.
//
// # One encoding, stated once
//
// A declared value is ordinary JSON of the type the field named (see
// pluginapi/v1/settings_schema.go). It is stored as that JSON, handed to a guest
// as that JSON, and rendered by the form as that JSON. There is no second spelling
// anywhere in the path, so nothing in it can disagree.

// FieldError is one refusal, named by the field it is about. The API answers with
// a list of these so a form can put each sentence under the control that caused
// it, which is the whole reason validation is server-side and structured rather
// than one prose sentence.
type FieldError struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// --- what a manifest may declare ----------------------------------------------

// fixedSettingKeys are the keys of the FIXED Settings shape. A declared field may
// not use one: two controls writing one idea is how an operator configures the
// wrong one, and the fixed half already has a dialog.
var fixedSettingKeys = map[string]struct{}{
	"enabled": {}, "secret": {}, "url": {}, "url2": {},
	"events": {}, "language": {}, "ratelimitmillis": {},
}

// validateSettingsFields checks a manifest's own field declarations, at load, with
// the rest of the manifest. A Plugin whose schema is unusable — a field with no
// key, an enum with no options, a default that its own rules refuse — is refused
// here rather than at the moment an Admin presses Save, because the person who can
// fix it is the author and the person who would meet it is not.
func validateSettingsFields(fields []pluginapi.SettingsField) error {
	seen := make(map[string]struct{}, len(fields))
	for i, f := range fields {
		where := fmt.Sprintf("settings field %d", i+1)
		if f.Key != "" {
			where = fmt.Sprintf("the settings field %q", f.Key)
		}
		if strings.TrimSpace(f.Key) == "" {
			return fmt.Errorf("%s has no key, and the key is how its value is stored and read", where)
		}
		if f.Key != settingKeyOf(f.Key) {
			return fmt.Errorf("%s must be lowercase letters, digits, dashes and underscores", where)
		}
		if _, fixed := fixedSettingKeys[strings.ToLower(f.Key)]; fixed {
			return fmt.Errorf("%s restates a fixed setting; the fixed shape already has a control for it", where)
		}
		if _, dup := seen[f.Key]; dup {
			return fmt.Errorf("%s is declared twice", where)
		}
		seen[f.Key] = struct{}{}

		if !knownFieldType(f.Type) {
			return fmt.Errorf("%s has the unknown type %q; this server knows %s",
				where, f.Type, fieldTypeList())
		}
		wantsOptions := f.Type == pluginapi.FieldEnum || f.Type == pluginapi.FieldMultiSelect
		switch {
		case wantsOptions && len(f.Options) == 0:
			return fmt.Errorf("%s is a %s and declares no options, so there is nothing to choose", where, f.Type)
		case !wantsOptions && len(f.Options) > 0:
			return fmt.Errorf("%s is a %s and declares options, which only an enum or a multi-select has", where, f.Type)
		}
		if f.Type != pluginapi.FieldInteger && (f.Min != nil || f.Max != nil) {
			return fmt.Errorf("%s is a %s and declares a min or a max, which only an integer has", where, f.Type)
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return fmt.Errorf("%s has a min above its max, so no value could satisfy it", where)
		}
		if len(f.Default) > 0 {
			if _, err := decodeFieldValue(f, f.Default); err != nil {
				return fmt.Errorf("the default of %s is not one this server would accept: %s", where, err.Error())
			}
		}
	}
	return nil
}

func knownFieldType(t pluginapi.SettingsFieldType) bool {
	for _, known := range pluginapi.AllSettingsFieldTypes() {
		if t == known {
			return true
		}
	}
	return false
}

func fieldTypeList() string {
	all := pluginapi.AllSettingsFieldTypes()
	out := make([]string, len(all))
	for i, t := range all {
		out[i] = string(t)
	}
	return strings.Join(out, ", ")
}

// settingKeyOf is the shape a field key must already have. Like slugOf it does not
// transform — a key this server renamed would be a key the guest asks for and
// never finds.
func settingKeyOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- judging what an Admin typed ------------------------------------------------

// PrepareSettings turns one submitted document into the rows to store, or into the
// list of field-level refusals that stopped it. Nothing is written unless every
// field passes: a half-saved form leaves an operator with no way to tell which
// half took.
//
// `submitted` is the raw JSON the Admin's form sent, keyed by field key. `stored`
// is what the Plugin already has, and it is what makes a SECRET survive a save
// that did not mention it — the API never returns a stored secret, so a form that
// re-submitted every field would otherwise clear the secret on every save.
//
// A key the manifest does not declare is REFUSED rather than ignored. A form
// sending one is a form built against a different version of this Plugin, and
// silently dropping it would store a document the operator believes they saved.
func PrepareSettings(fields []pluginapi.SettingsField, submitted map[string]json.RawMessage, stored []store.PluginSetting) ([]store.PluginSetting, []FieldError) {
	declared := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		declared[f.Key] = struct{}{}
	}
	var errs []FieldError
	unknown := make([]string, 0, len(submitted))
	for key := range submitted {
		if _, ok := declared[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		errs = append(errs, FieldError{Key: key, Message: "this plugin declares no setting called " + key})
	}

	have := make(map[string]store.PluginSetting, len(stored))
	for _, s := range stored {
		have[s.Key] = s
	}

	out := make([]store.PluginSetting, 0, len(fields))
	for _, f := range fields {
		raw, sent := submitted[f.Key]
		if sent && isJSONNull(raw) {
			// An explicit null is "unset this", which is the only way a form can clear a
			// secret it cannot see.
			sent = false
			delete(have, f.Key)
		}
		if !sent {
			// Keep what is on file; fall back to the manifest's default for a field that
			// has never been saved.
			if kept, ok := have[f.Key]; ok {
				out = append(out, kept)
				continue
			}
			if len(f.Default) > 0 {
				value, err := decodeFieldValue(f, f.Default)
				if err == nil {
					out = append(out, store.PluginSetting{Key: f.Key, Value: encodeFieldValue(value), Secret: f.Type == pluginapi.FieldSecret})
					continue
				}
			}
			if f.Required {
				errs = append(errs, FieldError{Key: f.Key, Message: requiredMessage(f)})
			}
			continue
		}

		value, err := decodeFieldValue(f, raw)
		if err != nil {
			errs = append(errs, FieldError{Key: f.Key, Message: err.Error()})
			continue
		}
		if f.Required && isEmptyValue(f, value) {
			errs = append(errs, FieldError{Key: f.Key, Message: requiredMessage(f)})
			continue
		}
		if isEmptyValue(f, value) && f.Type != pluginapi.FieldBool && f.Type != pluginapi.FieldInteger {
			// An emptied optional field is an absent one, not a stored empty string:
			// absent and zero are different instructions across this whole contract.
			continue
		}
		out = append(out, store.PluginSetting{Key: f.Key, Value: encodeFieldValue(value), Secret: f.Type == pluginapi.FieldSecret})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

// requiredMessage names the field in the operator's own words, with the label the
// author wrote where there is one, because "region is required" is readable and
// "field 3 is required" is not.
func requiredMessage(f pluginapi.SettingsField) string {
	if f.Type == pluginapi.FieldMultiSelect {
		return f.DisplayLabel() + " needs at least one selection"
	}
	return f.DisplayLabel() + " is required"
}

// decodeFieldValue reads one submitted JSON value as the Go value its field type
// names, applying every constraint the manifest declared. The error IS the message
// the form shows, so it names the field and says what would be acceptable rather
// than restating that something is wrong.
func decodeFieldValue(f pluginapi.SettingsField, raw json.RawMessage) (any, error) {
	label := f.DisplayLabel()
	switch f.Type {
	case pluginapi.FieldString, pluginapi.FieldSecret:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s must be text", label)
		}
		return s, nil

	case pluginapi.FieldURL:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s must be text", label)
		}
		if s == "" {
			return "", nil
		}
		u, err := url.Parse(s)
		if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("%s must be an absolute http or https URL", label)
		}
		return s, nil

	case pluginapi.FieldBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s must be true or false", label)
		}
		return b, nil

	case pluginapi.FieldEnum:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s must be text", label)
		}
		if s == "" {
			return "", nil
		}
		if !f.HasOption(s) {
			return nil, fmt.Errorf("%s must be one of %s", label, strings.Join(f.Options, ", "))
		}
		return s, nil

	case pluginapi.FieldMultiSelect:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%s must be a list of values", label)
		}
		for _, v := range list {
			if !f.HasOption(v) {
				return nil, fmt.Errorf("%s may only contain %s, and %q is not one of them",
					label, strings.Join(f.Options, ", "), v)
			}
		}
		if list == nil {
			list = []string{}
		}
		return list, nil

	case pluginapi.FieldInteger:
		// json.Number rather than int, so 7.5 is refused as "not a whole number"
		// instead of being silently truncated or reported as a type error.
		//
		// The quote check is not belt and braces. encoding/json DELIBERATELY accepts
		// a JSON string into a json.Number when its contents parse as one, so "4"
		// would arrive as 4 — and the contract says an integer is a JSON number.
		// Accepting the string spelling here would make the host lenient about the
		// one thing the schema exists to pin down.
		if trimmed := strings.TrimSpace(string(raw)); strings.HasPrefix(trimmed, `"`) {
			return nil, fmt.Errorf("%s must be a whole number, not text", label)
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("%s must be a whole number", label)
		}
		i, err := n.Int64()
		if err != nil {
			return nil, fmt.Errorf("%s must be a whole number", label)
		}
		switch {
		case f.Min != nil && f.Max != nil && (i < int64(*f.Min) || i > int64(*f.Max)):
			return nil, fmt.Errorf("%s must be between %d and %d", label, *f.Min, *f.Max)
		case f.Min != nil && i < int64(*f.Min):
			return nil, fmt.Errorf("%s must be at least %d", label, *f.Min)
		case f.Max != nil && i > int64(*f.Max):
			return nil, fmt.Errorf("%s must be at most %d", label, *f.Max)
		}
		return i, nil
	}
	return nil, fmt.Errorf("%s has a type this server does not render", label)
}

// isEmptyValue reports whether a decoded value is "nothing was filled in". A bool
// is never empty — false is an answer — and neither is an integer, which is why
// Required is not applied to a bool at all.
func isEmptyValue(f pluginapi.SettingsField, value any) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case []string:
		return len(v) == 0
	default:
		_ = f
		return false
	}
}

// encodeFieldValue is the one place a validated value becomes the text the column
// holds: its own JSON, canonically re-encoded from the decoded value rather than
// passed through, so the row never carries an operator's whitespace or a number
// spelled `7.0`.
func encodeFieldValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		// Every branch of decodeFieldValue returns a string, a bool, an int64 or a
		// []string, all of which marshal. Reaching here would be a bug in this file.
		return "null"
	}
	return string(raw)
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// --- what comes back out ---------------------------------------------------------

// SettingValues is what a GUEST reads through settings_get: every declared field
// that has a value, decoded into the JSON shape its type names, SECRETS INCLUDED.
// It is only ever handed into a call (see Plugin.settingValues), never to the API.
//
// A field with no row and no default is ABSENT rather than present as a zero, and
// a stored row whose field the manifest no longer declares is dropped: the manifest
// on disk is what says which settings exist.
func SettingValues(fields []pluginapi.SettingsField, rows []store.PluginSetting) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	have := make(map[string]store.PluginSetting, len(rows))
	for _, r := range rows {
		have[r.Key] = r
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		raw, ok := have[f.Key]
		source := json.RawMessage(raw.Value)
		if !ok || len(source) == 0 {
			if len(f.Default) == 0 {
				continue
			}
			source = f.Default
		}
		value, err := decodeFieldValue(f, source)
		if err != nil {
			// A stored value the manifest no longer accepts — the author retyped the
			// field, or narrowed an enum. It is dropped rather than handed over: a guest
			// reading a value of the wrong shape is worse than one reading nothing, and
			// the next save is where the operator is told.
			continue
		}
		out[f.Key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PublicSettingValues is what the settings API returns: the same document minus
// every secret field, plus a per-secret "there is one on file" boolean. A secret's
// VALUE is never in a response, exactly as the fixed Secret's is not — the form
// shows only whether one is set.
func PublicSettingValues(fields []pluginapi.SettingsField, rows []store.PluginSetting) (values map[string]any, secrets map[string]bool) {
	values = map[string]any{}
	secrets = map[string]bool{}
	all := SettingValues(fields, rows)
	for _, f := range fields {
		if f.Type == pluginapi.FieldSecret {
			s, _ := all[f.Key].(string)
			secrets[f.Key] = s != ""
			continue
		}
		if v, ok := all[f.Key]; ok {
			values[f.Key] = v
		}
	}
	return values, secrets
}

// --- the Manager's half ----------------------------------------------------------

// ApplySettings reads every Installed plugin's declared settings out of the
// database and onto the live Plugins, so a guest's first call already sees what an
// Admin saved before the last restart.
//
// It is exported because the composition root builds the Set itself, at boot, and
// hands it to NewManager — which performs no I/O and cannot fail, and is not the
// place to start doing either. Every later load goes through rebuild, which does
// this for itself.
func (m *Manager) ApplySettings() {
	m.applySettings(m.Plugins())
}

// applySettings stamps the stored values onto each Plugin of a Set. A read that
// fails is logged and skipped rather than propagated: a Plugin whose settings
// could not be read is a Plugin running on its manifest's defaults, which is a
// worse day than usual and not a reason to refuse to start (ADR-0001).
func (m *Manager) applySettings(set *Set) {
	if m.store == nil {
		return
	}
	for _, p := range set.Plugins() {
		fields := p.Manifest().Settings.Fields
		if len(fields) == 0 {
			p.SetSettingValues(nil)
			continue
		}
		rows, err := m.store.PluginSettings(p.ID())
		if err != nil {
			m.logf("obelo: could not read the settings of plugin %s: %v", p.ID(), err)
			continue
		}
		p.SetSettingValues(SettingValues(fields, rows))
	}
}

// SaveSettings validates one submitted settings document against the Plugin's OWN
// manifest and, if every field passes, writes it and publishes it to the guest.
//
// Three things about this, each of which is the issue:
//
//   - VALIDATION IS THE HOST'S, from the manifest on disk. A guest is never asked
//     whether a value is acceptable and has nothing to say so with; the refusal is
//     per field so the form can put each sentence under the control that caused it.
//   - NOTHING IS WRITTEN unless everything passes. A half-saved form leaves an
//     operator unable to tell which half took.
//   - A SECRET THE FORM DID NOT SEND IS KEPT. The API never returns a stored
//     secret, so a form re-submitting every field it can see would otherwise clear
//     the one field it cannot. Sending an explicit null is how a secret is cleared.
//
// It does NOT rebuild-and-swap. The declared values are read at call time
// (Plugin.withSettingValues), so a save takes effect on the next call into the
// guest, and re-compiling every module to change one string would be a restart
// wearing a different name.
func (m *Manager) SaveSettings(ctx context.Context, id string, submitted map[string]json.RawMessage) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = ctx

	if err := m.mustBeInstalled(id); err != nil {
		return Installed{}, err
	}
	p := m.plugin(id)
	if p == nil {
		return Installed{}, refuse(ReasonUnknown,
			"the plugin %q is installed but did not load, so there is nothing to configure yet", id)
	}
	fields := p.Manifest().Settings.Fields
	if len(fields) == 0 {
		return Installed{}, refuse(ReasonSettings,
			"the plugin %q declares no settings of its own; what it does is configured on the screen for its extension point", id)
	}
	if m.store == nil {
		return Installed{}, refuse(ReasonSettings, "this server cannot remember plugin settings")
	}
	// A Plugin an operator placed by hand has files and no row; its settings are
	// worth as much as an uploaded one's, and a save is a switch being flipped.
	if err := m.ensureRow(id); err != nil {
		return Installed{}, err
	}
	stored, err := m.store.PluginSettings(id)
	if err != nil {
		return Installed{}, err
	}
	rows, fieldErrs := PrepareSettings(fields, submitted, stored)
	if len(fieldErrs) > 0 {
		return Installed{}, &Refusal{
			Reason:  ReasonSettings,
			Message: fieldErrs[0].Message,
			Fields:  fieldErrs,
		}
	}
	if err := m.store.ReplacePluginSettings(id, rows); err != nil {
		return Installed{}, err
	}
	p.SetSettingValues(SettingValues(fields, rows))
	m.logf("obelo: plugin %s: settings saved (%d of %d declared fields hold a value)", id, len(rows), len(fields))
	return m.view(id)
}

// declaredFields is one Plugin's declared settings schema, from the manifest on
// disk. Empty for a Plugin that did not load — there is no file this server has
// read that says what it wants, and inventing one would put a form on screen that
// nothing will ever answer.
func (m *Manager) declaredFields(id string) []pluginapi.SettingsField {
	p := m.plugin(id)
	if p == nil {
		return nil
	}
	return p.Manifest().Settings.Fields
}

// settingRows is one Plugin's stored settings, or none when they cannot be read. A
// listing degrades to "nothing is configured" rather than failing: the Plugins
// screen is where an Admin goes when something is already wrong.
func (m *Manager) settingRows(id string) []store.PluginSetting {
	if m.store == nil {
		return nil
	}
	rows, err := m.store.PluginSettings(id)
	if err != nil {
		m.logf("obelo: could not read the settings of plugin %s: %v", id, err)
		return nil
	}
	return rows
}
