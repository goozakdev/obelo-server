//go:build wasm

package onlinesource

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

const noProvider = "this module serves no Online source provider: call onlinesource.Serve from init() — " +
	"a -buildmode=c-shared module never runs main"

//go:wasmexport online_source_rows
func onlineSourceRows(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail(noProvider)
	}
	var call pluginapi.OnlineRowsCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not an OnlineRowsCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Rows(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("rows: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport online_source_row
func onlineSourceRow(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail(noProvider)
	}
	var call pluginapi.OnlineRowCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not an OnlineRowCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Row(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("row: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport online_source_search
func onlineSourceSearch(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail(noProvider)
	}
	var call pluginapi.OnlineSearchCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not an OnlineSearchCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Search(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("search: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport online_source_resolve
func onlineSourceResolve(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail(noProvider)
	}
	var call pluginapi.OnlineResolveCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not an OnlineResolveCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Resolve(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("resolve: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}
