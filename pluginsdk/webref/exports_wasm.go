//go:build wasm

package webref

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport web_reference_links
func webReferenceLinks(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail("this module serves no Web reference provider: call webref.Serve from init() — " +
			"a -buildmode=c-shared module never runs main")
	}
	var call pluginapi.WebReferencesCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a WebReferencesCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Links(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("web references: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}
