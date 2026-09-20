package pluginsdk

import (
	"context"
	"fmt"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

// Host is everything a plugin can do that has an effect outside its own linear
// memory: the six host functions of ADR-0058 decision 5, typed.
//
// It is an INTERFACE and not a struct for one reason, and the reason is the whole
// argument for this SDK. Provider logic written against it has no net/http, no
// socket and no clock of its own, so the same code runs unchanged inside the
// sandbox ([Sandbox], which calls the host functions) and in a native `go test`
// ([sdktest.Host], which answers out of a routing table). That is what lets the
// seven bundled providers keep the tests they already have while moving into
// WebAssembly (ADR-0059).
//
// Every method is request-response and JSON-shaped underneath, exactly like the
// contract it carries. Nothing here streams, blocks on a channel or hands back a
// reader.
type Host interface {
	// Fetch performs one outbound HTTP request THROUGH THE HOST. A plugin never
	// opens a socket; it asks, and the host — which checks the manifest allowlist,
	// the address behind the name and the byte cap — answers or refuses.
	//
	// The error is the ABI failing, which is rare and not the plugin's business to
	// recover from. Everything a source can do to you is in the response:
	// Refused ("this server will not do this", never worth retrying unchanged),
	// Error (the request was attempted and did not complete) and Status (an
	// answer, whatever the number). See [FetchError] and [DoJSON], which fold the
	// three into one error for the common case.
	//
	// The context is honoured by a test host and is advisory in the sandbox: the
	// real deadline is the host's, enforced by unwinding the guest, and a plugin
	// cannot outlive it by ignoring a ctx.
	Fetch(ctx context.Context, req pluginapi.FetchRequest) (pluginapi.FetchResponse, error)

	// Log writes one line to the SERVER's log, prefixed by the host with this
	// plugin's id. It is why a guest needs no stdout, and a plugin cannot make its
	// line look like the server's own.
	Log(level Level, msg string)

	// KVGet reads one key from this plugin's OWN namespace. found distinguishes a
	// key that was never written from one holding zero bytes; the error is the
	// host refusing (an oversize key) or the store failing.
	KVGet(key string) (value []byte, found bool, err error)
	// KVSet writes one key in this plugin's own namespace, replacing whatever was
	// there. An oversize key or value is REFUSED, never truncated.
	KVSet(key string, value []byte) error
	// KVDelete removes one key. Deleting a key that was never written is not an
	// error — it is the state the caller asked for.
	KVDelete(key string) error

	// Settings is what the Admin configured, as the host resolved it FOR THIS
	// CALL: the enabled flag, the decrypted secret, the effective URLs, the
	// server-wide metadata language, the operator's rate policy and any
	// manifest-declared values.
	//
	// SECRETS AT CALL TIME ONLY. Outside a call the host answers a zero Settings,
	// so a plugin that stashes this in a package variable is stashing something
	// that will be empty when it next looks. Read it per call; it is cheap.
	Settings() pluginapi.Settings
}

// Level is a log level for [Host.Log]. The host maps the number to a word; an
// unrecognized value is read as info rather than dropped.
type Level uint32

// The four levels, with the numbers the host function takes.
const (
	LevelDebug Level = 0
	LevelInfo  Level = 1
	LevelWarn  Level = 2
	LevelError Level = 3
)

// Logf logs one formatted line. It is the only sugar this SDK puts on the host
// functions, because "log this URL and this status" is what every provider's
// error path does and fmt.Sprintf at every call site is noise.
//
// There is no Debugf/Infof/Warnf/Errorf set behind it. A levelled logging API
// would be this SDK having opinions about something the host already owns: the
// host writes the prefix, maps the level and truncates the line, and a plugin
// that wants more structure than one string has no way to get it across the
// boundary anyway.
func Logf(h Host, level Level, format string, args ...any) {
	h.Log(level, fmt.Sprintf(format, args...))
}
