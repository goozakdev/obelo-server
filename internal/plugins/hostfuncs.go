package plugins

import (
	"bytes"
	"context"
	"encoding/json"
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
func (h *hostFuncs) emit(ctx context.Context, mod api.Module, resp pluginapi.FetchResponse) uint64 {
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
