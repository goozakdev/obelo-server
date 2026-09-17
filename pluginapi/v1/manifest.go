package v1

// What an INSTALLED plugin is, on the wire (ADR-0058).
//
// A Built-in registers itself from the composition root, in Go, and its static
// facts are a Descriptor written beside its code. An Installed plugin cannot do
// that: it is a WebAssembly module on disk with no Go in it, possibly no Go at
// all, so the same facts arrive as a JSON document the author ships beside the
// module. That document is the Manifest, and the host turns it into exactly the
// Descriptor a Built-in would have registered — which is why nothing downstream
// of the registry can tell the two apart.
//
// # Trust
//
// A manifest is written by the author, so it is a CLAIM and never an authority.
// The host reads only the facts a Plugin is allowed to assert about itself — what
// it is called, what it provides, which hosts it wants to reach — and enforces
// every one of them from its own side. In particular Network.Hosts is checked
// host-side, against the file on disk, on every fetch; nothing the guest says at
// call time can widen it.
//
// # The ABI these types travel over
//
// The request and response of every guest call are the wire types in this
// package, JSON-encoded, passed through the guest's own linear memory
// (ADR-0058 decision 3). That is the whole reason they are here rather than in
// the loader: an author writing in another language has the JSON schema and five
// function signatures, and needs no Go and no PDK.

// APIVersion is the contract major this server speaks, and the only value a
// Manifest's APIVersion may carry for this build. It is the SAME number as the
// package's v1: a Plugin built against pluginapi/v1 declares 1, and a host that
// one day ships pluginapi/v2 declares 2 and refuses a 1 by name.
//
// It is checked when a Plugin is installed and at boot, never at call time
// (ADR-0058 decision 8): a Plugin is never half-loaded and never discovers the
// mismatch in the middle of somebody's scan.
const APIVersion = 1

// Manifest is the document an author ships beside a module: `manifest.json` in
// the Plugin's own directory. It is the Installed half of a Descriptor, plus the
// two things a Built-in never needs to declare because the compiler knew them —
// which API version it was built against, and which hosts it may reach.
type Manifest struct {
	// ID is the stable slug, and it plays exactly the role a Built-in's
	// Descriptor.Slug does: it is the key the settings row is written under, the
	// name in the settings API's routes, and the directory the module is installed
	// into. It must be unique across every Plugin on a server, Built-in included —
	// a Plugin claiming "webhook" is refused rather than shadowing the Built-in,
	// because the alternative is an Admin's signing secret moving to code the
	// maintainer did not write.
	ID string `json:"id"`
	// Name is the human name the settings screen shows.
	Name string `json:"name"`
	// Version is the AUTHOR's version of their own Plugin, an opaque display
	// string. The host never parses it, orders by it or decides anything from it;
	// it exists so an operator can tell which build they are running.
	Version string `json:"version,omitempty"`
	// APIVersion is the contract major this Plugin was built against. It MUST
	// equal the host's APIVersion; a mismatch is refused with a message naming
	// which side to upgrade (ADR-0058 decision 8).
	APIVersion int `json:"apiVersion"`
	// Provides is what this Plugin implements. A list rather than a single value
	// because one module may legitimately fill two seams — a source that both
	// supplies metadata and can be told when a scan finished — and because the
	// list is how a later Extension point arrives without the document changing
	// shape.
	Provides []ManifestProvides `json:"provides"`
	// Network is the outbound allowlist. Absent means the Plugin makes no outbound
	// requests at all, which is a legitimate and much safer Plugin.
	Network ManifestNetwork `json:"network,omitzero"`
	// Settings declares the fixed-shape settings this Plugin wants an Admin to
	// fill in. It is NOT a schema: the settings shape is fixed for every Plugin at
	// every Extension point (enabled, secret, url, url2, events), and what a
	// manifest may say about it is only which parts are required and what the
	// defaults are. A manifest-declared settings SCHEMA is a later addition that
	// arrives beside this field, never inside it.
	Settings ManifestSettings `json:"settings,omitzero"`
	// Description and DocsURL are the human-facing copy the settings screen shows,
	// exactly as a Built-in's Descriptor carries them.
	Description string `json:"description,omitempty"`
	DocsURL     string `json:"docsUrl,omitempty"`
	// Module is the file name of the WebAssembly module beside this manifest.
	// Empty means the conventional "plugin.wasm", which is what an author should
	// ship; the field exists so one directory can hold a module named after the
	// Plugin without the host guessing.
	Module string `json:"module,omitempty"`
}

// ManifestProvides is one Extension point this Plugin fills, with the static
// facts that seam needs. It is the manifest's half of a Descriptor: the host
// copies these fields onto the Descriptor it registers, so an Installed Plugin
// and a Built-in reach the registry as the same kind of thing.
type ManifestProvides struct {
	// Kind is which seam this entry fills. The field is named "kind" rather than
	// "extensionPoint" because that is the word the PRD's manifest uses and the
	// word an author types; it carries the ExtensionPoint values.
	Kind ExtensionPoint `json:"kind"`
	// Kinds are the coarse media-kind groups a provider serves. Empty and ignored
	// for an Event sink, which is not kind-scoped.
	Kinds []string `json:"kinds,omitempty"`
	// Role and Class are the chain position and the Authoritative eligibility a
	// provider DEFAULTS to (ADR-0027). They are a Plugin's claim about where it
	// belongs; the host's Enrichment policy decides whether to honour it, and only
	// a "full" Plugin may ever lead a Library.
	Role  Role  `json:"role,omitempty"`
	Class Class `json:"class,omitempty"`
	// Capabilities are the OPTIONAL operations this entry implements. The host
	// consults the declaration before calling, so an undeclared capability costs
	// no call into the guest and answers "unavailable" exactly as an unconfigured
	// source does.
	Capabilities []Capability `json:"capabilities,omitempty"`
	// RequiresSecret reports whether this seam needs a secret to be turned on. An
	// Event sink that signs its documents says true, for the reason the Webhook
	// Built-in does: enabling an unsigned sink hands the operator a receiver they
	// cannot defend.
	RequiresSecret bool `json:"requiresSecret,omitempty"`
}

// ManifestNetwork is the outbound allowlist, and it is the most load-bearing
// claim in the document — which is why it is the one the host trusts least.
//
// A Plugin never opens a socket: its only way out is the host's fetch function,
// the host performs the request through its own guarded fetcher, and the host
// checks the target against THIS list, read from the file on disk. A guest
// cannot widen it, cannot restate it, and does not get to know why a refusal
// happened beyond "refused".
type ManifestNetwork struct {
	// Hosts is the exact host names this Plugin may reach, without scheme, port
	// or path — "api.example.test", never "https://api.example.test/v1" and never
	// "*.example.test". Matching is exact and case-insensitive on the URL's host
	// with its port removed, so an entry covers a source on any port. A wildcard
	// is deliberately absent: the whole value of the list is that an operator can
	// read it and know what this code may talk to.
	Hosts []string `json:"hosts,omitempty"`
}

// ManifestSettings is what a manifest may declare about the fixed settings shape:
// which fields must be filled before the Plugin can be turned on, and what the
// defaults are. The shape itself is not negotiable (ADR-0057 consequences).
type ManifestSettings struct {
	// RequiresSecret is the Plugin-wide version of the per-seam flag: true when
	// this Plugin cannot work without a secret, whatever it provides.
	RequiresSecret bool `json:"requiresSecret,omitempty"`
	// DefaultURL and DefaultURL2 are the endpoints used when the operator sets no
	// override. For an Event sink there IS no sensible default target, so a sink
	// manifest leaves them empty and the Admin types the URL.
	DefaultURL  string `json:"defaultUrl,omitempty"`
	DefaultURL2 string `json:"defaultUrl2,omitempty"`
}

// --- the guest call, for the Event sink Extension point ----------------------

// SinkDeliverRequest is what the host hands an Installed Event sink for one
// event: the event itself and the Settings the Admin saved.
//
// The settings travel WITH the call and are never installed into the guest,
// which is the whole reason a separate "give me my settings" host function is not
// needed for a sink (ADR-0058 decision 5: secrets at call time only). A guest
// that is rebuilt after a trap therefore starts with no credential of anybody's,
// and a guest that stores one is storing it in memory that dies with the
// instance.
type SinkDeliverRequest struct {
	// Event is the curated event, exactly as the Webhook Built-in receives it.
	Event SinkEvent `json:"event"`
	// Settings carries URL (the target the Admin typed), Secret (the signing key)
	// and Events (what they subscribed to). The host has already filtered on
	// Events, so a guest is never handed an event it did not ask for and need not
	// check.
	Settings Settings `json:"settings"`
}

// SinkDeliverResponse is what an Installed Event sink answers.
//
// It carries no Outcome for the reason the Go EventSink interface returns only an
// error: a sink has no domain judgment to report. Delivery either happened or it
// did not, and everything that can go wrong is transport, which the host counts
// and then forgets.
type SinkDeliverResponse struct {
	// Delivered is true when the sink got the event where it was going. False with
	// an empty Error is still a failure — the host counts it and says so — but an
	// author who fills Error gives their operator something to read.
	Delivered bool `json:"delivered"`
	// Error is why delivery failed, in the author's own words. It reaches the
	// server log and the Plugin's last-error, so it should name the target and the
	// status rather than restating that something went wrong.
	Error string `json:"error,omitempty"`
}

// --- the host functions ------------------------------------------------------

// FetchRequest is what a guest asks the host to send. It is the ONLY way out: a
// Plugin has no sockets, and this request is performed by the HOST, through the
// same guarded fetcher every outbound call in this server uses.
//
// Three things the host decides and a guest cannot: whether the target's host is
// on the manifest allowlist, whether the address behind it is one this server
// will talk to, and how many bytes may come back. All three answer with a refusal
// rather than a lie, so an author can tell "you may not" from "it did not work".
type FetchRequest struct {
	// Method is the HTTP method. Empty means GET.
	Method string `json:"method,omitempty"`
	// URL is the absolute http(s) URL to fetch. Its host must appear in the
	// manifest's network.hosts or the request is refused unsent.
	URL string `json:"url"`
	// Headers are the request headers. The host sets its own User-Agent and fills
	// in Content-Length; anything else a Plugin needs — an Authorization header
	// built from its secret, a Content-Type — goes here.
	Headers []FetchHeader `json:"headers,omitempty"`
	// Body is the request body, base64-encoded by the JSON encoding as every byte
	// payload in this contract is.
	Body []byte `json:"body,omitempty"`
}

// FetchHeader is one header. A list of name/value pairs rather than a map because
// a header may legitimately repeat, and because the order an author wrote them in
// is the order they are sent.
type FetchHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// FetchResponse is what came back, or why nothing did.
//
// Exactly one of three things is true of any response: Refused is set (the host
// would not make this request), Error is set (the host tried and the network or
// the target failed), or neither is and Status is what the target answered. A
// non-2xx status is NOT an error here — it is an answer, and what to do about it
// is the Plugin's business.
type FetchResponse struct {
	// Status is the HTTP status the target answered, 0 when there was no answer.
	Status int `json:"status,omitempty"`
	// Headers are the response headers, in no guaranteed order.
	Headers []FetchHeader `json:"headers,omitempty"`
	// Body is the response body, truncated at the host's cap. A body that was cut
	// short is reported through Refused rather than silently shortened.
	Body []byte `json:"body,omitempty"`
	// Refused is the host declining to make or complete the request, in the host's
	// own words ("host not in allowlist"). The exact text is host-authored and a
	// Plugin must not branch on it: ANY non-empty Refused means this server would
	// not do this, and retrying it unchanged will be refused again. A refusal is
	// also an audit line in the server log naming the Plugin and the host.
	Refused string `json:"refused,omitempty"`
	// Error is a transport failure: the request was allowed, attempted, and did
	// not complete. Unlike a refusal it may be worth retrying.
	Error string `json:"error,omitempty"`
}
