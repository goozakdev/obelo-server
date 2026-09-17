package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/goozakdev/obelo-server/internal/safefetch"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The two host functions this slice grants, and nothing else (ADR-0058 decision 5
// names four; kv_get/kv_set and settings_get arrive with the Extension points that
// need them — a sink's settings ride WITH the call).
//
// A host function is the ONLY thing a guest can do that has an effect outside its
// own linear memory, so the set is closed the way the Extension points are, and
// each one is request-response and JSON-shaped like the contract itself.

// The refusal sentences a guest may be told. They are host-authored prose, not an
// enum: a Plugin must treat ANY non-empty Refused as "this server will not do
// this", and branching on the text is explicitly forbidden by the contract. They
// are constants here only so the log line and the guest's answer cannot drift.
const (
	refusedAllowlist  = "host not in allowlist"
	refusedBadURL     = "not an absolute http or https URL"
	refusedPrivate    = "the target resolves to an address this server will not let a plugin reach"
	refusedUnreached  = "the target could not be reached under this server's fetch policy"
	refusedOversize   = "the response is larger than a plugin may receive"
	refusedNoResponse = "the request could not be built"
)

// The audit reasons that appear in the log line. An operator greps for these.
const (
	auditAllowlist = "allowlist"
	auditPrivate   = "private-address"
	auditBadURL    = "bad-url"
	auditBlocked   = "fetch-policy"
	auditOversize  = "oversize"
)

// hostFuncs is the host half of the ABI for ONE Plugin. It closes over that
// Plugin, which is how `log` knows whose id to prefix and how `http_fetch` knows
// which allowlist to check — neither is ever read from something the guest said.
type hostFuncs struct {
	p *Plugin
}

// instantiate builds the "obelo" module. Both functions take and return the flat
// integer types a wasm import may use; everything structured travels as JSON in
// the guest's own memory.
func (h *hostFuncs) instantiate(ctx context.Context, rt wazero.Runtime) error {
	_, err := rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().WithFunc(h.httpFetch).Export("http_fetch").
		NewFunctionBuilder().WithFunc(h.log).Export("log").
		// --- issue 11: the remaining two of ADR-0058 decision 5's four -----------
		NewFunctionBuilder().WithFunc(h.kvGet).Export("kv_get").
		NewFunctionBuilder().WithFunc(h.kvSet).Export("kv_set").
		NewFunctionBuilder().WithFunc(h.kvDelete).Export("kv_delete").
		NewFunctionBuilder().WithFunc(h.settingsGet).Export("settings_get").
		// ------------------------------------------------------------------------
		Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("instantiating the %s host module: %w", hostModule, err)
	}
	return nil
}

// log writes one line to the server log, prefixed with the Plugin id. It is how a
// Plugin author debugs, and it is why a guest needs no stdout — which is why the
// sandbox can withhold stdio without taking anything away.
//
// A guest cannot make this line look like the server's own: the id is written by
// the host, from the manifest, and the level is a number the host maps.
func (h *hostFuncs) log(_ context.Context, mod api.Module, level, ptr, n uint32) {
	msg, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return
	}
	h.p.logf("obelo: plugin %s [%s] %s", h.p.id, levelName(level), sanitizeLine(string(msg)))
}

func levelName(level uint32) string {
	switch level {
	case LogDebug:
		return "debug"
	case LogWarn:
		return "warn"
	case LogError:
		return "error"
	default:
		// An unrecognized level is read as info rather than dropped: a bad number is
		// not worth losing the operator's line over.
		return "info"
	}
}

// sanitizeLine keeps a guest from forging log structure. A message with a newline
// in it could otherwise write a second line that looks like the server's own.
func sanitizeLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLogLine {
		s = s[:maxLogLine] + "…(truncated)"
	}
	return s
}

// maxLogLine bounds one guest log line. A Plugin that logs a megabyte is a Plugin
// filling the operator's disk.
const maxLogLine = 2000

// httpFetch is the only way out of the sandbox.
//
// The host performs the request. The guest supplies a URL and gets an answer or a
// refusal; it never holds a socket, a connection or a handle, and it is never told
// anything about the refusal beyond the sentence.
//
// The response is written into a buffer the GUEST allocates — the host calls back
// into obelo_alloc from inside this function rather than inventing an address,
// which is the ABI's one invariant (the guest owns every buffer on both sides).
func (h *hostFuncs) httpFetch(ctx context.Context, mod api.Module, ptr, n uint32) uint64 {
	var req pluginapi.FetchRequest
	if raw, ok := mod.Memory().Read(ptr, n); ok {
		if err := json.Unmarshal(raw, &req); err != nil {
			return h.emit(ctx, mod, pluginapi.FetchResponse{Error: "the request is not a FetchRequest: " + err.Error()})
		}
	} else {
		return h.emit(ctx, mod, pluginapi.FetchResponse{Error: "the request pointer is not in guest memory"})
	}
	return h.emit(ctx, mod, h.fetch(ctx, req))
}

// fetch is httpFetch without the memory, so it can be tested as the policy it is.
func (h *hostFuncs) fetch(ctx context.Context, req pluginapi.FetchRequest) pluginapi.FetchResponse {
	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		h.p.audit(req.URL, auditBadURL)
		return pluginapi.FetchResponse{Refused: refusedBadURL}
	}
	host := normalizeHost(target.Hostname())

	// THE ALLOWLIST, checked host-side against the manifest on disk and never
	// against anything the guest says (ADR-0058 decision 5).
	//
	// The Plugin's own configured target is permitted alongside it, and that is
	// not a hole: an author cannot know the URL an operator will type for their
	// own receiver, so a sink that could reach only manifest hosts could never
	// post anywhere. It is the OPERATOR's URL, which is the same asymmetry
	// safefetch documents for every other fetch in this server.
	operatorChose := host != "" && host == h.p.operatorHost()
	if !operatorChose && !h.p.allows(host) {
		h.p.audit(host, auditAllowlist)
		h.p.recordViolation(fmt.Sprintf("fetched %s, which its manifest does not allow", host))
		return pluginapi.FetchResponse{Refused: refusedAllowlist}
	}

	// A target the MANIFEST allowlists is checked against the same address rule
	// the redirect policy enforces, because the Plugin chose it. A target the
	// OPERATOR typed is not, because they did — a receiver on their own LAN is the
	// point of this product (ADR-0001), and it is their box either way.
	if !operatorChose {
		if refusal := h.refuseInternal(ctx, target.Hostname()); refusal != "" {
			h.p.audit(host, auditPrivate)
			h.p.recordViolation(fmt.Sprintf("fetched %s, which resolves into address space a plugin may not reach", host))
			return pluginapi.FetchResponse{Refused: refusal}
		}
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		h.p.audit(host, auditBadURL)
		return pluginapi.FetchResponse{Refused: refusedNoResponse}
	}
	httpReq.Header.Set("User-Agent", "obelo/1.0 (self-hosted; plugin "+h.p.id+")")
	for _, hd := range req.Headers {
		if hd.Name == "" {
			continue
		}
		httpReq.Header.Add(hd.Name, hd.Value)
	}

	resp, err := h.p.httpClient().Do(httpReq)
	if err != nil {
		// A refusal by the server's own fetch policy — a redirect into private
		// space, too many hops, a scheme change — is an AUDIT, not a network blip,
		// and it is reported to the guest as a refusal so it does not retry.
		if isPolicyRefusal(err) {
			h.p.audit(host, auditBlocked)
			h.p.recordViolation(fmt.Sprintf("fetched %s, which this server's fetch policy refused: %v", host, err))
			return pluginapi.FetchResponse{Refused: refusedUnreached}
		}
		return pluginapi.FetchResponse{Error: err.Error()}
	}
	defer resp.Body.Close()

	limit := h.p.opts.MaxFetchBytes
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		return pluginapi.FetchResponse{Status: resp.StatusCode, Error: readErr.Error()}
	}
	if int64(len(payload)) > limit {
		// Truncating and answering anyway would hand the guest a document it would
		// parse as complete. The honest answer is a refusal that names the reason.
		h.p.audit(host, auditOversize)
		return pluginapi.FetchResponse{Status: resp.StatusCode, Refused: refusedOversize}
	}

	out := pluginapi.FetchResponse{Status: resp.StatusCode, Body: payload}
	for name, values := range resp.Header {
		for _, v := range values {
			out.Headers = append(out.Headers, pluginapi.FetchHeader{Name: name, Value: v})
		}
	}
	return out
}

// refuseInternal fails CLOSED: a name that will not resolve is refused rather than
// handed to the transport, exactly as safefetch's redirect check does, because the
// alternative leaves the decision to a resolver whose answer we never saw.
func (h *hostFuncs) refuseInternal(ctx context.Context, hostname string) string {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
	if err != nil || len(addrs) == 0 {
		return refusedPrivate
	}
	for _, a := range addrs {
		if safefetch.IsInternalIP(a.IP) {
			return refusedPrivate
		}
	}
	return ""
}

// isPolicyRefusal reports whether an error is this server saying no rather than
// the network failing. Only safefetch's own refusal counts: everything else is a
// blip the Plugin may legitimately retry.
func isPolicyRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), safefetch.ErrRedirectBlocked.Error())
}

// emit writes a response into a buffer the GUEST allocates and answers with the
// packed pointer and length. A failure to allocate answers 0, which the guest
// reads as "no response" — the same sentinel a guest uses for its own failures,
// so an author has one thing to check rather than two.
//
// It takes `any` rather than one response type because every host function that
// answers something structured answers it the same way (issue 11 added three more
// of them); what varies is the document, and the document is JSON.
func (h *hostFuncs) emit(ctx context.Context, mod api.Module, resp any) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		return 0
	}
	alloc := mod.ExportedFunction(exportAlloc)
	if alloc == nil {
		return 0
	}
	res, err := alloc.Call(ctx, uint64(len(out)))
	if err != nil || len(res) == 0 {
		return 0
	}
	ptr := uint32(res[0])
	if !mod.Memory().Write(ptr, out) {
		return 0
	}
	return uint64(ptr)<<32 | uint64(len(out))
}

// ============================================================================
// issue 11 — kv_get / kv_set / kv_delete and settings_get
//
// The other two host functions ADR-0058 decision 5 names. They arrive here, with
// the Metadata provider Extension point, for the reason issue 09 gave for leaving
// them out: an Event sink needs neither, because its Settings ride WITH the call
// and it has nothing to remember between deliveries.
//
// They are request-response and JSON-shaped like everything else across this
// boundary, and every structured answer goes out through emit, which allocates
// through the GUEST's allocator — the ABI's one invariant.
// ============================================================================

// The refusal sentences the kv functions may answer with. Host-authored prose,
// like a fetch refusal: a Plugin must treat ANY non-empty Error as "this server
// will not do this" and must not branch on the text.
const (
	kvRefusedNoStore  = "this server has no key-value store for plugins"
	kvRefusedNoKey    = "a key-value entry needs a key"
	kvRefusedBigKey   = "the key is longer than a plugin may use"
	kvRefusedBigValue = "the value is larger than a plugin may store"
)

// kvGet reads one key from THIS Plugin's namespace.
//
// The namespace is the Plugin's id, taken from the manifest on disk by the host.
// A guest never spells it and there is no request field it could put one in, so
// "two Plugins writing the same key do not see each other's value" is a property
// of this line rather than of anybody's good behaviour.
func (h *hostFuncs) kvGet(ctx context.Context, mod api.Module, ptr, n uint32) uint64 {
	var req pluginapi.KVGetRequest
	if err := h.readRequest(mod, ptr, n, &req); err != nil {
		return h.emit(ctx, mod, pluginapi.KVGetResponse{Error: err.Error()})
	}
	if refusal := h.checkKey(req.Key); refusal != "" {
		return h.emit(ctx, mod, pluginapi.KVGetResponse{Error: refusal})
	}
	kv := h.p.opts.KV
	if kv == nil {
		return h.emit(ctx, mod, pluginapi.KVGetResponse{Error: kvRefusedNoStore})
	}
	value, found, err := kv.PluginKV(h.p.id, req.Key)
	if err != nil {
		h.p.logf("obelo: plugin %s: reading its key %q failed: %v", h.p.id, sanitizeLine(req.Key), err)
		return h.emit(ctx, mod, pluginapi.KVGetResponse{Error: "the key could not be read"})
	}
	return h.emit(ctx, mod, pluginapi.KVGetResponse{Found: found, Value: value})
}

// kvSet writes one key in THIS Plugin's namespace.
func (h *hostFuncs) kvSet(ctx context.Context, mod api.Module, ptr, n uint32) uint64 {
	var req pluginapi.KVSetRequest
	if err := h.readRequest(mod, ptr, n, &req); err != nil {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: err.Error()})
	}
	if refusal := h.checkKey(req.Key); refusal != "" {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: refusal})
	}
	if len(req.Value) > h.p.opts.MaxKVValueBytes {
		// A refusal, never a truncation. A guest handed back a shortened value would
		// parse it as its whole cursor, which is the failure mode that makes a size
		// cap worth having in the first place.
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: kvRefusedBigValue})
	}
	kv := h.p.opts.KV
	if kv == nil {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: kvRefusedNoStore})
	}
	if err := kv.SetPluginKV(h.p.id, req.Key, req.Value); err != nil {
		h.p.logf("obelo: plugin %s: writing its key %q failed: %v", h.p.id, sanitizeLine(req.Key), err)
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: "the key could not be written"})
	}
	return h.emit(ctx, mod, pluginapi.KVWriteResponse{OK: true})
}

// kvDelete removes one key from THIS Plugin's namespace. Removing a key that was
// never written is not an error — it is the state the guest asked for.
//
// It does NOT drop the namespace: that is store.DeletePluginNamespace, which
// UNINSTALL calls, and a guest has no way to ask for it. A Plugin that could erase
// its own installation record would be a Plugin deciding it is not installed.
func (h *hostFuncs) kvDelete(ctx context.Context, mod api.Module, ptr, n uint32) uint64 {
	var req pluginapi.KVDeleteRequest
	if err := h.readRequest(mod, ptr, n, &req); err != nil {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: err.Error()})
	}
	if refusal := h.checkKey(req.Key); refusal != "" {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: refusal})
	}
	kv := h.p.opts.KV
	if kv == nil {
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: kvRefusedNoStore})
	}
	if err := kv.DeletePluginKV(h.p.id, req.Key); err != nil {
		h.p.logf("obelo: plugin %s: deleting its key %q failed: %v", h.p.id, sanitizeLine(req.Key), err)
		return h.emit(ctx, mod, pluginapi.KVWriteResponse{Error: "the key could not be deleted"})
	}
	return h.emit(ctx, mod, pluginapi.KVWriteResponse{OK: true})
}

// settingsGet hands the guest the Settings the HOST resolved for the call it is
// currently inside: the Admin's enabled flag, the decrypted secret, the effective
// URLs, the server-wide metadata language and the operator's rate policy.
//
// It takes no request, because there is nothing to ask for: a Plugin has exactly
// one settings row and it is its own.
//
// SECRETS AT CALL TIME ONLY (ADR-0058 decision 5). Nothing is installed into the
// guest and nothing is persisted on its behalf; the answer exists only while a
// call the host made is on the stack, and outside one this answers a ZERO
// Settings — enabled false, no secret. That is why a Metadata provider needs this
// function at all where an Event sink did not: a sink has one call and its
// Settings ride with it, while a provider has eight, and eight per-call envelopes
// carrying the same document would be eight places for a secret to be forgotten.
func (h *hostFuncs) settingsGet(ctx context.Context, mod api.Module) uint64 {
	return h.emit(ctx, mod, h.p.currentSettings())
}

// readRequest decodes one host-function request out of guest memory. The pointer
// is the guest's own — the host never fabricates one — so a pointer that is not in
// its memory is the guest having made a mistake, and it is told so.
func (h *hostFuncs) readRequest(mod api.Module, ptr, n uint32, out any) error {
	raw, ok := mod.Memory().Read(ptr, n)
	if !ok {
		return errors.New("the request pointer is not in guest memory")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the request is not the shape this call takes: %w", err)
	}
	return nil
}

// checkKey is the key half of the size cap, shared by the three kv calls so a
// guest gets the same refusal whichever one it used.
func (h *hostFuncs) checkKey(key string) string {
	switch {
	case key == "":
		return kvRefusedNoKey
	case len(key) > h.p.opts.MaxKVKeyBytes:
		return kvRefusedBigKey
	}
	return ""
}
