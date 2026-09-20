// The Plugin contract, as its own Go module (ADR-0057, ADR-0059 decision 9).
//
// It is separate from the server module for one reason: a plugin author writing
// in Go must be able to import the wire types WITHOUT pulling the server's
// dependency graph — wazero, SQLite, Tailscale and the rest — into a WebAssembly
// module that is allowed to import nothing but the standard library anyway.
//
// So this module has NO REQUIRES, and that is a property to preserve rather than
// an accident of today. pluginapi/v1 imports the standard library and nothing
// else (see v1/doc.go); the schema generator beside it does the same. A require
// line appearing here means a type in the contract grew a dependency, which is
// the mistake the package's doc comment exists to prevent.
module github.com/goozakdev/obelo-server/pluginapi

go 1.26
