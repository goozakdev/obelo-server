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

// TestPlanOnlineCapsAnEncodeAtTheDevicesH264Limit: the encode is always h264, so it
// is bound by the device's h264 ceiling as well as the request's cap, exactly as a
// Title's re-encode is, and the variant is chosen against that bound.
func TestPlanOnlineCapsAnEncodeAtTheDevicesH264Limit(t *testing.T) {
	profile := onlineProfile()
	profile.VideoCodecs = []VideoCodecSupport{{Codec: "h264", MaxResolution: "1080p"}}
	for name, c := range map[string]Constraints{"constraint above the device": {MaxResolution: "2160p"}, "no constraint": {}} {
		plan, why := PlanOnline(profile, c, []pluginapi.OnlineVariant{mp4("2160p")}, true)
		if why != nil || !plan.Transcode || plan.MaxHeight != 1080 {
			t.Errorf("%s: plan = %+v (%v), want a transcode capped at 1080", name, plan, why)
		}
	}
	plan, why := PlanOnline(profile, Constraints{}, []pluginapi.OnlineVariant{split("2160p"), split("1080p")}, true)
	if why != nil || plan.Variant.Resolution != "1080p" || plan.MaxHeight != 1080 {
		t.Errorf("variant chosen against the device cap = %+v (%v), want the 1080p split", plan, why)
	}
	plan, _ = PlanOnline(profile, Constraints{MaxResolution: "720p"}, []pluginapi.OnlineVariant{mp4("2160p")}, true)
	if plan.MaxHeight != 720 {
		t.Errorf("a tighter request cap = %d, want 720", plan.MaxHeight)
	}
}

func TestOnlineServerBusySuggestsAStepDown(t *testing.T) {
	if got := OnlineServerBusy(Constraints{}).SuggestedMaxBitrate; got != busyBitrateFloor {
		t.Errorf("no ask: suggested %d, want the floor %d", got, busyBitrateFloor)
	}
	if got := OnlineServerBusy(Constraints{MaxBitrate: 4_000_000}).SuggestedMaxBitrate; got != 2_000_000 {
		t.Errorf("asked 4 Mbit/s: suggested %d, want half", got)
	}
}

func split(res string) pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{
		Kind: pluginapi.OnlineVariantSplit, VideoURL: "https://m.example/" + res + ".v", AudioURL: "https://m.example/" + res + ".a",
		Container: "mp4", Codecs: []string{"h264", "aac"}, Resolution: res,
	}
}

func manifest() pluginapi.OnlineVariant {
	return pluginapi.OnlineVariant{Kind: pluginapi.OnlineVariantManifest, URL: "https://m.example/master.m3u8", Container: "hls"}
}

// TestPlanOnlineRelaysAMuxedVariantTheClientCanPlay: B wins whenever it can.
func TestPlanOnlineRelaysAMuxedVariantTheClientCanPlay(t *testing.T) {
	plan, why := PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{split("1080p"), manifest(), mp4("720p")}, true)
	if why != nil || plan.Transcode || plan.Variant.Resolution != "720p" {
		t.Fatalf("plan = %+v (%v), want the 720p mp4 relayed", plan, why)
	}
}

// TestPlanOnlineSendsWhatCannotBeRelayedToFFmpeg: a client that cannot play the
// muxed variant, a split variant, a manifest, and a variant above the ceiling each
// go to ffmpeg; with no ffmpeg they are the refusal a Title gives.
func TestPlanOnlineSendsWhatCannotBeRelayedToFFmpeg(t *testing.T) {
	webm := pluginapi.OnlineVariant{URL: "https://m.example/x.webm", Container: "webm", Codecs: []string{"vp9", "opus"}, Resolution: "720p"}
	plan, why := PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{webm}, true)
	if why != nil || !plan.Transcode || plan.Variant.URL != webm.URL {
		t.Fatalf("a webm for an mp4 client = %+v (%v), want ffmpeg", plan, why)
	}
	plan, why = PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{split("1080p")}, true)
	if why != nil || !plan.Transcode || plan.Variant.Kind != pluginapi.OnlineVariantSplit {
		t.Fatalf("a split variant = %+v (%v), want ffmpeg", plan, why)
	}
	plan, why = PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{manifest()}, true)
	if why != nil || !plan.Transcode || plan.Variant.Kind != pluginapi.OnlineVariantManifest {
		t.Fatalf("a manifest variant = %+v (%v), want ffmpeg", plan, why)
	}

	c := Constraints{MaxResolution: "480p", MaxBitrate: 1_500_000}
	plan, why = PlanOnline(onlineProfile(), c, []pluginapi.OnlineVariant{mp4("1080p")}, true)
	if why != nil || !plan.Transcode || plan.MaxHeight != 480 || plan.MaxBitrate != 1_500_000 {
		t.Fatalf("1080p under a 480p ceiling = %+v (%v), want a transcode capped at 480 and 1.5 Mbit/s", plan, why)
	}

	if _, why = PlanOnline(onlineProfile(), c, []pluginapi.OnlineVariant{mp4("1080p")}, false); why == nil || why.Reason != ReasonResolution {
		t.Fatalf("without ffmpeg = %v, want the resolution refusal a Title gives", why)
	}
	if _, why = PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{split("720p")}, false); why == nil {
		t.Fatal("a split variant with no ffmpeg was planned")
	}
}

// TestPlanOnlineChoosesTheTallestVariantWithinTheCeiling for a transcode, and the
// smallest above it when none fits, and ignores variants without a usable URL.
func TestPlanOnlineChoosesTheTallestVariantWithinTheCeiling(t *testing.T) {
	c := Constraints{MaxResolution: "720p"}
	plan, why := PlanOnline(onlineProfile(), c, []pluginapi.OnlineVariant{split("2160p"), split("1080p"), split("720p"), split("480p")}, true)
	if why != nil || plan.Variant.Resolution != "720p" {
		t.Fatalf("plan = %+v (%v), want the 720p split", plan, why)
	}
	plan, why = PlanOnline(onlineProfile(), c, []pluginapi.OnlineVariant{split("2160p"), split("1080p")}, true)
	if why != nil || plan.Variant.Resolution != "1080p" || plan.MaxHeight != 720 {
		t.Fatalf("plan = %+v (%v), want the 1080p split scaled down to 720", plan, why)
	}
	broken := pluginapi.OnlineVariant{Kind: pluginapi.OnlineVariantSplit, VideoURL: "https://m.example/v", Container: "mp4"}
	if _, why = PlanOnline(onlineProfile(), Constraints{}, []pluginapi.OnlineVariant{broken, {Kind: "teleport", URL: "https://m.example/x"}}, true); why == nil || why.Reason != ReasonNoFile {
		t.Fatalf("variants with no usable URL = %v, want noFile", why)
	}
}

// TestOnlineBitrateRuleOnlyTheCeilingForcesFFmpeg: a variant states no bitrate, so
// only an Admin-set Playback ceiling bitrate keeps it off the relay; a bitrate the
// client merely asked for is ignored for it (the web player always sends one).
func TestOnlineBitrateRuleOnlyTheCeilingForcesFFmpeg(t *testing.T) {
	variants := []pluginapi.OnlineVariant{mp4("720p")}

	// A client constraint only (an Admin, no ceiling): relayed.
	c, bound := ClampToCeiling(Constraints{MaxBitrate: 100_000_000}, access.Scope{})
	if bound {
		t.Fatal("no ceiling bound the request")
	}
	plan, why := PlanOnline(onlineProfile(), c, variants, true)
	if why != nil || plan.Transcode {
		t.Fatalf("client bitrate only = %+v (%v), want the muxed variant relayed", plan, why)
	}
	if _, why = ChooseOnlineVariant(onlineProfile(), c, variants); why != nil {
		t.Fatalf("ChooseOnlineVariant under a client bitrate only = %v, want playable", why)
	}

	// A bitrate ceiling (a Member): ffmpeg held to it, or refused without ffmpeg.
	c, bound = ClampToCeiling(Constraints{MaxBitrate: 100_000_000}, access.Scope{MaxBitrate: 4_000_000})
	if !bound {
		t.Fatal("the bitrate ceiling did not bind")
	}
	plan, why = PlanOnline(onlineProfile(), c, variants, true)
	if why != nil || !plan.Transcode || plan.MaxBitrate != 4_000_000 {
		t.Fatalf("under a bitrate ceiling = %+v (%v), want ffmpeg at 4 Mbit/s", plan, why)
	}
	if _, why = PlanOnline(onlineProfile(), c, variants, false); why == nil || why.Reason != ReasonBitrate {
		t.Fatalf("under a bitrate ceiling without ffmpeg = %v, want the bitrate refusal", why)
	}

	// A ceiling looser than the client's own ask still counts as a ceiling.
	c, _ = ClampToCeiling(Constraints{MaxBitrate: 2_000_000}, access.Scope{MaxBitrate: 8_000_000})
	plan, why = PlanOnline(onlineProfile(), c, variants, true)
	if why != nil || !plan.Transcode || plan.MaxBitrate != 2_000_000 {
		t.Fatalf("looser ceiling = %+v (%v), want ffmpeg held to the stricter 2 Mbit/s", plan, why)
	}
}
