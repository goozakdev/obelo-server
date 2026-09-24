//go:build wasm

package signin

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport sign_in_authorize_url
func signInAuthorizeURL(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var call pluginapi.SignInAuthorizeCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SignInAuthorizeCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.AuthorizeURL(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("authorize url: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport sign_in_exchange
func signInExchange(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var call pluginapi.SignInExchangeCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SignInExchangeCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Exchange(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("exchange: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

func noProvider() uint64 {
	return pluginsdk.Fail("this module serves no redirect Sign-in provider: call signin.ServeRedirect from init() — " +
		"a -buildmode=c-shared module never runs main")
}
