//go:build wasm

package signin

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport sign_in_password
func signInPassword(ptr, n uint32) uint64 {
	p := servedPassword
	if p == nil {
		return pluginsdk.Fail("this module serves no password Sign-in provider: call signin.ServePassword from init() — " +
			"a -buildmode=c-shared module never runs main")
	}
	var call pluginapi.SignInPasswordCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SignInPasswordCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.CheckPassword(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("password sign-in: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

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

//go:wasmexport sign_in_refresh
func signInRefresh(ptr, n uint32) uint64 {
	p, ok := served.(pluginapi.SignInRefreshProvider)
	if !ok {
		return pluginsdk.Fail("this module's redirect Sign-in provider does not implement Refresh")
	}
	var call pluginapi.SignInRefreshCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SignInRefreshCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Refresh(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("refresh: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport sign_in_lookup
func signInLookup(ptr, n uint32) uint64 {
	p, ok := lookupProvider()
	if !ok {
		return pluginsdk.Fail("this module's Sign-in provider does not implement Lookup")
	}
	var call pluginapi.SignInLookupCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SignInLookupCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Lookup(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("lookup: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

func noProvider() uint64 {
	return pluginsdk.Fail("this module serves no redirect Sign-in provider: call signin.ServeRedirect from init() — " +
		"a -buildmode=c-shared module never runs main")
}
