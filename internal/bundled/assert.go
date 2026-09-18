package bundled

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/goozakdev/obelo-server/internal/plugins"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The boot-time assertion (ADR-0059 decision 1): before the loader reads
// <dataDir>/plugins/, the server makes its own shipped plugins true on disk.
//
// It runs on EVERY boot and not only the first, because the alternative is a
// server that ships a fixed TMDB plugin and never delivers it. The table it
// applies is the PRD's, whole:
//
//	Installed copy of a bundled id          Action
//	--------------------------------------  -------------------------------------
//	absent, never declined                  write the files, insert the row
//	present, origin=bundled, older version  replace module + manifest in place
//	present, origin=bundled, same or newer   nothing
//	present, origin=admin                    nothing — theirs wins
//	absent, declined                         nothing — they meant it
//
// The two "nothing" rows are the interesting ones and they are the same promise
// read twice: the operator's decisions about their own server survive an upgrade.
// An Admin who uploaded their own plugin under a shipped id has said something,
// and an Admin who uninstalled a shipped one has said something, and a server that
// overwrote either would be winning an argument it was not invited to.
//
// NOTHING HERE MAY STOP A BOOT (ADR-0001, and the ADR-0043 template). Every
// failure below is a logged line and a plugin that is not there; a server with no
// TMDB plugin is a server that does not enrich movies, which is exactly the state
// a server with no TMDB key is already in and which the whole product degrades
// over.

// Store is the persistence the assertion owns. *store.DB satisfies it; the narrow
// interface is what keeps this package's tests free of a database.
type Store interface {
	Plugins() ([]store.PluginRow, error)
	InsertPlugin(p store.PluginInsert) error
	UpdatePluginManifest(p store.PluginInsert) error
	DeclinedPluginIDs() ([]string, error)
}

// Source is this server's shipped plugins, bound to one data directory and one
// database. It is what the composition root asserts at boot and what it hands the
// plugin Manager so "reinstall the shipped version" can put one back.
//
// It satisfies plugins.BundledSource.
type Source struct {
	dir   string
	store Store
	logf  func(string, ...any)
}

var _ plugins.BundledSource = (*Source)(nil)

// NewSource binds the shipped plugins to a data directory. dataDir is the data
// directory itself, not the plugins folder under it — the folder name is
// plugins.DirName and is the loader's to choose.
//
// A nil store is a server that cannot record what it installed, which is a narrow
// test and never production: the files are still written, and the loader still
// finds them, because THE DIRECTORY IS THE TRUTH ABOUT WHAT IS INSTALLED.
func NewSource(dataDir string, st Store, logf func(string, ...any)) *Source {
	if logf == nil {
		logf = log.Printf
	}
	return &Source{dir: filepath.Join(dataDir, plugins.DirName), store: st, logf: logf}
}

// Has reports whether this server ships a plugin with this id and carries a
// module for it.
func (s *Source) Has(id string) bool { return has(id) }

// Name is the shipped manifest's human name for an id, "" when there is none.
func (s *Source) Name(id string) string {
	m, err := Manifest(id)
	if err != nil {
		return ""
	}
	return m.Name
}

// AssertAll applies the table above to every shipped plugin, in shipped order.
//
// It returns no error: each id is independent, and one plugin whose files could
// not be written must not take the other six with it.
func (s *Source) AssertAll(ctx context.Context) {
	declined, err := s.declined()
	if err != nil {
		// Fails CLOSED on the one decision that cannot be taken back cheaply: if
		// the declined marks cannot be read, installing would silently undo an
		// Admin's uninstall, so nothing is installed this boot and the line says so.
		s.logf("obelo: the declined plugins could not be read, so no shipped plugin was asserted this boot: %v", err)
		return
	}
	rows, err := s.rows()
	if err != nil {
		s.logf("obelo: the plugin rows could not be read, so no shipped plugin was asserted this boot: %v", err)
		return
	}
	for _, id := range Present() {
		if _, no := declined[id]; no {
			continue
		}
		if err := s.assert(ctx, id, rows[id]); err != nil {
			s.logf("obelo: the plugin %s shipped with this server but could not be installed: %v", id, err)
		}
	}
}

// Assert applies the table to ONE shipped plugin — what "reinstall the shipped
// version" calls, with the declined mark already cleared.
//
// Unlike AssertAll it returns the error, because there is somebody waiting for an
// answer: an Admin pressed a button, and a failure has to reach them rather than
// the log.
func (s *Source) Assert(ctx context.Context, id string) error {
	if !has(id) {
		return fmt.Errorf("this server ships no plugin with the id %q", id)
	}
	rows, err := s.rows()
	if err != nil {
		return err
	}
	return s.assert(ctx, id, rows[id])
}

// assert is the table, for one id, given its row (the zero value when it has
// none).
func (s *Source) assert(_ context.Context, id string, row store.PluginRow) error {
	hasRow := row.ID != ""
	if hasRow && row.Origin == plugins.OriginAdmin {
		// Theirs wins, whatever version it is. This is the whole of decision 1's
		// promise to an operator who replaced a shipped provider with their own.
		return nil
	}
	_, statErr := os.Stat(filepath.Join(s.dir, id))
	onDisk := statErr == nil
	if onDisk && !hasRow {
		// Files with no row is a plugin an operator placed BY HAND, which is how
		// every Installed plugin arrived before there was an installer. It is
		// theirs for the same reason an upload is, and overwriting it would be the
		// one case where this server destroyed something a person put there.
		return nil
	}

	manifestRaw, err := ManifestBytes(id)
	if err != nil {
		return err
	}
	shipped, err := Manifest(id)
	if err != nil {
		return err
	}

	if onDisk {
		// A bundled copy is already here. Replace it only when this build ships a
		// newer version — comparing what is ON DISK, because that is what the
		// loader will read, rather than what the row remembers.
		installed := s.installedVersion(id)
		if !olderThan(installed, shipped.Version) {
			return nil
		}
		s.logf("obelo: replacing the installed %s plugin (version %s) with the one this server ships (version %s)",
			id, describeVersion(installed), describeVersion(shipped.Version))
	}

	module, err := Module(id)
	if err != nil {
		return err
	}
	if err := plugins.InstallFiles(s.dir, id, manifestRaw, module); err != nil {
		return err
	}
	if s.store == nil {
		return nil
	}
	record := store.PluginInsert{
		ID:         shipped.ID,
		Name:       shipped.Name,
		Version:    shipped.Version,
		APIVersion: shipped.APIVersion,
		Provides:   providesOf(shipped),
		Source:     SourceShipped,
		Origin:     plugins.OriginBundled,
	}
	if hasRow {
		return s.store.UpdatePluginManifest(record)
	}
	if err := s.store.InsertPlugin(record); err != nil {
		return err
	}
	s.logf("obelo: the plugin %s (%s %s) shipped with this server and was installed", shipped.ID, shipped.Name, shipped.Version)
	return nil
}

// SourceShipped is the provenance recorded on a Bundled plugin's row, where an
// Admin's upload records "upload" or the URL they pasted. The Plugins screen reads
// `origin` rather than this, but the column is what an operator sees in an export
// and in the database, and "" there would read as "nobody knows".
const SourceShipped = "shipped with obelo"

// installedVersion is the version in the manifest ON DISK, "" when there is no
// readable manifest.
//
// The FILE and not the row, deliberately: the row is what this server remembers
// writing and the file is what the loader will read, and when they disagree it is
// the file that decides what is actually running.
func (s *Source) installedVersion(id string) string {
	raw, err := os.ReadFile(filepath.Join(s.dir, id, plugins.ManifestFile))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m.Version
}

func (s *Source) rows() (map[string]store.PluginRow, error) {
	out := map[string]store.PluginRow{}
	if s.store == nil {
		return out, nil
	}
	rows, err := s.store.Plugins()
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

func (s *Source) declined() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if s.store == nil {
		return out, nil
	}
	ids, err := s.store.DeclinedPluginIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

// providesOf is the Extension points a manifest declares, as the contract's own
// tokens, for the row. It is the same value install.go records for an upload, so
// the Plugins screen reads one shape however a plugin arrived.
func providesOf(m pluginapi.Manifest) []string {
	out := make([]string, 0, len(m.Provides))
	for _, p := range m.Provides {
		out = append(out, string(p.Kind))
	}
	return out
}

// --- Versions ------------------------------------------------------------------

// olderThan reports whether the version installed on disk is older than the one
// this server ships, which is the one question the re-assert asks.
//
// # Why "semver-ish" and not semver
//
// pluginapi.Manifest.Version is documented as an OPAQUE display string the host
// never parses, and that is the right rule for a plugin an Admin installed: its
// versioning is its author's business. It is not the right rule for a plugin THIS
// SERVER ships, where the same project writes both sides and an upgrade has to be
// able to say "this one is newer". So the comparison is deliberately narrow: dotted
// runs of digits, compared numerically, longest wins on a tie ("1.2.1" > "1.2").
//
// ANYTHING ELSE IS OLDER. A version with a letter in it, an empty one, a date, a
// git hash — none of them can be ordered against "1.1.0" without inventing a rule,
// and the safe reading of "I cannot tell" is "replace it": the installed copy is
// then refreshed on one boot, which costs a file write, where the other default
// would leave a server running a plugin the maintainer had already fixed.
func olderThan(installed, shipped string) bool {
	a, okA := versionParts(installed)
	b, okB := versionParts(shipped)
	if !okB {
		// This server's OWN manifest has an unreadable version. Replacing on every
		// boot forever would be worse than leaving it; this is a bug in the tree,
		// and TestEveryBundledManifestCarriesAComparableVersion is where it surfaces.
		return false
	}
	if !okA {
		return true
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// versionParts reads a dotted run of digits. ok is false for anything else.
func versionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// describeVersion is a version for a log line, naming the empty case rather than
// printing nothing.
func describeVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "(none)"
	}
	return v
}
