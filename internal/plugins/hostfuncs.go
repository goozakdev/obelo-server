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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

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
	refusedOffline    = "this call has no network"
)

// errFetchDeadline is what a fetch answers when the call's own budget ran out
// (ADR-0059 decision 6). It is an ERROR and not a refusal, deliberately: the host
// was willing, the request was allowed, and the far end was simply too slow —
// which is the same thing a connection refusal is, and the same thing a Plugin
// should answer "unavailable" to so the item takes ADR-0048's backoff.
//
// The same sentence covers both halves of the rule, because they are the same
// fact from the guest's side: a request that ran out of budget mid-flight, and
// one there was no budget left to start. A guest that can tell them apart would
// only be tempted to branch on the difference.
const errFetchDeadline = "the fetch did not finish before this call's deadline"

// The audit reasons that appear in the log line. An operator greps for these.
const (
	auditAllowlist = "allowlist"
	auditPrivate   = "private-address"
	auditBadURL    = "bad-url"
	auditBlocked   = "fetch-policy"
	auditOversize  = "oversize"
	auditOffline   = "no-network-call"
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
		// The socket grant (ADR-0064): exported to every guest, so a module links
		// the same everywhere, and refused to every call but a sign-in call into a
		// Plugin that declares it (socket.go).
		NewFunctionBuilder().WithFunc(h.socket).Export("socket").
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
	if h.p.secretCall() {
		// The guest was handed a credential, so nothing it writes is logged: one
		// fixed line says that it wrote something, and the rest are dropped.
		if !h.p.withheldLog {
			h.p.withheldLog = true
			h.p.logf("obelo: plugin %s: its log lines during a call that carries a credential are withheld", h.p.id)
		}
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
// in it could otherwise write a second line that looks like the server's own, and
// an escape sequence could drive an operator's terminal; every control rune and
// the Unicode line separators become a space. Truncation lands on a rune boundary.
func sanitizeLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxLogLine {
		cut := maxLogLine
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…(truncated)"
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
	// A call made under a no-network policy — a Web reference provider's — has no
	// way out at all. Refused FIRST, before the URL is so much as parsed, so no
	// name is resolved and nothing reaches the fetcher; and counted as a
	// violation, because a pure computation reaching for the network is a Plugin
	// doing something its Extension point says it does not.
	if h.p.offlineCall() {
		h.p.audit(req.URL, auditOffline)
		h.p.recordViolation("tried to fetch during a call that has no network")
		return pluginapi.FetchResponse{Refused: refusedOffline}
	}
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
	// safefetch documents for every other fetch in this server — and it is
	// exactly the host AND port they typed: their host on another port is a
	// manifest fetch like any other. A second URL they typed (an image host, a
	// CDN) is theirs the same way, at its own host and port.
	//
	// So are the token and userinfo endpoints the issuer's discovery document
	// names, once a redirect Sign-in provider has read it during this call (Google's
	// live on other hosts) — each at exactly the https origin named. The issuer is
	// the operator's, but its SERVER chose those, so off the issuer's own host they
	// still get the address check below; on it, the named origin is the
	// operator's as the issuer is.
	addr := dialKey(host, targetPort(target))
	named := h.p.discoveryNamed(target)
	operatorChose := false
	for _, operator := range h.p.operatorAddrs() {
		operatorHost, _, _ := net.SplitHostPort(operator)
		if host != "" && (addr == operator || (named && host == operatorHost)) {
			operatorChose = true
		}
	}
	// An address a test named (Options.ExemptFetchAddrs) is treated as the
	// operator's, exactly that host and port. Production names none.
	if !operatorChose && slices.Contains(h.p.opts.ExemptFetchAddrs, addr) {
		operatorChose = true
	}
	issuerNamed := !operatorChose && named
	if !operatorChose && !issuerNamed && !h.p.allows(host) {
		h.p.audit(host, auditAllowlist)
		h.violation("allowlist", fmt.Sprintf("fetched %s, which its manifest does not allow", host))
		return pluginapi.FetchResponse{Refused: refusedAllowlist}
	}

	// THE DEADLINE, before anything is sent (ADR-0059 decision 6).
	//
	// Every request this function makes — the name lookup below included — ends at
	// min(now+FetchTimeout, callDeadline−FetchGrace), so the guest is handed an
	// answer while it still has time to turn it into one of its own. A budget that
	// is already spent means no request at all: the honest answer is available
	// without asking anybody.
	fctx, cancel, ok := h.deadline(ctx)
	if !ok {
		return pluginapi.FetchResponse{Error: errFetchDeadline}
	}
	defer cancel()

	// A target the MANIFEST allowlists is checked against the same address rule
	// the redirect policy enforces, because the Plugin chose it. A target the
	// OPERATOR typed is not, because they did — a receiver on their own LAN is the
	// point of this product (ADR-0001), and it is their box either way.
	if !operatorChose {
		if refusal := h.refuseInternal(fctx, target.Hostname()); refusal != "" {
			h.p.audit(host, auditPrivate)
			h.violation("private address", fmt.Sprintf("fetched %s, which resolves into address space a plugin may not reach", host))
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
	// The operator's address is exempt at the dial as it is above, and ONLY
	// that host on this port: every other dial this request makes, a redirect's
	// included, is checked against the address actually connected to.
	client := h.p.httpClient()
	if operatorChose {
		client = h.p.opts.operatorClient
		fctx = context.WithValue(fctx, exemptAddrKey{}, addr)
	}
	httpReq, err := http.NewRequestWithContext(fctx, method, target.String(), body)
	if err != nil {
		h.p.audit(host, auditBadURL)
		return pluginapi.FetchResponse{Refused: refusedNoResponse}
	}
	// THE IDENTITY IS THE HOST'S (ADR-0059 decision 7).
	//
	// The guest's headers go on first and the agent goes on last, with Set rather
	// than Add, so exactly one User-Agent leaves this server whatever the guest
	// sent. The previous line stamped a hardcoded "obelo/1.0" and then ADDED every
	// guest header, so a Plugin that identified itself produced two agents and a
	// version that had not existed since 0.1.0.
	//
	// A guest-supplied agent is DROPPED rather than appended. Code running inside
	// Obelo's fetcher, under Obelo's allowlist and Obelo's fetch policy, is Obelo
	// as far as the far end is concerned, and MusicBrainz — which requires the
	// shape and throttles anything else — has to be able to rely on that. The
	// Plugin is named in the agent's own comment instead, which tells the far end
	// which part called without letting the guest write the line.
	var sentOwnAgent bool
	for _, hd := range req.Headers {
		if hd.Name == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(hd.Name), "user-agent") {
			sentOwnAgent = true
			continue
		}
		httpReq.Header.Add(hd.Name, hd.Value)
	}
	httpReq.Header.Set("User-Agent", h.p.userAgent)
	if sentOwnAgent {
		// A debug note for the AUTHOR, once per Plugin — not an audit line about
		// the operator's server, and not one per fetch: a provider that sets an
		// agent sets it on every request it ever makes.
		h.p.noteDroppedUserAgent()
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		// A refusal by the server's own fetch policy — a redirect into private
		// space, too many hops, a scheme change — is an AUDIT, not a network blip,
		// and it is reported to the guest as a refusal so it does not retry.
		if isPolicyRefusal(err) {
			h.p.audit(host, auditBlocked)
			h.violation("redirect", fmt.Sprintf("fetched %s, which this server's fetch policy refused: %v", host, err))
			return pluginapi.FetchResponse{Refused: refusedUnreached}
		}
		return pluginapi.FetchResponse{Error: fetchErrorText(err)}
	}
	defer resp.Body.Close()

	limit := h.p.fetchLimit
	payload, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		// A body that stopped arriving because the budget ran out says so in the
		// budget's words: it is the same fact as a request that never completed,
		// and the status is kept because the target did answer.
		return pluginapi.FetchResponse{Status: resp.StatusCode, Error: fetchErrorText(readErr)}
	}
	if int64(len(payload)) > limit {
		// Truncating and answering anyway would hand the guest a document it would
		// parse as complete. The honest answer is a refusal that names the reason.
		h.p.audit(host, auditOversize)
		return pluginapi.FetchResponse{Status: resp.StatusCode, Refused: refusedOversize}
	}

	h.p.noteDiscovery(resp.Request, resp.StatusCode, payload)
	out := pluginapi.FetchResponse{Status: resp.StatusCode, Body: payload}
	for name, values := range resp.Header {
		for _, v := range values {
			out.Headers = append(out.Headers, pluginapi.FetchHeader{Name: name, Value: v})
		}
	}
	return out
}

// violation records a fetch the policy refused, as detail — or, during a call
// that carries a credential, as a fixed sentence and kind: detail quotes a host
// and an error the guest's URL chose, and either can carry the credential.
func (h *hostFuncs) violation(kind, detail string) {
	if h.p.secretCall() {
		detail = "a fetch during a call that carries a credential was blocked: " + kind
	}
	h.p.recordViolation(detail)
}

// deadline derives the context ONE fetch runs under, and it is the whole of
// "a fetch always returns before the call deadline" (ADR-0059 decision 6).
//
// It ends at min(now+FetchTimeout, callDeadline−FetchGrace). The first term is
// the per-fetch bound this server has always had; the second is the new one, and
// it is what makes a slow upstream cost an item its backoff instead of costing
// the Plugin a strike. Under ADR-0058 a fetch ran under the call's OWN context,
// so a source that took longer than the call budget killed the guest at the
// deadline — and a kill is counted, so three slow lookups disabled a whole
// provider for something the far end did.
//
// The returned context is a CHILD of the call's, so a cancelled call still stops
// a fetch immediately; what it never does is outlive the guest's chance to answer.
//
// ok is false when there is no time left to spend. The caller answers the same
// error without asking anybody, because that is the true answer and because a
// zero-length deadline would otherwise become a request the host makes and
// abandons.
func (h *hostFuncs) deadline(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	end := time.Now().Add(h.p.opts.FetchTimeout)
	if callEnd, ok := ctx.Deadline(); ok {
		if latest := callEnd.Add(-h.p.opts.FetchGrace); latest.Before(end) {
			end = latest
		}
	}
	if !end.After(time.Now()) {
		return ctx, func() {}, false
	}
	fctx, cancel := context.WithDeadline(ctx, end)
	return fctx, cancel, true
}

// fetchErrorText is what a failed request tells the guest. A deadline gets the
// budget's sentence rather than Go's "context deadline exceeded" — an author
// reading their own log should learn which of this server's rules they met, and
// the transport's wording is neither stable nor about them.
//
// Everything else is passed through verbatim: a connection refused, a TLS
// failure, a DNS miss. Those are the far end's facts and the Plugin may
// legitimately retry them.
func fetchErrorText(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return errFetchDeadline
	}
	return err.Error()
}

// isTimeout catches the timeouts that do not wrap context.DeadlineExceeded —
// net.Error's own, and the one http.Client's Timeout field produces.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// lookupIPAddr is the resolver refuseInternal asks, a variable so a test can
// answer for a name that has no address outside it.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// refuseInternal fails CLOSED: a name that will not resolve is refused rather than
// handed to the transport, exactly as safefetch's redirect check does, because the
// alternative leaves the decision to a resolver whose answer we never saw.
func (h *hostFuncs) refuseInternal(ctx context.Context, hostname string) string {
	addrs, err := lookupIPAddr(ctx, hostname)
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
// the network failing. Only safefetch's own refusal and the dial check's count:
// everything else is a blip the Plugin may legitimately retry.
func isPolicyRefusal(err error) bool {
	return err != nil && (errors.Is(err, errDialRefused) ||
		strings.Contains(err.Error(), safefetch.ErrRedirectBlocked.Error()))
}

// errDialRefused is the dial check's refusal, answered to the guest the way a
// refused redirect is.
var errDialRefused = errors.New("plugins: dial refused: the address is one a plugin may not reach")

// checkDialedAddress is the net.Dialer Control every plugin fetch dials under: the
// private-address rule applied to the address the socket is about to connect to.
//
// refuseInternal and safefetch.CheckRedirect each judge a lookup of their own,
// and the transport then resolves the name AGAIN to dial it, so a resolver that
// answers public for the check and private for the dial gets through — DNS
// rebinding. Here there is no second lookup to disagree with: the address is the
// one being connected to. An address that does not parse as an IP (a zoned
// link-local one, say) is refused, for the reason refuseInternal fails closed.
//
// The rule is safefetch.IsInternalIP's, so 100.64.0.0/10 — a tailnet's addresses —
// stays reachable, as it is everywhere else.
func checkDialedAddress(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errDialRefused
	}
	ip := net.ParseIP(host)
	if ip == nil || safefetch.IsInternalIP(ip) {
		return errDialRefused
	}
	return nil
}

// exemptAddrKey carries, on the context of a fetch to the operator's own host,
// the one address (dialKey) its dials are not checked for: that host on the port
// the fetch named, so a redirect to the same host on another port is checked.
type exemptAddrKey struct{}

// dialKey is how an address is compared at the dial: the host spelled as
// normalizeHost spells it, and the port.
func dialKey(host, port string) string { return net.JoinHostPort(normalizeHost(host), port) }

// targetPort is the port a request to u dials, the scheme's own when u names none.
func targetPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "socks5", "socks5h":
		return "1080"
	}
	return "80"
}

// proxyAddrs is the set of proxy addresses the transport's Proxy function has
// named, which are the only dials a proxy makes from this server.
type proxyAddrs struct{ seen sync.Map }

// wrap returns proxy, noting every address it names.
func (a *proxyAddrs) wrap(proxy func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if proxy == nil {
		return nil
	}
	return func(req *http.Request) (*url.URL, error) {
		u, err := proxy(req)
		if u != nil {
			a.seen.Store(dialKey(u.Hostname(), targetPort(u)), struct{}{})
		}
		return u, err
	}
}

func (a *proxyAddrs) has(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	_, ok := a.seen.Load(dialKey(host, port))
	return ok
}

// dialCheckedClients returns c in the two forms a plugin fetch uses, both dialing
// under checkDialedAddress:
//
//   - checked, for every fetch the MANIFEST or the issuer licensed, where every
//     dial is checked;
//   - operator, for a fetch to the host the OPERATOR typed, where a dial to that
//     host and port (exemptAddrKey) is not, and every other dial — a redirect off
//     it — is. It keeps no idle connections, so an unchecked connection is never
//     handed to a later request its exemption did not cover.
//
// In both, a dial to the HTTP(S) proxy the operator configured (HTTP_PROXY and
// HTTPS_PROXY, read by http.DefaultTransport's Proxy) is not checked either: the
// proxy is the operator's choice, on their LAN as often as not. The dial is then
// to the proxy and never to the target, so the target is judged only by the
// lookups refuseInternal and safefetch.CheckRedirect make — through a proxy, DNS
// rebinding is NOT closed. Nor is a dial to the proxy's own address that a
// request makes directly (one NO_PROXY exempts), which the same lookups judge.
//
// A client whose Transport is not an *http.Transport is returned as it is, for
// both: there is no dialer to put the check on. The production wiring passes a
// nil client, which is http.DefaultTransport's.
func dialCheckedClients(c *http.Client) (checked, operator *http.Client) {
	base, ok := c.Transport.(*http.Transport)
	if c.Transport == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok {
		return c, c
	}
	plain := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: checkDialedAddress}
	proxies := &proxyAddrs{}

	ct := base.Clone()
	ct.Proxy = proxies.wrap(base.Proxy)
	ct.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if proxies.has(addr) {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	ot := base.Clone()
	ot.Proxy = proxies.wrap(base.Proxy)
	ot.DisableKeepAlives = true
	ot.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		exempt, _ := ctx.Value(exemptAddrKey{}).(string)
		if host, port, err := net.SplitHostPort(addr); err == nil && exempt != "" && dialKey(host, port) == exempt {
			return plain.DialContext(ctx, network, addr)
		}
		return ct.DialContext(ctx, network, addr)
	}

	checked, operator = new(http.Client), new(http.Client)
	*checked, *operator = *c, *c
	checked.Transport, operator.Transport = ct, ot
	return checked, operator
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
		h.kvFailed("reading", req.Key, err)
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
		h.kvFailed("writing", req.Key, err)
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
		h.kvFailed("deleting", req.Key, err)
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
	return h.emit(ctx, mod, h.p.currentSettings(ctx))
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

// kvFailed logs a key-value call the store failed — naming neither the key nor
// the store's error during a call that carries a credential, since the guest
// chose the key and the error may quote it.
func (h *hostFuncs) kvFailed(op, key string, err error) {
	if h.p.secretCall() {
		h.p.logf("obelo: plugin %s: %s one of its keys failed during a call that carries a credential", h.p.id, op)
		return
	}
	h.p.logf("obelo: plugin %s: %s its key %q failed: %v", h.p.id, op, sanitizeLine(key), err)
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
