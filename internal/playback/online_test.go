package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	pluginapi "github.com/goozakdev/obelo-server/pluginapi/v1"
)

func onlineProfile() DeviceProfile {
	return DeviceProfile{
		Containers:  []string{"mp4"},
		VideoCodecs: []VideoCodecSupport{{Codec: "h264"}},
		AudioCodecs: []string{"aac"},
	}
}

func mp4(res string) pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{URL: "https://m.example/" + res + ".mp4", Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: res}
}

func TestChooseOnlineVariantPicksTheTallestPlayable(t *testing.T) {
	got, why := ChooseOnlineVariant(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{
		mp4("480p"),
		{URL: "https://m.example/x.webm", Container: "webm", Codecs: []string{"vp9", "opus"}, Resolution: "1080p"},
		mp4("720p"),
	})
	if why != nil || got.Resolution != "720p" {
		t.Fatalf("chose %+v (%v), want the 720p mp4: the webm is not playable", got, why)
	}
}

func TestChooseOnlineVariantHoldsTheCeiling(t *testing.T) {
	c, bound := ClampToCeiling(Constraints{}, access.Scope{MaxResolution: "480p"})
	if !bound {
		t.Fatal("the User's ceiling did not bind an unconstrained request")
	}
	got, why := ChooseOnlineVariant(onlineProfile(), c, []pluginapi.OnlineVariant{mp4("480p"), mp4("1080p")})
	if why != nil || got.Resolution != "480p" {
		t.Fatalf("chose %+v (%v), want the 480p mp4 under a 480p ceiling", got, why)
	}
	_, why = ChooseOnlineVariant(onlineProfile(), c, []pluginapi.OnlineVariant{mp4("1080p")})
	if why == nil || why.Reason != ReasonResolution {
		t.Fatalf("over the ceiling = %v, want a resolution refusal", why)
	}
	unstated := mp4("")
	if _, why = ChooseOnlineVariant(onlineProfile(), c, []pluginapi.OnlineVariant{unstated}); why == nil {
		t.Fatal("a variant stating no height played under a resolution ceiling")
	}
	if _, why = ChooseOnlineVariant(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{unstated}); why != nil {
		t.Fatalf("a variant stating no height was refused with no ceiling: %v", why)
	}
}

func TestChooseOnlineVariantReportsWhyNothingPlays(t *testing.T) {
	_, why := ChooseOnlineVariant(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{
		{URL: "https://m.example/x.webm", Container: "webm", Codecs: []string{"vp9", "opus"}},
	})
	if why == nil || why.Reason != ReasonContainer {
		t.Fatalf("refusal = %v, want container", why)
	}
	_, why = ChooseOnlineVariant(onlineProfile(), Constraints{}, nil)
	if why == nil || why.Reason != ReasonNoFile {
		t.Fatalf("no variants = %v, want noFile", why)
	}
}
