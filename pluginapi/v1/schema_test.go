package v1

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The schema half of the contract's own suite. The Go package is a convenience;
// the SCHEMA is the contract (ADR-0057 decision 7), which means the two can never
// be allowed to disagree — and the way they would disagree is silently, because a
// Go author reads the structs and a Plugin author in another language reads the
// schema and neither is looking at the other.
//
// So every golden document the round-trip suite already pins is ALSO run through
// the checked-in schema here. A field renamed in Go fails the round-trip test; a
// field renamed in the schema fails the staleness test next door; and a schema
// that is merely too loose to notice either is caught by the three negative cases
// below, which assert that the rules the contract states in prose actually refuse
// a document.
//
// The validator is github.com/santhosh-tekuri/jsonschema/v6, a TEST-ONLY
// dependency. It is pure Go (no cgo, so CGO_ENABLED=0 linux/amd64 and
// darwin/arm64 are untouched — ADR-0006), it implements draft 2020-12 including
// the "not"/"required" form the two mutual-exclusion rules use, and it adds one
// module to the tree whose only other requirement, golang.org/x/text, is already
// in it. Writing a validator here instead was considered and rejected: a schema
// checked only by a checker this repo also wrote proves that the two agree, not
// that either is right.

const schemaFile = "pluginapi.schema.json"

// loadSchema compiles the CHECKED-IN file — deliberately not a freshly generated
// one. What a Plugin author downloads is the file, so the file is what the
// goldens are held against; whether the file still matches the structs is the
// separate question TestCheckedInSchemaIsNotStale asks.
func loadSchema(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	f, err := os.Open(schemaFile)
	if err != nil {
		t.Fatalf("open %s: %v", schemaFile, err)
	}
	defer f.Close()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse %s: %v", schemaFile, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaID, doc); err != nil {
		t.Fatalf("add %s: %v", schemaFile, err)
	}
	return c
}

// schemaID is the $id the generator writes. It is a URN and not a URL because
// this repository publishes no schema host; a reference to one type is
// "urn:obelo:pluginapi:v1#/$defs/SinkEvent".
const schemaID = "urn:obelo:pluginapi:v1"

func compileDef(t *testing.T, c *jsonschema.Compiler, def string) *jsonschema.Schema {
	t.Helper()
	sch, err := c.Compile(schemaID + "#/$defs/" + def)
	if err != nil {
		t.Fatalf("compile #/$defs/%s: %v", def, err)
	}
	return sch
}

func instance(t *testing.T, doc string) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("parse instance %s: %v", doc, err)
	}
	return v
}

// TestGoldenDocumentsValidateAgainstTheSchema: every document the round-trip
// suite pins is a document the schema accepts, under the $defs entry named after
// its Go type. This is the join between the two halves — the goldens are what the
// Go structs actually emit, so if they validate, the schema describes what this
// server sends and accepts rather than what someone believed it did.
func TestGoldenDocumentsValidateAgainstTheSchema(t *testing.T) {
	c := loadSchema(t)
	for _, tc := range wireCases() {
		t.Run(tc.name, func(t *testing.T) {
			def := reflect.TypeOf(tc.value).Name()
			if def == "" {
				t.Fatalf("wire case %q has an unnamed type %T", tc.name, tc.value)
			}
			if err := compileDef(t, c, def).Validate(instance(t, tc.golden)); err != nil {
				t.Fatalf("golden document is not valid against #/$defs/%s:\n%v\ndocument: %s", def, err, tc.golden)
			}
		})
	}
}

// TestEveryWireTypeInTheSchemaIsReachable: a $defs entry nothing can produce is a
// promise to an author that this server does not keep. Every struct listed in the
// generator has to be either exercised by a golden or referenced by a type that
// is, so a def cannot be added and then forgotten.
func TestEveryWireTypeInTheSchemaIsReachable(t *testing.T) {
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read %s: %v", schemaFile, err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", schemaFile, err)
	}

	covered := map[string]bool{}
	for _, tc := range wireCases() {
		covered[reflect.TypeOf(tc.value).Name()] = true
	}
	// Anything a covered def points at is reachable too, transitively.
	for grew := true; grew; {
		grew = false
		for name := range covered {
			for _, target := range refsIn(doc.Defs[name]) {
				if !covered[target] {
					covered[target] = true
					grew = true
				}
			}
		}
	}
	// Page is embedded rather than referenced: its limit and offset are flat keys
	// on every paging request, so no $ref ever points at it. It stays in the schema
	// because an author reading a request wants to know the paging shape is shared.
	covered["Page"] = true

	var orphans []string
	for name := range doc.Defs {
		if !covered[name] {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) > 0 {
		t.Fatalf("schema defines types no golden document reaches: %v\n"+
			"either add a wire case for it in contract_test.go or drop it from contractTypes()", orphans)
	}
}

func refsIn(raw json.RawMessage) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "$ref" {
					if s, ok := val.(string); ok {
						out = append(out, strings.TrimPrefix(s, "#/$defs/"))
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		walk(v)
	}
	return out
}

// TestSchemaRefusesAnActorThatIsBothAUserAndALink: ADR-0054's rule, as a
// constraint rather than a sentence. A sink event names a User of this server OR
// the Link a relayed session arrived over, never both — and a Plugin author reads
// the schema, not the Go comment, so the schema is where the rule has to live.
//
// Neither is legal, deliberately: a session the host cannot attribute carries no
// actor at all, because unattributed is a smaller lie than misattributed.
func TestSchemaRefusesAnActorThatIsBothAUserAndALink(t *testing.T) {
	sch := compileDef(t, loadSchema(t), "EventActor")

	if err := sch.Validate(instance(t, `{"userId":"user-1","linkId":"user-9","name":"both"}`)); err == nil {
		t.Fatal("an actor carrying BOTH a user id and a link id was accepted; " +
			"a relayed session must never also name a person (ADR-0054)")
	}
	for _, legal := range []string{
		`{"userId":"user-1","name":"brandon"}`,
		`{"linkId":"user-9","name":"Brandon's server"}`,
		`{"name":"unattributed"}`,
		`{}`,
	} {
		if err := sch.Validate(instance(t, legal)); err != nil {
			t.Errorf("legal actor %s was rejected: %v", legal, err)
		}
	}
}

// TestSchemaRefusesAnEventThatIsBothAScanAndAnEnrich: a scan and an enrichment
// pass are separate events even when one follows the other, so the two count
// blocks are mutually exclusive. As with the actor, NEITHER is legal — a
// library.changed event carries no counts at all.
func TestSchemaRefusesAnEventThatIsBothAScanAndAnEnrich(t *testing.T) {
	sch := compileDef(t, loadSchema(t), "SinkEvent")

	both := `{"id":"e-1","type":"scan.completed","at":"2026-09-16T12:00:00Z",` +
		`"scan":{"titlesFound":1,"filesFound":1},"enrich":{"total":1,"done":1,"matched":1,"unmatched":0}}`
	if err := sch.Validate(instance(t, both)); err == nil {
		t.Fatal("an event carrying BOTH a scan block and an enrich block was accepted")
	}
	for _, legal := range []string{
		`{"id":"e-1","type":"scan.completed","at":"2026-09-16T12:00:00Z","scan":{"titlesFound":0,"filesFound":0}}`,
		`{"id":"e-2","type":"enrich.completed","at":"2026-09-16T12:00:00Z","enrich":{"total":0,"done":0,"matched":0,"unmatched":0}}`,
		`{"id":"e-3","type":"library.changed","at":"2026-09-16T12:00:00Z"}`,
	} {
		if err := sch.Validate(instance(t, legal)); err != nil {
			t.Errorf("legal event %s was rejected: %v", legal, err)
		}
	}

	// The event type is a closed set, and the schema says so because the Go package
	// declares constants for it. An invented type is not a v1 event.
	if err := sch.Validate(instance(t, `{"id":"e-4","type":"scan.started","at":"2026-09-16T12:00:00Z"}`)); err == nil {
		t.Error("an event type outside the curated set was accepted")
	}
}

// TestRateLimitAbsentAndZeroAreBothValidAndNotTheSame: the one pointer in the
// contract, and the reason it is one. Absent is "use your own default pacing" and
// 0 is "do not throttle at all" (a self-hosted mirror with no rate policy) — two
// different operator instructions, neither of which is the other's zero.
//
// The schema must accept both documents, must NOT require the key, and must not
// exclude 0 from its range; and the Go decode has to keep them apart, which is
// the half a schema alone cannot promise. Both are asserted here, together,
// because the failure mode is that one of them quietly starts meaning the other.
func TestRateLimitAbsentAndZeroAreBothValidAndNotTheSame(t *testing.T) {
	sch := compileDef(t, loadSchema(t), "Settings")

	const absent = `{"enabled":true}`
	const zero = `{"enabled":true,"rateLimitMillis":0}`

	for _, doc := range []string{absent, zero, `{"enabled":true,"rateLimitMillis":1000}`} {
		if err := sch.Validate(instance(t, doc)); err != nil {
			t.Fatalf("%s was rejected by the schema: %v", doc, err)
		}
	}

	decode := func(doc string) *int {
		var s Settings
		if err := json.Unmarshal([]byte(doc), &s); err != nil {
			t.Fatalf("decode %s: %v", doc, err)
		}
		return s.RateLimitMillis
	}
	if got := decode(absent); got != nil {
		t.Errorf("an absent rateLimitMillis decoded to %d, want nil (use the host default)", *got)
	}
	if got := decode(zero); got == nil {
		t.Fatal("an explicit rateLimitMillis of 0 decoded to nil, collapsing 'no rate policy' into 'use the default'")
	} else if *got != 0 {
		t.Errorf("rateLimitMillis 0 decoded to %d", *got)
	}

	// A negative interval is not an instruction anyone can mean, and the freeze is
	// the last moment a constraint can be tightened.
	if err := sch.Validate(instance(t, `{"enabled":true,"rateLimitMillis":-1}`)); err == nil {
		t.Error("a negative rateLimitMillis was accepted")
	}
}

// TestSchemaToleratesAFieldItDoesNotKnow: additive-only evolution is only safe if
// a guest compiled against v1.0 keeps validating documents a later v1.x host
// sends, which is why additionalProperties is left OPEN everywhere. This test is
// what stops a future contributor from "tightening" the schema with
// additionalProperties: false and breaking every existing Plugin at once.
func TestSchemaToleratesAFieldItDoesNotKnow(t *testing.T) {
	c := loadSchema(t)
	cases := map[string]string{
		"SinkEvent": `{"id":"e-1","type":"library.changed","at":"2026-09-16T12:00:00Z","futureField":42}`,
		"Settings":  `{"enabled":true,"futureField":"whatever"}`,
		"MediaRef":  `{"kind":"movie","futureId":"x"}`,
	}
	for def, doc := range cases {
		if err := compileDef(t, c, def).Validate(instance(t, doc)); err != nil {
			t.Errorf("#/$defs/%s rejected an unknown field: %v\n%s", def, err, doc)
		}
	}
}

// TestSchemaDeclaresTheDraftAndItsIdentity keeps the two facts a downstream
// toolchain needs from drifting silently.
func TestSchemaDeclaresTheDraftAndItsIdentity(t *testing.T) {
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatalf("read %s: %v", schemaFile, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", schemaFile, err)
	}
	if got := fmt.Sprint(doc["$schema"]); got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %s", got)
	}
	if got := fmt.Sprint(doc["$id"]); got != schemaID {
		t.Errorf("$id = %s, want %s", got, schemaID)
	}
}
