//go:build wasm

package marker

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport marker_provider_markers
func markerProviderMarkers(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail("this module serves no Marker provider: call marker.Serve from init() — " +
			"a -buildmode=c-shared module never runs main")
	}
	var call pluginapi.MarkersCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a MarkersCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Markers(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("markers: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}
