package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

// The host side of Bundled plugins (ADR-0059, .scratch/bundled-plugins issue 04):
// where a plugin came from, how the server puts its own copy on disk, and what
// "uninstall" means for a plugin the server ships.
//
// internal/bundled owns the modules and the boot-time assertion; this file owns
// what the LOADER and the Manager need to know about them, which is deliberately
// almost nothing. The arrow points one way — internal/bundled imports this
// package — so that nothing in the sandbox, the lifecycle or the settings surface
// grows a second code path for a plugin that happens to have shipped.

// Where an Installed plugin came from. The values are what the `plugins.origin`
// column holds and what the settings API returns.
const (
	// OriginAdmin is a plugin a person put here: an upload, a pasted URL, or a
	// directory they placed by hand.
	OriginAdmin = "admin"
	// OriginBundled is a plugin this server shipped and installed itself.
	OriginBundled = "bundled"
)

// StateDeclined is the Installed.State of a Bundled plugin an Admin uninstalled:
// this server ships it, it is not here, and that is the Admin's decision rather
// than an accident. It is the row the Plugins screen puts "reinstall the shipped
// version" on.
const StateDeclined = "declined"

// BundledSource is the server's own shipped plugins, as the Manager needs to see
// them. internal/bundled satisfies it through a small adapter in the composition
// root.
//
// Three methods, because the Manager only ever asks three questions: does this
// server ship an id, what is it called, and would you please put it back.
// Everything else about a Bundled plugin — the ordered list, the versions, the
// embedded bytes — is settled before the Manager exists.
type BundledSource interface {
	// Has reports whether this server ships a plugin with this id AND carries a
	// module for it.
	Has(id string) bool
	// Name is the shipped manifest's human name, for the row a declined plugin
	// gets on the Plugins screen. "" falls back to the id.
	Name(id string) string
	// Assert puts the shipped plugin for id on disk and records its row, exactly
	// as the boot-time assertion does. It is called with the declined mark already
	// cleared.
	Assert(ctx context.Context, id string) error
}

// stagingSeq names staging directories for writes that do not go through a
// Manager (the boot-time assertion runs before one exists). It plays the part
// Manager.staging plays for an install: two writes in the same nanosecond must
// not pick the same directory name.
var stagingSeq atomic.Int64

// InstallFiles writes a manifest and a module into <dir>/<id>/ through the SAME
// staging-and-rename path an Admin's upload takes, and it is how a Bundled plugin
// reaches the disk — both the first time and when this build ships a newer one.
//
// # What a crash leaves behind
//
// The files are written into a dot-prefixed staging directory inside dir, which
// the loader skips, and moved into place by rename. Two cases:
//
//   - Nothing is installed under this id: the whole staged directory is renamed
//     into place, which is one atomic operation on the one filesystem this can
//     happen on. A crash before it leaves no plugin and a staging directory the
//     next write cleans up.
//   - A plugin IS installed under this id (the re-assert replacing an older
//     bundled copy): the two files are renamed over the old ones INDIVIDUALLY,
//     module first and manifest LAST. Each rename is atomic, so no reader ever
//     sees half a file. A crash between them leaves the new module beside the OLD
//     manifest — which still carries the old version, so the next boot's re-assert
//     sees an old copy and finishes the job. The other order would record the new
//     version over the old module and never try again.
//
// It does NOT compile the module first, and that is the one thing it does
// differently from an install. An upload is a stranger's code and is compiled in
// staging so that "it does not instantiate" is a refusal rather than a broken
// plugin on the screen; this module was built from this repository by `make
// plugins` and is verified by the ordinary load a moment later, where a failure
// shows up on the Plugins screen with the sentence that says why — the same place
// an operator would look for any other plugin that did not come up.
func InstallFiles(dir, id string, manifestRaw, module []byte) error {
	var man struct {
		Module string `json:"module"`
	}
	if err := json.Unmarshal(manifestRaw, &man); err != nil {
		return fmt.Errorf("plugins: the manifest for %s is not valid JSON: %w", id, err)
	}
	name := DefaultModuleFile
	if man.Module != "" {
		name = man.Module
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("plugins: the manifest for %s names the module %q, which is a path", id, man.Module)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("plugins: preparing %s: %w", dir, err)
	}
	staging := filepath.Join(dir, fmt.Sprintf(".bundled-%d-%d", os.Getpid(), stagingSeq.Add(1)))
	staged := filepath.Join(staging, id)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return fmt.Errorf("plugins: preparing to install %s: %w", id, err)
	}
	defer os.RemoveAll(staging)

	if err := os.WriteFile(filepath.Join(staged, name), module, 0o644); err != nil {
		return fmt.Errorf("plugins: writing the module for %s: %w", id, err)
	}
	// Byte for byte as it was shipped, never re-encoded — the rule install.go
	// follows for an author's manifest, for the same reason.
	if err := os.WriteFile(filepath.Join(staged, ManifestFile), manifestRaw, 0o644); err != nil {
		return fmt.Errorf("plugins: writing the manifest for %s: %w", id, err)
	}

	target := filepath.Join(dir, id)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		if err := os.Rename(staged, target); err != nil {
			return fmt.Errorf("plugins: installing %s: %w", id, err)
		}
		return nil
	}
	// Replace in place. The module first and the manifest last; see above.
	if err := os.Rename(filepath.Join(staged, name), filepath.Join(target, name)); err != nil {
		return fmt.Errorf("plugins: replacing the module of %s: %w", id, err)
	}
	if err := os.Rename(filepath.Join(staged, ManifestFile), filepath.Join(target, ManifestFile)); err != nil {
		return fmt.Errorf("plugins: replacing the manifest of %s: %w", id, err)
	}
	return nil
}

// --- The Manager's two Bundled verbs -------------------------------------------

// ReinstallShipped puts back a Bundled plugin the Admin uninstalled: it clears the
// declined mark, asks the shipped source to write the files and the row, and then
// rebuilds and swaps so the plugin is live without a restart.
//
// It is a separate verb from install and NOT a special case of it, because the
// Admin is not supplying anything. There is no file to choose, no URL to trust and
// no signature to check: they are undoing their own earlier decision, and the only
// thing this server has to get right is that the plugin comes back as BUNDLED —
// so the next upgrade keeps maintaining it — rather than as something they
// uploaded.
//
// Refusals, each its own sentence: an id this server does not ship, and an id that
// is already installed (which is not a failure the Admin caused, so it says so
// rather than reinstalling over their copy).
func (m *Manager) ReinstallShipped(ctx context.Context, id string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.bundled == nil || !m.bundled.Has(id) {
		return Installed{}, refuse(ReasonUnknown,
			"this server does not ship a plugin with the id %q, so there is no shipped version to reinstall", id)
	}
	if _, err := os.Stat(m.pluginDir(id)); err == nil {
		return Installed{}, refuse(ReasonDuplicate,
			"the plugin %q is already installed; uninstall it first if you want the shipped version back", id)
	}
	if m.store != nil {
		if _, err := m.store.UndeclinePlugin(id); err != nil {
			return Installed{}, err
		}
	}
	if err := m.bundled.Assert(ctx, id); err != nil {
		return Installed{}, err
	}
	if err := m.rebuild(ctx); err != nil {
		return Installed{}, err
	}
	m.logf("obelo: the shipped version of plugin %s was reinstalled by an admin", id)
	return m.view(id)
}

// rowOrigin is where the plugin with this id came from, "" when it has no row.
// A read failure answers "" rather than guessing: the caller's only use for it is
// deciding whether to write a declined mark, and not writing one is the state the
// server was already in.
func (m *Manager) rowOrigin(id string) string {
	if m.store == nil {
		return ""
	}
	rows, err := m.rows()
	if err != nil {
		m.logf("obelo: plugin %s: its origin could not be read: %v", id, err)
		return ""
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Origin
		}
	}
	return ""
}

// declineIfUninstalled remembers that the Admin removed a plugin THIS SERVER
// SHIPS, so the next boot's re-assert leaves it alone.
//
// Without it "uninstall" would mean "until you restart": the server would find the
// id missing, conclude it had never been installed, and put it straight back. The
// mark is the difference between a lifecycle the operator controls and one the
// binary does.
//
// The caller decides on the ROW's origin rather than on whether this server
// happens to ship the id, because those are different questions: an Admin who
// uploaded their own `tmdb` and then removed it has removed THEIRS, and the
// shipped one — which their upload has been shadowing all along — is then free to
// arrive on the next boot, which is what they would expect.
func (m *Manager) declineIfUninstalled(id string) {
	if m.store == nil {
		return
	}
	if err := m.store.DeclinePlugin(id); err != nil {
		m.logf("obelo: plugin %s was uninstalled but the server could not record that it was declined, "+
			"so it may come back on the next restart: %v", id, err)
		return
	}
	m.logf("obelo: plugin %s shipped with this server and was declined; it will not be reinstalled on boot", id)
}

// declinedRows is the Plugins screen's row for every Bundled plugin this server
// ships, is not installed, and was declined. It is appended to List.
//
// A declined plugin has no files, no row and no loader state, so nothing else in
// this package would ever mention it — and a screen that simply did not show it
// would leave an Admin with no way back except reading a changelog. It is the
// minimum a row can be: the id, the name out of the shipped manifest, the origin,
// and the state that puts the button on it.
func (m *Manager) declinedRows(installed []Installed) []Installed {
	if m.store == nil || m.bundled == nil {
		return nil
	}
	declined, err := m.store.DeclinedPluginIDs()
	if err != nil {
		m.logf("obelo: the declined plugins could not be read: %v", err)
		return nil
	}
	have := make(map[string]struct{}, len(installed))
	for _, item := range installed {
		have[item.ID] = struct{}{}
	}
	var out []Installed
	for _, id := range declined {
		if _, ok := have[id]; ok {
			continue // it came back under the same id; the real row says everything
		}
		if !m.bundled.Has(id) {
			continue // a mark for something this build no longer ships
		}
		out = append(out, Installed{
			ID:       id,
			Name:     m.bundledName(id),
			Provides: []string{},
			Origin:   OriginBundled,
			State:    StateDeclined,
		})
	}
	return out
}

// bundledName is the shipped manifest's name for an id, falling back to the id.
func (m *Manager) bundledName(id string) string {
	if m.bundled == nil {
		return id
	}
	if name := m.bundled.Name(id); name != "" {
		return name
	}
	return id
}
