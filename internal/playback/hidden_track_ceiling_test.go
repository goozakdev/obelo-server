package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// A File holding ANY video track above the User's resolution ceiling is never direct
// played: direct play serves the whole File, so the over-ceiling track would be
// extractable even though the track the checks approved is within the ceiling. The
// approved track is remuxed instead (directplay-ceiling-bypass).

func hiddenTrackEdition() store.Edition {
	f := store.File{ID: "f1", Path: "/movies/hidden.mkv", Container: "matroska,webm", VideoCodec: "h264", AudioCodec: "aac",
		Height: 2160, Bitrate: 5_000_000, Present: true, Streams: []store.Stream{
			{ID: "v0", Kind: "video", Codec: "h264", Height: 2160},
			{ID: "v1", Kind: "video", Codec: "h264", Height: 720, IsDefault: true},
			{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
		}}
	return store.Edition{ID: "e1", Files: []store.File{f}}
}

func TestHiddenTrackOverCeilingRemuxes(t *testing.T) {
	svc := NewService(ceilingStore{detail: titleWith(hiddenTrackEdition())}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	dec := negotiateOK(t, svc, d35Request())
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "v1" {
		t.Fatalf("tier %s video %s, want directStream v1", dec.Tier, dec.VideoStream.ID)
	}
}

func TestHiddenTrackOverCeilingExplicitPickStillRemuxes(t *testing.T) {
	svc := NewService(ceilingStore{detail: titleWith(hiddenTrackEdition())}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.VideoStreamID = "v1"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "v1" {
		t.Fatalf("tier %s video %s, want directStream v1", dec.Tier, dec.VideoStream.ID)
	}
}

func TestHiddenTrackWithoutCeilingStaysDirectPlay(t *testing.T) {
	svc := NewService(ceilingStore{detail: titleWith(hiddenTrackEdition())}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.Scope.MaxResolution = ""
	// The client's own maxResolution is not a Playback ceiling; it must not force a remux.
	req.Constraints.MaxResolution = "1080p"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierDirectPlay {
		t.Fatalf("tier %s, want directPlay (no User ceiling)", dec.Tier)
	}
}

func TestUnknownHeightTrackAndCoverArtDoNotCountAsOverCeiling(t *testing.T) {
	ed := hiddenTrackEdition()
	ed.Files[0].Streams = []store.Stream{
		{ID: "v0", Kind: "video", Codec: "h264", Height: 0},
		{ID: "v1", Kind: "video", Codec: "h264", Height: 720, IsDefault: true},
		{ID: "cov", Kind: "video", Codec: "mjpeg", Height: 2160},
		{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
	}
	svc := NewService(ceilingStore{detail: titleWith(ed)}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	dec := negotiateOK(t, svc, d35Request())
	if dec.Tier != TierDirectPlay {
		t.Fatalf("tier %s, want directPlay", dec.Tier)
	}
}
