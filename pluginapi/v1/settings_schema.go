package v1

import "encoding/json"

// The manifest-declared settings SCHEMA (.scratch/plugin-system issue 13) — the
// second variant of the settings field, beside the fixed shape.
//
// # Two shapes, one field, and why both
//
// Settings is FIXED for every Plugin at every Extension point: enabled, one
// secret, one URL, an optional second URL, a sink's events, and the two
// host-resolved knobs. That shape is what every Built-in needs and it is what the
// provider dialog renders, so it stays exactly as it is and it stays the
// contract's default (ADR-0057 consequences).
//
// What it cannot express is a source with a knob nobody compiled this server
// around: a region code, a "fetch adult titles" switch, a minimum score, a list of
// sub-sources to include. A Built-in adds such a knob by adding a named field to
// this package and a control to a screen, because the maintainer wrote both. An
// Installed plugin's author has neither, so an Installed plugin DECLARES its own
// fields in its manifest and the web app renders a form from the declaration.
//
// The declared values travel beside the fixed ones, in Settings.Values, and a
// guest reads them back in the shape it declared. Nothing about the fixed shape
// moves, and a manifest that declares no fields behaves exactly as it did before
// this type existed.
//
// # The JSON encoding of each field type
//
// A declared value is ordinary JSON of the type the field named. There is no
// second encoding and no string-stuffing: an integer is a JSON number, not "7".
//
//	string        JSON string. Free text.
//	secret        JSON string. Handed to a guest ONLY inside a call, and never
//	              returned by the settings API — which reports only whether one is
//	              on file, exactly as it does for the fixed Secret.
//	url           JSON string. An absolute http(s) URL; anything else is refused
//	              by the host with a message naming the field.
//	bool          JSON true / false. Never "true".
//	enum          JSON string, and it must be one of Options.
//	multi-select  JSON array of strings, every element one of Options. An empty
//	              array is a legitimate value and is not the same as absent.
//	integer       JSON number with no fractional part, within Min/Max when either
//	              is declared.
//
// A field the Admin never filled is ABSENT from Values rather than present as a
// zero, unless the manifest declared a Default — in which case the default is
// what the host stores the first time the form is saved and what a guest reads.
// Absent and zero are different instructions here for the reason they are on
// Settings.RateLimitMillis, so a guest must distinguish a missing key from a
// present zero value.

// SettingsFieldType names the kind of control an Admin gets and the JSON shape of
// the value behind it. The set is closed: a field type the host cannot render is
// a field an Admin cannot fill, so adding one is a decision on this side rather
// than something a manifest can ask for.
type SettingsFieldType string

const (
	// FieldString is free text — a region code, an account name, a path prefix.
	FieldString SettingsFieldType = "string"
	// FieldSecret is free text the host must never hand back: an API key, a token,
	// a signing secret. Stored, masked on read, and handed to a guest only inside
	// a call (ADR-0058 decision 5), exactly as the fixed Secret is.
	FieldSecret SettingsFieldType = "secret"
	// FieldURL is an absolute http(s) URL. It is its own type rather than a
	// validated string so the host can refuse a typo at save time, where the
	// operator is still looking at the form, instead of at the first fetch.
	FieldURL SettingsFieldType = "url"
	// FieldBool is a switch.
	FieldBool SettingsFieldType = "bool"
	// FieldEnum is one of Options.
	FieldEnum SettingsFieldType = "enum"
	// FieldMultiSelect is any subset of Options, including none.
	FieldMultiSelect SettingsFieldType = "multi-select"
	// FieldInteger is a whole number, optionally bounded by Min and Max.
	FieldInteger SettingsFieldType = "integer"
)

// AllSettingsFieldTypes is every field type the contract defines, in declaration
// order. It exists for the same reason AllOutcomes does: a host-side switch over
// the set can be exhaustive by construction, and the generated schema's enum is
// the Go constants rather than a second list that can drift from them.
func AllSettingsFieldTypes() []SettingsFieldType {
	return []SettingsFieldType{
		FieldString,
		FieldSecret,
		FieldURL,
		FieldBool,
		FieldEnum,
		FieldMultiSelect,
		FieldInteger,
	}
}

// SettingsField is one setting a manifest declares: what it is called, what shape
// its value takes, what an Admin should be told about it, and what the host must
// refuse.
//
// Every constraint here is enforced by the HOST, at save time, against the file on
// disk — never by the guest and never from anything a guest said. That is the same
// rule ManifestNetwork states, for the same reason: a declaration is a claim about
// what the Plugin wants, and the host is what makes it true.
type SettingsField struct {
	// Key is the name the value is stored and read under. It is the guest's own
	// vocabulary — the host never interprets it — and it must be unique within a
	// manifest, non-empty, and made of lowercase letters, digits, dashes and
	// underscores, so that it is safe as a form field id and a database key.
	Key string `json:"key"`
	// Type is the field's shape (see the package comment for each one's JSON).
	Type SettingsFieldType `json:"type"`
	// Label is what the form shows beside the control. Empty means the host shows
	// the Key, which is a worse label than the author could have written and is
	// never a reason to refuse the Plugin.
	Label string `json:"label,omitempty"`
	// Help is one sentence under the control, in the author's own words.
	Help string `json:"help,omitempty"`
	// Required refuses a save that leaves this field empty. For a string, secret,
	// url or enum that means a non-empty value; for a multi-select, at least one
	// selection; for an integer, a number. It is NOT applied to a bool, because
	// false is an answer and "a required switch" would mean "a switch that must be
	// on", which a manifest should say with no switch at all.
	//
	// A required SECRET is satisfied by one already on file: the API never returns
	// a stored secret, so a form that re-submitted every field would otherwise
	// clear it on every save.
	Required bool `json:"required,omitempty"`
	// Default is the value used when the Admin has filled nothing in, as raw JSON
	// of this field's own type. Absent means there is no default and an unfilled
	// field is simply absent from Values. A default that does not satisfy this
	// field's own constraints is refused when the Plugin is loaded, because a
	// manifest that ships an invalid default ships a form nobody can save.
	Default json.RawMessage `json:"default,omitempty"`
	// Options is the closed set of values for an enum or a multi-select, and is
	// meaningless (and refused) for every other type. Order is the order the form
	// shows them in.
	Options []string `json:"options,omitempty"`
	// Min and Max bound an integer, inclusive, and are meaningless for every other
	// type. They are POINTERS because 0 is a perfectly ordinary bound and an int
	// whose zero meant "no bound" could not express "at least 0".
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// HasOption reports whether value is one of this field's declared Options. It is
// the enum-membership test the host applies on save and the multi-select test it
// applies to every element, kept here so the rule is stated once beside the
// declaration it enforces.
func (f SettingsField) HasOption(value string) bool {
	for _, o := range f.Options {
		if o == value {
			return true
		}
	}
	return false
}

// DisplayLabel is what a form shows beside the control: the author's Label, or the
// Key when they wrote none.
func (f SettingsField) DisplayLabel() string {
	if f.Label != "" {
		return f.Label
	}
	return f.Key
}
