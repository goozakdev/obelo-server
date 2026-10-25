package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goozakdev/obelo-server/internal/access"
	"github.com/goozakdev/obelo-server/internal/playback"
	"github.com/goozakdev/obelo-server/internal/store"
)

// A directPlay decision serves the File byte-identical, so the player plays the
// container-default video track. When negotiation ranked and checked a DIFFERENT track
// (ADR-0025), the decision must be a remux of that track, never a direct play that
// skips the checks — here a 2160p default under a 1080p Playback ceiling.

type ceilingStore struct{ d store.TitleDetail }

func (s ceilingStore) TitleByID(string) (store.TitleDetail, error) { return s.d, nil }
func (s ceilingStore) WatchStateFor(string, string) (store.WatchState, error) {
	return store.WatchState{}, nil
}
func (s ceilingStore) SaveWatchState(string, string, int64, bool, bool) error { return nil }

func TestCeilingBoundNonDefaultTrackIsRemuxedNotDirectPlayed(t *testing.T) {
	t.Parallel()
	f := store.File{ID: "f1", Path: "/movies/two.mkv", Container: "matroska,webm", VideoCodec: "h264", AudioCodec: "aac",
		Height: 2160, Bitrate: 5_000_000, Present: true, Streams: []store.Stream{
			{ID: "v0", Kind: "video", Codec: "h264", Height: 2160, IsDefault: true},
			{ID: "v1", Kind: "video", Codec: "h264", Height: 720},
			{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
		}}
	td := store.TitleDetail{Editions: []store.Edition{{ID: "e1", Files: []store.File{f}}}}
	td.Title.ID = "t1"
	svc := playback.NewService(ceilingStore{td}, nil, "", playback.Governance{})
	svc.SetCanTranscode(true)
	req := playback.Request{UserID: "u1", TitleID: "t1",
		Profile: playback.DeviceProfile{Containers: []string{"mkv", "mp4"},
			VideoCodecs: []playback.VideoCodecSupport{{Codec: "h264", MaxResolution: "2160p"}},
			AudioCodecs: []string{"aac"}, MaxAudioChannels: 8},
		Scope: access.Scope{LibraryIDs: []string{""}, MaxResolution: "1080p"}}

	dec, sess, unsup, busy, err := svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != playback.TierDirectStream {
		t.Errorf("tier = %q, want directStream (the default track is over the ceiling)", dec.Tier)
	}
	if dec.VideoStream.ID != "v1" {
		t.Errorf("video = %q, want v1", dec.VideoStream.ID)
	}
	if sess.Tier != playback.TierDirectStream {
		t.Errorf("session tier = %q, want directStream", sess.Tier)
	}
	if url := toDecisionResponse(sess.ID, "t1", dec, nil).StreamURL; !strings.Contains(url, "/hls/") {
		t.Errorf("streamUrl = %q, want the HLS remux, not the raw File", url)
	}
}

// An explicit audioStreamId naming the File's default audio must not skip the ceiling
// check: the decision stays a remux and the raw-File route refuses the session.
func TestDefaultAudioPickUnderCeilingIsNotServedRaw(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "two.mkv")
	if err := os.WriteFile(path, []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := store.File{ID: "f1", Path: path, Container: "matroska,webm", VideoCodec: "h264", AudioCodec: "aac",
		Height: 2160, Bitrate: 5_000_000, Present: true, Streams: []store.Stream{
			{ID: "v0", Kind: "video", Codec: "h264", Height: 2160, IsDefault: true},
			{ID: "v1", Kind: "video", Codec: "h264", Height: 720},
			{ID: "a0", Kind: "audio", Codec: "aac", Channels: 2, IsDefault: true},
		}}
	td := store.TitleDetail{Editions: []store.Edition{{ID: "e1", Files: []store.File{f}}}}
	td.Title.ID = "t1"
	svc := playback.NewService(ceilingStore{td}, nil, "", playback.Governance{})
	svc.SetCanTranscode(true)
	req := playback.Request{UserID: "u1", TitleID: "t1", AudioStreamID: "a0",
		Profile: playback.DeviceProfile{Containers: []string{"mkv", "mp4"},
			VideoCodecs: []playback.VideoCodecSupport{{Codec: "h264", MaxResolution: "2160p"}},
			AudioCodecs: []string{"aac"}, MaxAudioChannels: 8},
		Scope: access.Scope{LibraryIDs: []string{""}, MaxResolution: "1080p"}}

	dec, sess, unsup, busy, err := svc.Negotiate(req)
	if err != nil || unsup != nil || busy != nil {
		t.Fatalf("negotiate err=%v unsup=%v busy=%v", err, unsup, busy)
	}
	if dec.Tier != playback.TierDirectStream || dec.VideoStream.ID != "v1" {
		t.Fatalf("tier %q video %q, want directStream v1", dec.Tier, dec.VideoStream.ID)
	}
	r := httptest.NewRequest(http.MethodGet, "/sessions/"+sess.ID+"/stream", nil)
	r = r.WithContext(withIdentity(r.Context(), identity{User: store.User{ID: "u1"}}))
	w := httptest.NewRecorder()
	handleSessionStream(svc, sess.ID)(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "session not found") {
		t.Errorf("raw /stream = %d %q, want the 404 hide-existence answer", w.Code, w.Body.String())
	}
}
