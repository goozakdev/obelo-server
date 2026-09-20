//go:build wasm

package pluginsdk

import (
	"context"
	"errors"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// The six host functions of ADR-0058 decision 5, declared once, and the [Host]
// that wraps them.
//
// A plugin author writes none of this. An author in another language writes all
// of it, from the six signatures below and pluginapi.schema.json — which is the
// claim the reference Discord plugin and the SDK-free test guest keep honest.

//go:wasmimport obelo http_fetch
func hostHTTPFetch(ptr, n uint32) uint64

//go:wasmimport obelo log
func hostLog(level, ptr, n uint32)

//go:wasmimport obelo kv_get
func hostKVGet(ptr, n uint32) uint64

//go:wasmimport obelo kv_set
func hostKVSet(ptr, n uint32) uint64

//go:wasmimport obelo kv_delete
func hostKVDelete(ptr, n uint32) uint64

//go:wasmimport obelo settings_get
func hostSettingsGet() uint64

// errHostSilent is the one error [Host.Fetch] returns: the host function answered
// 0, which means the ABI itself went wrong rather than the request failing. A
// plugin cannot do anything about it and should report it and stop.
var errHostSilent = errors.New("the host answered nothing")

// sandbox is the Host backed by the host functions. It holds no state but the
// call-scoped settings a sink or a subtitle provider is handed WITH its request
// (see [PublishCallSettings]).
type sandbox struct{}

// Sandbox is the Host a plugin running inside Obelo calls. It is the only
// constructor that exists in a wasm build, and it does not exist in a native one
// — a native test uses sdktest.Host, which is the point of the interface.
func Sandbox() Host { return sandbox{} }

var _ Host = sandbox{}

func (sandbox) Fetch(_ context.Context, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error) {
	var resp pluginapi.FetchResponse
	if !hostRoundTrip(hostHTTPFetch, req, &resp) {
		return pluginapi.FetchResponse{}, errHostSilent
	}
	return resp, nil
}

func (sandbox) Log(level Level, msg string) {
	b := []byte(msg)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	hostLog(uint32(level), ptr, uint32(len(b)))
	free(ptr)
}

func (sandbox) KVGet(key string) ([]byte, bool, error) {
	var resp pluginapi.KVGetResponse
	if !hostRoundTrip(hostKVGet, pluginapi.KVGetRequest{Key: key}, &resp) {
		return nil, false, errHostSilent
	}
	if resp.Error != "" {
		return nil, false, errors.New(resp.Error)
	}
	return resp.Value, resp.Found, nil
}

func (sandbox) KVSet(key string, value []byte) error {
	return kvWrite(hostKVSet, pluginapi.KVSetRequest{Key: key, Value: value})
}

func (sandbox) KVDelete(key string) error {
	return kvWrite(hostKVDelete, pluginapi.KVDeleteRequest{Key: key})
}

func kvWrite(call func(ptr, n uint32) uint64, req any) error {
	var resp pluginapi.KVWriteResponse
	if !hostRoundTrip(call, req, &resp) {
		return errHostSilent
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	if !resp.OK {
		return errors.New("the host refused the write and said nothing about why")
	}
	return nil
}

// Settings is what the host resolved for the call on the stack.
//
// Two sources, and which one applies is a fact about the Extension point. A
// Metadata provider asks the host (settings_get), because it has eight calls and
// eight per-call envelopes carrying the same document would be eight places for a
// secret to be forgotten. An Event sink and a Subtitle provider are HANDED their
// settings with the request, so the dispatcher publishes them here for the
// duration of the call and this answers those instead — one spelling of
// "settings" for provider code whichever seam it fills.
func (sandbox) Settings() pluginapi.Settings {
	if callSettings != nil {
		return *callSettings
	}
	var s pluginapi.Settings
	if !readAnswer(hostSettingsGet(), &s) {
		return pluginapi.Settings{}
	}
	return s
}

// callSettings is the settings that rode in with the current call, or nil when
// none did. There is no lock: a guest instance is single-threaded and the host
// serializes every call into it (ADR-0058 decision 7).
var callSettings *pluginapi.Settings

// PublishCallSettings makes the settings that arrived with a request visible to
// [Host.Settings] for the duration of the call, and returns the function that
// withdraws them.
//
// It is called by the dispatchers, not by plugin code. It is exported because
// they are separate packages, and withdrawn on the way out for the reason the
// host withdraws its own: SECRETS AT CALL TIME ONLY. A plugin reading Settings
// outside a call gets a zero value either way.
func PublishCallSettings(s pluginapi.Settings) func() {
	callSettings = &s
	return func() { callSettings = nil }
}
