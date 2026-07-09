package playback

import (
	"errors"
	"testing"

	"github.com/marioquake/juicebox/internal/store"
)

// Unit tests for the image-subtitle burn-in negotiation (ADR-0020, subtitles/04):
// resolving a burnSubtitleId to its burnable image track, and escalating a
// Decision to a transcode that burns it in. The real-ffmpeg tier + governance
// behavior is covered end-to-end by the api integration test.

// burnFixture is a Title with one Movie Edition whose File carries embedded video/
// audio/text/image subtitle Streams, plus Sidecar text + image tracks — the shape
// the resolver walks.
func burnFixture() store.TitleDetail {
	file := store.File{
		ID:         "f1",
		Path:       "/movies/Sub Movie (2020).mkv",
		Container:  "matroska",
		VideoCodec: "h264",
		AudioCodec: "aac",
		Height:     1080,
		Bitrate:    8_000_000,
		DurationMs: 600_000,
		Present:    true,
		Streams: []store.Stream{
			{ID: "v", Kind: "video", Codec: "h264", Height: 1080},
			{ID: "a", Kind: "audio", Codec: "aac", Channels: 2},
			{ID: "s-text", Kind: "subtitle", Codec: "subrip", Language: "eng"},
			// The 2nd subtitle stream (subtitle-relative index 1) is an image sub.
			{ID: "s-pgs", Kind: "subtitle", Codec: "hdmv_pgs_subtitle", Language: "ger"},
		},
	}
	return store.TitleDetail{
		Editions: []store.Edition{{ID: "ed1", Files: []store.File{file}}},
		Subtitles: []store.Subtitle{
			{ID: "sc-text", Source: "sidecar", Kind: "text", Codec: "srt", Language: "es"},
			{ID: "sc-img", Source: "sidecar", Kind: "image", Codec: "vobsub", Language: "it", Path: "/movies/Sub Movie (2020).it.idx"},
		},
	}
}

func TestResolveBurnTargetEmbeddedImage(t *testing.T) {
	got, ok := resolveBurnTarget(burnFixture(), "s-pgs")
	if !ok {
		t.Fatal("resolveBurnTarget(embedded image) not found")
	}
	if got.Path != "/movies/Sub Movie (2020).mkv" {
		t.Errorf("Path = %q, want the source container path", got.Path)
	}
	if got.StreamIndex != 1 {
		t.Errorf("StreamIndex = %d, want 1 (subtitle-relative index of the 2nd sub stream)", got.StreamIndex)
	}
	if got.EditionID != "ed1" || got.FileID != "f1" {
		t.Errorf("owner = %s/%s, want ed1/f1", got.EditionID, got.FileID)
	}
}

func TestResolveBurnTargetSidecarImage(t *testing.T) {
	got, ok := resolveBurnTarget(burnFixture(), "sc-img")
	if !ok {
		t.Fatal("resolveBurnTarget(sidecar image) not found")
	}
	if got.Path != "/movies/Sub Movie (2020).it.idx" {
		t.Errorf("Path = %q, want the sidecar file path", got.Path)
	}
	if got.StreamIndex != -1 {
		t.Errorf("StreamIndex = %d, want -1 (a sidecar file needs no si=)", got.StreamIndex)
	}
	if got.FileID != "" {
		t.Errorf("FileID = %q, want empty (a sidecar is Title-scoped)", got.FileID)
	}
}

// A text track (embedded or sidecar) and an unknown id are all "no image sub to
// burn" — the resolver reports not-found so the caller returns
// ErrBurnSubtitleNotFound (text is delivered selectably, never burned).
func TestResolveBurnTargetTextOrUnknownNotFound(t *testing.T) {
	for _, id := range []string{"s-text", "sc-text", "nope"} {
		if _, ok := resolveBurnTarget(burnFixture(), id); ok {
			t.Errorf("resolveBurnTarget(%q) = found, want not-found (not a burnable image track)", id)
		}
	}
}

// escalateForBurn turns a would-be direct-play Decision into a transcode with the
// image sub burned in: the tier is transcode, Burn carries the resolved path/index,
// and the Subtitle list is preserved.
func TestEscalateForBurnEmbedded(t *testing.T) {
	detail := burnFixture()
	base := Decision{
		Tier:    TierDirectPlay,
		Edition: detail.Editions[0],
		File:    detail.Editions[0].Files[0],
	}
	req := Request{BurnSubtitleID: "s-pgs"}
	dec, err := escalateForBurn(req, detail, base)
	if err != nil {
		t.Fatalf("escalateForBurn: %v", err)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode (burn-in escalates)", dec.Tier)
	}
	if dec.Burn == nil || dec.Burn.StreamIndex != 1 {
		t.Errorf("Burn = %+v, want the embedded image sub at si=1", dec.Burn)
	}
	if len(dec.Subtitles) == 0 {
		t.Error("escalated decision dropped the Subtitle list")
	}
}

// A sidecar image burns over the base File (Title-scoped), still escalating to
// transcode.
func TestEscalateForBurnSidecar(t *testing.T) {
	detail := burnFixture()
	base := Decision{
		Tier:    TierDirectStream,
		Edition: detail.Editions[0],
		File:    detail.Editions[0].Files[0],
	}
	dec, err := escalateForBurn(Request{BurnSubtitleID: "sc-img"}, detail, base)
	if err != nil {
		t.Fatalf("escalateForBurn(sidecar): %v", err)
	}
	if dec.Tier != TierTranscode {
		t.Errorf("tier = %q, want transcode", dec.Tier)
	}
	if dec.Burn == nil || dec.Burn.Path != "/movies/Sub Movie (2020).it.idx" || dec.Burn.StreamIndex != -1 {
		t.Errorf("Burn = %+v, want the sidecar path with si=-1", dec.Burn)
	}
}

func TestEscalateForBurnUnknownIsNotFound(t *testing.T) {
	detail := burnFixture()
	base := Decision{Tier: TierDirectPlay, Edition: detail.Editions[0], File: detail.Editions[0].Files[0]}
	if _, err := escalateForBurn(Request{BurnSubtitleID: "nope"}, detail, base); !errors.Is(err, ErrBurnSubtitleNotFound) {
		t.Errorf("err = %v, want ErrBurnSubtitleNotFound", err)
	}
}

// transcodeJobPlan carries the Decision's Burn into the transcode job so the args
// builder can emit the overlay.
func TestTranscodeJobPlanCarriesBurn(t *testing.T) {
	detail := burnFixture()
	dec := Decision{
		Tier:        TierTranscode,
		Edition:     detail.Editions[0],
		File:        detail.Editions[0].Files[0],
		VideoStream: detail.Editions[0].Files[0].Streams[0],
		AudioStream: detail.Editions[0].Files[0].Streams[1],
		Burn:        &BurnSubtitle{Path: "/movies/Sub Movie (2020).mkv", StreamIndex: 1},
	}
	job := transcodeJobPlan(DeviceProfile{}, Constraints{}, dec)
	if job.Burn == nil || job.Burn.StreamIndex != 1 {
		t.Errorf("job.Burn = %+v, want the burn carried through", job.Burn)
	}
}
