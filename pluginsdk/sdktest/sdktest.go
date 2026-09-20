// Package sdktest is an in-memory [pluginsdk.Host] for NATIVE tests of plugin
// code (ADR-0059: "Provider tests — native through the SDK's host interface; API
// suites through wazero").
//
// It is the second half of the argument for [pluginsdk.Host] being an interface.
// A provider written against that interface has no net/http, so a test of it
// needs no network, no sandbox and no wasm toolchain: it hands the provider a
// Host whose Fetch answers out of an http.Handler, calls Lookup, and asserts the
// record. That test runs in milliseconds, can be table-driven, and is the SAME
// test the provider had before it became a plugin — which is the property the
// seven bundled providers are ported under.
//
// What it does NOT do is replace the end-to-end suites. A Host that answers from
// a map proves the provider's logic; only wazero proves the module loads, the
// exports are spelled right and the JSON survives the boundary. Both exist, and
// they prove different things.
//
// # The four ways to answer a fetch
//
// In the order they are consulted:
//
//  1. [WithFetch] — a function. Total control, for the test that wants to return
//     a refusal or a transport error rather than a response.
//  2. [WithHostHandler] — an http.Handler per URL host. This is the routing table
//     a provider with two hosts needs (an API and an image CDN).
//  3. [WithHandler] — one http.Handler for everything else. This is the ordinary
//     case, and it is what an httptest.NewServer handler becomes when a provider
//     test stops needing a server.
//  4. [WithHTTPClient] — a real client, for a test that still wants a real
//     httptest.Server. Passing http.DefaultClient makes this host a thin shim over
//     the network, which is occasionally what a port needs on its first day.
//
// With none of them configured, every fetch answers a refusal naming the URL, so
// a test that forgot to wire a route fails with a sentence instead of a nil map.
package sdktest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// Host is an in-memory [pluginsdk.Host]: a fetch route or two, a captured log, a
// key-value map and fixed settings.
//
// It is safe for concurrent use, which a real guest's host is not — a guest
// instance is single-threaded — because a native test is free to exercise a
// provider from several goroutines and a data race there would be the test's
// fault rather than the provider's.
type Host struct {
	mu sync.Mutex

	settings pluginapi.Settings

	fetch    func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error)
	routes   map[string]http.Handler
	fallback http.Handler
	client   *http.Client
	allowed  map[string]bool

	requests []pluginapi.FetchRequest
	logs     []LogLine
	kv       map[string][]byte
}

// LogLine is one call to [Host.Log].
type LogLine struct {
	Level   pluginsdk.Level
	Message string
}

// Option configures a Host.
type Option func(*Host)

// New builds a Host. The zero configuration is a usable Host that refuses every
// fetch and remembers everything.
func New(opts ...Option) *Host {
	h := &Host{
		routes: map[string]http.Handler{},
		kv:     map[string][]byte{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

var _ pluginsdk.Host = (*Host)(nil)

// --- configuration --------------------------------------------------------

// WithSettings fixes what [Host.Settings] answers.
func WithSettings(s pluginapi.Settings) Option {
	return func(h *Host) { h.settings = s }
}

// WithURL is WithSettings for the one field most provider tests set: the base URL
// the operator configured.
func WithURL(raw string) Option {
	return func(h *Host) { h.settings.URL = raw }
}

// WithSecret sets the API key the host resolved for this call.
func WithSecret(secret string) Option {
	return func(h *Host) { h.settings.Secret = secret }
}

// WithRateLimitMillis sets the operator's pacing override. Pass a negative number
// for "the operator set none", which is the absent pointer and is NOT the same as
// zero (see [pluginsdk.IntervalFrom]).
func WithRateLimitMillis(ms int) Option {
	return func(h *Host) {
		if ms < 0 {
			h.settings.RateLimitMillis = nil
			return
		}
		v := ms
		h.settings.RateLimitMillis = &v
	}
}

// WithFetch answers every fetch with a function of the test's own.
func WithFetch(fn func(context.Context, pluginapi.FetchRequest) (pluginapi.FetchResponse, error)) Option {
	return func(h *Host) { h.fetch = fn }
}

// WithHandler answers every fetch that no host route claims by serving it to an
// http.Handler in memory — no listener, no port, no TLS.
func WithHandler(handler http.Handler) Option {
	return func(h *Host) { h.fallback = handler }
}

// WithHandlerFunc is WithHandler for the common literal.
func WithHandlerFunc(fn func(http.ResponseWriter, *http.Request)) Option {
	return WithHandler(http.HandlerFunc(fn))
}

// WithHostHandler routes one URL host to its own handler, so a provider whose
// images come from a second host can be tested against both at once. The host is
// matched case-insensitively with its port removed, exactly as the real host's
// allowlist matches.
func WithHostHandler(host string, handler http.Handler) Option {
	return func(h *Host) { h.routes[normalizeHost(host)] = handler }
}

// WithHTTPClient forwards every unrouted fetch to a real client, for a test that
// keeps an httptest.Server.
func WithHTTPClient(c *http.Client) Option {
	return func(h *Host) { h.client = c }
}

// WithAllowedHosts makes this Host refuse a fetch to any other host, in the real
// host's own words — so a test can prove its provider handles a refusal without
// standing up a sandbox.
func WithAllowedHosts(hosts ...string) Option {
	return func(h *Host) {
		h.allowed = map[string]bool{}
		for _, raw := range hosts {
			h.allowed[normalizeHost(raw)] = true
		}
	}
}

// WithKV seeds the key-value namespace, for the provider that reads a cursor it
// wrote on an earlier pass.
func WithKV(entries map[string][]byte) Option {
	return func(h *Host) {
		for k, v := range entries {
			h.kv[k] = append([]byte(nil), v...)
		}
	}
}

// --- the Host interface ---------------------------------------------------

// RefusedAllowlist is the host's own sentence for a target the manifest does not
// allow. It is here so a test can assert the refusal it configured, and it is
// still true that a plugin must never branch on this text.
const RefusedAllowlist = "host not in allowlist"

// Fetch answers the request from whichever of the four sources is configured,
// recording it first.
func (h *Host) Fetch(ctx context.Context, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	h.mu.Lock()
	h.requests = append(h.requests, req)
	fetch, fallback, client := h.fetch, h.fallback, h.client
	allowed := h.allowed
	h.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return pluginapi.FetchResponse{}, err
	}
	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return pluginapi.FetchResponse{Refused: "not an absolute http or https URL"}, nil
	}
	if allowed != nil && !allowed[normalizeHost(target.Hostname())] {
		return pluginapi.FetchResponse{Refused: RefusedAllowlist}, nil
	}
	if fetch != nil {
		return fetch(ctx, req)
	}
	h.mu.Lock()
	handler := h.routes[normalizeHost(target.Hostname())]
	h.mu.Unlock()
	if handler == nil {
		handler = fallback
	}
	if handler != nil {
		return serveInMemory(ctx, handler, req)
	}
	if client != nil {
		return doLive(ctx, client, req)
	}
	return pluginapi.FetchResponse{}, fmt.Errorf(
		"sdktest: nothing is configured to answer %s — pass WithHandler, WithHostHandler, WithFetch or WithHTTPClient", req.URL)
}

// Log records one line.
func (h *Host) Log(level pluginsdk.Level, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, LogLine{Level: level, Message: msg})
}

// KVGet reads one key from the in-memory namespace.
func (h *Host) KVGet(key string) ([]byte, bool, error) {
	if key == "" {
		return nil, false, fmt.Errorf("a key-value entry needs a key")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.kv[key]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// KVSet writes one key.
func (h *Host) KVSet(key string, value []byte) error {
	if key == "" {
		return fmt.Errorf("a key-value entry needs a key")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kv[key] = append([]byte(nil), value...)
	return nil
}

// KVDelete removes one key. Removing a key that was never written is not an
// error.
func (h *Host) KVDelete(key string) error {
	if key == "" {
		return fmt.Errorf("a key-value entry needs a key")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.kv, key)
	return nil
}

// Settings answers the fixed settings.
func (h *Host) Settings() pluginapi.Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

// --- what a test reads back -----------------------------------------------

// SetSettings replaces the settings mid-test, for the case a save would have
// changed them between two calls.
func (h *Host) SetSettings(s pluginapi.Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings = s
}

// Requests is every fetch this Host was asked for, in order.
func (h *Host) Requests() []pluginapi.FetchRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.FetchRequest(nil), h.requests...)
}

// Paths is the path of every fetch, in order — the assertion a provider test
// usually wants, because "did it resolve by id or fall back to a search" is a
// question about which path it asked for.
func (h *Host) Paths() []string {
	var out []string
	for _, req := range h.Requests() {
		if u, err := url.Parse(req.URL); err == nil {
			out = append(out, u.Path)
			continue
		}
		out = append(out, req.URL)
	}
	return out
}

// Logs is every line the provider logged, in order.
func (h *Host) Logs() []LogLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]LogLine(nil), h.logs...)
}

// KVKeys is every key currently held, sorted, so an assertion is stable.
func (h *Host) KVKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.kv))
	for k := range h.kv {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the two ways to actually answer --------------------------------------

// serveInMemory runs one request through an http.Handler with no network at all.
func serveInMemory(ctx context.Context, handler http.Handler, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	r := httptest.NewRequest(method, req.URL, body)
	r = r.WithContext(ctx)
	for _, hd := range req.Headers {
		if hd.Name != "" {
			r.Header.Add(hd.Name, hd.Value)
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	res := rec.Result()
	defer res.Body.Close()

	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return pluginapi.FetchResponse{Status: res.StatusCode, Error: err.Error()}, nil
	}
	return pluginapi.FetchResponse{
		Status:  res.StatusCode,
		Headers: headersOf(res.Header),
		Body:    payload,
	}, nil
}

// doLive performs the request for real, which is what a port keeping its
// httptest.Server needs.
func doLive(ctx context.Context, client *http.Client, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	r, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return pluginapi.FetchResponse{Refused: "the request could not be built"}, nil
	}
	for _, hd := range req.Headers {
		if hd.Name != "" {
			r.Header.Add(hd.Name, hd.Value)
		}
	}
	res, err := client.Do(r)
	if err != nil {
		return pluginapi.FetchResponse{Error: err.Error()}, nil
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(res.Body)
	if err != nil {
		return pluginapi.FetchResponse{Status: res.StatusCode, Error: err.Error()}, nil
	}
	return pluginapi.FetchResponse{
		Status:  res.StatusCode,
		Headers: headersOf(res.Header),
		Body:    payload,
	}, nil
}

func headersOf(h http.Header) []pluginapi.FetchHeader {
	var out []pluginapi.FetchHeader
	for name, values := range h {
		for _, v := range values {
			out = append(out, pluginapi.FetchHeader{Name: name, Value: v})
		}
	}
	return out
}

// normalizeHost lowercases a host and drops any port, matching the real host's
// allowlist rule: an entry covers a source on any port.
func normalizeHost(raw string) string {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}
