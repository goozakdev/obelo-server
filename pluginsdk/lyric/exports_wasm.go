//go:build wasm

package lyric

import (
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport lyric_provider_lyrics
func lyricProviderLyrics(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return pluginsdk.Fail("this module serves no Lyric provider: call lyric.Serve from init() — " +
			"a -buildmode=c-shared module never runs main")
	}
	var call pluginapi.LyricsCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a LyricsCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	ctx, cancel := pluginsdk.CallContext(call.Settings.CallRemainingMillis)
	defer cancel()
	resp, err := p.Lyrics(ctx, call.Request)
	if err != nil {
		return pluginsdk.Fail("lyrics: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}
