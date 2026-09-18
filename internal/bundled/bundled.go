// Package bundled is the seven shipped metadata providers, as WebAssembly
// modules carried inside the binary (ADR-0059 decisions 1-3).
//
// A Bundled plugin is an Installed plugin the server shipped with. It is placed
// under <dataDir>/plugins/<id>/ on first boot exactly as an Admin's upload would
// be, it is re-asserted on every boot after that, and from the moment it is on
// disk NOTHING downstream can tell it from a plugin somebody uploaded: same
// sandbox, same allowlist, same contract, same lifecycle, same screen. The only
// difference an operator sees is one sentence — "Shipped with Obelo" where an
// Admin's plugin names the file or URL it came from — and the one thing that
// sentence buys them: a "reinstall the shipped version" button after they remove
// it.
//
// # What is embedded, and what is not
//
// modules/ holds one gzip-compressed module and one manifest per bundled id,
// built from plugins/<id>/ by `make plugins`. IT IS GITIGNORED. A .wasm is build
// output, it is ~1 MB compressed, and committing one would make every plugin
// change a binary diff nobody can review — `make check-no-bundled-modules-tracked`
// fails if one is ever added. The committed .keep is what keeps the directory in
// the tree so the embed pattern below has something to match on a fresh clone.
//
// A clone that has not run `make plugins` therefore COMPILES and fails
// [TestEveryBundledModuleIsPresent] with the sentence "run make plugins" — a loud,
// specific failure rather than a server that quietly ships no providers.
//
// # The order
//
// [IDs] is the shipped registration order, and it is the whole of what decides
// which source leads a kind. The first authoritative Full provider of a kind is
// that kind's default lead (ADR-0027), the settings screen lists sources in
// registration order, and the music chain's two fill-only slots are filled in it —
// so TMDB leading video and MusicBrainz leading music is a property of this list
// and of nothing else. It is the full seven from the first slice, even while most
// of them are still Built-ins, because the ORDER is decided once and the modules
// arrive one issue at a time.
package bundled

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sync"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The embedded modules and manifests, written by `make plugins`.
//
// `all:` rather than a bare `modules/*`, and that is not a style choice: the
// embed patterns skip names beginning with a dot unless the prefix is there, and
// on a fresh clone the ONLY file in this directory is the committed `.keep`. A
// bare pattern would then match nothing, which is a COMPILE error — so a clone
// that had not run `make plugins` would fail to build at all instead of failing
// the one test that explains what to do.
//
//go:embed all:modules
var modules embed.FS

// The file names inside modules/. One module and one manifest per id, named after
// the id so a directory listing reads as the list of what this binary ships.
const (
	moduleSuffix   = ".wasm.gz"
	manifestSuffix = ".manifest.json"
)

// ids is the ordered list of Bundled plugin ids — today's registration order,
// preserved exactly (see the package comment for why the order is load-bearing).
//
// It is the full seven from the first slice even though issue 04 only ships the
// first module: ADR-0059 decision 3 asks for the order to be decided once, and an
// id with no module yet is simply an id [Manifests] has nothing for. Issues 05-07
// add modules, not entries.
var ids = []string{
	"tmdb",
	"omdb",
	"thetvdb",
	"anidb",
	"musicbrainz",
	"fanarttv",
	"theaudiodb",
}

// IDs is the ordered list of Bundled plugin ids. The slice is a copy.
func IDs() []string {
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// Present is the ordered ids this binary actually carries a module AND a manifest
// for. It is what the composition root asserts and what the loader orders by; an
// id in [IDs] with nothing embedded is one issues 05-07 have not reached yet.
func Present() []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if has(id) {
			out = append(out, id)
		}
	}
	return out
}

// Missing is the ordered ids with no embedded module or no embedded manifest.
// Empty is the shipped state; anything else is a tree that has not run
// `make plugins`, or a plugin whose module failed to build.
func Missing() []string {
	var out []string
	for _, id := range ids {
		if !has(id) {
			out = append(out, id)
		}
	}
	return out
}

func has(id string) bool {
	if _, ok := supplied.Load(id); ok {
		return true
	}
	for _, name := range []string{id + moduleSuffix, id + manifestSuffix} {
		if _, err := fs.Stat(modules, "modules/"+name); err != nil {
			return false
		}
	}
	return true
}

// supplied holds modules a TEST built from source, standing in for the embedded
// ones. See [SupplyForTests].
var supplied sync.Map // id -> pair

type pair struct{ manifest, module []byte }

// SupplyForTests stands a module built from source in for the embedded one, for
// the rest of this process. THE SERVER NEVER CALLS IT.
//
// It exists because the embedded modules are BUILD OUTPUT and `go test ./...` is
// run constantly on a tree where `make plugins` has not been run — in which case
// this package embeds nothing, the boot-time assertion installs nothing, and every
// black-box suite that expects TMDB to lead video fails for a reason that has
// nothing to do with what it tests. The test harness builds the modules once per
// process (internal/bundled/bundledtest) and supplies them here, so `go test` on
// a fresh clone exercises the REAL module through the REAL sandbox.
//
// It is not a general override and must not become one: supplying a module for an
// id this server does not ship is refused, because the ordered list in this file
// is what decides which source leads a kind and a test that could add to it could
// prove something about a server nobody runs.
func SupplyForTests(id string, manifestRaw, module []byte) error {
	known := false
	for _, want := range ids {
		if want == id {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("bundled: %q is not one of this server's bundled plugin ids", id)
	}
	if len(manifestRaw) == 0 || len(module) == 0 {
		return fmt.Errorf("bundled: supplying %q needs both a manifest and a module", id)
	}
	supplied.Store(id, pair{manifest: manifestRaw, module: module})
	return nil
}

// ManifestBytes is one bundled plugin's manifest EXACTLY as `make plugins` copied
// it out of plugins/<id>/manifest.json.
//
// The bytes matter and are never re-encoded: they are what lands on disk, and a
// re-encoded document is a different document — the same rule install.go follows
// for an Admin's upload, and the reason a signature over one would keep verifying.
func ManifestBytes(id string) ([]byte, error) {
	if v, ok := supplied.Load(id); ok {
		return v.(pair).manifest, nil
	}
	raw, err := modules.ReadFile("modules/" + id + manifestSuffix)
	if err != nil {
		return nil, fmt.Errorf("bundled: %s has no embedded manifest — run make plugins", id)
	}
	return raw, nil
}

// Manifest is one bundled plugin's parsed manifest.
func Manifest(id string) (pluginapi.Manifest, error) {
	raw, err := ManifestBytes(id)
	if err != nil {
		return pluginapi.Manifest{}, err
	}
	var m pluginapi.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return pluginapi.Manifest{}, fmt.Errorf("bundled: the embedded manifest for %s is not valid JSON: %w", id, err)
	}
	if m.ID != id {
		return pluginapi.Manifest{}, fmt.Errorf("bundled: the embedded manifest for %s declares the id %q", id, m.ID)
	}
	return m, nil
}

// Manifests is every present bundled plugin's parsed manifest, in [IDs] order.
func Manifests() ([]pluginapi.Manifest, error) {
	out := make([]pluginapi.Manifest, 0, len(ids))
	for _, id := range Present() {
		m, err := Manifest(id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Module is one bundled plugin's WebAssembly module, decompressed.
//
// Compressed in the binary and decompressed HERE, once, on the boot that installs
// it — a stock-Go guest is ~3.9 MB and about a megabyte gzipped, and seven of them
// uncompressed would be most of the binary's size for bytes that are written to
// disk at most once in a server's life.
func Module(id string) ([]byte, error) {
	if v, ok := supplied.Load(id); ok {
		return v.(pair).module, nil
	}
	raw, err := modules.ReadFile("modules/" + id + moduleSuffix)
	if err != nil {
		return nil, fmt.Errorf("bundled: %s has no embedded module — run make plugins", id)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("bundled: the embedded module for %s is not gzip: %w", id, err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("bundled: decompressing the embedded module for %s: %w", id, err)
	}
	return out, nil
}
