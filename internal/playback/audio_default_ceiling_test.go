package playback

import (
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/store"
)

// An audio pick that resolves to the File's default audio must not turn a remux into a
// direct play: direct play serves the File whole, and its container-default video track
// (2160p here) is not the one the 1080p ceiling approved (directplay-ceiling-bypass).

func d35Edition() store.Edition {
	f := store.File{ID: "f1", Path: "/movies/two.mkv", Container: "matroska,webm", VideoCodec: "h264", AudioCodec: "aac",
		Height: 2160, Bitrate: 5_000_000, Present: true, Streams: []store.Stream{
			{ID: "v0", Kind: "video", Codec: "h264", Height: 2160, IsDefault: true},
			{ID: "v1", Kind: "video", Codec: "h264", Height: 720},
			{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, Language: "eng", IsDefault: true},
			{ID: "a1", Kind: "audio", Codec: "aac", Channels: 2, Language: "fra"},
		}}
	return store.Edition{ID: "e1", Files: []store.File{f}}
}

func d35Request() Request {
	return Request{UserID: "u1", TitleID: "t1",
		Profile: DeviceProfile{Containers: []string{"mkv", "mp4"},
			VideoCodecs: []VideoCodecSupport{{Codec: "h264", MaxResolution: "2160p"}},
			AudioCodecs: []string{"aac"}, MaxAudioChannels: 8},
		Scope: access.Scope{LibraryIDs: []string{""}, MaxResolution: "1080p"}}
}

func TestExplicitDefaultAudioUnderCeilingStaysRemux(t *testing.T) {
	svc := NewService(ceilingStore{detail: titleWith(d35Edition())}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	req := d35Request()
	req.AudioStreamID = "a0"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "v1" {
		t.Fatalf("tier %s video %s, want directStream v1 (the default track is over the ceiling)", dec.Tier, dec.VideoStream.ID)
	}
}

func TestRememberedDefaultAudioUnderCeilingStaysRemux(t *testing.T) {
	var reads int
	mem := &store.RememberedAudio{Language: "en", Channels: 2}
	svc := memPicksSvc(mem, nil, &reads, d35Edition())
	req := d35Request()
	req.Constraints.PreferredAudioLang = "fra"
	dec := negotiateOK(t, svc, req)
	if dec.Tier != TierDirectStream || dec.VideoStream.ID != "v1" || dec.AudioStream.ID != "a0" {
		t.Fatalf("tier %s video %s audio %s, want directStream v1 a0", dec.Tier, dec.VideoStream.ID, dec.AudioStream.ID)
	}
}
