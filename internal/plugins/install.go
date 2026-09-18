package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goozakdev/obelo-server/internal/plugins/signing"
	"github.com/goozakdev/obelo-server/internal/safefetch"
	"github.com/goozakdev/obelo-server/internal/store"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Installing a Plugin on a RUNNING server (.scratch/plugin-system issue 10).
//
// # The hard part, stated plainly
//
// The registry is a value with no locking, read by five different things on paths
// that never expected to be interrupted — the enrichment Catalog, the subtitle
// builder, the event-sink Manager, the settings handlers and the per-Library
// policy resolver. An install cannot reach into it. So it does what the provider
// Manager already does with a composed provider: it builds a WHOLE NEW value and
// swaps it in one store (pluginapi.Registry.Swap), then tells each Manager to
// Reload so the live sinks and providers rebuild from it, and only then closes the
// Set the old value pointed at — after the sinks that were calling into it have
// stopped, or an in-flight delivery would trap on the way out and a healthy Plugin
// would be recorded as having failed.
//
// Rebuild-and-swap also means an install is never a delta. The Set is re-read from
// disk in full and the Built-ins are registered again from scratch, so the state
// after an install is exactly the state a reboot would produce — which is the only
// definition of "no restart required" worth making a claim about.
//
// # What an install refuses, and why each one is its own sentence
//
// A bad manifest, an unsupported API version, a duplicate id, a module that will
// not instantiate, and a source this server will not fetch from. They are five
// different things for the Admin to do next, so they are five different sentences
// and five different Reasons, and NOTHING IS LEFT ON DISK OR IN THE DATABASE when
// any of them fires: the files are written to a staging directory inside the
// plugins folder, compiled there, and only renamed into place once the module has
// proved it loads.

// Reasons an install or a lifecycle verb was refused. The API layer maps these to
// its own codes; they are a small closed vocabulary rather than error identity
// because the sentence, not the sentinel, is what an Admin reads.
const (
	// ReasonManifest: there is no manifest, it is not JSON, or it claims something
	// a manifest may not claim.
	ReasonManifest = "manifest"
	// ReasonAPIVersion: the manifest names a plugin API version this server does
	// not speak. Its message ALWAYS names which side to upgrade (ADR-0058
	// decision 8, the ADR-0055 posture).
	ReasonAPIVersion = "api-version"
	// ReasonDuplicate: the id is already claimed — by an Installed plugin, or by
	// a Built-in this binary ships.
	ReasonDuplicate = "duplicate"
	// ReasonModule: there is no module, or it will not compile or instantiate in
	// this server's sandbox.
	ReasonModule = "module"
	// ReasonSource: the URL an Admin pasted is not one this server will fetch a
	// plugin from, or what came back was not usable.
	ReasonSource = "source"
	// ReasonUnknown: a lifecycle verb named a Plugin that is not installed.
	ReasonUnknown = "unknown"
	// ReasonSettings: a settings save did not satisfy the schema the Plugin's own
	// manifest declared (issue 13). It is the one refusal that is PER FIELD rather
	// than per request, so its Refusal carries Fields and the API renders each
	// sentence under the control that caused it.
	ReasonSettings = "settings"
	// ReasonSignature: this server has publisher keys PINNED, and the plugin does
	// not satisfy them (issue 15) — it carries no signature, names a publisher
	// nobody pinned, covers other bytes, or does not verify. Its message always
	// names the publisher the plugin CLAIMED, because that is the one fact that
	// tells an operator which of those happened. A server with nothing pinned
	// never produces this refusal.
	ReasonSignature = "signature"
)

// Refusal is an install this server would not perform. Message is written for the
// Admin and is what the API returns verbatim; Reason is the stable token an API
// layer maps to its own error code, so the prose can be improved without breaking
// a client.
type Refusal struct {
	Reason  string
	Message string
	// Fields is the per-field detail of a ReasonSettings refusal, and empty for
	// every other reason. A form needs to know WHICH control to put a sentence
	// under, and a single prose message cannot say it — so the structured list
	// travels beside the message rather than instead of it, and Message names the
	// first field so a caller that ignores the list still reads something useful.
	Fields []FieldError
}

func (r *Refusal) Error() string { return r.Message }

func refuse(reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Limits on what an Admin may hand this server. Generous rather than tight: a
// stock-Go guest is ~3.4 MiB and a TinyGo one ~243 KiB (ADR-0058), so the module
// cap is there to stop a mistake filling a disk, not to express a policy.
const (
	// MaxModuleBytes caps an uploaded or fetched WebAssembly module.
	MaxModuleBytes = 64 << 20
	// MaxManifestBytes caps a manifest document. A manifest is a few hundred bytes
	// of JSON; anything near this is not one.
	MaxManifestBytes = 256 << 10
	// SourceFetchTimeout bounds each of the fetches a URL install makes.
	SourceFetchTimeout = 60 * time.Second
	// MaxSignatureBytes caps the detached signature document (issue 15). It is the
	// signing package's cap, restated here so the three limits an install applies
	// read as one list.
	MaxSignatureBytes = signing.MaxSignatureBytes
	// MaxCatalogBytes caps a catalog index. A household's index is tens of entries;
	// a megabyte is room for thousands and a refusal for anything that is not an
	// index at all.
	MaxCatalogBytes = 1 << 20
	// CatalogFetchTimeout bounds the catalog fetch, and is SHORT on purpose: the
	// Plugins screen asks for it on every load, and an index that is slow must cost
	// a quiet note rather than a screen that hangs.
	CatalogFetchTimeout = 10 * time.Second
)

// SourceUpload is the recorded provenance of a Plugin an Admin sent through the
// browser. A URL install records the URL itself.
const SourceUpload = "upload"

// Installed is one Installed plugin as the Plugins screen shows it: what the
// manifest said, what the Admin decided, and what the loader has to report.
//
// It joins two records that are deliberately NOT the same thing. Enabled is the
// Admin's switch, persisted in the plugins row. Disabled is the loader saying this
// server will not call this code — because it was refused at load, or because it
// failed enough times at runtime to be stopped. A Plugin can be enabled and
// disabled at once, and that combination is precisely the state that needs
// explaining on a screen.
type Installed struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	APIVersion  int      `json:"apiVersion,omitempty"`
	Provides    []string `json:"provides"`
	Enabled     bool     `json:"enabled"`
	Disabled    bool     `json:"disabledByFailure"`
	LastError   string   `json:"lastError,omitempty"`
	Source      string   `json:"source,omitempty"`
	InstalledAt string   `json:"installedAt,omitempty"`
	// Publisher and KeyID are who SIGNED this Plugin, recorded at install time and
	// ONLY when the signature verified against a key an Admin had pinned (issue
	// 15). Empty means no verification happened — either nothing was pinned or
	// nothing was signed — and a screen must not read it as "unsigned", because
	// this server does not know that.
	Publisher string `json:"publisher,omitempty"`
	KeyID     string `json:"keyId,omitempty"`
	// Origin is who put this Plugin here: OriginBundled for one this server
	// shipped (ADR-0059), OriginAdmin for one a person uploaded, pasted a URL for
	// or placed by hand. It is what the Plugins screen turns into "Shipped with
	// Obelo" against the upload name or URL an Admin's plugin shows.
	Origin string `json:"origin,omitempty"`
	// State is the row's lifecycle state when it is not simply installed. Today
	// there is exactly one value — StateDeclined, a Bundled plugin the Admin
	// uninstalled — and its row is the one the screen offers "reinstall the
	// shipped version" on. Empty for every installed Plugin.
	State string `json:"state,omitempty"`
	// SettingsSchema is what this Plugin's manifest declares about its OWN settings
	// (issue 13), straight from the file on disk: the ordered field list the web app
	// renders a form from. Empty for a Plugin configured entirely through the fixed
	// shape, which is every Plugin written before the schema existed — and the
	// screen then shows no panel at all rather than an empty one.
	SettingsSchema []pluginapi.SettingsField `json:"settingsSchema,omitempty"`
	// Settings is what is currently IN FORCE against that schema, with every secret
	// removed. Present exactly when SettingsSchema is.
	Settings *SettingsView `json:"settings,omitempty"`
}

// SettingsView is a Plugin's declared settings as the API returns them: the values
// an Admin can see, and a per-secret "there is one on file" boolean.
//
// Values are EFFECTIVE, not merely stored: a field nobody has saved carries the
// default its manifest declared, because that is what the guest will read and a
// screen showing an empty box for a field the Plugin will treat as "eu" is a
// screen telling an operator something false. A field with neither a saved value
// nor a default is absent, and absent is not the same as zero.
//
// A SECRET'S VALUE IS NEVER HERE. It is the same policy the fixed Secret has and
// the same policy metadata_providers.api_key has — the form shows that one is set
// and offers to replace it, and nothing this server sends ever carries the value
// back out.
type SettingsView struct {
	Values  map[string]any  `json:"values"`
	Secrets map[string]bool `json:"secrets"`
}

// ManagerStore is the persistence the Manager owns. *store.DB satisfies it; the
// narrow interface is what keeps this package's tests free of a database.
type ManagerStore interface {
	Plugins() ([]store.PluginRow, error)
	DisabledPluginIDs() ([]string, error)
	InsertPlugin(p store.PluginInsert) error
	SetPluginEnabled(id string, enabled bool) (bool, error)
	SetPluginLastError(id, message string) error
	DeletePlugin(id string) error
	PluginSettings(pluginID string) ([]store.PluginSetting, error)
	ReplacePluginSettings(pluginID string, values []store.PluginSetting) error
	// PluginPublishers is the pinned-key policy (issue 15). An EMPTY list is the
	// shipped state and means nothing is verified, so it is asked on every install
	// rather than cached: unpinning the last key has to take effect at once, and
	// so does pinning the first.
	PluginPublishers() ([]store.PluginPublisher, error)
	UpsertPluginPublisher(p store.PluginPublisher) error
	DeletePluginPublisher(publisher string) (bool, error)
	// SetPluginSigner records who signed, and is called only after a signature has
	// verified against a pinned key.
	SetPluginSigner(id, publisher, keyID string) error
	// PluginCatalogURL / SetPluginCatalogURL are the operator's chosen index —
	// empty by default, and empty means this server browses no catalog at all.
	PluginCatalogURL() (string, error)
	SetPluginCatalogURL(url string) error
	// The declined memory (ADR-0059 decision 2): the Bundled plugins an Admin
	// uninstalled. Uninstalling one records the mark; reinstalling the shipped
	// version clears it; the boot-time re-assert reads it before it writes
	// anything.
	DeclinedPluginIDs() ([]string, error)
	DeclinePlugin(id string) error
	UndeclinePlugin(id string) (bool, error)
}

// ManagerConfig is what the composition root hands the Manager. Every field but
// Dir and Registry has a usable zero value, so a narrow test wires two things.
type ManagerConfig struct {
	// Dir is <dataDir>/plugins — the directory that is the TRUTH about what is
	// installed. The database records what the files cannot say; it never decides
	// what loads.
	Dir string
	// Registry is the LIVE registry every reader holds. The Manager never mutates
	// it; it builds a fresh one and calls Swap.
	Registry *pluginapi.Registry
	// Set is what plugins.Load found at boot, so the Manager starts owning the
	// runtimes the composition root already built rather than compiling them twice.
	Set *Set
	// Store is the plugins table. Nil is a server that cannot remember an Admin's
	// switch across a restart, which is a narrow test and never production.
	Store ManagerStore
	// Loader tunes the sandbox the way app.New tunes it at boot; the same Options
	// are used for every rebuild, so a Plugin installed at runtime lives under the
	// budgets a rebooted server would give it.
	Loader Options
	// Base registers everything that is NOT an Installed plugin into a fresh
	// registry: the Built-ins, and whatever else the composition root put there.
	// It is a callback rather than a call to internal/builtins because registration
	// is a composition-root act (ADR-0057 decision 5) and this package must not
	// become a second place that decides what a server ships.
	Base func(*pluginapi.Registry)
	// Reload rebuilds the live values composed FROM the registry — the provider
	// Manager, the subtitle Manager, the sink Manager — after a swap. It runs
	// before the old Set is closed, which is the ordering that keeps an in-flight
	// delivery from trapping on a runtime being torn down underneath it.
	Reload func(context.Context) error
	// Client fetches a pasted URL. Nil means safefetch.Client; whatever is passed
	// is GUARDED, so the redirect policy cannot be wired away.
	Client *http.Client
	// Logf is where install and lifecycle lines go. Nil means the server log.
	Logf func(format string, args ...any)
	// Bundled is the plugins this server SHIPS (ADR-0059), which the Manager needs
	// for exactly two verbs: uninstalling one has to remember the Admin declined
	// it, and "reinstall the shipped version" has to put it back. Nil is a server
	// with no Bundled plugins — every narrow test, and every server before
	// ADR-0059 — and both verbs then behave as they did.
	Bundled BundledSource
	// AllowPrivateSources drops the address check on a pasted URL. IT IS FOR TESTS
	// AND FOR NOTHING ELSE: the suite serves its fixture plugin from an
	// httptest.Server on 127.0.0.1, and there is no hermetic public address to
	// serve it from instead. Production leaves it false, which is what makes
	// "a URL resolving to a private address is refused" true.
	AllowPrivateSources bool
}

// Manager owns the Installed-plugin lifecycle: the directory, the rows, the live
// Set, and the rebuild-and-swap that makes an install take effect with no restart.
//
// ONE WRITER AT A TIME (mu). Installing is a filesystem edit, a database write and
// a registry swap, and two of them interleaved would race over the same directory
// name. Readers do not take mu at all: the Set is an atomic pointer and the
// registry is swapped in one store, so the Plugins screen and the delivery path
// never wait behind a download.
type Manager struct {
	dir      string
	registry *pluginapi.Registry
	store    ManagerStore
	loader   Options
	base     func(*pluginapi.Registry)
	reload   func(context.Context) error
	client   *http.Client
	logf     func(string, ...any)
	bundled  BundledSource

	allowPrivateSources bool

	// mu serializes install / uninstall / enable / disable / re-enable.
	mu sync.Mutex
	// set is the live Set, replaced wholesale by every rebuild. Atomic because a
	// reader must never block on a writer holding mu through a network fetch.
	set atomic.Pointer[Set]
	// staging counts the staging directories this process has made, so two
	// installs in the same nanosecond cannot pick one name.
	staging atomic.Int64
}

// NewManager wires the Manager. It performs no I/O and cannot fail: a server with
// no plugins directory, no database and no Plugins is a valid one.
func NewManager(cfg ManagerConfig) *Manager {
	m := &Manager{
		dir:                 cfg.Dir,
		registry:            cfg.Registry,
		store:               cfg.Store,
		loader:              cfg.Loader.withDefaults(),
		base:                cfg.Base,
		reload:              cfg.Reload,
		client:              safefetch.Guard(cfg.Client),
		logf:                cfg.Logf,
		bundled:             cfg.Bundled,
		allowPrivateSources: cfg.AllowPrivateSources,
	}
	if m.logf == nil {
		m.logf = m.loader.Logf
	}
	if m.client.Timeout == 0 {
		m.client.Timeout = SourceFetchTimeout
	}
	m.set.Store(cfg.Set)
	return m
}

// Plugins is the live Set. It is what the settings API's Status join reads, and it
// changes under the caller's feet by design — an install swaps the whole thing.
func (m *Manager) Plugins() *Set {
	if m == nil {
		return nil
	}
	return m.set.Load()
}

// Status reports one Installed plugin's loader state by slug, so *Manager
// satisfies the same one-method interface *Set does and the event-sink settings
// response keeps working across an install.
func (m *Manager) Status(slug string) (Status, bool) {
	return m.Plugins().Status(slug)
}

// Close releases every runtime the live Set holds.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	return m.Plugins().Close(ctx)
}

// --- Lifecycle ---------------------------------------------------------------

// Install writes a manifest and a module into <dir>/<id>/ and brings the Plugin
// up on the running server.
//
// The order is the whole design. Validate the manifest; refuse a duplicate id
// BEFORE touching the disk; write both files into a staging directory and COMPILE
// THE MODULE THERE, so "it does not instantiate" is discovered while nothing is
// installed; rename the staged directory into place, which is atomic on the one
// filesystem this can happen on; record the row; and only then rebuild and swap.
//
// signatureRaw is the detached signature document that travelled with the plugin
// (pluginapi.SignatureFile), or nil when none did. It is checked against the keys
// an Admin pinned — signature.go holds the two-state policy — and stored beside
// the manifest either way, as provenance.
//
// source is the provenance recorded on the row: SourceUpload, or the URL.
func (m *Manager) Install(ctx context.Context, manifestRaw, module, signatureRaw []byte, source string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.install(ctx, manifestRaw, module, signatureRaw, source)
}

// InstallFromURL fetches a manifest from an absolute URL and the module from
// BESIDE IT — the manifest's own `module` file name (default plugin.wasm),
// resolved against the manifest's directory — and then installs exactly as an
// upload does.
//
// One URL, two fetches, no archive format. An archive would need this server to
// decide a container format, unpack untrusted paths and defend against a member
// called "../../obelo.db"; two GETs against a layout that already exists — the
// same two files, in the same directory, under the same names they take on disk —
// need none of that. It is also the layout a catalog (issue 15) can list: a
// catalog entry is a manifest URL and nothing more.
//
// Both fetches go through safefetch, so the redirect policy and the bounded chain
// apply. Unlike every other outbound fetch in this server, the FIRST hop is
// checked too: see refuseInternalSource.
//
// A THIRD fetch looks for the detached signature (issue 15), beside the manifest
// under its conventional name unless signatureURL names somewhere else — which is
// what a catalog entry's optional signatureUrl is for. A 404 is NOT an error:
// most plugins are unsigned, and a server with no publisher keys pinned does not
// care. What a missing signature COSTS is decided afterwards by the pinned-key
// policy, which is the one place that decision belongs.
//
// This is also the whole of a catalog install (issue 15): a catalog entry is a
// manifest URL, so browsing one adds a way to choose an address and adds nothing
// whatever to this path — including the first-hop check, which is why an entry
// pointing into this server's own network is refused with the sentence a pasted
// one gets.
func (m *Manager) InstallFromURL(ctx context.Context, rawURL, signatureURL string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	manifestURL, err := m.checkSourceURL(ctx, rawURL)
	if err != nil {
		return Installed{}, err
	}
	manifestRaw, err := m.get(ctx, manifestURL.String(), MaxManifestBytes, "manifest")
	if err != nil {
		return Installed{}, err
	}
	// Decoded once here only to learn the module's file name; install decodes it
	// again as the authority, from the same bytes.
	man, err := decodeManifest(manifestRaw)
	if err != nil {
		return Installed{}, err
	}
	beside := func(name string) string {
		u := *manifestURL
		u.RawQuery = ""
		u.Fragment = ""
		u.Path = path.Join(path.Dir(manifestURL.Path), name)
		return u.String()
	}
	module, err := m.get(ctx, beside(moduleFile(man)), MaxModuleBytes, "module")
	if err != nil {
		return Installed{}, err
	}
	sigTarget := strings.TrimSpace(signatureURL)
	if sigTarget == "" {
		sigTarget = beside(pluginapi.SignatureFile)
	} else if _, err := m.checkSourceURL(ctx, sigTarget); err != nil {
		// An explicit signature URL gets the same address check the manifest gets.
		// It is not code, but it is the document that decides whether code is
		// trusted, and a catalog entry able to point it inside this network would be
		// a catalog entry choosing whose signature this server believes.
		return Installed{}, err
	}
	signatureRaw, err := m.getOptional(ctx, sigTarget, MaxSignatureBytes, "signature")
	if err != nil {
		return Installed{}, err
	}
	return m.install(ctx, manifestRaw, module, signatureRaw, manifestURL.String())
}

// SetEnabled is the Admin's switch. Switching a Plugin OFF un-registers it, so
// nothing downstream can reach it: the sink Manager rebuilds from a registry the
// Plugin is not in, and its worker stops before this call returns. That is why
// there is no "enabled" flag on the delivery path — there is nothing to check,
// because there is nothing there.
func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureRow(id); err != nil {
		return Installed{}, err
	}
	if m.store != nil {
		if _, err := m.store.SetPluginEnabled(id, enabled); err != nil {
			return Installed{}, err
		}
	}
	if err := m.rebuild(ctx); err != nil {
		return Installed{}, err
	}
	m.logf("obelo: plugin %s is now %s", id, enabledWord(enabled))
	return m.view(id)
}

// Reenable forgets a Plugin's recorded failure and gives it another chance.
//
// It clears the loader's state — disabled, the last error, the consecutive-failure
// count and the allowlist-violation count — AND rebuilds from disk, which is the
// part that matters for the failure an operator most wants to retry: a Plugin
// refused at LOAD has no module behind it, so clearing a flag would leave it just
// as dead. A rebuild re-reads the files and recompiles, so an Admin who fixed the
// manifest and pressed Re-enable gets what they expected.
//
// It does NOT touch the Admin's enable switch. A Plugin that was switched off is
// switched on with Enable; conflating the two would make one button mean two
// things and make the screen lie about which one is in force.
func (m *Manager) Reenable(ctx context.Context, id string) (Installed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureRow(id); err != nil {
		return Installed{}, err
	}
	if p := m.plugin(id); p != nil {
		p.Reenable()
	}
	if m.store != nil {
		if err := m.store.SetPluginLastError(id, ""); err != nil {
			return Installed{}, err
		}
	}
	if err := m.rebuild(ctx); err != nil {
		return Installed{}, err
	}
	m.logf("obelo: plugin %s was re-enabled by an admin", id)
	return m.view(id)
}

// Uninstall unloads the module, deletes the files and deletes the rows.
//
// The directory is renamed ASIDE first, to a dot-prefixed name the loader skips,
// and only deleted once the rebuild has taken the Plugin out of the registry and
// the old Set has been closed. Deleting it up front would mean the window between
// "the files are gone" and "the runtimes are released" is a window in which a
// live sink is calling into a module whose bytes no longer exist anywhere but in
// memory — which happens to work, and is the kind of thing that works until it
// does not.
//
// The rows are store.DeletePlugin's one transaction, and they include the guest's
// own key-value namespace: a cursor or a cache that outlived its Plugin would be
// read straight back by the next install under the same id, whoever wrote it.
//
// What it deliberately does NOT delete: the artwork and the subtitles the Plugin
// produced. Those are identity-keyed and live in the ordinary caches, indis-
// tinguishable from anything else the server fetched, and they are the Library's
// now (ADR-0007).
func (m *Manager) Uninstall(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.mustBeInstalled(id); err != nil {
		return err
	}
	dir := m.pluginDir(id)
	trash := ""
	if _, err := os.Stat(dir); err == nil {
		trash = filepath.Join(m.dir, fmt.Sprintf(".uninstall-%s-%d", id, m.staging.Add(1)))
		if err := os.Rename(dir, trash); err != nil {
			return fmt.Errorf("plugins: removing %s: %w", id, err)
		}
	}
	rebuildErr := m.rebuild(ctx)
	// Read the origin BEFORE the row goes, and record the decline AFTER it has:
	// DeletePlugin is one transaction over every table that names this id, and a
	// declined mark written first would be a row it has no reason to know about.
	bundledRow := m.rowOrigin(id) == OriginBundled
	if m.store != nil {
		if err := m.store.DeletePlugin(id); err != nil {
			return err
		}
		if bundledRow {
			m.declineIfUninstalled(id)
		}
	}
	if trash != "" {
		if err := os.RemoveAll(trash); err != nil {
			m.logf("obelo: plugin %s was uninstalled but its files could not be deleted: %v", id, err)
		}
	}
	if rebuildErr != nil {
		return rebuildErr
	}
	m.logf("obelo: plugin %s was uninstalled", id)
	return nil
}

// List is every Installed plugin, in id order, joined from the rows and the live
// loader.
//
// It also writes the loader's last error BACK onto the row, best-effort. That is
// the one write on a read path in this file and it is deliberate: the loader's
// state dies with the process, so without it a Plugin that failed yesterday comes
// back after a restart looking perfectly healthy until the next delivery fails
// again. This is the moment the question is being asked, so this is the moment the
// answer is worth keeping.
//
// A Plugin on disk with NO row is listed too — that is a Plugin an operator placed
// by hand, which is how every Installed plugin arrived before this issue existed,
// and it must not vanish from the screen because it did not come through the
// upload form.
func (m *Manager) List(ctx context.Context) ([]Installed, error) {
	rows, err := m.rows()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.PluginRow, len(rows))
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
		order = append(order, r.ID)
	}
	statuses := map[string]Status{}
	for _, p := range m.Plugins().Plugins() {
		st := p.Status()
		statuses[st.ID] = st
		if _, ok := byID[st.ID]; !ok {
			order = append(order, st.ID)
		}
	}
	// id order, the loader's own order, so the screen and the log agree.
	sort.Strings(order)

	out := make([]Installed, 0, len(order))
	for _, id := range order {
		row, hasRow := byID[id]
		st, loaded := statuses[id]
		item := Installed{
			ID:          id,
			Name:        firstNonEmpty(st.Name, row.Name, id),
			Version:     firstNonEmpty(st.Version, row.Version),
			APIVersion:  row.APIVersion,
			Provides:    row.Provides,
			Enabled:     !hasRow || row.Enabled,
			Disabled:    st.Disabled,
			LastError:   firstNonEmpty(st.LastError, row.LastError),
			Source:      row.Source,
			InstalledAt: row.InstalledAt,
			Publisher:   row.Publisher,
			KeyID:       row.KeyID,
			Origin:      originOf(row.Origin),
		}
		if p := m.plugin(id); p != nil && len(p.Manifest().Provides) > 0 {
			item.Provides = providesOf(p.Manifest())
			if item.APIVersion == 0 {
				item.APIVersion = p.Manifest().APIVersion
			}
		}
		// The declared settings schema comes from the MANIFEST ON DISK, never from
		// the row: the row remembers what an Admin decided, and what a Plugin asks to
		// be configured with is the author's, changing with the file whenever it does.
		if fields := m.declaredFields(id); len(fields) > 0 {
			item.SettingsSchema = fields
			values, secrets := PublicSettingValues(fields, m.settingRows(id))
			item.Settings = &SettingsView{Values: values, Secrets: secrets}
		}
		if item.Provides == nil {
			item.Provides = []string{}
		}
		out = append(out, item)
		// Persist what the loader knows, when it differs from what the row says.
		if hasRow && loaded && m.store != nil && st.LastError != row.LastError {
			if err := m.store.SetPluginLastError(id, st.LastError); err != nil {
				m.logf("obelo: could not record the last error of plugin %s: %v", id, err)
			}
		}
	}
	// A Bundled plugin the Admin uninstalled has no files, no row and no loader
	// state, so it appears nowhere above. Its row joins the list here so the screen
	// can offer it back (ADR-0059 decision 2) — in id order with everything else,
	// because a declined plugin is not a second kind of thing and does not belong
	// in a second list.
	if declined := m.declinedRows(out); len(declined) > 0 {
		out = append(out, declined...)
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return out, nil
}

// originOf reads a row's origin, defaulting an empty column to OriginAdmin.
//
// The column has that default, so this only fires for a row this server never
// wrote — a database restored from before the migration, or a test's fake store.
// "Admin" is the honest answer for both: whatever put it there, it was not this
// binary's shipped set.
func originOf(raw string) string {
	if raw == "" {
		return OriginAdmin
	}
	return raw
}

// --- The rebuild-and-swap ------------------------------------------------------

// rebuild re-reads the whole plugins directory, builds a fresh registry from the
// Built-ins plus the Plugins the Admin has switched on, swaps it in, reloads
// everything composed from it, and closes the Set the old value pointed at.
//
// Caller holds mu.
func (m *Manager) rebuild(ctx context.Context) error {
	next, err := Load(ctx, m.dir, m.loader)
	if err != nil {
		// The same rule as boot: an unreadable plugins directory is a logged
		// warning and an empty set, never a failure of the thing the Admin asked
		// for (ADR-0001, and the ADR-0043 template).
		m.logf("obelo: installed plugins were not fully re-read: %v", err)
	}
	off, err := m.disabledIDs()
	if err != nil {
		_ = next.Close(ctx)
		return err
	}

	// The Bundled plugins, then the Built-ins, then the rest — the same sequence
	// app.New registers at boot, produced by the same function so the two cannot
	// drift (see RegisterEnabledAround).
	fresh := pluginapi.NewRegistry()
	next.RegisterEnabledAround(fresh, off, m.base)

	// The declared setting values are read onto the fresh Plugins BEFORE anything
	// can call them (issue 13): a guest handed a call with no values would read an
	// unconfigured source for exactly as long as the window lasted.
	m.applySettings(next)

	previous := m.set.Swap(next)
	m.registry.Swap(fresh)

	// Rebuild the live values composed FROM the registry, then release the old
	// runtimes. Both happen even if the reload fails: the registry has already
	// been swapped, so leaving the old Set open would leak a wazero runtime per
	// Plugin for the life of the process.
	var reloadErr error
	if m.reload != nil {
		reloadErr = m.reload(ctx)
	}
	if previous != nil && previous != next {
		_ = previous.Close(ctx)
	}
	return reloadErr
}

// install is Install with the lock already held, so InstallFromURL can fetch and
// install as one indivisible act.
func (m *Manager) install(ctx context.Context, manifestRaw, module, signatureRaw []byte, source string) (Installed, error) {
	man, err := decodeManifest(manifestRaw)
	if err != nil {
		return Installed{}, err
	}
	if len(module) == 0 {
		return Installed{}, refuse(ReasonModule,
			"there is no module: a plugin is a %s and a %s, and only the manifest arrived",
			ManifestFile, moduleFile(man))
	}
	if int64(len(module)) > MaxModuleBytes {
		return Installed{}, refuse(ReasonModule,
			"the module is %d bytes, and this server will not install one larger than %d",
			len(module), int64(MaxModuleBytes))
	}
	// WHOSE CODE IS THIS — asked here, between the manifest being understood and
	// anything being written, because this is the only point at which both the raw
	// manifest bytes and the module bytes are in hand and nothing has yet touched
	// the disk or the database. A server with no publisher keys pinned answers
	// "nobody asked" and this costs one query (signature.go).
	signedBy, err := m.checkSignature(man, manifestRaw, module, signatureRaw)
	if err != nil {
		return Installed{}, err
	}
	if err := m.checkDuplicate(man.ID); err != nil {
		return Installed{}, err
	}

	// Staged inside the plugins directory so the rename into place is on one
	// filesystem, and dot-prefixed so Load skips it if anything interrupts us.
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return Installed{}, fmt.Errorf("plugins: preparing %s: %w", m.dir, err)
	}
	staging := filepath.Join(m.dir, fmt.Sprintf(".install-%d-%d", os.Getpid(), m.staging.Add(1)))
	if err := os.MkdirAll(filepath.Join(staging, man.ID), 0o755); err != nil {
		return Installed{}, fmt.Errorf("plugins: preparing to install %s: %w", man.ID, err)
	}
	defer os.RemoveAll(staging)

	staged := filepath.Join(staging, man.ID)
	// The manifest is written as the author shipped it, byte for byte, and never
	// re-encoded. A re-encoded document is a different document, and a signature
	// over it (issue 15) would stop verifying for no reason anybody could see.
	if err := os.WriteFile(filepath.Join(staged, ManifestFile), manifestRaw, 0o644); err != nil {
		return Installed{}, fmt.Errorf("plugins: writing the manifest for %s: %w", man.ID, err)
	}
	if err := os.WriteFile(filepath.Join(staged, moduleFile(man)), module, 0o644); err != nil {
		return Installed{}, fmt.Errorf("plugins: writing the module for %s: %w", man.ID, err)
	}
	// The signature goes in beside them, byte for byte, whenever one arrived —
	// verified or not. It is provenance: an operator who pins a key next month can
	// check what they already have without fetching it again. The FILE is never the
	// claim that anything was verified; the row's publisher column is.
	if err := writeSignature(staged, signatureRaw); err != nil {
		return Installed{}, err
	}

	// COMPILE IT WHERE IT CANNOT BE FOUND. loadOne does everything boot does —
	// reads the manifest again from the file, checks the id against the directory,
	// compiles, checks the imports, builds the sandbox — and a Plugin that fails
	// any of it comes back disabled with the sentence that says why. Doing it here
	// is what makes "a module that fails to instantiate" a refusal rather than a
	// Plugin permanently listed as broken.
	probe := loadOne(ctx, staged, man.ID, m.loader)
	probeStatus := probe.Status()
	probe.close(ctx)
	if probeStatus.Disabled {
		return Installed{}, refuse(ReasonModule, "%s", probeStatus.LastError)
	}

	if err := os.Rename(staged, m.pluginDir(man.ID)); err != nil {
		return Installed{}, fmt.Errorf("plugins: installing %s: %w", man.ID, err)
	}
	if m.store != nil {
		if err := m.store.InsertPlugin(store.PluginInsert{
			ID:         man.ID,
			Name:       man.Name,
			Version:    man.Version,
			APIVersion: man.APIVersion,
			Provides:   providesOf(man),
			Source:     source,
		}); err != nil {
			// Nothing is left behind by a failed install, including here.
			_ = os.RemoveAll(m.pluginDir(man.ID))
			return Installed{}, err
		}
		if signedBy.Publisher != "" {
			if err := m.store.SetPluginSigner(man.ID, signedBy.Publisher, signedBy.KeyID); err != nil {
				// The Plugin IS installed and its signature DID verify; failing the whole
				// install over a display column would throw away the thing that worked.
				m.logf("obelo: plugin %s was installed but its publisher could not be recorded: %v", man.ID, err)
			}
		}
	}
	if err := m.rebuild(ctx); err != nil {
		return Installed{}, err
	}
	m.logf("obelo: plugin %s (%s %s) was installed from %s", man.ID, man.Name, man.Version, source)
	return m.view(man.ID)
}

// --- Refusals ------------------------------------------------------------------

// decodeManifest parses and validates a manifest document that is NOT on disk yet.
// It is the install-time half of readManifest, and it shares validateManifest with
// it so the two can never disagree about what a manifest may claim.
//
// The API-version mismatch is separated out because it is the one refusal that
// tells the Admin to go and upgrade something, and an API layer wants to say so
// with its own code.
func decodeManifest(raw []byte) (pluginapi.Manifest, error) {
	if len(raw) == 0 {
		return pluginapi.Manifest{}, refuse(ReasonManifest,
			"there is no %s: a plugin is a manifest and a module, and only the module arrived", ManifestFile)
	}
	if int64(len(raw)) > MaxManifestBytes {
		return pluginapi.Manifest{}, refuse(ReasonManifest,
			"the manifest is %d bytes, which is not a manifest", len(raw))
	}
	var m pluginapi.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return pluginapi.Manifest{}, refuse(ReasonManifest, "%s is not valid JSON: %v", ManifestFile, err)
	}
	if err := checkAPIVersion(m.APIVersion); err != nil {
		return pluginapi.Manifest{}, refuse(ReasonAPIVersion, "%s", err)
	}
	if err := validateManifest(m); err != nil {
		return pluginapi.Manifest{}, refuse(ReasonManifest, "%s", err)
	}
	return m, nil
}

// checkDuplicate refuses an id something already answers to, BEFORE a byte is
// written. Three ways it can be taken, and they are not the same sentence: a
// directory on disk, a row in the database, or a Built-in compiled into this
// binary. The last one matters most — installing over "webhook" would move an
// Admin's signing secret onto code the maintainer did not write.
func (m *Manager) checkDuplicate(id string) error {
	if _, err := os.Stat(m.pluginDir(id)); err == nil {
		return refuse(ReasonDuplicate,
			"a plugin with the id %q is already installed; uninstall it before installing another under the same id", id)
	}
	if m.registry != nil {
		_, sink := m.registry.EventSink(id)
		_, meta := m.registry.MetadataProvider(id)
		_, sub := m.registry.SubtitleProvider(id)
		if sink || meta || sub {
			return refuse(ReasonDuplicate,
				"the id %q is already claimed by a plugin this server ships; the plugin must choose another id", id)
		}
	}
	rows, err := m.rows()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == id {
			return refuse(ReasonDuplicate,
				"a plugin with the id %q is already installed; uninstall it before installing another under the same id", id)
		}
	}
	return nil
}

// checkSourceURL decides whether this server will fetch a plugin from a URL.
//
// # The first hop IS checked here, and only here
//
// safefetch documents — at length, and with a warning not to "improve" it — that
// it validates redirect targets and never the initial request, because an operator
// pointing a provider at a mirror on their own LAN is the point of this product.
// This function deliberately does the opposite, for one narrow case, and the
// difference is what is being fetched: every other URL in this server fetches
// DATA, and this one fetches CODE THIS SERVER WILL EXECUTE. "Paste a URL and I
// will run whatever comes back" is a different promise from "paste a URL and I
// will read a poster from it", and 169.254.169.254 answering with a module is not
// a deployment anybody meant to have.
//
// The cost is real and is accepted: an operator who wants to serve plugins from a
// box on their own LAN uploads the file instead, which is two clicks and no new
// trust. The acceptance criterion for this issue names the refusal by hand, which
// is the other half of why it is here.
func (m *Manager) checkSourceURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, refuse(ReasonSource,
			"the source must be an absolute http:// or https:// URL pointing at a plugin's %s", ManifestFile)
	}
	if m.allowPrivateSources {
		return u, nil
	}
	addrs, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if lookupErr != nil || len(addrs) == 0 {
		// Fails CLOSED, exactly as safefetch's redirect check does: a name this
		// server cannot resolve is not a name it will fetch code from.
		return nil, refuse(ReasonSource, "the host %q could not be resolved, so this server will not fetch a plugin from it", u.Hostname())
	}
	for _, a := range addrs {
		if safefetch.IsInternalIP(a.IP) {
			return nil, refuse(ReasonSource,
				"%s resolves to an address on this server's own network, and a plugin is code this server will run — upload the file instead",
				u.Hostname())
		}
	}
	return u, nil
}

// get fetches one of the two documents a URL install needs, under the safe
// fetcher's redirect policy and a byte cap. Everything that can go wrong with it
// is the Admin's URL being wrong, so everything that goes wrong is ReasonSource.
func (m *Manager) get(ctx context.Context, target string, limit int64, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, refuse(ReasonSource, "%s is not a URL this server can request", target)
	}
	req.Header.Set("User-Agent", "obelo/1.0 (self-hosted; plugin install)")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, refuse(ReasonSource, "the plugin's %s could not be fetched from %s: %v", what, target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, refuse(ReasonSource, "the plugin's %s at %s answered %d", what, target, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, refuse(ReasonSource, "the plugin's %s could not be read from %s: %v", what, target, err)
	}
	if int64(len(body)) > limit {
		// A refusal, never a truncation: a truncated module is one this server
		// would then try to compile, and a truncated manifest is one it would try
		// to parse.
		return nil, refuse(ReasonSource, "the plugin's %s at %s is larger than %d bytes", what, target, limit)
	}
	return body, nil
}

// getOptional is get for a document that is allowed not to exist — today, the
// detached signature.
//
// A 404 (or any other non-200) answers nil and no error, because MOST PLUGINS ARE
// UNSIGNED and an absent signature is not a failure of anything: whether it costs
// the install is the pinned-key policy's decision, made later and once. A
// transport failure is a refusal like any other, though, since "the source went
// away halfway through" must not silently become "there is no signature".
func (m *Manager) getOptional(ctx context.Context, target string, limit int64, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, refuse(ReasonSource, "%s is not a URL this server can request", target)
	}
	req.Header.Set("User-Agent", "obelo/1.0 (self-hosted; plugin install)")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, refuse(ReasonSource, "the plugin's %s could not be fetched from %s: %v", what, target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, refuse(ReasonSource, "the plugin's %s could not be read from %s: %v", what, target, err)
	}
	if int64(len(body)) > limit {
		return nil, refuse(ReasonSource, "the plugin's %s at %s is larger than %d bytes", what, target, limit)
	}
	return body, nil
}

// --- Small helpers -------------------------------------------------------------

func (m *Manager) pluginDir(id string) string { return filepath.Join(m.dir, id) }

func (m *Manager) plugin(id string) *Plugin {
	for _, p := range m.Plugins().Plugins() {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func (m *Manager) rows() ([]store.PluginRow, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.Plugins()
}

func (m *Manager) disabledIDs() ([]string, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.DisabledPluginIDs()
}

// mustBeInstalled refuses a verb aimed at a Plugin this server does not have.
func (m *Manager) mustBeInstalled(id string) error {
	if id == "" {
		return refuse(ReasonUnknown, "no plugin was named")
	}
	if _, err := os.Stat(m.pluginDir(id)); err == nil {
		return nil
	}
	rows, err := m.rows()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == id {
			return nil
		}
	}
	return refuse(ReasonUnknown, "no plugin with the id %q is installed", id)
}

// ensureRow makes sure a Plugin has a row before a switch is flipped on it. A
// Plugin an operator placed by hand has files and no row, and it would otherwise
// be the one Plugin on the screen whose buttons did nothing.
func (m *Manager) ensureRow(id string) error {
	if err := m.mustBeInstalled(id); err != nil {
		return err
	}
	if m.store == nil {
		return nil
	}
	rows, err := m.rows()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.ID == id {
			return nil
		}
	}
	man := pluginapi.Manifest{ID: id, Name: id}
	if p := m.plugin(id); p != nil && p.Manifest().ID != "" {
		man = p.Manifest()
	}
	return m.store.InsertPlugin(store.PluginInsert{
		ID:         id,
		Name:       man.Name,
		Version:    man.Version,
		APIVersion: man.APIVersion,
		Provides:   providesOf(man),
		Source:     "placed by hand",
	})
}

// view is one Plugin's row of List, for the response a verb answers with.
func (m *Manager) view(id string) (Installed, error) {
	list, err := m.List(context.Background())
	if err != nil {
		return Installed{}, err
	}
	for _, item := range list {
		if item.ID == id {
			return item, nil
		}
	}
	return Installed{}, refuse(ReasonUnknown, "no plugin with the id %q is installed", id)
}

// providesOf is the Extension points a manifest declares, as the contract's own
// tokens, for the row and for the screen.
func providesOf(m pluginapi.Manifest) []string {
	out := make([]string, 0, len(m.Provides))
	for _, p := range m.Provides {
		out = append(out, string(p.Kind))
	}
	return out
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled by the admin"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- Set and Plugin additions --------------------------------------------------

// RegisterEnabled is Register, minus the Plugins an Admin has switched off.
//
// A switched-off Plugin is NOT REGISTERED AT ALL, rather than registered with a
// factory that refuses. That is the difference between "disable stops delivery"
// being a property of the composition and being a property of whichever delivery
// path remembered to check a flag. The sink Manager builds from the registry, so a
// Plugin that is not in it has no worker, no queue and nothing to drop.
//
// It is also why a switched-off Plugin still shows its name and version on the
// Plugins screen: the Set still holds it, and the screen reads the Set, not the
// registry.
func (s *Set) RegisterEnabled(reg *pluginapi.Registry, off []string) {
	s.RegisterEnabledAround(reg, off, nil)
}

// RegisterEnabledAround is RegisterEnabled with the Built-ins in the middle, and
// it exists because REGISTRATION ORDER IS THE CATALOG ORDER (ADR-0059 decision 3).
//
// Three groups, in this sequence:
//
//  1. the Bundled plugins, in the order this server ships them
//     (Options.BundledFirst);
//  2. whatever base registers — the Built-ins, and whatever else the composition
//     root put there;
//  3. every other Installed plugin, alphabetically.
//
// The first group has to come first because the first authoritative Full provider
// of a kind is that kind's default lead (ADR-0027), the settings screen lists
// sources in registration order, and the music chain's two fill-only slots are
// filled in it. Before ADR-0059 those facts were properties of a slice in
// internal/enrich; they are now properties of this ordering, and a bundled TMDB
// registered after the remaining Built-ins would quietly stop leading video.
//
// It is ONE function rather than three calls at each site because there are two
// sites — the boot in app.New and the Manager's rebuild-and-swap — and they must
// produce the identical registry. "A rebuild produces what a reboot would" is the
// property every install, uninstall and enable depends on, and two hand-ordered
// call sequences are how it would come apart.
//
// A nil base is a server with no Built-ins to interleave, which is what a narrow
// test has.
func (s *Set) RegisterEnabledAround(reg *pluginapi.Registry, off []string, base func(*pluginapi.Registry)) {
	if reg == nil {
		return
	}
	if s == nil {
		if base != nil {
			base(reg)
		}
		return
	}
	skip := make(map[string]struct{}, len(off))
	for _, id := range off {
		skip[id] = struct{}{}
	}
	byID := make(map[string]*Plugin, len(s.plugins))
	for _, p := range s.plugins {
		byID[p.id] = p
	}
	done := make(map[string]struct{}, len(s.first))
	for _, id := range s.first {
		p, ok := byID[id]
		if !ok {
			continue // this server ships it, but it is not installed (declined, or removed)
		}
		done[id] = struct{}{}
		if _, offNow := skip[id]; offNow {
			continue
		}
		s.registerOne(reg, p)
	}
	if base != nil {
		base(reg)
	}
	for _, p := range s.plugins {
		if _, already := done[p.id]; already {
			continue
		}
		if _, offNow := skip[p.id]; offNow {
			continue
		}
		s.registerOne(reg, p)
	}
}

// Reenable forgets everything this server holds against a Plugin: that it is
// disabled, the sentence saying why, the consecutive-failure count and the
// allowlist-violation count.
//
// The violation count is cleared too, and that is a decision rather than an
// oversight. Violations never reset on their own, precisely so a Plugin that
// reaches for a forbidden host occasionally cannot clear its own record — but an
// ADMIN saying "try again" is a person deciding to forgive it, which is a
// different act from the Plugin behaving well for a while.
func (p *Plugin) Reenable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled = false
	p.lastError = ""
	p.failures = 0
	p.violations = 0
}
