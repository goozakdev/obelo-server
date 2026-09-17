// Package schemagen generates the Plugin contract's JSON schema from the Go wire
// structs in pluginapi/v1, so the schema an author in another language reads and
// the Go package a Built-in compiles against cannot disagree.
//
// It lives under internal/ of the contract package itself, for one reason: the
// contract package must keep importing NOTHING but the standard library (ADR-0057
// decision 2), and a reflection-based generator wants reflect, sort and a JSON
// writer of its own. Generation is therefore a build-time act of a neighbouring
// package, not a runtime capability of the contract.
//
// Regenerate with:
//
//	go generate ./pluginapi/v1
//
// which runs cmd/schemagen and rewrites pluginapi/v1/pluginapi.schema.json. A
// test in the contract package regenerates in memory and fails if the checked-in
// file is stale, so the file cannot drift from the structs.
//
// # Why this and not an off-the-shelf reflector
//
// Three of the contract's rules are not expressible by reflecting over struct
// tags alone, and all three are rules a Plugin author has to obey:
//
//   - An EventActor carries a user id OR a link id, never both (ADR-0054). Both
//     absent is legal — an unattributable session names no one.
//   - A SinkEvent carries a scan block or an enrich block, never both.
//   - Settings.rateLimitMillis is a POINTER, so absent ("use your own default
//     pacing") and 0 ("do not throttle at all") are two different instructions.
//     A generator that emitted `"minimum": 1` or made the field required would
//     erase the distinction the Go type exists to preserve.
//
// So the type list, the enums and those constraints are stated explicitly in
// spec.go and the reflector fills in the shapes.
package schemagen

import (
	"fmt"
	"reflect"
	"strings"
)

// Draft is the JSON Schema dialect the generated document declares.
const Draft = "https://json-schema.org/draft/2020-12/schema"

// ID is the schema's identity. It is a URN and NOT a URL: this repository
// publishes no schema host, and a URL that 404s is a worse promise than a name
// that never claimed to be fetchable. An author references one type as
// "urn:obelo:pluginapi:v1#/$defs/SinkEvent".
const ID = "urn:obelo:pluginapi:v1"

// typeSpec is one wire struct that becomes a $defs entry.
type typeSpec struct {
	// value is a zero value of the struct, which is what the reflector walks.
	value any
	// doc is the one-line description an author reads instead of the Go comment.
	doc string
	// constraints are the schema keywords this type carries beyond its fields —
	// today only the two mutual-exclusion rules, as a "not"/"required" pair.
	constrain func(*object)
}

// enumSpec is one closed set of string values. The rule for what becomes an enum
// is deliberately mechanical: a set is an enum exactly when the contract package
// declares Go CONSTANTS for its values. So Outcome, Capability, ExtensionPoint,
// Role, Class, the five event types and the two media kinds are enums, while
// "poster | background | logo | cover" and the fine entity kinds — documented in
// prose, with no constants behind them — are plain strings. Inventing a
// constraint the Go package does not state would make the schema stricter than
// the contract.
type enumSpec struct {
	name   string
	doc    string
	values []string
	// goType is the named Go type whose fields should $ref this enum, or nil for
	// an enum reached only through an explicit field override (the event types and
	// the media kinds are plain `string` on the wire).
	goType reflect.Type
}

type generator struct {
	types  []typeSpec
	enums  []enumSpec
	fields map[string]any

	byName map[string]bool
	byEnum map[reflect.Type]string
}

// Generate returns the schema document, indented and newline-terminated, exactly
// as it is checked in beside the package.
func Generate() ([]byte, error) {
	g := &generator{
		types:  contractTypes(),
		enums:  contractEnums(),
		fields: fieldOverrides(),
		byName: map[string]bool{},
		byEnum: map[reflect.Type]string{},
	}
	for _, ts := range g.types {
		g.byName[reflect.TypeOf(ts.value).Name()] = true
	}
	for _, es := range g.enums {
		if es.goType != nil {
			g.byEnum[es.goType] = es.name
		}
	}

	defs := obj()
	for _, es := range g.enums {
		defs.set(es.name, obj().
			set("type", "string").
			setIf("description", es.doc).
			set("enum", es.values))
	}
	for _, ts := range g.types {
		t := reflect.TypeOf(ts.value)
		schema, err := g.structSchema(t)
		if err != nil {
			return nil, err
		}
		if ts.doc != "" {
			// description first, so the reader meets the sentence before the fields.
			schema = obj().set("type", "object").set("description", ts.doc).merge(schema)
		}
		if ts.constrain != nil {
			ts.constrain(schema)
		}
		defs.set(t.Name(), schema)
	}

	root := obj().
		set("$schema", Draft).
		set("$id", ID).
		set("title", "Obelo Plugin contract v1").
		set("description", schemaDoc).
		set("$defs", defs)

	return encodeIndented(root)
}

// merge folds another object's keys into o, keeping o's order for keys it already
// has. It exists only so a type's description can precede its "properties".
func (o *object) merge(other *object) *object {
	for i, k := range other.keys {
		o.set(k, other.vals[i])
	}
	return o
}

func (g *generator) structSchema(t reflect.Type) (*object, error) {
	props := obj()
	var required []string
	if err := g.collect(t, t.Name(), props, &required); err != nil {
		return nil, err
	}
	out := obj().set("type", "object").set("properties", props)
	if len(required) > 0 {
		out.set("required", required)
	}
	// additionalProperties is deliberately LEFT OPEN — see schemaDoc.
	return out, nil
}

// collect walks a struct's exported fields in declaration order, flattening an
// embedded struct that carries no json name (Page, whose limit and offset are
// flat fields on every request that pages).
func (g *generator) collect(t reflect.Type, owner string, props *object, required *[]string) error {
	if t.Kind() != reflect.Struct {
		return fmt.Errorf("schemagen: %s is not a struct", t)
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: not on the wire
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				if err := g.collect(ft, owner, props, required); err != nil {
					return err
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}

		var schema any
		if override, ok := g.fields[owner+"."+name]; ok {
			schema = override
		} else {
			s, err := g.typeSchema(f.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", owner, name, err)
			}
			schema = s
		}
		props.set(name, schema)

		// A field is required exactly when the Go marshaler always writes it: no
		// omitempty, no omitzero. That keeps "what the schema demands" and "what the
		// Go struct emits" the same fact rather than two opinions.
		if !hasOpt(opts, "omitempty") && !hasOpt(opts, "omitzero") {
			*required = append(*required, name)
		}
	}
	return nil
}

func hasOpt(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}

func (g *generator) typeSchema(t reflect.Type) (any, error) {
	// A pointer is absence, not null: every pointer field in this contract is
	// omitempty, so nil means the key is missing and a present key is the pointee's
	// own shape. Nothing here ever marshals to JSON null.
	if t.Kind() == reflect.Pointer {
		return g.typeSchema(t.Elem())
	}
	if name, ok := g.byEnum[t]; ok {
		return ref(name), nil
	}
	switch t.Kind() {
	case reflect.String:
		return obj().set("type", "string"), nil
	case reflect.Bool:
		return obj().set("type", "boolean"), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return obj().set("type", "integer"), nil
	case reflect.Float32, reflect.Float64:
		return obj().set("type", "number"), nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			// encoding/json writes a []byte as base64, which is how a subtitle file
			// crosses this contract whole (ADR-0057 decision 2: no streaming).
			return obj().set("type", "string").set("contentEncoding", "base64"), nil
		}
		items, err := g.typeSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return obj().set("type", "array").set("items", items), nil
	case reflect.Struct:
		if !g.byName[t.Name()] {
			// A new wire struct has to be named in spec.go before it can be
			// generated, so adding one is a deliberate act with a description
			// attached rather than an anonymous shape appearing in the schema.
			return nil, fmt.Errorf("schemagen: struct %s is not listed in contractTypes()", t.Name())
		}
		return ref(t.Name()), nil
	}
	return nil, fmt.Errorf("schemagen: no JSON schema mapping for %s (kind %s)", t, t.Kind())
}

func ref(name string) *object { return obj().set("$ref", "#/$defs/"+name) }

// notBoth is the schema form of "these two keys are mutually exclusive": a
// document carrying both fails, a document carrying either or neither passes.
// Expressed with "not"/"required" rather than "oneOf" precisely because NEITHER
// must stay legal — an unattributable session names no actor at all, and a
// library.changed event carries no counts block.
func notBoth(a, b string) func(*object) {
	return func(o *object) {
		o.set("not", obj().set("required", []string{a, b}))
	}
}
