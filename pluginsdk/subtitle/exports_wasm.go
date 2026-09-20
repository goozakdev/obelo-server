//go:build wasm

package subtitle

import (
	"context"

	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
	"github.com/goozakdev/obelo-server/pluginsdk"
)

//go:wasmexport obelo_subtitle_search
func subtitleSearch(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var call pluginapi.SubtitleSearchCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SubtitleSearchCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	resp, err := p.SearchSubtitles(context.Background(), call.Request)
	if err != nil {
		return pluginsdk.Fail("subtitle search: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

//go:wasmexport obelo_subtitle_download
func subtitleDownload(ptr, n uint32) uint64 {
	p := served
	if p == nil {
		return noProvider()
	}
	var call pluginapi.SubtitleDownloadCall
	if !pluginsdk.TakeRequest(ptr, n, &call) {
		return pluginsdk.Fail("the request is not a SubtitleDownloadCall")
	}
	withdraw := pluginsdk.PublishCallSettings(call.Settings)
	defer withdraw()

	resp, err := p.DownloadSubtitle(context.Background(), call.Request)
	if err != nil {
		return pluginsdk.Fail("subtitle download: " + err.Error())
	}
	return pluginsdk.Reply(resp)
}

func noProvider() uint64 {
	return pluginsdk.Fail("this module serves no Subtitle provider: call subtitle.Serve from init() — " +
		"a -buildmode=c-shared module never runs main")
}
