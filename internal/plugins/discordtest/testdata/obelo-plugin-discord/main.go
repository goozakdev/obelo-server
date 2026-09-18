//go:build wasm

// The Obelo Discord Event sink, whole, in one file.
//
// It is an Event sink: Obelo tells it when something terminal happened on the
// server — a scan finished, an enrichment pass finished, somebody pressed play or
// stop, a Library changed — and it posts a readable line to a Discord webhook.
// That is all it does, and a Plugin that does one thing is the shape this contract
// is for.
//
// It is also the REFERENCE plugin: every code sample in the server's authoring
// guide (docs/plugins/authoring.md) is lifted out of this file by a test, between
// the `// sample:begin NAME` / `// sample:end NAME` markers below, and the build
// fails if the guide and this file disagree. So nothing here is illustrative-
// but-slightly-wrong; if you can read it, it runs.
//
// # The ABI, in full
//
// Exports this file provides (the three fixed ones, plus the Event sink's one
// contract call):
//
//	obelo_alloc(size u32) -> ptr u32    the host asks for a buffer to write into
//	obelo_free(ptr u32)                 the host gives one back
//	deliver(ptr u32, len u32) -> i64    one event in, one answer out, as
//	                                    (ptr<<32 | len) of the response JSON — or 0
//	last_error() -> i64                 why the last call answered 0
//
// Imports the host provides, in module "obelo". A sink needs two of the six; the
// other four (kv_get, kv_set, kv_delete, settings_get) belong to the seams that
// have something to remember or eight calls to carry a secret through.
//
//	http_fetch(ptr u32, len u32) -> i64   the ONLY way out of the sandbox
//	log(level u32, ptr u32, len u32)      a line in the server log
//
// The guest owns every buffer on both sides. The host never invents a pointer: it
// calls obelo_alloc, writes, and frees — which is why `pinned` below can refuse a
// pointer it did not hand out, and why the answer to a fetch comes back as an
// entry this guest already holds.
//
// # What is NOT in here
//
// No filesystem, no environment, no socket, no stdout, no clock you can trust and
// no way to reach anything but the hosts manifest.json allowlists. None of it is
// missing: a Plugin's whole job is to answer the call it was given.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"unsafe"
)

func main() {}

// =============================================================================
// The shapes, copied from the contract's JSON schema
// =============================================================================
//
// pluginapi/v1/pluginapi.schema.json, $id urn:obelo:pluginapi:v1. An author in
// another language reads the schema and declares these in their own; this file
// declares them in Go and imports nothing, which is the honest demonstration that
// the schema is enough.
//
// Only the fields this Plugin uses are here. The contract leaves
// additionalProperties open on every type precisely so a guest may ignore what it
// does not need, and so a server one version ahead can add a field without
// breaking this module.

// sample:begin event

// EventEntity ($defs/EventEntity): one thing on the server — its id, the name an
// operator would recognise it by, and its kind. The id is always there; the name
// is best-effort and a sink must not identify anything by it.
type eventEntity struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// EventActor ($defs/EventActor): WHO a playback event belongs to, and the one
// field in the contract with a rule rather than a shape.
//
// Exactly one of UserID and LinkID is set. A session relayed from a linked server
// names the LINK and never the person watching on the other side, and Name is
// then the label THIS server's Admin typed for that linked server — never a name
// from the other household. See who() below: printing a person for a Link actor
// is the one thing this Plugin must never do.
type eventActor struct {
	UserID string `json:"userId,omitempty"`
	LinkID string `json:"linkId,omitempty"`
	Name   string `json:"name,omitempty"`
}

// EventScan ($defs/EventScan): the terminal counts of a Scanner pass.
type eventScan struct {
	TitlesFound int    `json:"titlesFound"`
	FilesFound  int    `json:"filesFound"`
	Added       int    `json:"added,omitempty"`
	Removed     int    `json:"removed,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// EventEnrich ($defs/EventEnrich): the terminal counts of an Enrichment pass.
// Retrying is deliberately apart from Failed — a transient blip will be tried
// again and raising an alarm about it is noise.
type eventEnrich struct {
	Total     int `json:"total"`
	Done      int `json:"done"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
	Failed    int `json:"failed,omitempty"`
	Disabled  int `json:"disabled,omitempty"`
	Retrying  int `json:"retrying,omitempty"`
}

// SinkEvent ($defs/SinkEvent): one curated event, whole. ID, Type and At are
// always present; everything else is omitted when the event has nothing to say
// there, so a sink reads one document shape per type rather than a union.
type sinkEvent struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	At      string       `json:"at"`
	Library eventEntity  `json:"library"`
	Title   eventEntity  `json:"title"`
	Actor   eventActor   `json:"actor"`
	Device  eventEntity  `json:"device"`
	Scan    *eventScan   `json:"scan"`
	Enrich  *eventEnrich `json:"enrich"`
}

// sample:end event

// sample:begin settings-shape

// Settings ($defs/Settings): the fixed shape every Plugin at every Extension
// point is configured through, plus Values — the settings THIS Plugin's own
// manifest declared, keyed by the key that declared them.
//
// Only the two halves this sink reads are declared. URL is the Target URL the
// Admin typed; see the note on it in README.md. Values is where the webhook URL,
// the templates and the mention switch arrive.
type settings struct {
	URL    string         `json:"url"`
	Values map[string]any `json:"values"`
}

// SinkDeliverRequest ($defs/SinkDeliverRequest): what the host hands a sink for
// one event. The settings travel WITH the call and are never installed into the
// guest — which is why a sink needs no settings_get, and why a guest rebuilt
// after a trap starts holding nobody's credential.
type deliverRequest struct {
	Event    sinkEvent `json:"event"`
	Settings settings  `json:"settings"`
}

// SinkDeliverResponse ($defs/SinkDeliverResponse): what a sink answers. There is
// no Outcome: delivery either happened or it did not, and everything that can go
// wrong is transport. Error is in the author's own words and reaches the server
// log and the Plugin's last-error on the Admin's screen, so it should name the
// target and the status rather than restate that something went wrong.
type deliverResponse struct {
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

// sample:end settings-shape

// sample:begin fetch-shape

// FetchRequest / FetchResponse ($defs/FetchRequest, $defs/FetchResponse): the
// host function that is the only way out.
//
// Exactly one of three things is true of a response: Refused is set (the host
// would not make this request, and retrying it unchanged will be refused again),
// Error is set (the host tried and the network failed, which may be worth
// retrying), or neither is and Status is what the target answered. A non-2xx
// status is NOT an error — it is an answer.
type fetchHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type fetchRequest struct {
	Method  string        `json:"method,omitempty"`
	URL     string        `json:"url"`
	Headers []fetchHeader `json:"headers,omitempty"`
	Body    []byte        `json:"body,omitempty"`
}

type fetchResponse struct {
	Status  int           `json:"status,omitempty"`
	Headers []fetchHeader `json:"headers,omitempty"`
	Body    []byte        `json:"body,omitempty"`
	Refused string        `json:"refused,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// sample:end fetch-shape

// --- Discord's own shape, which is nobody's contract but Discord's -----------

// discordMessage is the execute-webhook body. Translating between the curated
// event and this is the entire job of this Plugin.
//
// AllowedMentions is NOT optional and its zero value is not safe: a webhook with
// no allowed_mentions honours every @ in the content, so a Title called
// "@everyone Forever" would ping the server. Parse is therefore always present
// and always empty unless this Plugin put a role there on purpose.
type discordMessage struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	// Parse is the categories Discord may resolve on its own. An EMPTY, PRESENT
	// array means "resolve none of them", which is what this Plugin wants; a
	// missing key means "resolve all of them", which it never does.
	Parse []string `json:"parse"`
	// Roles is the explicit allowlist of role ids the content may ping.
	Roles []string `json:"roles,omitempty"`
}

// =============================================================================
// What this Plugin actually does
// =============================================================================

// sample:begin deliver

//go:wasmexport deliver
func deliver(ptr, n uint32) uint64 {
	// The host passes a pointer it got from obelo_alloc. A pointer that is not in
	// `pinned` is one this guest never handed out, and refusing it is what makes
	// "the guest owns every buffer on both sides" checkable from in here.
	buf, ok := pinned[ptr]
	if !ok || uint32(len(buf)) < n {
		return fail("the host passed a pointer this guest did not allocate")
	}
	var req deliverRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fail("the request is not a SinkDeliverRequest: " + err.Error())
	}
	return reply(post(req))
}

// sample:end deliver

// sample:begin post

// post renders the event and sends it to the Discord webhook the Admin pasted
// into this Plugin's settings.
//
// The webhook URL is a CREDENTIAL — anyone holding it can post to the channel —
// so it lives in a `secret` settings field, it is handed over only inside this
// call, and it is never written down, never logged and never cached in a package
// variable. A guest rebuilt after a trap must start with nothing.
func post(req deliverRequest) deliverResponse {
	cfg := configure(req.Settings)
	if cfg.webhookURL == "" {
		return deliverResponse{Error: "no Discord webhook URL is configured for this plugin"}
	}
	content := render(cfg, req.Event)
	if content == "" {
		// A message template an Admin cleared is an instruction not to post this
		// event type, not a failure — so the delivery succeeded and nothing was
		// sent. Reporting it as a failure would disable the Plugin for doing as it
		// was told.
		return deliverResponse{Delivered: true}
	}

	body, err := json.Marshal(discordMessage{
		Content:         content,
		AllowedMentions: cfg.mentions(),
	})
	if err != nil {
		return deliverResponse{Error: "the message could not be encoded: " + err.Error()}
	}

	resp := fetch(fetchRequest{
		Method:  "POST",
		URL:     cfg.webhookURL,
		Headers: []fetchHeader{{Name: "Content-Type", Value: "application/json"}},
		Body:    body,
	})
	switch {
	case resp.Refused != "":
		// The host would not make this request and will refuse it again, so there
		// is nothing to retry and nothing to be clever about. The refusal text is
		// the HOST's prose: report it, never branch on it.
		logLine(levelError, "the host refused the request: "+resp.Refused)
		return deliverResponse{Error: "refused by the host: " + resp.Refused}
	case resp.Error != "":
		return deliverResponse{Error: "the request to Discord failed: " + resp.Error}
	case resp.Status == 429:
		// Discord's rate limit. The host's per-call deadline is already ticking, so
		// sleeping in here would only get the instance killed; the honest answer is
		// a failure the operator can see on the sink's counters.
		return deliverResponse{Error: "Discord rate-limited this webhook (429)"}
	case resp.Status < 200 || resp.Status > 299:
		return deliverResponse{Error: "Discord answered " + itoa(resp.Status)}
	}
	logLine(levelInfo, "posted one "+req.Event.Type+" message to Discord")
	return deliverResponse{Delivered: true}
}

// sample:end post

// =============================================================================
// The settings, and the messages they produce
// =============================================================================

// sample:begin config

// The keys this Plugin's manifest declares. They are this author's own
// vocabulary — the host stores and returns them and never interprets one — and
// they must match manifest.json exactly, because a key the manifest does not
// declare is REFUSED at save rather than ignored.
const (
	keyWebhookURL  = "webhook_url"
	keyMentionRole = "mention_role"
	keyRoleID      = "role_id"

	// One template per curated event type. The key is the event type with its dot
	// turned into an underscore, so adding an event type to the contract is one
	// manifest line and one constant here.
	keyTemplateScan    = "template_scan_completed"
	keyTemplateEnrich  = "template_enrich_completed"
	keyTemplatePlayed  = "template_playback_started"
	keyTemplateStopped = "template_playback_stopped"
	keyTemplateChanged = "template_library_changed"
)

// config is the settings this delivery will use, read out of Settings.Values.
//
// It is a local value built per call and thrown away with the call. Nothing here
// is cached: a settings save reaches the very next delivery because the host
// stamps the declared values onto every call, and a Plugin that remembered them
// would be a Plugin serving the Admin's last-but-one answer.
type config struct {
	webhookURL  string
	mentionRole bool
	roleID      string
	values      map[string]any
}

func configure(s settings) config {
	return config{
		webhookURL:  strings.TrimSpace(stringValue(s.Values, keyWebhookURL)),
		mentionRole: boolValue(s.Values, keyMentionRole),
		roleID:      strings.TrimSpace(stringValue(s.Values, keyRoleID)),
		values:      s.Values,
	}
}

// mentions is the allowed_mentions block. Discord resolves EVERY @ in a message
// unless it is told not to, so the block is always present and always empty
// unless a role was configured on purpose.
func (c config) mentions() allowedMentions {
	m := allowedMentions{Parse: []string{}}
	if c.mentionRole && c.roleID != "" {
		m.Roles = []string{c.roleID}
	}
	return m
}

// sample:end config

// sample:begin render

// render turns one event into the line that will be posted, or "" for an event
// this Admin has switched off by clearing its template.
func render(c config, ev sinkEvent) string {
	tmpl, ok := c.template(ev.Type)
	if !ok || strings.TrimSpace(tmpl) == "" {
		return ""
	}
	out := substitute(tmpl, placeholders(ev))
	if c.mentionRole && c.roleID != "" {
		out = "<@&" + c.roleID + "> " + out
	}
	return out
}

// template is the message template for one event type, or "" when the contract
// grows a type this build has no template for — in which case this Plugin says
// nothing rather than inventing a line.
func (c config) template(eventType string) (string, bool) {
	var key string
	switch eventType {
	case "scan.completed":
		key = keyTemplateScan
	case "enrich.completed":
		key = keyTemplateEnrich
	case "playback.started":
		key = keyTemplatePlayed
	case "playback.stopped":
		key = keyTemplateStopped
	case "library.changed":
		key = keyTemplateChanged
	default:
		return "", false
	}
	value, present := c.values[key]
	if !present {
		// ABSENT and empty are different instructions. A key the Admin never filled
		// is absent from Values unless the manifest declared a default, so absence
		// here means "the manifest declares no default and nobody typed one" — post
		// nothing. An empty string means the Admin cleared it — also post nothing,
		// but deliberately.
		return "", false
	}
	text, _ := value.(string)
	return text, true
}

// placeholders is what a template may substitute, and the whole list of it. Every
// value is derived from the event document and from nothing else — there is no
// other source of truth in here, and there is no way to ask for one.
func placeholders(ev sinkEvent) map[string]string {
	p := map[string]string{
		"type":    ev.Type,
		"at":      ev.At,
		"library": nameOr(ev.Library, "a library"),
		"title":   nameOr(ev.Title, "a title"),
		"kind":    firstNonEmpty(ev.Title.Kind, ev.Library.Kind),
		"who":     who(ev.Actor),
		"device":  nameOr(ev.Device, ""),
		"count":   "",
		"total":   "",
		"files":   "",
		"scope":   "",
	}
	switch {
	case ev.Scan != nil:
		p["count"] = itoa(ev.Scan.TitlesFound)
		p["total"] = itoa(ev.Scan.TitlesFound)
		p["files"] = itoa(ev.Scan.FilesFound)
		p["scope"] = ev.Scan.Scope
	case ev.Enrich != nil:
		p["count"] = itoa(ev.Enrich.Matched)
		p["total"] = itoa(ev.Enrich.Total)
		p["files"] = itoa(ev.Enrich.Done)
	}
	return p
}

// sample:end render

// sample:begin who

// who is the attribution rule, and it is the one place in this Plugin where
// getting it wrong would be a privacy failure rather than a bug.
//
// A playback event that arrived over a Link carries the LINK's id and the label
// THIS server's Admin typed for it. The person on the other sofa is not a User
// here, their name never crossed the wire, and this Plugin must never print
// anything that reads as a person for such an event. So a Link actor is rendered
// as what it is — a linked server — and a local User as their display name.
//
// An event with neither is an unattributed session, which is what the host sends
// when it could not read the session's owner: unattributed is a smaller lie than
// misattributed, and this Plugin keeps it that way.
func who(a eventActor) string {
	switch {
	case a.LinkID != "":
		if a.Name == "" {
			return "a linked server"
		}
		return a.Name + " (a linked server)"
	case a.UserID != "":
		if a.Name == "" {
			return "someone on this server"
		}
		return a.Name
	default:
		return "someone"
	}
}

// sample:end who

// substitute replaces every {placeholder} the map knows. A brace pair naming
// something it does not know is left EXACTLY as the Admin typed it, so a typo
// shows up in the channel as a typo rather than as a silently missing word.
func substitute(tmpl string, values map[string]string) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(tmpl, '{')
		if open < 0 {
			b.WriteString(tmpl)
			return b.String()
		}
		close := strings.IndexByte(tmpl[open:], '}')
		if close < 0 {
			b.WriteString(tmpl)
			return b.String()
		}
		close += open
		b.WriteString(tmpl[:open])
		name := tmpl[open+1 : close]
		if value, ok := values[name]; ok {
			b.WriteString(value)
		} else {
			b.WriteString(tmpl[open : close+1])
		}
		tmpl = tmpl[close+1:]
	}
}

// nameOr is an entity's display name, or a fallback when the host could not read
// one. A sink shows the name and keys on the id; this Plugin only ever shows.
func nameOr(e eventEntity, fallback string) string {
	if e.Name != "" {
		return e.Name
	}
	if e.ID != "" && fallback == "" {
		return e.ID
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sample:begin values

// stringValue and boolValue read one declared setting out of Settings.Values.
//
// The values arrive as ordinary JSON of the type the manifest named — a `bool`
// field is true, never "true", and an `integer` is a number, never "7" — so the
// type assertion is the check. A key that is ABSENT is not the same as one set to
// its zero value, which is why these answer a bare zero rather than an error: an
// unset switch and a switch turned off mean the same thing to this Plugin, and
// nothing else here has to tell them apart.
func stringValue(values map[string]any, key string) string {
	s, _ := values[key].(string)
	return s
}

func boolValue(values map[string]any, key string) bool {
	b, _ := values[key].(bool)
	return b
}

// sample:end values

// =============================================================================
// The ABI glue an author writes once
// =============================================================================

// sample:begin alloc

// pinned keeps every buffer the host holds a pointer to reachable, and is how a
// host-supplied pointer becomes a slice again without pointer arithmetic: a
// pointer that did not come out of alloc simply is not in here.
//
// It is a plain map and needs no lock. One Plugin holds one guest instance and
// the host serialises every call through it, so two calls are never inside this
// module at once.
var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport obelo_free
func free(ptr uint32) {
	delete(pinned, ptr)
}

// lastError is why the last call answered 0. The host reads it through
// last_error() and puts it on the Admin's screen, so it is worth a sentence.
var lastError []byte

//go:wasmexport last_error
func lastErrorFn() uint64 {
	if len(lastError) == 0 {
		return 0
	}
	return emit(lastError)
}

// emit copies b into a fresh guest buffer and packs its pointer and length into
// the single i64 a wasm export may return.
func emit(b []byte) uint64 {
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

// fail is the 0 answer: no response, and a sentence saying why.
func fail(msg string) uint64 {
	lastError = []byte(msg)
	return 0
}

// reply encodes a response document and hands it back.
func reply(v any) uint64 {
	out, err := json.Marshal(v)
	if err != nil {
		return fail(err.Error())
	}
	lastError = nil
	return emit(out)
}

// sample:end alloc

// sample:begin hostfuncs

//go:wasmimport obelo http_fetch
func hostFetch(ptr, n uint32) uint64

//go:wasmimport obelo log
func hostLog(level, ptr, n uint32)

// The log levels. Anything else the host reads as info, because a bad number is
// not worth losing the operator's line over.
const (
	levelDebug uint32 = 0
	levelInfo  uint32 = 1
	levelWarn  uint32 = 2
	levelError uint32 = 3
)

// fetch asks the HOST to perform a request. The answer comes back in a buffer the
// host obtained from alloc, so it is already one of ours and freeing it is this
// guest's job.
func fetch(req fetchRequest) fetchResponse {
	in, err := json.Marshal(req)
	if err != nil {
		return fetchResponse{Error: err.Error()}
	}
	ptr := alloc(uint32(len(in)))
	copy(pinned[ptr], in)
	packed := hostFetch(ptr, uint32(len(in)))
	free(ptr)

	if packed == 0 {
		return fetchResponse{Error: "the host answered nothing"}
	}
	rptr, rlen := uint32(packed>>32), uint32(packed)
	buf, ok := pinned[rptr]
	if !ok || uint32(len(buf)) < rlen {
		return fetchResponse{Error: "the host answered with a buffer this guest does not hold"}
	}
	var resp fetchResponse
	uerr := json.Unmarshal(buf[:rlen], &resp)
	free(rptr)
	if uerr != nil {
		return fetchResponse{Error: uerr.Error()}
	}
	return resp
}

// logLine writes one line to the server log, prefixed by the host with this
// Plugin's id. It is why a guest needs no stdout.
//
// Never log a secret. The webhook URL is one, and nothing in this file puts it
// here.
func logLine(level uint32, msg string) {
	b := []byte(msg)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	hostLog(level, ptr, uint32(len(b)))
	free(ptr)
}

// sample:end hostfuncs

// itoa is strconv.Itoa under another name, kept so the samples above read the
// same in any language an author ports them to.
func itoa(n int) string { return strconv.Itoa(n) }

// levelDebug and levelWarn are part of the ABI an author is being shown even
// though this Plugin has nothing to say at either level.
var _ = [...]uint32{levelDebug, levelWarn}
