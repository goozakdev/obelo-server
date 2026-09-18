// Command schemagen writes the Plugin contract's JSON schema beside the contract
// package. It is run by the //go:generate directive in pluginapi/v1/doc.go:
//
//	go generate ./pluginapi/v1
//
// A test in the contract package regenerates the same bytes in memory and fails
// if the checked-in file differs, so forgetting to run this is caught by the
// suite rather than discovered by a Plugin author.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/goozakdev/obelo-server/pluginapi/v1/internal/schemagen"
)

func main() {
	out := flag.String("o", "pluginapi.schema.json", "path to write the schema to")
	flag.Parse()

	doc, err := schemagen.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "schemagen: wrote %s (%d bytes)\n", *out, len(doc))
}
