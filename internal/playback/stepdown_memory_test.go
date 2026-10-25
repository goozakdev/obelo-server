package playback

import (
	"fmt"
	"testing"

	"github.com/goozakdev/obelo-server/internal/store"
)

// memPicksStore is a ceilingStore that also holds one Remembered audio and video pick
// for the Title, counting how often the Title-level memory is read.
type memPicksStore struct {
	ceilingStore
	audio *store.RememberedAudio
	vid   *store.RememberedVideo
	reads *int
}

func (m memPicksStore) RememberedAudioForTitle(u, t string) (store.RememberedAudio, bool, error) {
	*m.reads++
	if m.audio == nil {
		return store.RememberedAudio{}, false, nil
	}
	return *m.audio, true, nil
}
func (m memPicksStore) SaveRememberedAudioForTitle(string, string, store.RememberedAudio) error {
	return nil
}
func (m memPicksStore) RememberedAudioForShow(string, string) (store.RememberedAudio, bool, error) {
	return store.RememberedAudio{}, false, nil
}
func (m memPicksStore) SaveRememberedAudioForShow(string, string, store.RememberedAudio) error {
	return nil
}
func (m memPicksStore) ShowIDForTitle(string) (string, bool, error) { return "", false, nil }
func (m memPicksStore) RememberedVideoForTitle(u, t string) (store.RememberedVideo, bool, error) {
	*m.reads++
	if m.vid == nil {
		return store.RememberedVideo{}, false, nil
	}
	return *m.vid, true, nil
}
func (m memPicksStore) SaveRememberedVideoForTitle(string, string, store.RememberedVideo) error {
	return nil
}
func (m memPicksStore) RememberedVideoForShow(string, string) (store.RememberedVideo, bool, error) {
	return store.RememberedVideo{}, false, nil
}
func (m memPicksStore) SaveRememberedVideoForShow(string, string, store.RememberedVideo) error {
	return nil
}

func memPicksSvc(a *store.RememberedAudio, v *store.RememberedVideo, reads *int, eds ...store.Edition) *Service {
	svc := NewService(memPicksStore{ceilingStore: ceilingStore{detail: titleWith(eds...)}, audio: a, vid: v, reads: reads}, nil, "", Governance{})
	svc.SetCanTranscode(true)
	return svc
}

// memMultiVideo is an Edition whose File holds v0 (default, c0 at h0) and v1 ("ALT", c1 at h1).
func memMultiVideo(id string, c0 string, h0 int, c1 string, h1 int, br int64) store.Edition {
	e := edition(id, "", h0, br)
	e.Files[0].VideoCodec = c0
	e.Files[0].Streams[0] = store.Stream{ID: id + "-v0", Kind: "video", Codec: c0, Height: h0, IsDefault: true, Title: "MAIN"}
	e.Files[0].Streams[1].ID = id + "-a0"
	e.Files[0].Streams = append(e.Files[0].Streams, store.Stream{ID: id + "-v1", Kind: "video", Codec: c1, Height: h1, Title: "ALT"})
	return e
}

// A Remembered video the client cannot decode: the ladder is derived as Negotiate plays it.
func TestLadderRememberedVideoUndecodable(t *testing.T) {
	var reads int
	v := &store.RememberedVideo{Label: "ALT", Codec: "hevc", Height: 2160}
	svc := memPicksSvc(nil, v, &reads, memMultiVideo("uhd", "h264", 2160, "hevc", 2160, 40_000_000), edition("hd", "", 1080, 8_000_000))
	checkDerived(t, "mem-video-undecodable", svc, stepReq())
}

// A Remembered video that is a lower h264 cut in the same File.
func TestLadderRememberedVideoLowerCut(t *testing.T) {
	var reads int
	v := &store.RememberedVideo{Label: "ALT", Codec: "h264", Height: 1080}
	p := uhdProfile()
	p.VideoCodecs = append(p.VideoCodecs, VideoCodecSupport{Codec: "hevc", MaxResolution: "2160p"})
	for _, br := range []int64{6_000_000, 40_000_000} {
		svc := memPicksSvc(nil, v, &reads, memMultiVideo("uhd", "hevc", 2160, "h264", 1080, br))
		base := stepReq()
		base.Profile = p
		checkDerived(t, fmt.Sprintf("mem-video-lower-%d", br), svc, base)
	}
}

// A Remembered audio the client decodes, with and without a cap of the client's own.
func TestLadderRememberedAudioOwnCap(t *testing.T) {
	var reads int
	e := edition("uhd", "", 2160, 40_000_000)
	e.Files[0].Streams[1].ID = "uhd-en"
	e.Files[0].Streams = append(e.Files[0].Streams, store.Stream{ID: "uhd-de", Kind: "audio", Codec: "ac3", Channels: 6, Language: "de"})
	a := &store.RememberedAudio{Language: "de", Channels: 6}
	svc := memPicksSvc(a, nil, &reads, e, edition("hd", "", 1080, 8_000_000))
	for _, own := range []Constraints{{}, {MaxBitrate: 50_000_000}} {
		q := stepReq()
		q.Constraints = own
		checkDerived(t, fmt.Sprintf("mem-audio-own-%d", own.MaxBitrate), svc, q)
	}
}

// Negotiate itself plays the Remembered picks and reads each Title memory once.
func TestNegotiateRememberedPicksUnchanged(t *testing.T) {
	var reads int
	e := memMultiVideo("uhd", "h264", 1080, "h264", 720, 8_000_000)
	e.Files[0].Streams = append(e.Files[0].Streams, store.Stream{ID: "uhd-de", Kind: "audio", Codec: "aac", Channels: 2, Language: "de"})
	a := &store.RememberedAudio{Language: "de", Channels: 2}
	v := &store.RememberedVideo{Label: "ALT", Codec: "h264", Height: 720}
	svc := memPicksSvc(a, v, &reads, e)
	d := negotiateOK(t, svc, stepReq())
	if d.AudioStream.ID != "uhd-de" || d.VideoStream.ID != "uhd-v1" {
		t.Fatalf("remembered picks not played: audio %q video %q", d.AudioStream.ID, d.VideoStream.ID)
	}
	if reads != 2 {
		t.Errorf("Negotiate read the Title memories %d times, want 2 (one audio, one video)", reads)
	}
}
