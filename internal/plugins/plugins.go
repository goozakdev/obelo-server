// Package plugins is the loader for INSTALLED plugins: the half of the Plugin
// system that runs code the maintainer did not write (ADR-0057 decision 6,
// ADR-0058).
//
// # What it does
//
// At boot it reads <dataDir>/plugins/<id>/, compiles each module into its own
// wazero sandbox, and registers what the manifest says it provides into the SAME
// pluginapi.Registry the Built-ins were registered into. From that point nothing
// downstream can tell an Installed plugin from a Built-in: the settings API lists
// it, the settings screen renders it, the event-sink Manager builds it from the
// same settings rows, and the translator hands it the same events. That sameness
// is the whole design — it is what makes "the contract was honest" a fact rather
// than a hope.
//
// # What it refuses
//
// Failure is the interesting half. A manifest naming an API version this server
// does not speak, a module that will not compile, one that imports a namespace the
// host did not offer, one whose id collides with a Plugin the server already has —
// each is recorded with its message and listed as disabled, and NONE of them can
// stop a boot (ADR-0001, and the ADR-0043 template: optional, off by default,
// cannot stop a boot). At runtime a guest that traps, spins past its deadline or
// reaches for a host its manifest does not allow is recorded the same way, and
// after a small number of consecutive failures the Plugin is disabled: it stays
// listed, with its last error, and stops being called.
//
// # What a guest can do
//
// Nothing, except answer the call it was given and use the two host functions in
// hostfuncs.go. There is no filesystem, no environment, no stdio, no clock beyond
// WASI's and no socket — a guest that tries to open one is talking to Go's
// in-process fake network stack inside its own linear memory (ADR-0058's sandbox
// probe). Its ONE way out is http_fetch, which the host performs, under the
// manifest's allowlist and the same redirect policy every outbound call in this
// server runs under.
//
// # Where the next slice picks up
//
// Load is callable more than once. Issue 10's install/uninstall flow rebuilds a
// Set and swaps it into a freshly built Registry rather than mutating a live one,
// which is why Load takes a directory and returns a value rather than writing into
// anything.
package plugins

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goozakdev/obelo-server/internal/safefetch"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// DirName is the folder under the data directory that holds Installed plugins,
// one directory per Plugin, named by the manifest's id (ADR-0007: blobs on disk,
// state in SQLite).
const DirName = "plugins"

// Defaults. Each is a number the first misbehaving Plugin will want changed, so
// each is an Option rather than a constant in the middle of a function.
const (
	// DefaultCallTimeout bounds ONE call into a guest. It sits comfortably inside
	// the event-sink host deadline, so a Plugin that spins is killed by its own
	// budget and the sink worker is free again rather than both expiring at once.
	DefaultCallTimeout = 10 * time.Second
	// DefaultFetchTimeout bounds one http_fetch.
	DefaultFetchTimeout = 20 * time.Second
	// DefaultMaxFetchBytes caps a response body handed back to a guest. A byte
	// payload costs what base64-in-JSON costs, and it lands in the guest's linear
	// memory, which only ever grows.
	DefaultMaxFetchBytes = 1 << 20
	// DefaultFailureThreshold is how many consecutive failures — traps, deadline
	// kills, refused answers — disable a Plugin. Small on purpose: "repeatedly" in
	// the issue means a pattern, and three in a row is a pattern.
	DefaultFailureThreshold = 3
	// DefaultRecycleBytes retires an instance after this many response bytes.
	// A guest's linear memory only ever grows — wasm cannot return a page — so a
	// long-lived instance answering large payloads eventually traps with an out of
	// bounds memory access (ADR-0058 decision 7 measured 560 calls at a mebibyte).
	// Rebuilding costs 0.03–1.74 ms; compiling, which is NOT repeated, costs
	// 43–606 ms.
	DefaultRecycleBytes = 64 << 20
)

// Options are the knobs the composition root sets. The zero value is usable: every
// field falls back to its Default above.
type Options struct {
	CallTimeout      time.Duration
	FetchTimeout     time.Duration
	MaxFetchBytes    int64
	FailureThreshold int
	RecycleBytes     int64
	// HTTPClient performs a guest's fetches. Nil means safefetch.Client, which is
	// the only sanctioned way to build one; a client supplied here is GUARDED
	// rather than trusted, so the redirect policy cannot be wired away.
	HTTPClient *http.Client
	// Logf is where the audit lines and the failure records go. Nil means the
	// server log.
	Logf func(format string, args ...any)
}

func (o Options) withDefaults() Options {
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	if o.MaxFetchBytes <= 0 {
		o.MaxFetchBytes = DefaultMaxFetchBytes
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = DefaultFailureThreshold
	}
	if o.RecycleBytes <= 0 {
		o.RecycleBytes = DefaultRecycleBytes
	}
	o.HTTPClient = safefetch.Guard(o.HTTPClient)
	if o.HTTPClient.Timeout == 0 {
		o.HTTPClient.Timeout = o.FetchTimeout
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	return o
}

// Status is one Installed plugin as an operator sees it: what it is, whether it is
// working, and — when it is not — the sentence that says why. It is host state, not
// contract data, which is why it lives here and not in pluginapi.
type Status struct {
	// ID is the manifest id, which is also the settings slug and the directory.
	ID string `json:"id"`
	// Name is the manifest name, or the directory when the manifest could not be
	// read at all.
	Name string `json:"name"`
	// Version is the author's own version string, for an operator to read.
	Version string `json:"version,omitempty"`
	// Disabled is true for a Plugin this server will not call: one that was
	// refused at load, and one that failed enough times at runtime to be stopped.
	Disabled bool `json:"disabled"`
	// LastError is why. Empty for a Plugin that is working.
	LastError string `json:"lastError,omitempty"`
}

// Plugin is one Installed plugin: its manifest, its compiled module, and the
// state that decides whether this server is still willing to call it.
//
// One instance, one call at a time, rebuilt on any trap or deadline kill and after
// a byte budget (ADR-0058 decision 7).
//
// TWO locks, and they are not interchangeable. callMu serializes calls into the
// guest and is held for the WHOLE call — including the time a host function is
// running, because a host function runs re-entrantly inside that call, on the
// guest's own stack. mu guards the state a host function reads and writes while
// that is happening. Folding them into one would deadlock the first time a guest
// called http_fetch.
type Plugin struct {
	id       string
	dir      string
	manifest pluginapi.Manifest
	opts     Options
	// hosts is the manifest allowlist, normalized once at load. Read on every
	// fetch and never rebuilt from anything a guest said, so it is immutable after
	// Load and needs no lock.
	hosts map[string]struct{}

	compiled *compiled

	// callMu serializes guest calls. One instance means one linear memory, and two
	// calls into it at once would share it.
	callMu   sync.Mutex
	instance *instance
	bytes    int64

	mu sync.Mutex
	// target is the host of the URL the Admin configured, for the duration of one
	// call. It is what makes "the operator chose this one" a fact the fetch policy
	// can read without the guest being able to claim it.
	target     string
	disabled   bool
	lastError  string
	failures   int
	violations int
}

// ID is the Plugin's stable identity: the manifest id, the settings slug and the
// directory on disk, all one string.
func (p *Plugin) ID() string { return p.id }

// Manifest is what the author declared. A copy, so a caller cannot edit the
// allowlist this server enforces.
func (p *Plugin) Manifest() pluginapi.Manifest { return p.manifest }

// Status reports what an operator should be shown.
func (p *Plugin) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := p.manifest.Name
	if name == "" {
		name = p.id
	}
	return Status{
		ID:        p.id,
		Name:      name,
		Version:   p.manifest.Version,
		Disabled:  p.disabled,
		LastError: p.lastError,
	}
}

func (p *Plugin) logf(format string, args ...any) { p.opts.Logf(format, args...) }

func (p *Plugin) httpClient() *http.Client { return p.opts.HTTPClient }

// audit writes the line an operator greps for when a Plugin is not doing what they
// expected. It names the Plugin, the host and the reason, and it is written even
// when the guest goes on to ignore the refusal, because the whole point is that
// the attempt is visible whatever the Plugin does about it.
func (p *Plugin) audit(host, reason string) {
	p.logf("obelo: plugin audit: refused a fetch: plugin=%s host=%s reason=%s",
		p.id, sanitizeLine(host), reason)
}

// allows reports whether the manifest's allowlist covers host. Exact and
// case-insensitive on the host with its PORT REMOVED, so one entry covers a source
// on any port — a port is not a security boundary, and an allowlist an operator
// cannot read at a glance is not doing its job.
func (p *Plugin) allows(host string) bool {
	_, ok := p.hosts[host]
	return ok
}

// operatorHost is the host of the URL the Admin typed for this Plugin, valid only
// while a call is in flight.
func (p *Plugin) operatorHost() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.target
}

// recordViolation counts a fetch the manifest did not license. Violations are
// counted SEPARATELY from call failures and never reset: a Plugin that reaches for
// a forbidden host on every third call and answers fine in between is still a
// Plugin doing something it said it would not, and letting a success clear the
// count would make it invisible.
func (p *Plugin) recordViolation(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.violations++
	p.lastError = detail
	if p.violations >= p.opts.FailureThreshold && !p.disabled {
		p.disabled = true
		p.logf("obelo: plugin %s is disabled after %d allowlist violations: %s", p.id, p.violations, detail)
	}
}

// recordFailure counts one failed call. Consecutive is the word that matters: a
// Plugin whose target is down for a minute recovers; one that fails every time is
// broken, and the threshold is what tells them apart.
func (p *Plugin) recordFailure(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures++
	p.lastError = err.Error()
	if p.failures >= p.opts.FailureThreshold && !p.disabled {
		p.disabled = true
		p.logf("obelo: plugin %s is disabled after %d consecutive failures: %v", p.id, p.failures, err)
	}
}

// clearFailures forgets a run of failures after a call that worked.
func (p *Plugin) clearFailures() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = 0
}

// beginCall takes the Plugin's permission to run and records the operator's
// target for the fetch policy, or reports why the call will not happen.
func (p *Plugin) beginCall(target string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		return fmt.Errorf("%w: %s", ErrDisabled, p.lastError)
	}
	p.target = target
	return nil
}

// endCall clears the operator's target so a later fetch can never borrow the last
// call's permission.
func (p *Plugin) endCall() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.target = ""
}

// refuse marks a Plugin unusable from the moment it is loaded. A refused Plugin is
// still REGISTERED, so an operator sees it on the settings screen with the reason,
// rather than wondering why the files they placed did nothing.
func (p *Plugin) refuse(err error) {
	p.disabled = true
	p.lastError = err.Error()
}

// Disabled reports whether this server will still call the Plugin.
func (p *Plugin) Disabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disabled
}

// ErrDisabled is what a call into a disabled Plugin answers. It reaches the sink
// worker, which counts it as a failed delivery — which is the honest number: the
// event was not delivered, and the reason is on the settings screen.
var ErrDisabled = errors.New("plugin is disabled")

// callGuest makes one call into the guest, serialized, under a deadline, with the
// instance lifecycle ADR-0058 decision 7 requires around it.
func (p *Plugin) callGuest(ctx context.Context, export string, target string, req, out any) error {
	p.callMu.Lock()
	defer p.callMu.Unlock()

	if p.compiled == nil {
		return fmt.Errorf("%w: the module was never loaded", ErrDisabled)
	}
	// The operator's URL for this call, readable by the fetch policy and by
	// nothing else.
	if err := p.beginCall(target); err != nil {
		return err
	}
	defer p.endCall()

	callCtx, cancel := context.WithTimeout(ctx, p.opts.CallTimeout)
	defer cancel()

	if p.instance == nil {
		inst, err := p.compiled.instantiate(callCtx)
		if err != nil {
			p.recordFailure(err)
			return err
		}
		p.instance = inst
		p.bytes = 0
	}

	n, err := p.instance.invoke(callCtx, export, req, out)
	p.bytes += int64(n)
	if err != nil {
		// A trap, a deadline kill or a guest that answered nothing: the instance is
		// not resumable — wazero CLOSES the module on a deadline — so it is dropped
		// and the next call rebuilds from the retained compiled artifact.
		p.instance.close(context.Background())
		p.instance = nil
		p.recordFailure(err)
		return err
	}
	if p.bytes >= p.opts.RecycleBytes {
		// The recycle budget. Retiring a healthy instance is not a failure and is
		// not counted as one.
		p.instance.close(context.Background())
		p.instance = nil
	}
	p.clearFailures()
	return nil
}

func (p *Plugin) close(ctx context.Context) {
	p.callMu.Lock()
	defer p.callMu.Unlock()
	if p.instance != nil {
		p.instance.close(ctx)
		p.instance = nil
	}
	if p.compiled != nil {
		_ = p.compiled.close(ctx)
		p.compiled = nil
	}
}

// Set is every Installed plugin one running server found on disk, refused ones
// included. It is a VALUE the composition root builds and hands around, exactly as
// the Registry is: issue 10's install flow builds a new Set and swaps it in rather
// than mutating a live one.
type Set struct {
	dir     string
	plugins []*Plugin
}

// Load reads dir — <dataDir>/plugins — and returns every Plugin under it, each
// either loaded or refused with the reason an operator should read.
//
// It returns an error ONLY when the directory itself cannot be listed, and not
// even then for the ordinary case: a directory that does not exist is a server
// with no Installed plugins, which is every server today.
func Load(ctx context.Context, dir string, opts Options) (*Set, error) {
	opts = opts.withDefaults()
	set := &Set{dir: dir}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return set, fmt.Errorf("plugins: reading %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	// A deterministic order, so the settings screen lists Plugins the same way on
	// every boot whatever the filesystem feels like.
	sort.Strings(names)

	for _, name := range names {
		set.plugins = append(set.plugins, loadOne(ctx, filepath.Join(dir, name), name, opts))
	}
	return set, nil
}

// loadOne reads one Plugin directory. It ALWAYS returns a Plugin, because a Plugin
// an operator placed and this server refused has to be visible; what it does not
// always return is a Plugin with a module behind it.
func loadOne(ctx context.Context, dir, dirName string, opts Options) *Plugin {
	p := &Plugin{id: dirName, dir: dir, opts: opts, hosts: map[string]struct{}{}}

	m, err := readManifest(dir)
	if err != nil {
		p.refuse(err)
		opts.Logf("obelo: plugin %s was not loaded: %v", dirName, err)
		return p
	}
	if m.ID != dirName {
		// The directory IS the id. A mismatch means the settings row and the files
		// would disagree about which Plugin this is, and the one thing worse than
		// refusing it is guessing which of the two is right.
		err := fmt.Errorf("the manifest id is %q but the directory is %q; they must match", m.ID, dirName)
		p.refuse(err)
		opts.Logf("obelo: plugin %s was not loaded: %v", dirName, err)
		return p
	}
	p.manifest = m
	for _, h := range m.Network.Hosts {
		p.hosts[normalizeHost(h)] = struct{}{}
	}

	wasm, err := os.ReadFile(filepath.Join(dir, moduleFile(m)))
	if err != nil {
		err = fmt.Errorf("reading %s: %w", moduleFile(m), err)
		p.refuse(err)
		opts.Logf("obelo: plugin %s was not loaded: %v", dirName, err)
		return p
	}

	// Compiling is the expensive half and happens exactly once per Plugin, here.
	c, err := newRuntime(ctx, wasm, &hostFuncs{p: p})
	if err != nil {
		p.refuse(err)
		opts.Logf("obelo: plugin %s was not loaded: %v", dirName, err)
		return p
	}
	p.compiled = c
	opts.Logf("obelo: plugin %s (%s) loaded", m.ID, m.Name)
	return p
}

// Plugins returns every Plugin in the Set, in id order. The slice is a copy.
func (s *Set) Plugins() []*Plugin {
	if s == nil {
		return nil
	}
	out := make([]*Plugin, len(s.plugins))
	copy(out, s.plugins)
	return out
}

// Status reports one Plugin's state by id, which is the settings slug. The
// settings API joins it onto the sink row so a refused or disabled Plugin is
// visible next to the Built-in it sits beside.
func (s *Set) Status(id string) (Status, bool) {
	if s == nil {
		return Status{}, false
	}
	for _, p := range s.plugins {
		if p.id == id {
			return p.Status(), true
		}
	}
	return Status{}, false
}

// Statuses reports every Installed plugin, in id order.
func (s *Set) Statuses() []Status {
	if s == nil {
		return nil
	}
	out := make([]Status, 0, len(s.plugins))
	for _, p := range s.plugins {
		out = append(out, p.Status())
	}
	return out
}

// Register adds every Plugin's registrations to reg — INCLUDING the refused ones,
// whose factory refuses with the reason. That is deliberate: a Plugin an operator
// placed must appear on the settings screen either way, and the alternative (a
// separate list of things that did not load) would be a second surface saying the
// same thing in a different vocabulary.
//
// A Plugin whose id is already claimed — by a Built-in, or by another directory —
// is NOT registered, and says so. Shadowing the Webhook would move an Admin's
// signing secret onto code the maintainer did not write.
func (s *Set) Register(reg *pluginapi.Registry) {
	if s == nil || reg == nil {
		return
	}
	for _, p := range s.plugins {
		s.registerOne(reg, p)
	}
}

func (s *Set) registerOne(reg *pluginapi.Registry, p *Plugin) {
	provides := p.manifest.Provides
	if len(provides) == 0 {
		// A Plugin refused before its manifest could be read still has to be
		// listed, and an Event sink is the only seam this slice registers into.
		provides = []pluginapi.ManifestProvides{{Kind: pluginapi.ExtensionEventSink}}
	}
	for _, entry := range provides {
		if entry.Kind == pluginapi.ExtensionSubtitleProvider {
			// The Subtitle provider seam (issue 12). Its whole branch is in
			// subtitle.go, beside the adapter it registers.
			s.registerSubtitleProvider(reg, p, entry)
			continue
		}
		if entry.Kind != pluginapi.ExtensionEventSink {
			// Metadata providers are issue 11. Saying so beats silence: an operator
			// who installs one today learns why it did nothing.
			p.logf("obelo: plugin %s provides %s, which this build does not load yet", p.id, entry.Kind)
			continue
		}
		if _, taken := reg.EventSink(p.id); taken {
			err := fmt.Errorf("the id %q is already claimed by another Plugin on this server", p.id)
			p.mu.Lock()
			p.refuse(err)
			p.mu.Unlock()
			p.logf("obelo: plugin %s was not registered: %v", p.id, err)
			continue
		}
		d := descriptorFor(p.manifest, entry)
		// The DIRECTORY is the identity, always. A Plugin refused before its
		// manifest could be parsed has no id of its own, and one whose manifest
		// disagreed with its directory was refused for saying so.
		d.Slug = p.id
		if d.Name == "" {
			d.Name = p.id
		}
		reg.RegisterEventSink(pluginapi.EventSinkRegistration{Descriptor: d, New: p.newEventSink})
	}
}

// Close releases every runtime and every live instance.
func (s *Set) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	for _, p := range s.plugins {
		p.close(ctx)
	}
	return nil
}
