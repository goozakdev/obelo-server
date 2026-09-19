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
	"github.com/goozakdev/obelo-server/internal/useragent"
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
	// DefaultMetadataCallBudget bounds ONE call into a Metadata provider guest,
	// and it is three times DefaultCallTimeout on purpose (ADR-0059 decision 6).
	//
	// A sink delivers one document to one receiver. A provider makes SEVERAL
	// fetches inside one lookup and — since the host-side throttle was withdrawn
	// and pacing became the guest's — deliberately waits between them. Ten seconds
	// for that is not a deadline, it is a coin flip on a slow source, and losing it
	// used to cost three strikes against the whole Plugin rather than one item's
	// backoff. A budget the guest can plan inside is what makes a deadline kill
	// mean what decision 6's threshold was written for: the guest itself spun.
	DefaultMetadataCallBudget = 30 * time.Second
	// DefaultMaxCallBudget is the ceiling a manifest's callBudgetMillis is clamped
	// to. Two minutes is long enough for any honest source and short enough that a
	// Plugin cannot hold an enrichment worker for an afternoon by declaring a
	// number. Over it is CLAMPED and logged, never refused: a manifest asking for
	// an hour is an author misjudging a number, not a Plugin that must not install.
	DefaultMaxCallBudget = 2 * time.Minute
	// DefaultFetchTimeout bounds one http_fetch.
	DefaultFetchTimeout = 20 * time.Second
	// DefaultFetchGrace is what a fetch leaves of the call budget for the guest to
	// answer in (ADR-0059 decision 6). Every fetch ends at least this long before
	// the call's own deadline, so a slow upstream comes back to the guest as a
	// fetch error it can turn into "unavailable" — instead of the deadline killing
	// the instance mid-request and costing the Plugin a strike for something the
	// far end did.
	//
	// A second is generous for "unmarshal a refusal and encode a response", which
	// is all the guest has left to do, and it is the margin rather than the answer:
	// a guest that needs longer than this to say one word is a guest that spun.
	DefaultFetchGrace = time.Second
	// DefaultMaxFetchBytes caps a response body handed back to a guest. A byte
	// payload costs what base64-in-JSON costs, and it lands in the guest's linear
	// memory, which only ever grows.
	DefaultMaxFetchBytes = 1 << 20
	// DefaultMaxFetchBytesCap is the ceiling a manifest's maxFetchBytes is clamped
	// to, and it is eight times the default rather than unbounded for the reason
	// the default exists at all: the bytes land in a linear memory that cannot
	// shrink, and the instance is recycled by a byte budget above it.
	DefaultMaxFetchBytesCap = 8 << 20
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
	// DefaultMaxKVKeyBytes and DefaultMaxKVValueBytes cap what one guest may store
	// in its namespace (issue 11). The namespace is for a cursor, an etag, a
	// token's expiry or a small response cache — ADR-0007 keeps blobs on disk, and
	// a generous cap here is how this table quietly becomes the blob store.
	DefaultMaxKVKeyBytes   = 256
	DefaultMaxKVValueBytes = 64 << 10
)

// Options are the knobs the composition root sets. The zero value is usable: every
// field falls back to its Default above.
type Options struct {
	// CallTimeout is the default budget for ONE call into a guest. It is the
	// Event sink's and the Subtitle provider's; a Metadata provider has its own
	// (MetadataCallBudget), because a lookup makes several fetches and paces
	// itself between them.
	CallTimeout time.Duration
	// MetadataCallBudget is the default budget for one Metadata provider call,
	// and MaxCallBudget is the ceiling a manifest may raise it to. A manifest
	// asking for more is CLAMPED with a line in the log at load, not refused
	// (ADR-0059 decision 6).
	MetadataCallBudget time.Duration
	MaxCallBudget      time.Duration
	FetchTimeout       time.Duration
	// FetchGrace is what every fetch leaves of the call budget for the guest to
	// answer in. A fetch ends at min(now+FetchTimeout, callDeadline−FetchGrace),
	// which is the whole of "a fetch always returns before the call deadline".
	FetchGrace time.Duration
	// MaxFetchBytes is the default body cap and MaxFetchBytesCap is the ceiling a
	// manifest's maxFetchBytes may raise it to, clamped and logged the same way a
	// call budget is.
	MaxFetchBytes    int64
	MaxFetchBytesCap int64
	FailureThreshold int
	RecycleBytes     int64
	// HTTPClient performs a guest's fetches. Nil means safefetch.Client, which is
	// the only sanctioned way to build one; a client supplied here is GUARDED
	// rather than trusted, so the redirect policy cannot be wired away.
	HTTPClient *http.Client
	// Logf is where the audit lines and the failure records go. Nil means the
	// server log.
	Logf func(format string, args ...any)

	// --- issue 11: the plugin-scoped key-value store -------------------------

	// KV backs the kv_get/kv_set/kv_delete host functions (ADR-0058 decision 5).
	// Nil is a legitimate configuration and means a server with NO key-value store:
	// every kv call answers a refusal naming that, and nothing else changes. A
	// Plugin that cannot cache a cursor is a slower Plugin, not a broken server
	// (ADR-0001), and a Set built by a narrow test needs no database.
	KV PluginKV
	// MaxKVKeyBytes and MaxKVValueBytes cap what one guest may store. Zero means
	// the Default. Exceeding either is a REFUSAL, never a truncation, for the
	// reason an oversize fetch is: a truncated document is one a guest parses as
	// complete.
	MaxKVKeyBytes   int
	MaxKVValueBytes int

	// --- .scratch/bundled-plugins issue 04: the shipped order ----------------

	// BundledFirst is the ids of the plugins this server SHIPPED, in the order it
	// ships them (internal/bundled.IDs). They are registered ahead of everything
	// else, and that ordering is the whole of what decides which source leads a
	// kind: the first authoritative Full provider of a kind is that kind's default
	// lead (ADR-0027), so TMDB leading video is a fact about this slice.
	//
	// It is a list of ids and not a reference to internal/bundled because the
	// arrow only goes one way — that package reads this one — and because a test
	// that wants to prove the ordering rule should be able to state an order
	// without shipping a module.
	//
	// Empty is a server with no Bundled plugins, which is every server before
	// ADR-0059 and every narrow test: everything then registers alphabetically,
	// exactly as it did.
	BundledFirst []string
}

// PluginKV is the durable half of the kv host functions — store.DB satisfies it.
// It is an interface here and not a *store.DB so that internal/plugins keeps
// depending on nothing it does not call, and so a test can hand a Set a map.
//
// EVERY method takes the plugin id first and it is never optional: the id comes
// from the manifest on disk, the host supplies it, and a guest never spells it.
// That is the whole of the isolation property.
//
// Dropping a whole namespace is deliberately NOT here. It is
// store.DeletePluginNamespace, called by UNINSTALL (issue 10), and a guest must
// have no path to it: a Plugin that could erase its own namespace wholesale would
// be a Plugin deciding it is not installed.
type PluginKV interface {
	PluginKV(pluginID, key string) (value []byte, found bool, err error)
	SetPluginKV(pluginID, key string, value []byte) error
	DeletePluginKV(pluginID, key string) error
}

func (o Options) withDefaults() Options {
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	if o.MetadataCallBudget <= 0 {
		o.MetadataCallBudget = DefaultMetadataCallBudget
	}
	if o.MaxCallBudget <= 0 {
		o.MaxCallBudget = DefaultMaxCallBudget
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = DefaultFetchTimeout
	}
	if o.FetchGrace <= 0 {
		o.FetchGrace = DefaultFetchGrace
	}
	if o.MaxFetchBytes <= 0 {
		o.MaxFetchBytes = DefaultMaxFetchBytes
	}
	if o.MaxFetchBytesCap <= 0 {
		o.MaxFetchBytesCap = DefaultMaxFetchBytesCap
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = DefaultFailureThreshold
	}
	if o.MaxKVKeyBytes <= 0 {
		o.MaxKVKeyBytes = DefaultMaxKVKeyBytes
	}
	if o.MaxKVValueBytes <= 0 {
		o.MaxKVValueBytes = DefaultMaxKVValueBytes
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

	// The two limits a manifest may RAISE, resolved once at load against the
	// host's caps (ADR-0059 decision 6) and immutable after, like hosts: they are
	// read on every call and every fetch and never from anything a guest said.
	//
	// metaCallBudget is the budget of one Metadata provider call. fetchLimit is
	// the body cap of one http_fetch — declared on the metadata entry, because
	// that is where a manifest says how big its documents are, and applied to
	// every fetch this Plugin makes, because one guest has one linear memory and a
	// second cap per seam would be a number nobody could predict from the file.
	metaCallBudget time.Duration
	fetchLimit     int64
	// userAgent is the identity every fetch carries: the host's own, with this
	// Plugin's id and version appended (ADR-0059 decision 7). Built once from the
	// manifest, so the string the far end sees can never be assembled from
	// something the guest said.
	userAgent string
	// droppedAgentOnce keeps the "you sent your own User-Agent" line to one per
	// Plugin per process. It is a debug note for the author, not an audit line
	// about the operator's server, and a provider that sets one sets it on every
	// single fetch.
	droppedAgentOnce sync.Once

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
	// refusalOnly says lastError was written by noteRefusal — a guest's own clean
	// error — and by nothing that counted against the Plugin. It is what lets the
	// next successful call retire that sentence while leaving a real failure's
	// sentence where an operator can still read it.
	refusalOnly bool

	// declared is this Plugin's manifest-declared setting VALUES (issue 13),
	// already decoded into the JSON shapes its own schema names. It is guarded by
	// mu because settings_get reads it, and mu is the lock a host function may
	// take. Nil is a Plugin that declares no fields, which is every Plugin written
	// before the schema existed.
	declared map[string]any

	// meta is the Metadata provider Extension point's per-call state (issue 11).
	// It lives in metadata.go; the field is here only because Plugin is.
	meta metaState
}

// SetSettingValues publishes the manifest-declared setting values this Plugin's
// guest reads. The Manager calls it after every load and after every save, so a
// saved setting reaches the next call without a rebuild-and-swap.
//
// The map is taken whole rather than merged: the manifest on disk decides which
// settings exist, so a field it no longer declares must stop being answerable.
func (p *Plugin) SetSettingValues(values map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.declared = values
}

// settingValues is the declared values as they travel into a call: a COPY, so a
// guest's document can never alias the map the host is about to reuse, and nil
// when there is nothing declared (an omitted key rather than an empty object).
func (p *Plugin) settingValues() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.declared) == 0 {
		return nil
	}
	out := make(map[string]any, len(p.declared))
	for k, v := range p.declared {
		out[k] = v
	}
	return out
}

// withSettingValues stamps the declared values onto the fixed Settings the host
// resolved for one call.
//
// It is called at CALL time and not when the Plugin is built, which is the whole
// of why a settings save takes effect immediately: the seam adapters hold the
// fixed half from the moment their factory ran, and the declared half is read
// fresh each time. It is also where SECRETS AT CALL TIME ONLY stays true of the
// declared secrets — they exist in a Settings value the host is handing into a
// call, and nowhere else.
func (p *Plugin) withSettingValues(s pluginapi.Settings) pluginapi.Settings {
	s.Values = p.settingValues()
	return s
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

// resolveLimits settles the numbers a manifest is allowed to move, ONCE, at
// load, against the host's own caps (ADR-0059 decision 6).
//
// Here rather than at call time for the reason the allowlist is read here: these
// are claims in a file, and a value re-read per call is a value that could be
// made to disagree with the log line the operator was shown. Over the cap is
// clamped and said out loud — an author who asked for ten minutes learns they got
// two, on the boot it happened, rather than wondering why their source keeps
// being cut off.
func (p *Plugin) resolveLimits() {
	p.metaCallBudget = p.opts.MetadataCallBudget
	p.fetchLimit = p.opts.MaxFetchBytes
	p.userAgent = useragent.ForPlugin(p.id, p.manifest.Version)

	for _, entry := range p.manifest.Provides {
		if entry.Kind != pluginapi.ExtensionMetadataProvider {
			continue
		}
		if entry.CallBudgetMillis > 0 {
			want := time.Duration(entry.CallBudgetMillis) * time.Millisecond
			if want > p.opts.MaxCallBudget {
				p.logf("obelo: plugin %s asks for a %s call budget; this server allows %s, so it is clamped",
					p.id, want, p.opts.MaxCallBudget)
				want = p.opts.MaxCallBudget
			}
			p.metaCallBudget = want
		}
		if entry.MaxFetchBytes > 0 {
			want := entry.MaxFetchBytes
			if want > p.opts.MaxFetchBytesCap {
				p.logf("obelo: plugin %s asks for a %d-byte fetch limit; this server allows %d, so it is clamped",
					p.id, want, p.opts.MaxFetchBytesCap)
				want = p.opts.MaxFetchBytesCap
			}
			p.fetchLimit = want
		}
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

// noteDroppedUserAgent says, ONCE, that this Plugin tried to set its own
// User-Agent and the host's was sent instead.
//
// A debug line and not an audit line, which is the distinction ADR-0059 draws: an
// audit line is "this Plugin reached for something it may not have" and an
// operator greps for those. This is "your header went nowhere" — a note to the
// AUTHOR about a header that was never going to travel, harmless, and worth
// saying exactly once because a Plugin that sets an agent sets it every time.
func (p *Plugin) noteDroppedUserAgent() {
	p.droppedAgentOnce.Do(func() {
		p.logf("obelo: plugin %s [debug] sent its own User-Agent; the host's identity is sent instead (%s)",
			p.id, p.userAgent)
	})
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
	p.refusalOnly = false
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
	p.refusalOnly = false
	if p.failures >= p.opts.FailureThreshold && !p.disabled {
		p.disabled = true
		p.logf("obelo: plugin %s is disabled after %d consecutive failures: %v", p.id, p.failures, err)
	}
}

// noteRefusal records the sentence behind a guest's own clean error WITHOUT
// counting a strike (ADR-0058 decision 7 as amended 2026-09-18). Two deliberate
// choices live here.
//
// It SURFACES: `lastError` is the one place an Admin is told anything about a
// Plugin, and "the guest refused the call: tmdb lookup: status 401" is the most
// actionable sentence this server can show them — it names the credential, not a
// broken module. A silently parked movie is not a diagnosis.
//
// It does NOT touch the failure streak, in either direction. A guest that ran and
// answered is neither a failure of the code (so it must not count) nor evidence
// that the code works (so it must not forgive a run of traps that is two deep).
// Leaving the counter alone is the reading that cannot turn a refusal into a way
// of resetting the threshold forever.
func (p *Plugin) noteRefusal(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastError = err.Error()
	p.refusalOnly = true
}

// clearFailures forgets a run of failures after a call that worked.
//
// It also clears `lastError` when the sentence there came from noteRefusal and
// nothing else. A failure's sentence is sticky — a Plugin that failed twice and
// then worked has still failed twice, and an operator should be able to read
// about it — but a refusal's is news about a CALL rather than about the Plugin,
// and a source that answered properly this time has retired it.
func (p *Plugin) clearFailures() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = 0
	if p.refusalOnly {
		p.lastError = ""
		p.refusalOnly = false
	}
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
	p.refusalOnly = false
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

// callPolicy is everything the EXTENSION POINT being called decides about one
// guest call. It is a parameter rather than a set of Plugin fields for the reason
// the budget always was: one module may fill two seams, and a sink's rules must
// not become a provider's because they share a directory.
type callPolicy struct {
	// budget bounds the call. Zero means the Plugin's default.
	budget time.Duration
	// refusalIsAnAnswer says a guest that ran to completion and cleanly answered
	// an error — errGuestRefused — has ANSWERED this call rather than failed it:
	// the instance is kept and no strike is counted. The error itself still
	// travels to the caller unchanged.
	//
	// True for a Metadata provider and for nothing else (ADR-0058 decision 7 as
	// amended 2026-09-18, ADR-0059). A provider's error is a claim about the
	// SOURCE — a rejected key, a document it cannot parse — which parks one item
	// under ADR-0048 and says nothing about whether the module works; three
	// lookups against a 401 used to take a whole provider off the server. A sink's
	// or a subtitle provider's error has no item to park and no other channel to
	// travel down, so for them a refusal stays what it has always been.
	refusalIsAnAnswer bool
}

// callGuest makes one call into the guest under the DEFAULT policy: the Event
// sink's and the Subtitle provider's. A Metadata provider calls callGuestUnder
// with its own (ADR-0059 decision 6).
func (p *Plugin) callGuest(ctx context.Context, export string, target string, req, out any) error {
	return p.callGuestUnder(ctx, callPolicy{budget: p.opts.CallTimeout}, export, target, req, out)
}

// callGuestUnder makes one call into the guest, serialized, under a deadline,
// with the instance lifecycle ADR-0058 decision 7 requires around it.
func (p *Plugin) callGuestUnder(ctx context.Context, policy callPolicy, export string, target string, req, out any) error {
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

	budget := policy.budget
	if budget <= 0 {
		budget = p.opts.CallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
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
	if errors.Is(err, errNoExport) {
		// NOT a trap (issue 11). The module simply does not export this call, which
		// is a fact about the module rather than a failure of the instance: nothing
		// ran, the guest's memory is untouched, and the next call is as likely to
		// work as the last one. So the instance is kept and no failure is counted —
		// only an optional Extension-point call reaches this, and its adapter turns
		// it into the same "unavailable" an undeclared capability answers.
		return err
	}
	if policy.refusalIsAnAnswer && errors.Is(err, errGuestRefused) {
		// The guest was entered, it ran, it decided it could not answer, and it came
		// back to say why. Its memory is intact and its next call is as likely to
		// work as its last one, so the instance is KEPT and no strike is counted —
		// the seam is the same one errNoExport above uses, for the same reason.
		//
		// The error travels on unchanged: a Metadata provider's caller turns it into
		// a parked item (ADR-0048, non-transient), which is exactly what the Go
		// providers this replaced did with a rejected key. What it must not do is
		// take the provider off the server; see callPolicy.
		p.noteRefusal(err)
		return err
	}
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
	// first is Options.BundledFirst, carried on the Set so that the registration
	// ORDER travels with the thing being registered. See RegisterEnabledAround.
	first []string
}

// Load reads dir — <dataDir>/plugins — and returns every Plugin under it, each
// either loaded or refused with the reason an operator should read.
//
// It returns an error ONLY when the directory itself cannot be listed, and not
// even then for the ordinary case: a directory that does not exist is a server
// with no Installed plugins, which is every server today.
func Load(ctx context.Context, dir string, opts Options) (*Set, error) {
	opts = opts.withDefaults()
	set := &Set{dir: dir, first: append([]string(nil), opts.BundledFirst...)}

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
	// The host's own numbers, in place BEFORE the manifest is read, so a Plugin
	// refused at any point below still has a budget, a byte cap and an identity
	// rather than three zeroes.
	p.resolveLimits()

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
	// Again, now that there is a manifest to read them off: the budget and the
	// byte cap it asks for, clamped to this server's, and the User-Agent its id
	// and version go into.
	p.resolveLimits()

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
		// The Metadata provider Extension point (issue 11). Its whole registration
		// is in metadata.go, beside the adapter, so this stays a dispatch.
		if entry.Kind == pluginapi.ExtensionMetadataProvider {
			s.registerMetadataProvider(reg, p, entry)
			continue
		}
		if entry.Kind != pluginapi.ExtensionEventSink {
			// An Extension point this build does not know. Saying so beats silence:
			// an operator who installs one learns why it did nothing.
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
