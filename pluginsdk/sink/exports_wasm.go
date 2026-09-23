//go:build wasm

package sink

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

// deliver is the Event sink Extension point's one contract call.
//
// A sink reports failure as a VALUE and not as a failed call: Delivered false
// with the author's own words is what the host counts and puts on the settings
// screen. A Go error from the served sink says the same thing, so it is folded
// into that response rather than answered as a broken ABI — the host would count
// it as a transport failure either way, and an operator reading "the target
// answered 503" is better served than one reading "the guest refused the call".
//
//go:wasmexport deliver
func deliver(ptr, n uint32) uint64 {
	s := served
	if s == nil {
		return pluginsdk.Fail("this module serves no Event sink: call sink.Serve from init() — " +
			"a -buildmode=c-shared module never runs main")
	}
	var req pluginapi.SinkDeliverRequest
	if !pluginsdk.TakeRequest(ptr, n, &req) {
		return pluginsdk.Fail("the request is not a SinkDeliverRequest")
	}
	withdraw := pluginsdk.PublishCallSettings(req.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(req.Settings.CallRemainingMillis)
	defer cancel()
	if err := s.Deliver(ctx, req.Event); err != nil {
		return pluginsdk.Reply(pluginapi.SinkDeliverResponse{Error: err.Error()})
	}
	return pluginsdk.Reply(pluginapi.SinkDeliverResponse{Delivered: true})
}
