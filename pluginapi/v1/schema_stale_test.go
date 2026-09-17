// This file is in the EXTERNAL test package on purpose. The staleness check has
// to run the generator, the generator reflects over the contract package, and a
// test inside package v1 importing it would be an import cycle. An external test
// package is the one place both sides are visible at once.
package v1_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/goozakdev/obelo-server/pluginapi/v1/internal/schemagen"
)

// TestCheckedInSchemaIsNotStale: regenerate in memory, compare byte for byte with
// the file.
//
// This test is the whole reason the checked-in schema can be trusted. A generated
// artifact that is committed and never re-checked is the exact shape CLAUDE.md
// records as having rotted once already — a guard that reports success because it
// is only looking at one half. Here the Go structs are the source and the file is
// the output, and the only way they can disagree is if this test fails.
func TestCheckedInSchemaIsNotStale(t *testing.T) {
	want, err := schemagen.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile("pluginapi.schema.json")
	if err != nil {
		t.Fatalf("read pluginapi.schema.json: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("pluginapi.schema.json is STALE — it no longer matches the Go wire structs.\n"+
			"Regenerate it with:\n\n    go generate ./pluginapi/v1\n\n"+
			"then review the diff: within v1 the schema may only change ADDITIVELY.\n"+
			"(checked-in %d bytes, generated %d bytes)", len(got), len(want))
	}
}
